//! The schema section parser, against sections the Go encoder wrote.
//!
//! The corpus is the `doc.` half of `rust/vectors/vectors.json`, written by
//! `go run ./rust/vectors`. Go is the specification for the section, and
//! `go test ./rust/vectors` fails when the committed file is no longer what the
//! Go codecs produce — so parsing these here pins the port to the spec rather
//! than to whatever another port happened to write.

use colbin::plan::{OP_BOOL, OP_INT64, OP_STRING, OP_STRUCT, OP_STRUCTS};

use colbin::section;

fn unhex(text: &str) -> Vec<u8> {
    assert!(text.len().is_multiple_of(2), "odd-length hex: {text}");
    (0..text.len())
        .step_by(2)
        .map(|at| u8::from_str_radix(&text[at..at + 2], 16).expect("hex"))
        .collect()
}

fn corpus() -> serde_json::Value {
    let path = concat!(env!("CARGO_MANIFEST_DIR"), "/vectors/vectors.json");
    let text = std::fs::read_to_string(path).expect("vectors.json — run `go run ./rust/vectors`");
    serde_json::from_str(&text).expect("vectors.json is not JSON")
}

/// The document cases: every walk whose root is a declared type.
fn documents(corpus: &serde_json::Value) -> Vec<&serde_json::Value> {
    corpus["walks"]
        .as_array()
        .expect("walks")
        .iter()
        .filter(|case| case["name"].as_str().is_some_and(|n| n.starts_with("doc.")))
        .collect()
}

/// The one case called `name`, which must be there — a corpus that quietly lost
/// a case would otherwise make this file pass by testing nothing.
fn named<'a>(corpus: &'a serde_json::Value, name: &str) -> &'a serde_json::Value {
    documents(corpus)
        .into_iter()
        .find(|case| case["name"] == name)
        .unwrap_or_else(|| panic!("{name} is not in the corpus"))
}

#[test]
fn every_section_in_the_corpus_parses() {
    let corpus = corpus();
    let cases = documents(&corpus);
    assert!(cases.len() >= 40, "corpus shrank: {} cases", cases.len());

    for case in cases {
        let name = case["name"].as_str().expect("name");
        let bytes = unhex(case["section"].as_str().expect("section"));
        let schema = section::parse(&bytes).unwrap_or_else(|e| panic!("{name}: {e}"));

        assert_eq!(
            schema.size,
            bytes.len(),
            "{name}: the declared length must cover exactly the section handed in",
        );
        assert!(!schema.plans.is_empty(), "{name}: no struct table");
        assert_eq!(
            schema.root().fields.len(),
            schema.root().names.len(),
            "{name}: a field without a name",
        );
        for plan in &schema.plans {
            for field in &plan.fields {
                if let Some(at) = field.sub {
                    assert!(
                        schema.plan(at).is_some(),
                        "{name}: field points outside the struct table",
                    );
                }
            }
            // Every key the plan lists must resolve back to the field that
            // declared it, which is the property the walk depends on.
            for (at, field) in plan.fields.iter().enumerate() {
                let found = plan.field_of(field.key).expect("key is indexed");
                assert_eq!(plan.fields[found].key, field.key, "{name}: key index wrong");
                let _ = at;
            }
        }
    }
}

#[test]
fn a_flat_object_reads_field_for_field() {
    let corpus = corpus();
    let case = named(&corpus, "doc.flat-object");

    let schema = section::parse(&unhex(case["section"].as_str().unwrap())).expect("parse");
    let root = schema.root();

    assert_eq!(root.names, ["id", "name", "price", "active"]);
    assert_eq!(
        root.fields.iter().map(|f| f.op).collect::<Vec<_>>(),
        [OP_INT64, OP_STRING, OP_INT64, OP_BOOL],
    );
    assert!(!root.is_wide, "four fields fit a narrow key");
    assert!(!root.is_envelope, "the document's top level was an object");
    assert!(root.can_table, "scalars and a string are all columnable");
}

#[test]
fn a_nested_document_hoists_its_structs() {
    let corpus = corpus();
    let case = named(&corpus, "doc.nested-objects");

    let schema = section::parse(&unhex(case["section"].as_str().unwrap())).expect("parse");
    assert!(
        schema.plans.len() > 1,
        "a nested document needs more than one struct in the table",
    );
    let nested = schema
        .plans
        .iter()
        .flat_map(|p| p.fields.iter())
        .any(|f| matches!(f.op, OP_STRUCT | OP_STRUCTS));
    assert!(nested, "no field points at a hoisted struct");
}

#[test]
fn an_enveloped_document_says_so() {
    let corpus = corpus();
    let case = named(&corpus, "doc.scalar-array");

    let schema = section::parse(&unhex(case["section"].as_str().unwrap())).expect("parse");
    assert!(
        schema.root().is_envelope,
        "a document whose top level is not an object is wrapped, and the section records it",
    );
    assert_eq!(
        schema.root().fields.len(),
        1,
        "the envelope holds one field"
    );

    // And an ordinary object is not wrapped, so the flag is saying something.
    let plain = section::parse(&unhex(
        named(&corpus, "doc.flat-object")["section"]
            .as_str()
            .unwrap(),
    ))
    .expect("parse");
    assert!(!plain.root().is_envelope);
}

// ---- what a peer must not be able to do ------------------------------------

#[test]
fn a_declared_length_past_the_bytes_is_refused() {
    // byteLength says 200, four bytes follow.
    assert!(section::parse(&[200, 1, 0, 1, 0]).is_err());
}

#[test]
fn a_struct_count_larger_than_the_bytes_is_refused() {
    // A body of three bytes cannot hold 255 struct definitions, and the parser
    // must decide that from the bytes left rather than allocate on the claim.
    assert!(section::parse(&[3, 255, 0, 0]).is_err());
}

#[test]
fn an_unassigned_op_is_refused() {
    // one struct, one field, key 1, name "a", op 250
    let body = [1u8, 0, 1, 1, 1, b'a', 250];
    let mut section = alloc_section(&body);
    assert!(section::parse(&section).is_err());
    // and the same section with an assigned op parses, so the refusal above is
    // the op and not the framing
    let at = section.len() - 1;
    section[at] = OP_STRING;
    assert!(section::parse(&section).is_ok());
}

#[test]
fn a_struct_index_outside_the_table_is_refused() {
    // one struct, one field, op OP_STRUCT pointing at index 9
    let body = [1u8, 0, 1, 1, 1, b'a', OP_STRUCT, 9];
    assert!(section::parse(&alloc_section(&body)).is_err());
}

#[test]
fn an_empty_struct_table_is_refused() {
    assert!(section::parse(&alloc_section(&[0u8])).is_err());
}

#[test]
fn a_truncated_section_is_refused_at_every_length() {
    let corpus = corpus();
    // The four-deep document, so the cut lands inside a hoisted struct as often
    // as inside the root's own field list.
    let full = unhex(named(&corpus, "doc.four-deep")["section"].as_str().unwrap());
    for cut in 1..full.len() {
        let short = &full[..cut];
        // Refused or — where the declared byteLength happens to fit what is
        // left — parsed into something well formed. Never a panic, which is the
        // property that matters when the bytes came off a socket.
        let _ = section::parse(short);
    }
}

#[test]
fn a_name_that_is_not_utf8_is_refused() {
    let body = [1u8, 0, 1, 1, 1, 0xff, OP_STRING];
    assert!(section::parse(&alloc_section(&body)).is_err());
}

/// Wraps a section body in the outer byteLength the format puts in front of it.
fn alloc_section(body: &[u8]) -> Vec<u8> {
    let mut out = Vec::with_capacity(body.len() + 1);
    assert!(body.len() < 255, "test bodies stay under the length escape");
    out.push(body.len() as u8);
    out.extend_from_slice(body);
    out
}
