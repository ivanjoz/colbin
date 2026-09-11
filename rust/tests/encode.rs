//! The compact-mode encoder, pinned against the Go corpora.
//!
//! Two assertions, and the first is the one that matters:
//!
//! * **Byte equality.** For every compact case the generator marked
//!   `rustByteExact`, `encode(decode(message))` must equal `message` exactly.
//!   That is what catches drift — a framing decision, a key width, a sign
//!   convention or an integer-array transform that stopped agreeing with Go
//!   shows up as a byte diff against a message Go actually wrote.
//! * **Round trip.** For every compact case, including the ones where a string
//!   packs, what Rust writes must decode back to the same records.
//!
//! A case is not byte-exact exactly when some string in it packs smaller than
//! raw, which is the one place this encoder deliberately differs. Go picks the
//! cheaper of the two per string and raw wins on anything short, so half the
//! corpus is byte-comparable; `everyStringIsRaw` in the generator is what decides
//! which, so the split is computed from the format rather than guessed here.

use colbin::{Kind, Record, Schema, Value};
use serde_json::Value as Json;

mod common;
use common::{decode_base64, kind_of};

const FLAT: &str = include_str!("../vectors/vectors.json");
const COMPOSITE: &str = include_str!("../vectors/composites.json");

/// Every compact case the generator says Rust can reproduce exactly, must be
/// reproduced exactly.
#[test]
fn reproduces_the_go_bytes() {
    let mut exact = 0;
    for Case {
        name,
        schema,
        byte_exact,
        message,
        shape,
    } in compact_cases()
    {
        if !byte_exact {
            continue;
        }
        let records =
            colbin::decode(&message, &schema).unwrap_or_else(|err| panic!("{name}: decode: {err}"));
        let written = encode_shaped(&schema, &records, shape)
            .unwrap_or_else(|err| panic!("{name}: encode: {err}"));
        assert_eq!(
            hex(&written),
            hex(&message),
            "{name}: the encoder did not reproduce the Go message"
        );
        exact += 1;
    }
    // A guard against the split silently collapsing to nothing: if the generator
    // ever marked every case divergent, the assertion above would pass vacuously.
    assert!(exact >= 8, "only {exact} cases were byte-compared");
}

/// What the encoder writes must decode back to what it was given, for every
/// compact case — including the ones whose strings pack, where the bytes
/// legitimately differ from Go's.
#[test]
fn round_trips_every_compact_case() {
    let mut checked = 0;
    for Case {
        name,
        schema,
        message,
        shape,
        ..
    } in compact_cases()
    {
        let records =
            colbin::decode(&message, &schema).unwrap_or_else(|err| panic!("{name}: decode: {err}"));
        let written = encode_shaped(&schema, &records, shape)
            .unwrap_or_else(|err| panic!("{name}: encode: {err}"));
        let reread = colbin::decode(&written, &schema)
            .unwrap_or_else(|err| panic!("{name}: re-decode: {err}"));
        assert_eq!(reread, records, "{name}: round trip");
        checked += 1;
    }
    assert!(checked >= 20, "only {checked} cases round-tripped");
}

/// A message Rust writes is a compact message: bit 0 set, no version byte, and
/// the header facts the schema implies.
#[test]
fn writes_a_well_formed_header() {
    let schema = Schema::from_ids([(1, Kind::Int32), (2, Kind::String)]).unwrap();
    let record = Record::new().with(1, Value::Int(7));
    let message = colbin::encode_one(&schema, &record).unwrap();
    assert!(colbin::is_compact(&message));

    // Narrow keys, since both ids fit the nibble; ALL_POSITIVE, since 7 >= 0.
    assert_eq!(message[0] & 1, 1, "compact bit");
    assert_eq!((message[0] >> 1) & 1, 1, "ALL_POSITIVE");
    assert_eq!((message[0] >> 2) & 3, 0, "shape: a lone struct");
    assert_eq!((message[0] >> 4) & 1, 1, "NARROW_KEYS");

    // A negative value clears ALL_POSITIVE.
    let record = Record::new().with(1, Value::Int(-7));
    let message = colbin::encode_one(&schema, &record).unwrap();
    assert_eq!((message[0] >> 1) & 1, 0, "ALL_POSITIVE with a negative");

    // A hashed id widens every key in the message.
    let wide = Schema::from_go(&[("Anything", Kind::Int32)]).unwrap();
    let message =
        colbin::encode_one(&wide, &Record::new().with(wide.ids()[0], Value::Int(1))).unwrap();
    assert_eq!((message[0] >> 4) & 1, 0, "NARROW_KEYS with a hashed id");
}

/// The omit-zero rule is the encoder's, not the caller's: a zero handed in is
/// still omitted, so a record built by hand and one that came back from `decode`
/// produce the same bytes.
#[test]
fn omits_zero_values() {
    let schema = Schema::from_ids([(1, Kind::Int32), (2, Kind::String), (3, Kind::Bool)]).unwrap();
    let sparse = Record::new().with(1, Value::Int(5));
    let padded = Record::new()
        .with(1, Value::Int(5))
        .with(2, Value::String(String::new()))
        .with(3, Value::Bool(false));
    assert_eq!(
        colbin::encode_one(&schema, &sparse).unwrap(),
        colbin::encode_one(&schema, &padded).unwrap(),
        "explicit zeros changed the message"
    );

    // A nested struct of nothing but zeros is omitted entirely, recursively.
    let inner = Schema::from_ids([(1, Kind::Int32)]).unwrap();
    let outer = Schema::from_ids([(1, Kind::Int64), (2, inner.nested())]).unwrap();
    let with_empty = Record::new()
        .with(1, Value::Int(1))
        .with(2, Value::Record(Record::new().with(1, Value::Int(0))));
    let without = Record::new().with(1, Value::Int(1));
    assert_eq!(
        colbin::encode_one(&outer, &with_empty).unwrap(),
        colbin::encode_one(&outer, &without).unwrap(),
        "an all-zero nested struct was written"
    );

    // Negative zero is not zero: comparing on the bits is what keeps it.
    let floats = Schema::from_ids([(1, Kind::Float64)]).unwrap();
    let minus = colbin::encode_one(&floats, &Record::new().with(1, Value::Float64(-0.0))).unwrap();
    let plus = colbin::encode_one(&floats, &Record::new().with(1, Value::Float64(0.0))).unwrap();
    assert_ne!(minus, plus, "-0.0 was omitted as a zero");
    let back = colbin::decode_one(&minus, &floats).unwrap();
    assert_eq!(back.f64(1).to_bits(), (-0.0_f64).to_bits());
}

/// A record of nothing is its terminator alone, and an empty or oversized batch
/// is refused: the shape is two header bits.
#[test]
fn refuses_what_the_shape_cannot_hold() {
    let schema = Schema::from_ids([(1, Kind::Int32)]).unwrap();
    assert_eq!(
        colbin::encode(&schema, &[]),
        Err(colbin::Error::RecordCount(0))
    );
    let four = vec![Record::new(); 4];
    assert_eq!(
        colbin::encode(&schema, &four),
        Err(colbin::Error::RecordCount(4))
    );
    for n in 1..=colbin::MAX_RECORDS {
        let records = vec![Record::new().with(1, Value::Int(3)); n];
        let message = colbin::encode(&schema, &records).expect("encodes");
        assert_eq!(colbin::decode(&message, &schema).unwrap(), records, "n={n}");
    }
}

/// A field the schema does not name, and a value that is not the form its kind
/// declares, are both caller errors rather than a corrupt message.
#[test]
fn refuses_a_record_the_schema_does_not_describe() {
    let schema = Schema::from_ids([(1, Kind::Int32)]).unwrap();
    assert_eq!(
        colbin::encode_one(&schema, &Record::new().with(9, Value::Int(1))),
        Err(colbin::Error::UnknownField(9))
    );
    assert_eq!(
        colbin::encode_one(&schema, &Record::new().with(1, Value::String("no".into()))),
        Err(colbin::Error::ValueKind { id: 1 })
    );
}

/// `Kind::Array` of a scalar names no Go type — a slice of scalars is its own
/// kind and rides a bulk codec — so a schema declaring one is refused when it is
/// built. Encoding it would produce a message Go's decoder misreads.
#[test]
fn refuses_an_array_of_scalars() {
    assert_eq!(
        Schema::from_ids([(1, Kind::Array(Box::new(Kind::Int32)))]),
        Err(colbin::Error::ArrayOfScalars { id: 1 })
    );
    // A slice of composites, and [][]byte, are the legal element forms.
    let inner = Schema::from_ids([(1, Kind::Int32)]).unwrap();
    Schema::from_ids([(1, Kind::Array(Box::new(inner.nested())))]).expect("a slice of structs");
    Schema::from_ids([(1, Kind::Array(Box::new(Kind::Bytes)))]).expect("[][]byte");
}

// --- corpus plumbing ---------------------------------------------------------

/// Every compact case in both corpora, as (name, (schema, byte-exact), message,
/// shape). Standard-mode cases are skipped: this encoder writes compact mode
/// only, which is what a single record is and what crosses the boundary.
/// One compact corpus case, as the encoder tests need it.
struct Case {
    name: String,
    schema: Schema,
    /// Whether the generator says Rust can reproduce this message exactly.
    byte_exact: bool,
    message: Vec<u8>,
    /// 0 for a lone struct, 1..3 for an array of that many records.
    shape: i64,
}

fn compact_cases() -> Vec<Case> {
    let mut out = Vec::new();
    for (corpus, composite) in [(FLAT, false), (COMPOSITE, true)] {
        let corpus: Json = serde_json::from_str(corpus).expect("valid JSON");
        for case in corpus["cases"].as_array().unwrap() {
            let message = decode_base64(case["message"].as_str().unwrap());
            if !colbin::is_compact(&message) {
                continue;
            }
            let schema = if composite {
                composite_schema(&case["fields"])
            } else {
                flat_schema(&case["fields"])
            };
            out.push(Case {
                name: case["name"].as_str().unwrap().to_owned(),
                schema,
                byte_exact: case["rustByteExact"].as_bool().unwrap_or(false),
                message,
                shape: case["shape"].as_i64().unwrap(),
            });
        }
    }
    out
}

/// Shape 0 is a lone struct; 1 to 3 are an array of that many records.
fn encode_shaped(
    schema: &Schema,
    records: &[Record],
    shape: i64,
) -> Result<Vec<u8>, colbin::Error> {
    if shape == 0 {
        colbin::encode_one(schema, &records[0])
    } else {
        colbin::encode(schema, records)
    }
}

fn flat_schema(fields: &Json) -> Schema {
    let specs: Vec<(u8, Kind)> = fields
        .as_array()
        .unwrap()
        .iter()
        .map(|field| {
            (
                field["id"].as_u64().unwrap() as u8,
                kind_of(field["kind"].as_str().unwrap()),
            )
        })
        .collect();
    Schema::from_ids(specs).expect("a valid schema")
}

fn composite_schema(fields: &Json) -> Schema {
    let specs: Vec<(u8, Kind)> = fields
        .as_array()
        .unwrap()
        .iter()
        .map(|field| {
            (
                field["id"].as_u64().unwrap() as u8,
                composite_kind(&field["k"]),
            )
        })
        .collect();
    Schema::from_ids(specs).expect("a valid schema")
}

fn composite_kind(node: &Json) -> Kind {
    match node["kind"].as_str().unwrap() {
        "struct" => composite_schema(&node["fields"]).nested(),
        "array" => Kind::Array(Box::new(composite_kind(&node["elem"]))),
        "map" => Kind::Map(
            Box::new(composite_kind(&node["key"])),
            Box::new(composite_kind(&node["val"])),
        ),
        scalar => kind_of(scalar),
    }
}

/// Hex, so a byte diff reports where rather than dumping two `Vec<u8>` debugs.
fn hex(bytes: &[u8]) -> String {
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}
