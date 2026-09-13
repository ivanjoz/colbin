//! The schema-driven walk, against the documents Go encoded and rendered.
//!
//! The corpus is the `doc.` half of `rust/vectors/vectors.json`, written by
//! `go run ./rust/vectors` from real Go values. Every case has a section, a
//! message and the JSON `colbin.ToJSON` produced from the two, so matching it
//! here is agreeing with the specification rather than with a second opinion.
//!
//! These cases used to come from `web/vectors/web_encoded.json`, which only the
//! AssemblyScript module writes — so this file could not run without first
//! building a module in another language. The shapes are the same ones
//! `web/tests/documents.mjs` drives that module over; only the hand that writes
//! them changed.

use colbin::{section, walk};

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

/// The document cases, which are the ones with a statically typed root — the
/// `dynamic.` half is `dynamic.rs`'s subject.
fn documents(corpus: &serde_json::Value) -> Vec<&serde_json::Value> {
    corpus["walks"]
        .as_array()
        .expect("walks")
        .iter()
        .filter(|case| case["name"].as_str().is_some_and(|n| n.starts_with("doc.")))
        .collect()
}

/// One case, unpacked into what a reader without the Go type is handed: a
/// parsed section, the body after the root byte, and the key width.
fn delivery(case: &serde_json::Value) -> (section::Schema, Vec<u8>, bool) {
    let name = case["name"].as_str().expect("name");
    let schema = section::parse(&unhex(case["section"].as_str().expect("section")))
        .unwrap_or_else(|e| panic!("{name}: {e}"));
    let message = unhex(case["message"].as_str().expect("message"));
    let wide = case["wide"].as_bool().expect("wide");
    (schema, message[1..].to_vec(), wide)
}

#[test]
fn every_document_renders_the_json_go_renders() {
    let corpus = corpus();
    let cases = documents(&corpus);
    assert!(cases.len() >= 40, "corpus shrank: {} cases", cases.len());

    let mut unsupported = Vec::new();
    for case in &cases {
        let name = case["name"].as_str().expect("name");
        let want = case["json"].as_str().expect("json");
        let (schema, body, wide) = delivery(case);

        match walk::to_json(&schema, &body, wide) {
            Ok(got) => {
                let got = String::from_utf8(got).expect("the walk writes UTF-8");
                assert_eq!(got, want, "{name}: the walk disagrees with Go");
            }
            Err(colbin::Error::Unsupported) => unsupported.push(name),
            Err(e) => panic!("{name}: {e}"),
        }
    }

    assert!(
        unsupported.is_empty(),
        "cases this decoder cannot render yet: {unsupported:?}",
    );
}

#[test]
fn a_truncated_message_is_refused_rather_than_panicking() {
    let corpus = corpus();
    for case in documents(&corpus) {
        let (schema, body, wide) = delivery(case);
        // Every prefix. A short read must become an error, never a panic and
        // never a read past the buffer — which is the property that matters when
        // the bytes came off a socket.
        for cut in 0..body.len() {
            let _ = walk::to_json(&schema, &body[..cut], wide);
        }
    }
}

#[test]
fn a_corrupted_byte_is_refused_or_renders_something_well_formed() {
    let corpus = corpus();
    let cases = documents(&corpus);
    // A table and a plain struct: the two dispatch differently, and a flipped
    // byte in a column header is a different kind of wrong from one in a field
    // descriptor.
    let chosen = ["doc.flat-object", "doc.records-past-the-table-threshold"];

    let mut seen = 0;
    for want in chosen {
        let case = cases
            .iter()
            .find(|case| case["name"] == want)
            .unwrap_or_else(|| panic!("{want} is not in the corpus"));
        let (schema, body, wide) = delivery(case);

        for at in 0..body.len() {
            for bit in 0..8u32 {
                let mut broken = body.clone();
                broken[at] ^= 1 << bit;
                if let Ok(text) = walk::to_json(&schema, &broken, wide) {
                    // Whatever it decoded to, it must still be JSON: a decoder
                    // that emits a half-written document on bad input is worse
                    // than one that refuses.
                    serde_json::from_slice::<serde_json::Value>(&text)
                        .expect("a successful decode must produce valid JSON");
                }
                seen += 1;
            }
        }
    }
    assert!(seen > 0);
}
