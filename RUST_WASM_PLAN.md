# One browser module, and it is the Rust one

**Status: done.** The demo site and the bun tests run on `rust/wasm`.
`web/assembly/` is gone, the encoder's rules moved to `rust/ENCODER.md` rather
than going with the documents that stated them, and §6 records what the module
actually measures — which is not what the budget said.

The goal, stated plainly: delete `web/assembly/` and have the demo site — and
the npm package `PACKAGE_PLAN.md` describes — run on `rust/wasm`, which is the
same crate every other Rust consumer links.

Two implementations of one format have to be kept in step by hand, and this
repository has spent real effort doing that: a vector corpus in each direction,
a CI step per direction, and a standing rule that a Go-side change fails on the
Go side's own push. That discipline is what made the AssemblyScript module
trustworthy. It is also the cost this removes.

---

## 1. Half of it is already done, and it won

`PACKAGE_PLAN.md` §2.6 rewrote the decoder in Rust and measured it against the
module it replaces:

| | AssemblyScript | Rust |
|---|---:|---:|
| module, gzipped | 46.4 KB | **34.8 KB** |
| products 1k | 128 MB/s | **430 MB/s** |
| nested 1k | 50 | **237** |

**2.6–4.8× faster and 25% smaller**, and faster than `JSON.parse` on five of six
shapes. There is nothing to reconsider about the decode path; `walk.rs`,
`section.rs`, `plan.rs` and `json.rs` landed with it, and `rust/tests/dynamic.rs`
has since added `map[string]any` to what it reads.

What §2.6 deliberately left was the other half: *"encode stays in AssemblyScript
until someone wants it in the browser."* This plan is someone wanting it.

---

## 2. Per-file verdict

`web/assembly/` is about 6 400 lines across seventeen files. Roughly 3 000 are
fully covered in Rust already and another 700 partly:

| AssemblyScript | lines | Rust | |
|---|---:|---|---|
| `walk.ts` | 851 | `walk.rs` | ✅ |
| `packed5.ts` | 700 | `packed5.rs` | ✅ |
| `column.ts` | 418 | `column.rs`, both directions | ✅ |
| `jsontext.ts` | 410 | `json.rs` | ✅ |
| `plan.ts` | 235 | `plan.rs` | ✅ |
| `bytes.ts`, `message.ts` | 358 | `wire/` | ✅ |
| `section.ts` | 412 | `section.rs` — **parse only** | ⚠️ |
| `index.ts` | 182 | `wasm/src/lib.rs` — **decode only** | ⚠️ |
| `diag.ts` | 121 | the `fail` envelope in `lib.rs` | ⚠️ |
| **`json.ts`** | 635 | — the JSON *parser* | ❌ |
| **`inspect.ts`** | 601 | — the field tree and its byte spans | ❌ |
| **`infer.ts`** | 560 | — type inference | ❌ |
| **`build.ts`** | 507 | — the plan-driven encode | ❌ |
| **`verify.ts`** | 349 | — the encode self-check | ❌ |
| **`decimal.ts`** | 293 | — *free in Rust, see below* | ❌ |
| `test-exports.ts` | 300 | n/a | |

**`decimal.ts` disappears rather than ports.** It exists for one reason, written
down in `web/PLAN.md` §A.5: AssemblyScript's `parseFloat` is not correctly
rounded, and "numbers are exact" is the module's central claim. Rust's
`str::parse::<f64>` *is* correctly rounded. 293 lines of arbitrary-precision
rational arithmetic become one call.

So the real port is **about 2 800 lines of logic** — json 635, inspect 601, infer
560, build 507, verify 349, plus a section writer of perhaps 150 — which should
land in rather less Rust than that, given the wire writers, the column encoder
and the packed5 codec are all already there.

---

## 3. Three things that make this bigger than it looks

**`web/assembly/` holds the only JSON→colbin encoder in the repository.** Go has
none — the README says so outright, and "it needs type inference, and it is a
separate job" is the whole entry. The Rust crate has only the derive encoder,
which needs the type at compile time. Deleting the module without porting would
remove a capability that exists nowhere else, and it is precisely what the demo's
headline does: type JSON, watch it shrink.

**AS is load-bearing for the *Rust* test suite today.** `rust/tests/section.rs`
and `rust/tests/walk.rs` both read `web/vectors/web_encoded.json`, which only the
AssemblyScript module writes, and `web/vectors/encoded_test.go` reads it too. So
the module cannot simply be deleted even if nothing in the browser wanted it.
Cutting that coupling is independent of the encoder and comes first.

**The encoder has no independent specification.** Every other layer of this port
was checked against Go, because Go is the specification. Inference is not in Go:
`web/PLAN.md` §3 and `REFACTOR_PLAN.md` §5.1–5.2 are its only statement, and the
module is its only implementation. A port that re-derives the rules from the
prose will get something subtly different, and nothing downstream will notice
until a message encodes differently.

*Resolved by phase 8 carrying the rules over rather than deleting them with the
documents that held them: `rust/ENCODER.md` is now where they live, and
`rust/tests/{infer,build}.rs` pin them against the bytes the AssemblyScript
module wrote, which the port reproduces exactly.*

---

## 4. Decisions taken

**The encoder is ported, not dropped.** The alternative — a decode-only demo with
pre-encoded examples — is days rather than weeks, and it costs the repository its
only JSON→colbin encoder and the site its argument.

**It lives in the `colbin` crate, feature-gated.** Not in `rust/wasm`. The crate
is where a Rust service can also reach it, and "JSON text in, colbin out" is
useful well outside a browser. `cargo test` in the main workspace then covers it,
which a `publish = false` cdylib's code would not be. A `encode` feature, on by
default for ordinary consumers and available to turn off, keeps a decode-only
wasm build at today's size.

**No JSON dependency.** `rust/Cargo.toml` opens with "No runtime dependencies:
this crate is a wire format, and a wire format that drags a dependency tree
behind it is one more thing to keep in step." A hand-written parser is a few
hundred lines and keeps that true. It also has to be a parser that preserves
integers past 2^53 exactly, which is the failure colbin exists to prevent and
which `serde_json`'s default number handling would reintroduce.

**AssemblyScript stays alive as the oracle until the encoder is proven.**
`REFACTOR_PLAN.md` §8 made the same call for the last port and was right: the
oracle gets rebuilt first. Concretely — through phases 3 to 7 both modules are
built, and `tests/documents.mjs` is encoded by each and diffed byte for byte.
Only when they agree on every document does `web/assembly/` get deleted.

**The ABI keeps Rust's spelling and the host adapts.** AS exports `resultPtr`,
`setSchema`, `lastError`, `inspectMessage`; Rust exports `result_ptr`,
`set_schema`, `last_error`. `src/lib/codec.ts` currently spells the first set.
The module being kept should not be the one that contorts, so `codec.ts` changes
and the snake_case names stay.

---

## 5. Phases

| | | unblocks |
|---|---|---|
| **1** | Extend `rust/vectors/vectors.json` to cover the reader cases `web_encoded.json` covers; re-point `rust/tests/{section,walk}.rs` at it | AS stops being load-bearing for Rust — worth landing on its own whatever happens next |
| **2** | `json.rs` gains a parser: exact integers, `str::parse` floats, positions for diagnostics | everything below |
| **3** | `infer.rs` (`PLAN.md` §3, §4; `REFACTOR_PLAN.md` §5.1–5.2) and the section **writer** in `section.rs` | a plan from a document |
| **4** | `build.rs`: the plan-driven encode over `wire::Writer`/`Writer8`, `column::append_array`, `packed5` | bytes out |
| **5** | `verify.rs`: decode what was just written and walk it against the parsed input | encode is trustworthy |
| **6** | `inspect.rs`: the span walk, which stops at a table's column where the rendering walk descends into every row | the page's field tree and hex view |
| **7** | The wasm ABI — `encode`, `section`, `inspect_message`, and the `SELF_DESCRIBING` / `VERIFY` / `PACK_STRINGS` flags; `src/lib/codec.ts` re-pointed; the nine `bun` test files driven against the Rust module; `web_encoded.json` regenerated from Rust | the site runs on Rust |
| **8** | Delete `web/assembly/`, `asconfig.json`, the `asc` scripts and the `assemblyscript` devDependency; retire `web/PLAN.md` and `web/REFACTOR_PLAN.md` **into `rust/ENCODER.md`**, which is the only statement of §3's rules; update CI, the READMEs, `RATIONALE.md`, and `PACKAGE_PLAN.md` | one implementation |

Phases 2–6 each end with a `cargo test` that compares against the AS module's
output for the same input, which is what makes the oracle discipline real rather
than stated.

---

## 6. The size budget, and what it actually cost

The budget was **55–65 KB gzipped**, reasoned from §2.6's 34.8 KB decoder plus
`dec2flt`, inference, build, verify and inspect. It was missed, and the two
halves of the miss are worth separating, because only one of them is this port's.

Measured on the module the page ships — `cargo build --release`, `gzip -9`, no
`wasm-opt` in the pipeline:

| | raw | gzipped | code section |
|---|---:|---:|---:|
| decode-only, at the commit before this work | 119 828 B | 44 906 B | |
| decode-only, today (`--no-default-features`) | 120 297 B | **44 967 B** | 108 647 B, 158 functions |
| with the encoder, today (what the site loads) | 244 511 B | **91 220 B** | 213 883 B, 269 functions |

**The feature gate holds.** Decode-only moved 61 bytes gzipped across the whole
port, which is the one thing §6 said to watch: nothing reaches across the gate,
and `colbin::inspect` — the largest ungated module — does not appear in that
build's symbols at all, because LTO drops what the ABI does not reach. CI now
asserts the two builds stay materially different rather than leaving it to a
reading of the source.

**The baseline was already wrong.** §2.6's 34.8 KB describes a decoder from
before `map[string]any` and the root envelope landed; the same build measures
44.9 KB gzipped at the commit this port started from. So the honest comparison is
44.9 → 91.3, and the encoder half costs **46 KB gzipped** against the ~25 KB the
budget assumed. `PACKAGE_PLAN.md` §2.6 has been corrected rather than left to be
quoted again.

Where the encode half goes, by owning module in the unstripped build:

| | | |
|---|---:|---|
| `build` | 18.0 KB | the plan-driven write, both key widths |
| `json::parse` + `core::dec2flt` | 14.3 KB | the scanner and the correctly-rounded float parse |
| `inspect` | 11.0 KB | the span walk — the page's hex view, and nothing else |
| `infer` | 10.5 KB | two passes over the document |
| `verify` | 7.1 KB | the self-check |
| the rest | | wider paths through `wire`, `column` and `packed5` that decode alone does not reach |

`core::flt2dec`, the float *formatter* §2.6 named as 30.6% of the decoder, is
15.8 KB and unchanged — it is the decoder's cost, not the encoder's.

### 6.1 Both levers named above have since been pulled

**`inspect` is a third feature now**, off by default, and the npm package ships
the build without it. The reasoning above was right — a consumer that encodes
has no hex view to feed — and the measurement that settled it is that the span
walk is 15 KB of the optimised module, 6.6% of its code, in every download.
`web/` imports `colbin/inspect`, which is a second `.wasm` carrying it;
everything else gets the one that does not. `js/tests/package.test.mjs` asserts
the published module has no `inspect_message` export, so the saving cannot
quietly evaporate.

**`wasm-opt -O3` is in the pipeline**, as a tool rather than a dependency:
`js/scripts/optimize.mjs` uses `wasm-opt` from the PATH, warns when there is
none, and CI installs binaryen from its release tarball and sets
`COLBIN_REQUIRE_WASM_OPT=1` so the published module cannot ship unoptimised.
The 10 MB devDependency the paragraph above declined turned out to be 104 MB
(binaryen compiled to JavaScript) or 61 MB of platform binaries behind a
postinstall — which is exactly what the `--ignore-scripts` install the package
test asserts cannot run. `-O3` rather than `-Oz`: it wins on *gzipped* size
(89 710 B against 89 790) and is the level that does not trade speed away, and
an A/B of the two modules through `bench.mjs` found no throughput difference at
all.

Where that leaves the modules, optimised:

| | raw | gzipped |
|---|---:|---:|
| decode + materialize (`--no-default-features --features materialize`) | 115 192 B | **45 335 B** |
| encode + materialize — what npm publishes | 209 109 B | **84 195 B** |
| the same with `inspect` — what `colbin/inspect` and the site carry | 224 199 B | 90 127 B |

Against the 91 220 B the table above measured, the module a consumer downloads
is now **84 195 B**: 8% off, without dropping anything a consumer calls.

The lever still unpulled is the big one, and it is packaging rather than Rust:
**the encoder is half the module.** A browser reading a Go service's answers
wants the 45 KB build, and the package has no entry that hands it one. That is
`PACKAGE_PLAN.md`'s decision to take, not this document's.

---

## 7. What stays out

**The column materialiser** (`PACKAGE_PLAN.md` §5) — the fast path that builds
objects from typed-array views without JSON text. It is an optimisation on top of
a working decoder, it is orthogonal to which language the module is written in,
and folding it into this would make one change that has to be right about two
things.

**`map[string]any` in the AssemblyScript module.** It is not ported and will not
be; the module refuses an op it does not know, which is what its `OP_COUNT` and
`MAP_KIND_COUNT` bounds are for.

**The narrow-map gap in the Rust walk** (`Error::Unsupported`). A four-bit
descriptor has no room for a class, so a narrow map's entries take their type
from the schema and need a second element codec. Unreachable from a dynamic
value, reachable from a typed `map[string]string` in a small struct. It should be
closed, but it is not part of this.

---

## 8. What this deletes for free

`PACKAGE_PLAN.md` §2.6 records a live bug in `web/assembly/walk.ts`'s
`columnValue`: a narrow unsigned column comes back sign-extended, so a `u8`
column is wrong for any value above 127. It is unreachable from the module's own
encoder — JSON numbers always infer to 64-bit — so no vector catches it, and it
is reachable from any Go-written message with a `uint8` field. The Rust port
truncates to the op's width, the way Go does. Phase 8 closes the bug by removing
the file it lives in.
