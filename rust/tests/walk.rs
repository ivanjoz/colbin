//! The schema-driven walk, against the documents Go encoded and rendered.
//!
//! The corpus is the `doc.` half of `rust/vectors/vectors.json`, written by
//! `go run ./rust/vectors` from real Go values. Every case has a section, a
//! message and the JSON `colbin.ToJSON` produced from the two, so matching it
//! here is agreeing with the specification rather than with a second opinion.
//!
//! These cases used to come from `js/vectors/web_encoded.json`, which at the
//! time only the AssemblyScript module wrote — so this file could not run
//! without first building a module in another language. The shapes are the same
//! ones `js/tests/documents.mjs` drives the browser module over; only the hand
//! that writes them changed.

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

// ---- unknown narrow keys ------------------------------------------------------
//
// The nibble says how long a narrow field is whatever its type, so a reader
// whose schema has lost a field steps over it exactly as a wide reader does.
// Each shape below is written under a key the reading schema does not list,
// between two it does; the walk must land on the second one every time.

use colbin::plan::{OP_INT64, OP_STRING, OP_STRUCTS, OP_UINT32, Plan, PlanField};
use colbin::wire::Writer;

/// A schema that knows key 0 (`a`, a `u32`), key 15 (`z`, a string) and key 14
/// (`rows`, a slice of a struct whose only field is key 0, `n`, an `i64`).
fn reader_schema() -> section::Schema {
    let mut root = Plan::default();
    root.fields = vec![
        PlanField {
            key: 0,
            op: OP_UINT32,
            ..PlanField::default()
        },
        PlanField {
            key: 14,
            op: OP_STRUCTS,
            sub: Some(1),
            ..PlanField::default()
        },
        PlanField {
            key: 15,
            op: OP_STRING,
            ..PlanField::default()
        },
    ];
    root.names = vec!["a".into(), "rows".into(), "z".into()];
    root.finish();
    let mut row = Plan::default();
    row.fields = vec![PlanField {
        key: 0,
        op: OP_INT64,
        ..PlanField::default()
    }];
    row.names = vec!["n".into()];
    row.finish();
    section::Schema {
        plans: vec![root, row],
        size: 0,
    }
}

/// Writes one field under the key it is given.
type WriteUnder = fn(&mut Writer<'_>, u8);

/// Every shape a narrow field can take, each written under `key`.
fn unknown_shapes() -> Vec<(&'static str, WriteUnder)> {
    vec![
        ("uint", |w, key| w.u64(key, 70_000)),
        ("inline uint", |w, key| w.u64(key, 3)),
        ("negative int", |w, key| w.i64(key, -300)),
        ("minus one", |w, key| w.i64(key, -1)),
        ("short string", |w, key| w.string(key, "abc")),
        ("long string", |w, key| w.string(key, &"x".repeat(300))),
        ("packed string", |w, key| {
            let at = w.buf.len();
            w.packed_string(key, "the quick brown fox jumps over the lazy dog");
            assert_eq!(w.buf[at] & 0b1111, 0b1101, "the string really packed");
        }),
        ("int array", |w, key| w.ints(key, &[-1_i32, 70_000, 3])),
        ("string array", |w, key| w.strings(key, &["a", "", "ccc"])),
        ("explicit zero", |w, key| w.zero(key)),
        ("struct", |w, key| {
            let mark = w.open_struct(key);
            {
                let mut inner = Writer::new(w.buf);
                inner.u64(0, 9);
                inner.string(1, "inner");
            }
            w.close(mark);
        }),
        ("wide struct", |w, key| {
            let mark = w.open_struct_wide(key);
            {
                let mut inner = colbin::wire::Writer8::new(w.buf);
                inner.u64(200, 9);
            }
            w.close(mark);
        }),
        ("list", |w, key| {
            let list = w.open_list(key, 2);
            for value in [1_u64, 2] {
                let element = w.open_element();
                Writer::new(w.buf).u64(0, value);
                w.close_element(element);
            }
            w.close(list);
        }),
        ("table", |w, key| {
            let table = w.open_table(key, 8);
            w.column(0, &[1_i64, 2, 3, 4, 5, 6, 7, 8]);
            w.strings(1, &["a"; 8]);
            w.close(table);
        }),
        ("map", |w, key| {
            let map = w.open_map(key, 2);
            w.element_string("k");
            w.element_int(-7);
            w.element_string("l");
            w.element_int(70_000);
            w.close(map);
        }),
        ("long struct", |w, key| {
            let mark = w.open_struct(key);
            Writer::new(w.buf).string(0, &"y".repeat(70_000));
            w.close(mark);
        }),
    ]
}

#[test]
fn an_unknown_narrow_key_of_every_shape_is_stepped_over() {
    let schema = reader_schema();
    for (name, write) in unknown_shapes() {
        let mut body = Vec::new();
        {
            let mut w = Writer::new(&mut body);
            w.u32(0, 7);
            write(&mut w, 1);
            w.string(15, "end");
        }
        let json = walk::to_json(&schema, &body, false).unwrap_or_else(|e| panic!("{name}: {e}"));
        assert_eq!(
            String::from_utf8(json).expect("UTF-8"),
            r#"{"a":7,"z":"end","rows":null}"#,
            "{name}",
        );
    }
}

#[test]
fn every_shape_at_once_is_stepped_over() {
    let schema = reader_schema();
    let shapes = unknown_shapes();
    let mut body = Vec::new();
    {
        let mut w = Writer::new(&mut body);
        w.u32(0, 7);
        // Keys 1..=13, wrapping onto the shapes that did not fit; none of them
        // is one the schema lists.
        for (at, (_, write)) in shapes.iter().enumerate() {
            write(&mut w, 1 + (at % 13) as u8);
        }
        w.string(15, "end");
    }
    let json = walk::to_json(&schema, &body, false).expect("walk");
    assert_eq!(
        String::from_utf8(json).expect("UTF-8"),
        r#"{"a":7,"z":"end","rows":null}"#,
    );
}

/// A table column the row type no longer declares is stepped over too, and the
/// rows still carry the columns it does.
#[test]
fn an_unknown_narrow_column_is_stepped_over() {
    let schema = reader_schema();
    let mut body = Vec::new();
    {
        let mut w = Writer::new(&mut body);
        let table = w.open_table(14, 8);
        w.strings(1, &["gone"; 8]);
        w.column(0, &[1_i64, 2, 3, 4, 5, 6, 7, -8]);
        w.column(2, &[9_i64; 8]);
        w.close(table);
    }
    let json = walk::to_json(&schema, &body, false).expect("walk");
    assert_eq!(
        String::from_utf8(json).expect("UTF-8"),
        r#"{"rows":[{"n":1},{"n":2},{"n":3},{"n":4},{"n":5},{"n":6},{"n":7},{"n":-8}],"a":0,"z":""}"#,
    );
}

/// What the walk refuses is a field it *does* know written in a form its type
/// does not have — not a field it does not know.
#[test]
fn a_known_narrow_key_in_the_wrong_form_is_refused() {
    let schema = reader_schema();
    let mut body = Vec::new();
    Writer::new(&mut body).i64(0, -300); // `a` is a u32
    assert!(walk::to_json(&schema, &body, false).is_err());
}
