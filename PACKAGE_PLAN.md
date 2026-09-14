# `colbin` on npm

**Status: built, not yet published.** Every phase in §11 is done. The package
is `js/` — `js/src/{core,materialize,index,node,asset,errors}.ts` — it builds
`rust/wasm` with cargo and copies the artifact in, the demo site consumes it
from the workspace exactly as a stranger would from npm, and `bun run test`
ends by packing a tarball, installing it outside the repository and importing
every entry from a file that has never seen this checkout.

What is left is the one manual step §10 always described: `npm publish` of
`0.1.0` by hand, because npm's trusted publishing needs the package to exist
before it can take over. The release job is written and waiting for it.

Read §5.5, §6.1 and §13 for where the implementation differs from the sketch
below and why. The string dictionary and the token tape (§12) remain out.

The goal, stated plainly: a browser client receives a self-describing colbin
message from a backend and turns it into a JavaScript object, as fast as that
conversion can be made to go.

**What this is optimising for, and what it is not.** The project's targets are
backend CPU, backend RAM, and transfer size. Client CPU and client RAM are
budget to *spend* in service of those, and spending more of them is an expected
and accepted outcome — so no decision in this plan trades away a backend or wire
win to save the client. What follows is narrower than that: given the bytes have
arrived, what is the cheapest way to turn them into objects. Comparisons against
`JSON.parse` appear as a reference point for reading the numbers, never as a bar
the design has to clear.

---

## 1. Why a git install does not work

`npm install github:ivanjoz/colbin` is a real thing npm supports, and it fails
here for four reasons. Three are worth fixing whether or not anything is
published.

1. **The package is not at the repository root.** npm clones the repository and
   looks for `package.json` beside `.git`. Ours is `web/package.json`. npm has no
   subdirectory support for git dependencies — that is a pnpm/yarn extension, not
   something a consumer on npm can spell. Fatal on its own.
2. **The `.wasm` is not in git.** `web/.gitignore` ignores `build` and
   `static/colbin.wasm`. npm *does* run `prepare` for git dependencies, so
   `"prepare": "cargo build …"` would work — and then every consumer needs a
   Rust toolchain and the `wasm32-unknown-unknown` target on install, which
   breaks under `--ignore-scripts` (CI's usual posture, and pnpm's default) and
   on any machine without rustup.
3. **There is no entry point.** No `main`, no `exports`, and the nearest thing to
   an API — `src/lib/codec.ts` — imports `$app/paths` on line 1 and fetches the
   module from a site-relative URL. It is page code, not a library.
4. **`"private": true`** blocks `npm publish` outright.

And a fifth that does not fail loudly: a git dependency has no semver. You pin a
commit or track a branch, and `npm update` on a branch ref moves under you with
no version to blame. For a pre-alpha format whose bytes are still changing, that
surfaces as corrupt data rather than a version error.

---

## 2. Where the client's time goes

All figures: Node 24 / V8, no network, best of seven runs of 200 warm
iterations. A thousand product records, seven fields, received self-describing.
Measured is pure client CPU from *bytes already in a `Uint8Array`* to *object in
hand*. Nothing here measures RAM.

| | wire | gzipped |
|---|---:|---:|
| minified JSON | 106 670 B | 12 124 B |
| colbin, self-describing | **30 475 B** | **3 250 B** |

| path to an object in hand | before §2.5 | after |
|---|---:|---:|
| colbin `decode` → UTF-8 JSON text in wasm memory | 1.708 ms | 0.81 ms |
| … + `TextDecoder` + `JSON.parse` = **object in hand** | **2.078 ms** | **1.20 ms** |
| *(reference: the same payload as JSON, `TextDecoder` + `JSON.parse`)* | *0.299 ms* | *0.30 ms* |

As throughput, which is the framing that stays comparable across payload sizes —
**MB/s of the JSON payload produced**, the same denominator on both paths:

| case | rows | JSON | colbin | `JSON.parse` | colbin→text | **colbin→object** | wire |
|---|---:|---:|---:|---:|---:|---:|---:|
| products | 100 | 10 KB | 3 KB | 314 MB/s | 98 | **74** | 22 |
| products | 1 000 | 104 KB | 30 KB | 319 MB/s | 128 | **87** | 25 |
| products | 10 000 | 1 051 KB | 297 KB | 294 MB/s | 130 | **94** | 26 |
| 8 ints | 1 000 | 72 KB | 5 KB | 321 MB/s | 142 | **92** | 6 |
| 8 ints | 10 000 | 799 KB | 46 KB | 317 MB/s | 134 | **98** | 6 |
| big ints | 1 000 | 84 KB | 1 KB | 360 MB/s | 215 | **132** | 2 |
| long strings | 1 000 | 103 KB | 90 KB | 867 MB/s | 156 | **130** | 113 |
| nested (not a table) | 1 000 | 93 KB | 38 KB | 185 MB/s | 45 | **37** | 15 |

The rate is flat across payload size — 100 rows and 10 000 rows decode at the
same MB/s — so this is steady-state throughput rather than a fixed cost being
amortised. The reference column is there to calibrate the others, not as a target.

### 2.5 Two fixes already landed, worth 2x

Profiling the decode (an `-O3` build with symbols, `--cpu-prof`) found the cost
was not where §2.2's shape sweep implied. It was **allocation**: 35% of decode
time was in `~lib/rt/tlsf` and `~lib/rt/itcms`, the allocator and the collector.

| | |
|---|---|
| `JSONSink#signed/unsigned` called `value.toString()` | a UTF-16 string allocated, walked back out a `charCodeAt` at a time, and left as garbage — **per integer** |
| `Walker` passed field names as `string` to `JSONSink#key` | `String.UTF8.encode` + `Uint8Array.wrap` — two allocations — then a byte-at-a-time escape, **per field per row** |

The second is the same mistake `plan.ts:138-145` records being found and fixed on
the *encoding* side, where it had cost 40% of encode. The decode path had it too,
and nothing caught it because every test asserts on bytes rather than on time.

Fixed by `Writer#writeDecimalU64/I64`, which formats straight into the output
buffer, and `Plan#keyRuns`, which renders `"name":` once per plan so a field
costs one `memory.copy` per row. Decode went **1.814 ms → 0.841 ms** on the
profiled build; allocator and GC fell from 35.5% to 15.5%. All 919 tests pass,
the Go cross round-trip passes, and the encoder's output is byte-identical —
only the decode path changed.

**What is left in the text path**, from the same profile, if it is ever worth
more: `JSONSink#byte` at 12.7% (one `ensure(1)` per structural byte — bulk-reserve
per row instead), `jsonString` at 12.3% (a byte-at-a-time escape scan — word-at-a-time
would suit the common all-safe run), `beforeValue` at 5.5% (an `Array<bool>`
push/pop per depth — a fixed stack would not allocate), and pre-sizing the output
`Writer` so a 107 KB document does not grow through seven doublings. Plausibly
another 1.5–2x. Most of it evaporates anyway under §5, which writes no text at
all — so none of it should be done before the §5.1 measurement says whether it
matters.

### 2.1 The boundary is not the problem

The instinct is to optimise the JS↔wasm communication. The numbers say that is
the wrong 18%:

| | |
|---|---:|
| 15 000 exported wasm calls (≈ every value and key in the payload) | 0.040 ms |
| build 1000 objects from columns, monomorphic object literal | **0.007 ms** |
| build 1000 objects, generic `o[key] = value` loop | 0.069 ms |
| 1000 short strings via `String.fromCharCode` over a memory view | 0.011 ms |
| 1000 short strings via `TextDecoder` per string | 0.064 ms |

Every crossing costs about 2.6 ns. Materialising the entire object graph costs
7 µs. The boundary and the object building together are a rounding error against
the 1.708 ms spent inside wasm.

### 2.2 Decode time tracks the text, not the binary

*(Measured before the §2.5 fixes; the absolute figures have since halved, but the
relationship is what this section is for and it is unchanged.)*

Decode time tracks **the JSON text the module writes**, not the binary it reads:

| shape | binary in | JSON out | decode | ms/KB in | ms/KB out |
|---|---:|---:|---:|---:|---:|
| 1000 × 2 ints | 1 202 B | 19 985 B | 0.472 ms | 0.402 | **0.024** |
| 1000 × 8 ints | 4 858 B | 73 764 B | 1.755 ms | 0.370 | **0.024** |
| 1000 × 2 short strings | 9 817 B | 23 781 B | 0.565 ms | 0.059 | **0.024** |
| 1000 × 4 big ints | 1 517 B | 86 001 B | 1.183 ms | 0.799 | 0.014 |
| 1000 × 2 long strings | 91 817 B | 105 781 B | 0.865 ms | 0.010 | 0.008 |
| products | 30 475 B | 106 670 B | 1.806 ms | 0.061 | 0.017 |

`ms/KB in` varies by **80x**; `ms/KB out` is pinned near 0.024 wherever numbers
are being formatted, and drops only for long strings, which are a `memcpy` with
no formatting to do. For reference, V8 writes the same 107 KB of text with
`JSON.stringify` in 0.180 ms — the AssemblyScript writer is **9.5x slower than
native at the one job that dominates decode**.

**The 9.5x is gone.** `web/tests/bench.mjs` now measures the module and V8 in the
same process on the same corpus, so the comparison is self-contained rather than
quoted:

| the same 1000 product records | |
|---|---:|
| colbin `decode` → 106 670 B of JSON text | **0.20 ms** |
| `JSON.stringify` of the same document | 0.14 ms |
| colbin `encode` — scan, infer, build | **1.11 ms** |
| `encode` with the self-check on | 1.88 ms |
| `JSON.parse` of the same text | 0.29 ms |

The writer is **1.4x** native, not 9.5x, which is the §2.6 rewrite rather than
anything in §2.5. That moves what §5 is worth: the text path's 0.20 ms plus a
`TextDecoder` and a `JSON.parse` is around 0.55 ms to an object in hand against
native JSON's 0.30, where §2 measured 1.20 against 0.30. The materialiser still
removes the parse — and should still end up *faster* than native — but it is
buying a 1.8x gap rather than a 4x one.

Encode is 3.9x `JSON.parse` and does considerably more than parsing: two passes
of inference over every record, then a write. The self-check is **about 70% on top** —
not the "roughly one decode" the first plan estimated, because it decodes, parses
the text that came out, and walks the two trees against each other.

### 2.3 The floor for a direct path

If the module hands over decoded values instead of text, here is what the
JavaScript side costs for 1000 rows. Every line is measured, not estimated.

**Strings**, laid out as one UTF-8 blob plus an offset table:

| | ASCII, 9 B each | 5 distinct, repeated | multi-byte |
|---|---:|---:|---:|
| `TextDecoder` per string | 0.0658 ms | 0.0692 ms | 0.1214 ms |
| **bulk `TextDecoder` once, then `substring`** | **0.0056 ms** | **0.0063 ms** | **0.0154 ms** |
| `String.fromCharCode` per string | 0.0566 ms | 0.0315 ms | — |
| dictionary: decode distinct values, reuse references | 0.0019 ms | 0.0021 ms | 0.0129 ms |

Decoding the whole blob in one call and slicing it with `substring` is **10–12x
faster** than decoding strings one at a time, because V8 slices rather than
copies. It is the single biggest lever on the JS side.

**Integers**, 1000 of them out of a column:

| | |
|---|---:|
| `Int32` low/high pair → `Number` (exact to 2^53) | **0.0011 ms** |
| `BigInt64Array` → `BigInt` | 0.0038 ms |
| `BigInt64Array` → `Number(BigInt)` | 0.0085 ms |

**Adding it up** for the products shape — four integer columns, two string
columns, one boolean, then the object literals from §2.1:

| | |
|---|---:|
| 4 integer columns | ~0.004 ms |
| 2 string columns, bulk-decoded | ~0.012 ms |
| boolean bitmap | ~0.001 ms |
| 1000 monomorphic object literals | 0.007 ms |
| boundary crossings | ~0.001 ms |
| **JavaScript side, total** | **~0.025 ms** |

Against 0.370 ms for `TextDecoder` + `JSON.parse` on the current output. The
remaining unknown is what the *wasm* side costs once it stops formatting text —
§5.1 measures it rather than guessing, but §2.2 bounds it: for `ints x2`, 1.2 KB
of binary produced 0.472 ms of work that was almost entirely decimal formatting
of 2000 integers, so the traversal and bit-unpacking underneath it are small.

### 2.4 The conclusion

**The JSON text intermediate is the cost, and it fights the format's own shape.**
colbin stores a table as columns of bit-packed integers. The current path goes
columns → formatted decimal text laid out as rows → re-parse → objects, where the
middle two steps exist only to hand V8 something it already knows how to turn
into objects.

So: **`unmarshal` must never produce JSON text.** The fast path decodes columns
into typed arrays in wasm memory, exposes them zero-copy, and builds the objects
in JavaScript. The measured JS side is ~0.025 ms; the plausible total is well
below today's 1.20 ms, and §2.5's 2x came only from taking allocations out of the
text writer without removing the text. The JSON-text path stays for the page, for
debugging, and for documents the fast path does not cover.

**Client RAM is explicitly spendable here**, which settles several choices that
would otherwise need arguing: columns are decoded eagerly rather than lazily,
generated row builders and decoded schemas are cached indefinitely per
connection, and the module is free to hold a larger linear memory rather than
reuse a tight buffer.

---

## 2.6 The decoder was rewritten in Rust, and it is built

Decode-only was what landed first, which is what a browser client of a Go
service needs. Encode followed in `RUST_WASM_PLAN.md`; the AssemblyScript
module is gone and the demo site runs on `rust/wasm`.

`rust/wasm` is a `cdylib` with its own workspace — Cargo only honours a
`[profile]` at a workspace root, and this one sets `panic = "abort"`, which would
break `cargo test` for every other member if it were hoisted. It depends on
`colbin` with `default-features = false`; nothing is duplicated, and LTO drops
what the ABI does not reach, which is how the encoder stays out of the module
without a second implementation to keep in step.

What had to be written, because the crate had the wire layer but no schema
layer: `plan.rs`, `section.rs`, `json.rs`, `walk.rs` — about 1 300 lines. The
crate went `no_std + alloc`, which cost one `std` feature gate and an
`use alloc::…` line per module.

| | AssemblyScript | Rust |
|---|---:|---:|
| module, gzipped | 46.4 KB | **34.8 KB** |
| products 1k | 128 MB/s | **430 MB/s** |
| products 10k | 131 | **434** |
| 8 ints 10k | 142 | **409** |
| big ints 1k | 213 | **559** |
| long strings 1k | 154 | **424** |
| nested 1k | 50 | **237** |

**2.6–4.8x faster and 25% smaller**, and — the part that was not predicted —
**faster than `JSON.parse`** on five of six shapes, on a payload 3.5x smaller on
the wire, while still going through the JSON-text intermediate §2.3 was written
to remove. §5's materialiser is now an optimisation rather than the thing that
makes this viable.

**The 34.8 KB is a snapshot of that day and should not be quoted for the module
now.** It predates `map[string]any` and the root envelope; the same decode-only
build measures **44.9 KB gzipped** by the time `RUST_WASM_PLAN.md` began, and the
default build — the encoder included, which is what the site loads — is 91.3 KB.
`RUST_WASM_PLAN.md` §6 has the measurement and the breakdown.

Three findings worth keeping:

**Rust's float formatter is 30.6% of the module** — `core::fmt`'s
shortest-round-trip machinery, larger than the whole wire layer. It is why the
module is 34.8 KB and not the 10–18 KB I guessed. Removing it needs a compact
Grisu/Ryū, which is a dependency or a hard thing to write exactly right, and
shortest-round-trip is not optional because the vectors compare float spellings.

**`opt-level` barely matters.** Through `wasm-opt -Oz`, `3`, `s` and `z` land
within 34 bytes of each other gzipped and within noise on throughput, so the
level is set to `3`. `wasm-opt` dominates the size outcome.

**A narrow unsigned column came back sign-extended in AssemblyScript.** The
column codec stores signed residuals and the width is derived from the type on
both sides, so a `u8` column read as `i8` and widened is wrong for any value
above 127. Unreachable from the JSON encoder — numbers always infer to 64-bit —
but reachable from a Go-written message with a `uint8` field. The Rust port
truncates to the op's width, which is what Go's `uint8(int8(v))` does, and
deleting the AssemblyScript module closed the bug by removing the file it lived
in.

---

## 3. Decisions taken

**The name is `colbin`,** unscoped. Free on npm, and it matches the Go module
path and the Rust crate. One name across the implementations.

**`rust/wasm` is the module, and the site becomes a consumer.** A thin package
beside `web/` with the `.wasm` copied in is a much smaller diff, and was
rejected because the copy step is a seam where a stale module ships and because a
package whose source lives in another directory has no tests of its own. Under
this layout the demo site imports `colbin` exactly as a stranger would, which
makes it the bundler integration test.

**Both inline and asset entries.** Inline base64 is the default — zero bundler
configuration, no runtime 404 mode. `colbin/asset` resolves the module with
`new URL('./colbin.wasm', import.meta.url)`, which Vite, webpack 5, Rollup and
Parcel all understand natively and which works unbundled in a browser too.

**`unmarshal` returns a real object, and this is now safe.** My first draft of
this plan had `decode` return a *string*, on the grounds that `JSON.parse`
destroys every integer past 2^53 — the exact failure colbin exists to prevent.
That constraint came entirely from having `JSON.parse` in the path. The
materialiser removes it: values arrive as i64 in a typed array, and the package
chooses `Number` or `BigInt` **per column**, using range information the column
header already carries. The fast path is therefore both faster *and* more correct
than the text path. `toJSONText()` stays available for callers who want the text.

---

## 4. Layout after the move

```
colbin/
  package.json            workspaces: ["js", "web"]   (new, root)
  rust/wasm/              the module — stays where it is, see below
  js/                     the package — publishes as `colbin`
    package.json
    src/
      core.ts             the Codec handle, and the ABI behind it
      materialize.ts      reads the value buffer, builds the objects, and
                          holds the per-schema generated row builders
      errors.ts           ColbinError
      shared.ts           what all three entries re-export
      index.ts            the module inline, as base64
      node.ts             the `node` condition: reads the .wasm beside itself
      asset.ts            new URL('./colbin.wasm', import.meta.url)
      generated/          the base64, written by the build, never committed
    tests/                moved from web/tests, plus package.test.mjs
    vectors/              moved from web/vectors (Go — generates the corpus)
    scripts/inline.mjs    the base64
    scripts/tarball.mjs   pack, install outside the repo, import every entry
    build/                the cargo artifact, gitignored
    dist/                 built, gitignored, shipped in the tarball
  web/                    the demo site, now `import { Codec } from 'colbin'`
```

**The module's source does not move into `js/`, and cannot.** That was written
when it was `assembly/` — a TypeScript directory with nothing but `asc` between
it and the `.wasm`. It is a Rust crate now, in the workspace every other Rust
consumer links, so `js/` builds it with `cargo` and copies the artifact in. That
is the copy step the decision above calls a seam, and this is the version of it
worth accepting: the seam is a `cargo build` in `prepack`, not a `.wasm` committed
in two places, and `js/tests/` runs against the copy rather than the source, so a
stale one fails the package's own tests rather than shipping.

Everything under `web/tests` resolves paths relative to `harness.mjs`, and
`web/vectors` is referenced only by its own files, so the move is mechanical.
Edited by hand: `web/package.json` scripts, the `working-directory: web` steps
and `go run ./web/vectors` paths in `.github/workflows/ci.yml`.

The root `package.json` is a workspace manifest only — `private: true`, never
published. `web/` gets `"colbin": "workspace:*"`.

---

## 5. The fast path

### 5.1 Calibrate the wasm side first

§2.3 measures the JavaScript side at ~0.025 ms. The wasm side is the one number
nobody has. Before §5.2–5.4 is built out, a throwaway sink that writes i64s into
a flat buffer instead of formatting decimal text answers it: **what does the walk
plus the column unpacking cost on its own?**

This is calibration, not a gate — the direction is already settled by §2.2, since
formatting text is provably most of the current cost and not formatting it cannot
be slower. What the number changes is *where the effort goes next*. If the wasm
side lands near 0.05 ms the work is done and §12's dictionary is the next lever.
If it lands near 0.5 ms, the column unpacking itself is worth attention and that
is a different piece of work from anything else in this plan. Measuring first is
how that gets decided on evidence instead of after the fact.

### 5.2 What changes in wasm

Less than it sounds, because the refactor already put the seam in the right
place: `walk.ts` is **sink-driven**, and `JSONSink` is one implementation. The
work is a second sink, not a second decoder. `column.ts`'s `decodeArray` already
produces exactly what is wanted — a contiguous run of i64 — and the JSON writer
is what is bolted on top of it.

A new export `materialize(len) -> i32` writes one buffer into wasm memory:

```
header   [version][rootKind][rowCount][fieldCount][blobOffset]
shape    per field: [nameOff][nameLen][type][flags][dataOff][nullBitmapOff]
data     per field, contiguous and aligned:
           int    -> i64 run          (+ a flag: does any value exceed 2^53)
           float  -> f64 run
           bool   -> bitmap
           string -> u32 offset table into one UTF-8 blob
```

`flags` carries optional/absent, whether any integer exceeds 2^53, and whether a
string blob is pure ASCII — the two bits §5.3 needs to pick `Number` over
`BigInt` and `substring` over `TextDecoder`. `nullBitmapOff` is zero when a field
has no nulls, which is the common case and lets the row builder skip the check
entirely.

### 5.3 What the JavaScript does

Read the header and shape once. Take zero-copy views over the data runs. Then
build the rows with a **generated** builder, because §2.1 shows the monomorphic
object literal beats the generic loop by 10x:

```js
// generated once per schema, then cached
(c0, c1, c2, n) => { const out = new Array(n)
  for (let i = 0; i < n; i++) out[i] = { id: c0[i], sku: c1[i], active: c2[i] === 1 }
  return out }
```

Four things this has to get right.

**Strings are decoded in bulk and sliced.** §2.3 measures one `TextDecoder` call
over the whole blob followed by `substring` per value at 10–12x the per-string
decode. There is a correctness trap in it: `substring` indexes **UTF-16 code
units**, while the offset table naturally holds **byte** offsets, and the two
agree only while the blob is ASCII. So the materialiser emits a per-column
`ascii` flag — free, since it is copying the bytes anyway — and the builder uses
`substring` when it is set and falls back to per-string decode when it is not.
The non-ASCII path must be covered by a vector, because an all-ASCII test corpus
would never reach it and the bug it hides is silently wrong strings rather than a
crash.

**i64 without paying for BigInt.** Reading a `BigInt64Array` allocates a BigInt
per element. Instead the run is read as `Int32Array` pairs and recombined as
`lo + hi * 4294967296` — 0.0011 ms per 1000, against 0.0038 ms for BigInt — exact
to 2^53. The per-field flag from §5.2 says whether any value exceeds it; only
then does the builder emit `BigInt`, and only for that field. The encoder already
computes this: the column codec's frame-of-reference transform derives a minimum
and a bit width, so the range is known without a second pass.

**`new Function` is not always allowed.** A page under a Content-Security-Policy
without `unsafe-eval` cannot compile a generated builder. The package must detect
this once and fall back to the generic loop, which costs 0.069 ms instead of
0.007 ms — still two orders of magnitude below the decode. The fallback must be
tested, not merely written, since the fast path is what every local test would
otherwise exercise.

**Builders are cached by schema.** Compiling one costs more than using it. With
an out-of-band schema there is one per connection; with a self-describing message
the cache is keyed on the section bytes, so a backend answering the same endpoint
repeatedly compiles a builder once.

### 5.4 What the fast path does not cover

Deeply nested or heterogeneous documents. The column materialiser wants a table —
which is the shape the stated use case sends, and the shape colbin is for. A
general flat token tape covering arbitrary documents without text is a natural
follow-on and is deliberately out of the first version. Until then anything the
materialiser declines falls back to the JSON-text path automatically, and
`codec.lastPath` reports which ran, so a caller can find out they are on the slow
one without guessing.

### 5.5 What actually shipped, and where it differs from the sketch

§5.1–5.4 above describe the design; `rust/src/materialize.rs` and
`js/src/materialize.ts` are the implementation, and `cargo test` and
`bun run test` both pass against it. Four differences from the sketch, all
deliberate:

**No `Sink` trait.** The Rust decoder was never refactored to make `Walker`
generic over a sink the way the old AssemblyScript `walk.ts` was — that would
have been a large change to well-tested code for a feature that only needs a
fraction of what `Walker` does. `materialize.rs` is a small, independent
function that reuses `walk.rs`'s column-gather step (`Gathered`,
`read_column`/`read_column8`, bumped to `pub(crate)`) rather than the sink
abstraction, since a table row can only hold a columnable op — no recursion, no
null bitmap, nothing the general walk needs that a table does.

**The buffer has no offset/length table.** Every run's byte length is derivable
from `rowCount` and the field's own kind (a bitmap is `ceil(rows/8)` bytes, an
int or float run is `rows*8`, a string run's blob length is its own offset
table's last entry), so the header carries only `[kind][flags][nameLen][name]`
per field — no `dataOff`/`nullBitmapOff` to keep in sync with the data.

**Scope is narrower than "a table": one field, at the root.** The covered shape
is a root wrapped in the one-field envelope (`Plan::is_envelope` — the top
level was a bare array or scalar, not an object) whose field the wire encoded
as a table. A named field holding a table inside a larger object (`{"rows":
[...]}`, ENCODER.md's own `records-past-the-table-threshold` fixture) is
**not** covered and falls back to JSON text — broadening this to any one-field
struct regardless of envelope was considered and rejected, because a
non-enveloped one-field struct's JSON is `{"name": [...]}`, not a bare array,
and returning bare rows for one shape and a wrapped object for the other would
make `unmarshal`'s return type shape-dependent in a way the JSON fallback path
does not need to be.

**`codec.lastPath` after all.** The first version of this returned
`path: 'materialize' | 'json'` on each call's result, because there was no
handle for a persistent flag to live on. There is one now, so the package has
the field §5.4 asked for; the page's wrapper still turns it back into a field
on its own result, since that is what its `Outcome` union wants.

---

## 6. The API

```ts
import { Codec } from 'colbin'

const codec = await Codec.open()              // compiles the module, once
const rows = codec.unmarshal(bytes)           // self-describing -> objects
```

```ts
// the out-of-band delivery: a section sent once per connection
codec.setSchema(section)
const rows = codec.unmarshal(bytes)
```

`Codec.open()` is async because compiling WebAssembly is; there is no synchronous
escape, since `new WebAssembly.Module` is blocked on the main thread above 4 KB
and the module is 244 KB. An earlier plan sketched a synchronous `decode`; that
sketch is not achievable, and the sketch went with the document.

**Everything after `open` is synchronous**, which was not planned and is what
the shape turned out to allow: the compile is hoisted into `open`, and
`new WebAssembly.Instance` over an already-compiled module needs no `await`. So
`unmarshal` returns rows rather than a promise of them. §6.1 is what that
settles.

| | |
|---|---|
| `Codec.open(opts?)` | compile + instantiate; `opts.module` takes a pre-compiled `WebAssembly.Module`, `opts.wasm` takes bytes, a `Response` or a URL |
| `Codec.fromModule(module)` | another handle over a module compiled already |
| `Codec.preload()` | start the compile without waiting for it |
| `codec.unmarshal(bytes, opts?)` | **the headline** — objects, via §5, no JSON text |
| `codec.columns(bytes, opts?)` | the columns themselves, for a caller feeding a grid or a chart that never wants row objects; `null` when the message is not that shape |
| `codec.toJSONText(bytes, opts?)` | the text path, unchanged |
| `codec.marshal(value \| jsonText, opts?)` | `{ message, section, standalone, warnings }` |
| `codec.setSchema(section \| null)` | hold a section for what follows; `null` clears |
| `codec.inspect(bytes, opts?)` | the field tree with byte spans — needs the `colbin/inspect` module (§7) |
| `codec.lastPath` | `'materialize'` or `'json'` — §5.4's "which one ran" |
| `codec.canInspect` / `canMaterialize` | which exports this handle's module actually has |
| `unmarshal` / `marshal` / … | the same without a handle, each `await`ed |
| `ColbinError` | `code`, `offset`, `line`, `path`, `message`, `warnings` |

`codec.columns()` is worth having for its own sake: a client rendering a table or
a chart wants columns, and handing back the arrays skips even the 7 µs. Float and
bigint columns are views straight over the buffer; a bitmap, a UTF-8 blob and a
pair of i32 lanes are not things a chart can read, so those three are built — the
cheap end of §2.3, and still less work than the row build this skips.

`marshal`'s `standalone` is **composed, not encoded twice**: the self-describing
form is the same body behind the same section with bit 2 of the root byte set, so
it is assembled on first read. `js/tests/package.test.mjs` asserts that against
what the module writes for `SELF_DESCRIBING` across the whole corpus, which is
what makes the shortcut checkable rather than merely plausible.

`marshal`'s self-check is **off** by default, where the page has it on: it costs
about 65% on top of the encode and what it catches is a codec bug rather than a
caller's mistake. `{ verify: true }` turns it on, and the README says when to.

Failure is a thrown `ColbinError` carrying every diagnostic field, rather than an
`Outcome` union — a library consumer's normal path is success, and the page keeps
its own wrapper. A *successful* marshal can still carry warnings (an always-null
column, a null where an object belongs), so warnings ride on the result.

### 6.1 The handle, and the measurement that chose it

`codec.ts` used to instantiate a fresh module per operation, justified by a
claim that a shared instance "would grow without bound". That was not true —
2000 decodes on one held instance settle at 32 pages (2 MB) and stay flat,
because the incremental runtime collects. Held is 1.73 ms against 1.97 ms
fresh, and instantiation alone is 0.062 ms, so speed was not the argument
either.

The argument is semantic: the format's recommended delivery is **a schema sent
once per connection**, and a fresh instance per call re-parses the section every
time and throws away the cached row builder with it. A handle is the shape that
delivery already implies.

**The pool is gone, and the reason it existed went with it.** What follows is
kept because it is the measurement that chose the handle, and because it
explains what the package does *not* need. `js/src/core.ts` holds **one**
instance per `Codec`, because `Codec.open()` hoists the compile out of the
calls: `new WebAssembly.Instance` over an already-compiled module is
synchronous, so every operation on the handle — `unmarshal`, `marshal`,
`inspect` — is a straight run of wasm calls with no `await` in it. A call that
cannot be preempted between writing its input and reading its output cannot
interleave with another one, so `Promise.all([codec.unmarshal(a),
codec.unmarshal(b)])` is safe by construction rather than by inspection. The
pool below existed only because that file compiled lazily *inside* each call,
which is the one thing that put an `await` in the middle. Holding the section
across messages — the semantic argument above, and the one that actually
matters — is what the single instance is for: §6's `setSchema` writes it into
the instance once and the identity check in `Codec.#schema` keeps every message
after it from re-parsing it.

**It was implemented as a pool first.** `web/src/lib/codec.ts` held a free-list
of instances (`acquire`/`release`) rather than one handle,
because every call here is a single synchronous stretch of wasm calls between
its one `await compile()` and its return — JS never preempts that stretch to
interleave a second call's writes into the same instance's `INPUT` — but two
calls fired together (`Promise.all([unmarshal(a), unmarshal(b)])`) still need
two *separate* instances the moment either has to wait on `compile()` for the
first time. A pool converges to size one under sequential calls (which is what
§6.1's measurement above is about) and grows only as far as real concurrent
demand requires, with no cap — consistent with §2.4's "client RAM is
spendable." `preload()` now also pushes one warm instance into the pool
alongside compiling the module, so the first real call finds one waiting.

That was structural safety rather than safety by construction: nothing in
`encode`/`decode`/`unmarshal`/`inspect` awaited between writing input and
reading output, which is what made a single instance safe to share even then.
The package makes the same invariant hold by construction, by having nothing
left in a call that *could* await. The demo page still fires `inspect` and
`unmarshal` together with `Promise.all` on every encode (§13), so
`web/tests/browser.mjs` drives two genuinely concurrent calls through the one
handle on every one of its encodable examples, in a real browser — which is now
a test of the handle rather than of a pool.

---

## 7. Build and packaging

`cargo` compiles `rust/wasm` as `web/` does now. The wrapper is TypeScript
compiled by `tsc` to ESM plus declarations — no bundler in the package's own
build, so `dist/` stays readable.

```
wasm      cargo build --release --target wasm32-unknown-unknown
          + wasm-opt -O3                           -> build/colbin.wasm
          the same again --features inspect        -> build/colbin.inspect.wasm
inline    scripts/inline.mjs                       -> src/generated/wasm-inline.ts
tsc                                                -> dist/*.js + dist/*.d.ts
copy      build/*.wasm                             -> dist/
```

A publisher therefore needs a Rust toolchain and the `wasm32-unknown-unknown`
target, which a *consumer* never does — the tarball ships the built module, and
§1's third reason is why that is not negotiable.

`scripts/inline.mjs` emits one base64 string, decoded at runtime with `atob`. It
is gitignored and regenerated by `prepack`, so the base64 cannot drift from the
`.wasm` beside it.

```json
"exports": {
  ".": { "types": "./dist/index.d.ts", "node": "./dist/node.js", "default": "./dist/index.js" },
  "./asset": { "types": "./dist/asset.d.ts", "default": "./dist/asset.js" },
  "./inspect": { "types": "./dist/inspect.d.ts", "default": "./dist/inspect.js" },
  "./colbin.wasm": "./dist/colbin.wasm",
  "./colbin.inspect.wasm": "./dist/colbin.inspect.wasm"
}
```

Plus `"files": ["dist", "README.md", "LICENSE"]`, `"sideEffects": false`,
`"type": "module"`, `"license": "MIT"`, `"repository"`,
`"engines": { "node": ">=18" }`, and `"./package.json"` in the exports map so a
tool can read it. No `prepare` script — the tarball ships built, and a git
install is explicitly unsupported. `prepack` runs the build, so a publish
cannot ship a stale `dist/`.

**Measured, and the guess was wrong.** Inline against asset, on the modules the
package ships today:

| | raw | gzipped |
|---|---:|---:|
| `colbin.wasm` — `colbin/asset`, and the `node` entry | 209 109 B | **84 195 B** |
| the same as inline base64 — the default entry | 279 124 B | **114 271 B** |
| `colbin.inspect.wasm` — `colbin/inspect` | 224 199 B | 90 127 B |
| the wrapper's own JavaScript, every entry | 36 085 B | 10 651 B |
| the tarball as published | 422 KB | — |

Base64 costs 33% raw and gzip recovers *less than half* of it, not "most":
**+36% gzipped**, because base64 destroys the byte alignment gzip's matcher
works on. Inline stays the default for the reason §3 chose it — no
configuration, no 404 mode — but the README says the number and points a
transfer-sensitive consumer at `colbin/asset`, which is what the demo site
itself imports.

**Two modules, not one, and `wasm-opt` on both.** Two things came out of asking
where 253 KB of WebAssembly was going (`RUST_WASM_PLAN.md` §6.1 has the
breakdown by subsystem):

*`inspect` has its own cargo feature and its own `.wasm`*, because the span
walk is 15 KB — 6.6% of the module's code — that only a tool drawing the bytes
ever calls, and every consumer was downloading it. `colbin` and `colbin/asset`
carry the build without it; `colbin/inspect` carries the build with it, and the
demo site imports that one. `codec.canInspect` says which a handle is over, and
`codec.inspect()` on a handle that cannot throws an error naming the entry that
can. `js/tests/package.test.mjs` asserts the published module has no
`inspect_message` export, so the saving cannot quietly evaporate.

*`wasm-opt -O3` runs over both*, as a tool rather than a dependency —
`js/scripts/optimize.mjs` takes it from the PATH, warns when there is none, and
CI installs binaryen and sets `COLBIN_REQUIRE_WASM_OPT=1` so a published module
cannot be an unoptimised one. It is worth 12% raw and 4% gzipped, and an A/B
through `bench.mjs` found no throughput difference between the two modules.
`-O3` rather than `-Oz`: `-Oz` wins 1.5 KB raw and gives it back gzipped. No
`--enable-*` flags are passed, because `rust/wasm`'s profile keeps rustc's
`target_features` section alive for `wasm-opt` to read — a hand-kept flag list
measured the same and would go stale on a toolchain upgrade.

Together those take the module a consumer downloads from **93 409 B gzipped to
84 195 B**, without removing anything a consumer calls.

A build step worth naming: `bun run dist` is the build *without* cargo, taking
whatever is already at `build/colbin.wasm`. That is what the release job runs
after downloading the `.wasm` artifact the tests ran against, so the module on
npm is the module that was tested rather than a second compile of the same
commit.

---

## 8. Versioning

All three implementations must agree on the bytes, so **one tag drives all
three**: `v0.1.0` publishes `colbin@0.1.0`, and is the Go module tag and the
crate version. While on `0.x`, compatibility is by matching **minor** version,
stated plainly in the README:

> colbin 0.x makes no wire-compatibility promise across minor versions. A Go
> service on 0.3 and a browser on 0.2 will not interoperate, and the failure will
> look like corrupt data rather than a version error. Pin both.

A format version byte on the wire is the honest fix and is out of scope here: the
root byte range is already pinned (`0xD0`–`0xDF`, the rest reserved for
applications) and spending one of those is a format decision, not a packaging
one. Worth raising separately.

First publish is **`0.1.0`**.

---

## 9. Testing

The existing suite moves and keeps running unchanged: 919 module tests against
vectors generated from the Go codecs, the cross round-trip where the module
encodes and Go decodes, the fuzz and corruption sweep. Four things are added.

**The materialiser against the text path.** Every vector is unmarshalled both
ways and the results compared. This is the test that matters most, because the
two paths share a walker but not a sink, and a divergence is a silent wrong
answer rather than a crash. Integers past 2^53 get their own cases, where the two
paths are *expected* to differ — the text path rounds, the fast path does not —
so the comparison is against the Go value, not against each other.

**The CSP fallback.** The generic row builder runs the same vectors, forced on.

**The tarball, as a consumer receives it.** `npm pack`, install into a temporary
directory, import all three entries from a file that has never seen the
repository. This catches a missing `files` entry, an `exports` map that resolves
to nothing, a relative import that worked from `src/` and not from `dist/`.

**The site is the bundler test.** Once `web/` imports `colbin` from the
workspace, `bun run build` exercises the exports map through Vite and
`tests/browser.mjs` drives eleven examples against the published shape. That is
the reason §3 chose the larger move, and the CI step's comment should say so, so
nobody later "simplifies" it back to a relative import.

---

## 10. CI and publishing

The workflow already does most of this: `release` runs on `v*` tags and uploads
`colbin.wasm` from the artifact the build job produced, so the bytes released are
the bytes the tests ran against. npm publish belongs in that same job for the same
reason.

- ~~**Build job** gains the tarball smoke test~~ **done**, and inside
  `bun run test` rather than as a step of its own, so a local run gets it too.
- ~~**Build job** gains a benchmark step~~ **done**: `bun run bench:check`.
  The threshold is a *ratio* against `JSON.parse` measured in the same process
  on the same corpus, never a millisecond figure — a shared runner's absolute
  speed varies by more than any regression worth catching, and the ratio does
  not. It sits at 1.5x, far above where the number lands (0.55x), because the
  job is to catch the JSON-text intermediate creeping back into `unmarshal`,
  which would cost 3-4x, and not to litigate a few percent.
- ~~**Release job** gains `npm publish`~~ **done**, publishing the `.wasm`
  artifact the build job tested rather than compiling again: the job downloads
  it, runs `bun run dist` over it, and publishes with `--ignore-scripts` so
  `prepack` cannot quietly rebuild it. It also refuses a tag whose version does
  not match `js/package.json`.
- **Trusted publishing** (npm's OIDC flow for GitHub Actions) rather than an
  `NPM_TOKEN` secret — no long-lived credential, and provenance is stamped
  automatically. It requires the package to exist first, so `0.1.0` is published
  by hand once and every tag after that is automatic. That manual step is
  deliberate, not an oversight.

---

## 11. Phases

| | | ends when |
|---|---|---|
| 0 | ~~Move `tests/`, `vectors/` into `js/`~~ **done.** `js/` builds `rust/wasm` with cargo and copies the artifact in; the root manifest makes `js/` and `web/` one Bun workspace. | every existing test passes from the new paths; `bun run build` and `tests/browser.mjs` still green |
| 1 | ~~**Calibration** (§5.1)~~ **done, skipped straight to the real thing.** The direction was already settled by §2.2 and §2.6's Rust rewrite, so no throwaway sink was built — `materialize.rs` was written directly and measured against `bench.mjs` instead. | wasm-side `materialize` measured at 0.16 ms against `decode`'s 0.20 ms and native `JSON.parse`'s 0.26 ms on the products corpus |
| 2 | ~~`rust/src/materialize.rs` and the `materialize` export~~ **done** — the value function beside `walk`, behind its own `materialize` feature (default-on, independent of `encode`). Scoped to §5.5's one-field-envelope-root shape rather than every table. | `rust/tests/materialize.rs`: every table-shaped corpus case decodes to what Go's JSON says, plus a hand-built >2^53 case, in `cargo test` |
| 3 | ~~`src/` — the `Codec` handle~~ **done**, now in `js/src/`: `core.ts` is the handle and `ColbinError` is `errors.ts`; the materialiser reader and the generated row builders with the CSP fallback are `materialize.ts`. The handle's operations are synchronous (§6.1). | `unmarshal` returns objects equal to the text path over every table-shaped vector (`js/tests/materialize.test.mjs`), and `bench.mjs` records the comparison |
| 4 | ~~The three entries, `scripts/inline.mjs`, the `exports` map, package metadata~~ **done.** | all three entries unmarshal the same message identically (`js/tests/package.test.mjs`) |
| 5 | ~~Tarball smoke test, wired into `bun run test` and CI~~ **done** — `js/scripts/tarball.mjs`, installed with `--ignore-scripts` because that is CI's and pnpm's posture and §1's second reason says it has to work under it. | `npm pack` → install → import works outside the repository |
| 6 | ~~`web/` consumes `colbin` from the workspace~~ **done**, through `colbin/asset`, so Vite resolving the exports map and emitting the `.wasm` is checked on every build. `src/lib/codec.ts` is now only the page's `Outcome` wrapper — 150 lines against 340. | the page runs every example through the package; `tests/browser.mjs` green with no console errors |
| 7 | ~~README~~ **done** (`js/README.md`, and the root README points at it); ~~`npm publish` in the release job~~ **done**. **Left: publish `0.1.0` by hand and enable trusted publishing** — npm's OIDC flow cannot claim a name that does not exist yet. | `npm install colbin` works from a clean machine |

Phase 0 was large and mechanical and landed alone — a move that size is
unreviewable mixed with new code. Phase 1 was meant to gate phases 2 and 3; it
did not need to, because §2.6's rewrite had already settled the direction.

---

## 12. What stays out

**A general token tape** for nested and heterogeneous documents (§5.4). The
column materialiser covers the stated use case; the tape is the follow-on that
retires the text path entirely.

**A string dictionary.** `enc` code 2 is reserved across all three
implementations and written by none of them. It is the largest remaining lever
and it pays on every axis this project cares about: the products corpus repeats
five city names 200 times each, so a dictionary shrinks the wire, cuts backend
encode work, and on the client decodes five strings instead of a thousand —
0.0021 ms against 0.0063 ms in §2.3, with 1000 shared references instead of 1000
string objects in RAM. It is a format change rather than a packaging one, so it
belongs with the §8 version byte, but it should be the next thing after this
plan.

**Typed output from the schema.** `unmarshal<T>(bytes): T[]` with types generated
from the section is a real idea and a much larger one.

**Streaming.** The ABI is bytes-in, bytes-out and allocates the whole input. A
message that does not fit in memory is out of scope for 0.1.

**A worker entry.** Decoding off the main thread is the caller's choice; shipping
a wrapper means owning a message protocol.

**The DNS work** — a `CNAME` for `colbin` in the `un.pe` zone and *Enforce
HTTPS* — is unrelated and still outstanding.

---

## 13. Open follow-ups from what's built

Tracked here rather than left implicit, now that the whole of §11 is built:

**Left: publish `0.1.0` by hand.** Everything else in phase 7 is done, and
this one step cannot be automated — npm's trusted publishing takes over a name
that already exists, so the first version has to be pushed from a machine with
a login. The release job is written against that assumption and refuses a tag
whose version does not match `js/package.json`.

**Done: the demo site calls `unmarshal`, concurrently with `inspect`.**
`web/src/routes/+page.svelte` fires both with `Promise.all` on every encode,
and a "fact" in the results panel names which path ran (`materializer` or
`JSON fallback`) with a timing. `web/tests/browser.mjs` asserts the fact
renders for every encodable example, and specifically that `Products, 200
records` (a bare-array table) takes the materializer while `Integers past
2^53` (three records, under the table threshold) falls back to JSON — through
the package, in a real browser, with two concurrent calls on the one handle.

**Done: the CI benchmark gate** (§10). `js/tests/bench.mjs --check` fails the
build if `unmarshal` lands above 1.5x `JSON.parse` on the products corpus; it
measures 0.14 ms against `JSON.parse`'s 0.26 and `decode`'s 0.20 — materialize
is faster than native JSON, not just closer to it.

**The encoder is half the module, and no entry drops it.** The lean build a
browser client of a Go service actually wants — decode plus the materializer,
no JSON parser, no inference, no self-check — is **45 335 B gzipped** against
the published module's 84 195. That is a bigger saving than everything §7's
two-module split and `wasm-opt` won together, and it is entirely a packaging
decision: the feature already exists (`--no-default-features --features
materialize`) and is already built and tested in CI. What it needs is an entry
(`colbin/reader`, say), a third artifact in the tarball, and an answer to what
`marshal` should do on a handle that cannot encode — `#needs` already throws
the right shape of error, so the answer may simply be "that".

**The convenience functions are not tree-shaken out of the site's bundle.**
`export const { unmarshal, ... } = convenience(Codec)` in each entry is a
destructuring initialiser, and Rollup keeps it even under `sideEffects: false`
because it cannot prove the call does nothing. It costs a few hundred bytes in
a page that only ever uses the `Codec` handle. Worth fixing if the surface
grows; not worth a lazier shape than one call while it is this small.

**Constant/all-absent columns materialize as full arrays, not a flag.** A
column the message never wrote (every row holds the field's zero value) is
written out as a full array of that zero value rather than a single "this
column is constant" marker the column codec itself already has a concept of.
Deliberate for now, per §2.4's "client RAM is spendable," but worth revisiting
if a very wide or very sparse table shape ever makes it matter.

**Everything in §12 stays out**, unchanged by any of the above — the token
tape, the string dictionary, typed output, streaming and a worker entry are
all still just this document. The package itself no longer is.
