//! The JSON scanner against Go: exact integers, correctly-rounded floats, and
//! the escape rules `encoding/json` defines — including the lone surrogate that
//! becomes U+FFFD, so what reaches the encoder is always valid UTF-8.
//!
//! The corpus is the `numbers` and `texts` groups of
//! `rust/vectors/vectors.json`, written by `go run ./rust/vectors`. Every
//! expected float is `strconv.ParseFloat`'s bit pattern and every expected
//! string is `encoding/json`'s decode, so this is a comparison with the
//! specification rather than with a second reading of the same grammar.
//!
//! 513 of the 527 number cases are floats, and 450 of those are random bit
//! patterns rendered three ways each. That is the half of the file that matters:
//! a hand-picked boundary tests what somebody thought of, and a doubly rounded
//! conversion goes wrong on the ones nobody did.

#![cfg(feature = "encode")]

use colbin::diag::Diag;
use colbin::json::parse::{
    self, K_ARRAY, K_BOOL, K_FLOAT, K_INT, K_NULL, K_OBJECT, K_STRING, K_UINT,
};

fn corpus() -> serde_json::Value {
    let path = concat!(env!("CARGO_MANIFEST_DIR"), "/vectors/vectors.json");
    let text = std::fs::read_to_string(path).expect("vectors.json — run `go run ./rust/vectors`");
    serde_json::from_str(&text).expect("vectors.json is not JSON")
}

/// Parses, returning the root kind or the diagnostic.
fn kind_of(text: &str) -> Result<(u8, parse::Doc), Diag> {
    let mut diag = Diag::new();
    match parse::parse(text.as_bytes(), &mut diag) {
        Some(doc) => Ok((doc.kind_of(doc.root()), doc)),
        None => {
            diag.locate(text.as_bytes());
            Err(diag)
        }
    }
}

#[test]
fn every_number_reads_the_way_go_reads_it() {
    let corpus = corpus();
    let cases = corpus["numbers"].as_array().expect("numbers");
    assert!(cases.len() >= 500, "corpus shrank: {} cases", cases.len());

    for case in cases {
        let name = case["name"].as_str().expect("name");
        let literal = case["literal"].as_str().expect("literal");
        let want = case["kind"].as_str().expect("kind");

        let parsed = kind_of(literal);
        if want == "error" {
            let diag = parsed
                .err()
                .unwrap_or_else(|| panic!("{name}: {literal} should have been refused"));
            assert!(
                !diag.message.is_empty(),
                "{name}: a refusal carries a message"
            );
            continue;
        }
        let (kind, doc) = parsed.unwrap_or_else(|e| panic!("{name}: {literal}: {}", e.message));
        let root = doc.root();

        match want {
            "int" => {
                assert_eq!(kind, K_INT, "{name}: {literal}");
                let value: i64 = case["value"].as_str().expect("value").parse().expect("i64");
                assert_eq!(doc.int_of(root), value, "{name}: {literal}");
            }
            "uint" => {
                assert_eq!(kind, K_UINT, "{name}: {literal}");
                let value: u64 = case["value"].as_str().expect("value").parse().expect("u64");
                assert_eq!(doc.uint_of(root), value, "{name}: {literal}");
            }
            "float" => {
                assert_eq!(kind, K_FLOAT, "{name}: {literal}");
                let bits = u64::from_str_radix(case["bits"].as_str().expect("bits"), 16)
                    .expect("bits are hex");
                assert_eq!(
                    doc.float_of(root).to_bits(),
                    bits,
                    "{name}: {literal}: float64 bits must match Go exactly \
                     (got {:016x}, want {bits:016x})",
                    doc.float_of(root).to_bits(),
                );
            }
            other => panic!("{name}: unknown kind {other}"),
        }
    }
}

#[test]
fn every_string_decodes_the_way_encoding_json_decodes_it() {
    let corpus = corpus();
    let cases = corpus["texts"].as_array().expect("texts");
    assert!(cases.len() >= 19, "corpus shrank: {} cases", cases.len());

    for case in cases {
        let name = case["name"].as_str().expect("name");
        let literal = case["literal"].as_str().expect("literal");
        let want = unbase64(case["decoded"].as_str().expect("decoded"));

        let (kind, doc) = kind_of(literal).unwrap_or_else(|e| panic!("{name}: {}", e.message));
        assert_eq!(kind, K_STRING, "{name}");
        assert_eq!(doc.str_of(doc.root()), want.as_slice(), "{name}: {literal}");
    }
}

#[test]
fn an_object_keeps_its_first_seen_key_order() {
    let (kind, doc) = kind_of(r#"{"z":1,"a":2,"m":3}"#).expect("parse");
    assert_eq!(kind, K_OBJECT);
    let root = doc.root();
    assert_eq!(doc.count(root), 3);
    let keys: Vec<&[u8]> = (0..3).map(|at| doc.key_of(root, at)).collect();
    assert_eq!(keys, [b"z".as_slice(), b"a", b"m"]);
}

/// The rule `JSON.parse` follows and every consumer downstream depends on: the
/// last value wins, and the key keeps the position it first appeared in. Without
/// it, inference would see one key holding two types and call it a conflict.
#[test]
fn a_duplicate_key_keeps_its_last_value_in_its_first_position() {
    let (_, doc) = kind_of(r#"{"a":1,"b":2,"a":3}"#).expect("parse");
    let root = doc.root();
    assert_eq!(doc.count(root), 2);
    assert_eq!(doc.key_of(root, 0), b"a");
    assert_eq!(doc.int_of(doc.child_at(root, 0)), 3);
    assert_eq!(doc.key_of(root, 1), b"b");
}

#[test]
fn arrays_and_objects_nest() {
    let (kind, doc) = kind_of(r#"[{"a":[1,2]},null,true]"#).expect("parse");
    assert_eq!(kind, K_ARRAY);
    let root = doc.root();
    assert_eq!(doc.count(root), 3);
    assert_eq!(doc.kind_of(doc.child_at(root, 0)), K_OBJECT);
    assert_eq!(doc.kind_of(doc.child_at(root, 1)), K_NULL);
    assert_eq!(doc.kind_of(doc.child_at(root, 2)), K_BOOL);
    assert!(doc.bool_of(doc.child_at(root, 2)));

    let inner = doc.child_at(root, 0);
    let list = doc.child_at(inner, 0);
    assert_eq!(doc.kind_of(list), K_ARRAY);
    assert_eq!(doc.count(list), 2);
    assert_eq!(doc.int_of(doc.child_at(list, 1)), 2);
}

#[test]
fn malformed_input_is_refused_with_a_position() {
    // A raw control character inside a string is illegal JSON; built rather than
    // written literally so this file stays printable.
    let control = format!("\"a{}b\"", char::from(1u8));
    let cases: [(&str, &str); 11] = [
        ("unterminated string", "\"abc"),
        ("trailing comma", "[1,2,]"),
        ("missing colon", "{\"a\" 1}"),
        ("bare word", "nul"),
        ("trailing content", "{} {}"),
        ("control char in string", &control),
        ("leading zero", "01"),
        ("bare minus", "-"),
        ("exponent with no digits", "1e"),
        ("point with no digits", "1."),
        ("empty input", ""),
    ];

    for (name, text) in cases {
        let diag = kind_of(text)
            .err()
            .unwrap_or_else(|| panic!("{name} should have been refused"));
        assert!(
            diag.offset >= 0,
            "{name}: a diagnostic must carry an offset"
        );
        assert!(!diag.message.is_empty(), "{name}: and a message");
    }
}

#[test]
fn nesting_past_the_depth_limit_is_refused() {
    let text = format!("{}{}", "[".repeat(200), "]".repeat(200));
    let diag = kind_of(&text).expect_err("refused");
    assert!(
        diag.message.contains("nesting deeper"),
        "got {}",
        diag.message,
    );
}

#[test]
fn a_diagnostic_carries_a_line_number() {
    let diag = kind_of("{\n  \"a\": 1,\n  \"b\": @\n}").expect_err("refused");
    assert_eq!(diag.line, 3);
}

/// The one the whole file exists for. `JSON.parse` and any reader that goes
/// through a double turn this into 7295013456321098800, and colbin has an exact
/// int64 column to put it in.
#[test]
fn an_integer_past_2p53_survives_to_the_last_digit() {
    let (kind, doc) = kind_of("7295013456321098765").expect("parse");
    assert_eq!(kind, K_INT);
    assert_eq!(doc.int_of(doc.root()), 7_295_013_456_321_098_765);
}

/// Base64 as `encoding/base64.StdEncoding` wrote it. Small enough to keep here
/// rather than make the crate's one dev-dependency two.
fn unbase64(text: &str) -> Vec<u8> {
    const ALPHABET: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    let mut out = Vec::with_capacity(text.len() / 4 * 3);
    let mut bits = 0u32;
    let mut held = 0u32;
    for byte in text.bytes() {
        if byte == b'=' {
            break;
        }
        let value = ALPHABET
            .iter()
            .position(|&c| c == byte)
            .unwrap_or_else(|| panic!("{} is not base64", char::from(byte)));
        bits = (bits << 6) | value as u32;
        held += 6;
        if held >= 8 {
            held -= 8;
            out.push((bits >> held) as u8);
        }
    }
    out
}
