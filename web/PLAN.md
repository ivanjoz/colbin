# colbin for AssemblyScript — plan

A colbin **marshaller and unmarshaller compiled to WebAssembly from
AssemblyScript**, which takes JSON with no schema attached, derives one, enforces
it, and emits a message any colbin reader can decode — including itself, and
including Go.

A static SvelteKit site on GitHub Pages is how it gets exercised and shown: paste
JSON, see the message, see which column cost what, download it, upload one back.

Status: **plan. Nothing built.** Measurements in the appendix are real, taken on
this machine against the Go implementation in this repository.

---

## 1. The thing being built

```
JSON (no schema)  ──▶  infer  ──▶  enforce  ──▶  colbin message  ──▶  JSON
                        §3          §4            §5                  §6
```

Three properties, in priority order. Everything else in this document serves
them.

1. **Schemaless in.** The caller has JSON and nothing else — no `.proto`, no Go
   struct, no type declaration. The schema is derived from the data.
2. **Type constraints enforced.** A derived schema is a *claim about every
   record*. Records that contradict it are rejected with a location, not
   silently widened, coerced, or dropped into an `any` column.
3. **Decodable, or an error — never anything else.** `encode` is a total
   function with exactly two outcomes: a message that decodes, or a diagnostic.
   It must never return bytes that a decoder cannot read, or reads as something
   other than what went in. The output is JSON-mode colbin (`0x04`), so it
   carries its own schema section and Go's `colbin.DecodeJSON` reads it without a
   Go type — not a private format only this module understands. §4.5 is how the
   property is held.

The web page is a consumer of that module, not the other way round. If the page
were deleted the module would still be the deliverable.

### Why AssemblyScript, concretely

`varint` searches over transforms of `int64` residuals; `packed5` runs an
LSB-first bitstream with unsigned shifts. **JavaScript has no int64.** A JS port
means `BigInt` — allocating, roughly an order of magnitude slower — or hand-split
hi/lo arithmetic through every one of those paths. AssemblyScript has native
`i64`, `u64` and the shift semantics the Go code was written against, so the port
stays close to a transliteration and each file can be reviewed line-by-line
against `varint/README.md` and `packed5/README.md`.

The alternative, a Go `GOOS=js` build, was measured at **1.27 MB gzipped**
(appendix A.4) against a target under 50 KB, and would not have produced a
library anyone could use from JavaScript.

---

## 2. The module

### 2.1 ABI — UTF-8 bytes in, UTF-8 bytes out

```
encode(jsonUtf8)        -> message        // JSON mode (0x04), self-describing
encodeBinary(jsonUtf8)  -> message        // binary mode (0x02), for size comparison
decode(message)         -> jsonUtf8       // any colbin JSON-mode message
inspect(message)        -> jsonUtf8       // column tree with byte spans
lastError()             -> jsonUtf8       // structured diagnostics, or empty
```

**The JSON parser lives inside the module.** The host never hands over a
JavaScript object graph, for one decisive reason and three supporting ones:

1. **`JSON.parse` destroys integers past 2^53.** `7295013456321098765` comes back
   as `7295013456321098800`. colbin has an exact int64 column; routing input
   through `JSON.parse` would corrupt values before the codec saw them. Parsing
   UTF-8 bytes in AS yields an exact `i64`. This alone settles it.
2. packed5 operates on **UTF-8 bytes** — its escape opcode carries bytes, not
   runes, which is what makes it byte-exact for invalid UTF-8. Taking the
   original bytes means zero string conversions; `TextEncoder`/`TextDecoder`
   handle the two that remain, in native code, once per call.
3. The module is then self-contained: one artifact, no TypeScript half to keep in
   step.
4. Errors can carry byte offsets into the input the caller actually holds.

**Nothing traps.** AS `abort()` becomes a WebAssembly `RuntimeError` that a
caller cannot act on, so every failure path sets a diagnostic (§4.4) and returns
an empty buffer.

### 2.2 Memory

`--runtime stub` is a bump allocator that never frees: smallest, fastest, and
wrong for a page where someone encodes fifty times. So **compile the module once
(`WebAssembly.Module`) and instantiate per operation** — each instance gets fresh
memory, instantiation of a ~50 KB module is well under a millisecond, and an input
large enough to exhaust memory takes its own instance down instead of poisoning
the caller.

The npm package exposes both shapes: one-shot helpers, and `createCodec()` over a
long-lived instance built with the incremental runtime for callers decoding in a
loop.

---

## 3. Inference

Rules, in full, because two implementations follow them (§7) and byte equality
between them is the test.

### 3.1 Shape

| JSON top level | colbin shape |
|---|---|
| array of objects | records mode, one record each |
| a single object | records mode, one record, `schSingleStruct` set — decodes back to an object, not a one-element array |
| array of scalars, array of arrays | value mode, `N = 1` |
| a bare scalar | value mode |
| `[]`, `null` | rejected — nothing to infer |

Nested objects become `ftStruct`, nested arrays `ftArray`. An array of objects
inside a record becomes an array of structs, which is the case the columnar
layout exists for.

### 3.2 Types

Every record is walked; each field unifies across all of them.

| observed across records | inferred |
|---|---|
| `true` / `false` | `skBool` |
| integers only | `skInt64` — **never narrowed**, see appendix A.3 |
| an integer above int64 max, all values non-negative | `skUint64` |
| **any one value fractional or exponential** | `skFloat64` for the entire column |
| string | `ftString` (packed5) |
| object | `ftStruct`, recursively |
| array | `ftArray`; the element type unifies across every element of every record |
| `null`, or the key absent from some record | the inferred type, made **nullable** — a pointer column: one flag byte, plus a presence bitmap only if a null actually occurs |
| only ever `null` | nullable string, with a warning |

`int` + `float` in one column unify to float64. That is the only widening;
everything else is a conflict.

### 3.3 Field order is load-bearing

**First-seen order**, across the record array, depth-first per object.

Field ids are an FNV-1a-32 hash of the JSON name xor-folded to 8 bits, with
collisions resolved by linear probing over the ids already taken — so the
assignment *depends on the order fields are walked*. Two implementations that
walk differently produce different bytes for identical input. First-seen also
preserves the caller's own key order through encode and back out of the AS
decoder (§6), which sorting would scramble.

---

## 4. Enforcement

This is property 2 of §1, and the part a schemaless encoder usually gets wrong by
being accommodating.

### 4.1 Conflicts are errors

A field holding two irreconcilable types across records is refused:

```json
{ "ok": false, "errors": [{
    "path": "[3].qty", "offset": 412, "line": 14,
    "message": "type conflict: string, but records 0-2 had int"
}]}
```

Refused rather than promoted to `ftAny`, deliberately. `ftAny` writes values
row-style with a tag each, which abandons the columnar layout for that field —
so an accommodating encoder would silently turn a 3.2x into a 1.2x and the caller
would never learn why (appendix A.1). Refusing is the honest failure.

An *opt-in* `{ "onConflict": "any" }` is a two-line change to the inference pass
and is deliberately not in the first release.

### 4.2 The other limits, all of them errors

| condition | why |
|---|---|
| more than 254 fields in one object | id 255 is the reserved terminator |
| a number that fits neither int64, uint64 nor float64 exactly | silent precision loss is the failure mode this module exists to avoid |
| nesting deeper than a fixed bound (proposed: 64) | the encoder recurses; unbounded input must not reach the stack |
| an input larger than a fixed bound (proposed: 64 MB) | a bump allocator cannot recover, so refuse before allocating |
| malformed JSON | reported with a byte offset |

### 4.3 What the *decoder* must survive

Decode is a separate trust boundary and a wider one: the encoder controls its own
input and can trust its counts, while the decoder is handed bytes by a stranger.
It also accepts messages this encoder would never produce, from a Go service that
had a real struct.

**Go's net is `panic` + `recover`, and AssemblyScript has neither half.**
`schema_decode.go` says so in its own comment: the column readers *"trust their
input and index the buffer directly, so a truncated or corrupt message can panic
on a slice bound. These entry points take data from a wire, so the panic is
converted here"* — `decodeSelfDescribing` wraps the lot in `defer recover()`. It
works. A record count corrupted to ~2^62 comes back as
`colbin: malformed self-describing message: runtime error: makeslice: len out of
range`, not as a crash. The mechanism is that Go bounds-checks every slice index,
unconditionally, and turns a bad one into a deterministic catchable panic.

Ported line-for-line, both halves are gone. Measured with a probe module
(`asc 0.28.20`, `--runtime incremental --optimize`, 1 page of memory, an
8-byte allocation at `36176`):

| access | result |
|---|---|
| `load<u8>(ptr + 8 / 64 / 1024)` — past the allocation, inside linear memory | **`0`. No trap, no error.** Reads whatever is there |
| `load<u8>(ptr + 60000)` — past linear memory | `RuntimeError` |
| `load<u8>(0xfffffff0)` | `RuntimeError: memory access out of bounds` |
| `buf[100]` on an 8-byte `Uint8Array`, `noAssert: false` | calls the imported `abort` |
| any export, *after* a trap | **still returns.** The instance is not killed |

So, precisely:

1. **The check depends on how you read.** Plain `Uint8Array` indexing *is*
   bounds-checked by default — that part of my first draft was wrong. But the
   check calls `abort`, and the hot path wants `load<T>` or `unchecked()`, which
   are not checked at all and return adjacent memory silently. **Neither form
   yields a diagnostic**: one traps, the other fabricates data. That is the
   argument, and it survives the correction.
2. **No `recover`.** `abort` is an *import*, so the host sees a message, a file
   and a line — but the module cannot catch it and cannot return
   `{"ok":false, path, offset}` from inside. And while the instance stays
   callable after a trap (measured — the canary export still returned `42`), a
   trap mid-allocation leaves the heap inconsistent, so continuing to use it is
   unsound even though it is possible.

So validation moves from the runtime into the code: **check every count and
offset against the remaining buffer before reading or allocating.** That is why
`decode` is not `encode` run backwards, and it is a genuine improvement on the Go
implementation rather than only a port.

**What this does not buy.** Safety, not correctness. All 267 single-byte
corruptions of an 89-byte message, run through Go's decoder (89 positions x masks
`0x01`, `0x80`, `0xff`):

| outcome | count |
|---|---:|
| clean error | 103 |
| **valid JSON, silently wrong** | **137** |
| identical — the bit did not matter | 27 |

Over half produce well-formed, wrong output, because a flipped bit inside a
varint payload is simply a different number and nothing distinguishes it from a
legitimate one. No amount of bounds checking changes that. Detecting corruption
needs a checksum the wire format does not have, which is a format question and
not this module's to answer. The guarantee is therefore precise: **no
out-of-bounds read, no unbounded allocation, always a diagnostic instead of a
trap** — and nothing at all about the values being right.

Confirmed empirically before the port started, not taken from the language
documentation — the table above is that probe, and it corrected the draft.

### 4.4 Diagnostics, not exceptions

One shape for every failure — parse, conflict, limit, corrupt message — carrying
`path`, byte `offset`, `line`, and a message that names both sides of the
disagreement. Warnings (an all-null column, an empty array that will not
round-trip) travel in the same envelope under `warnings`.

### 4.5 Never emit what cannot be read

The invariant of §1.3, and the one property worth paying for: **two outcomes
only — a decodable message, or an error.** Bytes that no decoder can read, or
that read back as something else, are the one failure this module must not have.
It is not free, and that is fine.

Four structural rules make it hold by construction rather than by care:

1. **Inference is a complete pass over every record before a single byte is
   emitted.** Never incremental. A schema decided from record 0 that record 500
   contradicts is exactly how an encoder emits a body its own schema does not
   describe.
2. **The body is generated from the inferred schema tree, never from the raw JSON
   values.** One source of truth means a value that does not fit its column type
   cannot be written — it is a §4.1 conflict at fill time instead.
3. **Field ids are asserted unique** after assignment. The decoder resolves a
   column through `st.byID[id]`; two fields sharing an id means one column is
   read into the wrong field, or the message dies with `field id N is not in the
   schema`. The probe loop must not be trusted to be right, it must be checked.
4. **The encoder's depth and size bounds are at or below the decoder's** (§4.2),
   so this module can always read back what it just wrote.

The specific places where an encoder silently produces an unreadable message,
each verified against the Go implementation:

| hazard | what goes wrong |
|---|---|
| **empty columns** | `varint.AppendArray` on an empty slice emits one `trRaw` header byte, *not* zero bytes (`varint/array.go:218`). An all-zero float column — and an empty one, vacuously — sets the empty bit and writes no payload at all. A nullable column with no nulls writes `nullFlags = 0` and no bitmap. Get any of these wrong by one byte and every later column in the message is misaligned |
| **`1e400`** | syntactically valid JSON. A naive `strtod` yields `+Inf`, the float column holds it, and `DecodeJSON` renders non-finite floats as `null` — the value silently becomes nothing. Must be a §4.2 error, which is also what `encoding/json` does |
| **lone surrogates** | `"\uD800"` unpaired is accepted by many JSON parsers. Defined here as `encoding/json` does it: replace with U+FFFD, so the bytes handed to packed5 are valid UTF-8 and parity with the oracle holds |
| **duplicate field ids** | rule 3 above |
| **`descCyclic`** | unreachable — JSON cannot express a self-referential type — so the bit is never set and the empty-column elision path is never taken. Worth stating because the Go encoder has it and a faithful port might carry it across |

**The self-check.** Structural rules are an argument; this is a proof. `encode`
decodes its own output and compares it structurally against the parsed input tree
before returning — the tree is already in memory, so the comparison is a walk.
Cost is roughly one decode, so call it 2x encode. **On by default**, because
§4.3 established that a well-formed-but-wrong message is the failure mode that
does not announce itself, and an encoder that verifies is worth twice the CPU of
one that hopes. `{ "verify": false }` opts out for a caller who has measured and
decided.

Note it compares *values*, not bytes: "it decoded without error" is far too weak
a check, for exactly the reason 137 of 267 corrupt messages in §4.3 decoded
without error.

**The CI property.** For any generated JSON, `encode` either returns a
diagnostic, or returns bytes that **both** the AS decoder and Go's `DecodeJSON`
accept and that compare equal to the input. That is §1.3 written as a test, and
it is what phase 2 gates on.

---

## 5. Encode: format coverage

| colbin column | encoder emits | notes |
|---|---|---|
| `ftInt` | yes | int64 / uint64 / bool |
| `ftFloat` | yes | float64 |
| `ftString` | yes | packed5, raw fallback |
| `ftStruct` | yes | nested objects |
| `ftArray` | yes | including arrays of structs |
| nullable | yes | null or absent key |
| `ftBytes` | **no** | JSON has no bytes type; inventing a `"base64:"` convention would be inventing format semantics the library does not have |
| value mode | yes | a top level that is not a batch of records: a bare scalar, an array of scalars, an array of arrays |
| `ftMap` | **no** | a JSON object is a struct here; a map needs a declared key type |
| `ftAny` | **no** | §4.1 |
| compact mode | phase 5 | affects only the binary-mode size number; `MarshalJSON` is always standard mode |

The decoder handles all of them regardless (§4.3) — asymmetric on purpose.

---

## 6. Decode

`decode(message)` renders JSON, matching Go's `DecodeJSON` conventions:
`[]byte` → base64, non-string map keys stringified, `NaN`/`±Inf` → `null`, empty
and nil slices both → `null`.

**One deliberate divergence: key order.** Go's `DecodeJSON` builds a
`map[string]any` per record and marshals it, so `encoding/json` sorts the keys
alphabetically — a struct declared `ID, Name, Price, Active` decodes to
`{"active":…,"id":…,"name":…,"price":…}`. The AS decoder reads columns in wire
order and never builds a map, so it emits them in that order, which is the
first-seen order the encoder recorded (§3.3). `encode` then `decode` therefore
returns the caller's own key order, and for the common case is textually
identical to the input. Go cannot do this and the difference is not a bug in
either — but it does mean §7's level-3 comparison must be on parsed values, not
on text.

Two properties are inherent to a dense columnar layout and will look like bugs to
anyone who has not read the README, so they are stated wherever they can be
observed:

- **A missing key and an explicit `null` decode identically.** Measured:
  `[{"id":1,"note":"hi"},{"id":2,"note":null},{"id":3}]` comes back with a third
  record of `{"id":3,"note":null}`. Columns are dense — every record carries a
  value for every field.
- **An empty array and `null` decode identically.**

Neither is fixable without a wire change, and neither is worth one.

---

## 7. Verification

Three levels, cheapest first, all in CI.

**Level 1 — layer vectors.** A Go generator writes frames from `packed5.Append`
(ASCII, accented, uppercase-dominant, digits, invalid UTF-8, NUL bytes, empty,
over 30 bytes so the length code escapes to a uvarint) and `varint.AppendArray`
(every transform: raw, delta, FOR, fixed; every `k`; zigzag both ways; every
element width; the `[100,240,250,380]` case `TestArrayDeltaExampleIsOptimal`
pins). The AS ports must reproduce each frame **byte for byte** and decode each
back. This is where the port's risk is concentrated, and it is testable before a
message exists.

**Level 2 — message vectors, against an oracle.** Measured and working
(appendix A.2): infer a shape from JSON, build a matching Go type with
`reflect.StructOf` carrying `cb:"<jsonName>"` tags, fill it by reflection, call
the real `colbin.MarshalJSON`. ~200 lines, checked in as `web/vectors/`, never
shipped to a browser. **The AS module must produce the same buffer for the same
input** — which tests §3's rules, field-id probing, the schema section and the
column layout in a single comparison, and turns any drift between the two
implementations of §3 into a diff instead of a subtly wrong payload.

**Level 3 — cross round-trip.** AS bytes → Go `DecodeJSON` → the original JSON.
Go bytes → AS `decode` → the same. Compared as **parsed values, not text**: the
two decoders order keys differently on purpose (§6). This is what proves
interoperation with a real service rather than with itself.

Plus a fuzz target on `decode`: random and mutated buffers must produce a
diagnostic, never a trap and never an out-of-bounds read (§4.3). Its oracle is
the corruption sweep above — Go answers 103 of 267 with an error and the rest
with silently wrong JSON, and the AS decoder must not do materially worse on the
first count.

---

## 8. Shape

```
web/
  PLAN.md
  package.json  asconfig.json  vite.config.ts  svelte.config.js  tsconfig.json
  assembly/                  the module — knows bytes and colbin, nothing about Svelte
    index.ts                 the ABI of §2.1 and nothing else
    json.ts                  UTF-8 scanner: exact i64/u64/f64, byte offsets for errors
    infer.ts                 §3
    enforce.ts               §4: constraints, limits, diagnostics
    fieldid.ts               FNV-1a-32 xor-folded to 8 bits, linear probe
    schema.ts                the JSON-mode schema section, both directions
    encode.ts  decode.ts     columnar body, recursive
    varint.ts                port of varint/
    packed5.ts               port of packed5/
    bitstream.ts             shared LSB-first reader/writer
    compact.ts               port of compact/                   (phase 5)
    inspect.ts               column tree with byte spans
  tests/                     node:test over the level-1/2/3 vectors
  vectors/                   Go: the oracle and the generator (§7)
  src/                       the SvelteKit site (§9)
  static/  .nojekyll  CNAME
.github/workflows/  ci.yml  pages.yml
```

`web/` sits inside the Go module; harmless, since `go` skips `node_modules` and
`web/vectors` imports only colbin and the standard library.

---

## 9. The page

Left: examples. Centre: the JSON. Right: the message.

```
┌──────────┬────────────────────────┬───────────────────────────────┐
│ products │  JSON                  │  24211 B → 7511 B   3.22x     │
│ metrics  │  (editable, the error  │                               │
│ invoices │   line marked)         │  columns          bytes       │
│ people   │                        │  ├ id      int64    14   11%  │
│ single   │                        │  ├ name    string   47   38%  │
│ scalars  │                        │  ├ price   int64    18   15%  │
│ bigints  │                        │  └ lines   [struct] 31   25%  │
│ nulls    │                        │     ├ sku  string   19        │
│ mixed    │                        │     └ qty  int64     8        │
│ broken   │                        │  ─────────────────────────    │
│          │                        │  hex (hover a column)         │
│          │                        │  [ download ]  [ upload ]     │
└──────────┴────────────────────────┴───────────────────────────────┘
```

The **column inspector is the point** — anyone can be told "3x smaller"; watching
`name` eat 38% of the payload is what makes the format legible. It is driven by
`inspect()`, so it is also the decoder's own test: if the spans do not tile the
buffer exactly, the decoder is wrong.

Encoding re-runs on a debounce as you type. Dropping a `.cbj` file decodes it
with no JSON pasted at all — the path a colbin *user* actually cares about.

Examples, chosen to cover §3 and §4 rather than to flatter the format:

| example | exercises |
|---|---|
| products, 200 | the ordinary case |
| metric points, 200 | near-uniform int deltas, varint's best case |
| invoices | nesting and arrays of structs |
| people | string-heavy, packed5 doing the work, accented names |
| one object | records mode with `schSingleStruct` — **and colbin losing to JSON**, 44 B against 35 B |
| `[1,2,3,4,5]` | value mode — **loses**, 0.61x |
| big integers | ids past 2^53 that `JSON.parse` corrupts and this module keeps exactly (§2.1) |
| nulls / missing keys | §6's dense-column property, visible |
| mixed numbers | one `2.5` promoting a column to float64 |
| broken | `qty` a string in record 3 — §4.1's error, doing its job |

The last five exist to be honest. A demo that ships only its best cases is one
nobody believes twice.

Plain Svelte 5 runes and scoped CSS — **no `@genix/ui`, and no Tailwind either**.
The plan called for Tailwind; the page turned out to be two panes, a tree and a
hex dump, so component-scoped CSS says the same thing with one less dependency
and one less config file. genix-ui earned its place in the facturago demo, which
needed tables, editable cells and form inputs, and would have brought the
dependency-optimizer and orphaned-asset work with it for nothing here.

**One thing the page had to get right to stay honest.** The examples are
pretty-printed so they can be read and edited, and comparing colbin against
*that* would have flattered it by two or three times — the lone-object example
came out looking like a win. The size comparison is therefore against the
minified JSON, computed with a string-aware scanner rather than
`JSON.parse` + `stringify`, since parsing would round any integer past 2^53 and
that is the failure the page exists to show.

---

## 10. Phases

Each ends somewhere verifiable. The page comes before codec breadth because you
asked for it first, and a deployed page that handles ints and strings is worth
more than a complete codec with nothing to look at.

| # | Phase | Done when |
|---|---|---|
| 1 | ✅ Scaffold: SvelteKit static, `asc` build, Pages, CI. | the site builds; the wasm instantiates; headless Chrome reports no console errors. **Not yet deployed** — §14 is the caller's to do |
| 2 | ✅ Module core: `bitstream`, `varint`, `packed5`, JSON scanner, §3 inference, §4 enforcement, schema section, int/bool/string columns. Level-1 and level-2 vectors for those types. | layer vectors byte-identical; the products example matches the oracle; **32.5 KB gzipped** against the 50 KB budget |
| 3 | ✅ The page: examples, editor, inspector, hex view, diagnostics, download, upload. | Chrome drives all 10 examples end to end with no console errors |
| 4 | ✅ Breadth. Encode: float64, uint64, nested struct, array of struct, nullable, value mode. Decode: also `ftBytes`, `ftMap`, `ftAny`, plus §4.3's bounds checking and the fuzz target. | every example matches the oracle; level 3 passes both directions; the fuzzer finds no trap |
| 5 | Compact mode, so the binary-mode number is right at 1–3 records; upload/drop. | a 1-record message is 12 B (appendix A.1) |
| 6 | Publish `@ivanjoz/colbin`; README section. | `npm i @ivanjoz/colbin` decodes a Go-produced message in Node |

Phase 1 deploys before there is anything to look at, deliberately — Pages
base-path and `.nojekyll` mistakes are cheap against a placeholder and expensive
against a finished UI.

---

## 11. The npm package is the point of the port

A JavaScript client of a Go service answering in colbin currently has no way to
read the response. After phase 4:

```js
import { decode, encode } from '@ivanjoz/colbin'

const rows = JSON.parse(new TextDecoder().decode(decode(bytes)))
```

Which is why `assembly/` stays free of anything page-specific.

---

## 12. Risks

| risk | mitigation |
|---|---|
| Bit-level divergence — a shift arithmetic in one language and logical in the other, an overflow Go wraps differently | Level-1 vectors, built first (phase 2), covering every transform and every `k`. This is *the* risk; the rest is ordinary work |
| Field-id probing drifts between the two §3 implementations | Level-2 vectors include a deliberate FNV collision and a 254-field object |
| `--runtime stub` growth on a large input | Instantiate per call (§2.2); refuse oversized input (§4.2) rather than hang |
| AS managed `Map`/`Array` allocating heavily in inference | Hot paths on typed arrays and an arena; measure at phase 4, do not pre-optimise |
| The 50 KB budget needs the incremental GC | Gated in phase 2, before the page depends on it; fallback is the GC and an honestly larger number |
| Two implementations of §3 forever | Accepted. The Go one is test infrastructure, never ships, and the vectors fail loudly |

---

## 13. Decisions taken

Answered 2026-08-26:

1. **AssemblyScript from the start** — no interim Go WASM backend, no throwaway
   TypeScript codec (§1, appendix A.4).
2. **`colbin.un.pe`**, served from the root, so `paths.base` stays empty.
   `static/CNAME` and `static/.nojekyll` carry it.
3. **English UI.**
4. **Inference over `ftAny`** (appendix A.1); **conflicts are errors** (§4.1).
5. **Integers are never narrowed** (appendix A.3) — your rule, and it costs
   nothing.
6. **No `@genix/ui`** (§9).

Open, cheap to change:

- **Extensions.** Proposing `.cbj` for a self-describing JSON-mode message and
  `.cb` for binary mode, since the two are not interchangeable. Not a wire
  concern.
- **`onConflict: "any"`** as an opt-in (§4.1).
- **Depth and size bounds** (§4.2) — 64 and 64 MB are proposals, not measurements.

---

## 14. Before the first deploy

`.github/workflows/ci.yml` does the rest of it: one workflow tests the codec,
compiles the module, drives the page in headless Chrome, publishes the `.wasm`
as an artifact, and deploys the site. `actions/configure-pages` runs with
`enablement: true`, so it turns Pages on with source *GitHub Actions* rather
than waiting for someone to tick it, and `static/CNAME` ships in the build
output, so the custom domain field fills itself in.

What is left is the part that lives outside the repository:

1. **DNS** — a `CNAME` record for `colbin` in the `un.pe` zone, pointing at
   `ivanjoz.github.io`.
2. **Enforce HTTPS** — tick it once GitHub's DNS check passes, which is minutes
   to hours after the record propagates.

A tag matching `v*` additionally publishes `colbin.wasm` as a release asset,
taken from the same build the tests ran against rather than compiled again.

---

## Appendix A — the measurements that settled a decision

Taken with a throwaway Go spike (`/tmp/cbspike`, not checked in; §7's
`web/vectors` is the version that gets built). Corpora are synthetic and regular.
These exist to justify choices above, not to make cross-format claims —
`comparison/` is the authority for those.

### A.1 Why inference, and not `ftAny`

colbin can already eat arbitrary JSON with no inference at all: marshalling
`[]map[string]any` uses `ftAny` columns, which write each value row-style as
`[tag][payload]`. It works today, and it is barely worth doing — **1.17x** versus
JSON at 200 records, against **3.22x** for the same data through an inferred
struct schema. §4.1's refusal to fall back to `ftAny` follows from this.

Sizes across the corpora, in bytes:

| corpus | JSON | binary mode | JSON mode |
|---|---:|---:|---:|
| products, 200 | 24211 | 7447 | 7511 |
| products, 1000 | 121550 | 36727 | 36791 |
| metrics, 200 | 9001 | 3215 | 3237 |
| nested, 2 records | 198 | 68 | 126 |
| one flat object | 35 | **12** | **44** |
| `[1,2,3,4,5]` | 11 | 12 | 18 |

The last two rows are why the page shows both modes and ships losing examples:
JSON mode carries a schema section, so small messages are mostly schema, while
binary mode needs the type at the far end.

### A.2 The oracle works

`reflect.StructOf` + `cb`/`json` tags + `colbin.MarshalJSON` round-tripped
through `DecodeJSON` on the first attempt across flat records, nested objects,
arrays of objects, nulls, mixed numbers, scalar arrays and four-deep nesting.
This is what makes §7's level 2 possible, and it is the single most valuable
finding here: it turns the port from a correctness leap into a diffing exercise.

### A.3 Never narrowing integers costs nothing

| 1000-record column | declared `int64` | declared narrow |
|---|---:|---:|
| products fixture | 13164 B | 13164 B (int32/int8) |
| uniform random over full int32 range | 4001 B | 4001 B (int32) |
| uniform random over full int16 range | 2007 B | 2007 B (int16) |

Byte-identical. `varint/README.md` explains it: the codec *declares* `k` and `M`
from the data rather than deriving them from the element type, so the declared
width survives only as a decode-side truncation rule. Column-width inference is
therefore not on the roadmap.

### A.4 What Go WASM would have cost

`GOOS=js GOARCH=wasm`, `-ldflags="-s -w"`, linking colbin + `reflect` +
`encoding/json`: **4.6 MB raw, 1.27 MB gzipped**. TinyGo cannot substitute —
colbin reaches `reflect` throughout and the oracle needs `reflect.StructOf`,
which TinyGo does not implement. Against a sub-50 KB target, a 25x difference on
the thing that must load before anything happens.

### A.5 AssemblyScript's parseFloat is not correctly rounded

Found during the port rather than before it, and it changed what the module
contains.

`2.2250738585072011e-308` sits exactly on the boundary between the largest
subnormal and the smallest normal — the value that has broken parsers before.
Go's `strconv` and JavaScript both read it as `000fffffffffffff`. AssemblyScript
0.28.20 returns `0010000000000000`: one ULP out, silently.

Not a corner this module can leave alone. "Numbers survive intact" is its
central claim (§2.1), and a float differing in the last bit from what every
other JSON reader produces is exactly the silent corruption the exact integer
path exists to prevent. It would also break level-2 byte parity, since the
oracle parses with Go.

So `assembly/decimal.ts` converts exactly instead: the literal is held as a
rational of arbitrary precision, the binary exponent comes from bit lengths, and
the significand is the correctly rounded quotient, ties to even. No
power-of-five table, so nothing extra to carry in the binary; no floating-point
step, so nothing to round twice. The scanner keeps a fast path for the ordinary
case — a significand under 2^53 with |exp| <= 22, where one multiply is exactly
rounded — and this runs for the rest.

Checked against Go over 527 number vectors: the hand-picked boundaries, 450
random float64s rendered three ways each, and 40 subnormals. All bit-identical.

### A.6 What the schemaless path costs, and what the self-check costs

Measured after the port, on 1000 product records (107 KB of JSON), best of 12
runs of 20:

| | time |
|---|---:|
| the module: parse + infer + encode | **6.07 ms** |
| the module, with the §4.5 self-check | 11.65 ms |
| the Go oracle: the same task, reflection-based | 5.31 ms |
| Go `MarshalJSON` on a typed struct, no parsing, no inference | 0.17 ms |

Two things worth reading off this.

**The columnar encode is not the cost; being schemaless is.** Go does the same
end-to-end job in 5.31 ms — within 15% of the WebAssembly module — while Go's
typed path, which is handed a struct and skips both parsing and inference, is
35x faster than either. So the module is not slow *because it is
AssemblyScript*; the parse and the two inference passes dominate, and they cost
what they cost in Go too. That also means optimising the AS columnar writer
would buy very little until the tree and the observation pass are cheaper —
which is what §12 says to measure before touching.

**The self-check costs 92%**, matching the "roughly one decode, so call it 2x"
the §4.5 estimate assumed. On by default is therefore a real price, and a
defensible one: it is the difference between believing the encoder and checking
it, and the very first thing it caught was a genuine bug (a duplicate key
encoding its first value while everything downstream expected its last).

### A.7 Encoding cost, for context

Not a design input, recorded because it will be asked. 1000 records:
`colbin.MarshalJSON` 168 µs against `json.Marshal` 330 µs — half the time and a
third of the bytes, so colbin is not trading CPU for size against JSON. (gzip at
level 6 costs 654 µs on the JSON and 266 µs on the colbin message, which is a
fact about gzip, not about either format.) Go's stdlib gzip is slow and this
laptop throttles; treat as indicative.
