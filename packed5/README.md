# packed5

A compact, self-delimiting codec for **short strings** — the kind that show up as
names, SKUs, city fields and labels in DB-row batches. It packs ASCII letters
into 5 bits each, keeps spaces, digits, common punctuation and Spanish accented
characters inside the same stream, and falls back to raw bytes whenever the
packed form would not actually be smaller.

```go
import "github.com/ivanjoz/colbin/packed5"

buf := packed5.Append(nil, "el niño comió jamón")  // 15 bytes, from 22
s, n, err := packed5.Decode(buf)                   // s == the original, n == 15
```

`Append` appends one frame and returns the extended slice; `Decode` reads one
frame off the front of a buffer and reports how many bytes it consumed, so frames
concatenate. `Size(s)` reports the frame length. For a container that already
carries a length and a flag — a colbin `BLOB` descriptor does both — `AppendPayload`
and `AppendString` write and read the bare stream with no frame header at all.

## Everything is a whole number of units

A **unit** is one 5-bit code, and every token is a whole number of them. That is
the property the codec is built around, because **8 units are 40 bits are 5 bytes
exactly**: the packer builds one `uint64` with eight compile-time shifts, stores
eight bytes and advances five. No bit accumulator, no variable-width drain, no
token that straddles the byte grid at an offset the compiler cannot see.

Measured in isolation against the accumulator this replaced — which already
drained 32 bits at a time, so it was writing whole bytes — the group kernel is
**3.4x faster to pack and 5.5x faster to unpack**, for byte-identical output.
`experiments/stringpack` has the standalone benchmark.

The cost is slack: a writer needs eight spare bytes past the payload, which it
reserves, and a reader wants up to seven readable past it. In a run of
back-to-back frames the next frame supplies that for free; the last frame falls
to a bounded copy of its final bytes.

## Guarantees

- **Byte-exact.** `Decode(Append(nil, s))` returns `s` for *any* Go string,
  including invalid UTF-8, NUL bytes and arbitrary binary.
- **Never inflates.** The encoder writes into a buffer bounded by the raw length
  and abandons the packed form the moment it would pass it, so a frame is never
  larger than the raw bytes plus their framing.
- **Canonical.** One string has exactly one encoding; the length prefix has a
  single legal form and the decoder rejects the other.
- **Allocation-free encoding at any length**, given room in the output slice.
  There is no scratch: the walk is fused with the packer and writes straight into
  the caller's buffer. Decoding is one allocation, the returned string.

## Wire format

Byte 0 carries the header flags and, for all but the longest payloads, the
payload length too:

```text
bit  0     PACKED_5     0 = raw payload, 1 = packed unit stream
bit  1     UPPERCASE    the stream starts in uppercase mode
bit  2     reserved     must be zero
bits 3-7   length code  payload byte length, or 31 = "see uvarint"
```

Length code 31 is followed by an LEB128 uvarint. Codes 0..30 hold the length
inline, so **every frame whose payload fits in 30 bytes costs exactly one byte of
framing** — which is most of the strings this codec exists for. The length is a
byte count in both modes, which is what makes a frame self-delimiting.

In raw mode the payload is the original bytes verbatim. In packed mode it is the
unit stream, five bits per unit, LSB-first within each byte, **starting at bit 0
of the first payload byte**. There is no pad prefix and no pad count: the unit
count follows from the length alone.

```text
units = len(payload) * 8 / 5
```

### Opcodes

```text
0..25   letter a..z, cased by the current case mode          (1 unit)
26      space                                                (1 unit)
27      CASE_TOGGLE_SIMPLE   invert the next letter only      (1 unit)
28      CASE_TOGGLE_LONG     invert the mode until the next   (1 unit)
29      + symTable index                                      (2 units)
30      + extTable index, or 31 to start a raw escape         (2 units)
31      + a 10-bit integer 0..1023, low five bits first       (3 units)
```

Symbol table (opcode 29). All 32 entries are assigned, so no operand of this
opcode can be invalid:

```text
0 1 2 3 4 5 6 7 8 9 . , - / : ; _ ( ) % # " ' ! ? @ = + * & < >
```

Extension table (opcode 30), indices 28..30 reserved and 31 the escape:

```text
ñ á é í ó ú ü Ñ Á É Í Ó Ú € $ ~ ` \ [ ] ^ { } | \n \t \r ¿
```

## Three things the specification left open

**Stream termination.** Every 5-bit value is a legal opcode, so trailing zero
padding would decode as extra `a`s. The encoder pads the stream to the unit grid
with a trailing `CASE_TOGGLE_SIMPLE` instead. A simple toggle applies to the next
letter, so one with no letter after it decodes to nothing — a no-op the alphabet
already had. It costs no bytes, because the gap exists exactly when the rounding
of `5 * units` bits up to a whole byte leaves room for one more unit.

That is the change that makes the grid work. The previous format opened the
payload with a 3-bit pad count, which put every token in the frame three bits off
the byte grid and cost three bits per frame besides.

**Raw escape.** Opcode 30 with operand 31, then one unit holding `n-1` for `n` in
1..4, then two units per raw byte — the low five bits, then the high three:
`3 + 2n` units. The escape carries *bytes*, not runes. That is what makes the
codec byte-exact for input that is not valid UTF-8, and it lets the encoder
amortise the three-unit header over a run of up to four bytes instead of paying
it per rune.

**Frame delimiting.** The header byte and length prefix above. The specification
defines the packed stream but not how a value is found in a buffer, so this is an
addition — and one an embedded string does not need; see below.

## The encoder is a single greedy scan

One left-to-right pass, fused with the packer, following the specification's
rules:

- an opposite-case run of one or two letters takes a `CASE_TOGGLE_SIMPLE` each;
  three or more takes a `CASE_TOGGLE_LONG`.
- a decimal run takes the longest prefix the number token can legally carry —
  except that a *lone* digit takes the symbol table instead, since 10 bits beat 15.
- a byte with no token of its own is escaped, merged with the following
  unrepresentable bytes up to the escape's four-byte limit.

**There is no planning pass.** The format this replaced had two behavioural
header flags — a default case and a number mode — which changed what the scan
emitted and so had to be priced before it ran, in a second full walk of the
string. That walk measured **32% to 46% of encode time**. Both flags are gone:
the number token is unconditional, and the case mode is an ordinary
`CASE_TOGGLE_LONG` that the encoder hoists into the header bit when it would have
landed first.

Picking the case mode is now the first letter's case, decided by a peek rather
than by pricing. It is never worse than starting lower: a leading run of `k`
opposite-case letters costs `2k` units from lower for `k` of one or two and `1+k`
from three up, where starting in that mode costs `k` plus the one toggle that
returns — equal at `k>=3` and strictly better below it, with both encoders in the
same state afterwards.

### How far from optimal is it?

The size-optimal encoder for this format is a shortest path over
`(offset, case mode)` nodes. This package deliberately does not ship one, but
`encode_test.go` keeps a brute-force reference and uses it as an oracle, so the
distance from optimal is measured and enforced rather than asserted:

- **On every corpus of realistic short strings, the scan is byte-for-byte
  identical to the optimum.** `TestOptimalOnRealisticStrings` fails if a change
  ever costs a single byte on names, SKUs, Spanish text, sentences and paragraphs.
- On synthetic input built to hurt it, the gap is small and bounded by
  `TestGapFromOptimal`:

```text
pool          strings hurt   bytes lost   worst case
words                0.00%        0.00%   +0
binary               0.00%        0.00%   +0
alphabet             2.15%        0.14%   +4 on "9B\x00Z_AbB€<aá=A+€az.%zz1aáñ"
digits              11.17%        0.86%   +4 on "-0-1A191009991a9aa9-aaA-9a190a"
case-heavy          22.77%        1.81%   +3 on "CBacAcacacAAcBCBbbBaa"
case+symbol         14.80%        1.07%   +4 on "BA.A.baabBaAabab.ABbAB.BBaA.B"
```

Against the pre-unit format, which scored 9.47/0.52 on alphabet, 13.28/0.95 on
digits, 19.45/1.50 on case-heavy and 24.66/2.13 on case+symbol: better on three
of the four, and a little worse on the one where the case rule does all the work.

The shape it misses is dense case changes interleaved with symbols, where a
run-length rule cannot see far enough. `TestKnownGapCaseAcrossSymbols` records it:
in `ab-CD-EF-gh` the four uppercase letters form two runs of two, so the scan
spends four simple toggles where two long toggles spanning the symbols would cost
two units less.

## Embedded in a container: no frame header

`AppendPayload` writes the bare unit stream and reports its byte length and case
mode; `AppendString` reads one back. They exist because a colbin `BLOB`
descriptor already carries a size and an encoding code, which makes all three of
the frame header's live fields duplicates.

```go
buf, size, upper, ok := packed5.AppendPayload(w.Buffer, value)
// ... the container records size and upper, and writes no frame header
out, err := packed5.AppendString(dst, src, size, upper)
```

`wire` uses this under both key widths. Under eight-bit keys the case mode rides
in the descriptor's spare `enc` code; under four-bit keys, which have no `enc`
field at all, it rides in the blob header's escape code. That second one is why
**packed5 no longer forces a message to eight-bit keys** — it used to, because a
narrow blob header had nowhere to say a string was packed, and the whole message
paid a byte per key to buy one. Measured on the corpus, turning packed5 on used
to save 1.0% of a record and now saves **13.5%**.

## Measured results

An i7-1355U, Go 1.27. "before" is the pre-unit format measured in the same
session, not quoted from an older run.

Frame size against raw byte length, including framing:

```text
 raw  before  now  flags  input
   5       5    5  p5     "hello"
  10       9    8  p5     "helloWorld"
  10       9    9  p5     "fooBARTest"
  10       8    8  p5     "product123"
  12      12   13  raw    "SKU-00042-XL"
  22      16   15  p5     "el niño comió jamón"
  19      14   13  p5     "the quick brown fox"
  19      14   13  p5+U   "THE QUICK BROWN FOX"
  21      17   16  p5     "user.name@example.com"
  24      25   23  p5     "{\"id\":1023,\"name\":\"ana\"}"
  10       8    8  p5     "€1023.45"
   7       7    6  p5+U   "Bogotá"
  25      21   20  p5+U   "Móvil Samsung Galaxy S23"
  17      13   13  p5+U   "Factura 2024-1023"
```

Eight smaller, five unchanged, one worse. The JSON row now packs at all, because
braces, quotes, colons and commas have symbol tokens where the old simple table
had no room for them. The `SKU-00042-XL` row is the one regression and it is the
format's weak spot: a leading zero cannot go through a number token — `"00042"`
would decode as `"42"` — so it shreds into lone digits at two units each. A
fixed-width three-digit token fixes that case and was measured; it is worth 0.06%
on a real SKU corpus and a loss wherever digit runs are one or two long, so it is
not in the format. `experiments/stringpack` records the measurement.

Throughput over ~1MB corpora of short strings:

```text
                          encode ms/MB      decode ms/MB        ratio
                         before    now    before    now    before    now
name      "Lima norte"     5.31   3.83      6.51   5.76     0.791  0.765
sku    "SKU-0421-azul"     6.49   3.76      6.38   5.22     0.893  0.894
spanish  "el niño ..."     4.36   2.40      4.35   3.40     0.697  0.677
sentence  5 words          5.76   3.41      5.50   4.41     0.701  0.690
paragraph 258 bytes        2.90   1.78      4.66   3.02     0.640  0.636
```

**1.4x to 1.8x faster to encode and 1.1x to 1.5x faster to decode, smaller on
four corpora out of five and level on the fifth.** The encode win is the deleted
planning pass and the group packer together; the size win is mostly the pad
prefix moving out of the payload, which is three bits on every frame.

Per-call figures, for the short strings the codec targets:

```text
                      before      now
BenchmarkAppend/len5    20.5     20.3 ns/op    0 B/op   0 allocs/op
BenchmarkAppend/len10   38.5     27.1 ns/op    0 B/op   0 allocs/op
BenchmarkAppend/len22   93.3     45.2 ns/op    0 B/op   0 allocs/op
BenchmarkAppend/len43  116.9     78.3 ns/op    0 B/op   0 allocs/op

BenchmarkDecode/len5    30.2     32.9 ns/op    5 B/op   1 allocs/op
BenchmarkDecode/len10   51.4     47.7 ns/op   16 B/op   1 allocs/op
BenchmarkDecode/len22   89.8     72.7 ns/op   24 B/op   1 allocs/op
BenchmarkDecode/len43  145.0    115.0 ns/op   48 B/op   1 allocs/op
```

The one number that got worse is a five-character decode, by about two
nanoseconds: the group reader keeps a small window on the stack where the bit
reader kept none, and on a four-byte payload there is nothing to amortise it
over. It turns positive by ten characters.

The single decode allocation is the returned string. Results up to 256 bytes are
built on the stack; the buffer is sized from the format's own expansion bound —
the three-byte `€` at three bytes per two units, so at most 2.4x — and the append
loop never has to grow.

`AppendDecoded` is the same decoder writing onto a caller's buffer. A caller
reading a run of frames can gather them into one backing array and cut the
strings out of it, trading one allocation per value for one per run; `colbin`
decodes string columns that way, and it is also the shape that gives the group
reader its slack for free.

## Tests

`go test ./packed5/` runs in under a second and covers 99.7% of statements.

- **Roundtrip.** A table of 56 named cases; all 256 single-byte inputs; all 65536
  two-byte inputs exhaustively; every length from 0 to 600 for seven repeating
  units; random strings from five character pools; random bytes and runes.
- **A second implementation.** `tokens_test.go` builds every frame the long way
  round — tokens, then units, then bits one at a time — and
  `TestFusedMatchesReference` pins the fused encoder against it over 24000
  strings. `TestGroupPackerMatchesBitPacker` isolates the kernel from the
  tokeniser.
- **Distance from optimal.** The scan against a brute-force shortest path, as
  above: byte-identical on realistic corpora, bounded elsewhere.
- **Structure.** Token-level assertions through a disassembler: which toggle a run
  of each length uses, that the header bit is hoisted and the stream never opens
  by toggling, that escape runs merge, that every table entry reaches its own
  opcode, and the exact unit cost of worked examples.
- **The leading-zero rule.** `00123` must not come back as `123`. Checked
  exhaustively over every decimal string up to six digits.
- **The grid pad.** That a trailing simple toggle decodes to nothing in either
  case mode, over every payload length.
- **Decoder robustness.** Every documented error path; every truncation prefix;
  every single-bit flip of seven valid frames; 200000 random buffers; and a
  decompression-bomb bound driven by the densest token.
- **Concurrency.** 16 goroutines encoding and decoding at once, under `-race`.
- **Fuzzing.** `FuzzRoundtrip` and `FuzzDecode`.

## Status

The format is complete and the wire layout is fixed by the three decisions above.
Reserved space is left for growth: header bit 2, and extension table indices 28,
29 and 30 — all rejected by the decoder today, so claiming them later is a clean
format change rather than a silent reinterpretation.

`rust/` is ported and `cargo test --features derive` passes, including the
cross-language vectors that hold the two implementations to the same bytes.

The browser module is `rust/wasm`, which links `rust/src/packed5.rs` — the same
codec the rest of the crate uses. There is no second packed5 implementation.
