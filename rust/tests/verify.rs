//! The encode self-check, and the whole encode it closes.
//!
//! `rust/ENCODER.md` §5 argues that a self-check costing one decode is worth
//! paying, on the evidence that five sixths of single-byte corruptions of a
//! colbin message decode to well-formed, *wrong* JSON. A decoder cannot tell.
//! An encoder can, because it still has the input.
//!
//! So this file runs every document through the whole encode with the check on,
//! and then — the part that makes the check worth having — corrupts what the
//! encoder wrote and asserts the check refuses it. A self-check that has never
//! said no is a self-check nobody has tested.

#![cfg(feature = "encode")]

use colbin::build::{self, PACK_STRINGS, SELF_DESCRIBING, VERIFY};
use colbin::diag::{D_CORRUPT, Diag};
use colbin::{json::parse, section, verify, walk};

fn oracle() -> serde_json::Value {
    let path = concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/../js/vectors/web_encoded.json"
    );
    let text = std::fs::read_to_string(path).expect("web_encoded.json — run `bun run emit` in js/");
    serde_json::from_str(&text).expect("web_encoded.json is not JSON")
}

fn documents() -> Vec<(String, String, u32)> {
    oracle()["cases"]
        .as_array()
        .expect("cases")
        .iter()
        .map(|case| {
            let name = case["name"].as_str().expect("name").to_string();
            let flags = if name.ends_with("(packed)") {
                PACK_STRINGS
            } else {
                0
            };
            (
                name,
                case["input"].as_str().expect("input").to_string(),
                flags,
            )
        })
        .collect()
}

#[test]
fn every_document_passes_its_own_self_check() {
    let cases = documents();
    assert!(cases.len() >= 40, "corpus shrank: {} cases", cases.len());

    for (name, input, flags) in cases {
        let mut diag = Diag::new();
        build::encode(input.as_bytes(), flags | VERIFY, &mut diag)
            .unwrap_or_else(|| panic!("{name}: {} at {}", diag.message, diag.path));
    }
}

/// And the standalone shape, which verifies through the section it wrote rather
/// than through the one it kept — so a section that describes the wrong body is
/// caught by the check rather than by whoever reads the message.
#[test]
fn every_document_passes_it_self_describing_too() {
    for (name, input, flags) in documents() {
        let mut diag = Diag::new();
        let encoded = build::encode(
            input.as_bytes(),
            flags | VERIFY | SELF_DESCRIBING,
            &mut diag,
        )
        .unwrap_or_else(|| panic!("{name}: {}", diag.message));

        assert_eq!(
            encoded.message[0] & 0x04,
            0x04,
            "{name}: the root byte must say it carries a section",
        );
        // The same body either way: the difference is the root bit and the
        // section in front of it.
        let plain = build::encode(input.as_bytes(), flags, &mut Diag::new()).expect(&name);
        assert_eq!(
            &encoded.message[1 + encoded.section.len()..],
            &plain.message[1..],
            "{name}: the two deliveries wrote different bodies",
        );
    }
}

/// The point of the whole file. Every single-byte corruption of a message, put
/// back through the check that was meant to catch it — either the decode fails,
/// or the walk disagrees with the input. What must never happen is a corrupted
/// message passing.
#[test]
fn a_corrupted_message_does_not_pass_the_check() {
    // Documents with enough shape to corrupt meaningfully and small enough to
    // sweep exhaustively.
    let inputs = [
        r#"{"id":1,"name":"Tin Light","price":599,"active":true}"#,
        r#"{"id":9,"inner":{"sku":"ABC-1","qty":2},"note":"pickup"}"#,
        r#"{"rows":[{"id":0,"qty":1},{"id":1,"qty":2},{"id":2,"qty":3},{"id":3,"qty":4},
                   {"id":4,"qty":5},{"id":5,"qty":6},{"id":6,"qty":7},{"id":7,"qty":8}]}"#,
        r#"{"ints":[1,2,3],"strings":["a","bb"]}"#,
    ];

    let mut caught = 0;
    let mut unchanged = 0;
    for input in inputs {
        let mut diag = Diag::new();
        let good = build::encode(input.as_bytes(), VERIFY, &mut diag).expect("encode");
        let schema = section::parse(&good.section).expect("section");
        let doc = parse::parse(input.as_bytes(), &mut Diag::new()).expect("parse");
        let root = colbin::infer::infer(&doc, &mut Diag::new())
            .expect("infer")
            .root;

        for at in 1..good.message.len() {
            for bit in 0..8u32 {
                let mut broken = good.message.clone();
                broken[at] ^= 1 << bit;
                let wide = broken[0] & 0x08 != 0;

                let mut diag = Diag::new();
                let passed = match walk::to_json(&schema, &broken[1..], wide) {
                    // A decode that fails is already a refusal.
                    Err(_) => false,
                    Ok(json) => verify::verify(&doc, root, &json, &mut diag),
                };
                if passed {
                    // The one honest exception: a flipped bit that does not
                    // change what the message means. A size code the field does
                    // not use, say. It must still decode to the same document,
                    // which is exactly what `verify` just confirmed.
                    unchanged += 1;
                } else {
                    caught += 1;
                    // Refused by the decoder, which leaves `diag` untouched, or
                    // by the walk-against-input, which reports it as corruption.
                    // Never as some third thing.
                    assert!(
                        diag.ok() || diag.code == D_CORRUPT,
                        "byte {at} bit {bit}: refused with code {}",
                        diag.code,
                    );
                }
            }
        }
    }

    assert!(caught > 0, "the self-check never refused anything");
    // A sanity bound rather than an exact number: if most corruptions started
    // passing, the check would have stopped checking.
    assert!(
        caught > unchanged,
        "only {caught} of {} corruptions were caught",
        caught + unchanged,
    );
}

/// The three differences the check must *not* flag, because each is a documented
/// property of a dense columnar layout rather than a lost value.
#[test]
fn the_expected_differences_are_not_failures() {
    let cases: [(&str, &str); 3] = [
        // A key absent from one record comes back as its zero — and here as
        // null, because inference made the field a pointer.
        (
            "a missing key",
            r#"[{"id":1,"note":"hi"},{"id":2},{"id":3},{"id":4},{"id":5},{"id":6},{"id":7},{"id":8}]"#,
        ),
        // An empty array comes back as null: the two are one thing on the wire.
        ("an empty array", r#"{"ints":[],"name":"x"}"#),
        // One float promotes the column, so the integers come back as floats.
        (
            "a promoted column",
            r#"[{"v":1},{"v":2.5},{"v":3},{"v":4},{"v":5},{"v":6},{"v":7},{"v":8}]"#,
        ),
    ];

    for (name, input) in cases {
        let mut diag = Diag::new();
        build::encode(input.as_bytes(), VERIFY, &mut diag)
            .unwrap_or_else(|| panic!("{name}: {} at {}", diag.message, diag.path));
    }
}

/// A number that does not survive its column is a failure rather than a
/// tolerance: the float promotion is allowed to change an integer's *spelling*,
/// not its value.
#[test]
fn a_value_that_does_not_survive_its_column_is_refused() {
    // 2^53+1 cannot be held by an f64, and one float in the column promotes it
    // there. The encoder must notice rather than round.
    let input = r#"[{"v":9007199254740993},{"v":2.5}]"#;
    let mut diag = Diag::new();
    let result = build::encode(input.as_bytes(), VERIFY, &mut diag);
    assert!(result.is_none(), "the rounding went unnoticed");
    assert_eq!(diag.code, D_CORRUPT, "{}", diag.message);
    assert!(
        diag.message.contains("does not read back as its input"),
        "{}",
        diag.message,
    );
}

/// Without the check it encodes happily, which is the whole argument for having
/// the check: the message is well formed, decodes cleanly, and is wrong.
#[test]
fn the_same_value_encodes_without_complaint_when_the_check_is_off() {
    let input = r#"[{"v":9007199254740993},{"v":2.5}]"#;
    let mut diag = Diag::new();
    assert!(build::encode(input.as_bytes(), 0, &mut diag).is_some());
}
