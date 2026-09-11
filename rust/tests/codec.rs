//! The [`Codec`] handle: the same bytes as the free functions, and the reuse it
//! exists for.
//!
//! The handle is an optimisation, so the first thing to pin is that it is *only*
//! an optimisation: it must produce, for every message in both corpora, exactly
//! what the free functions produce. Everything else here is about the state it
//! carries — a cached plan and a caller-owned buffer — not leaking between calls.

use colbin::{Codec, Kind, Record, Schema, Value};
use serde_json::Value as Json;

mod common;
use common::{decode_base64, kind_of};

const FLAT: &str = include_str!("../vectors/vectors.json");
const COMPOSITE: &str = include_str!("../vectors/composites.json");

/// A handle changes what encoding costs, not what comes out — over every compact
/// message in both corpora, composites included.
#[test]
fn writes_what_the_free_functions_write() {
    let mut checked = 0;
    for (name, schema, message, shape) in compact_cases() {
        let records = colbin::decode(&message, &schema).expect("decodes");
        let free = if shape == 0 {
            colbin::encode_one(&schema, &records[0])
        } else {
            colbin::encode(&schema, &records)
        }
        .expect("encodes");

        let codec = Codec::new(schema);
        let handle = if shape == 0 {
            codec.encode_one(&records[0])
        } else {
            codec.encode(&records)
        }
        .expect("encodes");

        assert_eq!(handle, free, "{name}: the handle wrote different bytes");
        assert_eq!(
            codec.narrow_keys(),
            (message[0] >> 4) & 1 == 1,
            "{name}: keys"
        );
        checked += 1;
    }
    assert!(checked >= 20, "only {checked} cases compared");
}

/// The handle reads back what it wrote, for every case, through the reusing
/// entry points — which is where a stale field would show up.
#[test]
fn round_trips_through_the_reusing_paths() {
    // One destination for every case, so a leak between differently shaped
    // messages has somewhere to show.
    let mut buf = Vec::new();
    let mut records = Vec::new();
    for (name, schema, message, shape) in compact_cases() {
        let codec = Codec::new(schema);
        let want = codec.decode(&message).expect("decodes");

        buf.clear();
        if shape == 0 {
            codec.append_one(&mut buf, &want[0]).expect("encodes");
        } else {
            codec.append(&mut buf, &want).expect("encodes");
        }
        // Against `encode_one`, not against the Go message: a string that packs
        // smaller than raw makes the two legitimately differ, and byte equality
        // with Go is what tests/encode.rs pins on the cases where it holds.
        let fresh = if shape == 0 {
            codec.encode_one(&want[0])
        } else {
            codec.encode(&want)
        }
        .expect("encodes");
        assert_eq!(buf, fresh, "{name}: append into a cleared buffer");
        assert_eq!(
            codec.decode(&buf).expect("decodes"),
            want,
            "{name}: what append wrote did not read back"
        );

        codec.decode_into(&message, &mut records).expect("decodes");
        assert_eq!(records, want, "{name}: decode_into");
    }
}

/// `append_one` appends: it does not assume the buffer starts empty, so a caller
/// can pack several messages back to back.
#[test]
fn appends_rather_than_overwrites() {
    let codec = Codec::new(Schema::from_ids([(1, Kind::Int32), (2, Kind::String)]).unwrap());
    let mut buf = vec![0xAA, 0xBB]; // a framing prefix the caller owns
    let mut offsets = vec![buf.len()];
    for id in 1..=3_i64 {
        let record = Record::new()
            .with(1, Value::Int(id))
            .with(2, Value::String(format!("row {id}")));
        codec.append_one(&mut buf, &record).expect("encodes");
        offsets.push(buf.len());
    }
    assert_eq!(&buf[..2], &[0xAA, 0xBB], "the prefix was overwritten");
    // Each message is byte aligned at its end, so the slices are independently
    // decodable — which is the property appending is for.
    for (index, window) in offsets.windows(2).enumerate() {
        let record = codec
            .decode_one(&buf[window[0]..window[1]])
            .unwrap_or_else(|err| panic!("message {index}: {err}"));
        assert_eq!(record.i64(1), index as i64 + 1);
        assert_eq!(record.str(2), format!("row {}", index + 1));
    }
}

/// Reusing a destination must not leave the previous message's fields visible:
/// compact mode omits a zero-valued field, so a stale value has nothing to
/// overwrite it.
#[test]
fn reuse_leaves_nothing_stale() {
    let codec = Codec::new(
        Schema::from_ids([(1, Kind::Int32), (2, Kind::String), (3, Kind::Bool)]).unwrap(),
    );
    let full = Record::new()
        .with(1, Value::Int(9))
        .with(2, Value::String("keep out".into()))
        .with(3, Value::Bool(true));
    let sparse = Record::new().with(1, Value::Int(1));

    let full_msg = codec.encode_one(&full).unwrap();
    let sparse_msg = codec.encode_one(&sparse).unwrap();

    let mut into = Record::new();
    for _ in 0..5 {
        codec.decode_one_into(&full_msg, &mut into).unwrap();
        assert_eq!(into.len(), 3);
        codec.decode_one_into(&sparse_msg, &mut into).unwrap();
        assert_eq!(into.len(), 1, "a stale field survived: {into:?}");
        assert_eq!(into.str(2), "", "stale string");
        assert!(!into.bool(3), "stale bool");
    }

    // The same for a record slice: a shorter message must not leave the longer
    // one's records past its own length.
    let many = vec![full.clone(), sparse.clone(), full.clone()];
    let few = vec![sparse.clone()];
    let many_msg = codec.encode(&many).unwrap();
    let few_msg = codec.encode(&few).unwrap();
    let mut dst = Vec::new();
    for _ in 0..5 {
        codec.decode_into(&many_msg, &mut dst).unwrap();
        assert_eq!(dst.len(), 3);
        codec.decode_into(&few_msg, &mut dst).unwrap();
        assert_eq!(dst.len(), 1, "records survived past the message: {dst:?}");
        assert_eq!(dst[0].len(), 1);
    }
}

/// The size hint is a hint: it must not change what comes out, whatever it holds.
#[test]
fn the_size_hint_never_changes_the_bytes() {
    let codec = Codec::new(Schema::from_ids([(1, Kind::String)]).unwrap());
    let short = Record::new().with(1, Value::String("x".into()));
    let long = Record::new().with(1, Value::String("x".repeat(500)));

    let short_alone = codec.encode_one(&short).unwrap();
    let long_alone = codec.encode_one(&long).unwrap();

    // Alternating leaves the hint pointing at the wrong size on every call.
    for _ in 0..4 {
        assert_eq!(codec.encode_one(&long).unwrap(), long_alone);
        assert_eq!(codec.encode_one(&short).unwrap(), short_alone);
    }
}

/// A one-record demand is checked rather than assumed.
#[test]
fn decode_one_refuses_a_multi_record_message() {
    let codec = Codec::new(Schema::from_ids([(1, Kind::Int32)]).unwrap());
    let records = vec![
        Record::new().with(1, Value::Int(1)),
        Record::new().with(1, Value::Int(2)),
    ];
    let message = codec.encode(&records).unwrap();
    assert_eq!(
        codec.decode_one(&message),
        Err(colbin::Error::RecordCount(2))
    );
    // And the shape-0 form of one record is accepted by both.
    let one = codec.encode_one(&records[0]).unwrap();
    assert_eq!(codec.decode_one(&one).unwrap(), records[0]);
    assert_eq!(codec.decode(&one).unwrap(), vec![records[0].clone()]);
}

/// A handle is immutable after construction, so it can be shared. This is a
/// compile-time assertion as much as a runtime one.
#[test]
fn is_send_and_sync() {
    fn assert_send_sync<T: Send + Sync>() {}
    assert_send_sync::<Codec>();

    let codec = std::sync::Arc::new(Codec::new(Schema::from_ids([(1, Kind::Int32)]).unwrap()));
    let record = Record::new().with(1, Value::Int(7));
    let expected = codec.encode_one(&record).unwrap();
    let handles: Vec<_> = (0..4)
        .map(|_| {
            let codec = std::sync::Arc::clone(&codec);
            let record = record.clone();
            let expected = expected.clone();
            std::thread::spawn(move || {
                // Each thread owns its own buffer; the handle is shared.
                let mut buf = Vec::new();
                for _ in 0..200 {
                    buf.clear();
                    codec.append_one(&mut buf, &record).unwrap();
                    assert_eq!(buf, expected);
                }
            })
        })
        .collect();
    for handle in handles {
        handle.join().expect("no thread panicked");
    }
}

// --- corpus plumbing ---------------------------------------------------------

fn compact_cases() -> Vec<(String, Schema, Vec<u8>, i64)> {
    let mut out = Vec::new();
    for (corpus, composite) in [(FLAT, false), (COMPOSITE, true)] {
        let corpus: Json = serde_json::from_str(corpus).expect("valid JSON");
        for case in corpus["cases"].as_array().unwrap() {
            let message = decode_base64(case["message"].as_str().unwrap());
            if !colbin::is_compact(&message) {
                continue;
            }
            let schema = schema_of(&case["fields"], composite);
            out.push((
                case["name"].as_str().unwrap().to_owned(),
                schema,
                message,
                case["shape"].as_i64().unwrap(),
            ));
        }
    }
    out
}

fn schema_of(fields: &Json, composite: bool) -> Schema {
    let specs: Vec<(u8, Kind)> = fields
        .as_array()
        .unwrap()
        .iter()
        .map(|field| {
            let id = field["id"].as_u64().unwrap() as u8;
            let kind = if composite {
                composite_kind(&field["k"])
            } else {
                kind_of(field["kind"].as_str().unwrap())
            };
            (id, kind)
        })
        .collect();
    Schema::from_ids(specs).expect("a valid schema")
}

fn composite_kind(node: &Json) -> Kind {
    match node["kind"].as_str().unwrap() {
        "struct" => schema_of(&node["fields"], true).nested(),
        "array" => Kind::Array(Box::new(composite_kind(&node["elem"]))),
        "map" => Kind::Map(
            Box::new(composite_kind(&node["key"])),
            Box::new(composite_kind(&node["val"])),
        ),
        scalar => kind_of(scalar),
    }
}
