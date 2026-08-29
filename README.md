# colbin

A compact columnar binary serializer for **slices of structs** — sized and
tuned for numeric, DB-row-shaped data. It exposes a familiar
`Marshal`/`Unmarshal` surface, but uses a fundamentally different layout from
row-oriented formats such as CBOR or JSON: colbin transposes the data and encodes
it **column-by-column** (SoA). Integer columns use the local adaptive `varint`
codec, while strings use self-delimiting `packed5` frames with a raw fallback.

The codecs choose their own compact or fixed/raw fallback, so wide random numbers
and strings outside the packed alphabet remain bounded rather than inflating
without limit.

Messages come in **two formats** — a columnar *standard mode* for batches and a
bit-level *compact mode* for one to three records. `Marshal` chooses; `Unmarshal`
accepts either. See [Modes](#modes).

## When to use it

Use colbin when you serialize **arrays of homogeneous structs** (every record has
the same fields) where numbers dominate — cache payloads, API list responses, DB
row batches. The wins come from encoding many similar values of one field together.

Single objects and two- or three-record replies are handled too, by compact mode
— a lone flat struct is about 3x smaller than JSON. What colbin is *not* for is
heterogeneous documents, and it must buffer all records before emitting (it is
not a record-at-a-time stream).

## Usage

```sh
go get github.com/ivanjoz/colbin
```

```go
import "github.com/ivanjoz/colbin"

type Product struct {
    ID        int64 `cb:"id"`
    CompanyID int32
    Price     int32
    Active    bool
    Name      string
    Weight    float32
}

rows := []Product{ /* ... */ }

data, err := colbin.Marshal(rows)   // []Product -> []byte

var out []Product
err = colbin.Unmarshal(data, &out)  // []byte -> *[]Product
```

`cb` tags are optional: without them, colbin derives field IDs from the Go field
names. Use an integer tag when you want to assign a stable numeric field ID
explicitly:

```go
type Product struct {
    ID    int64  `cb:"1"`
    Name  string `cb:"2"`
    Price int32  `cb:"3"`
}
```

`Marshal` accepts a slice of structs, a pointer to one, or a single struct
(encoded as `N = 1`). Other supported top-level values, such as maps and scalar
slices, use value mode. `Unmarshal` needs a non-nil pointer to the corresponding
destination. Decoding requires a **compatible Go type** — the format is not
self-describing about field names (see [Field ids](#field-ids)).

### Sparse records: `SetOmitEmpty`

A column is positional — N records, N values — and a value that happens to be
zero still takes its slot. The integer codec floors at a byte per element, so a
thousand records of an untouched field cost a thousand bytes to say nothing, and
a wide struct pays that once per unset field.

```go
colbin.SetOmitEmpty(true)   // once, at startup
```

With it on, a column holding nothing but empty values — `0`, `false`, `""`, an
empty slice, `nil` — is written as its **type byte alone**, with no payload.
Float columns have always done this; the flag extends it to the integer and
string columns, and through them to everything framed by a length sub-column, so
`[]byte`, arrays and maps collapse too.

| ten-field struct, one field set | dense | omit-empty |
|---|---:|---:|
| 100 records | 933 B | **127 B** (−86%) |
| 1000 records | 9034 B | **1028 B** (−89%) |

It also lets **compact mode carry pointer fields**, which it otherwise refuses:
compact mode says a field is present by naming it, so `nil` has to mean absent.
A four-field struct of pointers goes from 30 B columnar to 18 B compact, and to
3 B when they are all nil.

That is the one thing the flag costs, and why it is opt-in:

> a `*T` pointing at `T`'s zero value decodes back as `nil`.

Nothing else changes — slices already lost nil-versus-empty in both directions
before the flag existed, and a zero scalar is a zero scalar. **Decoding needs no
configuration**: both forms are self-describing per column, so a reader never has
to be set to match its writer. A message written with the flag on does carry its
own version byte (`0x06`, or `0x08` in JSON mode), so a decoder that predates the
flag rejects it rather than misreading it.

### Many small messages: `Codec[T]`

`Marshal` works the type out from scratch on every call — what it is, what its
fields are, where they sit, which mode carries them. Over one message holding a
thousand records that is nothing; over a thousand messages holding one record
each it is most of the cost. `Codec[T]` resolves it once and holds it:

```go
var statsCodec = colbin.MustCodec[SaleOrderProductStats]()

buf := make([]byte, 0, 64)
for _, rec := range records {
    buf, _ = statsCodec.Append(buf[:0], &rec)  // no allocation per record
    send(buf)
}

var out SaleOrderProductStats
err := statsCodec.Unmarshal(data, &out)
```

The record arrives as `*T` rather than as `any`, which is the other half of it:
an interface boxes the value, and a struct behind one is not addressable, so it
has to be copied before its fields can be read at all.

10 000 records, each its own message, on the seven-field struct above:

| | time | allocations |
|---|---:|---:|
| `Marshal` | 4.6 ms | 59 003 |
| `Marshal`, today | 2.2 ms | 30 340 |
| `Codec.Append` onto a reused buffer | **1.24 ms** | **0** |
| `Unmarshal` | 5.1 ms | 10 000 |
| `Unmarshal`, today | 1.7 ms | 0 |
| `Codec.Unmarshal` | **1.48 ms** | **0** |

A `Codec` is safe for concurrent use and writes ordinary messages: `Unmarshal`
reads what a `Codec` wrote, and a `Codec` reads what `Marshal` wrote. It covers
structs and slices of them (`Append`/`AppendSlice`, `Unmarshal`/`UnmarshalSlice`)
in binary mode; JSON mode stays on the package functions, where the schema
section is the dominant cost anyway.

## Modes

**Bit 0 of the first byte** selects the format:

```
bit 0 == 0    standard mode — columnar, one column per field, any record count
bit 0 == 1    compact mode  — bit-level, one to three records
```

`Marshal` picks the mode and `Unmarshal` dispatches on that bit, so neither is
something callers choose.

### Standard mode

The default, and what the rest of this document mostly describes. Records are
transposed and written **column by column**: all N values of one field sit
together, which is what lets the `varint` codec find a delta or frame-of-reference
transform across them and what makes similar strings pack well. Every column
carries a field id and a type byte, and the message carries a version byte, a
record count and a column count. Spread over hundreds of records that framing is
noise.

Because bit 0 is the discriminator, **every standard version byte is even**:
`0x02` for the binary form and `0x04` for JSON mode, or `0x06` and `0x08` for the
same two written with [`SetOmitEmpty`](#sparse-records-setomitempty) on.

### Compact mode

At one to three records there is nothing to spread that framing over — a
one-record message pays a field id *plus* a type byte per column to describe a
single value each. So compact mode drops the version byte, the record count, the
column count and the type bytes, and writes a five-bit header followed by one
LSB-first bitstream of `[field_id][value]` runs. A field holding its zero value
is omitted entirely.

Field ids are 8 bits, or **4 bits** when every id in the type is 14 or below,
which is what explicit `cb:"1"`-style tags give you. The header says which, so
both widths decode without the reader having to guess.

Arrays of primitives still delegate to the same `varint` and `packed5` codecs, so
an array field costs the same in either mode.

Layout in [Wire format](#compact-mode-1); the codec in
[`compact/README.md`](compact/README.md).

### How the mode is chosen

Compact mode is used when the record count is 1 to 3 **and** every field is a
scalar, string, `[]byte`, or an array of those. Nested structs, arrays of structs,
pointers, maps, `any`, and `[]int`/`[]uint` elements fall back to standard mode —
chiefly because a struct holding 100 sub-structs carries 100 records' worth of
columnar data, so the premise fails. The test is structural and memoised per type;
a slice's *length* does not enter it.

At one record compact always wins and is taken directly. At two or three the
answer turns on how many fields are zero and on the key width, so both are built
and the smaller kept. `MarshalJSON` is always standard mode — a compact message
has no room for a schema section.

The same 5-field record, hashed ids against explicit `cb:"1"`..`cb:"5"` ones:

| | standard | compact | compact, numbered | `encoding/json` |
|---|---:|---:|---:|---:|
| one 5-field struct | 34 B | 25 B | **22 B** | 76 B |
| same, three fields zero | 28 B | 13 B | **11 B** | — |
| 2 records | 50 B | 50 B | **44 B** | 154 B |
| 3 records | 62 B | 65 B | **57 B** | 220 B |
| struct with a 1000-element `[]int32` | 1017 B | 1010 B | **1008 B** | — |

## JSON mode

`Marshal` writes nothing about field *names*, and only three bits about a field's
type, because both sides derive the rest from the Go type. `MarshalJSON` is the
mode for readers that have no such type: it prefixes a **schema section** — the
field names plus the type facts the columns leave out — and the reader turns the
message into JSON on its own.

```go
data, err := colbin.MarshalJSON(rows)   // []Product -> []byte (schema + payload)

text, err := colbin.DecodeJSON(data)    // -> [{"id":1,"name":"Tin Light",...},...]
value, err := colbin.DecodeAny(data)    // -> []any of map[string]any
```

The payload under the schema is **byte for byte what `Marshal` writes**, so the
binary mode is unchanged in size and speed, and `Unmarshal` accepts either form
(with a Go type in hand it steps over the schema). The schema is built once per
type and cached, so marshalling copies it rather than deriving it.

JSON keys come from the `json` tag, else the `cb` name, else the Go field name —
one struct can serve `encoding/json` and this mode at once. A `json` tag never
feeds the field-id hash, so adding one leaves the payload untouched.

| Go value | `DecodeJSON` output |
|---|---|
| `[]Struct` | array of objects |
| a single `Struct` | one object |
| map, scalar, `[]*T`, `[][]T` (value mode) | that value |
| non-string map key | stringified, as `encoding/json` does |
| `[]byte` | base64 string, as `encoding/json` does |
| `NaN`, `±Inf` | `null` (JSON has no other form); `DecodeAny` keeps the float |
| empty or nil slice/map | `null` (the format does not distinguish them) |

Two `encoding/json` behaviours it cannot reproduce: `omitempty` (a column is
dense — every record carries a value for every field) and `json:"-"` (the field
is in the payload either way, so it still needs a key; use `cb:"-"` to leave it
out of the payload entirely).

### Schema size

The section is a per-message constant — it describes the *type*, so it does not
grow with the record count. Over the 21 comparison models:

| model | payload, 10 recs | payload, 200 recs | schema |
|---|---:|---:|---:|
| `MetricPoints` | 195 B | 3.4 KB | 44 B |
| `Products` | 684 B | 13.4 KB | 68 B |
| `People` | 1.1 KB | 22.6 KB | 118 B |
| `Invoices` (deepest) | 4.9 KB | 96.2 KB | 362 B |

So it is 10–27% of a 10-record message and well under 1% of a 200-record one.
Run the report with:

```sh
go test ./comparison -run TestJSONModeSchemaOverhead -v -count=1
```

### Schema wire format

```
message   := [version=0x04] [schemaLen:uvarint] schema body

schema    := [flags:1] [structCount:uvarint] structDef{structCount} rootDesc
structDef := [fieldCount:1] ( [field_id:1] packed5(json_name) desc )*
desc      := [desc_flags:1] extra
```

`flags` bit 0 marks records mode (`body` is the usual
`[recordCount:uvarint] subTable`); bit 1 marks a lone struct, which renders as an
object rather than a one-element array. `desc_flags` holds the `field_type` in
its low three bits plus two bits for the properties the columnar layout depends
on but never writes: `nullable` (a pointer column starts with `nullFlags`, with
no type byte of its own) and `cyclic` (an empty column of a self-referential type
is elided). `extra` depends on the type:

| type | extra |
|---|---|
| int, float | `[scalar_kind:1]` — width *and* signedness; a `uint64` and a `bool` are both int columns, and the width decides where the varint frame ends |
| string, bytes, any | — |
| array | the element `desc` |
| struct | `[structIndex:uvarint]` into the struct table |
| map | the key `desc`, then the value `desc` |

Struct types are hoisted into an indexed table instead of being inlined so a
self-referential type describes itself in finite space: the index is reserved
before its fields are walked, so a back-edge resolves to an index already
assigned.

## Field ids and the `cb` tag

Every field is identified on the wire by a single `uint8`. By default it is derived
from a **FNV-1a-32 hash of the field name, xor-folded to 8 bits**, with collisions
resolved by linear probing (id, id+1, …, wrapping). Id `255` is reserved as a
terminator, so a struct may have at most **254 encodable fields**.

The `cb` struct tag controls this. Its value is a comma-separated list; an **integer
token sets the id explicitly** (no hashing), and the first **non-integer token
overrides the name** used for hashing:

| Tag | Effect |
|---|---|
| `cb:"name"` | hash `name` instead of the Go field name |
| `cb:"5"` | fixed id `5`, no hashing |
| `cb:"id,5"` | name `id`, fixed id `5` |
| `cb:"-"` | skip the field entirely |
| *(no tag)* | hash the Go field name |

Explicit ids are reserved first, then hashed fields probe around them. Two fields
with the same explicit id, or an id above 254, are rejected. Because ids come from
the type on both sides, encoder and decoder derive the same mapping without
transmitting field names.

**Numbering a struct `cb:"1"`, `cb:"2"`, … is worth doing.** If every id lands at
14 or below, compact mode writes its keys as 4 bits instead of 8 and each record
sheds half its framing — a seven-field struct goes from 21 B to 17 B, and compact
mode starts winning at two and three records where it used to lose. A tag that
only renames a field leaves its id to the hash, which lands anywhere in 0..254
and keeps the whole type on the wide key. It changes nothing in standard mode.

## Supported types

| Go type | Encoding |
|---|---|
| `int8..int64`, `uint8..uint64`, `int`, `uint`, `bool` | adaptive varint column |
| `float32`, `float64` | raw IEEE-754 (32/64-bit) |
| `string` | self-delimiting packed5 frame (raw fallback when packing would grow it) |
| `[]byte` | varint length column + concatenated bytes |
| `[]T` (T scalar or struct) | varint length column + flattened element column |
| nested `struct` | recursive sub-table of columns |
| `[][]T`, deeper nesting | recursion |
| `*T` (incl. `*struct`, `[]*T`) | nullable column (null bitmap, see below) |
| `map[K]V` (K scalar/string, V any) | length column + flattened keys + values |
| `interface{}` / `any` (incl. `map[string]any`, `[]any`) | self-describing tagged values (see below) |

**Not yet supported** (error on encode): `time.Time`, pointer-to-pointer
(`**T`), non-scalar map keys, a struct/`chan`/`func` held inside an `interface{}`.
`nil` and empty slices/maps are indistinguishable — both decode to `nil`.

### Nullability

Any pointer type is nullable and distinguishes all three states — `nil`, a pointer
to the zero value (`&0`, `&""`), and a pointer to any other value. A nullable column
writes a 1-byte `nullFlags`; if it has any nulls, a `ceil(N/8)` presence bitmap
follows (1 = present) and only the non-null values are stored densely. A column with
no nulls costs just the 1 flag byte. This is how `[]*int32` stores a null *slot*
(distinct from `0`) and how `map[K]*V` stores null values.

## How the integer encoding works

Fixed-width Go integers are converted to the signed type with the same width and
passed to `varint.AppendArray`; `int` and `uint` use the stable 64-bit wire path.
The codec evaluates raw, delta-of-previous, frame-of-reference, and fixed-width
representations, then writes the smallest plan. Its one-byte header records the
selected transform, zigzag mode, and variable-length parameters;
`varint.DecodeArray` returns the exact payload span consumed. Same-width conversion
preserves unsigned bit patterns—including the full `uint64` range—while keeping
fixed-width fallbacks at 1, 2, 4, or 8 bytes.

See [`varint/README.md`](varint/README.md) for the codec and wire layout.

## Wire format

Both formats start at bit 0 of byte 0; see [Modes](#modes) for the
discrimination rule.

### Standard mode

```
message := [version=0x02:1] [recordCount:uvarint] subTable   // 0x06 under omit-empty

subTable := [colCount:1] column*                 // one column per field

column   := [field_id:1] [type_flags:1] payload
```

`type_flags` holds the `field_type` in its low three bits. **Bit 7 says the
column is empty**: every value in it is the zero value and *no payload follows*.
Float columns set it whenever they are all zero; the integer and string columns
set it only under [`SetOmitEmpty`](#sparse-records-setomitempty), which is what
makes a column of a thousand unset values cost one byte instead of a thousand.
Bytes, array and map columns inherit it through the length column that frames
them.

Payloads by `field_type`:

- **int**: one adaptive `varint` array frame; N comes from the containing layout.
- **float**: `N × (32|64) raw IEEE-754 bits` (empty column: no payload).
- **string**: N consecutive self-delimiting `packed5` frames. Each frame may use
  packed tokens or raw bytes, whichever is smaller.
- **bytes**: a varint length column, then concatenated raw bytes.
- **array**: a varint length column (element count per record), then one
  flattened element column (recursively `[flags] payload`).
- **struct**: a nested `subTable`.
- **map**: a varint length column (entry count per record), then a flattened
  keys column and a flattened values column.
- **nullable** (pointer types): `[nullFlags:1] [presence bitmap IF has_nulls]` in
  front of the (dense) inner column.

Recursive types (`type Node struct{ Kids []Node }`, including cycles spanning
several types, or closed through a pointer or map value) are supported. Because
the layout is schema-driven, an *empty* array/map/nullable column would still
write the nested columns of its element type, which never terminates when that
type refers back to itself. So for element types that can reach themselves, and
only those, a column holding zero values is omitted entirely. Both sides agree
without an extra marker: the count comes from the length/presence sub-column that
precedes it, and "can reach itself" is a static property of the Go type. Types
that are not self-referential are unaffected, byte for byte.
- **any** (`interface{}`): `N` self-describing tagged values. Unlike every other
  column, `any` values can't be columnarized (concrete type is unknown at build
  time and varies per value), so each is written row-style as `[tag:1] payload` —
  a compact escape hatch inside the columnar frame. `nil`/`bool`/`int64`/`uint64`/
  `float64`/`string`/`[]byte`/`[]any`/`map[string]any` (recursive). Decode
  normalizes numbers to `int64`/`uint64`/`float64`.

The integer decoder reports the consumed payload span and each packed5 string is
self-delimiting, so the decoder can advance directly to the next column or frame.

### Compact mode

```
message := [header:5 bits] record{n}                    // then padded to a byte

header  := mode(bit 0 = 1) | ALL_POSITIVE(bit 1) | shape(bits 2-3)
                           | NARROW_KEYS(bit 4)

record  := ( [field_id:k] value )* [terminator:k]       // k = 8, or 4 if narrow
```

| bit | field | meaning |
|---|---|---|
| 0 | mode | always `1` |
| 1 | `ALL_POSITIVE` | every signed integer in the message is `>= 0`, so its payload is the magnitude rather than the zigzag — one bit saved on each |
| 2-3 | shape | `0` lone struct · `1`/`2`/`3` array of that many records |
| 4 | `NARROW_KEYS` | field ids are 4 bits (ids `0..14`, `15` closes a record) rather than 8 (ids `0..254`, `255` closes a record) |

Shape `0` and `1` both carry one record and differ only in whether it renders as
an object or an array of one, which the binary path takes from the destination Go
type but a JSON reader would need told. Id `255` closing a wide record is the
same id the standard mode reserves as a terminator; `15` is that same reservation
one nibble down.

`NARROW_KEYS` is set when every id the message writes is 14 or below, which in
practice means a struct whose `cb` tags number its fields. It saves 4 bits on
every key and on every terminator, against the one bit it costs the message, so it
is ahead from the first field onwards. It is in the header rather than derived
from the type on both sides so that two ends holding different versions of a
struct fail as a mismatched id rather than as a misparse.

Everything after the five header bits is one LSB-first bitstream; nothing is byte
aligned until the final pad, which is never read back because a record ends at
its terminator and the message ends at its last record.

| value | encoding |
|---|---|
| int | varint over the magnitude or the zigzag, per `ALL_POSITIVE` |
| uint | varint over the value; never zigzagged |
| bool | **1 bit** |
| float32/64 | raw IEEE-754, 32 or 64 bits |
| string | one `packed5` frame, self-delimiting, no length alongside it |
| bytes | varint length, then the bytes |
| array of int / string / float | element count, then the same codec the columnar mode uses |
| array of bool | element count, then a bitmap, one bit each |

The compact varint spends unit 0's seventh payload bit on a selector:

```
unit 0    [selector:1] [cont:1] [payload:6]   (+ [payload:4] if selector)
unit i    [cont:1] [payload:7]
```

`cont` says another unit follows; the nibble is a one-time offset on unit 0, not
a per-unit addition. That gives two capacity ladders, `6+7k` and `10+7k`, and the
encoder takes whichever reaches the value first — three ties, three wins of four
bits and one loss of four per seven bit-lengths, the loss falling only where
LEB128's first byte was already exactly full.

## Performance

Four design choices keep it fast:

- **Field access via `github.com/viant/xunsafe`** — cached, typed, unsafe struct
  field get/set on the hot numeric path (array elements use direct pointer casts).
  Type layout (`typeInfo`, field ids, accessors) is built once per type and cached.
- **Compact mode runs a cached per-type plan** (`codec/compact_plan.go`): one
  opcode per field, holding the field's offset and wire id, so a record is a flat
  switch over a slice rather than three nested ones on class, kind and width. The
  plan also fuses the passes — a field used to be read once to test it for zero,
  once for the `ALL_POSITIVE` scan and once to write it — and its id-to-field
  table is an array, which took the map lookup per key out of the decoder.
- **The codecs append directly to the output buffer** and integer/byte columns
  reuse scratch slices from `sync.Pool`.
- **The output buffer is sized from what the type last measured.** Each type
  remembers its encoded bytes per record, so the buffer is allocated about the
  right size instead of growing into place — a payload that starts from a
  field-count guess is copied again at every regrow. Against the field-count
  estimate alone this cut encode allocations from 8 to 3 and bytes allocated by
  69% on the 1000-record scalar batch, by 38% on the nested one, and by 62% on
  the comparison corpus — 14%, 14% and 26% less time respectively. The estimate
  is capped at 1 MiB, since records of one type can vary in size without bound;
  past that the buffer grows the ordinary way.

### Benchmarks

Medians of a `-count=6` run on an i7-1355U with Go 1.27:

| 1000 records | payload | encode | decode |
|---|---:|---:|---:|
| scalar | 26.8 KB | 158 µs | 97 µs |
| nested | 43.5 KB | 499 µs | 541 µs |

One record per message, 10 000 of them, on a seven-field numbered struct — the
case [`Codec[T]`](#many-small-messages-codect) exists for:

| | encode | decode |
|---|---:|---:|
| `Marshal` / `Unmarshal` | 2.2 ms | 1.7 ms |
| `Codec`, buffer reused | **1.24 ms**, 0 allocs | **1.48 ms**, 0 allocs |

Each benchmark ran in its own process from a cold start (package under 62 °C).
This laptop throttles hard enough that one sequential `go test -bench .` charges
the later benchmarks 6–11% for the heat the earlier ones produced, which reads
as a code difference and is not one.

JSON mode on the same fixtures: `MarshalJSON` costs what `Marshal` does (1.02x —
the schema is a cached copy), while reading without a Go type costs 3.6x the typed
decode for `DecodeAny` and 13x for `DecodeJSON` on the scalar batch (6.5x on the
nested one, whose typed decode is already map- and slice-heavy). Both build a map
per record instead of filling a struct, and `DecodeJSON` then renders text.

These numbers are a development snapshot, not a cross-format comparison. Re-run
on the target workload before making a storage or latency decision.

Run them:

```sh
go test ./codec -bench . -benchmem
```

### Cross-format comparison

The [`comparison/`](comparison/) package contains a seeded `gofakeit` generator,
21 varied models, checked-in Protobuf bindings, round-trip tests, per-model
payload-size reporting, and paired encode/decode benchmarks for Colbin, Protobuf,
Go's `encoding/json/v2`, and `fxamacker/cbor`. Every serializer receives the same
generated `BenchmarkCorpus`, and fixture generation is excluded from benchmark
timing.

```sh
go test ./comparison -run TestPayloadSizeComparison -v -count=1
go test ./comparison -run '^$' -bench . -benchmem -count=5
```

See [`comparison/README.md`](comparison/README.md) for the model coverage,
current snapshot, and Protobuf regeneration instructions.

## In the browser

[`web/`](web/) holds an AssemblyScript port of the codec, compiled to
WebAssembly, and a page that runs it: paste JSON, and it derives a schema,
encodes it, and shows which column cost what.

It takes JSON with **no schema attached** — no Go type, no `.proto` — derives one
from the data, and enforces it: a field whose type changes between records is
refused with the record and the field named, rather than widened or dropped into
an `any` column. The output is JSON mode, so `DecodeJSON` above reads it.

```
web/assembly/   the port: varint, packed5, the columnar body, the schema section
web/vectors/    Go: the golden frames and the reflect.StructOf oracle
web/src/        the page
```

The port is checked against this package at three levels, not against itself:
every `varint` and `packed5` frame byte for byte, every whole message against
`MarshalJSON`, and a cross round trip through `DecodeJSON`. Run it with
`bun run test` in `web/`.

Two differences from the Go implementation are deliberate. The decoder
bounds-checks every count and offset rather than trusting them, because
AssemblyScript has neither the unconditional bounds check nor the `recover` that
turns a bad index here into an error. And decoding preserves the key order the
message carries, where `DecodeJSON` sorts alphabetically — so encode then decode
returns the caller's own key order.

Not yet deployed; see [`web/PLAN.md`](web/PLAN.md) for the design and what is
still open.

## In Rust

[`rust/`](rust/) holds a Rust **decoder**, published as a Cargo crate out of this
repository. A Cargo workspace and a Go module share one tree without either
noticing the other: Cargo clones the repo, reads the root manifest and finds the
member named `colbin`.

```toml
# Pinned by rev, not tag: the tags here are the Go module's version ladder, so a
# tag requirement would either entangle the two release cadences or resolve to a
# tag that predates the crate.
colbin = { git = "https://github.com/ivanjoz/colbin", rev = "<sha>" }
```

```rust
use colbin::{Kind, Schema};

// The caller supplies the layout, because neither binary mode is
// self-describing — the same contract the Go and AssemblyScript ports have.
let schema = Schema::from_go(&[
    ("CompanyID", Kind::Int32),
    ("ID", Kind::Int32),
    ("Hash", Kind::Uint64),
    ("User", Kind::String),
])?;
let record = colbin::decode_one(message, &schema)?;
let user = record.str(schema.fields()[3].id);
```

It reads both wire modes and the value kinds compact mode itself carries:
scalars, strings, byte blobs and slices of primitives, with hashed and `cb:"N"`
field ids. What it does not read is a nested struct, a map, an `interface{}`
column or a `MarshalJSON` message — those are standard-mode-only shapes, and it
rejects them rather than half-decoding one. There is no encoder: nothing in Rust
writes colbin, and an unused encoder is a second specification to keep honest for
free.

```
rust/src/       the decoder: bitstream, varint, packed5, compact, standard
rust/vectors/   Go: the corpus generator, and the vectors it commits
rust/tests/     the corpus test
```

Nothing in the corpus is written by hand. Every message comes from
`colbin.Marshal` on a real Go value; every field id is read out of a schema
section colbin wrote; every expected value is read back by the Go decoder — the
compact ones through a `compact.Reader`, the columnar ones through `Unmarshal` —
and the generator refuses to write a corpus that misses a mode, a shape, a key
width, a version byte or a value kind. The vectors are committed, so `cargo test`
needs no Go toolchain; CI regenerates them and fails on a diff, which is what
catches a Go-side format change nobody ported.

## Files

| file | role |
|---|---|
| `colbin.go` | small public `Marshal` / `Unmarshal` / `MarshalJSON` facade |
| `doc.go` | public package documentation |
| `codec/` | serialization engine, schema metadata, pools, and white-box tests |
| `codec/schema.go` | JSON mode: the schema section, built once per type and cached |
| `codec/schema_decode.go` | JSON mode: schema-driven decode to Go values or JSON |
| `codec/compact_mode.go` | compact mode: eligibility, mode selection, and the Go-type bridge |
| `codec/compact_plan.go` | the per-type compact plan: one resolved op per field, built once |
| `codec/omitempty.go` | the omit-empty flag, the empty-column bit and the version it selects |
| `codec/typed.go` | `Codec[T]`, the cached typed handle over the same format |
| `compact/` | compact mode wire format: bitstream, selector varint, reader/writer |
| `varint/` | adaptive integer-array codec used by integer and length columns |
| `packed5/` | self-delimiting string codec used by every string path |
| `comparison/` | 21-model Colbin, Protobuf, JSON v2, and CBOR comparison corpus |
| `web/` | the AssemblyScript port, its vectors, and the browser playground |
| `rust/` | the Rust decoder, its Go-generated vectors, and the corpus test |
| `colbin_test.go` | public API integration test |

## Limitations

- The plain binary mode is not self-describing: encoder and decoder must share a
  compatible Go type. Use JSON mode when the reader has no such type.
- Compact mode carries no schema, so `MarshalJSON` always uses standard mode. A
  compact message also rejects an unknown field id rather than skipping it: the
  wire holds no type tag, so there is no way to know how far to step.
- Not a streaming format — all records are buffered before output.
- `SetOmitEmpty` is global and encoder-side. Set it once at startup: flipping it
  mid-flight is safe (it is atomic, and the caches it invalidates rebuild on
  demand) but leaves two encodings of the same value in flight. The browser port
  **decodes** omit-empty messages; its encoder still writes dense columns, so the
  `omitempty` vector tier is skipped in the port's encoder tests.
- The Rust port is a decoder only, and carries only the value kinds compact mode
  admits: scalars, strings, byte blobs and slices of primitives. A nested struct,
  a map, an `interface{}` column or a `MarshalJSON` message is refused with an
  error naming why. It also rejects a string that is not valid UTF-8, which this
  package permits — `packed5` is byte exact, and a Rust `String` cannot be.
- Trusts the input buffer on decode (internal use); malformed data can panic on
  slice bounds rather than returning an error. `DecodeJSON` and `DecodeAny` do
  turn that panic into an error, but still trust the counts they read, so a
  corrupt message can ask for a large allocation.
- Self-referential *values* (a pointer graph that loops back on itself) are not
  detected and will recurse until the stack runs out. Self-referential *types* are
  fine — see below.
- No backwards-compatibility guarantees — the format version byte is bumped on any
  wire change (this project is pre-alpha). Bumped version bytes must stay **even**,
  since bit 0 discriminates compact mode. Compact mode has no version byte to bump,
  so a change to its header — the `NARROW_KEYS` bit was the last one — is not
  detectable in an already-encoded message; both ends must be rebuilt together.
