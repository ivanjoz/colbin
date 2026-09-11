//! Re-encodes every compact case in both Go corpora with the Rust encoder and
//! writes the results for the Go side to verify.
//!
//! This is the reverse of the arrangement that pins the decoder. There, Go writes
//! and Rust reads; here Rust writes and Go reads, and both directions are needed
//! because they fail differently. The decoder tests catch Rust misreading Go's
//! bytes. Byte equality (`tests/encode.rs`) catches Rust writing different bytes
//! from Go — but only for the cases where the two agree exactly, and a string
//! that packs smaller than raw is deliberately not one of them. Those are exactly
//! the messages no test has ever handed to Go, which is where a raw frame at an
//! odd bit offset would hide.
//!
//! Run with `cargo run --example emit_vectors`; the output is committed, and CI
//! regenerates it and fails on a diff.

use std::collections::BTreeMap;

use base64::Engine;
use colbin::{Kind, Schema};
use serde_json::Value as Json;

fn main() {
    let root = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("vectors");
    let mut out: BTreeMap<String, String> = BTreeMap::new();

    for (file, composite) in [("vectors.json", false), ("composites.json", true)] {
        let text = std::fs::read_to_string(root.join(file)).expect("the corpus is readable");
        let corpus: Json = serde_json::from_str(&text).expect("the corpus is valid JSON");
        for case in corpus["cases"].as_array().expect("cases is an array") {
            let name = case["name"].as_str().expect("a case name");
            let message = decode_base64(case["message"].as_str().expect("a message"));
            // Compact mode only: that is all this encoder writes.
            if !colbin::is_compact(&message) {
                continue;
            }
            let schema = if composite {
                composite_schema(&case["fields"])
            } else {
                flat_schema(&case["fields"])
            };
            let records = colbin::decode(&message, &schema)
                .unwrap_or_else(|err| panic!("{name}: decode: {err}"));
            let shape = case["shape"].as_i64().expect("a shape");
            let written = if shape == 0 {
                colbin::encode_one(&schema, &records[0])
            } else {
                colbin::encode(&schema, &records)
            }
            .unwrap_or_else(|err| panic!("{name}: encode: {err}"));

            out.insert(
                name.to_owned(),
                base64::engine::general_purpose::STANDARD.encode(&written),
            );
        }
    }

    let body =
        serde_json::to_string_pretty(&serde_json::json!({ "messages": out })).expect("serialises");
    let path = root.join("rust_encoded.json");
    std::fs::write(&path, format!("{body}\n")).expect("the output is writable");
    println!("{} messages -> {}", out.len(), path.display());
}

fn flat_schema(fields: &Json) -> Schema {
    let specs: Vec<(u8, Kind)> = fields
        .as_array()
        .expect("fields is an array")
        .iter()
        .map(|field| {
            (
                field["id"].as_u64().expect("an id") as u8,
                kind_of(field["kind"].as_str().expect("a kind")),
            )
        })
        .collect();
    Schema::from_ids(specs).expect("a valid schema")
}

fn composite_schema(fields: &Json) -> Schema {
    let specs: Vec<(u8, Kind)> = fields
        .as_array()
        .expect("fields is an array")
        .iter()
        .map(|field| {
            (
                field["id"].as_u64().expect("an id") as u8,
                composite_kind(&field["k"]),
            )
        })
        .collect();
    Schema::from_ids(specs).expect("a valid schema")
}

fn composite_kind(node: &Json) -> Kind {
    match node["kind"].as_str().expect("a kind name") {
        "struct" => composite_schema(&node["fields"]).nested(),
        "array" => Kind::Array(Box::new(composite_kind(&node["elem"]))),
        "map" => Kind::Map(
            Box::new(composite_kind(&node["key"])),
            Box::new(composite_kind(&node["val"])),
        ),
        scalar => kind_of(scalar),
    }
}

/// The corpus kind names, which both corpora share for their scalar leaves.
fn kind_of(name: &str) -> Kind {
    match name {
        "bool" => Kind::Bool,
        "i8" => Kind::Int8,
        "i16" => Kind::Int16,
        "i32" => Kind::Int32,
        "i64" => Kind::Int64,
        "u8" => Kind::Uint8,
        "u16" => Kind::Uint16,
        "u32" => Kind::Uint32,
        "u64" => Kind::Uint64,
        "f32" => Kind::Float32,
        "f64" => Kind::Float64,
        "str" => Kind::String,
        "bytes" => Kind::Bytes,
        "i8s" => Kind::Int8s,
        "i16s" => Kind::Int16s,
        "i32s" => Kind::Int32s,
        "i64s" => Kind::Int64s,
        "u16s" => Kind::Uint16s,
        "u32s" => Kind::Uint32s,
        "u64s" => Kind::Uint64s,
        "bools" => Kind::Bools,
        "strs" => Kind::Strings,
        "f32s" => Kind::Float32s,
        "f64s" => Kind::Float64s,
        other => panic!("the corpus names an unknown kind {other}"),
    }
}

fn decode_base64(text: &str) -> Vec<u8> {
    base64::engine::general_purpose::STANDARD
        .decode(text)
        .expect("base64")
}
