//! Timing and size for the Rust encoder and decoder.
//!
//! `cargo run --release --example bench`. Release matters: the debug build is
//! several times slower and says nothing useful.
//!
//! A plain `Instant` harness rather than criterion, which would be a heavy dev
//! dependency for a crate that deliberately has no runtime ones. The numbers are
//! a median of repeated batches, which is enough to see a regression or a
//! difference against the Go figures; they are not a statistical claim.
//!
//! # Reading the numbers against Go's
//!
//! The subject is `benchStats` from `codec/typed_bench_test.go` — seven numbered
//! integer fields, so compact mode with narrow keys — which is what
//! `BenchmarkRecordAppendCodec` and `BenchmarkRecordUnmarshalCodec` measure.
//!
//! One difference is not a difference in the codec: Go's `Codec[T].Append` writes
//! into a buffer the caller hands back, so it allocates nothing per message,
//! while `encode_one` returns a fresh `Vec`. That allocation is in every figure
//! below, and it is the honest cost of an API that hands back an owned message.

use std::hint::black_box;
use std::time::Instant;

use base64::Engine;
use colbin::{Codec, Colbin, Kind, Record, Schema, TypedCodec, Value};
use serde_json::Value as Json;

/// The Go benchmark's subject as a derived Rust struct: the same seven numbered
/// integer fields `codec/typed_bench_test.go` measures.
#[derive(Colbin, Clone, PartialEq, Debug)]
struct BenchStats {
    #[cb(1)]
    quantity: i32,
    #[cb(2)]
    quantity_pending_delivery: i32,
    #[cb(3)]
    sub_quantity: i16,
    #[cb(4)]
    sub_quantity_pending: i16,
    #[cb(5)]
    sub_divisor: i16,
    #[cb(6)]
    total_amount: i32,
    #[cb(7)]
    total_debt_amount: i32,
}

/// A token-shaped record: mixed integers and a string, which is what actually
/// crosses the fareward boundary.
#[derive(Colbin, Clone, PartialEq, Debug)]
struct Token {
    #[cb(1)]
    company_id: i32,
    #[cb(2)]
    id: i32,
    #[cb(3)]
    created: i32,
    #[cb(4)]
    hash: u64,
    #[cb(5)]
    user: String,
}

fn main() {
    let (schema, record) = bench_stats();
    let message = colbin::encode_one(&schema, &record).expect("encodes");
    println!(
        "subject: 7 numbered integer fields (codec/typed_bench_test.go benchStats), {} B compact\n",
        message.len()
    );

    // The free functions: a plan built per call, a fresh Vec per message.
    let free_encode = time("encode_one", 200_000, || {
        black_box(colbin::encode_one(&schema, &record).expect("encodes"));
    });
    let free_decode = time("decode_one", 200_000, || {
        black_box(colbin::decode_one(&message, &schema).expect("decodes"));
    });

    // The handle: the plan resolved once, and the buffer the caller's to reuse.
    let codec = Codec::new(schema.clone());
    let handle_encode = time("Codec::encode_one", 200_000, || {
        black_box(codec.encode_one(&record).expect("encodes"));
    });
    let mut buf = Vec::new();
    let append = time("Codec::append_one", 200_000, || {
        buf.clear();
        codec.append_one(&mut buf, &record).expect("encodes");
        black_box(&buf);
    });
    let handle_decode = time("Codec::decode_one", 200_000, || {
        black_box(codec.decode_one(&message).expect("decodes"));
    });
    let mut into = Record::new();
    let decode_into = time("Codec::decode_one_into", 200_000, || {
        codec.decode_one_into(&message, &mut into).expect("decodes");
        black_box(&into);
    });
    assert_eq!(buf, message, "the handle must write the same bytes");

    println!("  encode_one              {free_encode:>8.1} ns/op   free function");
    println!("  Codec::encode_one       {handle_encode:>8.1} ns/op   plan cached, buffer presized");
    println!("  Codec::append_one       {append:>8.1} ns/op   reused buffer, zero allocations");
    println!("                                        (Go Codec.Append ~120 ns/op, 0 allocs)");
    println!("  decode_one              {free_decode:>8.1} ns/op   free function");
    println!("  Codec::decode_one       {handle_decode:>8.1} ns/op   no intermediate Vec<Record>");
    println!(
        "  Codec::decode_one_into  {decode_into:>8.1} ns/op   reused Record, zero allocations"
    );
    println!("                                        (Go Codec.Unmarshal ~150 ns/op, 0 allocs)\n");

    // The integer array codec: the one size optimiser kept, and the one
    // deliberately expensive thing in the encoder — four transforms, each
    // histogrammed and scored against 32 (k, d) pairs. Measured through a
    // one-field record rather than through a hole in the API, so the figure
    // includes the message framing every other row here also pays.
    println!("a record whose one field is an integer array, encode_one:\n");
    for (label, kind, value) in [
        (
            "sorted ids, 64 x i32",
            Kind::Int32s,
            Value::Ints((0..64).map(|i| 1000 + i * 3).collect()),
        ),
        (
            "scattered, 64 x i32",
            Kind::Int32s,
            Value::Ints((0..64).map(|i| (i * 2_654_435_761_i64) % 70_000).collect()),
        ),
        (
            "full-width, 64 x i64",
            Kind::Int64s,
            Value::Ints(
                (0..64)
                    .map(|i| i * 0x9E37_79B9_7F4A_7C15_u64 as i64)
                    .collect(),
            ),
        ),
        (
            "small run, 8 x i32",
            Kind::Int32s,
            Value::Ints((1..=8).collect()),
        ),
    ] {
        let codec = Codec::new(Schema::from_ids([(1, kind)]).expect("a valid schema"));
        let record = Record::from_fields([(1, value)]);
        let bytes = codec.encode_one(&record).expect("encodes").len();
        let mut buf = Vec::new();
        let per = time(label, 20_000, || {
            buf.clear();
            codec
                .append_one(&mut buf, black_box(&record))
                .expect("encodes");
            black_box(&buf);
        });
        println!("  {label:<22} {per:>8.1} ns/op   -> {bytes} B");
    }
    println!();

    typed_report(&message);

    size_report();
}

/// The fastest of many batches, in nanoseconds per iteration.
///
/// The minimum rather than the median, deliberately. For a throughput
/// microbenchmark the fastest batch is the one least contaminated by whatever
/// else the machine was doing, and the noise here is large enough to invert a
/// comparison outright: taking the median, one measurement of this same code
/// moved a fixed cost from 19 ns to 40 ns between runs.
fn time(_label: &str, iterations: u32, mut body: impl FnMut()) -> f64 {
    for _ in 0..iterations / 4 {
        body();
    }
    let mut best = f64::MAX;
    for _ in 0..25 {
        let start = Instant::now();
        for _ in 0..iterations {
            body();
        }
        let per = start.elapsed().as_nanos() as f64 / f64::from(iterations);
        if per < best {
            best = per;
        }
    }
    best
}

/// The typed path against the dynamic one, on the same subject.
///
/// This is what the derive is for: no `Record`, no `Value`, no allocation per
/// field — the value goes into the struct field it belongs to.
fn typed_report(dynamic_message: &[u8]) {
    let typed = TypedCodec::<BenchStats>::new().expect("a valid layout");
    let stats = BenchStats {
        quantity: 480,
        quantity_pending_delivery: 120,
        sub_quantity: 12,
        sub_quantity_pending: 3,
        sub_divisor: 24,
        total_amount: 145_900,
        total_debt_amount: 32_000,
    };
    let message = typed.encode(&stats).expect("encodes");
    assert_eq!(
        message, dynamic_message,
        "the typed path must write the dynamic path's bytes"
    );

    let mut buf = Vec::new();
    let append = time("TypedCodec::append", 200_000, || {
        buf.clear();
        typed.append(&mut buf, &stats).expect("encodes");
        black_box(&buf);
    });
    let decode = time("TypedCodec::decode", 200_000, || {
        black_box(typed.decode(&message).expect("decodes"));
    });
    let mut into = BenchStats::colbin_zero();
    let decode_into = time("TypedCodec::decode_into", 200_000, || {
        typed.decode_into(&message, &mut into).expect("decodes");
        black_box(&into);
    });

    println!("#[derive(Colbin)], same 7-field subject, straight into the struct:\n");
    println!("  TypedCodec::append       {append:>8.1} ns/op   reused buffer, zero allocations");
    println!("  TypedCodec::decode       {decode:>8.1} ns/op   a fresh struct");
    println!(
        "  TypedCodec::decode_into  {decode_into:>8.1} ns/op   reused struct, zero allocations"
    );
    println!("                                        (Go Codec.Unmarshal ~150 ns/op, 0 allocs)");

    // A token: the mixed shape, where a String allocation is unavoidable.
    let codec = TypedCodec::<Token>::new().expect("a valid layout");
    let token = Token {
        company_id: 7,
        id: 42,
        created: 1_700_000_000,
        hash: 12_720_753_295_591_565_293,
        user: "tester".into(),
    };
    let message = codec.encode(&token).expect("encodes");
    let mut into = Token::colbin_zero();
    let decode_into = time("token", 200_000, || {
        codec.decode_into(&message, &mut into).expect("decodes");
        black_box(&into);
    });
    let mut buf = Vec::new();
    let append = time("token", 200_000, || {
        buf.clear();
        codec.append(&mut buf, &token).expect("encodes");
        black_box(&buf);
    });
    println!(
        "\n  token, 4 ints + a string ({} B): append {append:.1} ns/op, decode_into {decode_into:.1} ns/op\n",
        message.len()
    );
}

/// Rust's bytes against Go's, per corpus case. The only expected difference is a
/// string: Rust writes raw packed5 frames, Go picks the cheaper of raw and
/// packed. This reports what that costs on real records rather than per string.
fn size_report() {
    let root = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("vectors");
    let rust: Json = serde_json::from_str(
        &std::fs::read_to_string(root.join("rust_encoded.json")).expect("run emit_vectors first"),
    )
    .expect("valid JSON");

    println!("size, Rust-written against Go-written, compact cases only:\n");
    println!("  {:<32} {:>6} {:>6} {:>8}", "case", "go", "rust", "delta");
    let mut go_total = 0_usize;
    let mut rust_total = 0_usize;
    let mut differing = 0;
    for file in ["vectors.json", "composites.json"] {
        let corpus: Json = serde_json::from_str(
            &std::fs::read_to_string(root.join(file)).expect("the corpus is readable"),
        )
        .expect("valid JSON");
        for case in corpus["cases"].as_array().unwrap() {
            let name = case["name"].as_str().unwrap();
            let Some(written) = rust["messages"].get(name) else {
                continue;
            };
            let go = decode_base64(case["message"].as_str().unwrap()).len();
            let mine = decode_base64(written.as_str().unwrap()).len();
            go_total += go;
            rust_total += mine;
            if mine != go {
                differing += 1;
                println!(
                    "  {name:<32} {go:>6} {mine:>6} {:>+8}",
                    mine as i64 - go as i64
                );
            }
        }
    }
    println!(
        "\n  {:<32} {go_total:>6} {rust_total:>6} {:>+8}   ({differing} of the cases differ)",
        "total",
        rust_total as i64 - go_total as i64
    );
    println!("  every difference is a string that packs smaller than raw; see rust/PLAN.md.");
}

/// The Go benchmark's subject, as a schema and a record.
fn bench_stats() -> (Schema, Record) {
    let schema = Schema::from_ids([
        (1, Kind::Int32),
        (2, Kind::Int32),
        (3, Kind::Int16),
        (4, Kind::Int16),
        (5, Kind::Int16),
        (6, Kind::Int32),
        (7, Kind::Int32),
    ])
    .expect("a valid schema");
    let record = Record::from_fields([
        (1, Value::Int(480)),
        (2, Value::Int(120)),
        (3, Value::Int(12)),
        (4, Value::Int(3)),
        (5, Value::Int(24)),
        (6, Value::Int(145_900)),
        (7, Value::Int(32_000)),
    ]);
    (schema, record)
}

fn decode_base64(text: &str) -> Vec<u8> {
    base64::engine::general_purpose::STANDARD
        .decode(text)
        .expect("base64")
}
