# Self-describing colbin: a schema section, and JSON out of it

A plan for bringing back what `codec/schema.go` and `codec/schema_decode.go` did
before the collapse to one format — a message a reader can decode **without the
Go type** — rebuilt on the current descriptors rather than ported.

Status: proposed. Nothing below is implemented.

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

Four detail bits, two allocated today:

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

### 6.3 Maps

- **Key order is not stable** (Go map iteration). JSON objects are unordered, so
  this is *acceptable* here — but it means a schema-described message with a map
  does not round-trip to identical JSON bytes, which rules maps out of any
  golden-vector test. Already true today; must be written down.
- **Integer keys.** JSON object keys are strings. `map[int64]string` must render
  keys as `"42"`. Decoding back the other way is out of scope.

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

---

## 8. Phases

Each phase ends green and is independently useful.

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

- **Writing** colbin from JSON (`parseJSON` in `web/assembly/json.ts` did this).
  A separate job, and harder: it needs type inference.
- **Re-porting `web/assembly/`.** It targets the old version-byte format and is
  stale in the same way the Rust port is. This plan is Go-only; the TypeScript
  follows once the section is settled.
- `interface{}` fields. They have no wire form yet, with or without a schema.

---

## 10. Cost, stated honestly

- **`fieldOp` becomes format.** Twenty-four constants that were free to reorder
  are now pinned by a wire contract.
- **A second decoder family**, roughly the size of `codec/`'s read path, to
  maintain alongside it.
- **~215 bytes** of schema for a `Sale`, if sent inline.
- No cost at all to the existing encode/decode paths: nothing above changes a
  byte of what `Marshal` writes today.
