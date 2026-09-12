# stringpack — byte alignment, and where packed5's time really goes

Test-only, like `experiments/bytealigned`. Nothing here is imported by the
module; everything lives in `*_test.go`.

```
go test ./experiments/stringpack/                      # correctness
go test ./experiments/stringpack/ -run XXX -bench .    # the numbers below
```

Every figure quoted is from one run: 13th Gen Intel Core i7-1355U, Go 1.27,
`-benchtime 1s -count 3`, best of three. Throughput is ms/MB of *source* text,
matching `packed5/README.md`. Ratio is encoded bytes over source bytes, framing
included.

---

## The question

> An `uint16` is a `uint5 + uint5 + uint5 + 1 bit`, so I can pack 3 `uint5` in
> one `uint16` — does it make sense? And the extra bit, I don't know what to do
> with it.

Yes, and the instinct behind it is right: get the packing onto fixed shifts and
whole-byte stores. But the `uint16` triple is the expensive form of that idea,
and bit packing is not where packed5 spends its encode time. Both claims are
measured rather than argued.

## Findings

1. **8 units in a `uint64` beats 3 units in a `uint16` on both axes.** 8 x 5 bits
   is exactly 40 bits = 5 bytes, so the same fixed-shift, one-store-per-group
   property costs **nothing**, where the `uint16` triple wastes 1 bit in 16
   (6.7%). Measured: 0.201 ns/unit to pack against 0.304, and the output is
   byte-identical to what packed5 writes today.
2. **32% to 46% of packed5's encode time is the planning pass**, not the packing.
   `packed5.Size` — which plans and throws the plan away — costs 2.26 ms/MB on
   `name` against `Append`'s 4.90.
3. **`u5b`, which does both, is faster *and* smaller than packed5 on all five
   corpora**: 1.3x to 2.3x faster to encode, 1.1x to 2.1x faster to decode, and
   0.02% to 3.4% fewer bytes. No planning pass, no wire-format flag guessing.
4. **`p6` — six flat bits per character, no state at all — is 1.6x to 3.2x
   faster to encode and 1.5x to 2.8x faster to decode**, for 12% to 20% more
   bytes than `u5b`. It is also the base64 layout, so its SIMD kernels exist.
5. Two things that did **not** work: a fixed-width 3-digit number token (worth
   0.06%), and table-driven byte classification (12% to 35% *slower* than the
   comparison chain it replaced). Both are kept in the tree; see
   [Negative results](#negative-results).

---

## 1. The packing kernel

`kernel_test.go` prices three ways of turning the same `[]uint8` of 5-bit units
into a stream, with no tokeniser in the way.

| kernel | shape | bytes/unit | pack ns/unit | unpack ns/unit |
| --- | --- | --- | --- | --- |
| `dense` | accumulate, drain 32 bits — packed5's `bitWriter` | 0.625 | 0.685 | 1.294 |
| `wide64` | 8 units = 40 bits: build a word, store 8 bytes, advance 5 | 0.625 | **0.201** | **0.234** |
| `triple16` | 3 units = 15 bits in a `uint16`, 1 bit spare | 0.667 | 0.304 | 0.342 |

`wide64` is **3.4x faster to pack and 5.5x faster to unpack** than the
accumulator, and `TestWide64IsDense` pins that it emits byte-identical output —
the grouping is a pure speed change, not a format change.

`triple16` is 1.5x slower than `wide64` **and** 6.7% larger. The `uint16` idea is
dominated: its spare bit would have to earn back 6.7% before it broke even, and
there is nothing to spend it on that `wide64` cannot do for free.

The price of either grouping is buffer slack. The last store writes up to 3 bytes
past the last real one and the last load reads up to 7 past it, so both ends need
`slack` (8) bytes of room. In a column of back-to-back frames the next frame
supplies it; only the final frame needs padding. This is the same trade
`BYTE_ALIGNED_PLAN.md` already accepts for bit-blocks, and `TestCodecsBackToBack`
pins that a real column decodes correctly under it.

### Why packed5's accumulator was already half-way there

Worth stating plainly, because it bounds the whole exercise: packed5's
`bitWriter` already drains **32 bits at a time**. It is not a byte-at-a-time bit
writer. The move to `wide64` is 32 bits -> 40 bits per store plus the removal of
the `append` bounds check and the variable drain branch — a 3.4x kernel win, but
a kernel that was never the dominant term.

## 2. Where packed5's encode time actually goes

`BenchmarkPlanSplit` runs `packed5.Size` (plans, discards the plan) against
`packed5.Append` (plans, then writes).

| corpus | plan only | plan + write | planning share |
| --- | --- | --- | --- |
| name | 2.26 | 4.90 | **46%** |
| sku | 2.49 | 5.88 | 42% |
| spanish | 1.89 | 4.20 | 45% |
| sentence | 2.11 | 5.55 | 38% |
| paragraph | 0.88 | 2.75 | 32% |

(ms/MB.) A third to a half of encode time is the pass that prices the four
settings of `UPPERCASE_DOMINANT` and `ENABLE_NUMBER_0_1023` before a single bit
is written. No packing kernel can touch it. `packed5.Size` is also a full,
branchy classification walk over every byte, so that half is really "classify the
string twice".

## 3. The codecs under test

### Shared framing

Every experimental codec here uses the same one-byte header as packed5, so the
ratios are directly comparable:

```
bit  0     packed        0 = raw bytes follow, 1 = packed unit stream
bit  1     drop          1 = the payload's last unit is padding
bit  2     upper         the stream starts in uppercase mode
bits 3-7   length        payload byte length, 31 = uvarint follows
```

Identical overhead to packed5 — one byte for any payload up to 30. The only
difference is what the flag bits are spent on.

`drop` needs a single bit: for a payload of P bytes holding U units of width w,
`w*U <= 8P < w*U+8`, so `floor(8P/w)` is U or U+1 and never more, for w of 5 and
of 6 alike. That freed a bit, which turned out to matter — see `u5b`.

### `u5` — packed5, unit-ised, no planner

Two changes from packed5:

**Every token is a whole number of 5-bit units.** packed5's 4-bit `opSimple`
operand, 2-bit escape count and 8-bit raw bytes are what stop its stream sitting
on a unit grid. Here the simple table is folded into two 32-entry symbol tables
(64 symbols instead of 30+15), the escape count is one unit, and a raw byte is
two. That is what makes `wide64` applicable at all.

**No planning pass**, because there are no behavioural header flags left to plan.
The case mode always starts lower and moves on an ordinary `CASE_TOGGLE_LONG`;
the number opcode is always a number. The freed header bits carry the stream
terminator instead, which takes the 3-bit `padBits` prefix out of the payload —
so the unit grid starts at bit 0 of byte 0 rather than bit 3.

```
0..25  letter a..z, cased by the current mode
26     space
27     CASE_TOGGLE_SIMPLE       next letter only
28     CASE_TOGGLE_LONG         until the next one
29     + 1 unit: index into u5TabA   (10 digits + 22 punctuation)
30     + 1 unit: index into u5TabB   (accents, rare punctuation), or 31 = escape
31     + 2 units: integer 0..1023
escape: 30, 31, one unit holding n-1 for n in 1..4, then 2 units per raw byte
```

Two encoders share one tokeniser: `u5Append` tokenises into a stack array and
then packs; `u5AppendFused` packs as it goes. `TestU5FusedMatches` pins them
byte-identical, so the gap between them is exactly the cost of materialising the
units.

### `u5b` — `u5` plus the leading case toggle hoisted into the header

`u5` lost 5.5% to packed5 on `sku` and nowhere else. The dash was the obvious
suspect and it is not the cause. Dumping the unit stream for `"SKU-4217-hola"`
settles it:

```
units = [28 18 10 20 29 12 31 5 13 29 7 29 12 28 7 14 11 0]
         ^^
         CASE_TOGGLE_LONG
```

The string opens in uppercase, so `u5` spends its first token switching case —
five bits that packed5 gets for free from `UPPERCASE_DOMINANT`. That is
essentially the whole gap.

The fix does not need the planning pass back. Tokenise as usual; then, if
`units[0]` is `CASE_TOGGLE_LONG`, drop it and set the header's spare `upper` bit.
One comparison on a slice that has already been built. Nothing is planned and
nothing is rescanned, and `"SKU-4217-hola"` goes from 13 bytes to 12.

`u5b` also carries the 3-digit number token described under
[Negative results](#negative-results); it is nearly free either way.

### `p6` — six bits flat, no state at all

The opposite bet. Sixty-four slots hold both letter cases, the space and all ten
digits outright, so there are no case toggles, no case mode, no header flags, no
lookahead and no planning. Encoding is one table index per source byte. Eight
units are 48 bits, so it packs in the same shape as `wide64` (store 8, advance 6).

```
0..25   'a'..'z'
26..51  'A'..'Z'
52      ' '
53..62  '0'..'9'
63      escape + 1 unit: index into p6Tab2, or 63 = raw byte + 2 units
```

## 4. Results

### Encode (ms/MB / ratio)

| corpus | packed5 | u5 | u5 fused | **u5b** | u5b tab | p6 |
| --- | --- | --- | --- | --- | --- | --- |
| name | 5.02 / 0.791 | 4.09 / 0.765 | 3.67 | **3.91 / 0.765** | 4.39 | 2.37 / 0.859 |
| sku | 6.06 / 0.893 | 3.96 / 0.942 | 3.92 | **3.54 / 0.893** | 4.04 | 2.41 / 0.952 |
| spanish | 4.39 / 0.697 | 2.28 / 0.677 | 2.36 | **2.16 / 0.677** | 2.55 | 1.96 / 0.801 |
| sentence | 5.75 / 0.701 | 3.49 / 0.690 | 3.24 | **3.24 / 0.690** | 3.82 | 1.80 / 0.793 |
| paragraph | 2.88 / 0.640 | 1.34 / 0.636 | 1.76 | **1.23 / 0.636** | 1.66 | 1.76 / 0.761 |

Zero allocations for every codec on every corpus.

### Decode (ms/MB)

| corpus | packed5 | u5 | **u5b** | p6 |
| --- | --- | --- | --- | --- |
| name | 4.60 | 4.08 | **4.02** | 3.09 |
| sku | 4.98 | 4.22 | **3.71** | 2.95 |
| spanish | 3.46 | 2.44 | **2.48** | 2.02 |
| sentence | 4.11 | 3.40 | **3.15** | 1.92 |
| paragraph | 2.72 | 1.59 | **1.31** | 0.96 |

### Classification cost, for scale

`u5.tokenize` is the single classification walk with the units thrown away:
2.45 (name), 2.70 (sku), 1.46 (spanish), 2.35 (sentence), 0.96 (paragraph) ms/MB.
Against `u5b`'s full encode, that is 63% of the remaining time on `name`. After
the planner is gone and the packing is grouped, **most of what is left is
deciding what each byte is** — though see the negative result on class tables
below for how not to attack it.

## What the numbers say

**`u5b` is strictly better than packed5 on this corpus set**: 1.3x to 2.3x faster
to encode, 1.1x to 2.1x faster to decode, and never larger. The wins compose from
three separate places, none of which is the bit packing on its own:

- deleting the planning pass (32-46% of encode time),
- grouping the packing (3.4x on the kernel, which is a smaller slice),
- moving the 3-bit `padBits` terminator out of the payload into header bits that
  were doing nothing, which is a pure size win of 3 bits per frame — about 4% on
  an eight-byte name.

That last one is why byte alignment paid for itself in *size* as well as speed,
which is the opposite of the usual trade. It only happened because the flags that
were deleted and the terminator that needed a home cancelled out.

**Fusing the packer into the tokeniser is a wash.** Fused wins ~6% on short
records, where the scratch-array setup is a fixed cost paid per string. Staged
wins 24% on `paragraph`, where the branch-free 8-at-a-time packing loop runs long
enough to amortise it. Pick by expected record length.

**`p6` is the fastest thing here and the largest.** 1.6x to 3.2x faster to
encode, 1.5x to 2.8x faster to decode, at 12% to 20% more bytes than `u5b`. Its
layout is base64's, so the `pshufb` kernels for it are a solved problem — the
scalar numbers here are a floor, not a ceiling. It is the right answer when a
column is wide and the CPU is the constraint, and the wrong one when the bytes
are.

One correction to an earlier claim of mine: `p6` is **not** smaller than packed5
on `name` or `sku`. 0.75 bytes/char is `p6`'s *payload* cost; packed5's 0.79 is a
measured ratio with framing included. Compared like for like, `p6` is 8.6% larger
on `name`. The flat-6-bit argument wins on speed, not on size.

## Negative results

Both are left in the tree, wired into the benchmark, so they are not proposed
again later.

**A fixed-width 3-digit number token is worth 0.06%.** The reasoning was sound —
a leading zero cannot go through a value-ranged number token, since `"0042"`
would decode as `"42"`, so both codecs shred it into lone digits. Making the
token exactly three zero-padded digits at 15 bits fixes that at five bits per
digit, flat. It does work: `"SKU-0042-hola"` goes from 14 bytes to 13. But the
corpus generates `SKU-%04d` from a uniform `0..9999`, so only one SKU in ten has
a leading zero at all, and the corpus-wide effect is 0.9423 -> 0.9417. The real
`sku` gap was the case toggle, which the dump above found in a minute and this
token never touched. `u5b` keeps it because it is free, not because it earned
its way in.

**Table-driven classification is slower than the comparison chain.**
`u5bTokenizeTab` replaces the `isLetter` / `== ' '` / `isDigit` / symbol-lookup
chain with one 256-entry `[256]uint16` load giving class and operand together —
the obvious answer to "classification is 63% of what is left". It is 12% (name)
to 35% (paragraph) slower on every corpus, and `TestU5BTabMatches` pins it to the
identical output, so the difference is nothing but the classifier. The chain
decides "is this a lowercase letter" in three ALU operations on a branch that
predicts almost perfectly for this data; the table costs an L1 load plus a
switch the branch predictor handles worse. The lesson is that the classification
cost is not *dispatch* — it is the per-character loop itself, which means the
way at it is SWAR or SIMD over several bytes at once, not a smarter table.

---

## 5. The rest of the design space

Measured above: `wide64`, `triple16`, unit-ising, deleting the planner, hoisting
the case toggle, flat 6-bit, 3-digit numbers, class tables. Everything below came
out of the same brainstorm and is **not** measured — recorded so the options are
not re-derived later.

### What the spare bit is actually good for

If the `uint16` triple is kept anyway, ranked by what earns back the 6.7%:

1. **Literal / extension tag.** `bit = 1` means the other 15 bits are not three
   opcodes but one literal: an integer 0..32767 (five digits, against packed5's
   10-bit 0..1023), or one raw byte plus 7 bits of tag. The stream becomes a
   uniform `uint16` array with exactly one branch per word, and escapes and
   numbers stop being variable-width. The only use that plausibly pays for itself.
2. **Stop bit.** The last word of a frame marks itself, so the stream
   self-terminates. `u5` already gets this effect for free from the header bits,
   which is the cheaper way to buy it.
3. **Group case inversion.** Group boundaries do not line up with word
   boundaries, so it is weak. `u5b`'s header hoist is the cheap version.
4. **Back-reference tag.** 15 bits = an index into a per-column dictionary of
   repeated values. At *column* level this is worth far more than 6.7%: a column
   of city names is mostly repeats. The one that could change the shape of the
   problem rather than shave it.

### The `uint16`'s one genuine advantage

A decode LUT keyed on the whole 16-bit word gives 3 characters per load. The
catch is size: 64K x 4 bytes = 256 KB, cache-hostile on this CPU. The practical
version is a 10-bit key into a 1024-entry, 2-character table — 2 KB, L1-resident.
That works for `wide64` too, so it is not really a `uint16` argument.

### SWAR over the classifier

The negative result on class tables points here. Load 8 bytes; test "all in
`'a'..'z'`" with two masked adds; if it holds, subtract `0x61` per byte and gather
the eight 5-bit values into 40 bits with three shift/or/mask pairs, then one
`PutUint64` and advance 5. Eight characters for roughly a dozen operations
against the current five or so *per* character. It only fires on a run of eight
lowercase letters landing on a group boundary, which is rare for the short words
in this corpus and common in real prose — so it needs a realistic corpus before
it is worth building.

### Deleting the planning pass, in pieces

`u5b` does all of these at once; they are separable if a smaller change is wanted.

- Put `'-'` in the symbol table and `ENABLE_NUMBER_0_1023` disappears entirely,
  halving the planner's four candidates to two.
- Hoist a leading `CASE_TOGGLE_LONG` into a header bit — `u5b`'s trick, and the
  cheapest thing in this whole document relative to what it recovers.
- Or decide case with a SWAR pass accumulating `or`/`and` over `c & 0x20` and
  pick between ALL_LOWER / ALL_UPPER / TITLE / MASK header modes. This catches
  strings that turn uppercase in the middle, which `u5b`'s hoist does not.
- Or write optimistically in one pass and fall back to a raw `memcpy` if the
  packed form fails to beat raw. One pass total, and the never-inflate guarantee
  is decided by writing rather than by predicting.

### Split streams

Emit opcodes into one region and raw escape bytes into a tail region of the
payload. An escape becomes `op + count` (2 units) and the bytes become a
`memcpy` — cheaper than packed5's 11+8n *and* it stops raw bytes from
misaligning the unit grid. At column level this generalises to one opcode
stream, one raw-byte arena and one number stream per column, each homogeneous
and independently vectorisable. Open problem: the frame stops being
self-delimiting unless the raw length is carried up front.

### Alternatives not built

- **`packed6` case-folded.** 26 letters + space + 10 digits + 24 punctuation +
  CASE_NEXT + CASE_LONG + ESC = 64, putting common punctuation at 6 bits. Loses
  to the caseless `p6` on Title Case names (a toggle per capital) and to packed5
  on digit runs.
- **Nibble hybrid.** 16 hot symbols at 4 bits, shifts only ever 0 or 4, ~0.8
  bytes/char.
- **Byte-aligned bigram/BPE codec.** 1 byte = 1 token, 256 entries ≈ 64 singles
  + 192 frequent bigrams/trigrams trained on the corpus. Decode is
  `PutUint32(out[p:], tbl[b]); p += lens[b]` — branchless, one load and one store
  per two or three characters, no bit operations at all. Plausibly 0.55 to 0.65
  bytes/char, i.e. **better compression than packed5 *and* faster than anything
  bit-packed**, at the cost of a trained table baked into the format and worse
  out-of-domain behaviour. The option most likely to beat everything measured
  here on both axes.

### Decode-side ideas, independent of the format

- Replace the opcode `switch` with a 32-entry table of `{4 bytes, length}` and do
  `PutUint32(out[p:], tbl[u].bytes); p += tbl[u].len` — branchless for letters,
  space and every symbol. (Note the class-table result above cuts against this
  on the *encode* side; the decode side has a different shape, since the operand
  is already a small dense index rather than an arbitrary byte.)
- SWAR fast path: test whether all 8 units in a group are `< 26` with one masked
  compare and, if so, emit 8 letters with two `uint64` stores, skipping dispatch.
- Unpack a group branch-free, then walk the units. Two simple passes beat one
  branchy fused pass — which is what `u5Expand` and `p6Decode` already do, and
  part of why their decode numbers are what they are.

---

## Next steps

- **Take `u5b` into `packed5/` proper.** It is faster and smaller than the
  current codec on every corpus here, and the format changes are all things
  pre-alpha status allows. It needs fuzzing and a hostile-input pass first.
- **Get a realistic corpus.** The SWAR classifier and the bigram/BPE codec both
  need one before they can be judged, and the `sku` diagnosis above shows how
  easily a synthetic generator hides or invents an effect.
- **Do not attack classification with a bigger table.** Measured, it loses. The
  next move there is SWAR over 8 bytes.

## Caveats

- The corpus is packed5's own (`packed5_test.go`), reproduced verbatim so the
  numbers line up with its README. It is synthetic: fourteen distinct Spanish
  words, one fixed pangram, `SKU-%04d` from a uniform draw. Fair for comparing
  codecs against each other, and unfair to anything dictionary-based in both
  directions.
- Every kernel here is scalar. `p6` in particular is written to be SIMD-friendly
  and is not SIMD.
- `u5`, `u5b` and `p6` are experiments, not hardened codecs. They round-trip
  every input in `edgeCases` plus 2000 corpus strings, decode correctly
  back-to-back out of one buffer, and never inflate — but they have had no
  fuzzing and their error paths are thinner than packed5's.

## Files

| file | what it holds |
| --- | --- |
| `kernel_test.go` | `dense` / `wide64` / `triple16` pack and unpack, round-trip tests, `ns/unit` benchmarks |
| `frame_test.go` | the shared one-byte header |
| `u5_test.go` | the unit-ised 5-bit codec, staged and fused encoders |
| `u5b_test.go` | `u5` plus the header case hoist, the 3-digit number token, and the class-table classifier |
| `p6_test.go` | the flat 6-bit codec |
| `bench_test.go` | the corpus, the round-trip / never-inflate / cross-encoder tests, end-to-end and plan-split benchmarks |
