//! The cross-language corpus: every message here was written by the Go codecs,
//! and every value here is the Rust mirror of the Go value that produced it.
//!
//! Both directions are asserted — this port must decode those bytes to this
//! value, and encode this value to those bytes — so neither side can move
//! without the other failing. Regenerate with `go run ./rust/vectors`, which
//! `go test ./rust/vectors` fails if you forget.
//!
//! The structs below mirror `rust/vectors/main.go` field for field and id for
//! id. Keeping them in step by hand is the price of a typed decoder; the corpus
//! is what makes forgetting loud.

use std::collections::{BTreeMap, HashMap};

use colbin::Colbin;

const CORPUS: &str = include_str!("../vectors/vectors.json");

#[derive(Colbin, Debug, PartialEq)]
struct Charge {
    #[cb(0)]
    company_id: u32,
    #[cb(1)]
    user_id: u32,
    #[cb(2)]
    route_id: u32,
    #[cb(3)]
    cpu: u8,
    #[cb(4)]
    memory: u16,
    #[cb(5)]
    duration: i64,
    #[cb(6)]
    access_1: u16,
    #[cb(7)]
    access_2: u16,
    #[cb(8)]
    created: i64,
    #[cb(9)]
    updated: i64,
}

#[derive(Colbin, Debug, PartialEq)]
struct Scalars {
    #[cb(0)]
    flag: bool,
    #[cb(1)]
    tiny: i8,
    #[cb(2)]
    small: i16,
    #[cb(3)]
    medium: i32,
    #[cb(4)]
    large: i64,
    #[cb(5)]
    byte: u8,
    #[cb(6)]
    half: u16,
    #[cb(7)]
    word: u32,
    #[cb(8)]
    giant: u64,
    #[cb(9)]
    single: f32,
    #[cb(10)]
    double: f64,
    #[cb(11)]
    text: String,
}

#[derive(Colbin, Debug, PartialEq)]
struct Arrays {
    #[cb(0)]
    blob: Vec<u8>,
    #[cb(1)]
    tiny: Vec<i8>,
    #[cb(2)]
    small: Vec<i16>,
    #[cb(3)]
    medium: Vec<i32>,
    #[cb(4)]
    large: Vec<i64>,
    #[cb(5)]
    halves: Vec<u16>,
    #[cb(6)]
    words: Vec<u32>,
    #[cb(7)]
    giants: Vec<u64>,
    #[cb(8)]
    texts: Vec<String>,
}

#[derive(Colbin, Debug, PartialEq)]
struct Optionals {
    #[cb(0)]
    maybe_int: Option<i32>,
    #[cb(1)]
    maybe_uint: Option<u32>,
    #[cb(2)]
    maybe_text: Option<String>,
    #[cb(3)]
    maybe_flag: Option<bool>,
    #[cb(4)]
    maybe_float: Option<f64>,
}

#[derive(Colbin, Debug, PartialEq, Clone)]
struct Line {
    #[cb(0)]
    sku: String,
    #[cb(1)]
    quantity: i32,
    #[cb(2)]
    price: f64,
}

#[derive(Colbin, Debug, PartialEq)]
struct Order {
    #[cb(0)]
    id: u32,
    #[cb(1)]
    customer: Line,
    #[cb(2)]
    lines: Vec<Line>,
    #[cb(3)]
    note: String,
}

#[derive(Colbin, Debug, PartialEq)]
struct Maps {
    #[cb(0)]
    labels: BTreeMap<String, String>,
    #[cb(1)]
    counts: BTreeMap<i64, f64>,
    #[cb(2)]
    flags: HashMap<String, bool>,
    #[cb(3)]
    sizes: BTreeMap<u32, f32>,
}

#[derive(Colbin, Debug, PartialEq)]
struct WideEvolved {
    #[cb(0)]
    first: u32,
    #[cb(200)]
    last: String,
    #[cb(201)]
    added: Vec<i32>,
    #[cb(202)]
    also: f64,
}

#[derive(Colbin, Debug, PartialEq)]
struct Wide {
    #[cb(0)]
    first: u32,
    #[cb(200)]
    last: String,
}

/// Every shape a `u16` takes on the wide key. Go's `U16` picks between the
/// varint and the byte-count form with one comparison against a constant, which
/// only holds if it lands on the same boundary this port computes.
#[derive(Colbin, Debug, PartialEq)]
struct WideWidths {
    #[cb(20)]
    inline: u16,
    #[cb(21)]
    one_byte: u16,
    #[cb(22)]
    varint: u16,
    #[cb(23)]
    tie: u16,
    #[cb(24)]
    max: u16,
}

#[derive(Colbin, Debug, PartialEq)]
struct Hashed {
    #[cb(name = "CompanyID")]
    company_id: i32,
    #[cb(name = "User")]
    user: String,
    #[cb(7)]
    pinned: u32,
    #[cb(skip)]
    ignored: u64,
}

#[derive(Colbin, Debug, PartialEq)]
#[cb(packed5)]
struct PackedText {
    #[cb(0)]
    text: String,
}

/// The corpus, parsed once.
fn corpus() -> serde_json::Value {
    serde_json::from_str(CORPUS).expect("the corpus is JSON")
}

/// The message a named case holds.
fn message(name: &str) -> Vec<u8> {
    let corpus = corpus();
    let case = corpus["cases"]
        .as_array()
        .expect("cases")
        .iter()
        .find(|case| case["name"] == name)
        .unwrap_or_else(|| panic!("the corpus has no case {name}"));
    let hex = case["message"].as_str().expect("a hex message");
    (0..hex.len())
        .step_by(2)
        .map(|at| u8::from_str_radix(&hex[at..at + 2], 16).expect("hex"))
        .collect()
}

/// Asserts both directions for a value that survives a round trip unchanged.
fn check<T: Colbin + PartialEq + core::fmt::Debug>(name: &str, value: &T) {
    check_into(name, value, value);
}

/// Asserts both directions where the decoded value differs from the encoded one
/// — which happens only for a field the type does not carry, and so cannot hold
/// after a decode.
fn check_into<T: Colbin + PartialEq + core::fmt::Debug>(name: &str, value: &T, decoded: &T) {
    let expected = message(name);
    assert_eq!(
        hex(&value.encode()),
        hex(&expected),
        "{name}: this port encoded something else"
    );
    assert_eq!(
        &T::decode(&expected).expect("decode"),
        decoded,
        "{name}: this port decoded something else"
    );
}

fn hex(bytes: &[u8]) -> String {
    bytes.iter().map(|byte| format!("{byte:02x}")).collect()
}

fn lines(count: usize) -> Vec<Line> {
    (0..count)
        .map(|index| Line {
            sku: format!("SKU-{index}"),
            quantity: index as i32,
            price: index as f64 * 1.5,
        })
        .collect()
}

#[test]
fn charge_cases() {
    check("charge.zero", &Charge::colbin_zero());
    check(
        "charge.benchmark",
        &Charge {
            company_id: 7,
            user_id: 42,
            route_id: 103,
            cpu: 5,
            access_1: 0x0139,
            ..Charge::colbin_zero()
        },
    );
    check(
        "charge.wide values",
        &Charge {
            company_id: 70_000,
            user_id: 4_000_000_000,
            route_id: 255,
            cpu: 255,
            memory: 65535,
            duration: -1,
            access_1: 256,
            access_2: 128,
            created: 1_700_000_000_000,
            updated: -1_700_000_000_000,
        },
    );
}

#[test]
fn scalar_cases() {
    check("scalars.zero", &Scalars::colbin_zero());
    check(
        "scalars.extremes",
        &Scalars {
            flag: true,
            tiny: i8::MIN,
            small: i16::MIN,
            medium: i32::MIN,
            large: i64::MIN,
            byte: u8::MAX,
            half: u16::MAX,
            word: u32::MAX,
            giant: u64::MAX,
            single: 1.0,
            double: -0.5,
            text: "el niño comió jamón".into(),
        },
    );
    check(
        "scalars.float trim",
        &Scalars {
            single: 1.0,
            double: 1.5,
            ..Scalars::colbin_zero()
        },
    );
}

#[test]
fn array_cases() {
    check("arrays.empty", &Arrays::colbin_zero());
    check(
        "arrays.mixed",
        &Arrays {
            blob: vec![0, 1, 2, 255],
            tiny: vec![i8::MIN, 0, i8::MAX],
            small: vec![i16::MIN, 0, i16::MAX],
            medium: vec![i32::MIN, 0, i32::MAX],
            large: vec![i64::MIN, 0, i64::MAX],
            halves: vec![0, u16::MAX],
            words: vec![0, u32::MAX],
            giants: vec![0, u64::MAX],
            texts: vec![String::new(), "ok".into(), "el niño".into()],
        },
    );
    check(
        "arrays.long blob",
        &Arrays {
            blob: vec![0; 3000],
            ..Arrays::colbin_zero()
        },
    );
    check(
        "arrays.long strings",
        &Arrays {
            texts: vec!["a".repeat(300), "short".into()],
            ..Arrays::colbin_zero()
        },
    );
}

#[test]
fn optional_cases() {
    check("optionals.nil", &Optionals::colbin_zero());
    check(
        "optionals.zero",
        &Optionals {
            maybe_int: Some(0),
            maybe_uint: Some(0),
            maybe_text: Some(String::new()),
            maybe_flag: Some(false),
            maybe_float: Some(0.0),
        },
    );
    check(
        "optionals.set",
        &Optionals {
            maybe_int: Some(-5),
            maybe_uint: Some(70_000),
            maybe_text: Some("x".into()),
            maybe_flag: Some(true),
            maybe_float: Some(2.5),
        },
    );
}

#[test]
fn composite_cases() {
    check(
        "order.nested",
        &Order {
            id: 90210,
            customer: Line {
                sku: "ACME".into(),
                quantity: 1,
                price: 0.0,
            },
            lines: lines(3),
            note: "ship soon".into(),
        },
    );
    check(
        "order.table",
        &Order {
            id: 1,
            lines: lines(40),
            ..Order::colbin_zero()
        },
    );
    check(
        "order.empty list",
        &Order {
            id: 2,
            ..Order::colbin_zero()
        },
    );
}

#[test]
fn map_cases() {
    check("maps.empty", &Maps::colbin_zero());
    let mut maps = Maps::colbin_zero();
    maps.labels.insert("a".into(), "one".into());
    maps.counts.insert(-1, 2.5);
    maps.flags.insert("on".into(), true);
    maps.sizes.insert(7, 1.5);
    check("maps.one entry each", &maps);
}

#[test]
fn wide_key_cases() {
    check(
        "wide.evolved",
        &WideEvolved {
            first: 9,
            last: "tail".into(),
            added: vec![1, 2, 3],
            also: 1.5,
        },
    );
    check(
        "wide.old peer",
        &Wide {
            first: 9,
            last: "tail".into(),
        },
    );
    check(
        "wide.integer widths",
        &WideWidths {
            inline: 100,
            one_byte: 200,
            varint: 313,
            tie: 1024,
            max: 65535,
        },
    );
    // The same message, read by a type that has never heard of its last two
    // fields. Only the wide width can do this.
    assert_eq!(
        Wide::decode(&message("wide.evolved")).expect("skip the unknown fields"),
        Wide {
            first: 9,
            last: "tail".into()
        }
    );
}

#[test]
fn hashed_id_cases() {
    check_into(
        "hashed.derived ids",
        &Hashed {
            company_id: -3,
            user: "ana".into(),
            pinned: 1000,
            ignored: 7,
        },
        &Hashed {
            company_id: -3,
            user: "ana".into(),
            pinned: 1000,
            // Not carried, so a decode leaves it zero — which is what Go does to
            // a `cb:"-"` field too.
            ignored: 0,
        },
    );
}

#[test]
fn packed5_cases() {
    check(
        "packed5.string",
        &PackedText {
            text: "el niño comió jamón".into(),
        },
    );
    check(
        "packed5.does not pack",
        &PackedText {
            text: "{\"id\":1023}".into(),
        },
    );
}

/// The blocked column codec on its own, across the shapes a table only
/// exercises by accident: the transform search, the per-block widths, the
/// partial last block, and the element width that comes from the type rather
/// than from the wire.
#[test]
fn column_cases() {
    let corpus = corpus();
    let columns = corpus["columns"].as_array().expect("columns");
    assert!(!columns.is_empty(), "the corpus holds no columns");

    for case in columns {
        let name = case["name"].as_str().expect("a name");
        let width = case["width"].as_u64().expect("a width");
        let expected = case["encoded"].as_str().expect("hex");
        let values: Vec<i64> = case["values"]
            .as_array()
            .expect("values")
            .iter()
            .map(|value| value.as_i64().expect("an integer"))
            .collect();

        // The width is not on the wire: encoder and decoder both derive it from
        // the element type, so the check has to instantiate at the same one.
        match width {
            1 => check_column::<i8>(name, &values, expected),
            2 => check_column::<i16>(name, &values, expected),
            4 => check_column::<i32>(name, &values, expected),
            _ => check_column::<i64>(name, &values, expected),
        }
    }
}

fn check_column<T>(name: &str, values: &[i64], expected: &str)
where
    T: colbin::column::Signed + PartialEq + core::fmt::Debug,
{
    let narrowed: Vec<T> = values.iter().map(|value| T::from_i64(*value)).collect();

    let mut encoded = Vec::new();
    colbin::column::append_array(&mut encoded, &narrowed);
    assert_eq!(
        hex(&encoded),
        expected,
        "{name}: this port encoded the column differently"
    );

    let raw = (0..expected.len())
        .step_by(2)
        .map(|at| u8::from_str_radix(&expected[at..at + 2], 16).expect("hex"))
        .collect::<Vec<u8>>();
    let mut out = vec![T::from_i64(0); narrowed.len()];
    let read = colbin::column::decode_array(&raw, narrowed.len(), &mut out)
        .unwrap_or_else(|err| panic!("{name}: decode: {err}"));
    assert_eq!(
        out, narrowed,
        "{name}: this port decoded the column differently"
    );
    assert_eq!(read, raw.len(), "{name}: the decode left bytes behind");
}

/// The ids a type resolves to are what the two ports have to agree on, and they
/// are computed rather than declared for any field without an explicit number.
/// This is the hash and the probe, pinned against Go's answer.
#[test]
fn derived_field_ids_match_go() {
    let corpus = corpus();
    let hashed = corpus["fieldIds"]
        .as_array()
        .expect("fieldIds")
        .iter()
        .find(|entry| entry["type"] == "main.Hashed")
        .expect("main.Hashed");

    let ids = colbin::assign_ids(&["CompanyID", "User", "Pinned"], &[-1, -1, 7]);
    for (index, name) in ["CompanyID", "User", "Pinned"].iter().enumerate() {
        assert_eq!(
            i64::from(ids[index]),
            hashed["ids"][name].as_i64().expect("an id"),
            "{name}"
        );
    }
    assert_eq!(colbin::fnv8("CompanyID"), 202);
}
