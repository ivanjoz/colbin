# Re-porting `web/assembly/` onto the byte-aligned format

`8b2bd7d` collapsed three wire modes into one and rebuilt the packages around
it; `fcdfca0` added the schema section. The AssemblyScript module was left
where it was. It does not implement an older *version* of the format — it
implements a format that no longer exists, and there is no migration path
between them, so this is a re-port rather than an upgrade.

Pre-alpha, so nothing here preserves compatibility with what the module writes
today. Every message it has ever produced is unreadable by the current Go
decoder and vice versa.

Status: **phases 0–5 done**; 6 (size, docs) and 7 (`u5b`) open.

---

## 1. The gap

| the module assumes | the format is now |
|---|---|
| a version byte: `0x02` binary, `0x04` JSON mode | one format; byte 0 is the root value's own K8 descriptor, `0xD0 \| detail`, and `0x04` in that nibble means "a schema section follows" |
| a message *is* a columnar record batch: `[recordCount][colCount]([id][flags][payload])*` | a message is one struct. Columnar is a *field class* (TABLE) that a `[]struct` field picks past eight rows |
| the `(k, M)` bit varint, with a candidate search per column | blocks of 128 residuals at an exact bit width per block, after one of four transforms — and only inside a table column |
| `FT_INT/FT_FLOAT/…` + `SK_*` from `codec/format.go` | 24 pinned `fieldOp` values plus `mapKind`; `format.go` is gone |
| a schema section of packed5 names and `descFlags` | `[byteLength][structCount]` then `[flags][fieldCount]([key][nameLen]name[op][extra])*`, names raw, every length `wire.AppendLength` |
| dense columns, nullable bitmaps, an `EMPTY_COLUMN` bit | a zero-valued field is simply not written, unconditionally, and there is no flag for it |
| one key width | four bits or eight, **chosen per key run**, with the width in the descriptor that opens the run |
| nothing on a field says its size | every composite carries a byte length, so every wide field is skippable |
| packed5 strings, each with their own frame header | packed5 is on its way out: `experiments/stringpack`'s `u5b` replaces the codec, and the frame header's two live bits move into the descriptor — §4 |

Two consequences worth stating before the file list.

**The decoder gets easier and stronger.** §4.3 of `PLAN.md` — check every count
and offset against the remaining buffer, because AssemblyScript has neither Go's
unconditional bounds check nor its `recover` — still holds word for word. But
where the old columnar body made the reader *derive* most sizes, every composite
now states its byte length, and a length past the end of its parent is a
definite error rather than a plausible one. The hardening argument is unchanged;
the work is smaller.

**One walk, one sink — and two other things that turned out not to be sinks.**
`codec/json.go` drives JSON text and `map[string]any` through a single `sink`
interface, and says why: the previous Go attempt had two walkers with parallel
switches and they drifted. The plan assumed three consumers of that walk.
Building them found that only one is:

- **JSON text** is the sink, and it is `jsontext.ts`.
- **The inspector** wants byte *positions*, which a sink never sees — and it
  stops at a column where a renderer descends into every row. It is its own,
  shallower walk, held honest by an invariant a sink could not give it: the
  spans tile the body exactly (§9).
- **The self-check** compares the decoder's output against the input document,
  so it walks two value trees rather than a message. It is not a walk of the
  wire at all.

Which still removes most of the 1030 lines of `decode.ts` — but for the reason
that the old file rendered every shape twice, not because three things now share
one walk.

---

## 2. Per-file verdict

| file | lines | verdict |
|---|---:|---|
| `bitstream.ts` | 88 | **freeze**, then delete. See §4 — the accumulator it ports is replaced by `wide64`, an eight-unit `u64` group |
| `packed5.ts` | 662 | **freeze**, then replace. The Go package is being replaced by `experiments/stringpack`'s `u5b`. Porting it now is work that gets deleted. See §4 |
| `decimal.ts` | 293 | **keep**, untouched. Correctly-rounded decimal→binary; appendix A.5's finding is independent of the wire |
| `json.ts` | 635 | **keep**. The UTF-8 scanner and the exact `i64`/`u64`/`f64` reading are the reason the module exists and none of it is format-coupled |
| `diag.ts` | 62 | **keep** |
| `bytes.ts` | 93 | **keep and extend**. `Writer`/`Reader` stay; the bounds-checked read discipline is the point. Add descriptor, length and magnitude primitives |
| `infer.ts` | 385 | **rewrite the output, keep the shape**. Two passes — observe every record, then resolve — survive verbatim (`PLAN.md` §4.5 rule 1). What it produces changes from `FT_*`/`SK_*` to a plan of `fieldOp`s, plus a key-width and a table decision |
| `verify.ts` | 180 | **rewrite**. Same argument, different mechanism: it decodes the message and compares the two value trees, which is where the three documented tolerances of a dense layout fall out (§1) |
| `index.ts` | 151 | **extend**. The ABI grows the out-of-band schema (§6) |
| `schema.ts` | 96 | **delete**. Replaced by `plan.ts` + `section.ts` |
| `varint.ts` | 409 | **delete**. Replaced by `column.ts` |
| `encode.ts` | 361 | **delete**. Replaced by `build.ts` |
| `decode.ts` | 1030 | **delete**. Replaced by `walk.ts` + `jsontext.ts` + `inspect.ts` |
| `test-exports.ts` | 365 | **rewrite** against the new layers |

About 1900 lines deleted, roughly 1700 of which are the old columnar body and
the varint search.

---

## 3. Target shape

Each new file is pinned to one Go file and reviewable line-by-line against it,
which is the discipline the first port used and the reason it worked.

```
assembly/
  index.ts       the ABI, and nothing else
  diag.ts        unchanged
  bytes.ts       Writer, bounds-checked Reader          (+ descriptor primitives)
  decimal.ts     unchanged
  json.ts        unchanged                              the JSON scanner
  column.ts      NEW   transforms + 128-value blocks ← column/pack.go, array.go
  wire/
    desc.ts      NEW   classes, lengths, magnitudes  ← wire/wide.go (header)
    narrow.ts    NEW   K4 run + composites           ← wire/narrow.go, narrow_composite.go
    wide.ts      NEW   K8 run + composites + table   ← wire/wide.go, composite.go, table.go
  plan.ts        NEW   the plan: key, name, op, sub  ← codec/codec.go (typePlan)
  section.ts     NEW   section build and parse       ← codec/schema.go, schema_plan.go
  walk.ts        NEW   the sink-driven message walk  ← codec/json.go
  jsontext.ts    NEW   the JSON text sink            ← codec/jsontext.go
  message.ts     NEW   root byte, section, delivery  ← codec/root.go, json.go
  inspect.ts     NEW   the field tree, with byte spans
  infer.ts       JSON document -> plan                  (§3 of PLAN.md, retargeted)
  build.ts       NEW   plan + document -> message
  wire/narrowwrite.ts, wire/widewrite.ts   NEW   the two writers
  verify.ts      the self-check: decode, then walk against the input

  packed5.ts     frozen, unimported — deleted in phase 7   §4
  bitstream.ts   frozen, unimported — deleted in phase 7   §4
  u5b.ts         phase 7   ← experiments/stringpack u5b_test.go, kernel_test.go
```

`plan.ts` is the hinge. Go's insight in `JSON_MODE_PLAN.md` §2 — that `typePlan`
*is* the schema once `offset`, `sliceType` and `stride` are stripped — is worth
more here than there, because AssemblyScript has no reflection and therefore
never had those three fields to strip. The inferrer builds a plan, the section
serialises a plan, the parser rebuilds a plan, and both walks consume one. There
is one type in the middle of the module and everything else points at it.

---

## 4. Strings are frozen: `packed5` is on its way out

`packed5/` is being replaced by `experiments/stringpack`'s `u5b`, so **nothing
about string packing is ported in this pass**. `packed5.ts` and `bitstream.ts`
stay in the tree, unimported — AssemblyScript reaches only what `index.ts`
imports, so an orphaned file costs nothing in the `.wasm` — and they are deleted
the day the Go side lands its replacement.

This is not only a swap of one codec for another, and the difference decides how
the rest of the port is written.

### 4.1 What is actually changing

Three separate things, from the experiment's own findings:

1. **The codec.** `u5b` is 1.3–2.3× faster to encode and 1.1–2.1× faster to
   decode than packed5, and smaller on four of five corpora. It deletes the
   planning pass — a third to a half of packed5's encode time was pricing the
   four settings of `UPPERCASE_DOMINANT` and `ENABLE_NUMBER_0_1023` before a bit
   was written.
2. **The packing kernel.** Eight 5-bit units in a `u64` is exactly 40 bits, so
   fixed shifts and one store per group cost nothing; it is 3.4× faster to pack
   and 5.5× to unpack than packed5's 32-bit-drain accumulator, byte-identical
   output. This is the part `bitstream.ts` implements, and the replacement suits
   AssemblyScript *better* than the original did — native `u64` with Go's shift
   semantics is what `PLAN.md` §1 chose the language for.
3. **The framing — and this one reaches into `wire/`.** The frame header's eight
   bits are dead weight once a string is embedded in a record: `L` duplicates the
   descriptor's size, `packed` duplicates `enc` under K8, `drop` is retired. Move
   the two live bits into the descriptor and the header byte disappears — worth
   1.7–8.7% off a string field, which is more than the packing kernel won.

### 4.2 The pending wire change, and how the port absorbs it

Finding 3 is the one that matters here, because it is not confined to a package
the module can ignore:

| | today | proposed |
|---|---|---|
| K4 | `[key:4][more:1][size hi:3][size lo:8]` → 0..2047 | a `STRING` nibble: `[key:4][packed:1][upper:1][size hi:2][size lo:8]` → 0..1022, 1023 escaping to u32 |
| K8 | `enc` = 0 raw · 1 packed5 · 2 dict · 3 reserved | `enc` = 0 raw · 1 u5b lower · 2 dict · **3 u5b upper** |

So **every K4 string field changes framing, packed or not** — the inline size
ceiling drops from 2047 to 1022 and two header bits are re-spent. The module
would port `wire/narrow.ts`'s blob path once now and again later.

That is cheap to absorb rather than to wait for:

- put all blob framing behind `blobHeader` / `readBlob` in `wire/desc.ts`, so
  the change is one file and not a search;
- give the vectors a **string-framing tier** of their own, regenerated from Go in
  CI. The day the Go side lands the new descriptor, exactly one tier goes red and
  it names the file to edit.

Nothing else in the port is blocked by it. Integers, floats, composites, tables,
the column codec, the schema section and the whole walk are untouched by string
packing.

### 4.3 What the module does about strings in the meantime

- **The encoder writes raw strings only.** There is no `PACKED5` flag in the ABI
  (§6), and no per-string never-inflate decision to make.
- **The decoder refuses `enc = 1` with a diagnostic** that names it, rather than
  implementing a codec that is being deleted. `Packed5()` is off by default in
  Go, so the corpus, the vectors and any ordinary Go service produce raw blobs;
  a caller who turned it on gets a clear message instead of a wrong string.
- **The vectors are generated with packed5 off**, which is Go's default, so
  tier 1 loses its packed5 frames and gains nothing it has to skip.

### 4.4 One consequence worth naming early

packed5 was **one of the two things that force a type onto eight-bit keys**
(`codec/wide.go`: `hasStrings && Packed5()`). With it out, the only thing left
that forces K8 is a field id above fifteen — which is entirely the module's own
choice to make. That makes §5.2 below stronger than it looked: answer it
"sequential" and *every* object with sixteen keys or fewer takes the K4 fast
path, strings included.

---

## 5. Two decisions, taken

Everything else in this document follows from the Go code. These did not, and
both were settled by the owner on 2026-09-12.

### 5.1 The root is a struct; JSON's top level often is not

`Marshal` encodes a struct and nothing else — `rootOf` has no case for a bare
array or a scalar. The module's headline case is an array of 200 objects, and
its examples deliberately include `[1,2,3,4,5]` and a lone scalar.

| | what it costs |
|---|---|
| **(a) an envelope struct** — one field, key 0, name `"rows"`, holding the document | works against today's Go with no format change; `ToJSON` on the Go side renders `{"rows":[…]}` where the module renders `[…]`, so the two decoders disagree textually on exactly this case |
| (b) refuse a non-object top level | honest, one rule, and it deletes the demo |
| (c) claim root detail bit `0x02` — "the root is an envelope, unwrap it" | the bit is reserved and unallocated (`codec/root.go`); both languages then agree. Costs a change to Go, which is outside what you asked for |

**Taken: (a).** An envelope struct — one field, key 0, named `"rows"`, holding
the document — with (c) left open for the day the asymmetry is worth a change to
Go. It also gives the page its columnar story back for free: an array of objects
becomes one `opStructs` field, which the writer transposes into a TABLE past
eight rows.

Two things follow that the implementation has to hold to:

- **The envelope is only ever built when the top level is not an object**, so it
  can never collide with a caller's own `rows` key.
- **The module unwraps it on decode**, so `encode` then `decode` returns the
  array the caller gave. Go's `ToJSON` on the same bytes renders
  `{"rows":[…]}` — that is the documented cost, and it goes in the README
  section rather than being discovered by whoever tries it first.

### 5.2 How field ids are assigned

Go derives an id from a field name with `fnv8` plus a linear probe when there is
no `cb` tag, and a derived id can land anywhere in 0..255, so **a derived id
forces eight-bit keys**. The old module copied that hash exactly, for parity with
an untagged Go struct.

Under the new format that means every message the module writes is K8: a byte
per present field, and no access to the K4 fast path at all.

| | |
|---|---|
| **sequential, first-seen order** (0, 1, 2, …) | ≤16 fields fit K4 — a byte per field smaller and, on the Go side, about 1.7× faster to decode. The section carries the ids, so nothing that reads the section can be confused |
| keep `fnv8` + probe | an AS-encoded message drops into an *untagged* Go struct with the same field names and decodes with no section at all — the property the old module had |

**Taken: sequential**, in first-seen order, depth-first per object. The `fnv8`
property only held when the Go struct was untagged *and* declared in the same
order as the JSON's first-seen keys, which is not a promise anyone should lean
on, and it cost the fast path on every message to keep it.

So `fnv8` and the linear probe leave the module entirely — `schema.ts` was their
only home and it is being deleted anyway. First-seen order stays exactly as
load-bearing as `PLAN.md` §3.3 said, for a different reason: it is no longer the
input to a hash, it *is* the id, so two implementations that walk differently
still produce different bytes.

With §4 in front of it, this is now the **only** thing that can put a message on
the wide path: more than sixteen fields in one run. Everything else the module
writes is K4.

---

## 6. The ABI

`PLAN.md` §2.1 stands: UTF-8 bytes in, UTF-8 bytes out, nothing traps, every
failure sets a diagnostic and returns a negative length. It grows one capability,
because the format did: the schema now travels **out of band** by default, which
is the delivery the README tells a service to use.

```
alloc(n)            -> ptr        as today
resultPtr()         -> ptr        as today
lastError()         -> len        as today

setSchema(len)      -> 0 | -1     parse a section from the input buffer and hold it;
                                  len = 0 clears it
encode(len, flags)  -> len        JSON -> a message.  flags: SELF_DESCRIBING, VERIFY
section()           -> len        the section for the last encode, for sending once
decode(len)         -> len        message -> JSON text, using the held schema or,
                                  when byte 0 has 0x04, the message's own
inspect(len)        -> len        message -> the field tree, with byte spans
```

`encode` without `SELF_DESCRIBING` writes `0xD0`/`0xD8` and leaves the section to
`section()`; with it, `0xD4`/`0xDC` and the section in front. Those are the two
deliveries `codec/schema.go` documents, and the page wants both — the size panel
should show the body and the section separately, because the honest number for a
stream is the body and the honest number for a file is the sum.

There is no `PACKED5` flag: §4 took it out, and it comes back with `u5b`.

---

## 7. The port, layer by layer

### 7.1 `column.ts` — `column/`

Smaller than `varint.ts` was. There is no `(k, M)` candidate search: the encoder
costs four transforms (`blockedCost` for raw, delta, frame-of-reference,
constant), picks the cheapest, and packs blocks of 128 at
`widthOf(block)` bits, LSB-first with no gap. The decoder is one load, one shift,
one mask per value for `w ≤ 57`, and a gather path above it and at the tail.

Three things the vectors have to pin: the tie-break order between transforms
(strict `<` in Go, and a tie broken the other way produces a valid frame that
does not match), the zigzag decision (`raw` zigzags only when the column holds a
negative), and the block boundary at 127/128/129 elements.

### 7.2 `wire/desc.ts`, `wire/narrow.ts`, `wire/wide.ts`

The framing, and the bulk of the new code. Go keeps the two widths in separate
files because a width the compiler cannot see is a width it cannot fold;
AssemblyScript compiles through Binaryen and the same argument applies, so the
split is kept rather than reasoned about again.

What has to be there for the decoder to read anything a Go service sends:

```
K4  integer/float  [key:4][positive:1][n:3]     magnitude, n bytes (7 -> 8)
    blob           [key:4][more:1][size hi:3][size lo:8] | escape to 2|4|8
    vec            [key:4][positive:1][w:2][more:1][count:8]
    strings        [key:4][more:1][count:11]  then [size:1, 0xFF -> 4] bytes
    struct/list/map/table   [key:4][sub:1][k8:1][lw:2][len][…]

K8  0 vvvvvvv                                    the value, 0..127
    1 ccc dddd   INT · BLOB · VEC · COL · LIST · STRUCT · MAP/TABLE · SPECIAL
    SPECIAL detail 0b1xxx  the varint integer: three bits in the descriptor,
                           seven per byte after it, zigzagged iff signed
```

Two traps, each worth its own test, both already named in the Go comments:

- **A float is a byte-reversed IEEE-754 bit pattern in a scalar field and a
  plain one in a column.** Reading either as the other is silent nonsense.
- **A narrow list's element is `[len][body]` with no descriptor between them**,
  which is exactly why a `structDef` carries its own key width — the wire does
  not say it for that one shape.

The encoder needs less than the decoder: the module never writes a MAP, a
`[]byte`, a bitmap run, a packed string or a COL outside a table. The asymmetry
is deliberate and is the same one `PLAN.md` §5 drew.

**The blob path is the one to isolate.** §4.2 renames and re-bits it under both
key widths; `blobHeader` and `readBlob` live in `desc.ts` so that the day it
lands, the edit is one function per direction and the string-framing vector tier
says so out loud.

### 7.3 `plan.ts` and `section.ts` — `codec/codec.go`, `schema.go`, `schema_plan.go`

`fieldOp` is now **format**: 24 values, pinned by `TestFieldOpsArePinned`, never
reordered. The AS copy needs the same test — a table of `(name, value)` asserted
against a fixture the Go generator emits, so a Go-side addition that the module
has not seen fails here rather than decodes as the wrong type.

Struct hoisting comes across as written: reserve the index before walking the
fields, so a self-referential type terminates. JSON cannot express one, so this
matters only for the decoder — and the decoder is exactly where a crafted
section could otherwise recurse forever. `maxSchemaDepth` is 128 in Go; the
module matches it, and `MAX_DEPTH` for the JSON scanner stays at 64, so the
encoder's bound is under the decoder's (`PLAN.md` §4.5 rule 4).

### 7.4 `walk.ts` + the sinks — `codec/json.go`, `jsontext.go`

One walk, three sinks. The walk knows the wire; a sink knows what to do with a
bool; neither knows the other.

Everything `codec/json.go`'s header lists has to hold here too, and each is a
test:

- **an absent key is a zero value, not a missing field** — every field of the
  schema appears in the output, the omitted ones as `0`, `""`, `false`, `null`;
- **`[]struct` has two shapes** and the reader dispatches on what it finds;
  the TABLE path is a transpose the Go decoder gets for free and this one does
  not, so columns are materialised and the peak is documented;
- **a NaN or an infinity is refused**, before anything is written, so the
  caller's buffer is either the whole document or untouched;
- **the number spelling is `encoding/json`'s** — `'f'` until the exponent leaves
  ±(1e-6, 1e21), then `'e'` with the leading zero trimmed — so a test compares
  bytes rather than parsed values;
- **K4 cannot skip**, so a narrow message holding a key the section does not
  list ends the decode with an error that says why.

The key-order divergence from `PLAN.md` §6 *disappears*, which is a small win
worth noticing: Go no longer builds a `map[string]any` and sorts, it walks the
section in order. `AppendJSON` puts the fields the message carried first, in wire
order, then the omitted ones — so matching it is a rule the module can follow
exactly rather than a difference to explain.

### 7.5 `infer.ts` and `build.ts`

The inference rules of `PLAN.md` §3 are unchanged as *rules*; what they produce
changes. Per column the inferrer now decides:

| observed | op |
|---|---|
| `true`/`false` | `opBool` |
| integers only | `opInt64`, never narrowed (appendix A.3 — and it is still free: a table column declares its own width from the data) |
| above int64 max, all non-negative | `opUint64` |
| any fractional or exponential value | `opFloat64` for the column |
| string | `opString` |
| object | `opStruct`, with a child plan |
| array of objects | `opStructs` |
| array of scalars | `opInt64s` / `opFloat64s`-equivalent / `opStrings` |
| `null` or an absent key | `opPointer` with `elemOp` set |

plus two decisions the old format did not have: **`isWide`**, which under §5.2
and §4 is now exactly "more than sixteen fields in this run", and **`canTable`**,
which must mirror `transposable()`
exactly — a struct holding a nested struct or a slice cannot be a column and
stays row-wise however long it gets, and getting that rule wrong changes the
layout Go would have picked for the same data.

`build.ts` then writes from the *plan*, never from the raw JSON values
(`PLAN.md` §4.5 rule 2), and the `[]struct` threshold is `tableThreshold = 8` — the same
constant, or the two encoders disagree on shape for eight rows.

---

## 8. Verification: the oracle has to be rebuilt first

**CI is red right now and has been since `8b2bd7d`.** `web/vectors/` was deleted
in that commit, `bun run test` still starts with `bun run vectors`, and
`go run ./vectors` from `web/` says `directory not found`. `web/tests/vectors/`
is generated, so the whole Node suite has nothing to run against. Rebuilding the
generator is the first commit, not the last.

`rust/vectors/` is the model, and it is a better one than what `web/vectors/` was:
committed JSON, regenerated in CI, `git diff --exit-code` on the result — so a
Go-side format change that nobody ported fails on the Go side's own push rather
than silently passing against stale expectations.

`web/vectors/main.go` emits four tiers. Every one is generated with `Packed5()`
off, which is Go's default and §4's position — the old packed5 tier is dropped
and comes back as a `u5b` tier when there is a `u5b`:

1. **string framing** — a blob at every length the header holds, at the 2047
   boundary and past it into each escape width, empty, invalid UTF-8, NUL bytes,
   under both key widths. This tier exists because §4.2 is going to change it:
   it is the tripwire that names the file to edit on the day it does.
2. **column frames** — every transform, both zigzag decisions, widths 0..64,
   counts 127/128/129/1000, and a column that is constant, one that is
   monotonic, one that is random.
3. **messages** — the `corpus` types, which did not exist when the first port
   was written and are exactly what this needs: `User`, `Product`, `Metric` flat;
   `Sale` at both `len(Detail) < 8` and `>= 8`, which straddles the table
   threshold on purpose; a K4 type and a K8 type; a pointer type; a map type
   (decode only — a Go map has no iteration order, so it is never a byte
   vector). Each entry carries the message, its section, `colbin.FieldIDs`, and
   `colbin.ToJSON` output.
4. **The other direction** — `web/vectors/web_encoded.json`, written by
   `node tests/emit.mjs` and read by `go test ./web/vectors`: the module
   encodes, Go parses its section and renders its message, and the two decoders
   must agree.

**Tier 4 is a round trip rather than the byte-parity oracle §7 originally
proposed, and that is a change of gate worth justifying.** The oracle would
build a `reflect.StructOf` from the inferred shape and compare `Marshal`'s bytes
to the module's — which needs the whole of `PLAN.md` §3's inference rules
implemented a second time, in Go, forever (§12 lists that as an accepted cost).

Two things have changed since that was written. The module now ships its
**section**, so Go does not have to agree with it about field ids or layout to
read a message — byte parity would be pinning a choice rather than a contract.
And the contract a caller actually has is "a JavaScript client encodes, a Go
service decodes", which byte parity does not test at all and this does. The
`rust/vectors/rust_encoded.json` step in CI makes exactly the same argument for
exactly the same reason.

What is given up is real and worth naming: nothing now catches the two encoders
*diverging in layout* — the module picking a list where Go picks a table, say —
as long as both still decode. The table threshold and the key-width rule are
therefore held by the comments that cite Go's constants and by nothing else.

Then the three levels of `PLAN.md` §7 hold as written, with one addition: the
fuzz target now has a much better shape to attack, since a crafted byte length
inside a composite is the obvious way to make the decoder read past its parent.

---

## 9. What the page needs

Done, and two of the five points turned out differently from the guess.

- `Report.columns` is now `fields` — a tree whose nodes are fields, with a
  table's columns and a list's elements one level deeper. Every node carries a
  real span, which is what lets the hex view highlight it.
- `recordCount` is `rows`: the element count of the root's list or table, taken
  at depth one only. A nested list is a field of a record rather than a count of
  them, and letting it win would have made an invoice with three lines report
  three records.
- `schemaBytes` is the section length, and **the size panel gained the number
  that keeps it honest** — see below.
- The `encodeBinary`/JSON-mode split in `src/lib/codec.ts` became the two
  deliveries, both returned from one `encode` call: encoding twice to get the
  second number would have made the timing beside it a lie.
- The download writes `.cb`, and writes the *self-describing* form rather than
  the one the page measures. A message on a wire goes behind a section the far
  end already has; a file has nowhere to put one.

### The honesty moved, and the examples had to move with it

`PLAN.md` §9 shipped five losing examples on purpose — "a demo that ships only
its best cases is one nobody believes twice" — and under the out-of-band schema
**none of them lose any more**. A lone object was 44 B against 35 B of JSON; it
is now eleven bytes and a 3.2× win, because the section is sent once and is not
in the message at all.

Deleting the losing examples would have been the wrong repair. The loss did not
go away, it *moved*: to the standalone file, which still carries its schema. So
the panel now states both, and the second is marked when it is under one:

| example | as a stream | as one file |
|---|---:|---:|
| metric points | **3.80×** | 3.76× |
| products | 3.48× | 3.44× |
| clients, 1000 | 2.78× | 2.78× |
| people | 1.79× | 1.42× |
| one object | 3.18× | **1.00×** |
| mixed numbers | 1.69× | **0.79×** |
| a bare array | 1.38× | **0.58×** |

Which is a truer story than the old one, and a more useful one: it is the
difference between the two deliveries, priced, on the reader's own examples.

## 10. Phases

| # | phase | done when |
|---|---|---|
| **0** ✅ | `web/vectors/` rebuilt and committed, regenerated in CI with a diff check. The nine suites whose tiers no longer exist are deleted; `json.test.mjs` survives and is rewired, because `json.ts` and `decimal.ts` are what the format change does not touch | `bun run test` runs end to end: 561 pass |
| **1** ✅ | `column.ts`, `wire/{desc,narrow,wide}.ts`, `plan.ts`, `section.ts`; `varint.ts`, `encode.ts`, `decode.ts`, `schema.ts`, `infer.ts`, `verify.ts` deleted | tier-1 and tier-2 vectors byte-identical both directions |
| **2** ✅ | `walk.ts` + `jsontext.ts` + `message.ts`: **decode**, every op, both key widths, LIST and TABLE | every tier-3 message renders the same bytes as `colbin.ToJSON`, through both deliveries |
| **3** ✅ | `PLAN.md` §4.3 hardening and the fuzz target on the new shapes | no trap, no out-of-bounds read, a diagnostic every time — and the module refuses **exactly** the 806 of 5046 corruptions Go refuses |
| **4** ✅ | `infer.ts` + `build.ts` + `verify.ts` + the writers: **encode**, with the §5.1 envelope and §5.2 ids | every document round-trips with the self-check on, and **Go reads every message the module writes** |
| **5** ✅ | `inspect.ts` and the page | the spans tile the body exactly, on both corpora; Chrome drives all eleven examples with no console errors |
| **6** | re-measure the gzipped size against the 50 KB budget; rewrite `PLAN.md` | numbers on the page come from the module that shipped |
| **7** | **strings**: `u5b.ts` + the `wide64` kernel, the §4.2 descriptors, `packed5.ts` and `bitstream.ts` deleted | *gated on the Go side landing `u5b` and the embedded field layouts* — the string-framing tier goes red, then green |

Phase 2 before phase 4 is a reversal of the original order, and deliberate. The
decoder is now the fully specified half — `codec/json.go` is the specification
and the corpus is the oracle — while the encoder still needs `PLAN.md` §4.1's
inference rules, which are the module's own invention and have no Go counterpart
to be checked against. It is also the half the npm package exists for: a
JavaScript client of a Go service needs to *read* the answer.

Phase 7 is last and blocks nothing. Phases 0–6 ship a module that reads and
writes every message an ordinary Go service produces, because `Packed5()` is off
by default; a string field is raw bytes and raw bytes are what the port handles.

---

## 11. What stays out

Unchanged from `PLAN.md` §5, and for the same reasons:

- `opBytes` on the encode side — JSON has no bytes type, and inventing a
  `"base64:"` convention would be inventing format semantics the library has not
  got. The decoder renders it, as base64, like `encoding/json`.
- `opMap` on the encode side — a JSON object is a struct here.
- Any `interface{}` equivalent. There is no wire form for one in either language.
- Compact mode. It no longer exists.

And three that are new. One is temporary:

- **Packed strings, both directions**, until phase 7. The encoder writes raw; the
  decoder refuses `enc = 1` by name. See §4.

Two are the format's own limits rather than this module's, found while the
encoder was being written and worth stating where someone will look for them:

- **An array of floats, an array of booleans and an array of arrays.**
  `sliceOp` in `codec/codec.go` resolves integers, strings and structs and
  refuses everything else, so there is no op for these — a JSON document holding
  one is refused by name rather than encoded into something that does not
  decode. An array of floats is the one a caller will actually hit.
- **A null where an object belongs.** The format refuses a pointer to a
  composite, so a nullable struct field has no form: the null is omitted and
  comes back as an object of zeros. The encoder warns rather than refuses,
  because the rest of the document is still exactly what it was.
