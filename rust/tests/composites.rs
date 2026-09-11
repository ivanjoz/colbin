//! The composite corpus: compact-mode messages carrying nested structs, arrays
//! of structs, maps and nested slices.
//!
//! `vectors/composites.go` writes it, forcing every case through compact mode
//! with `colbin.MarshalForceCompact` — `Marshal` picks the mode on size for a
//! composite type, and a case that quietly came out columnar would test nothing
//! here. As in the flat corpus, every expected value was read back out of the
//! bytes by the real Go compact reader, so what is pinned is what the format
//! wrote, presence included.
//!
//! The schema is a tree here rather than a string per field, which is why this is
//! a second file and a second parser: the 26 flat vectors keep pinning the format
//! as it stood before composites existed.

use colbin::{Kind, Schema, Value};
use serde_json::Value as Json;

mod common;
use common::{assert_record, decode_base64, kind_of};

const CORPUS: &str = include_str!("../vectors/composites.json");

fn corpus() -> Json {
    serde_json::from_str(CORPUS).expect("the corpus is valid JSON")
}

/// Every composite message, decoded against the schema tree its Go type implies,
/// must yield the values and the presence the Go decoder read out of the same
/// bytes.
#[test]
fn decodes_the_go_composite_corpus() {
    let corpus = corpus();
    let cases = corpus["cases"].as_array().expect("cases is an array");
    assert!(!cases.is_empty());
    for case in cases {
        let name = case["name"].as_str().unwrap();
        let schema = schema_of(&case["fields"]);
        let message = decode_base64(case["message"].as_str().unwrap());

        // Every case is compact by construction; a case that stopped being would
        // stop testing the recursion it was written for.
        assert!(colbin::is_compact(&message), "{name} is not compact");
        // The header facts the Go writer recorded, so a key-width or sign
        // decision that drifted is caught here rather than as a value mismatch.
        assert_eq!(
            schema.narrow_keys(),
            case["narrowKeys"].as_bool().unwrap(),
            "{name}: narrow keys"
        );

        let records =
            colbin::decode(&message, &schema).unwrap_or_else(|err| panic!("{name}: {err}"));
        let want = case["records"].as_array().expect("records is an array");
        assert_eq!(records.len(), want.len(), "{name}: record count");
        for (index, (got, want)) in records.iter().zip(want).enumerate() {
            assert_record(&format!("{name} record {index}"), got, want, &schema);
        }
    }
}

/// A nested struct all of whose fields are zero is omitted entirely, so the
/// record does not name it at all — the omit-zero rule reaching into a composite
/// is worth asserting directly rather than only through a value comparison.
#[test]
fn a_zero_nested_struct_is_absent() {
    let corpus = corpus();
    let case = case_named(&corpus, "cc-nested-zero");
    let schema = schema_of(&case["fields"]);
    let message = decode_base64(case["message"].as_str().unwrap());
    let record = colbin::decode_one(&message, &schema).expect("decodes");

    assert!(record.get(2).is_none(), "the zero nested struct was named");
    assert_eq!(record.i64(1), 7);
    assert_eq!(record.str(3), "outer");
}

/// The three composite values come back as the tree the schema described, so a
/// caller reads them without knowing how they were framed.
#[test]
fn composites_decode_into_the_value_tree() {
    let corpus = corpus();

    let case = case_named(&corpus, "cc-nested-struct");
    let schema = schema_of(&case["fields"]);
    let record =
        colbin::decode_one(&decode_base64(case["message"].as_str().unwrap()), &schema).unwrap();
    let sub = record.get(2).and_then(Value::as_record).expect("a record");
    assert_eq!(sub.i64(1), -3);
    assert_eq!(sub.str(2), "inner");
    assert!(sub.bool(3));

    let case = case_named(&corpus, "cc-rows");
    let schema = schema_of(&case["fields"]);
    let record =
        colbin::decode_one(&decode_base64(case["message"].as_str().unwrap()), &schema).unwrap();
    let rows = record.get(2).and_then(Value::as_array).expect("an array");
    assert_eq!(rows.len(), 3);
    assert_eq!(rows[0].as_record().unwrap().str(2), "a");
    assert_eq!(rows[1].as_record().unwrap().i64(1), -2);
    // The third row held nothing, so its key run is a bare terminator.
    assert!(rows[2].as_record().unwrap().is_empty());

    let case = case_named(&corpus, "cc-maps");
    let schema = schema_of(&case["fields"]);
    let record =
        colbin::decode_one(&decode_base64(case["message"].as_str().unwrap()), &schema).unwrap();
    let entries = record.get(1).and_then(Value::as_map).expect("a map");
    assert_eq!(entries.len(), 1);
    assert_eq!(entries[0].0, Value::String("alpha".into()));
    assert_eq!(entries[0].1, Value::Int(-2));
}

/// A schema nested past the ceiling is refused when it is built, so the readers
/// never descend far enough to exhaust the stack.
#[test]
fn a_schema_nested_too_deep_is_refused() {
    let mut kind = Kind::Int32;
    let mut built = 0;
    loop {
        match Schema::from_ids([(1, kind.clone())]) {
            Ok(schema) => {
                built += 1;
                assert!(
                    built <= colbin::MAX_DEPTH,
                    "built {built} levels, past the ceiling"
                );
                kind = schema.nested();
            }
            Err(err) => {
                assert_eq!(err, colbin::Error::TooDeep(colbin::MAX_DEPTH + 1));
                assert_eq!(built, colbin::MAX_DEPTH);
                return;
            }
        }
    }
}

/// A composite kind in a standard-mode column is refused rather than panicked
/// on: the columnar layout carries these as sub-tables, which this decoder does
/// not read.
#[test]
fn a_composite_in_a_standard_column_is_refused() {
    let inner = Schema::from_ids([(1, Kind::Int32)]).unwrap();
    let schema = Schema::from_ids([(1, Kind::Int64), (2, inner.nested())]).unwrap();
    // A standard-mode message: byte 0 is an even version byte. The record count
    // and the first column are enough to reach field 2's descriptor.
    let message = [0x02, 4, 2, 1, 0x00, 0, 0, 0, 0, 2, 0x05];
    match colbin::decode(&message, &schema) {
        Err(colbin::Error::CompositeUnsupported { id: 2 }) => {}
        // A truncated or type-mismatched column is an equally acceptable refusal;
        // what must not happen is a panic or a silent wrong value.
        Err(_) => {}
        Ok(records) => panic!("a composite standard column decoded: {records:?}"),
    }
}

// --- the schema tree ---------------------------------------------------------

fn case_named<'a>(corpus: &'a Json, name: &str) -> &'a Json {
    corpus["cases"]
        .as_array()
        .unwrap()
        .iter()
        .find(|case| case["name"] == name)
        .unwrap_or_else(|| panic!("the corpus has no case {name}"))
}

/// Builds the schema a case describes. The ids are taken as given rather than
/// rehashed: `field_ids_match_the_go_layout` in the flat corpus is what pins the
/// hash, and every struct here is tagged or deliberately not.
fn schema_of(fields: &Json) -> Schema {
    let specs: Vec<(u8, Kind)> = fields
        .as_array()
        .expect("fields is an array")
        .iter()
        .map(|field| {
            let id = field["id"].as_u64().expect("a field id") as u8;
            (id, kind_tree(&field["k"]))
        })
        .collect();
    Schema::from_ids(specs).expect("the corpus describes a valid schema")
}

/// One kind node, which for the three composite forms holds kinds of its own.
fn kind_tree(node: &Json) -> Kind {
    match node["kind"].as_str().expect("a kind name") {
        "struct" => schema_of(&node["fields"]).nested(),
        "array" => Kind::Array(Box::new(kind_tree(&node["elem"]))),
        "map" => Kind::Map(
            Box::new(kind_tree(&node["key"])),
            Box::new(kind_tree(&node["val"])),
        ),
        scalar => kind_of(scalar),
    }
}
