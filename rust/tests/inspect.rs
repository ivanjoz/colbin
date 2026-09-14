//! The span walk, against the messages Go wrote and the ones this crate writes.
//!
//! The invariant that makes a second walk safe rather than a liability: **the
//! spans tile the body exactly**. A key run has no padding and an omitted field
//! writes nothing, so the fields a message *did* write are contiguous from the
//! first byte of the body to the last. If this walk consumed a field differently
//! from the decoder, a gap or an overlap appears here.

use colbin::inspect;
use colbin::section;

fn unhex(text: &str) -> Vec<u8> {
    assert!(text.len().is_multiple_of(2), "odd-length hex: {text}");
    (0..text.len())
        .step_by(2)
        .map(|at| u8::from_str_radix(&text[at..at + 2], 16).expect("hex"))
        .collect()
}

fn go_types() -> Vec<serde_json::Value> {
    let path = concat!(env!("CARGO_MANIFEST_DIR"), "/../js/vectors/vectors.json");
    let text = std::fs::read_to_string(path).expect("js/vectors/vectors.json");
    let held: serde_json::Value = serde_json::from_str(&text).expect("vectors.json is JSON");
    held["types"].as_array().expect("types").clone()
}

fn report(section_bytes: Option<&[u8]>, message: &[u8]) -> serde_json::Value {
    let schema = section_bytes.map(|bytes| section::parse(bytes).expect("section"));
    let json = inspect::inspect(message, schema.as_ref()).unwrap_or_else(|e| panic!("{e}"));
    serde_json::from_slice(&json).expect("inspect writes JSON")
}

/// Every node's span sits inside its parent's, children are ascending and do not
/// overlap, and — when `exhaustive` — the top level covers the body with nothing
/// left over.
fn check_tiling(report: &serde_json::Value, what: &str) {
    let body_start = report["rootBytes"].as_u64().expect("rootBytes")
        + report["schemaBytes"].as_u64().expect("schemaBytes");
    let body_end = report["totalBytes"].as_u64().expect("totalBytes");
    walk_spans(
        report["fields"].as_array().expect("fields"),
        body_start,
        body_end,
        what,
        true,
    );
}

fn walk_spans(nodes: &[serde_json::Value], from: u64, to: u64, what: &str, exhaustive: bool) {
    let mut cursor = from;
    for node in nodes {
        let name = node["name"].as_str().unwrap_or("?");
        let start = node["start"].as_u64().expect("start");
        let end = node["end"].as_u64().expect("end");
        let bytes = node["bytes"].as_u64().expect("bytes");
        assert!(end >= start, "{what}: {name} ends before it starts");
        assert!(
            start >= from && end <= to,
            "{what}: {name} escapes its parent"
        );
        assert!(
            start >= cursor,
            "{what}: {name} at {start} overlaps what came before, at {cursor}"
        );
        assert_eq!(bytes, end - start, "{what}: {name} bytes disagree");
        cursor = end;
        if let Some(children) = node["children"].as_array()
            && !children.is_empty()
        {
            walk_spans(children, start, end, &format!("{what} > {name}"), false);
        }
    }
    if exhaustive {
        assert_eq!(
            cursor,
            to,
            "{what}: the fields cover {} of {} bytes",
            cursor - from,
            to - from
        );
    }
}

#[test]
fn the_go_corpus_tiles() {
    let cases = go_types();
    assert!(cases.len() >= 20, "only {} cases", cases.len());
    for case in &cases {
        let name = case["name"].as_str().expect("name");
        let section = unhex(case["section"].as_str().expect("section"));
        let message = unhex(case["message"].as_str().expect("message"));
        let tree = report(Some(&section), &message);
        assert_eq!(
            tree["schemaBytes"].as_u64().unwrap(),
            0,
            "{name}: an out-of-band message carries no section"
        );
        check_tiling(&tree, name);

        let standalone = unhex(case["selfDescribing"].as_str().expect("selfDescribing"));
        let tree = report(None, &standalone);
        assert!(
            tree["schemaBytes"].as_u64().unwrap() > 0,
            "{name}: a self-describing message carries a section"
        );
        check_tiling(&tree, name);
    }
}

#[test]
fn a_table_reports_its_columns_not_its_rows() {
    let case = go_types()
        .into_iter()
        .find(|c| c["name"] == "table-wide")
        .expect("table-wide");
    let tree = report(
        Some(&unhex(case["section"].as_str().unwrap())),
        &unhex(case["message"].as_str().unwrap()),
    );
    let table = tree["fields"]
        .as_array()
        .unwrap()
        .iter()
        .find(|f| f["type"] == "[]struct")
        .expect("the table field should be in the tree");
    let children = table["children"].as_array().unwrap();
    assert_eq!(children.len(), 3, "300 rows of three fields: three columns");
    for column in children {
        assert!(
            column["type"].as_str().unwrap().ends_with(" column"),
            "{}",
            column["type"]
        );
    }
    assert_eq!(tree["rows"].as_u64().unwrap(), 300);
}

#[test]
fn a_list_reports_its_elements() {
    let case = go_types()
        .into_iter()
        .find(|c| c["name"] == "list")
        .expect("list");
    let tree = report(
        Some(&unhex(case["section"].as_str().unwrap())),
        &unhex(case["message"].as_str().unwrap()),
    );
    let list = tree["fields"]
        .as_array()
        .unwrap()
        .iter()
        .find(|f| f["type"] == "[]struct")
        .expect("the list field should be in the tree");
    let children = list["children"].as_array().unwrap();
    assert_eq!(children.len(), 3, "three elements, each with a span");
    for element in children {
        assert!(
            !element["children"].as_array().unwrap().is_empty(),
            "an element shows its own fields"
        );
    }
}

#[test]
fn the_tree_names_the_wire_shape_of_every_field() {
    let case = go_types()
        .into_iter()
        .find(|c| c["name"] == "scalars")
        .expect("scalars");
    let tree = report(
        Some(&unhex(case["section"].as_str().unwrap())),
        &unhex(case["message"].as_str().unwrap()),
    );
    let named: std::collections::HashMap<String, String> = tree["fields"]
        .as_array()
        .unwrap()
        .iter()
        .map(|f| {
            (
                f["name"].as_str().unwrap().to_string(),
                f["type"].as_str().unwrap().to_string(),
            )
        })
        .collect();
    assert_eq!(named["Flag"], "bool");
    assert_eq!(named["Large"], "int64");
    assert_eq!(named["Giant"], "uint64");
    assert_eq!(named["Single"], "float32");
    assert_eq!(named["Text"], "string");
    assert_eq!(named["Blob"], "bytes");
}

#[test]
fn a_pointer_field_is_marked_optional() {
    let case = go_types()
        .into_iter()
        .find(|c| c["name"] == "optionals-zero")
        .expect("optionals-zero");
    let tree = report(
        Some(&unhex(case["section"].as_str().unwrap())),
        &unhex(case["message"].as_str().unwrap()),
    );
    let fields = tree["fields"].as_array().unwrap();
    assert!(!fields.is_empty());
    for field in fields {
        assert_eq!(field["optional"], true, "{}", field["name"]);
    }
}

#[cfg(feature = "encode")]
#[test]
fn an_envelope_says_so() {
    let mut diag = colbin::diag::Diag::new();
    let encoded =
        colbin::build::encode(b"[1,2,3]", colbin::build::VERIFY, &mut diag).expect("encode");
    let tree = report(Some(&encoded.section), &encoded.message);
    assert_eq!(tree["envelope"], true);
    assert_eq!(tree["fields"].as_array().unwrap().len(), 1);
    assert_eq!(tree["fields"][0]["name"], "rows");
}

/// A self-describing message is described by the section it carries, even when
/// the caller is holding one of its own — which is what `walk::to_json` does for
/// the same bytes under `decode`. The two disagreeing would put the spans on one
/// reading of the body and the rendered values on another, and the page's hex
/// view would highlight bytes the values did not come from.
#[test]
fn a_carried_section_beats_a_held_one() {
    let cases = go_types();
    let mine = cases
        .iter()
        .find(|c| c["name"] == "corpus-product")
        .expect("corpus-product");
    let stranger = cases
        .iter()
        .find(|c| c["name"] == "corpus-sale-table")
        .expect("corpus-sale-table");

    let standalone = unhex(mine["selfDescribing"].as_str().expect("selfDescribing"));
    let alone = report(None, &standalone);
    let held = report(
        Some(&unhex(stranger["section"].as_str().expect("section"))),
        &standalone,
    );
    assert_eq!(alone, held, "a held section changed the reading");
    check_tiling(&held, "corpus-product under a stranger's section");
}

#[test]
fn what_the_encoder_writes_tiles() {
    let corpus = {
        let path = concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/../js/vectors/web_encoded.json"
        );
        let text = std::fs::read_to_string(path).expect("web_encoded.json");
        serde_json::from_str::<serde_json::Value>(&text).expect("JSON")
    };
    for case in corpus["cases"].as_array().expect("cases") {
        let name = case["name"].as_str().expect("name");
        let section = unhex(case["section"].as_str().expect("section"));
        let message = unhex(case["message"].as_str().expect("message"));
        check_tiling(&report(Some(&section), &message), name);
    }
}

#[test]
fn inspect_refuses_what_decode_refuses() {
    for bytes in [&[][..], &[0x00][..], &[0xd1, 0x00][..], &[0xd4, 0xff][..]] {
        assert!(inspect::inspect(bytes, None).is_err(), "accepted {bytes:?}");
    }
}

#[test]
fn every_truncation_is_a_diagnostic_or_a_tiling_tree() {
    let cases = go_types();
    for name in ["corpus-sale-table", "corpus-product", "list", "table"] {
        let case = cases
            .iter()
            .find(|c| c["name"] == name)
            .unwrap_or_else(|| panic!("{name}"));
        let full = unhex(case["selfDescribing"].as_str().unwrap());
        for cut in 1..full.len() {
            match inspect::inspect(&full[..cut], None) {
                Err(_) => {}
                Ok(json) => {
                    let tree: serde_json::Value = serde_json::from_slice(&json).expect("JSON");
                    let start =
                        tree["rootBytes"].as_u64().unwrap() + tree["schemaBytes"].as_u64().unwrap();
                    walk_spans(
                        tree["fields"].as_array().unwrap(),
                        start,
                        cut as u64,
                        &format!("{name}@{cut}"),
                        false,
                    );
                }
            }
        }
    }
}
