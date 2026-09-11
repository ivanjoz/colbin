//! `#[derive(Colbin)]` and `TypedCodec`.
//!
//! The typed path is an optimisation, so what has to be pinned first is that it
//! is *only* an optimisation: for the same value it must write exactly what the
//! dynamic `Codec` writes, and read back exactly what the dynamic path reads. It
//! is checked against the Go corpus through the structs below, which mirror the
//! generator's own types — so a message Go wrote reaches a Rust struct and comes
//! back byte for byte.

#![cfg(feature = "derive")]

use colbin::{Codec, Colbin, Kind, Record, Schema, TypedCodec, Value};
use serde_json::Value as Json;

mod common;
use common::decode_base64;

const FLAT: &str = include_str!("../vectors/vectors.json");

/// Mirrors `UsuarioToken` in `rust/vectors/main.go` — and, through it, genix's
/// `core.UsuarioToken`. The Rust fields are `snake_case` where the Go ones are
/// not, so each carries the name colbin hashes.
#[derive(Colbin, Debug, PartialEq, Clone)]
struct UsuarioToken {
    #[cb(name = "CompanyID")]
    company_id: i32,
    #[cb(name = "ID")]
    id: i32,
    #[cb(name = "Created")]
    created: i32,
    #[cb(name = "Hash")]
    hash: u64,
    #[cb(name = "User")]
    user: String,
}

/// Mirrors `benchStats` in `codec/typed_bench_test.go`: seven numbered fields,
/// so the message takes narrow keys.
#[derive(Colbin, Debug, PartialEq, Clone)]
struct BenchStats {
    #[cb(1)]
    quantity: i32,
    #[cb(2)]
    quantity_pending_delivery: i32,
    #[cb(3)]
    sub_quantity: i16,
    #[cb(4)]
    sub_quantity_pending: i16,
    #[cb(5)]
    sub_divisor: i16,
    #[cb(6)]
    total_amount: i32,
    #[cb(7)]
    total_debt_amount: i32,
}

/// Mirrors `Scalars` in the generator: every scalar kind the derive carries.
#[derive(Colbin, Debug, PartialEq, Clone)]
struct Scalars {
    #[cb(name = "B")]
    b: bool,
    #[cb(name = "I8")]
    i8: i8,
    #[cb(name = "I16")]
    i16: i16,
    #[cb(name = "I32")]
    i32: i32,
    #[cb(name = "I64")]
    i64: i64,
    #[cb(name = "U8")]
    u8: u8,
    #[cb(name = "U16")]
    u16: u16,
    #[cb(name = "U32")]
    u32: u32,
    #[cb(name = "U64")]
    u64: u64,
    #[cb(name = "F32")]
    f32: f32,
    #[cb(name = "F64")]
    f64: f64,
    #[cb(name = "S")]
    s: String,
    #[cb(name = "Blob")]
    blob: Vec<u8>,
}

/// Mirrors `Slices`: every primitive-slice kind.
#[derive(Colbin, Debug, PartialEq, Clone)]
struct Slices {
    #[cb(name = "I8s")]
    i8s: Vec<i8>,
    #[cb(name = "I16s")]
    i16s: Vec<i16>,
    #[cb(name = "I32s")]
    i32s: Vec<i32>,
    #[cb(name = "I64s")]
    i64s: Vec<i64>,
    #[cb(name = "U16s")]
    u16s: Vec<u16>,
    #[cb(name = "U32s")]
    u32s: Vec<u32>,
    #[cb(name = "U64s")]
    u64s: Vec<u64>,
    #[cb(name = "Bs")]
    bs: Vec<bool>,
    #[cb(name = "Ss")]
    ss: Vec<String>,
    #[cb(name = "F32s")]
    f32s: Vec<f32>,
    #[cb(name = "F64s")]
    f64s: Vec<f64>,
}

/// The derived ids must be the ones the Go builder assigned. This is the same
/// assertion the dynamic corpus test makes, and it is what catches a `name`
/// attribute that does not match the Go field it mirrors — which would otherwise
/// decode to nothing but zero values without reporting anything.
#[test]
fn derived_ids_match_the_go_layout() {
    for (name, derived) in [
        (
            "compact-token",
            UsuarioToken::colbin_schema().unwrap().ids(),
        ),
        ("compact-scalars", Scalars::colbin_schema().unwrap().ids()),
        ("compact-slices", Slices::colbin_schema().unwrap().ids()),
    ] {
        let case = case_named(name);
        let want: Vec<u8> = case["fields"]
            .as_array()
            .unwrap()
            .iter()
            .map(|field| field["id"].as_u64().unwrap() as u8)
            .collect();
        assert_eq!(derived, want, "{name}: derived ids");
    }
    // The numbered struct's ids are its tags, verbatim.
    assert_eq!(
        BenchStats::colbin_schema().unwrap().ids(),
        [1, 2, 3, 4, 5, 6, 7]
    );
    assert!(TypedCodec::<BenchStats>::new().unwrap().narrow_keys());
    assert!(!TypedCodec::<UsuarioToken>::new().unwrap().narrow_keys());
}

/// A Go-written message, read into a Rust struct's fields.
#[test]
fn reads_the_go_corpus_into_struct_fields() {
    let codec = TypedCodec::<UsuarioToken>::new().unwrap();

    let token = codec.decode(&message_of("compact-token")).unwrap();
    assert_eq!(
        token,
        UsuarioToken {
            company_id: 7,
            id: 42,
            created: 1234,
            hash: 12_720_753_295_591_565_293,
            user: "tester".into(),
        }
    );

    // An omitted field arrives as its zero value, which is what the omit-zero
    // rule means on this side.
    let sparse = codec
        .decode(&message_of("compact-token-empty-user"))
        .unwrap();
    assert_eq!(
        sparse,
        UsuarioToken {
            company_id: 1,
            id: 1,
            created: 0,
            hash: 0,
            user: String::new(),
        }
    );

    let scalars = TypedCodec::<Scalars>::new()
        .unwrap()
        .decode(&message_of("compact-scalars"))
        .unwrap();
    assert_eq!(scalars.i8, -7);
    assert_eq!(scalars.i32, -70000);
    assert_eq!(scalars.u64, u64::MAX);
    assert_eq!(scalars.f32, 1.5);
    assert_eq!(scalars.s, "Usuario1");
    assert_eq!(scalars.blob, [0, 1, 2, 250, 255]);

    let slices = TypedCodec::<Slices>::new()
        .unwrap()
        .decode(&message_of("compact-slices"))
        .unwrap();
    assert_eq!(slices.i8s, [-1, 0, 1, 127, -128]);
    assert_eq!(slices.i64s, [i64::MIN, 0, i64::MAX]);
    assert_eq!(slices.u64s, [u64::MAX, 0]);
    assert_eq!(slices.ss, ["uno", "dos", "tres"]);
    assert_eq!(slices.bs, [true, false, true, true, false]);
    assert_eq!(slices.f64s, [0.1, 1e300, -0.0]);
}

/// The typed path must write what the dynamic path writes — for the corpus
/// values, so the comparison is against messages that came from Go.
#[test]
fn writes_what_the_dynamic_path_writes() {
    assert_same_bytes::<UsuarioToken>("compact-token");
    assert_same_bytes::<UsuarioToken>("compact-token-empty-user");
    assert_same_bytes::<UsuarioToken>("compact-token-accented");
    assert_same_bytes::<Scalars>("compact-scalars");
    assert_same_bytes::<Scalars>("compact-all-positive");
    assert_same_bytes::<Scalars>("compact-mostly-omitted");
    assert_same_bytes::<Scalars>("compact-empty-record");
    assert_same_bytes::<Slices>("compact-slices");
    assert_same_bytes::<Slices>("compact-single-element-slices");
}

/// Reads a Go message with both paths, re-encodes with both, and requires all
/// four to agree — which pins the typed writer against the dynamic writer, and
/// both against Go's own bytes where the corpus says they can agree.
fn assert_same_bytes<T: Colbin + std::fmt::Debug>(name: &str) {
    let case = case_named(name);
    let message = decode_base64(case["message"].as_str().unwrap());
    let schema = T::colbin_schema().unwrap();

    let typed = TypedCodec::<T>::new().unwrap();
    let value = typed
        .decode(&message)
        .unwrap_or_else(|err| panic!("{name}: typed decode: {err}"));
    let from_typed = typed.encode(&value).unwrap();

    let dynamic = Codec::new(schema);
    let record = dynamic.decode_one(&message).unwrap();
    let from_dynamic = dynamic.encode_one(&record).unwrap();

    assert_eq!(
        hex(&from_typed),
        hex(&from_dynamic),
        "{name}: the typed writer disagrees with the dynamic one"
    );
    // And against Go, wherever a string did not pack smaller than raw.
    if case["rustByteExact"].as_bool().unwrap_or(false) {
        assert_eq!(hex(&from_typed), hex(&message), "{name}: against Go");
    }
    // What it wrote must read back into the same struct.
    assert_eq!(
        hex(&typed.encode(&typed.decode(&from_typed).unwrap()).unwrap()),
        hex(&from_typed),
        "{name}: not idempotent"
    );
}

/// Reusing a destination must leave nothing of the previous message behind.
#[test]
fn reuse_leaves_nothing_stale() {
    let codec = TypedCodec::<UsuarioToken>::new().unwrap();
    let full = codec.decode(&message_of("compact-token")).unwrap();
    let sparse = codec
        .decode(&message_of("compact-token-empty-user"))
        .unwrap();
    let full_msg = codec.encode(&full).unwrap();
    let sparse_msg = codec.encode(&sparse).unwrap();

    let mut into = UsuarioToken::colbin_zero();
    let mut buf = Vec::new();
    for _ in 0..5 {
        codec.decode_into(&full_msg, &mut into).unwrap();
        assert_eq!(into, full);
        codec.decode_into(&sparse_msg, &mut into).unwrap();
        assert_eq!(into, sparse, "a field survived a message that omits it");

        buf.clear();
        codec.append(&mut buf, &full).unwrap();
        assert_eq!(buf, full_msg);
        buf.clear();
        codec.append(&mut buf, &sparse).unwrap();
        assert_eq!(buf, sparse_msg);
    }
}

/// `#[cb(skip)]` leaves a field out of the layout entirely, as Go's `cb:"-"`
/// does — so the ids of the fields around it are the ones the Go struct without
/// it would get.
#[test]
fn skip_leaves_a_field_out() {
    #[derive(Colbin, Debug, PartialEq)]
    struct WithSkipped {
        #[cb(1)]
        kept: i32,
        #[cb(skip)]
        local: String,
        #[cb(2)]
        also_kept: i32,
    }
    assert_eq!(WithSkipped::COLBIN_FIELDS, 2);
    assert_eq!(WithSkipped::colbin_schema().unwrap().ids(), [1, 2]);

    let codec = TypedCodec::<WithSkipped>::new().unwrap();
    let value = WithSkipped {
        kept: 5,
        local: "not encoded".into(),
        also_kept: 6,
    };
    let round = codec.decode(&codec.encode(&value).unwrap()).unwrap();
    assert_eq!(round.kept, 5);
    assert_eq!(round.also_kept, 6);
    assert_eq!(round.local, "", "a skipped field reached the wire");
}

/// Negative zero survives, and a negative anywhere clears ALL_POSITIVE — the two
/// places the generated presence and sign tests could go wrong.
#[test]
fn signs_and_zeroes() {
    #[derive(Colbin, Debug, PartialEq)]
    struct Signs {
        #[cb(1)]
        a: i32,
        #[cb(2)]
        b: f64,
        #[cb(3)]
        c: u32,
    }
    let codec = TypedCodec::<Signs>::new().unwrap();

    let positive = codec.encode(&Signs { a: 5, b: 1.0, c: 7 }).unwrap();
    assert_eq!((positive[0] >> 1) & 1, 1, "ALL_POSITIVE with no negatives");

    let negative = codec
        .encode(&Signs {
            a: -5,
            b: 1.0,
            c: 7,
        })
        .unwrap();
    assert_eq!((negative[0] >> 1) & 1, 0, "ALL_POSITIVE with a negative");
    assert_eq!(codec.decode(&negative).unwrap().a, -5);

    // -0.0 is not zero: the presence test compares on the bits.
    let minus = codec
        .encode(&Signs {
            a: 0,
            b: -0.0,
            c: 0,
        })
        .unwrap();
    let plus = codec.encode(&Signs { a: 0, b: 0.0, c: 0 }).unwrap();
    assert_ne!(minus, plus, "-0.0 was omitted as a zero");
    assert_eq!(
        codec.decode(&minus).unwrap().b.to_bits(),
        (-0.0_f64).to_bits()
    );
}

/// A handle is immutable after construction, so it can be shared.
#[test]
fn is_send_and_sync() {
    fn assert_send_sync<T: Send + Sync>() {}
    assert_send_sync::<TypedCodec<BenchStats>>();

    let codec = std::sync::Arc::new(TypedCodec::<BenchStats>::new().unwrap());
    let value = BenchStats {
        quantity: 480,
        quantity_pending_delivery: 120,
        sub_quantity: 12,
        sub_quantity_pending: 3,
        sub_divisor: 24,
        total_amount: 145_900,
        total_debt_amount: 32_000,
    };
    let expected = codec.encode(&value).unwrap();
    let threads: Vec<_> = (0..4)
        .map(|_| {
            let codec = std::sync::Arc::clone(&codec);
            let value = value.clone();
            let expected = expected.clone();
            std::thread::spawn(move || {
                let mut buf = Vec::new();
                let mut out = BenchStats::colbin_zero();
                for _ in 0..200 {
                    buf.clear();
                    codec.append(&mut buf, &value).unwrap();
                    assert_eq!(buf, expected);
                    codec.decode_into(&buf, &mut out).unwrap();
                    assert_eq!(out, value);
                }
            })
        })
        .collect();
    for thread in threads {
        thread.join().expect("no thread panicked");
    }
}

/// A message whose id the layout does not have is reported, not skipped: the
/// wire holds no type tag, so there is no way to know how far to step.
#[test]
fn an_unknown_field_is_refused() {
    let codec = TypedCodec::<BenchStats>::new().unwrap();
    // A schema with an extra field writes an id BenchStats does not have.
    let wider = Codec::new(Schema::from_ids([(1, Kind::Int32), (9, Kind::Int32)]).unwrap());
    let message = wider
        .encode_one(&Record::new().with(1, Value::Int(1)).with(9, Value::Int(2)))
        .unwrap();
    assert_eq!(codec.decode(&message), Err(colbin::Error::UnknownField(9)));
}

// --- corpus plumbing ---------------------------------------------------------

fn corpus() -> Json {
    serde_json::from_str(FLAT).expect("the corpus is valid JSON")
}

fn case_named(name: &str) -> Json {
    corpus()["cases"]
        .as_array()
        .unwrap()
        .iter()
        .find(|case| case["name"] == name)
        .unwrap_or_else(|| panic!("the corpus has no case {name}"))
        .clone()
}

fn message_of(name: &str) -> Vec<u8> {
    decode_base64(case_named(name)["message"].as_str().unwrap())
}

fn hex(bytes: &[u8]) -> String {
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}
