# Self-describing colbin: a schema section, and JSON out of it

A plan for bringing back what `codec/schema.go` and `codec/schema_decode.go` did
before the collapse to one format — a message a reader can decode **without the
Go type** — rebuilt on the current descriptors rather than ported.

Status: **implemented**, in `codec/schema.go`, `codec/schema_plan.go`,
`codec/json.go`, `codec/jsontext.go` and `codec/any.go`. All nine phases are in,
with tests in `codec/schema_test.go`, `codec/json_test.go` and
`corpus/json_test.go`.

Four things came out differently from the proposal below, and each is marked
**[built]** where it belongs:

1. **A structDef carries its key width.** §3.3 is wrong: a *narrow* list's
   element is a length and a body with no descriptor between them, so its key
   width is the one thing the wire does not say. The section says it, for every
   struct rather than only that one.
2. **`mapKind` splits the two float widths.** A map value's float width came
   from the destination Go field, which a schema-only reader does not have.
3. **The names.** `Schema` is the parsed type, so the constructors are
   `SchemaFor[T]()`, `SchemaOf(v)` and `ParseSchema(section)`; `Schema.Bytes()`
   is what §7 called `Schema[T]() []byte`. A nil schema means "the message
   carries its own".
4. **The measurements.** A `Sale` section is 173 bytes, not the ~215 estimated
   in §4 — 1.9x a mean sale body rather than 2.4x. `go test ./corpus -run
   ReportSchema -v` prints the table.

---

## 1. The problem, stated precisely

colbin's whole speed argument is that **the type is not on the wire**. A field is
a key and a payload; what that payload *means* — signed or unsigned, float or
integer, string or blob, which Go field it belongs to — comes from the schema
both sides already have.

That is exactly what a browser, a `jq`-style tool, a log inspector or a
dynamically-typed client does not have.

Two different capabilities are tangled together here, and the old code did not
separate them:

| | needs | K4 | K8 |
|---|---|---|---|
| **skip** an unknown field | a byte length or a derivable one | ✗ | ✓ |
| **name and type** a known field | the schema | ✗ | ✗ |

`Skip` already works under K8 — that is what the wide descriptor buys. But
skipping is not understanding: a K8 `INT` descriptor says "an integer of *n*
bytes", not "the `TaxCents` field, an int64". **No amount of descriptor gets you
to JSON. Only a schema does.**

So the feature is: ship the schema, and give the decoder a walk that consumes it
in place of `reflect`.

---

## 2. The key insight

`codec.typePlan` **is** the schema. It is what `reflect` is boiled down to
before any encoding happens:

```go
type planField struct {
    key                uint8
    offset             uintptr     // ← the only reflect-specific part
    op                 fieldOp
    sub                *typePlan   // nested struct
    sliceType          reflect.Type
    stride             uintptr
    elemOp             fieldOp     // pointer pointee
    keyKind, valueKind mapKind     // maps
}
```

Strip `offset`, `sliceType` and `stride` — the three things that exist only to
write into a Go struct — and what remains is precisely what a JSON decoder
needs: **a key, a name, and an op**, plus a child plan for composites.

That reframes the work. This is not a new format and not a second decoder
family. It is:

1. **serialise `typePlan`** to bytes (encoder side, once per type, cached);
2. **parse it back** into the same shape (decoder side);
3. **a second set of walkers** that take a plan and emit JSON instead of
   filling a struct at `unsafe` offsets.

Step 3 is the bulk of the work and is unavoidable — the existing walkers write
through pointers into a known layout, and there is no layout here.

---

## 3. Wire layout

### 3.1 Root bytes

The root byte is an ordinary K8 descriptor whose class is STRUCT — class 5, so
`1 101 dddd` — which is why every colbin message begins in **0xD0..0xDF**. That
range is now reserved and documented in `codec/root.go`; the other 240 values
are guaranteed never to be written, and belong to the application.

Four detail bits, two allocated today (**[built]** and now written, so four of
the sixteen root bytes are assigned and twelve are still refused):

```
0x08  wide     eight-bit keys inside
0x04  schema   (this plan)
0x02  —        unallocated
0x01  —        unallocated
```

Taking `0x04` for *a schema section precedes the body*:

```
0xD4  schema section, then a narrow-key struct
0xDC  schema section, then a wide-key struct
```

So a self-describing message is identified by its **first byte**, with no magic
prefix and no length probe: `data[0] & 0x04 != 0`. `colbin.IsColbin` already
separates colbin from an application's own framing.

`rootOf` gains two cases and `Unmarshal` learns to step over a schema section it
does not need — so **a schema-carrying message still decodes into the Go type**,
which is what made the old `MarshalJSON` output acceptable to `Unmarshal`. That
property is worth keeping.

### 3.2 The section

```
section   := [byteLength]        the whole section, so a typed reader can skip it
             [structCount]
             structDef{structCount}     struct 0 is the root
structDef := [fieldCount] field{fieldCount}
field     := [key:1] [nameLen:1] name [desc]
desc      := [op:1] extra
```

**[built]** with one byte more per struct and one length rule throughout:

```
structDef := [flags:1] [fieldCount] field{fieldCount}     flags bit 0: wide keys
field     := [key:1] [nameLen] name [desc]                nameLen as every length
```

`extra` by op:

| op | extra |
|---|---|
| scalars, string, bytes | — |
| integer/float arrays | — (the op names the element type) |
| `opStrings` | — |
| `opStruct`, `opStructs` | `[structIndex]` |
| `opMap` | `[keyKind:1] [valueKind:1]` |
| `opPointer` | `[elemOp:1]` |

Lengths are colbin's own `Uint` framing, not varints — the format's own rule.

**Struct hoisting is kept from the old design and for the old reason:** a
self-referential type (`type Node struct{ Kids []Node }`) must describe itself in
finite space. The index is reserved before the fields are walked, so a back-edge
resolves to an already-assigned index. This is the same trick `planForBuilding`
plays with its `building` map.

**`op` goes on the wire as a number.** That makes the `fieldOp` constants part of
the format, not an implementation detail — they must stop being reordered
casually, and the values need pinning in a test. This is a real cost of the
feature and should be stated in the docs.

### 3.3 What the schema does *not* carry

The key width. Each run's own descriptor already says it — `rootOf` for the
root, the `k8` bit for a nested struct, the table's own bit for its columns. The
decoder reads it from the wire, as the Go decoder does.

**[built] This is wrong, and it is the one hole the implementation found.** A
narrow list's element is `[len][body]` with no descriptor between them — that
missing byte per element is exactly what makes a narrow list of small structs
smaller than a wide one — so nothing on the wire says what width the run inside
it uses. `structDef` therefore begins with a flags byte whose bit 0 is "eight-bit
keys", stated for every struct rather than only for that case, because one rule
is cheaper to hold than an exception. It is also why `SetPacked5` now drops the
schema cache: packed5 is one of the two things that decide a type's width.

---

## 4. Delivery: inline is the wrong default

The old `MarshalJSON` prefixed the schema onto **every message**. For the corpus
`Sale` type that is roughly:

```
Sale:     9 fields × (1 key + 1 len + ~11 name + 1..2 desc)  ≈ 130 B
SaleLine: 6 fields × ~14                                     ≈  85 B
                                                        total ≈ 215 B
```

against a sale body of **~90 bytes**. The schema is **2.4× the message**.

That is fine for a one-off document and absurd for a stream. So:

| mode | API | use |
|---|---|---|
| **out-of-band** | `Schema[T]() []byte` | send once per connection, then ordinary messages |
| **inline** | `MarshalSelfDescribing(v)` | a single document that must stand alone |

Out-of-band is the one that should be documented first and used by the web
client. The decoder takes a parsed schema plus a plain `0xD0`/`0xD8` message —
no new root byte needed on that path at all.

---

## 5. Decoder architecture

```
wireSchema  ─parse─→  *typePlan (offset/stride zeroed)
                          │
                          ├─ narrow body → jsonNarrowRun()  ─┐
                          └─ wide body   → jsonWideRun()    ─┼→ JSON bytes
                                                             │
                                       (or → any: map[string]any, []any, …)
```

Two output targets, one walk each:

- `DecodeAny(schema, data) (any, error)` — `map[string]any`, `[]any`, `int64`,
  `string`, … the old surface.
- `AppendJSON(dst, schema, data) ([]byte, error)` — straight to JSON text, no
  intermediate map. This is the one worth having: the intermediate `map[string]any`
  is where all the allocation is, and a browser wants text anyway.

`DecodeAny` should be built *on* `AppendJSON`'s walk, not beside it, or the two
will drift — that is how `schema.go` and `schema_decode.go` ended up with
parallel switches.

---

## 6. The hard parts

These are the reasons this is not a port. Each needs a decision.

### 6.1 `[]Struct` is two different shapes

The encoder picks LIST or TABLE per value at `tableThreshold = 8`, and the
reader dispatches on what it finds (`IsTable()`). The JSON decoder must do the
same, and **the table path is the harder one**: it must read each column through
the column codec, then emit row-wise JSON — a transpose in the decoder, which
the Go decoder gets for free by scattering into a slice.

For a 4 000-row table this means either materialising every column first
(memory) or making *n* passes (time). Recommendation: materialise, and document
the peak. It is the same buffer `scratch` already holds.

### 6.2 Floats

Floats travel as **byte-reversed IEEE-754 bit patterns** inside the integer
field, so the schema must say "this is a float" and the decoder must call
`ReverseBytes64`. Reading a float field as an integer silently yields garbage
rather than failing — so this is a correctness trap and needs a dedicated test.

Then: **JSON number formatting.** `strconv.AppendFloat(f, 'g', -1, 64)` is the
round-trip-exact form. NaN and ±Inf have no JSON representation; the format has
`SPECIAL` codes reserved for them and the JSON encoder must decide — `null`, or
refuse. Recommend refuse, loudly, because silently turning NaN into null loses
data.

**[built] Refused**, and refused *before* anything is written, so a caller's
buffer is either the whole document or exactly what it was. The number format is
not `'g'` but `encoding/json`'s own rule — `'f'` until the exponent leaves
±(1e-6, 1e21), then `'e'` with the leading zero trimmed off the exponent — so the
two write the same bytes and a test can compare them directly rather than through
a JSON parser. A `float32` is formatted at 32 bits, so `1.1` prints as `1.1`.

### 6.3 Maps

- **Key order is not stable** (Go map iteration). JSON objects are unordered, so
  this is *acceptable* here — but it means a schema-described message with a map
  does not round-trip to identical JSON bytes, which rules maps out of any
  golden-vector test. Already true today; must be written down.
- **Integer keys.** JSON object keys are strings. `map[int64]string` must render
  keys as `"42"`. Decoding back the other way is out of scope.
- **[built] Float values needed a width.** `mapKind` collapsed `float32` and
  `float64` into one code, because the Go decoder takes the width from the
  destination field. A schema-only reader has no destination, and a 32-bit
  reversed bit pattern read as a 64-bit one is a plausible-looking number rather
  than an error. `mapFloat32` and `mapFloat64` are now separate kinds, and the
  kinds are pinned by a test alongside the ops.

### 6.4 Pointers

`nil` → `null`. A non-nil pointer to a zero value writes an explicit zero on the
wire, so it must render as `0` / `""` / `false`, **not** `null`. The schema's
`opPointer` + `elemOp` carries enough to do this; the test is the one from
`pointer_test.go`, rerun through JSON.

### 6.5 `[]byte`

Base64, matching `encoding/json`. Worth stating because colbin's `opBytes` and
`opStrings` are different ops and only one is base64.

### 6.6 packed5

`PackedString` reads either encoding from the descriptor, so the JSON decoder
gets this free **on the wide path**. Under K4 there is no `enc` field and strings
are always raw. No work, but a test each way.

### 6.7 K4 and unknown fields

A narrow body plus a schema is fully decodable — the schema plays the role the Go
type plays. But if the *message* holds a key the *schema* does not list, a narrow
reader still cannot skip it and must fail. That is K4's standing trade and the
error should say so, as `unmarshalNarrow` already does.

---

## 7. Naming

The old names were misleading and should not come back unexamined:

- `MarshalJSON(v)` returned **colbin binary**, not JSON. It also shadows the
  `json.Marshaler` method name, which is a different thing entirely.
- `DecodeJSON(data)` returned **JSON**. So the pair was asymmetric.

Proposed:

| old | proposed |
|---|---|
| `MarshalJSON(v)` | `MarshalSelfDescribing(v)` |
| — | `Schema[T]() ([]byte, error)` |
| `DecodeJSON(data)` | `AppendJSON(dst, schema, data)` / `ToJSON(schema, data)` |
| `DecodeAny(data)` | `DecodeAny(schema, data)` |

Open for the owner to overrule — the old names can be kept as aliases.

**[built]** `Schema` is the *type* — a parsed schema, however it was obtained —
so it cannot also be the constructor. What shipped:

| | |
|---|---|
| `SchemaFor[T]() (*Schema, error)` | from the Go type |
| `SchemaOf(v) (*Schema, error)` | from a value |
| `ParseSchema(section) (*Schema, error)` | from the wire |
| `Schema.Bytes() []byte` | the section, to send — §7's `Schema[T]()` |
| `MarshalSelfDescribing(v)` | as proposed |
| `AppendJSON(dst, schema, data)` / `ToJSON(schema, data)` | as proposed |
| `DecodeAny(schema, data)` | as proposed |

A **nil** schema on the three decode entries means "the message carries its own",
so the self-describing path needs no second set of names.

---

## 8. Phases

Each phase ends green and is independently useful. **[built] All nine are in**;
the tests landed in `codec/schema_test.go`, `codec/json_test.go` and
`corpus/json_test.go`, with two fuzz targets — one over the section, one over the
message — because both are untrusted input.

| # | deliverable | test |
|---|---|---|
| **1** | `fieldOp` values pinned; `schema.go`: serialise a `typePlan`, cached per type | golden bytes for the corpus types; a test that fails if a `fieldOp` constant moves |
| **2** | `schemaPlan.go`: parse a section back into a `*typePlan` | round-trip: serialise → parse → compare against the reflective plan, for every corpus type |
| **3** | `AppendJSON` for **flat** records — scalars, strings, bytes, arrays | corpus `User`, `Product`, `Metric` against `encoding/json` on the same struct |
| **4** | nested `opStruct` + `opStructs` **LIST** form | corpus `Sale` with < 8 lines |
| **5** | **TABLE** form (the transpose) | corpus `Sale` with ≥ 8 lines; same JSON as phase 4 for the same data |
| **6** | maps, pointers, floats, NaN policy | `pointer_test.go` cases through JSON; map key-order caveat documented |
| **7** | root bytes `0xD4`/`0xDC`; `MarshalSelfDescribing`; `Unmarshal` skips a section | a self-describing message decodes into the Go type *and* into JSON |
| **8** | `DecodeAny` on the phase-3..6 walk | the old `schema_test.go` cases, recovered from `HEAD^` and re-targeted |
| **9** | benchmarks and size table | JSON out vs `encoding/json`; schema size per corpus type |

**Phases 1–3 are the useful minimum.** They give a browser flat records, which
is most of a REST payload. Phases 4–5 are where the real work is.

---

## 9. Out of scope

- **Writing** colbin from JSON. A separate job, and harder: it needs type
  inference. *(Since built, in Rust rather than in Go: `rust/ENCODER.md` states
  the rules and `rust/src/{json,infer,build,verify}.rs` implement them. Go still
  has no JSON encoder.)*
- **The browser module.** It targets the old version-byte format and is stale in
  the same way the Rust port is. This plan is Go-only; the port follows once the
  section is settled. *(Since done twice: re-ported onto this format, then
  replaced by `rust/wasm` — `RUST_WASM_PLAN.md`.)*
- `interface{}` fields. They have no wire form yet, with or without a schema.

---

## 10. Cost, stated honestly

- **`fieldOp` becomes format.** Twenty-four constants that were free to reorder
  are now pinned by a wire contract.
- **A second decoder family**, roughly the size of `codec/`'s read path, to
  maintain alongside it.
- **~215 bytes** of schema for a `Sale`, if sent inline.
- No cost at all to the existing encode/decode paths: nothing above changes a
  byte of what `Marshal` writes today. **[built] Measured**: eleven benchmarks
  over encode, decode, nesting, tables and maps, before and against after, are
  unchanged or marginally faster, and identical to the byte in allocation. The
  field names a section needs live in a slice beside `typePlan.fields` rather
  than inside `planField`, so the decode path does not drag sixteen bytes per
  field through the cache for a name it never reads.
