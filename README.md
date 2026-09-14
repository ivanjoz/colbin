# colbin

A byte-aligned binary format for Go structs, built for wires that are
latency-bound: many small messages, a hot path, a known type on both ends.

```go
data, err := colbin.Marshal(&charge)
err = colbin.Unmarshal(data, &back)
```

**No dependencies.** `go.mod` has no `require` block and there is no `go.sum`
beside it, so importing colbin adds nothing to your module graph — not a
download, not a version floor, not a line in your SBOM. The benchmarks that need
protobuf live in their own repository, [colbin-benchmarks][bench-repo], so that
this one can say that without an asterisk.

Against protocol buffers on the same six-field record, protobuf driven through
its generated code:

| one flat record | protobuf | colbin | |
|---|---:|---:|---|
| encode, reusing a buffer | 123 ns | **39 ns** | 3.2× |
| encode, onto a fresh buffer | 140 ns | **76 ns** | 1.8× |
| decode | 116 ns | **61 ns** | 1.9× |
| bytes | 32 | **27** | |

| one order, three nested lines | protobuf | colbin | |
|---|---:|---:|---|
| encode | 271 ns | **81 ns** | 3.3× |
| decode | 485 ns | **314 ns** | 1.5× |
| bytes | 49 | **46** | |

colbin through `Codec[T]`, protobuf through its generated code. Generating the
colbin codec instead takes the flat record to **27 ns** encode and **55 ns**
decode — 4.5× and 2.1×.

Every number above is reproducible from [colbin-benchmarks][bench-repo] —
`go test ./bench -bench .`, on an i7-1355U, Go 1.27, best of eight in one run,
packed5 off. Run a benchmark alone and it lands 5–10% faster than it does in the
sweep; both sides are measured the same way, so the ratios hold either way.

## Why it is fast

Nothing is packed across a byte boundary, and **no size is a varint**. A header
carries the common size and, when it does not fit, names the *width* of the one
that follows — so no read is ever a loop whose trip count is data, and a string
field is a sub-slice of the message rather than a copy.

A field holding its zero value is not written at all, which is where most of the
saving comes from.

There is one format. There used to be three modes and a byte at the front to
tell them apart; that byte is now the root value's own descriptor, and it says
the class and the key width.

## Usage

A struct with no tags at all works: each field takes `fnv8` of its name, linear
probed past anything already used. That key lands anywhere in 0..255, so an
untagged type uses **eight-bit keys** — which costs a byte per present field and
buys `Skip` over an unknown one.

```go
type Charge struct {
    CompanyID int32   // id = fnv8("CompanyID")
    Name      string
}
```

Numbering the fields is how a type asks for the four-bit key, and the number is
what a reader in another language has to agree on.

**Field ids start at 1.** Four key bits hold sixteen fields, so a narrow type
numbers 1..16 and a wide one 1..256.

```go
type Charge struct {
    CompanyID int32  `cb:"1"`
    UserID    int32  `cb:"2"`
    RouteID   uint16 `cb:"3"`
    Name      string `cb:"4"`
}

data, err := colbin.Marshal(&charge)
```

The byte on the wire is the id minus one. It has to be: a key is a bare nibble
or a bare byte with every value spoken for, so there is no spare encoding to
reserve for a zero that means "absent". `cb:"1"` writes key 0 and `cb:"16"`
writes key 15.

That subtraction is the only place the two numbers differ, and it is worth
knowing about in exactly one situation — reading bytes. `colbin.FieldIDs(v)` and
the schema section both report the **key**, because both describe a message that
already exists rather than the tags that produced it. So `CompanyID` above is
`cb:"1"` in source and `0` everywhere you inspect the encoding.

### A hot path should hold a handle

`Codec[T]` resolves the plan once and allocates nothing per record.

```go
var chargeCodec = colbin.MustCodec[Charge]()

buf := make([]byte, 0, 64)
for _, charge := range charges {
    buf = chargeCodec.Append(buf[:0], &charge)
    send(buf)
}
```

### A hotter one should generate the codec

`codec.Generate` emits the straight-line calls, which is about three times
faster than the reflective walk:

| ten-field record | encode | decode |
|---|---:|---:|
| hand-written against `wire` | **5.1 ns** | **14.8 ns** |
| generated | 8.8 ns | 21.1 ns |
| `Codec[T]` handle | 22.3 ns | 25.3 ns |
| `Marshal` / `Unmarshal` | 50.9 ns | 43.3 ns |

Taken in one run; absolute figures move ±20% between runs on this machine, so
compare rows against each other rather than against a number taken elsewhere.

```go
source, _ := codec.GenerateString("billing", Charge{})
os.WriteFile("charge_colbin.go", []byte(source), 0o644)
```

## Key widths

A field id is four bits or eight, chosen **per key run** rather than per message.

| | cost | buys |
|---|---|---|
| 4-bit | — | the fast path: 5.3 ns encode, 14.8 ns decode |
| 8-bit | a byte per present field | 256 ids, `Skip` over an unknown field, packed5 |

### The three ways to hand colbin a struct

Same six-field sensor reading, same run, protobuf through its generated code:

| | encode | decode | bytes |
|---|---:|---:|---:|
| protobuf | 123 ns | 116 ns | 32 |
| **colbin + tags** (4-bit keys) | **40.6 ns** | **62.3 ns** | **27** |
| colbin untagged (8-bit keys) | 44.2 ns | 70.5 ns | 33 |
| colbin + packed5 (8-bit keys) | 52.1 ns | 69.4 ns | 33 |

Untagged costs 9% on encode and 13% on decode, and six bytes — one per present
field. All of that is the key width, not the hashing, which happens once when the
plan is built.

packed5 on this record is pure loss: its only string is `"C"`. On a record that
plays to it — a product with a SKU, a name and three category strings — it is
still close to a wash:

| string-heavy product | encode | decode | bytes |
|---|---:|---:|---:|
| protobuf | 170 ns | 389 ns | 81 |
| **colbin + tags** | **43.1 ns** | **230 ns** | 81 |
| colbin + packed5 | 282 ns | 427 ns | **80** |

One byte, for six times the encode cost. The packing saves 7 bytes and the wide
key it forces costs 6. **Turn it on only when strings dominate the record and
size matters more than speed.**

### The two widths encode integers differently

They are optimised separately, because what is scarce differs.

**K4** has four descriptor bits and a reader that already knows the type. An
unsigned field therefore spends no sign bit: all sixteen codes carry
information, `0..7` being the value itself with no payload at all and `8..15` a
magnitude of one to eight bytes. A `bool`, a small count or a flag is a single
byte, key included, and the widths are exact — a seven-byte magnitude costs
seven where the signed form still rounds it to eight.

**K8** has already spent a byte on the key, so its descriptor starts empty. It
gets a second form: a varint that carries three value bits in the descriptor and
seven per byte after it. The writer emits it **only when it is shorter** than the
sign-and-magnitude form, so nothing on the wire ever got bigger — a random
`int64` still takes the byte count, because seven bits per byte loses to eight
once a value is wide.

| average bytes per field | sign+magnitude | with varint |
|---|---:|---:|
| small ids 0..1000 | 3.60 | **3.34** |
| deltas −1000..1000 | 3.67 | **3.42** |
| random int32 / int64 | 5.99 / 9.99 | 5.99 / 9.99 |

A signed field zigzags into the varint and an unsigned one does not, which is
safe for the same reason the K4 split is: the schema picks the reader. An
unknown field stays skippable either way — the varint is self-delimiting and
lives under a class, which is what `Skip` walks.

A type goes wide when it has an id above fifteen, or `SetPacked5` on and a
string to spend it on. A nested struct, slice of structs, map or table does
*not* force it: a narrow descriptor carries the byte length those need, with the
class coming from the schema. Nothing else changes, and a
narrow-keyed struct can hold a wide-keyed one or the reverse — the width is in
the descriptor that opens each run.

**A narrow message cannot skip an unknown field.** Four descriptor bits have no
room for a class, so a reader that does not know a key cannot size it. Adding a
field is a coordinated deploy of both sides unless the type is on the wide path.

## packed5

`colbin.SetPacked5(true)` turns on a string encoding worth about five bits per
character on upper-case alphanumerics. It is **off by default** and it is a
*writer* setting: the encoding is recorded in each string's own descriptor, so a
decoder reads either form without being told.

The encoder chooses per string, so turning it on can never make a message
larger. It costs a pass over every string on both sides, and it puts the type on
the wide key path.

## Supported types

| | |
|---|---|
| yes | `bool`, every sized `int`/`uint`, `float32/64`, `string`, `[]byte`, slices of integers and of strings, nested structs, `[]struct`, recursive types, `map` with string or integer keys, pointers to any scalar or string, `any`, `[]any`, `map[string]any` |
| not yet | arrays, pointers to composites, maps of structs, `map[any]T` |

`int` and `uint` encode as their 64-bit forms, so a message written on one
platform reads on another.

### Pointers are how a field says "absent" rather than "zero"

A nil pointer is omitted and costs nothing. A non-nil pointer *to* a zero value
— `new(int32)`, a `*string` to `""` — writes an explicit zero, two bytes, because
otherwise it would be indistinguishable from nil. That is the one value in the
format written solely to say it is there.

```go
type Patch struct {
    Name  *string `cb:"1"` // nil: leave it alone. &"": clear it.
    Limit *int32  `cb:"2"`
}
```

Pointers to structs, slices and maps are refused: those carry a length already,
and what a nil one should mean is not settled.

## A slice of structs picks its own layout

Past a threshold, a `[]Struct` field is **transposed into a table**: one key per
column rather than one per field per row, with each column through the blocked
column codec.

| six-field row | |
|---|---:|
| list of structs, 7 rows | 16.9 B/row |
| table, 1000 rows | **11.0 B/row** |

The choice is made per field on the row count, and the two are different
descriptor classes, so a reader dispatches on what it finds. A struct with a
nested struct or a slice inside it cannot be a column and stays row-wise however
long it gets.

## Reading a message without the Go type

The wire carries no type — that is where the speed comes from — so a browser, a
`jq`-style tool or any dynamically typed client cannot name a field or tell a
float from an integer. A **schema section** gives it those. It is the same plan
the encoder already resolves from the struct, written out as bytes: a key, a
name and a type code per field, with nested structs hoisted into an indexed
table so a recursive type describes itself in finite space.

Send it once per connection, then send ordinary messages:

```go
schema, _ := colbin.SchemaFor[Sale]()
send(schema.Bytes())                       // once

for _, sale := range sales {
    data, _ := colbin.Marshal(&sale)       // unchanged, and unchanged in size
    send(data)
}
```

and on the other side:

```go
schema, _ := colbin.ParseSchema(section)
text, _ := colbin.ToJSON(schema, message)  // {"ID":1,"UserID":42,...}
value, _ := colbin.DecodeAny(schema, message)
```

`colbin.MarshalSelfDescribing(&sale)` puts the section in front of the body
instead, for a document that has to stand alone. Its root byte is `0xD4` or
`0xDC` — the schema bit, `0x04` — and `Unmarshal` steps over the section, so a
self-describing message still decodes into the Go type.

It is the wrong default for a stream. Measured on the corpus:

| table | schema | B/message | schema/msg |
|---|---:|---:|---:|
| users | 61 B | 61.9 B | 1.0x |
| sales (with detail) | 173 B | 89.5 B | 1.9x |
| metrics | 28 B | 10.8 B | 2.6x |

The JSON is what `encoding/json` would have written for the same record, down to
the escaping and the spelling of numbers — with two exceptions the wire forces:
an empty slice or map is indistinguishable from a nil one and comes out `null`,
and a NaN or an infinity is **refused** rather than quietly written as `null`.
`DecodeAny` keeps them.

Going straight to text is also the faster direction, because the intermediate
`map[string]any` is where all the allocation is:

| 100 corpus users | ns/op | B/op |
|---|---:|---:|
| `colbin.AppendJSON` | 29 500 | 6 512 |
| `encoding/json` on the structs | 40 000 | 14 327 |
| `colbin.DecodeAny` | 41 000 | 54 712 |

Writing colbin *from* JSON is not in this package: it needs type inference, and
it is a separate job. It is a built one — in Rust rather than in Go, because the
case that wanted it was a browser. `colbin::build::encode` takes JSON text and
returns a message and the section that describes it, and `rust/ENCODER.md` is
what it infers, refuses and warns about. Go reads what it writes; that is what
`go test ./js/vectors` checks.

## `map[string]any`, for the part of the answer that has no type

A service answering a browser often holds a shape like this, and the values have
no declared type for the schema to describe:

```go
data, _ := colbin.MarshalSelfDescribing(map[string]any{
    "rows":  sales,   // []Sale
    "total": len(sales),
    "page":  1,
})
```

`any`, `[]any` and `map[string]any` are carried anywhere a field, a slice element
or a map value can go. Such a value is the one thing in colbin that puts its type
*on* the wire — one descriptor byte saying integer, float, string, blob, list,
map, `null`, `true` or `false`.

**An array of records does not pay for that per row.** A `[]Sale` inside an `any`
is written behind a tag naming a struct the schema section describes, and the
rows behind it are byte for byte what a typed `[]Sale` field writes — the table,
the column codec, all of it. A `[]any` that happens to hold one record type is
promoted to the same thing, so an answer assembled dynamically costs what a typed
one costs; a mixed one falls back to a list of self-describing values. On a
thousand five-field records:

| | bytes |
|---|---:|
| `[]User` as the whole message | 26 024 |
| the same inside `map[string]any{"rows": …}` | **26 067** |
| `encoding/json` | 78 196 |

Two caveats worth reading before you rely on it:

- **A struct inside an `any` needs `MarshalSelfDescribing`.** The tag names a
  struct *in the section*, and plain `Marshal` has no section — so there it falls
  back to writing the record as an object with its field names spelled out, which
  is the same document and several times the bytes. If you send an out-of-band
  schema, the schema describes the map and not what the map turned out to hold,
  so the same applies.
- **The keys are on the wire, per message.** A dynamic map spells every key as a
  string every time, which is exactly what a declared type saves you. Use it for
  the subtree whose shape is genuinely unknown, not instead of a struct.

It decodes back into Go as well, with the normalisation `DecodeAny` documents:
records come back as `map[string]any`, integers as `int64` (or `uint64` past
2^63), and both float widths as `float64`. Dynamic maps are written in key order,
so the same value always encodes to the same bytes.

## Layout

```
wire/     the format: field framing, all three key-run framings,
          composites, tables, opt-in packed5
column/   the column codec: blocks of 128 residuals at a chosen bit width
codec/    the reflection façade and the source generator
packed5/  the opt-in string packing
corpus/   a reproducible, real-shaped dataset: users, products, sales
```

That is the whole module. The comparison against protocol buffers is not here —
it lives in [colbin-benchmarks][bench-repo], for the reasons in
[Benchmarks](#benchmarks) below.

### The corpus

`corpus.Generate(corpus.Seed, corpus.Small)` builds the same seven tables every
time — users, products, categories, stores, sales with nested `Detail
[]SaleLine`, events and metrics. Money is integer cents throughout; a sale holds
no string and no float.

The line count per sale straddles the table threshold on purpose, so one dataset
reaches both layouts:

| 300 sales, 1 514 lines | sales | lines | B/line |
|---|---:|---:|---:|
| list of structs (<8 lines) | 246 | 820 | 21.9 |
| table, transposed (≥8) | 54 | 694 | **12.8** |

#### Against protocol buffers, same rows both sides

`bench/corpus.pb.go` in [colbin-benchmarks][bench-repo] is the protobuf twin,
field for field. Cents are `int64`
rather than `sint64` because every amount is non-negative and int64 is the
shorter of the two — protobuf gets its best form, not the matching one.

| table | rows | protobuf | colbin | |
|---|---:|---:|---:|---:|
| users | 100 | 6 275 | 6 187 | −1.4% |
| products | 200 | 12 996 | 12 876 | −0.9% |
| **sales** (nested detail) | 300 | 33 489 | **26 856** | **−19.8%** |
| metrics | 2 000 | 22 000 | 21 680 | −1.5% |
| total | | 74 760 | **67 599** | −9.6% |

Per row, in one run:

| | protobuf | colbin | |
|---|---:|---:|---:|
| user encode | 141 ns | **35 ns** | 4.0× |
| user decode | 244 ns | **86 ns** | 2.9× |
| sale encode | 616 ns | **363 ns** | 1.7× |
| sale decode | 967 ns | **504 ns** | 1.9× |
| metric encode | 61 ns | **15 ns** | 4.0× |

Decoding a sale allocates 5.1 times against protobuf's 9.1.

Note the shape of the size result: on flat records the two formats are within
1.5% of each other — both omit zero fields and write a key per present field, so
there is little to choose between them. **The whole of colbin's size advantage is
in the nested table**, where a slice of integer-only structs is transposed into
columns and protobuf has no equivalent.

`go test ./corpus -run Report -v` prints bytes per row for every table;
`go test ./bench -run CorpusSizes -v`, in [colbin-benchmarks][bench-repo],
prints the comparison above.

`wire` and `column` have no reflection and no type registry — they are driven by
a caller that already knows the Go type, which is what `codec.Generate` emits.

### The column codec

An integer column is a transform — raw, delta, frame-of-reference or constant —
then blocks of 128 residuals packed at an exact bit width chosen per block. 128
values at `w` bits is exactly `16w` bytes, so a block is byte-aligned at both
ends and no state crosses a boundary.

| 256 × int64 | raw | encoded |
|---|---:|---:|
| monotonic ids | 2048 B | 171 B |
| timestamps | 2048 B | 235 B |
| all zeros | 2048 B | 3 B |
| random | 2048 B | 2051 B |

1.3 ns per element to decode, 3.5 to encode.

## Benchmarks

Every comparative number in this README — the tables at the top, the corpus
sizes above — is produced by a **separate repository**:

**→ [github.com/ivanjoz/colbin-benchmarks][bench-repo]**

```sh
git clone https://github.com/ivanjoz/colbin-benchmarks
cd colbin-benchmarks
go test ./bench -bench . -benchmem       # the timing tables
go test ./bench -run CorpusSizes -v      # the size comparison
```

### Why it is a separate repository

Because a dependency you do not use still costs you something. protobuf was only
ever imported by those benchmarks and by the tool that generates their input —
never by a line of colbin itself. A consumer never downloaded it either; Go
fetches only modules whose packages are actually imported.

But `require google.golang.org/protobuf` in this `go.mod` still reached them:
it became a minimum-version constraint in their build, a line in their `go.sum`,
an entry in `go mod graph`, and a row in whatever dependency audit their
employer runs. That is a real cost to charge someone for a comparison they are
not running, and "zero dependencies" is not a claim you can make with an asterisk
attached.

So it moved, and CI here fails if it ever comes back — the check inspects the
module graph rather than just building, because a test-only dependency compiles
fine and still shows up downstream.

### The corpus is pinned, not just generated

`corpus.Generate` is deterministic, but the benchmarks repository does not rely
on that alone. It materialises the corpus as canonical JSON and pins all three
scales with SHA-256, so a reported measurement names the exact dataset that
produced it — verifiable with `sha256sum`, no Go required.

Two things make that worth the trouble. Go's compatibility promise does not
cover the `math/rand` bit stream in writing; it is stable because changing it
would break too much, which is why `math/rand/v2` shipped as a new package
rather than a fix to the old one. And `corpus.Event` carries a
`map[string]string` — Go randomises map iteration order, so the *colbin* bytes
for the Events table are not stable run to run, while `encoding/json` sorts map
keys and the JSON is. The checksum only means something because of that second
fact, and there is a test in that repository asserting it.

## Documents

- `BYTE_ALIGNED_PLAN.md` — the design, its measurements, and what is still open
- `RATIONALE.md` — the decisions, including the ones the measurements reversed
  and the optimisations that did not pay
- `wire/README.md`, `column/README.md` — the layouts
- `rust/README.md` — the Rust port
- `js/README.md` — the npm package, and the numbers the browser client hits
- `PACKAGE_PLAN.md` — why the client decodes to objects without JSON text
- [colbin-benchmarks][bench-repo] — the comparison against protocol buffers, and
  the corpus materialised and pinned by checksum

## Rust

`rust/` is the same format in Rust: the wire at all three key framings, the
column codec, packed5, and a `#[derive(Colbin)]` that emits the straight-line
encode and decode rather than a reflective walk.

```rust
#[derive(Colbin)]
struct Charge {
    #[cb(1)] company_id: u32,
    #[cb(2)] note: String,
}
```

The two ports are pinned to each other rather than to a description.
`rust/vectors/main.go` writes a corpus with the Go codecs — the messages, the
field ids and the columns — and the Rust tests assert both directions against
it, so neither side can move without the other failing.

```sh
go run ./rust/vectors && go test ./rust/vectors
cargo test -p colbin --features derive
```

## JavaScript, in the browser and on Node

`js/` is the client: the Rust implementation compiled to WebAssembly, published
to npm as [`colbin`](https://www.npmjs.com/package/colbin), with a wrapper that
turns a message into JavaScript objects **without JSON text in the middle**.

```sh
npm install colbin
```

```js
import { Codec } from 'colbin'

const codec = await Codec.open()
codec.setSchema(section)                 // sent once per connection
const rows = codec.unmarshal(message)    // objects, no JSON.parse
```

A thousand product records decode to objects in 0.14 ms, against 0.26 ms for
`JSON.parse` on the same data — from 3.5x fewer bytes on the wire, and exactly
past 2^53, which `JSON.parse` is not. `js/README.md` has the whole surface and
the measurements; the demo at [colbin.un.pe](https://colbin.un.pe) is a consumer
of the published package rather than a copy of it.

## Status

Alpha. The wire format is settled, the Go façade covers everything in the table
above, and the Rust port covers the same ground — including the schema section,
which it now both writes and reads. Interfaces have no form on the wire yet.

One tag drives all three implementations, and while on `0.x` there is no
wire-compatibility promise across minor versions: a Go service on 0.3 and a
browser on 0.2 will not interoperate, and the failure looks like corrupt data
rather than a version error. Pin both.

[bench-repo]: https://github.com/ivanjoz/colbin-benchmarks
