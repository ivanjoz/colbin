//! The Go codecs are the specification, so the port is diffed against them
//! rather than against itself.
//!
//! `vectors/main.go` marshals a fixed corpus with the real `colbin` package and
//! records, per case, the message it produced and the values the Go decoder
//! reads back out of it. Nothing in the corpus is hand-written: the field ids
//! come out of a schema section colbin wrote, the compact expectations come out
//! of a `compact.Reader`, and the columnar ones come out of `colbin.Unmarshal`.
//!
//! The corpus is committed, so this test needs no Go toolchain; CI regenerates
//! it and fails on a diff, which is what catches a Go-side format change nobody
//! ported.

use base64::Engine;
use colbin::{Error, FieldSpec, Kind, Record, Schema, Value};
use serde_json::Value as Json;

const CORPUS: &str = include_str!("../vectors/vectors.json");

fn corpus() -> Json {
    serde_json::from_str(CORPUS).expect("the corpus is valid JSON")
}

/// Every field id in the corpus was assigned by the Go builder. Rebuilding them
/// from the names alone is what pins the hash and the linear probe: a change
/// there renames every field at once, and a message would then decode to nothing
/// but zero values without ever reporting an error.
#[test]
fn field_ids_match_the_go_layout() {
    let corpus = corpus();
    let cases = corpus["fieldIds"].as_array().expect("fieldIds is an array");
    assert!(!cases.is_empty());
    for case in cases {
        let name = case["name"].as_str().unwrap();
        let (schema, ids) = schema_of(&case["fields"]);
        assert_eq!(schema.ids(), ids, "field ids for {name}");
    }
}

/// Every message, decoded against the schema its Go type implies, must yield the
/// values and the presence the Go decoder read out of the same bytes.
#[test]
fn decodes_the_go_message_corpus() {
    let corpus = corpus();
    let cases = corpus["cases"].as_array().expect("cases is an array");
    assert!(!cases.is_empty());
    for case in cases {
        let name = case["name"].as_str().unwrap();
        let (schema, ids) = schema_of(&case["fields"]);
        assert_eq!(schema.ids(), ids, "field ids for {name}");

        let message = decode_base64(case["message"].as_str().unwrap());
        // The mode is the message's own business, but a case that stopped
        // exercising the mode it was written for would silently stop testing it.
        assert_eq!(
            colbin::is_compact(&message),
            case["mode"] == "compact",
            "wire mode for {name}"
        );

        let records = colbin::decode(&message, &schema).unwrap_or_else(|err| {
            panic!("{name}: {err}");
        });
        let expected = case["records"].as_array().unwrap();
        assert_eq!(records.len(), expected.len(), "record count for {name}");
        for (index, (record, want)) in records.iter().zip(expected).enumerate() {
            assert_record(&format!("{name} record {index}"), record, want, &schema);
        }
    }
}

/// The bridge's own shape, spelled out: a session token is one compact record,
/// and the five fields are found by the ids the Go struct's field names hash to.
#[test]
fn decodes_a_session_token() {
    let schema = Schema::from_go(&[
        ("CompanyID", Kind::Int32),
        ("ID", Kind::Int32),
        ("Created", Kind::Int32),
        ("Hash", Kind::Uint64),
        ("User", Kind::String),
    ])
    .unwrap();
    assert_eq!(schema.ids(), [202, 53, 159, 26, 106]);

    let message = decode_base64(case_named("compact-token")["message"].as_str().unwrap());
    let record = colbin::decode_one(&message, &schema).unwrap();
    assert_eq!(record.i64(202), 7);
    assert_eq!(record.i64(53), 42);
    assert_eq!(record.i64(159), 1234);
    assert_eq!(record.u64(26), 12_720_753_295_591_565_293);
    assert_eq!(record.str(106), "tester");

    // A field the encoder omitted because it held its zero value reads back as
    // that zero value, which is what the Go decoder writes into the struct.
    let empty = decode_base64(
        case_named("compact-token-empty-user")["message"]
            .as_str()
            .unwrap(),
    );
    let record = colbin::decode_one(&empty, &schema).unwrap();
    assert_eq!(record.i64(202), 1);
    assert_eq!(record.i64(159), 0);
    assert_eq!(record.str(106), "");
    assert_eq!(record.get(106), None);
}

#[test]
fn rejects_messages_it_cannot_read() {
    let schema = Schema::from_go(&[("ID", Kind::Int32)]).unwrap();

    assert_eq!(colbin::decode(&[], &schema), Err(Error::Truncated));
    assert_eq!(
        colbin::decode(&[0x0a, 0x01, 0x00], &schema),
        Err(Error::BadVersion(0x0a))
    );
    // A self-describing message carries a schema section this decoder skips over
    // nothing, so it says so rather than reading the section as a record count.
    assert_eq!(
        colbin::decode(&[0x04, 0x01, 0x00], &schema),
        Err(Error::SelfDescribing(0x04))
    );
    // A column naming a field the schema does not have cannot be stepped over:
    // the wire carries no type tag, so there is no way to know how far.
    assert_eq!(
        colbin::decode(&[0x02, 0x01, 0x01, 0x07, 0x00], &schema),
        Err(Error::UnknownField(7))
    );
    // A record count that outruns the payload.
    assert!(matches!(
        colbin::decode(&[0x02, 0x04, 0x01, 53, 0x00], &schema),
        Err(Error::Truncated)
    ));
    // decode_one is for the single-record shape only.
    let message = decode_base64(case_named("compact-shape-3")["message"].as_str().unwrap());
    let (schema, _) = schema_of(&case_named("compact-shape-3")["fields"]);
    assert_eq!(
        colbin::decode_one(&message, &schema),
        Err(Error::RecordCount(3))
    );
}

#[test]
fn rejects_schemas_that_cannot_name_a_field() {
    assert_eq!(
        Schema::from_ids([(1, Kind::Int32), (1, Kind::String)]).unwrap_err(),
        Error::DuplicateFieldId(1)
    );
    assert_eq!(
        Schema::from_ids([(255, Kind::Int32)]).unwrap_err(),
        Error::FieldIdOutOfRange(255)
    );
    assert_eq!(
        Schema::from_fields(&[
            FieldSpec::tagged("a", 3, Kind::Int32),
            FieldSpec::tagged("b", 3, Kind::Int32),
        ])
        .unwrap_err(),
        Error::DuplicateFieldId(3)
    );
}

// --- corpus plumbing ---------------------------------------------------------

fn case_named(name: &str) -> Json {
    corpus()["cases"]
        .as_array()
        .unwrap()
        .iter()
        .find(|case| case["name"] == name)
        .unwrap_or_else(|| panic!("no case named {name}"))
        .clone()
}

/// Builds the schema a case's Go type implies, and returns the ids the Go
/// builder assigned alongside it so the two can be compared.
fn schema_of(fields: &Json) -> (Schema, Vec<u8>) {
    let fields = fields.as_array().expect("fields is an array");
    let names: Vec<String> = fields
        .iter()
        .map(|field| field["name"].as_str().unwrap().to_owned())
        .collect();
    let specs: Vec<FieldSpec<'_>> = fields
        .iter()
        .zip(&names)
        .map(|(field, name)| FieldSpec {
            name,
            id: field["explicitId"].as_u64().map(|id| id as u8),
            kind: kind_of(field["kind"].as_str().unwrap()),
        })
        .collect();
    let ids = fields
        .iter()
        .map(|field| field["id"].as_u64().unwrap() as u8)
        .collect();
    (
        Schema::from_fields(&specs).expect("the corpus schema is buildable"),
        ids,
    )
}

fn assert_record(what: &str, got: &Record, want: &Json, schema: &Schema) {
    let want = want.as_array().expect("a record is an array of fields");
    let got: Vec<(u8, &Value)> = got.iter().collect();
    assert_eq!(got.len(), want.len(), "{what}: field count");
    for (index, ((id, value), field)) in got.iter().zip(want).enumerate() {
        let want_id = field["id"].as_u64().unwrap() as u8;
        assert_eq!(*id, want_id, "{what}: field {index} id");
        let kind = schema
            .fields()
            .iter()
            .find(|f| f.id == want_id)
            .expect("the id is in the schema")
            .kind;
        let want_value = expected_value(kind, &field["v"]);
        assert!(
            same(value, &want_value),
            "{what}: field {want_id} is {value:?}, expected {want_value:?}"
        );
    }
}

/// Values are compared with floats taken by their bits, so a sign of zero or a
/// rounding that survived the wire is not quietly accepted by `-0.0 == 0.0`.
fn same(got: &Value, want: &Value) -> bool {
    match (got, want) {
        (Value::Float32(a), Value::Float32(b)) => a.to_bits() == b.to_bits(),
        (Value::Float64(a), Value::Float64(b)) => a.to_bits() == b.to_bits(),
        (Value::Float32s(a), Value::Float32s(b)) => {
            a.len() == b.len() && a.iter().zip(b).all(|(x, y)| x.to_bits() == y.to_bits())
        }
        (Value::Float64s(a), Value::Float64s(b)) => {
            a.len() == b.len() && a.iter().zip(b).all(|(x, y)| x.to_bits() == y.to_bits())
        }
        _ => got == want,
    }
}

/// Turns one corpus value into the [`Value`] the decoder must produce.
///
/// Integers travel as decimal strings because a JSON number cannot hold the
/// whole of `i64` or `u64`, and floats as their IEEE bit patterns because a
/// decimal rendering describes the bytes on the wire only approximately.
fn expected_value(kind: Kind, json: &Json) -> Value {
    match kind {
        Kind::Bool => Value::Bool(json.as_bool().unwrap()),
        Kind::Int8 | Kind::Int16 | Kind::Int32 | Kind::Int64 => Value::Int(int(json)),
        Kind::Uint8 | Kind::Uint16 | Kind::Uint32 | Kind::Uint64 => Value::Uint(uint(json)),
        Kind::Float32 => Value::Float32(f32::from_bits(bits(json) as u32)),
        Kind::Float64 => Value::Float64(f64::from_bits(bits(json))),
        Kind::String => Value::String(json.as_str().unwrap().to_owned()),
        Kind::Bytes => Value::Bytes(decode_base64(json.as_str().unwrap())),
        Kind::Int8s | Kind::Int16s | Kind::Int32s | Kind::Int64s => {
            Value::Ints(array(json).iter().map(int).collect())
        }
        Kind::Uint16s | Kind::Uint32s | Kind::Uint64s => {
            Value::Uints(array(json).iter().map(uint).collect())
        }
        Kind::Bools => Value::Bools(array(json).iter().map(|v| v.as_bool().unwrap()).collect()),
        Kind::Strings => Value::Strings(
            array(json)
                .iter()
                .map(|v| v.as_str().unwrap().to_owned())
                .collect(),
        ),
        Kind::Float32s => Value::Float32s(
            array(json)
                .iter()
                .map(|v| f32::from_bits(bits(v) as u32))
                .collect(),
        ),
        Kind::Float64s => Value::Float64s(
            array(json)
                .iter()
                .map(|v| f64::from_bits(bits(v)))
                .collect(),
        ),
    }
}

fn array(json: &Json) -> &Vec<Json> {
    json.as_array().expect("a slice value is a JSON array")
}

fn int(json: &Json) -> i64 {
    json.as_str().unwrap().parse().expect("a decimal integer")
}

fn uint(json: &Json) -> u64 {
    json.as_str().unwrap().parse().expect("a decimal integer")
}

fn bits(json: &Json) -> u64 {
    u64::from_str_radix(json.as_str().unwrap(), 16).expect("hex float bits")
}

fn kind_of(name: &str) -> Kind {
    match name {
        "bool" => Kind::Bool,
        "i8" => Kind::Int8,
        "i16" => Kind::Int16,
        "i32" => Kind::Int32,
        "i64" => Kind::Int64,
        "u8" => Kind::Uint8,
        "u16" => Kind::Uint16,
        "u32" => Kind::Uint32,
        "u64" => Kind::Uint64,
        "f32" => Kind::Float32,
        "f64" => Kind::Float64,
        "str" => Kind::String,
        "bytes" => Kind::Bytes,
        "i8s" => Kind::Int8s,
        "i16s" => Kind::Int16s,
        "i32s" => Kind::Int32s,
        "i64s" => Kind::Int64s,
        "u16s" => Kind::Uint16s,
        "u32s" => Kind::Uint32s,
        "u64s" => Kind::Uint64s,
        "bools" => Kind::Bools,
        "strs" => Kind::Strings,
        "f32s" => Kind::Float32s,
        "f64s" => Kind::Float64s,
        other => panic!("the corpus names a kind this decoder has no name for: {other}"),
    }
}

fn decode_base64(text: &str) -> Vec<u8> {
    base64::engine::general_purpose::STANDARD
        .decode(text)
        .expect("the corpus holds standard base64")
}
