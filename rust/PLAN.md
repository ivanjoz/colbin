# Plan — a Rust decoder that reads composites, and a Rust compact-mode encoder

Two parts. They share one prerequisite, so the order is fixed:

0. **`Schema` / `Kind` / `Value` become trees.** Both parts need it.
1. **The decoder reads composites** — nested structs, arrays of structs, maps.
2. **A compact-mode encoder.**

---

## 0. The shared prerequisite: a recursive schema

Today `Kind`, `Value` and `Schema` are flat (`rust/src/lib.rs:165`, `:229`,
`:341`). `Schema::kind(id) -> Option<Kind>` returns a `Copy` scalar tag, and
`Kind` has no variant that holds anything. That is the single reason the decoder
supports no composite in **either** wire mode — not compact mode, and not
standard mode either.

```rust
pub enum Kind {
    Bool, Int8, ..., Float64, String, Bytes,
    Int8s, ..., Float64s, Bools, Strings,

    Struct(Box<Schema>),          // a nested key run / a sub-table
    Array(Box<Kind>),             // a slice whose elements are not scalars
    Map(Box<Kind>, Box<Kind>),    // key kind, value kind
}

pub enum Value {
    Bool(bool), Int(i64), ..., Float64s(Vec<f64>),

    Record(Record),               // Kind::Struct
    Array(Vec<Value>),            // Kind::Array
    Map(Vec<(Value, Value)>),     // Kind::Map — a Vec, not a BTreeMap
}
```

Three consequences worth naming up front:

- **`Kind` stops being `Copy`.** It is matched on in `compact.rs:184-252` and
  throughout `standard.rs`; those become `&Kind` matches. Mechanical, but it
  touches nearly every line of both readers.
- **`Value::Map` is a `Vec<(Value, Value)>`, not a map type.** Go writes entries
  in its own iteration order and a colbin map key can be a float, which is not
  `Ord` or `Hash`. A `Vec` preserves exactly what the wire said and costs the
  caller a lookup; anything else would either reorder or refuse valid messages.
- **`Schema` needs a depth limit.** A hostile or buggy caller can nest
  `Kind::Struct` arbitrarily, and a recursive `read_value` would blow the stack
  on a crafted message. A `MAX_DEPTH` (32) checked at schema-build time, so the
  readers stay non-recursive in their error handling.

`Schema::from_go` gains a sibling that takes nested field lists. The id hashing
is per-struct and already correct (`fieldid::assign_ids`), so a nested schema is
just another `assign_ids` call — no new id logic.

**Cost:** ~150 lines changed, ~120 added. Everything after this depends on it.

---

## 1. The decoder reads composites

### 1.1 Compact mode (`rust/src/compact.rs`)

This is the small half, because compact mode's composites are the recursion the
format already had. `read_value` gains three arms mirroring
`codec/compact_plan.go`:

```rust
Kind::Struct(schema) => Value::Record(read_record(reader, schema)?),
Kind::Array(elem)    => { let n = reader.count(min_bits(elem))?;
                          Value::Array((0..n).map(|_| read_value(reader, elem)).collect()?) }
Kind::Map(k, v)      => { let n = reader.count(min_bits(k) + min_bits(v))?;
                          Value::Map((0..n).map(|_| Ok((read_value(reader,k)?,
                                                        read_value(reader,v)?))).collect()?) }
```

Plus `min_bits` (the count bounds check: 1 for bool, 32/64 for floats, the key
width for a struct, 8 otherwise) and depth tracking. `read_record` already
exists and already loops to the terminator, so a nested key run needs no new
framing.

**Cost:** ~120 lines. **This is what makes the Go change I just made readable in
Rust**, and it is the part that closes the gap between the two.

### 1.2 Standard mode (`rust/src/standard.rs`) — a scoping decision

Standard mode's composites are a different, larger job, because the columnar
layout is not recursive in the same tidy way. What `standard.rs` would need:

| form | wire | what it costs |
|---|---|---|
| nested struct | `[ftStruct][colCount][id][column]…` | a recursive `decode_sub_table`; the column reader currently assumes a flat field list |
| array of structs | `[ftArray][length sub-column][ftStruct] sub-table` over *flattened* elements | the flatten/split step — one sub-table holds every record's elements end to end, and `split` (`standard.rs:182`) has to cut them per record |
| map | `[ftMap][length sub-column][key elem column][value elem column]` | same flatten/split, twice |
| nullable (`*T`) | a null bitmap wrapper + a *dense* value column | `null_map.go`'s presence layout, which the Rust side has never read |
| `any` | row-style `[tag:1] payload` per value | a self-describing value reader — the one thing compact mode cannot carry, but standard mode can |

Note the last two: they are standard-mode-only shapes with no compact
counterpart, so they are pure addition rather than a port of something the
compact reader already does.

**Cost:** ~400-450 lines, and the `split`/flatten logic is where the bugs live.

**Why it may still be needed:** a genix ORM blob with 4 or more records arrives
**columnar**, not compact. So a Rust reader of a multi-record blob holding a
nested struct needs this. A reader of a *token* (one record, always compact) does
not — which is all `fareward` reads today
(`fareward/src/bridge/token.rs:57-61`, a flat 5-field struct).

---

## 2. A compact-mode encoder

### 2.1 What it is

```rust
pub fn encode(schema: &Schema, records: &[Record]) -> Result<Vec<u8>, Error>
```

Symmetric with `decode`: the same `Schema`, the same `Record`/`Value` tree, the
inverse direction. Not serde — colbin's wire ids come from hashing field names in
declaration order, which `Serialize` does not surface, and a derive macro is a
second crate. A `Value` tree is also exactly what the round-trip test needs.

Refuses what compact mode refuses: more than 3 records, zero records, and
`Kind::Struct` with an empty field list. It has no `any` to refuse — the Rust
`Value` has no dynamic variant.

### 2.2 The four pieces, and their real cost

| piece | Go source | Rust cost | notes |
|---|---|---|---|
| bitstream writer | `compact/bitstream.go` (writer half) | ~90 | LSB-first, 32-bit chunks, `flush_whole` for byte-aligned payloads. Straight port, no decisions. |
| compact selector varint | `compact/varint.go` | ~40 | The `[selector:1][cont:1][payload:6]` ladder. Straight port. |
| record/value writer | `codec/compact_plan.go` write half | ~250 | Header bits, key runs, the omit-zero rule (**recursive** — an all-zero nested struct is omitted), `ALL_POSITIVE` pre-scan, `NARROW_KEYS` decision folded over every reachable schema. |
| **`packed5` encoder** | `packed5/encode.go` — **384 lines** | ~400 | See below. |
| **`varint` array encoder** | `varint/array.go` — **366 lines** | ~250 | See below. |

The last two are 60% of the work and neither is a straight port — both are
**size optimizers**, and that is the scoping decision.

### 2.3 The two size optimizers

**`packed5`** picks the cheaper of a raw payload and a packed 5-bit token
stream. The packed path is a greedy left-to-right scan with rules for case-toggle
runs (simple vs long), digit runs vs the 10-bit number opcode, a 30-entry symbol
table, and multi-byte escapes — and it computes the exact cost of **all four**
header-flag settings in one pass to pick the cheapest
(`packed5/encode.go:17-22`). Its own comment records that it is not size-optimal
but is byte-identical to a brute-force shortest-path reference on realistic
input.

**`varint::AppendArray`** maps the input through one of four transforms (raw,
delta, frame-of-reference, fixed), optionally zigzags, and searches `k` in 1..4
against 8 `d` codes — **scoring every combination and keeping the smallest**
(`varint/array.go:8-11`).

Both have a **trivially valid** alternative, because the Go decoder reads any
well-formed message:

- packed5: the raw path (`PACKED_5 = 0`) — header byte, then the bytes. ~20 lines.
- varint array: one fixed choice, e.g. `trFixed` (little-endian words of the
  element width) or `trRaw` at a computed `k`/`d`. ~60 lines.

So the encoder is really one of:

| | strings | int arrays | Rust lines | Rust output vs Go's |
|---|---|---|---|---|
| **A. byte-exact** | full port | full port | ~1030 | **identical** for every value |
| **B. valid only** | raw frames | one transform | ~460 | decodes the same; **larger** |
| **C. hybrid** | full port | one transform | ~610 | identical unless the record has an int array |

Byte-exactness is not about correctness — B round-trips fine. It is about
**testability**: A is the only option where the test is "Rust's bytes == the
committed Go vector", which is the test that actually catches drift. Under B the
test has to be a round-trip, which needs new machinery (§2.4).

The size cost of B is real and concentrated in strings: a token's `User` field is
exactly the short lowercase string packed5 was built for. On `"tester"` the
packed path is 5 bytes against 7 raw; on a longer name the gap widens.

### 2.4 Testing an encoder, in whichever shape

Today's vectors run **Go writes → Rust reads** (`rust/vectors/main.go` →
`rust/tests/vectors.rs`), and CI regenerates and diffs. An encoder needs the
reverse direction, and that is new machinery either way:

- **Under A**, the existing corpus is enough: for every vector, assert
  `encode(schema, decode(bytes)) == bytes`. A pure Rust test, no new Go, and it
  pins byte-exactness on every message already committed. **This is the cheap,
  strong test, and it only exists under A.**
- **Under B or C**, that assertion is false by design. The test becomes: Rust
  writes a corpus, a **new Go test** decodes it with `colbin.Unmarshal` and
  compares against the expected values — a second CI job with a Rust→Go
  direction, plus a committed Rust-written corpus and the expected values in a
  form Go can read.

Either way I would also add: encode → decode in Rust for every vector's values,
and a `cargo test` case per new wire form.

### 2.5 What an encoder costs beyond the lines

`RATIONALE.md` argued against a Rust encoder: *"nothing in Rust writes colbin,
and an unused encoder would be a second copy of the specification to keep honest
for free, and the first thing to rot."* That reasoning does not survive an actual
caller, which is presumably the point of asking. But the mitigation has to be
real, and it is §2.4 — without a Rust→Go check in CI, the encoder rots exactly as
predicted, and silently, because a decoder that reads its own output proves
nothing.

Worth stating plainly: **compact mode has no version byte.** A change to its
header is undetectable in an already-encoded message (`README.md`, Limitations),
so a Go-side format change with a Rust encoder in the field means both ends must
ship together.

---

## 3. Files

| file | change |
|---|---|
| `rust/src/lib.rs` | `Kind`/`Value`/`Schema` become trees; `MAX_DEPTH`; nested schema builders; new `Error` variants |
| `rust/src/compact.rs` | recursive `read_value`; `min_bits`; depth tracking |
| `rust/src/standard.rs` | **only if standard-mode composites are in scope** — sub-tables, flatten/split, null bitmaps, `any` |
| `rust/src/bitstream.rs` | `BitWriter` (new) |
| `rust/src/varint.rs` | `put_varint`; array encoder (scope per §2.3) |
| `rust/src/packed5.rs` | frame encoder (scope per §2.3) |
| `rust/src/encode.rs` | new: the compact writer, key runs, omit-zero, `ALL_POSITIVE`, `NARROW_KEYS` |
| `rust/tests/vectors.rs` | composite cases; the encoder assertion of §2.4 |
| `rust/vectors/main.go` | composite vectors — **these do not exist yet**, since Go refused these types until now |
| `README.md`, `RATIONALE.md` | the Limitations entries I just wrote say the Rust side is decode-only and flat; both change |

## 4. Sequencing

§0 → §1.1 → (§1.2 if in scope) → §2. Each is independently shippable and each
leaves `cargo test` green. I would land §0+§1.1 first: it is what makes the Go
change I already made readable in Rust, it is ~270 lines, and it does not commit
to anything about the encoder.

## 5. Settled

1. **Compact mode only.** Standard-mode composites stay out. A columnar message
   carrying a nested struct keeps its present clear error. §1.2 is not built.

2. **Encoder shape: raw strings, full integer optimization.** Not one of A/B/C —
   the inverse of C:

   | | Rust lines | vs Go's bytes |
   |---|---|---|
   | packed5 frames | ~25 | **raw payload only** (`PACKED_5 = 0`) — larger |
   | varint arrays | ~250 | **identical** — the full transform/zigzag/k/d search |
   | bitstream, selector varint, record writer | ~380 | identical |

   ~655 lines. Strings are the one place Rust's output differs from Go's.

   **A raw frame is still a packed5 frame** — header byte, the uvarint length
   escape above 30, then the bytes. It is not a bare string, or Go could not
   decode it. Go's own encoder already emits raw frames when raw is the cheaper
   of the two, so this is a path already on the wire and already read; verified
   at every length including the 30/31 escape boundary.

   Measured size cost of dropping the packed path:

   | string | raw | packed5 |
   |---|---:|---:|
   | `""` | 1 B | 1 B |
   | `"tester"` | 7 B | 6 B |
   | 8 chars | 9 B | 8 B |
   | 30 chars | 31 B | 21 B |
   | 300 chars | 303 B | 191 B |

   So +1 byte on the short identifiers a token carries, and up to +59% on long
   lowercase text. Dropping the packed path is also a *CPU win* on encode — it
   removes a greedy scan and a four-way flag cost plan per string.

3. **The caller is fareward ↔ Go backend traffic, with mirrored structs.** So
   both directions must work, which they now do asymmetrically: Go writes packed
   frames that Rust reads, Rust writes raw frames that Go reads. The `Schema`
   declaration is the mirror of the Go struct — the pattern `fareward` already
   uses at `src/bridge/token.rs:57`— and `Record`/`Value` is the payload, so
   `encode` is exactly `decode` inverted.

4. **Benchmarks are part of the deliverable** (§6).

## 5b. Built — what landed and what it measured

All of §0, §1.1 and §2 are in. §1.2 (standard-mode composites) is not, as agreed.

| | |
|---|---|
| `Kind`/`Value`/`Schema` | trees; `MAX_DEPTH` 32 checked at build; `Record` is now constructible (`new`/`with`/`from_fields`) because the encoder needs it |
| compact decoder | recursive: nested structs, arrays of structs, maps, nested slices, any depth |
| compact encoder | `encode` / `encode_one`; raw packed5 frames, full integer-array transform search |
| corpora | `composites.json` (13 Go-written composite messages), `rust_encoded.json` (30 Rust-written) |
| tests | 20 passing: 5 flat corpus, 5 composite corpus, 3 encoder, doctests — plus the Go-side verifier |

Two things surfaced while building that were not in the plan:

- **`Kind::Array` of a bare scalar is refused at schema-build time.** No Go type
  produces it — a slice of scalars is its own kind and rides a bulk codec — so an
  encoder that accepted it would write a message Go's decoder misreads. `Array`
  carries composites and `Bytes`; `[][]int32` is `Array(Int32s)`, which is legal.
- **Go already emits raw frames for short strings.** It picks the cheaper of raw
  and packed per string, and raw wins up to about six letters, so "Rust does not
  pack" costs far less than it sounds and half the corpus stays byte-comparable.
  The generator now records `rustByteExact` per case by asking `packed5` directly,
  which is what lets the encoder test be an exact assertion instead of a warning.

### Measured

`cargo run --release --example bench`, subject `benchStats` from
`codec/typed_bench_test.go` — seven numbered integer fields, 17 B compact:

| | free function | `Codec` | `TypedCodec` + derive | Go `Codec[T]` |
|---|---:|---:|---:|---:|
| encode | 124 ns/op | 65 ns/op | **46 ns/op** | ~120 ns/op |
| decode | 222 ns/op | 190 ns/op | **103 ns/op** | ~150 ns/op |

Both handle columns are zero-allocation on a reused buffer. `TypedCodec` needs
the `derive` feature and carries the flat forms; composites stay on `Codec`.

Measured as the minimum of 25 batches, not the median: this machine's noise is
large enough to invert a comparison, and taking the median moved a fixed cost
from 19 ns to 40 ns between runs of the same binary.

Encode is **faster than Go's**, which allocates nothing — the difference is that
streaming the packed5 and varint frames through the bitstream removed two scratch
buffers Go still assembles per message.

Decode was ~46 ns behind Go on the dynamic path, and that was the API rather than
the codec: an owned `Vec<(u8, Value)>` with a 32-byte `Value` per field, where Go
writes into a struct. `#[derive(Colbin)]` removes it — the value goes into the
field it belongs to — and takes decode past Go as well.

See `RATIONALE.md` for the three optimisations that measured as dead ends,
including Go's unaligned-load fast path, which made the Rust reader *slower*, and
for the unsigned-slice bug the typed path's byte-equality test caught.

### Still open

- **The derive carries only the flat forms.** A nested struct, a slice of structs
  or a map needs the schema of the type one level down at the point of each read,
  which means a precomputed tree of id-to-index tables. `Codec` carries them
  today; extending the derive is the next step if a mirrored struct needs one.
- **Strings cost ~60 ns/field to decode**, mostly an unavoidable `String`
  allocation plus shifting an unaligned tail. Bounding the shift to the frame's
  own length — readable from the packed5 header — is the available win.
- **`fareward` pins colbin by `rev`**, so none of this reaches it until colbin is
  pushed and the pin bumped.

Integer arrays, through a one-field record, so the figure includes the framing
every other row also pays:

| | ns/op | bytes |
|---|---:|---:|
| sorted ids, 64 × i32 | ~1300–1470 | 70 |
| scattered, 64 × i32 | ~1470–1630 | 166 |
| full-width, 64 × i64 | ~1235 | 517 |
| small run, 8 × i32 | ~645 | 12 |

That is the transform search, deliberately kept: four transforms histogrammed and
scored against 32 (k, d) pairs. It is what turns 64 sorted ids into 70 bytes.

**Size, Rust against Go, over all 30 compact cases: 685 B against 654 B, +4.7%.**
16 cases differ, every one of them by a string; the worst single record is +6 B
(`cc-nested-only`, whose one field is a 19-character string). The other 14 are
byte-identical.

## 6. Benchmarks

Two questions, both of which the decisions above make worth measuring rather than
asserting:

- **Size.** Rust-encoded vs Go-encoded, same value, over the vector corpus. The
  only expected difference is strings; this quantifies it per realistic record
  rather than per string.
- **Speed.** Rust encode and decode, ns/op, against the Go `Codec[T]` figures
  (`BenchmarkRecordAppendCodec` ≈ 120 ns/op, `BenchmarkRecordUnmarshalCodec`
  ≈ 150 ns/op for a 5-field record). Also the integer-array encoder on its own,
  since the transform search is the one thing deliberately kept expensive.

`cargo bench` needs a harness; criterion is the usual choice but is a heavy dev
dependency. Plan is `#[bench]`-free plain `cargo test --release` timing harness,
or criterion if you would rather have the statistics — noted as a small open
choice, not a blocker.
