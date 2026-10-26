# Performance log: the October 2026 optimisation passes

What was tried on the encode and decode paths between v0.6.2 and `74f2c8d`,
in two passes (to `f47e3c5`, then on), what it measured, and what was kept. It is here so that the next pass starts
from what is known: the dead ends are as much the result as the wins, and
several of them looked certain before they were measured.

Numbers are from `colbinbench` in github.com/ivanjoz/colbin-benchmarks, on an
i7-1355U laptop, one case per E-core, GOMAXPROCS=1. They compare two builds of
colbin with each other and are not for reporting; the paper's numbers come from
that repository's `matrix/` under `reproduce.sh`. "−7%" is time against the
commit before, A/B in alternating rounds, p < 0.05 unless marked. Every kept
change left the encoded bytes identical (the harness prints `msg-B`).

## How to measure, and how a measurement lied

- **Never compare against a stale baseline file.** The laptop drifts ±5–9%
  between runs minutes apart, which is the size of most changes here, and it
  faked wins. Build the old commit (a `git worktree` plus a `GOWORK` file, not a
  stash) and run `colbinbench -baseline OLD -ab DIR`: the builds take turns and
  both see the same drift. An A/A run of that mode is clean.
- **Neighbours are part of the measurement.** Cases run side by side, and cores
  4–7 and 8–11 each share an L2 and its bandwidth. A sample is slowed by what
  its neighbours do at that moment: TPC-H nested beside TPC-H normalized lost
  ~15%. When each operation's samples were taken as a block, encode with a
  reused buffer measured *slower* than encode into a fresh one (141 against
  124 ms); alone it was 121 against 123. The harness now samples round-robin
  over every format × operation. For one case's clean number, pass only it to
  `-cases`.
- **Treat ±2–3% on a case a change cannot touch as code layout.**
- **A profile of the harness includes its forced `runtime.GC()` between samples
  and the round-trip check.** Filter with `-focus 'Codec.*\)\.Append' -ignore
  'runtime\.GC$'`, or GC and `formats.equal` look like the hot spots.
- **One iteration per sample measures a cold encode.** TPC-H takes ~100 ms an
  encode, so a 100 ms sample is one call right after a GC. Check `n` before
  reading a large case's number; `-sample 1s` gives a steady state.
- **A micro-benchmark is not the data.** The string-copy kernel measured −5% on
  a synthetic "rows" shape and −27% on TPC-H; the row-path prefetch was
  plausible on paper and measured +4%. Decide on the real cases.
- **There is no `perf` on this machine**, so a cache-miss claim is inferred from
  where pprof puts the samples and from what a prefetch does to them. Two such
  inferences were wrong (see the dead ends).
- **Run CI's whole `go-checks` job before pushing**, not only `go test`: it
  includes `GOARCH=386 go test ./...`, where `int` is 32 bits, and a 10-second
  fuzz of every target. A test constant `-1 << 50` in an `int` map once failed
  `main` there.

## What was kept

In commit order. The first pass, to `f47e3c5`, is on `main`; the second
follows it under "The second pass".

- **`821cb0b` Transpose a table in one pass.** In chunks of 256 rows, every
  column read out of a chunk while it is in cache, instead of a pass over the
  rows per column; a wide row is a cache line or more, so sixteen columns read
  the slice sixteen times.
- **`63216df`, `ef443a2` One switch per field in a nested plan**, encode and
  decode, generated into `ops_gen.go` with the value arms inline beside the
  composite ones.
- **`c854ddb` Decode a large table whole, then scatter it a chunk at a time**,
  with a direct mode for tables under 256 KiB. TPC-H normalized decode about
  −48% over the branch. A generic `readTableColumns` made the reader escape to
  the heap; the per-width loops share a non-generic `tableDecode` instead.
- **`739d47b` A string list's strings share one copy of its bytes.**
- **`43d8e6f` Reserve a large composite's length in its sized form up front**,
  and widen or narrow it at close (`widenLength`), so the bytes are still the
  shortest form.
- **`b51fa81` Score a column's three transforms in one pass** (`blockedCosts`).
- **`04fde6b` Compile a map of basic types when its plan is built**: 6 key × 9
  value types as generic instantiations, picked by `reflect.MapOf`, sorted
  entries on a stack array of 16. Maps went on a par with code-generated formats.
- **`a2dddfe` Size an encode's buffer once**, from the first row of each slice of
  structs and a per-plan learned ratio (`sizeRatio`, 1/16 slack). Events encode
  −69%, fresh encodes −20 to −34%. A reused buffer pays ~1.5 ns for the check,
  accepted.
- **`9e95c08`, `31d129b` AVX2 through `simd/archsimd`** (GOEXPERIMENT=simd): the
  encoder's scans, and an unpack with byte shuffles. The unpack was −37% on its
  micro-benchmark and marginal end to end. Since removed: the assembly below
  does the scans in every build, and colbin depends on no GOEXPERIMENT.
- **`eabe647` The same scans in Go assembly**, for every amd64 build: `orAVX2`,
  `statsAVX2`, `blockSetsAVX2`, `stepsAVX2`, with CPUID/XGETBV detection and the
  tails in Go. Encode scans −7 to −31% against purego and −2 to −14% against
  archsimd. The `purego` tag turns every kernel off for an A/B.
- **`1da8592` A block's residuals and its width in one AVX2 pass**
  (`residualsAVX2`, modes raw / zigzag / frame / delta). Metrics −19%.
- **`32e6716` Pack groups of eight in AVX2 at widths up to 16.** Eight values at
  w bits are w whole bytes, so a group is one word at w ≤ 8 and two halves joined
  at bit 4w up to 16, each store overlapping the next group's zero top.
  `BenchmarkPackRun` −80% at w=3, −71% at w=12; encode −7% on TPC-H normalized
  and −6% on the synthetic dataset.
- **`8cecd84` Copy a list's string elements in assembly, prefetching ahead**
  (`wire/elements_amd64.s`). The buffer grows once to the payload, each value is
  two moves that may overlap, and the bytes of the value 32 on are prefetched.
  TPC-H normalized encode −27%, of which the prefetch alone was −14%. A loader
  that cuts fields from lines leaves each value of a column in a different
  line's allocation, a miss no hardware prefetcher predicts, and Go has no
  prefetch to ask for it.
- **`c7b7e9d` Walk a narrow record with the buffer in a local.** The narrow
  field writers moved to `internal/wirefmt` as functions over a `[]byte`, and
  the generated narrow walks write every scalar through them, so the buffer stays
  in registers. Nested −2% (reused) and −4% (fresh), sales −2.5%; a direct
  caller of the `wire` API −5% (`BenchmarkNarrowRecord`, 24.0 → 22.7 ns).

### The second pass

The first pass had worked on encode; this one started from decode, whose row
path was where colbin lost to msgpack and vtprotobuf. The profile of TPC-H
nested decode put half its time in the collector's marking, not in the forced
`runtime.GC()` between samples, after 2.55M allocations a decode.

- **`adc2c9f` A nested decode's strings share chunks of an arena**
  (`wire.Arena`, `Reader.StringIn`). Chunks of up to 16 KiB, never larger than
  the message left to give one; a string past 2 KiB takes its own. Nested decode
  −9% fresh, −7% reused, allocations 2.55M → 302k. The retention is bounded by
  the chunk: one string kept alive keeps up to 16 KiB.
- **`ff3b2d9` Grow a decoded slice in its field.** `reflect.MakeSlice` keeps its
  Value's slice header on the heap, an allocation per slice; `NewAt`, `Grow` and
  `SetLen` grow the field in place. Nested allocations 302k → 152k, sales
  20k → 10k.
- **`8157db6` The AVX2 pack's lane shifts from a table built once.** Built per
  call, they were eight 8-byte stores read back at once as two 32-byte loads,
  which the CPU cannot forward: a store-forwarding stall that was a quarter of a
  short column's encode. `AppendArray` −17 to −23% at 4..16 values.
- **`86d2f40` A short column in two scalar passes** (`appendShort`, up to 32
  values). The four AVX2 calls' fixed cost was most of a short column's encode:
  2× slower than scalar at 4 values. An OR's width is its largest member's, so
  raw's and frame-of-reference's widths come from the column's extremes, and one
  pass scores all three transforms. Same bytes as the long path, held by a test.
  Sales and dataset encode −5.8%.
- **`94a029b` Decode a column with the rest of the message after it.** Cut at
  its end, every column took the byte-at-a-time gather for its last values,
  which for a short column is all of them. `DecodeArray` already returns what it
  consumed, so it is handed the rest of the message, and a column that consumes
  more than its size is refused. A short column's decode −30 to −40%.
- **`3cca675` Unpack and invert the transform in one pass**, where eight bytes
  follow the block, with no scratch between (the simd build keeps its vectorised
  unpack). `ArrayDecode` −13 to −18%; TPC-H normalized decode −8%.
- **`de9cde8` A nested walk tries the field after the last one first**, then the
  key table. Expected at a few percent and measured at **−8 to −14%** on every
  row-path decode. In the flat `readScalars` it measured +4% on a short record,
  so that loop keeps the table alone.
- **`74f2c8d` Pool a large table's buffer while large tables keep coming.** See
  "Raising the pool cap" below: what was left by decision is done with a rule
  that bounds the memory. A buffer past 512k values goes back when its message
  used a quarter of it, up to 8M values, so the first small table drops it, and
  `sync.Pool` lets go of anything idle for two collections. `release` clears only
  the strings it reserved. TPC-H normalized encode −8.3% (116 MB → 0 allocated),
  decode −9.7% (242 → 131 MB). Measured on a 600k-row table: 96 MB held for one
  GC when idle and none after two; none while small tables keep arriving; four
  concurrent encoders hold four buffers for one GC.

The second pass against `f47e3c5`, A/B, n = 20, before the pool change:
nested decode −17% fresh and −20% reused, sales decode −17/−19% and encode
−11%, dataset −6 to −9%, TPC-H normalized decode −5%; the flat record and TPC-H
encode unchanged. The pool change added the TPC-H normalized numbers above.

#### Tried in the second pass and not kept

- **Pack in assembly for w 17..32.** Scalar `packRun` is 1.65% of TPC-H
  normalized encode, the case with the most such widths: at most ~1.5% to win.
- **String columns read straight from the rows**, to drop their half of the
  transposition scratch. Not built: with a 144-byte lineitem row, five string
  columns are five strided passes over ~86 MB each, plus one to size the payload,
  against the one chunked pass `821cb0b` gave the table.
- **Scratch at each column's native width.** Still open, but the AVX2 kernels
  take only int64, so an int32 column would fall to the scalar scans, 2–3× slower
  on a long column. It needs int32 kernels to pay.
- **A scalar path for columns under ~12 values**, where it beat AVX2: subsumed
  by `appendShort`.

## Dead ends: expected to pay, measured, reverted

Each with what made it look promising, so the reasoning can be checked rather
than the experiment repeated.

### A branchless `packRun`

The scalar packer branches on whether a residual straddles the word boundary.
A branchless version, every value shifted into a 128-bit accumulator, looked
like a clear win on a branch that is data-dependent. It measured **2× slower**
and failed a test before it was fixed. The branch is well predicted: for a
fixed w the straddle pattern repeats every group. The win at small widths came
from the AVX2 pack instead (`32e6716`).

### Prefetching a row path's strings and slices

The profile of TPC-H nested put 23% of encode in `memmove` under
`Writer.String`, and the table path's prefetch had just paid 14%. Two versions
were tried, both through a plan-time list of each struct's string, `[]byte`
and struct-slice offsets and a small assembly prefetch:

- one level, the fields of the row 4 ahead: **+2%**, noise;
- two levels, the whole `Lines` array of the order 8 ahead and the strings of
  the rows of the order 4 ahead: **+4% on nested, +2–3% on sales and dataset**.

The row path's misses are not what holds it back: an order's lines are
allocated one after another, and the hardware prefetcher already follows them.
The extra calls and loads are pure cost.

### Copying short strings without `memmove`

The same 23% read as `memmove`'s call and size dispatch, a branch shared by
every string field. Two overlapping 8-byte loads and stores (4-byte for 4..7,
single bytes below), in pure Go — the loads compile to plain `MOVQ` with no
copy — **measured nothing** on nested. It is neither the call nor the misses;
what `memmove` shows is mostly the bytes of the longer comment strings.

### Raising the pool cap for a large table's scratch

Every TPC-H encode allocates ~116 MB of transposition scratch, because
`maxPooledRows = 1<<19` keeps a table past 512k values out of the pool so that
one huge table does not pin its memory. Raising the cap measured **−9% on TPC-H
encode** and no allocation per call. It works; it was **left as it is by
decision**, not because it failed: a codec would hold ~100 MB after one large
table. The alternative not tried is scratch at each column's real width (int32
columns stored as int64 today), which would about halve it.

*Resolved in the second pass* (`74f2c8d`): the cap stays for buffers that are
not earning their size, and one that is goes back to the pool. What the
decision was protecting against, a large buffer living on, is bounded by
dropping it at the first small table and by `sync.Pool`'s two-collection
release.

### A wider prefetch distance

For the string-element kernel: 8 values ahead was short of a miss (−5% on the
micro-benchmark only), 16 inside noise, **32 the best**, and **64 the same as
32** end to end. Distance 32 is what shipped.

### Things that only looked like problems

- **`slices.Grow` in the reuse encode's profile** is the warm-up call growing the
  buffer once; steady state does not grow (B/op ~24).
- **`reserveTable`'s `makeslice` and `memclr`** in the TPC-H profile are the
  pool cap above, not a leak.
- **Encode with a reused buffer slower than fresh** was the harness (see
  "Neighbours are part of the measurement"). Reuse is faster in every case:
  0.85–0.99× of fresh on the large ones. The margin is small because a fresh
  encode is sized once from the learned ratio and never grows by doubling;
  msgpack's does. Measured at `74f2c8d`, msgpack's fresh encode is 1.6–2.4× its
  reused one (TPC-H nested: 197 MB allocated for a 62 MB message), colbin's
  1.02–1.15× (66 MB). That, not colbin's reuse, is why the table below has
  colbin losing encode into a reused buffer and winning into a fresh one.

## The third pass: table encode against msgpack

Started from TPC-H normalized encode into a reused buffer, where colbin (80 ms,
SF 0.1, one E-core) trailed tinylib/msgp (77 ms). A/B against the commit before,
alone on a core, bytes identical throughout.

**Kept**

- **Sum a string column's payload while gathering it** (`gatherStrings`,
  `StringColumnPayload`). `Strings` and `allEmpty` had walked 58 MB of header
  scratch a second time, from memory, only to add up lengths. **−10%.** An
  all-empty column is `payload == rows`.
- **Touch the rows 8 chunks ahead in `gatherTable`.** Plain loads, no assembly,
  so arm64 gets it too. The hardware prefetcher did not keep up with 144-byte
  rows on an E-core. **−6.4%** more; any distance from 2 to 32 chunks is the same.
- **Integer columns that fit an int32 are gathered and scanned as int32 on
  amd64** (`scanVec32`, `residualsVec32`: the int64 kernels with a sign-extending
  load). **−3.0%** (p=0.033, n=12). Same bytes as the int64 column, held by
  `TestAppendArrayInt32MatchesInt64`.

After them: 64 ms against msgpack's 74 ms, alone on a core.

**Tried and reverted**

- **A 64-byte copy for every string up to 64 bytes**, with a page-boundary guard
  so the over-read cannot fault: **+2.8%**. The string kernel is bound by memory,
  not by its length branches; the over-read touches extra lines.
- **Also prefetching the line of each string's last byte**: **+1.7%**.
- **`gatherChunk` at 16 or 64** with the prefetch: 16 is +3%, 64 the same as 32.
- **String headers in a scratch the collector does not scan** (`uintptr` pairs, so
  nothing to `clear` on release): −0.7%, p=0.06, n=10. Not worth the `unsafe`.
- **Encoding strings while gathering**, into a buffer per column: **+3.9%**, and
  +1.6% with the string bytes touched further ahead.
- **Copying every row's strings to their place in the message in one pass**, with
  the integer columns encoded first to a side buffer so the layout is known:
  **+19%**. It halved `copyElements` (a row's strings are usually one allocation,
  and the column-at-a-time copy re-reads those lines once per string column), but
  the line fetches moved into the touch loop and a second pass over the rows was
  added. On an E-core the limit is how many misses it keeps in flight, not how
  many passes it makes.
- **NEON int32 kernels** (`statsVec`, `blockSetsVec`, `stepsVec`,
  `residualsVec` over int32, held to the int64 bytes in 2826 cases on Graviton3):
  **~3% slower** on Lambda arm64 (58.3 against 56.7 ms, msgpack steady at 62.4).
  The sign extension costs what the halved input saves, so arm64 keeps int64.

Not tried: statistics computed in the gather (one column read saved, about as
much scalar work added), and encoding columns in parallel, which the harness's
`GOMAXPROCS=1` cannot show.

`GOARCH=386 go vet ./codec` fails on `dictionary_test.go:274`, where the constant
`2654435761` overflows `int`; it is from the dictionary commit, not this pass.

## The fourth pass: a decode into a used value

A decode cleared its destination with `*value = *new(T)`, and `newSlice`
cleared the field before growing it, so a decode into the same value every time
allocated as much as one into a new value: sales 10k allocations and 2.6 MB a
call where msgpack, which reslices what the value holds, made none. Measured on
the laptop A/B against `bf7151f`, alone on a core, bytes unchanged.

**Kept**

- **Slices lend their arrays to the next decode** (`reset.go`). A type is
  cleared by a program built once from its layout: plain bytes in runs, a typed
  store per string, pointer and interface, and each slice cut to length zero
  with its capacity kept. A list or table that fits is decoded into it, its rows
  cleared by the element's program (one `clear` for a row without pointers), and
  a list of integers or strings appends to the field cut to zero. `settle` sets
  what the message did not carry back to nil, so a decode into a used value is
  `DeepEqual` to one into a new value, held by a test that also interleaves
  broken messages. It is encoding/json's rule: a slice taken from the value
  before the decode sees its elements overwritten. Decode-reuse: sales −21 to
  −23% (allocations 10k → 0), dataset −28%, TPC-H normalized −45% (131 → 40 MB),
  TPC-H nested 152k → 2.3k allocations and no change in time, reading −12 to
  −18% (1 → 0 allocations).
- **A list of integers grows once to its length** (`appendArray`), not by
  doubling.

**Tried and changed**

- **The root cleared by its program on every decode** cost reading's
  decode-fresh +8%: a step per field against one clear. **Saving the root's
  slices on the stack, clearing whole and restoring them** cost the same. What
  shipped clears a root whole unless one of its slices has capacity, which a new
  value never does: decode-fresh +1.5%, inside layout noise.

After it, against msgpack on the laptop: sales decode-reuse 6.1 against 7.6 ms,
dataset 9.5 against 14.2 ms, reading 123 against 124 ns.

### Short tables

A sale's lines are a table from 8 rows: on sales, 1,870 tables of 8 to 32 rows
an encode, half its time, at ~130 ns a column of ~16 values. Each column was
read four times: gathered, checked for all zeros (`Writer.Column`), scanned by
`appendShort`, and turned into residuals. A/B against `c7fe25a`, bytes
unchanged.

**Kept**

- **No per-value overflow check in `appendShort`.** Every step lies between
  low−high and high−low, so none overflows while high−low does not; only a
  column spanning more than 2^63 is walked again (`stepOverflows`). The check was
  the costliest line of the scan. Sales encode −7.5%, dataset −4.6%. A test
  shape spans past 2^63 in steps that do not overflow, which a check of the
  span alone gets wrong.
- **A short table's scans taken while it is gathered** (`gatherScanned`,
  `column.Scan`, `Writer.ColumnScanned`): a table of up to 32 rows is one gather
  chunk, so each integer column's extremes and steps come out of the gather
  loop, and the all-zeros check is `Low == High == 0`. With the change above:
  sales encode −8.9%, dataset −4.7%, TPC-H unchanged; on a 16-row table, 600 →
  500 ns. Held to `gatherTable`'s bytes by a test over every column op.

- **A list of numeric rows written in one go** (`rowProgram.writeRows`): a row
  of numbers and bools writes at most `fixedRoom` a field, so when that is under
  a list element's one-byte length the room for the whole list is made once,
  and each element is its length byte and a kernel call, without
  `OpenElement`, a grow, the program's loop and `CloseElement`. Sales' lines
  under eight rows are such lists. Sales encode −5.3% (n=12, alone on a core;
  beside other cases it read −11%, which was the neighbours). Held to
  writeFields' bytes over every numeric kind and edge value.

**Tried and reverted**

- **The scan in the gather through a widening func**, as `gather` does: +25%.
  `gather` inlines, so its func does; `gatherScan` is too large to, and the func
  became a call per value. It converts instead, a float as its bits and a bool
  as its byte.
- **The scan as a method on `Scan`** (`s.Step(x, previous)`): +5%, the
  accumulators in memory rather than registers.
- **A scalar pack for short columns**, skipping the AVX2 call for two groups of
  eight: +8%. The kernel pays from one group on.
- **The flat root through the row program** (reading encode): +53%, then +14%
  with the writer kept off the heap. On a record of six fields, one of them a
  list the program does not spell, two kernel entries cost more than
  `appendScalars`' switch.
- **Integer list elements stored into room made once** (`appendElements`):
  +5% on six elements, `slices.Grow` a call where append was not.

### int32 scratch on arm64

The harness collects before every sample, so `sync.Pool` has dropped a TPC-H
table's scratch by the next encode, and each one allocates it again: 116 MB on
arm64, where integer columns were gathered as int64, against 90 MB on amd64.
That was most of normalized encode's 0.66× on Graviton2. arm64 now gathers an
int32-fitting column as int32 too (`NarrowColumns`), and `appendWidened` widens
it back into a pooled int64 buffer for the scans, so the NEON kernels stay as
they are: one column's worth of int64 instead of the table's.

Lambda Graviton2, against `da29574`: normalized encode **161 → 104 ms** (msgpack
101 ms, 0.66× → 0.97×), 116 → 96 MB a call; the rest unchanged. Same bytes, held
by `TestWidenedColumnsEncodeAsInt64Ones`.

## Lessons for the next pass

- **Go's inliner budget (80) is load-bearing here.** Moving the writers to
  `internal/wirefmt` and making the methods plain forwards pushed `Uint` and
  `U32` from 80 to 88 and `String`/`Bytes` from 75 to 82: none inlined, and
  `wire`'s own notes measured a `Uint` that does not inline at 18.9 ns against
  7.0 a record. Those four keep their hot half spelled over the writer and call
  wirefmt's cold half. Check `go build -gcflags=-m=2` after any change to a
  writer, and keep cold halves `//go:noinline`.
- **`internal/` costs nothing at run time.** Cross-package inlining is the same
  as within a package; only the budget above and escape analysis matter, and a
  pointer or a generic shape dictionary in a signature is what breaks the
  latter (see `appendElements` in `wire/narrow.go`).
- **Assembly pays where Go cannot say the thing at all**: a prefetch, a variable
  shift per lane, a byte shuffle. It did not pay where the Go was already a
  tight loop the CPU predicts.
- **The interpreted row path is the floor.** On TPC-H nested what remains is
  spread across the per-field walk — loading each field's offset, the first load
  from each row, the indirect check, the switch — with no single hot spot. That
  is the cost a generated per-type encoder (msgpack's) does not pay, and neither
  assembly nor prefetching moved it. The local-buffer walk was expected at −5%
  from its experiment and landed at −2 to −4%.
- **On tables, the remaining encode is memory traffic**: the string copy (now
  prefetched), the gather out of rows, and the scratch of a whole table.
- **Count a decode's allocations before its instructions.** The second pass's
  largest decode wins were allocations (strings, slice headers) and a lookup,
  not the codec's arithmetic: the collector's marking does not show up under
  the function that caused it.
- **A kernel's fixed cost decides short inputs.** Most columns in a table of
  nested rows are 8–32 values, where four AVX2 calls cost more than the scalar
  work; and building a kernel's operand in Go just before the call can stall on
  store forwarding.

## Open leads, not yet tried

- **A generated encoder per type** for the row path: the only lead that
  addresses what is left of encode into a reused buffer (0.63–0.83× of msgpack),
  and a different product decision from the rest of this list. `Generate`
  already covers flat records: on the reading record, alone on a P-core, its
  `AppendColbin` is 24 ns against `Codec.Append`'s 37, and `UnmarshalColbin`
  (reusing slices since `48a5e3f`) 42 ns against 59, same bytes. msgpack's side
  of the benchmark is msgp's generated code. Records with tables (sales,
  dataset) are refused today.
- **Peak memory.** Measured per case, format and operation in a process of its
  own (VmHWM over a baseline, after `clear_refs`), five operations, amd64:
  colbin is lower on every fresh encode and decode, and higher where msgpack
  allocates nothing — TPC-H normalized encode into a reused buffer (92 MB
  against 0.1) and normalized decode-reuse (323 MB against 223), both the
  transposition scratch, which `sync.Pool` loses at every collection inside the
  call. Bounding it needs the table written in row groups (a format change) or
  three passes over the rows: the transform is scored on block costs that need
  the column's minimum first.
- **Scratch at each column's native width**, with int32 AVX2 kernels; without
  them it costs more on the scans than it saves on the traffic.
- **Unpack in assembly.** Less to win than before: the fused read no longer
  goes through scratch.

## Where it stands at `74f2c8d`

Same harness and cases as the table below it. Bytes are unchanged.

| case | op | msgpack | vtprotobuf | protobuf |
|---|---|---|---|---|
| TPC-H normalized | encode / fresh | 0.78× / 1.21× | 0.81× / 0.88× | 2.46× / 2.44× |
| | decode fresh / reuse | 2.01× / 1.92× | 1.64× / 1.72× | 3.14× / 3.09× |
| | size | 3.37× | 1.35× | 1.35× |
| TPC-H nested | encode / fresh | 0.67× / 1.04× | 0.74× / 0.67× | 2.19× / 2.03× |
| | decode fresh / reuse | 1.14× / 1.05× | 0.89× / 0.93× | 1.70× / 1.69× |
| | size | 2.62× | 1.06× | 1.06× |
| dataset | encode / fresh | 0.83× / 2.56× | 1.10× / 1.31× | 3.35× / 3.18× |
| | decode fresh / reuse | 1.31× / 1.26× | 1.84× / 2.10× | 2.91× / 2.87× |
| | size | 4.75× | 1.74× | 1.74× |
| sales | encode / fresh | 0.63× / 1.49× | 0.73× / 1.00× | 2.20× / 2.53× |
| | decode fresh / reuse | 1.32× / 1.04× | 2.21× / 1.72× | 3.36× / 2.84× |
| | size | 5.09× | 1.25× | 1.25× |

The row path's decode now beats msgpack (TPC-H nested 0.86× → 1.14×, sales
reused 0.73× → 1.04×) and is within 7–11% of vtprotobuf on TPC-H nested, from
21–28%. Encode into a reused buffer still loses to both code-generated formats
except on dataset. The "fresh" encode cells move between runs with how each
format grows its buffer (dataset against msgpack: 1.81× then 2.56×); read them
as the cost of a fresh buffer, not of the encode.

## Where it stood at `f47e3c5`

One `colbinbench` run, round-robin sampling (absolute times are higher than in
the A/Bs above because every format runs beside every other; compare the
ratios). Time and size of the other format over colbin's, so above 1× colbin
is faster or smaller.

| case | op | msgpack | vtprotobuf | protobuf |
|---|---|---|---|---|
| TPC-H normalized | encode / fresh | 0.72× / 1.25× | 0.75× / 0.85× | 2.39× / 2.50× |
| | decode fresh / reuse | 1.71× / 1.78× | 1.41× / 1.54× | 2.46× / 2.67× |
| | size | 3.37× | 1.35× | 1.35× |
| TPC-H nested | encode / fresh | 0.71× / 1.17× | 0.71× / 0.77× | 2.24× / 2.18× |
| | decode fresh / reuse | 0.86× / 0.86× | 0.72× / 0.79× | 1.41× / 1.43× |
| | size | 2.62× | 1.06× | 1.06× |
| dataset | encode / fresh | 0.75× / 1.81× | 1.00× / 2.04× | 2.96× / 3.14× |
| | decode fresh / reuse | 1.38× / 1.05× | 2.39× / 1.91× | 3.83× / 3.67× |
| | size | 4.75× | 1.74× | 1.74× |
| sales | encode / fresh | 0.60× / 2.30× | 0.69× / 0.97× | 2.02× / 2.35× |
| | decode fresh / reuse | 0.95× / 0.73× | 1.67× / 1.44× | 2.38× / 1.92× |
| | size | 5.09× | 1.25× | 1.25× |

colbin decodes tables fastest and is the smallest everywhere; it loses encode
into a reused buffer to the code-generated formats (0.6–0.75×), and the row
path's decode to vtprotobuf.
