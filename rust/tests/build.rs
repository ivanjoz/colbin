//! The encoder, against committed bytes.
//!
//! For each of the documents in `js/tests/documents.mjs`,
//! `js/vectors/web_encoded.json` holds the JSON text that went in and the
//! message that came out — twice, once with strings raw and once with the
//! opt-in packing on. This file encodes the same text and asserts the bytes are
//! identical. The file was written by the AssemblyScript module
//! `RUST_WASM_PLAN.md` phase 8 deleted, and reproducing it byte for byte is
//! what made the deletion safe.
//!
//! Byte equality rather than "it decodes to the same document", and that is the
//! point. Two encoders that agree on the document but not on the bytes are two
//! formats, and the difference would show up as a size regression, a shape the
//! Go decoder handles differently, or nothing at all until it mattered.
//!
//! Go is not an oracle for this layer — it has no JSON encoder — but it *is* the
//! oracle for what comes out: `rust/tests/walk.rs` and the Go reader both read
//! what this writes, and `tests/verify.rs` closes the loop per document.

#![cfg(feature = "encode")]

use colbin::build::Builder;
use colbin::diag::Diag;
use colbin::{infer, json::parse, section};

/// The public ABI's flag, mirrored here because the corpus was generated with it.
const PACK_STRINGS: u32 = 4;

fn oracle() -> serde_json::Value {
    let path = concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/../js/vectors/web_encoded.json"
    );
    let text = std::fs::read_to_string(path).expect("web_encoded.json — run `bun run emit` in js/");
    serde_json::from_str(&text).expect("web_encoded.json is not JSON")
}

fn hex(bytes: &[u8]) -> String {
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}

/// Parse, infer, build — the whole encode, without the self-check.
fn encode(text: &str, flags: u32) -> Result<(Vec<u8>, Vec<u8>), Diag> {
    let mut diag = Diag::new();
    let Some(doc) = parse::parse(text.as_bytes(), &mut diag) else {
        return Err(diag);
    };
    let Some(inferred) = infer::infer(&doc, &mut diag) else {
        return Err(diag);
    };
    let mut out = Vec::new();
    let mut builder = Builder::new(&doc, &inferred.schema, &mut diag);
    builder.pack_strings = flags & PACK_STRINGS != 0;
    builder.build(&mut out, inferred.root, false);
    if !diag.ok() {
        return Err(diag);
    }
    Ok((out, section::build(&inferred.schema)))
}

#[test]
fn every_document_encodes_to_the_bytes_the_module_wrote() {
    let corpus = oracle();
    let cases = corpus["cases"].as_array().expect("cases");
    assert!(cases.len() >= 40, "corpus shrank: {} cases", cases.len());

    let mut packed = 0;
    for case in cases {
        let name = case["name"].as_str().expect("name");
        let input = case["input"].as_str().expect("input");
        let want = case["message"].as_str().expect("message");
        // The corpus runs every document twice, and the packed half is named so.
        let flags = if name.ends_with("(packed)") {
            packed += 1;
            PACK_STRINGS
        } else {
            0
        };

        let (message, _) = encode(input, flags).unwrap_or_else(|e| panic!("{name}: {}", e.message));
        assert_eq!(
            hex(&message),
            want,
            "{name}: the message differs\n  input: {input}",
        );
    }
    assert!(packed >= 20, "only {packed} packed cases ran");
}

/// What the message and the section are for: a reader that has neither the type
/// nor the encoder reads the document back out. This is the round trip the whole
/// port exists to make possible, run through the crate's own decoder.
#[test]
fn what_the_encoder_writes_the_decoder_reads_back() {
    let corpus = oracle();
    for case in corpus["cases"].as_array().expect("cases") {
        let name = case["name"].as_str().expect("name");
        let input = case["input"].as_str().expect("input");
        let flags = if name.ends_with("(packed)") {
            PACK_STRINGS
        } else {
            0
        };

        let (message, section_bytes) =
            encode(input, flags).unwrap_or_else(|e| panic!("{name}: {}", e.message));
        let schema = section::parse(&section_bytes).unwrap_or_else(|e| panic!("{name}: {e}"));
        let wide = message[0] & 0x08 != 0;
        let json = colbin::walk::to_json(&schema, &message[1..], wide)
            .unwrap_or_else(|e| panic!("{name}: {e}"));

        // The module's own rendering of the same message, which Go has already
        // agreed with (`go test ./js/vectors`).
        let want = case["json"].as_str().expect("json");
        assert_eq!(
            String::from_utf8(json).expect("the walk writes UTF-8"),
            want,
            "{name}",
        );
    }
}

/// Packing is a size choice and never a correctness one: the descriptor says
/// which form each string took, and the encoder keeps the packed form only when
/// it is smaller. So turning it on can never make a message larger.
#[test]
fn packing_never_costs_bytes() {
    let corpus = oracle();
    for case in corpus["cases"].as_array().expect("cases") {
        let name = case["name"].as_str().expect("name");
        if name.ends_with("(packed)") {
            continue;
        }
        let input = case["input"].as_str().expect("input");
        let (raw, _) = encode(input, 0).expect(name);
        let (packed, _) = encode(input, PACK_STRINGS).expect(name);
        assert!(
            packed.len() <= raw.len(),
            "{name}: packing grew the message from {} to {} bytes",
            raw.len(),
            packed.len(),
        );
    }
}

/// The self-describing shape: the root byte says a section rides in front, and
/// the message is standalone. It is the same body either way, so the difference
/// is exactly the root bit and the section.
#[test]
fn a_self_describing_message_carries_its_own_section() {
    let text = r#"{"id":1,"name":"Tin Light"}"#;
    let mut diag = Diag::new();
    let doc = parse::parse(text.as_bytes(), &mut diag).expect("parse");
    let inferred = infer::infer(&doc, &mut diag).expect("infer");
    let section_bytes = section::build(&inferred.schema);

    let mut out = Vec::new();
    out.push(if inferred.schema.root().is_wide {
        0xdc
    } else {
        0xd4
    });
    out.extend_from_slice(&section_bytes);
    let mut builder = Builder::new(&doc, &inferred.schema, &mut diag);
    builder.run(&mut out, 0, inferred.root);
    assert!(diag.ok(), "{}", diag.message);

    assert_eq!(
        out[0] & 0x04,
        0x04,
        "the root byte says it carries a section"
    );
    let schema = section::parse(&out[1..]).expect("its own section");
    let body = &out[1 + schema.size..];
    let json = colbin::walk::to_json(&schema, body, schema.root().is_wide).expect("walk");
    assert_eq!(String::from_utf8(json).expect("utf-8"), text);
}
