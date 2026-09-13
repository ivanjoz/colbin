# colbin for AssemblyScript

A colbin **encoder and decoder compiled to WebAssembly from AssemblyScript**. It
takes JSON with no schema attached, derives one, enforces it, and emits a message
any colbin reader can decode — including Go. It reads one back the same way,
given the schema or a message that carries its own.

A static SvelteKit site on GitHub Pages is how it gets exercised and shown: paste
JSON, see the message, see which field cost what, download it, open one back.

Status: **built**. This document describes what the module is. `REFACTOR_PLAN.md`
is the record of the re-port that produced it — the format changed underneath the
first version, and the decisions taken while catching up live there rather than
here. The two things still open are listed in §10.

---

## 1. The thing that got built

```
JSON (no schema)  ──▶  infer  ──▶  enforce  ──▶  colbin message  ──▶  JSON
                        §3          §4            §5                  §6
```

Three properties, in priority order. Everything else serves them.

1. **Schemaless in.** The caller has JSON and nothing else — no `.proto`, no Go
   struct, no type declaration. The schema is derived from the data.
2. **Type constraints enforced.** A derived schema is a *claim about every
   record*. Records that contradict it are rejected with a location, not
   silently widened, coerced, or dropped into an untyped column.
3. **Decodable, or an error — never anything else.** `encode` is a total
   function with exactly two outcomes: a message that decodes, or a diagnostic.
   It must never return bytes that a decoder cannot read, or that read back as
   something other than what went in. §4.5 is how that is held.

The web page is a consumer of the module, not the other way round. If the page
were deleted the module would still be the deliverable.

### Why AssemblyScript, concretely

The column codec packs `int64` residuals at a bit width chosen per block;
`packed5` runs an LSB-first bitstream with unsigned shifts. **JavaScript has no
int64.** A JS port means `BigInt` — allocating, roughly an order of magnitude
slower — or hand-split hi/lo arithmetic through every one of those paths.
AssemblyScript has native `i64`, `u64` and the shift semantics the Go code was
written against, so each file stays close enough to a transliteration to be
reviewed line by line against the Go one it names.

A Go `GOOS=js` build was measured at **1.27 MB gzipped** (appendix A.4) against a
target under 50 KB, and would not have produced a library anyone could use from
JavaScript.

---

## 2. The module

### 2.1 ABI — UTF-8 bytes in, UTF-8 bytes out

```
alloc(n)             -> ptr      reserve n bytes for the caller to write into
resultPtr()          -> ptr      where the last call left its output
lastError()          -> len      the last diagnostic, as JSON

encode(len, flags)   -> len      JSON -> a message.  flags: SELF_DESCRIBING, VERIFY
section()            -> len      the schema for the last encode, to send once
setSchema(len)       -> 0 | -1   hold a section for the decodes that follow
decode(len)          -> len      message -> JSON text
inspectMessage(len)  -> len      message -> the field tree, with byte spans
```

**The JSON parser lives inside the module.** The host never hands over a
JavaScript object graph, for one decisive reason and three supporting ones:

1. **`JSON.parse` destroys integers past 2^53.** `7295013456321098765` comes back
   as `7295013456321098800`. colbin has an exact int64 field; routing input
   through `JSON.parse` would corrupt values before the codec saw them. Parsing
   UTF-8 bytes in AssemblyScript yields an exact `i64`. This alone settles it.
   It is not a hypothetical: it was found *again* during the re-port, in the
   module's own test corpus, which recorded column values as JSON numbers and so
   could only be read by a parser that corrupted them.
2. String encodings operate on **UTF-8 bytes**. Taking the original bytes means
   zero string conversions; `TextEncoder`/`TextDecoder` handle the two that
   remain, in native code, once per call.
3. The module is then self-contained: one artifact, no TypeScript half to keep in
   step.
4. Errors can carry byte offsets into the input the caller actually holds.

**Nothing traps.** An AssemblyScript `abort()` becomes a WebAssembly
`RuntimeError` that a caller cannot act on, so every failure path sets a
diagnostic and returns a negative length.

### 2.2 Where the schema travels

Out of band by default, which is the delivery the format's own README advises:
send `section()` once per connection, then send ordinary messages. The section is
not in the message and does not grow with the record count.

`SELF_DESCRIBING` puts it in front of the body instead and sets the schema bit in
the root byte, for a document that has to stand alone. That is what the page's
download button writes. §9 prices the difference.

### 2.3 Memory

The module is compiled once (`WebAssembly.Module`) and **instantiated per
operation**: each instance gets fresh memory, instantiation is well under a
millisecond, and an input large enough to exhaust memory takes its own instance
down instead of poisoning the caller.

One consequence bites anyone driving the ABI directly, and it is worth stating
because it has now caught two callers: **a call that grows memory detaches every
existing `ArrayBuffer` view.** Take the offset from `alloc` first, then build the
view.

---

## 3. Inference

Rules, in full, because the encoder and the page both depend on them being
predictable.

### 3.1 Shape

A colbin message is **one struct**. JSON's top level often is not, so:

| JSON top level | what is built |
|---|---|
| an object | that object is the record; the root plan is its struct |
| anything else | a one-field **envelope**, named `rows`, holding the document |
| `[]`, `null` | rejected — nothing to infer |

The envelope is marked in the schema section, not guessed at on the way back. A
`structDef`'s flags byte has seven spare bits and Go's parser reads only bit 0,
so bit 1 says "envelope": this module unwraps it, and Go — which does not know
the bit — renders `{"rows": …}`, which is more literal rather than wrong. The
root byte was the wrong place for it: those detail bits belong to the format, and
a reader refuses every one it does not assign, so a message marked there would
have been refused outright.

### 3.2 Types

Every record is walked; each field unifies across all of them.

| observed across records | inferred |
|---|---|
| `true` / `false` | `bool` |
| integers only | `int64` — **never narrowed**, see appendix A.3 |
| an integer above int64 max, all values non-negative | `uint64` |
| **any one value fractional or exponential** | `float64` for the entire field |
| string | `string` |
| object | a nested struct, recursively |
| array of objects | a slice of structs — list or table, decided per field |
| array of integers / of strings | the matching array op |
| `null`, or the key absent from some record | a **pointer** to the inferred type |

`int` + `float` in one field unify to float64. That is the only widening;
everything else is a conflict.

### 3.3 Field ids are sequential, in first-seen order

**First-seen order**, across the record array, depth-first per object — and the
id *is* the position: 0, 1, 2.

The first version hashed the name with `fnv8` and probed past collisions, to
match what Go does for an untagged struct. A derived id lands anywhere in 0..255,
which four key bits cannot hold, so that put **every** message on eight-bit keys.
Numbering them instead puts any object of sixteen fields or fewer on the four-bit
fast path, a byte per present field smaller. The section carries the ids, so
nothing that reads it can be confused by the change.

First-seen order stays exactly as load-bearing as it was, for a different reason:
it is no longer the input to a hash, it *is* the id. Two implementations that
walk differently produce different bytes for identical input.

---

## 4. Enforcement

This is property 2 of §1, and the part a schemaless encoder usually gets wrong by
being accommodating.

### 4.1 Conflicts are errors

A field holding two irreconcilable types across records is refused:

```json
{ "ok": false, "errors": [{
    "path": "[3].qty", "offset": 412, "line": 14,
    "message": "type conflict: string, but records 0-2 had number"
}]}
```

Refused rather than promoted to an untyped column, deliberately. A column of
"whatever this row had" is not a column: it abandons the layout for that field
and the caller never learns why. Refusing is the honest failure.

### 4.2 The other limits, all of them errors

| condition | why |
|---|---|
| more than 256 fields in one object | a key is one byte |
| a number that fits neither int64, uint64 nor float64 exactly | silent precision loss is the failure mode this module exists to avoid |
| nesting deeper than 64 on encode | the encoder recurses; unbounded input must not reach the stack. The decoder's bound is 128, so the module can always read back what it wrote |
| an array of floats, of booleans, or of arrays | the format has no op for one. `sliceOp` in `codec/codec.go` resolves integers, strings and structs and refuses the rest |
| malformed JSON | reported with a byte offset |

A **null where an object belongs** warns rather than refuses. The format has no
pointer to a composite, so the null is omitted and comes back as an object of
zeros — the rest of the document is still exactly what it was.

### 4.3 What the *decoder* must survive

Decode is a separate trust boundary and a wider one: the encoder controls its own
input and can trust its counts, while the decoder is handed bytes by a stranger.
It also accepts messages this encoder would never produce, from a Go service that
had a real struct.

**Go's net is `panic` + `recover`, and AssemblyScript has neither half.** Go
bounds-checks every slice index unconditionally and turns a bad one into a
deterministic catchable panic; `codec` converts those at its entry points.
Ported line-for-line, both halves are gone. Measured with a probe module:

| access | result |
|---|---|
| `load<u8>(ptr + 8 / 64 / 1024)` — past the allocation, inside linear memory | **`0`. No trap, no error.** Reads whatever is there |
| `load<u8>(ptr + 60000)` — past linear memory | `RuntimeError` |
| `buf[100]` on an 8-byte `Uint8Array`, `noAssert: false` | calls the imported `abort` |
| any export, *after* a trap | **still returns.** The instance is not killed |

So: the check depends on how you read, and **neither form yields a diagnostic** —
one traps, the other fabricates data. And there is no `recover`: `abort` is an
import, so the host sees a message, a file and a line, but the module cannot
catch it and cannot return `{"ok":false, path, offset}` from inside.

Validation therefore moves from the runtime into the code: **check every count
and offset against the remaining buffer before reading or allocating.** The new
format makes that easier than the old one did — every composite carries a byte
length, so a length past the end of its parent is a definite error rather than a
plausible one.

**One place a number still decides an allocation**, and it is the one divergence
from Go on this path: a table's row count. Go allocates what the message asks for
and lets `makeslice: len out of range` become a recovered panic; here an
allocation that cannot be satisfied calls `abort`. So the count is refused in
front of the allocation, against a bound that is honestly a budget rather than a
proof — a constant column is nine bytes for any length, so no bound can be
derived from the bytes left.

**What this does not buy.** Safety, not correctness. Every single-byte corruption
of every message in the corpus — 5046 of them — run through both decoders:

| outcome | count |
|---|---:|
| refused, by **both** Go and this module | 806 |
| **valid JSON, silently wrong** | 4193 |
| identical — the bit did not matter | 47 |

The two refuse **exactly the same 806**, per case, which is the property the
fuzz target pins: fewer would mean walking into something Go catches, and more
would mean rejecting a message Go accepts, which on a decoder is the worse
failure. Eighty-three per cent produce well-formed, wrong output, because a flipped bit
inside a magnitude is simply a different number. No amount of bounds checking
changes that; detecting it needs a checksum the format does not have. So the
guarantee is precise: **no out-of-bounds read, no unbounded allocation, always a
diagnostic instead of a trap** — and nothing at all about the values being right.

(An earlier draft of this document quoted 38.6% refused, from a sweep against the
old columnar format. That format carried more redundancy to contradict. A leaner
wire detects less, and the honest figure for this one is 16%.)

### 4.4 Diagnostics, not exceptions

One shape for every failure — parse, conflict, limit, corrupt message — carrying
`path`, byte `offset`, `line`, and a message that names both sides of the
disagreement. Warnings travel in the same envelope under `warnings`.

### 4.5 Never emit what cannot be read

The invariant of §1.3, and the one property worth paying for. Four structural
rules make it hold by construction rather than by care:

1. **Inference is a complete pass over every record before a single byte is
   emitted.** Never incremental. A schema decided from record 0 that record 500
   contradicts is exactly how an encoder emits a body its own schema does not
   describe.
2. **The body is generated from the inferred plan, never from the raw JSON
   values.** One source of truth means a value that does not fit its field
   cannot be written — it is a §4.1 conflict at fill time instead.
3. **Field ids are positions**, so two fields cannot collide by construction.
   (The hash-and-probe this replaced needed an assertion instead.)
4. **The encoder's depth and size bounds are at or below the decoder's** (§4.2),
   so this module can always read back what it just wrote.

**The self-check.** Structural rules are an argument; this is a proof. `encode`
decodes its own output and walks it against the parsed input before returning.
**On by default**, because §4.3 established that a well-formed-but-wrong message
is the failure mode that does not announce itself.

It compares *values*, not bytes: "it decoded without error" is far too weak a
check, and comparing the two texts would be too strong — the decoder writes every
field of the schema where the input wrote only what it had. Three differences are
expected rather than failures, and each is a documented property of the layout
(§6): an absent key comes back as its zero, an empty array comes back as null,
and an integer in a field one float promoted comes back as that float.

Two hazards are worth naming because each was found rather than foreseen:

| hazard | what goes wrong |
|---|---|
| **`1e400`** | syntactically valid JSON. A naive `strtod` yields `+Inf`, and JSON has no spelling for one. Refused, which is also what `encoding/json` does |
| **lone surrogates** | `"\uD800"` unpaired is accepted by many JSON parsers. Defined here as `encoding/json` does it: replaced with U+FFFD |
| **duplicate keys** | the last occurrence wins, as `JSON.parse` does. Taking the first was a real bug, and the self-check caught it the first time it ran |

---

## 5. Encode: format coverage

| colbin field | encoder emits | notes |
|---|---|---|
| integers, bool | yes | int64 / uint64 / bool |
| floats | yes | float64 |
| strings | yes | raw. The module *reads* the packed encoding but does not write it — §10 |
| nested structs | yes | |
| slices of structs | yes | list or table, decided per field on the row count |
| integer and string arrays | yes | |
| pointers | yes | a null or an absent key |
| `[]byte` | **no** | JSON has no bytes type; inventing a `"base64:"` convention would be inventing format semantics the library does not have |
| maps | **no** | a JSON object is a struct here; a map needs a declared key type |
| float / bool / nested arrays | **no** | §4.2 — the format has no op |

The decoder handles all of them regardless, asymmetrically and on purpose: a Go
service that had a real struct will send `[]byte`, map and eight-bit-keyed
fields, and refusing to read them would make the decoder useless for the case it
exists for.

---

## 6. Decode

`decode` renders JSON matching Go's `ToJSON` byte for byte — down to the escaping
and the spelling of the numbers, which is a stronger claim than "valid JSON" and
the one the vectors check. A test that compared parsed values would not notice a
float printed one digit differently.

Two properties are inherent to the layout and will look like bugs to anyone who
has not read the README, so they are stated wherever they can be observed:

- **A missing key and an explicit `null` decode identically.** A field holding
  its zero is not written at all, so `[{"id":1,"note":"hi"},{"id":2,"note":null},
  {"id":3}]` comes back with a third record of `{"id":3,"note":null}`.
- **An empty array and `null` decode identically.**

Neither is fixable without a wire change, and neither is worth one.

The key-order divergence the first version documented is **gone**: Go no longer
builds a `map[string]any` and lets `encoding/json` sort it, so both sides now
write the fields the message carried first, in wire order, then the ones it
omitted.

---

## 7. Verification

Four tiers, generated from the Go codecs into `web/vectors/vectors.json` —
committed, regenerated in CI, and diffed, which is the discipline `rust/vectors`
keeps. `go test ./web/vectors` fails if the committed corpus has drifted.

1. **String framing** — a blob at every length the header holds, at each escape
   boundary, under both key widths. This tier exists because §10's change will
   move it: it is the tripwire that names the file to edit.
2. **The column codec** — every transform, both zigzag decisions, widths 0..64,
   and the block boundary at 127/128/129.
3. **Messages** — the `corpus` types, each with its section, its field ids, both
   deliveries and its `ToJSON` output. Byte-identical, both directions.
4. **The JSON scanner** — 527 number literals and 19 string literals, including
   450 random float64s rendered three ways each and 40 subnormals.

**And the other direction, which no byte-for-byte corpus can test.**
`node tests/emit.mjs` writes what the module encodes and `go test ./web/vectors`
reads it back with `ParseSchema` + `ToJSON`. The module's encoder is the thing
under test rather than the thing being copied, so there is nothing to diff it
against — and this is the contract a caller actually has: a JavaScript client
encodes, a Go service decodes. Same shape as the Rust port's `emit_vectors` step.

Plus a **fuzz target** on decode: random buffers, random buffers behind a valid
root byte, truncation at every length, mutated sections, and the corruption sweep
of §4.3. Every one must produce a diagnostic or valid JSON, never a trap.

What is *not* checked: that the two encoders pick the same **layout** for the
same data. Byte parity would need §3's rules implemented a second time in Go
forever, and since the module ships its section, Go does not need to agree about
ids or layout to read a message. The table threshold and the key-width rule are
held by the comments that cite Go's constants and by nothing else.

---

## 8. Shape

```
web/
  PLAN.md              this
  REFACTOR_PLAN.md     the re-port that produced it
  assembly/            the module — knows bytes and colbin, nothing about Svelte
    index.ts           the ABI of §2.1 and nothing else
    json.ts            UTF-8 scanner: exact i64/u64/f64, byte offsets for errors
    decimal.ts         correctly-rounded decimal to binary  (appendix A.5)
    diag.ts  bytes.ts  diagnostics; growable output, bounds-checked input
    column.ts          the block codec                    ← column/
    wire/desc,narrow,wide          the two key widths, readers   ← wire/
    wire/narrowwrite,widewrite     the two key widths, writers
    plan.ts            a key, a name and an op — the hinge ← codec/codec.go
    section.ts         the schema section, both ways      ← codec/schema*.go
    infer.ts           §3                                  build.ts   §5
    walk.ts jsontext.ts message.ts   §6                   ← codec/json.go
    verify.ts          §4.5           inspect.ts          the field tree
    packed5.ts bitstream.ts          frozen; see §10
  tests/               node:test over the vectors, plus the fuzz target
  vectors/             Go: the generator, the oracle and the committed corpus
  src/                 the SvelteKit site (§9)
```

Every file under `assembly/` that ports a Go one names it, and is meant to be
readable side by side with it.

---

## 9. The page

Left: examples. Centre: the JSON. Right: the message.

The **field inspector is the point** — anyone can be told "three times smaller";
watching `name` eat 38% of the payload is what makes the format legible. It is
driven by `inspectMessage`, so it is also the decoder's own test: **the spans
tile the body exactly**, and if they do not, something read a field differently
from the way it was written.

### The examples are chosen to be believed

They cover §3 and §4 rather than flattering the format, and the honest cases are
the ones that took work to keep honest. The first version shipped five examples
colbin *loses* on. Under the out-of-band schema none of them lose any more — a
lone object was 44 B against 35 B of JSON and is now eleven bytes, a 3.2× win,
because the section is sent once and is not in the message at all.

Deleting them would have been the wrong repair: the loss did not go away, it
*moved* to the standalone file, which still carries its schema. So the panel
states both, and marks the second when it is under one:

| example | exercises | as a stream | as one file |
|---|---|---:|---:|
| metric points, 200 | near-uniform deltas, the column codec's best case | 3.80× | 3.76× |
| products, 200 | the ordinary case | 3.48× | 3.44× |
| clients, 1000 | nine fields, a real mix | 2.78× | 2.78× |
| invoices | nesting, and a list under the table threshold | 2.63× | 1.52× |
| big integers | ids past 2^53 that `JSON.parse` corrupts | 2.04× | 1.49× |
| people | string-heavy, where the format has least to offer | 1.79× | 1.42× |
| one object | **a tie as a file**: a document that is mostly schema | 3.18× | 1.00× |
| mixed numbers | one `2.5` promoting a field to float64 | 1.69× | **0.79×** |
| `[1,2,3,4,5]` | framing with nothing to amortise it over | 1.38× | **0.58×** |
| nulls / missing keys | §6's dense property, visible | 3.25× | 1.21× |
| broken | `qty` a string in record 3 — §4.1 doing its job | refused | |

**One thing the page had to get right to stay honest.** The examples are
pretty-printed so they can be read and edited, and comparing colbin against
*that* would have flattered it by two or three times. The comparison is therefore
against the minified JSON, computed with a string-aware scanner rather than
`JSON.parse` + `stringify`, since parsing would round any integer past 2^53 and
that is the failure the page exists to show.

Plain Svelte 5 runes and scoped CSS — no component library and no Tailwind. The
page is two panes, a tree and a hex dump; component-scoped CSS says that with one
less dependency and one less config file.

---

## 10. What is still open

**The packed string writer.** The module reads the opt-in encoding at both key
widths — which encoding a field used comes off the wire, so nothing is
configured — and writes raw.

That asymmetry is the right way round rather than an unfinished edge. `Packed5()`
is off by default in Go, so nothing an ordinary service sends is packed and a
module that writes raw interoperates with everything; the *reader* is what a
caller cannot do without, because a service that turned the encoding on to save
bytes would otherwise be unreadable. The writer is the larger half — a greedy
tokeniser over five operand tables, a case-mode hoist, a number opcode, a raw
escape and a never-inflate fallback — and buys the module nothing it can use
today. `REFACTOR_PLAN.md` §4.5 is the longer version.

**The npm package.** `@ivanjoz/colbin`, which is the point of the port: a
JavaScript client of a Go service answering in colbin currently has no way to
read the response.

```js
import { decode, setSchema } from '@ivanjoz/colbin'
setSchema(section)                                  // once
const rows = JSON.parse(new TextDecoder().decode(decode(bytes)))
```

Which is why `assembly/` stays free of anything page-specific.

**Before the first deploy**, the part that lives outside the repository: a
`CNAME` record for `colbin` in the `un.pe` zone pointing at `ivanjoz.github.io`,
and *Enforce HTTPS* ticked once GitHub's DNS check passes. CI already runs
`actions/configure-pages` with `enablement: true` and ships `static/CNAME`.

---

## Appendix A — the measurements that settled a decision

### A.1 What the module costs

`node --experimental-strip-types tests/bench.mjs`, on the corpus this appendix
has always used: 1000 product records, seven fields, 107 KB of minified JSON.
Best of twelve, each on a fresh instance. This laptop throttles; the ratios are
the point.

| | time |
|---|---:|
| the scanner alone | 3.94 ms |
| parse + infer + build | **6.11 ms** |
| with the §4.5 self-check | 13.18 ms |
| decode | 2.18 ms |

Two things worth reading off it.

**The codec is not the cost; being schemaless is.** The scanner is a large share
of the encode on its own, and the two inference passes sit in front of the writer
as well. The exact split is not readable off the table above, because the scanner
can only be timed through the debug build's test exports while the encode is the
optimised one — so 3.94 ms is an upper bound rather than a term to subtract. What
it does establish is the shape: parsing and inferring dominate, which was true of
the first version too, and is why optimising the writer would buy very little.

**The self-check costs 116%**, near the "roughly one decode, so call it 2x" the
estimate assumed. On by default is a real price and a defensible one: it is the
difference between believing the encoder and checking it, and the very first
thing it caught was a genuine bug — a duplicate key encoding its first value
where everything downstream expected its last.

Both figures were nearly twice as bad when first measured, and the cause was the
same mistake in two places: a `String.UTF8.encode` and a `subarray` inside the
innermost loop, where a view is an allocation. Removing them took encode from
10.38 ms to 6.11 and the self-check from 12.5 ms of overhead to 7.1 — and changed
not one byte of output, which the vectors confirm.

### A.2 Size

**42.9 KB gzipped** (156 KB raw, 34.5 KB brotli) against the 50 KB budget, with
encode, decode, inference, the self-check and the inspector. The frozen string
files are unimported and cost nothing.

### A.3 Never narrowing integers costs nothing

| 1000-record field | declared `int64` | declared narrow |
|---|---:|---:|
| products fixture | 13164 B | 13164 B (int32/int8) |
| uniform random over the full int32 range | 4001 B | 4001 B (int32) |
| uniform random over the full int16 range | 2007 B | 2007 B (int16) |

Byte-identical. The column codec *declares* its width from the data rather than
deriving it from the element type, so the declared width survives only as a
decode-side truncation rule. Width inference is therefore not on the roadmap.

### A.4 What Go WASM would have cost

`GOOS=js GOARCH=wasm`, `-ldflags="-s -w"`, linking colbin + `reflect` +
`encoding/json`: **4.6 MB raw, 1.27 MB gzipped**. TinyGo cannot substitute —
colbin reaches `reflect` throughout. Against a sub-50 KB target, a 25× difference
on the thing that must load before anything happens.

### A.5 AssemblyScript's `parseFloat` is not correctly rounded

Found during the first port rather than before it, and it changed what the module
contains.

`2.2250738585072011e-308` sits exactly on the boundary between the largest
subnormal and the smallest normal — the value that has broken parsers before.
Go's `strconv` and JavaScript both read it as `000fffffffffffff`. AssemblyScript
returns `0010000000000000`: one ULP out, silently.

Not a corner this module can leave alone. "Numbers survive intact" is its central
claim (§2.1), and a float differing in the last bit from what every other JSON
reader produces is exactly the silent corruption the exact integer path exists to
prevent.

So `assembly/decimal.ts` converts exactly instead: the literal is held as a
rational of arbitrary precision, the binary exponent comes from bit lengths, and
the significand is the correctly rounded quotient, ties to even. No
power-of-five table, so nothing extra to carry; no floating-point step, so
nothing to round twice. The scanner keeps a fast path for the ordinary case — a
significand under 2^53 with |exp| ≤ 22, where one multiply is exactly rounded —
and this runs for the rest. Checked against Go over 527 number vectors: all
bit-identical.

### A.6 Where AssemblyScript's float *formatting* differs, and where it does not

Measured across the corpus rather than assumed. AssemblyScript's `toString`
implements JavaScript's rule, and `encoding/json`'s thresholds for giving up `f`
notation — below 1e-6, at or above 1e21 — are the same ones. So the format
selection, the shortest-round-trip digits and the exponent's spelling all come
out right by themselves.

Two differences are left, and both are real: AssemblyScript writes `1.0` where Go
and JavaScript write `1`, and `0.0` for a negative zero where Go writes `-0`.
Trimming the first is exact rather than a guess — it applies only in `f` notation
and only when every digit after the point is that one zero.
