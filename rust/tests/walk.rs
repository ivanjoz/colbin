//! The schema-driven walk, against the 46 messages the AssemblyScript module
//! encoded and the Go decoder already reads.
//!
//! The corpus is the strongest check available to this port: every case has a
//! section, a message, and the JSON the module rendered for it — and
//! `go test ./web/vectors` has already agreed with that JSON. Matching it here
//! makes three implementations that agree rather than two and a newcomer.

use colbin::{section, walk};

fn unhex(text: &str) -> Vec<u8> {
    (0..text.len())
        .step_by(2)
        .map(|at| u8::from_str_radix(&text[at..at + 2], 16).expect("hex"))
        .collect()
}

fn corpus() -> serde_json::Value {
    let path = concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/../web/vectors/web_encoded.json"
    );
    let text =
        std::fs::read_to_string(path).expect("web_encoded.json — run `bun run emit` in web/");
    serde_json::from_str(&text).expect("web_encoded.json is not JSON")
}

/// The root byte says the key width, and whether a section rides in front.
/// `0xD4` / `0xDC` are the self-describing pair; `0xD0` / `0xD8` are the bare
/// ones the out-of-band delivery uses.
fn root_is_wide(byte: u8) -> bool {
    byte & 0x08 != 0
}

#[test]
fn every_message_in_the_corpus_renders_the_same_json() {
    let corpus = corpus();
    let cases = corpus["cases"].as_array().expect("cases");
    let mut checked = 0;
    let mut unsupported = Vec::new();

    for case in cases {
        let name = case["name"].as_str().expect("name");
        let section_bytes = unhex(case["section"].as_str().expect("section"));
        let message = unhex(case["message"].as_str().expect("message"));
        let want = case["json"].as_str().expect("json");

        let schema = section::parse(&section_bytes).unwrap_or_else(|e| panic!("{name}: {e}"));
        let wide = root_is_wide(message[0]);

        match walk::to_json(&schema, &message[1..], wide) {
            Ok(got) => {
                let got = String::from_utf8(got).expect("the walk writes UTF-8");
                assert_eq!(got, want, "{name}: the walk disagrees with the module");
                checked += 1;
            }
            Err(colbin::Error::Unsupported) => unsupported.push(name),
            Err(e) => panic!("{name}: {e}"),
        }
    }

    assert!(
        unsupported.is_empty(),
        "cases this decoder cannot render yet: {unsupported:?}",
    );
    assert!(checked >= 40, "only {checked} cases ran");
}

#[test]
fn a_truncated_message_is_refused_rather_than_panicking() {
    let corpus = corpus();
    let cases = corpus["cases"].as_array().expect("cases");

    for case in cases {
        let section_bytes = unhex(case["section"].as_str().unwrap());
        let message = unhex(case["message"].as_str().unwrap());
        let Ok(schema) = section::parse(&section_bytes) else {
            continue;
        };
        let wide = root_is_wide(message[0]);
        // Every prefix. A short read must become an error, never a panic and
        // never a read past the buffer — which is the property that matters when
        // the bytes came off a socket.
        for cut in 1..message.len() {
            let _ = walk::to_json(&schema, &message[1..cut], wide);
        }
    }
}

#[test]
fn a_corrupted_byte_is_refused_or_renders_something_well_formed() {
    let corpus = corpus();
    let case = &corpus["cases"].as_array().expect("cases")[0];
    let schema = section::parse(&unhex(case["section"].as_str().unwrap())).expect("parse");
    let message = unhex(case["message"].as_str().unwrap());
    let wide = root_is_wide(message[0]);

    let mut refused = 0;
    let mut rendered = 0;
    for at in 1..message.len() {
        for bit in 0..8 {
            let mut broken = message.clone();
            broken[at] ^= 1 << bit;
            match walk::to_json(&schema, &broken[1..], wide) {
                Ok(text) => {
                    // Whatever it decoded to, it must still be JSON: a decoder
                    // that emits a half-written document on bad input is worse
                    // than one that refuses.
                    serde_json::from_slice::<serde_json::Value>(&text)
                        .expect("a successful decode must produce valid JSON");
                    rendered += 1;
                }
                Err(_) => refused += 1,
            }
        }
    }
    assert!(refused + rendered > 0);
}
