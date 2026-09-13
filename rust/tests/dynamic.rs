//! `map[string]any` and the other dynamic shapes, against what Go renders.
//!
//! The corpus is `rust/vectors/vectors.json`, written by `go run ./rust/vectors`
//! from real Go values. Unlike the `walk.rs` corpus this one needs nothing but
//! Go — no third implementation in the loop — which is what makes it the pin for
//! a feature whose bytes are new.
//!
//! Every case is checked **twice**, because a dynamic value does not encode the
//! same way under the two deliveries:
//!
//! * `message` + `section` — the schema sent once per connection. There is no
//!   section to describe a struct reached through an `any`, so such a record
//!   goes out as a map of its field names.
//! * `selfDescribing` — the standalone message, whose own section *can* hold
//!   that struct, so the record goes out as a type tag and ordinary rows.
//!
//! The two are different bytes and must be the same document. A port that got
//! only one of them right would pass half of this.

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

/// The dynamic half of the walk corpus. The `doc.` half has a declared type at
/// the root and no `any` anywhere in it, so it is `walk.rs`'s subject; running
/// it here too would only make both files slower.
fn walks(corpus: &serde_json::Value) -> Vec<&serde_json::Value> {
    corpus["walks"]
        .as_array()
        .expect("walks")
        .iter()
        .filter(|case| {
            case["name"]
                .as_str()
                .is_some_and(|n| n.starts_with("dynamic."))
        })
        .collect()
}

/// Splits a self-describing message: the root byte, then a length-prefixed
/// section, then the body. `Schema::size` is what the section occupied.
fn split_standalone(message: &[u8]) -> (section::Schema, &[u8], bool) {
    let root = message[0];
    assert_eq!(root & 0xF0, 0xD0, "not a colbin root byte: {root:#04x}");
    assert_ne!(
        root & 0x04,
        0,
        "the root byte carries no section: {root:#04x}"
    );
    let schema = section::parse(&message[1..]).expect("the message's own section");
    let body = &message[1 + schema.size..];
    (schema, body, root & 0x08 != 0)
}

fn same_document(got: &[u8], want: &str) -> bool {
    let Ok(left) = serde_json::from_slice::<serde_json::Value>(got) else {
        return false;
    };
    let Ok(right) = serde_json::from_str::<serde_json::Value>(want) else {
        return false;
    };
    left == right
}

#[test]
fn every_dynamic_case_renders_what_go_renders() {
    let corpus = corpus();
    let cases = walks(&corpus);
    assert!(cases.len() >= 8, "corpus shrank: {} cases", cases.len());

    for case in &cases {
        let name = case["name"].as_str().expect("name");
        let want = case["json"].as_str().expect("json");
        let wide = case["wide"].as_bool().expect("wide");

        // The out-of-band delivery.
        let section_bytes = unhex(case["section"].as_str().expect("section"));
        let message = unhex(case["message"].as_str().expect("message"));
        let schema = section::parse(&section_bytes).unwrap_or_else(|e| panic!("{name}: {e}"));
        let got = walk::to_json(&schema, &message[1..], wide)
            .unwrap_or_else(|e| panic!("{name}: out of band: {e}"));
        assert!(
            same_document(&got, want),
            "{name}: out of band rendered\n {}\nwant\n {want}",
            String::from_utf8_lossy(&got),
        );

        // And the standalone one, whose section can name a struct the type
        // never mentions.
        let standalone = unhex(case["selfDescribing"].as_str().expect("selfDescribing"));
        let (schema, body, wide) = split_standalone(&standalone);
        let got = walk::to_json(&schema, body, wide)
            .unwrap_or_else(|e| panic!("{name}: self-describing: {e}"));
        assert!(
            same_document(&got, want),
            "{name}: self-describing rendered\n {}\nwant\n {want}",
            String::from_utf8_lossy(&got),
        );
    }
}

/// The claim the type tag exists to make. An array of records described by the
/// section is much smaller than the same array with its field names spelled out
/// per row, and if that ever stopped being true the feature would be pointless
/// while every other test still passed.
#[test]
fn a_tagged_array_of_records_is_smaller_than_a_named_one() {
    let corpus = corpus();
    let case = walks(&corpus)
        .into_iter()
        .find(|case| case["name"] == "dynamic.records.table")
        .expect("dynamic.records.table");

    let named = unhex(case["message"].as_str().unwrap()).len();
    let tagged = unhex(case["selfDescribing"].as_str().unwrap()).len();
    assert!(
        tagged * 2 < named,
        "the tagged form is {tagged} bytes and the named one {named}: \
         the type tag is not doing its job",
    );
}

/// A short read has to become an error, never a panic and never a read past the
/// buffer — which is the property that matters when the bytes came off a socket.
#[test]
fn a_truncated_dynamic_message_is_refused_rather_than_panicking() {
    let corpus = corpus();
    for case in walks(&corpus) {
        let standalone = unhex(case["selfDescribing"].as_str().unwrap());
        let (schema, body, wide) = split_standalone(&standalone);
        for cut in 0..body.len() {
            let _ = walk::to_json(&schema, &body[..cut], wide);
        }
    }
}

/// And a flipped bit either refuses or renders something that is still JSON. A
/// decoder that emits half a document on bad input is worse than one that stops.
#[test]
fn a_corrupted_dynamic_byte_is_refused_or_renders_well_formed_json() {
    let corpus = corpus();
    let case = walks(&corpus)
        .into_iter()
        .find(|case| case["name"] == "dynamic.scalars")
        .expect("dynamic.scalars");
    let standalone = unhex(case["selfDescribing"].as_str().unwrap());
    let (schema, body, wide) = split_standalone(&standalone);

    let mut seen = 0;
    for at in 0..body.len() {
        for bit in 0..8u32 {
            let mut broken = body.to_vec();
            broken[at] ^= 1 << bit;
            if let Ok(text) = walk::to_json(&schema, &broken, wide) {
                serde_json::from_slice::<serde_json::Value>(&text)
                    .expect("a successful decode must produce valid JSON");
            }
            seen += 1;
        }
    }
    assert!(seen > 0);
}
