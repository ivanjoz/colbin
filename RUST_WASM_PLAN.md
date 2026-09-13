# One browser module, and it is the Rust one

**Status: proposed.** Phase 1 aside, nothing here is built yet.

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
| **8** | Delete `web/assembly/`, `asconfig.json`, the `asc` scripts and the `assemblyscript` devDependency; retire `web/PLAN.md` and `web/REFACTOR_PLAN.md`; update CI, the READMEs, `RATIONALE.md`, and `PACKAGE_PLAN.md` §3 | one implementation |

Phases 2–6 each end with a `cargo test` that compares against the AS module's
output for the same input, which is what makes the oracle discipline real rather
than stated.

---

## 6. The size budget

34.8 KB gzipped today, and §2.6 found that **30.6% of it is `core::fmt`'s float
formatter** — larger than the whole wire layer. The parser side adds `dec2flt`,
which is the same machinery in reverse, and inference, build, verify and inspect
add code that has no counterpart in the decoder.

A plausible landing zone is **55–65 KB gzipped**, against AssemblyScript's 46.4.
That is a real regression on the one axis where AS currently wins, and it is why
the `encode` feature exists: a consumer decoding a Go service's answers should
link neither the parser nor the encoder, and LTO already drops what the ABI does
not reach. The npm package can ship both builds and let the entry point choose —
`PACKAGE_PLAN.md` §3 already commits to two entries for a different reason.

Measure it at the end of phase 4, not at the end of phase 8. If the decode-only
build has grown at all, something is reaching across the feature gate.

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
