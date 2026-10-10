# colbin

colbin is a binary serialization format for Go structs, with Rust and
JavaScript (WebAssembly) implementations that read and write the same bytes.

**It gives the compression and performance of Protocol Buffers without a
precompilation step and without dependencies in Go.** An optional
self-describing mode, colbin/JSON, makes a message convertible to JSON with
nothing else on hand.

- **No precompilation step.** The Go struct is the schema. There is no `.proto`
  file, no `protoc` and no generated types; `cb` field tags are optional.
- **No dependencies.** The Go module's `go.mod` has no `require` block.
- **Protobuf-level size and speed.** In the [measurements](#measurements) colbin
  is at least as small as protobuf on every shape. On slices of structs it is
  several times smaller, because it writes them as compressed columns. It
  encodes 2–6× faster and decodes 1.8–7× faster.
- **Self-describing mode (colbin/JSON).** The schema section travels inside the
  message, so `ToJSON` renders it without the Go type. The section adds under
  1% to a table or a document.

```go
data, err := colbin.Marshal(&order)            // a struct, no tags required
err = colbin.Unmarshal(data, &back)

doc, err := colbin.MarshalSelfDescribing(&order) // colbin/JSON
text, err := colbin.ToJSON(nil, doc)             // JSON, no schema or type needed
```

- [The problem](#the-problem)
- [Design](#design)
- [Usage](#usage)
- [Measurements](#measurements)
- [Limitations](#limitations)
- [Implementations](#implementations)
- [Where it is used](#where-it-is-used)
- [Documents](#documents)

## The problem

The target is bandwidth between services, storage and clients, without
changing how the code that produces and consumes the data is written.

- **JSON** costs no setup. But every record carries its field names, and every
  number is decimal text. A list of a thousand records repeats every key a
  thousand times.
- **Protocol buffers** remove the names and encode numbers in binary. They need
  a `.proto` file, a compiler step and generated types kept in sync with the
  application's own. They also encode a list of records row by row: each row
  repeats every field tag, and each element pays a tag and a length.
- **Varints**, used by protobuf for keys, lengths and integers, are compact but
  decoded byte by byte, in a loop whose trip count depends on the data.

colbin is designed around four constraints:

1. **The Go struct is the schema.** Field ids come from optional `cb` tags, or
   are derived from field names when there are none.
2. **One format for every shape.** A 30-byte message, a table of thousands of
   rows and a nested document of hundreds of kilobytes use the same encoding.
   The layout adapts per field, not per message.
3. **A message can be read without the producer's type.** A schema section,
   sent out of band or embedded in the message, is enough to render it as JSON.
4. **Decoding never loops on data.** Every size is either in a header or in a
   field whose width the header names.

## Design

Each decision below was kept because it measured better than the alternative.
[`RATIONALE.md`](RATIONALE.md) records the measurements, including the ideas
that were reverted. [`INTERNALS.md`](INTERNALS.md) specifies the format bit by
bit.

### Messages are key runs, at one of two key widths

A message is a root descriptor byte followed by a **key run**: a sequence of
`[key][descriptor][payload]` fields, terminated by the end of the enclosing
length. The run's key width is chosen per run and recorded in the descriptor
that opens it:

| | layout | fields | can skip an unknown field |
|---|---|---|---|
| **K4** (narrow) | `[key:4][desc:4]`, one byte | ids 1–16 | yes: the descriptor says how long the field is; the type comes from the schema |
| **K8** (wide) | `[key:8][desc:8]`, two bytes | ids 1–255 | yes: the descriptor names a class with a known size rule |

A nested struct opens its own run, so a narrow struct can hold a wide one and
the reverse. A type with more than 255 fields is split into **pages** of 255.
Key 255 of each page links to the next, up to id 4 080.
([INTERNALS §3–4, §6.5](INTERNALS.md#3-narrow-key-runs-k4))

### Field ids: tags are optional

A field's id comes from one of two places.

**From a tag.** `cb:"N"` makes the field id N, and its wire key is N−1. A type
whose ids all fit in 1–16 uses the one-byte K4 field header.

**From the field name.** A field without a number takes `fnv8` of its name
(FNV-1a folded to one byte). If that key is taken, it moves to the next free one
upward. Fields are assigned in declaration order, after all tagged fields. The
Rust and JavaScript implementations use the same hash, so an untagged Go struct
is readable from both.

```go
type Reading struct {          // no tags: ids from the names, K8 keys
    SensorID  uint64
    Timestamp int64
    Value     float64
}

type Reading struct {          // tags: explicit ids, K4 keys
    SensorID  uint64  `cb:"1"`
    Timestamp int64   `cb:"2"`
    Value     float64 `cb:"3"`
}
```

Untagged types stay compatible as long as fields keep their names and their
relative order:
- New fields go at the end.
- A rename keeps the old id if the old name is put in the tag: `cb:"SensorID"`.
- Reordering is only unsafe when two names hash to the same byte, because
  declaration order decides which one moves.

What an untagged type costs, measured on a six-field record: derived keys span
0–255, so the type always uses K8. That is one extra byte per present field:
33 B instead of 27, and 44 ns instead of 38 to encode. Decode time is the same.
Both widths skip a field the reader does not know, so producer and consumer can
add or drop fields independently either way.

Tags are therefore an optimization for size and for the narrow fast path, not a
requirement. In a table, keys are written per column rather than per row, so
the key width hardly matters there.

### Scalars are byte-aligned, and sizes are not varints

- **Zero values are omitted.** A field holding `0`, `""`, `false`, or an empty
  slice or map is not written at all.
- **Integers are sign-and-magnitude**, with the byte count in the descriptor:
  - A value takes 1 to 8 bytes, as many as it needs.
  - In K4 the descriptor is a byte count first: 0–3 carry no payload, 4–11
    carry 1–8 bytes, 12–15 a length. The values 1–4 (1–3 and −1 for a signed
    field) are the descriptor itself, so a `bool`, a status code or a small
    count is one byte, key included. A negative value other than −1 takes the
    length form, one byte more.
  - K8 adds a varint form, which the encoder writes only when it is shorter:
    3.34 B against 3.60 per field on ids 0–1000.
- **Floats are written byte-reversed.** The zero bytes of an IEEE-754 value are
  the low mantissa bytes, and reversing them lets the byte count drop them:
  `1.0` is 2 bytes, and a `float64` holding an exact `float32` is 5.
- **Strings and blobs** of up to 8 bytes carry no size in K4: the descriptor's
  byte count is the size. Longer ones carry a length of 1, 3 or 5 bytes. A
  decoded string is a sub-slice of the message, not a copy.
- **No size is a varint.** Every read of a size is a single load of a width the
  header already gave, so decoding never runs a loop whose trip count is data.

([INTERNALS §3.1–3.2, §5](INTERNALS.md#31-the-nibble-sizes-the-field))

### Composites carry a byte length

Nested structs, lists, tables and maps are written with a backpatched byte
length:
- In K8, that length is what lets a reader skip a composite without knowing its
  type.
- In K4, a composite is the length form of the descriptor (`[key:4][1 1 0 f]`,
  the flag a struct's key width or a table), and the class comes from the
  schema. Composites therefore do not force the wide width.

Pointers distinguish "absent" from "zero":
- A nil pointer is omitted.
- A pointer to a zero value writes an explicit zero, 2 bytes. This is the only
  value the format writes just to mark presence.
- A pointer to a slice or a map is encoded as the slice or the map.

### Slices of structs become tables

A `[]Struct` whose element fields are all columnable switches layout at
**8 rows**:

- **Below 8 rows** it is a **list**: each element is a key run.
- **From 8 rows up** it is a **table**: one key per column instead of one per
  field per row.

The choice is made per field, at encode time, and the two layouts have different
descriptor classes, so the reader dispatches on what it finds.

| six-field row | bytes per row |
|---|---:|
| list of structs, 7 rows | 16.9 |
| table, 1 000 rows | 11.0 |

Each integer column goes through the **column codec**:
- First, a transform chosen per column: raw, delta, frame-of-reference or
  constant.
- Then the residuals are bit-packed in blocks of 128 values, at a width chosen
  per block. 128 values at `w` bits is exactly `16w` bytes, so every block is
  byte-aligned and no state crosses a block boundary.

| 256 × int64 | raw | encoded |
|---|---:|---:|
| monotonic ids | 2 048 B | 171 B |
| timestamps | 2 048 B | 235 B |
| constant zeros | 2 048 B | 3 B |
| random | 2 048 B | 2 051 B |

Decode runs at 1.3 ns per value and encode at 3.5. A struct containing a nested
struct or a slice cannot be a column, and it stays a list at any length.
([INTERNALS §6.2–6.3, §7](INTERNALS.md#62-slices-of-structs-a-list-or-a-table))

### packed5 (opt-in)

`colbin.SetPacked5(true)` packs strings drawn from upper-case letters and
digits at about 5 bits per character:
- The encoder decides per string and falls back to plain bytes when packing
  would not be shorter, so turning it on never grows a message.
- The choice is recorded in each string's descriptor, so the reader needs no
  configuration.
- It costs a pass over every string on both sides, which is why it is off by
  default.

([INTERNALS §8](INTERNALS.md#8-packed5-packed5))

### The schema section, and colbin/JSON

The wire carries no names and no types. The **schema section** is the type's
encoding plan serialized: a key, a name and an op code per field. Nested and
recursive types are hoisted into an indexed struct table, so a recursive type
describes itself in finite space. It can travel two ways:

- **Out of band**, once per connection. Messages stay exactly the size
  `Marshal` produces.
- **Embedded** (`MarshalSelfDescribing`). This is **colbin/JSON**, the
  self-describing mode: root byte `0xD4`/`0xDC`, then the section, then the
  body. The body is byte for byte what `Marshal` writes. `Unmarshal` skips the
  section, and `ToJSON(nil, msg)` renders the message with no schema and no Go
  type available.

The section is 38 to 448 bytes for the types measured here. On a table or a
document that is under 1% of the message. On a single small record it is most
of the message, which is why a stream of small messages should send the
section once instead.

`ToJSON`'s output has the same content as `encoding/json`'s, with three
differences:
- Fields a record omitted are rendered after the ones it holds.
- Empty slices and maps render as `null`.
- NaN and infinity are refused.

([INTERNALS §10, §12](INTERNALS.md#10-the-schema-section))

### Dynamic values

`any`, `[]any` and `map[string]any` are carried anywhere a typed field can go.
A dynamic value writes its own type on the wire: one descriptor byte naming
integer, float, string, blob, list, map, null, true or false.

An array of records inside a dynamic value is written behind a tag that refers
to a struct in the section. The rows are then the same table a typed field
would write. A thousand five-field records take:
- 26 024 B as a typed `[]User`;
- 26 067 B as `map[string]any{"rows": users}`;
- 78 196 B in `encoding/json`.

([INTERNALS §9](INTERNALS.md#9-dynamic-values))

## Usage

```sh
go get github.com/ivanjoz/colbin
```

```go
type SaleLine struct {
    ProductID uint32
    Quantity  uint32
    UnitCents int64
}

type Sale struct {
    ID         uint64
    CreatedAt  int64
    TotalCents int64
    Lines      []SaleLine // a table from 8 lines up
}

data, err := colbin.Marshal(&sale)
err = colbin.Unmarshal(data, &back)
```

**A handle** resolves the type's plan once. After that, encode and decode
allocate only what the decoded value itself needs:

```go
var saleCodec = colbin.MustCodec[Sale]()

buf, err = saleCodec.Append(buf[:0], &sale)
err = saleCodec.Unmarshal(buf, &back)
```

**Generated code**: `codec.Generate` emits the straight-line calls against the
`wire` package that one would write by hand.

| ten-field record | encode | decode |
|---|---:|---:|
| hand-written against `wire` | 7.6 ns | 22.4 ns |
| `codec.Generate` | 6.9 ns | 22.9 ns |
| `Codec[T]` handle | 23.5 ns | 27.6 ns |
| `Marshal` / `Unmarshal` | 52.7 ns | 48.5 ns |

**Reading without the type:**

```go
schema, _ := colbin.SchemaFor[Sale]()
section := schema.Bytes()                      // send once

parsed, _ := colbin.ParseSchema(section)       // on the reader
text, _ := colbin.ToJSON(parsed, message)
value, _ := colbin.DecodeAny(parsed, message)  // map[string]any

standalone, _ := colbin.MarshalSelfDescribing(&sale)   // colbin/JSON
text, _ = colbin.ToJSON(nil, standalone)
```

## Measurements

**Setup.** Measured on an i7-1355U with Go 1.27.0, best of three runs, by
`bench/formats_test.go` in [colbin-benchmarks][bench-repo]:
- **JSON** is `encoding/json`.
- **protobuf** runs through its generated code. Tables and documents are
  written as the `repeated` fields of a wrapper message, which is byte for byte
  what generated code emits.
- **colbin** is `Codec[T]` with tagged types, appending to a reused buffer.
- **colbin/JSON** is `MarshalSelfDescribing`, which allocates its result.
- Decoding allocates a fresh value per call in JSON and protobuf, and reuses
  one in colbin.

| shape | content |
|---|---|
| Reading | one record, six fields, one of them an `[]int32` |
| Order | one record with three nested lines |
| Product | one record of mostly strings: SKU, name, three categories |
| Metrics | 2 000 rows × 3 integers, one message |
| Sales | 300 rows with 1 514 nested lines, one message |
| Dataset | six tables, 2 617 rows, one message |

### Size (bytes)

| | JSON | protobuf | colbin | colbin + packed5 | colbin/JSON |
|---|---:|---:|---:|---:|---:|
| Reading | 110 | 32 | 27 | 27 | 87 |
| Order | 198 | 49 | 46 | 44 | 121 |
| Product | 148 | 81 | 81 | 72 | 137 |
| Metrics | 88 645 | 26 000 | 3 979 | 3 979 | 4 017 |
| Sales | 186 938 | 34 168 | 26 875 | 26 875 | 27 058 |
| Dataset | 317 364 | 80 503 | 49 434 | 47 992 | 49 882 |

| gzip | JSON | colbin | colbin/JSON |
|---|---:|---:|---:|
| Metrics | 9 548 | 2 638 | 2 675 |
| Sales | 20 702 | 17 393 | 17 546 |
| Dataset | 38 029 | 27 509 | 27 849 |

**Reading the size tables:**
- **Single flat records.** colbin and protobuf are within a few bytes. Both
  omit zeros and write one key per present field, and the difference is
  colbin's one-byte K4 header against protobuf's tag varint.
- **Lists of records.** The difference is the table layout: the 2 000 metric
  rows take 26 000 B in protobuf and 3 979 B in colbin.
- **Sales** gains less than Metrics because only sales with 8 or more lines
  are written as tables (see the corpus split below).
- **colbin/JSON** costs the size of its section.
- **gzip** still finds redundancy in colbin, and the gzipped colbin stays
  smaller than gzipped JSON.

### Time

| encode | JSON | protobuf | colbin | colbin + packed5 | colbin/JSON |
|---|---:|---:|---:|---:|---:|
| Reading | 414 ns | 118 ns | 40 ns | 50 ns | 134 ns |
| Order | 700 ns | 267 ns | 122 ns | 145 ns | 273 ns |
| Product | 416 ns | 168 ns | 44 ns | 129 ns | 230 ns |
| Metrics | 236 µs | 179 µs | 30 µs | 30 µs | 38 µs |
| Sales | 467 µs | 227 µs | 95 µs | 94 µs | 154 µs |
| Dataset | 886 µs | 480 µs | 141 µs | 166 µs | 228 µs |

| decode (into the Go type) | JSON | protobuf | colbin | colbin + packed5 | colbin/JSON |
|---|---:|---:|---:|---:|---:|
| Reading | 958 ns | 172 ns | 64 ns | 63 ns | 65 ns |
| Order | 1 717 ns | 540 ns | 279 ns | 337 ns | 288 ns |
| Product | 882 ns | 389 ns | 210 ns | 419 ns | 204 ns |
| Metrics | 546 µs | 213 µs | 31 µs | 29 µs | 30 µs |
| Sales | 1 187 µs | 250 µs | 140 µs | 144 µs | 140 µs |
| Dataset | 2 023 µs | 564 µs | 229 µs | 278 µs | 236 µs |

| to JSON text | colbin, section held | colbin/JSON | `json.Marshal` from the structs |
|---|---:|---:|---:|
| Reading | 332 ns | 864 ns | 414 ns |
| Order | 573 ns | 1 289 ns | 700 ns |
| Product | 488 ns | 1 014 ns | 416 ns |
| Metrics | 162 µs | 168 µs | 236 µs |
| Sales | 429 µs | 450 µs | 467 µs |
| Dataset | 707 µs | 718 µs | 886 µs |

**Reading the time tables:**
- **colbin/JSON decodes** at plain colbin's speed, because the section is
  skipped.
- **colbin/JSON encodes** slower than plain colbin because it serializes the
  section and allocates a new buffer every call.
- **Rendering JSON from colbin/JSON** parses the section per call, about
  0.5 µs. That is visible on single records and negligible on tables.
- **packed5** costs 3× on encode and 2× on decode for the string-heavy Product,
  for a saving of 9 bytes (11%). On numeric data it costs nothing and saves
  nothing.

### The corpus split between lists and tables

The Sales shape generates line counts on both sides of the threshold, so both
layouts appear in one dataset:

| 300 sales, 1 514 lines | sales | lines | bytes per line |
|---|---:|---:|---:|
| list (< 8 lines) | 246 | 820 | 21.9 |
| table (≥ 8 lines) | 54 | 694 | 12.8 |

### In the browser

The npm package decodes a table into a flat typed buffer in WebAssembly. A row
builder, generated per field signature and cached, then turns that buffer into
objects, so no JSON text is produced or parsed. Measured on 1 000 product
records under Node 24 (`bun run bench` in `js/`):

| | wire | gzip | to objects |
|---|---:|---:|---:|
| JSON + `JSON.parse` | 106 670 B | 12 124 B | 0.26 ms |
| colbin + `codec.unmarshal` | 30 475 B | 3 250 B | 0.14 ms |

### Reproducing

```sh
git clone https://github.com/ivanjoz/colbin-benchmarks && cd colbin-benchmarks
go test ./bench -run FormatSizes -v          # sizes, and round-trip checks
go test ./bench -bench Formats -benchmem     # timings
```

The corpus is generated deterministically by `corpus.Generate`, and that
repository pins it with a SHA-256 checksum. Comparative benchmarks live there
so that colbin's `go.mod` stays free of protobuf.

## Limitations

- **A field's type is not on the K4 wire.** A reader steps over a field it does
  not know, but a field whose type changed under the same id is read as the new
  type. Give a changed field a new id.
- **Untagged ids depend on field names and declaration order**, as described
  under [Field ids](#field-ids-tags-are-optional).
- **Not supported yet:** fixed-size arrays, `map[K]*Struct` and `map[any]T`.
  `int` and `uint` are encoded as 64-bit.
- **An empty slice and a nil slice are the same on the wire.** Both decode as
  nil, and a pointer to an empty slice decodes as a nil pointer.
- **No compatibility promise across minor versions while on `0.x`.** Every
  side must run the same version, and a mismatch shows up as corrupt data
  rather than a version error.
- **Known implementation gaps** are listed in
  [INTERNALS §20](INTERNALS.md#20-known-gaps). For example, the Rust reader
  does not yet handle a pointer to a struct (op 27), or narrow maps.

## Implementations

| | location | notes |
|---|---|---|
| Go | the repository root | the reference implementation: reflection codec, `Codec[T]`, code generator, schema section, JSON rendering |
| Rust | `rust/` | `#[derive(Colbin)]` emits straight-line encode and decode; JSON → colbin encoder with schema inference ([`rust/ENCODER.md`](rust/ENCODER.md)) |
| JavaScript | `js/`, npm `colbin` | the Rust crate compiled to WebAssembly; decodes to objects, columns or JSON text, encodes from JSON |

```rust
#[derive(Colbin)]
struct Charge {
    #[cb(1)] company_id: u32,
    #[cb(2)] note: String,
}
let message = charge.encode();
let back = Charge::decode(&message)?;
```

```js
import { Codec } from 'colbin'
const codec = await Codec.open()
codec.setSchema(section)
const rows = codec.unmarshal(message)
```

The implementations are pinned to each other with test corpora that Go writes:
- **Rust** decodes Go's bytes, encodes the same bytes from the same values, and
  renders every document walk to Go's exact JSON.
- **JavaScript** does the same for every type in its corpus, and refuses every
  single-byte corruption that Go refuses.
- **CI** regenerates each corpus and fails on any difference.

([INTERNALS §18](INTERNALS.md#18-keeping-three-implementations-in-step))

## Where it is used

colbin is used in [Genix](https://github.com/ivanjoz/genix), a self-hostable
ERP and e-commerce platform:

- **Backups.** Tenant data exports, one `.colbin.zstd` entry per batch of
  table rows.
- **[genix-orm](https://github.com/ivanjoz/genix-orm).** Struct, slice and map
  columns stored as colbin blobs in ScyllaDB, and whole records in DynamoDB.
- **Configuration and sessions.** The per-company configuration document, and
  the session token.
- **Next:** responses from the Genix backend to its frontend, through the npm
  package.

Two more projects use it:

- **[fareward](https://github.com/ivanjoz/fareward)**, inside Genix: framing
  between the Go client and the Rust services (credits, locks, budgets, the
  request log). Go uses `Codec[T]` and Rust uses `#[derive(Colbin)]`.
- **[genix-search](https://github.com/ivanjoz/genix-search)**: query requests
  and responses.

## Documents

| | |
|---|---|
| [`INTERNALS.md`](INTERNALS.md) | the format bit by bit, the Go codec, the Rust crate, the WebAssembly module, the npm package, cross-language testing and release |
| [`RATIONALE.md`](RATIONALE.md) | the decisions and the measurements behind them, including reverted ones |
| [`rust/README.md`](rust/README.md) | the Rust crate and its derive |
| [`rust/ENCODER.md`](rust/ENCODER.md) | encoding JSON to colbin: inference, refusals, warnings |
| [`js/README.md`](js/README.md) | the npm package |
| [colbin-benchmarks][bench-repo] | the comparative benchmarks and the pinned corpus |

[bench-repo]: https://github.com/ivanjoz/colbin-benchmarks
