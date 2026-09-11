//! Helpers shared by the corpus tests.
//!
//! Both corpora describe an expected value the same way -- integers as decimal
//! strings because a JSON number cannot hold the whole of `i64`, floats as their
//! IEEE bit patterns because a decimal rendering describes the bytes on the wire
//! only approximately -- so the translation into a [`Value`] lives here rather
//! than once per test binary.

#![allow(dead_code)]

use base64::Engine;
use colbin::{Kind, Record, Schema, Value};
use serde_json::Value as Json;

pub fn assert_record(what: &str, got: &Record, want: &Json, schema: &Schema) {
    let want = want.as_array().expect("a record is an array of fields");
    let got: Vec<(u8, &Value)> = got.iter().collect();
    assert_eq!(got.len(), want.len(), "{what}: field count");
    for (index, ((id, value), field)) in got.iter().zip(want).enumerate() {
        let want_id = field["id"].as_u64().unwrap() as u8;
        assert_eq!(*id, want_id, "{what}: field {index} id");
        let kind = schema
            .fields()
            .iter()
            .find(|f| f.id == want_id)
            .expect("the id is in the schema")
            .kind
            .clone();
        let want_value = expected_value(&kind, &field["v"]);
        assert!(
            same(value, &want_value),
            "{what}: field {want_id} is {value:?}, expected {want_value:?}"
        );
    }
}

/// Values are compared with floats taken by their bits, so a sign of zero or a
/// rounding that survived the wire is not quietly accepted by `-0.0 == 0.0`.
pub fn same(got: &Value, want: &Value) -> bool {
    match (got, want) {
        (Value::Float32(a), Value::Float32(b)) => a.to_bits() == b.to_bits(),
        (Value::Float64(a), Value::Float64(b)) => a.to_bits() == b.to_bits(),
        (Value::Float32s(a), Value::Float32s(b)) => {
            a.len() == b.len() && a.iter().zip(b).all(|(x, y)| x.to_bits() == y.to_bits())
        }
        (Value::Float64s(a), Value::Float64s(b)) => {
            a.len() == b.len() && a.iter().zip(b).all(|(x, y)| x.to_bits() == y.to_bits())
        }
        _ => got == want,
    }
}

/// Turns one corpus value into the [`Value`] the decoder must produce.
///
/// Integers travel as decimal strings because a JSON number cannot hold the
/// whole of `i64` or `u64`, and floats as their IEEE bit patterns because a
/// decimal rendering describes the bytes on the wire only approximately.
pub fn expected_value(kind: &Kind, json: &Json) -> Value {
    match kind {
        Kind::Bool => Value::Bool(json.as_bool().unwrap()),
        Kind::Int8 | Kind::Int16 | Kind::Int32 | Kind::Int64 => Value::Int(int(json)),
        Kind::Uint8 | Kind::Uint16 | Kind::Uint32 | Kind::Uint64 => Value::Uint(uint(json)),
        Kind::Float32 => Value::Float32(f32::from_bits(bits(json) as u32)),
        Kind::Float64 => Value::Float64(f64::from_bits(bits(json))),
        Kind::String => Value::String(json.as_str().unwrap().to_owned()),
        Kind::Bytes => Value::Bytes(decode_base64(json.as_str().unwrap())),
        Kind::Int8s | Kind::Int16s | Kind::Int32s | Kind::Int64s => {
            Value::Ints(array(json).iter().map(int).collect())
        }
        Kind::Uint16s | Kind::Uint32s | Kind::Uint64s => {
            Value::Uints(array(json).iter().map(uint).collect())
        }
        Kind::Bools => Value::Bools(array(json).iter().map(|v| v.as_bool().unwrap()).collect()),
        Kind::Strings => Value::Strings(
            array(json)
                .iter()
                .map(|v| v.as_str().unwrap().to_owned())
                .collect(),
        ),
        Kind::Float32s => Value::Float32s(
            array(json)
                .iter()
                .map(|v| f32::from_bits(bits(v) as u32))
                .collect(),
        ),
        Kind::Float64s => Value::Float64s(
            array(json)
                .iter()
                .map(|v| f64::from_bits(bits(v)))
                .collect(),
        ),

        // Composites. A nested struct's expected value is a field list exactly
        // like a record's, which is what makes this recursion the same shape as
        // assert_record's.
        Kind::Struct(schema) => Value::Record(expected_record(schema, json)),
        Kind::Array(elem) => Value::Array(
            array(json)
                .iter()
                .map(|v| expected_value(elem, v))
                .collect(),
        ),
        Kind::Map(key, val) => Value::Map(
            array(json)
                .iter()
                .map(|entry| {
                    (
                        expected_value(key, &entry["k"]),
                        expected_value(val, &entry["v"]),
                    )
                })
                .collect(),
        ),
    }
}

/// The record a `{id, v}` field list describes, read against `schema`.
pub fn expected_record(schema: &Schema, json: &Json) -> Record {
    let mut record = Record::new();
    for field in json.as_array().expect("a record is an array of fields") {
        let id = field["id"].as_u64().expect("a field id") as u8;
        let kind = schema
            .fields()
            .iter()
            .find(|f| f.id == id)
            .expect("the id is in the schema")
            .kind
            .clone();
        record.push(id, expected_value(&kind, &field["v"]));
    }
    record
}

pub fn array(json: &Json) -> &Vec<Json> {
    json.as_array().expect("a slice value is a JSON array")
}

pub fn int(json: &Json) -> i64 {
    json.as_str().unwrap().parse().expect("a decimal integer")
}

pub fn uint(json: &Json) -> u64 {
    json.as_str().unwrap().parse().expect("a decimal integer")
}

pub fn bits(json: &Json) -> u64 {
    u64::from_str_radix(json.as_str().unwrap(), 16).expect("hex float bits")
}

pub fn kind_of(name: &str) -> Kind {
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
        other => panic!("the corpus names a kind this decoder has no name for: {other}"),
    }
}

pub fn decode_base64(text: &str) -> Vec<u8> {
    base64::engine::general_purpose::STANDARD
        .decode(text)
        .expect("the corpus holds standard base64")
}
