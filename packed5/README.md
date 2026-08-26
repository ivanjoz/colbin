# packed5

A compact, self-delimiting codec for **short strings** — the kind that show up as
names, SKUs, city fields and labels in DB-row batches. It packs ASCII letters
into 5 bits each, keeps spaces, digits, common punctuation and Spanish accented
characters inside the same bitstream, and falls back to raw bytes whenever the
packed form would not actually be smaller.

```go
import "github.com/ivanjoz/colbin/packed5"

buf := packed5.Append(nil, "el niño comió jamón")  // 17 bytes, from 22
s, n, err := packed5.Decode(buf)                   // s == the original, n == 17
```

`Append` appends one frame and returns the extended slice; `Decode` reads one
frame off the front of a buffer and reports how many bytes it consumed, so
frames concatenate. `Size(s)` reports the frame length without encoding.

## Guarantees

- **Byte-exact.** `Decode(Append(nil, s))` returns `s` for *any* Go string,
  including invalid UTF-8, NUL bytes and arbitrary binary.
- **Never inflates.** A frame is never larger than the raw bytes plus their
  framing. The encoder measures both representations and keeps the smaller.
- **Canonical.** One string has exactly one encoding at a given input; the
  length prefix has a single legal form and the decoder rejects the other.
- **Allocation-free encoding** for strings up to 64 bytes, given room in the
  output slice. Decoding is one allocation: the returned string.

## Wire format

Byte 0 carries the four header flags and, for all but the longest payloads, the
payload length too:

```text
bit  0     PACKED_5              0 = raw UTF-8 payload, 1 = packed stream
bit  1     UPPERCASE_DOMINANT    default case of the packed stream
bit  2     ENABLE_NUMBER_0_1023  opcode 31 is a 10-bit integer, not '-'
bits 3-7   length code           payload byte length, or 31 = "see uvarint"
```

Length code 31 is followed by an LEB128 uvarint. Codes 0..30 hold the length
inline, so **every frame whose payload fits in 30 bytes costs exactly one byte
of framing** — which is most of the strings this codec exists for. The length is
a byte count in both modes, which is what makes a frame self-delimiting.

In raw mode the payload is the original bytes verbatim. In packed mode it is a
bitstream, LSB-first within each byte:

```text
3 bits    padBits: unused bits at the end of the final payload byte
tokens    5-bit opcodes plus their operands
padBits   zero bits
```

### Opcodes

```text
0..25   letter a..z, cased by the current case mode
26      space
27      CASE_TOGGLE_SIMPLE   invert the case of the next letter only     (5 + 5 bits)
28      CASE_TOGGLE_LONG     invert the case mode until the next 28      (5 bits)
29      + 5-bit symbol index                                             (5 + 5 bits)
30      + 4-bit simple index; index 15 starts a UTF-8 escape             (5 + 4 bits)
31      + 10-bit integer if ENABLE_NUMBER_0_1023, else '-'          (5 + 10 / 5 bits)
```

Symbol table (opcode 29), indices 30 and 31 reserved:

```text
< > / " ' % # | ( ) ! ? $ ~ ` € @ \ [ ] ^ { } _ ñ á é í ó ú
```

Simple table (opcode 30): `0`–`9` `.` `-` `+` `*` `=`, then the escape.

## Three things the specification left open

**Stream termination.** Every 5-bit value is a legal opcode, so trailing zero
padding would decode as extra `a`s. The stream therefore opens with a 3-bit pad
count; the decoder derives the exact token bit count as
`8*len(payload) - 3 - padBits` and stops there. Three bits is the cheapest
possible terminator, and it is paid once per frame rather than once per token.

**UTF-8 escape framing.** Opcode 30 with operand 15, then a 2-bit count `n-1`,
then `n` raw bytes for `n` in 1..4 — `11 + 8n` bits. The escape carries *bytes*,
not runes. That is what makes the codec byte-exact for input that is not valid
UTF-8, and it lets the encoder amortise the 11-bit header over a run of up to
four bytes instead of paying it per rune.

**Frame delimiting.** The header byte and length prefix described above. The
specification defines the packed stream but not how a value is found in a
buffer, so this is an addition.

## The encoder is a single greedy scan

One left-to-right pass, following the specification's rules:

- an opposite-case run of one or two letters takes a `CASE_TOGGLE_SIMPLE` each;
  three or more takes a `CASE_TOGGLE_LONG`.
- a decimal run takes the longest prefix `NUMBER_0_1023` can legally carry —
  except that a *lone* digit takes the cheaper 4-bit simple symbol, since 9 bits
  beats 15.
- a byte with no token of its own is escaped, merged with the following
  unrepresentable bytes up to the escape's four-byte limit.

The two behavioural header flags are not guessed. `UPPERCASE_DOMINANT` and
`ENABLE_NUMBER_0_1023` change what the scan emits, so a single planning pass
computes the exact greedy cost of all four candidate settings. Knowing the
winner and its exact bit count, the encoder then walks the string a second time
and writes the bits as it goes: there is no token list in between, and the frame
is sized before a single bit is written.

Counting letters to pick the dominant case is the obvious shortcut, and it is
wrong often enough to matter: in `SKU-0421-azul` the lowercase letters win 4 to
3, but starting *uppercase* is a byte smaller, because what decides the flag is
the number of case **runs**, not of letters.

### How far from optimal is it?

The size-optimal encoder for this format is a shortest path over
`(offset, case mode)` nodes. This package deliberately does not ship one — it is
about 2.3x slower — but `encode_test.go` keeps a brute-force reference and uses
it as an oracle, so the distance from optimal is measured and enforced rather
than asserted:

- **On every corpus of realistic short strings, the scan is byte-for-byte
  identical to the optimum.** `TestOptimalOnRealisticStrings` fails if a change
  ever costs a single byte on ~4MB of names, SKUs, Spanish text, sentences and
  paragraphs.
- On synthetic input built to hurt it, the gap is small and bounded by
  `TestGapFromOptimal`:

```text
pool          strings hurt   bytes lost   worst case
words                0.00%        0.00%   +0
binary               0.00%        0.00%   +0
alphabet             9.47%        0.52%   +3 on "aá€ \xffZ\x00b€\x00"
digits              13.28%        0.95%   +4 on "a9a0AAA1aa10a1-9aa999919aa-09-"
case-heavy          19.45%        1.50%   +4 on "caCBaCBAcbCacAabAacCbaAaaCBaA"
case+symbol         24.66%        2.13%   +5 on "ABA-b.aaaBBbAB-.B-bBA-a-BAaB-B"
```

The one shape it misses is dense case changes interleaved with symbols, where a
run-length rule cannot see far enough. `TestKnownGapCaseAcrossSymbols` records
it: in `ab-CD-EF-gh` the four uppercase letters form two runs of two, so the scan
spends four simple toggles (20 bits) where two long toggles spanning the symbols
would cost 10 — one byte on the wire.

## Measured results

Frame size against raw byte length, including framing:

```text
 raw  enc  flags   input
   5    5  p5      "hello"
  10    9  p5      "helloWorld"
  10    8  p5+N    "product123"
  19   14  p5      "the quick brown fox"
  19   14  p5+U    "THE QUICK BROWN FOX"
  22   17  p5      "el niño comió jamón"
  25   22  p5+N    "Móvil Samsung Galaxy S23"
  17   13  p5+N    "Factura 2024-1023"
  21   18  p5      "user.name@example.com"
  24   26  raw     "{\"id\":1023,\"name\":\"ana\"}"
```

`p5+U` is `UPPERCASE_DOMINANT`, `p5+N` is `ENABLE_NUMBER_0_1023`. The last row is
the fallback working as intended: quote-and-brace-heavy JSON has no packed
representation that beats its bytes, so the frame is the raw string plus two
framing bytes.

Throughput on an i7-1355U, Go 1.27, over ~1MB corpora of short strings:

```text
                           encode      decode    packed
name      "Lima norte"  5.5 ms/MB   7.0 ms/MB      0.79
sku    "SKU-0421-azul"  6.3 ms/MB   6.9 ms/MB      0.89
spanish  "el niño ..."  4.7 ms/MB   4.5 ms/MB      0.73
sentence  5 words       6.1 ms/MB   5.6 ms/MB      0.73
paragraph 258 bytes     3.0 ms/MB   4.0 ms/MB      0.64
```

So roughly **5 ms/MB to encode and 6 ms/MB to decode** — about 200 MB/s and
170 MB/s. The `sku` row is the slow end of encoding: a decimal run makes the
number-mode half of the planning pass do real work, and its dense case changes
give the writing pass the most to decide.

Per-call figures, for the short strings the codec targets:

```text
BenchmarkAppend/len5             22.6 ns/op     0 B/op   0 allocs/op
BenchmarkAppend/len10            39.3 ns/op     0 B/op   0 allocs/op
BenchmarkAppend/len22            97.0 ns/op     0 B/op   0 allocs/op
BenchmarkAppend/len43             125 ns/op     0 B/op   0 allocs/op

BenchmarkDecode/len5             34.1 ns/op     5 B/op   1 allocs/op
BenchmarkDecode/len10            55.5 ns/op    16 B/op   1 allocs/op
BenchmarkDecode/len22            93.7 ns/op    24 B/op   1 allocs/op
BenchmarkDecode/len43             161 ns/op    48 B/op   1 allocs/op
```

Encoding allocates nothing at any length, and needs no scratch to do it: the
planning pass carries four running costs in registers, and the writing pass goes
straight to the output slice, which the plan has already sized exactly. Neither
pass builds a token list, so there is no stack buffer to zero and no pool to
draw from.

The single decode allocation is the returned string. Results up to 256 bytes are
built on the stack; the buffer is sized from the format's own expansion bound
(the three-byte `€` symbol at 3 bytes per 10 bits, so at most 2.4x), so the
append loop never has to grow.

`AppendDecoded` is the same decoder writing onto a caller's buffer instead. A
caller reading a run of frames can gather them all into one backing array and
cut the strings out of it, trading one allocation per value for one per run;
`colbin` decodes string columns that way.

## Tests

`go test ./packed5/` runs in under a second and covers 100% of statements.

- **Roundtrip.** A table of 56 named cases; all 256 single-byte inputs; all
  65536 two-byte inputs exhaustively; all 17576 three-symbol combinations over a
  mixed-width alphabet; random strings from five character pools; random bytes;
  random runes; every length from 0 to 600 for seven repeating units.
- **Distance from optimal.** The scan against a brute-force reference, as
  described above: byte-identical on realistic corpora, bounded elsewhere.
- **Structure.** Token-level assertions, via a test-only disassembler: which
  toggle a run of each length uses, that the toggle decision spans symbols, that
  escape runs merge and swallow encodable bytes, that every symbol-table entry
  reaches its own opcode, and the exact bit cost of the specification's own
  worked examples, and the lone-digit token rule.
- **The leading-zero rule.** `00123` must not come back as `123`. Checked
  exhaustively over every decimal string up to six digits.
- **Decoder robustness.** Every documented error path; every truncation prefix
  of six valid frames; every single-bit flip of seven valid frames; 200000
  random buffers (of which about 48000 decode, 7000 through the packed path);
  and a decompression-bomb bound driven by the two densest tokens.
- **Concurrency.** 16 goroutines encoding and decoding long strings at once,
  under `-race`.
- **Fuzzing.** `FuzzRoundtrip` and `FuzzDecode`. 635k and 10.2M executions
  respectively with no failures.

## Status

The format is complete and the wire layout is fixed by the three decisions
above. Reserved space is left for growth: header bit 3, and symbol indices 30
and 31 — both rejected by the decoder today, so claiming them later is a clean
format change rather than a silent reinterpretation.
