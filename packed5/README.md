# packed5

A compact encoding for **short strings** — the kind that show up as names, SKUs,
city fields and labels in DB-row batches. It packs ASCII letters into 5 bits
each, keeps spaces, digits, common punctuation and Spanish accented characters
inside the same stream, and is only used when it is smaller than the raw bytes.

```go
import "github.com/ivanjoz/colbin/packed5"

buf, size, upper, ok := packed5.AppendPayload(buf, "el niño comió jamón") // 14 bytes, from 22
// store size and upper beside the payload; when !ok, store the string raw
out, err := packed5.AppendString(dst, src, size, upper) // src starts at the payload
```

## The container carries the length and the case mode

A payload has no header of its own. `AppendPayload` reports its byte length and
the case mode the stream opens in, and the container stores both; `AppendString`
takes them back. colbin's `BLOB` descriptor already carries a size and an
encoding code, so a packed string costs no more framing than a raw one:

- under eight-bit keys, the descriptor's `enc` code says raw (0), packed and
  opening lowercase (1), or packed and opening uppercase (3);
- under four-bit keys, which have no `enc` field, the blob header's escape code
  says the same.

See `wire/packed.go`. Because the choice is per string, a string that does not
pack is written raw and the descriptor says so.

## Everything is a whole number of units

A **unit** is one 5-bit code, and every token is a whole number of them. That is
the property the codec is built around, because **8 units are 40 bits are 5 bytes
exactly**: the packer builds one `uint64` with eight compile-time shifts, stores
eight bytes and advances five. No bit accumulator, no variable-width drain, no
token that straddles the byte grid at an offset the compiler cannot see.

The cost is slack. The writer needs eight spare bytes past the payload, which
`AppendPayload` reserves. The reader wants up to seven readable bytes past it:
`AppendString` takes them from whatever follows the payload in `src` — so pass the
rest of the message — and otherwise copies the last group once into a small
buffer.

## Guarantees

- **Byte-exact.** `AppendString` returns the bytes `AppendPayload` was given, for
  *any* Go string, including invalid UTF-8, NUL bytes and arbitrary binary.
- **Never inflates.** `ok` is true only when the payload is strictly smaller than
  the string. The encoder writes into a buffer bounded by the raw length and
  gives up the moment it would pass it; on `!ok` nothing is appended.
- **Bounded decoding.** Any `src`, `size` and case mode either decode to some
  string or return an error — never a panic, never a read past `src`, and never
  more than 2.4 output bytes per payload byte, plus four. Both directions are
  fuzzed.
- **Allocation-free** in both directions, given capacity: `AppendPayload` writes
  straight into the caller's slice and needs `len(s)+8` spare bytes;
  `AppendString` appends onto `dst`.

The decoder does not check that a payload is the one the encoder would have
written — a letter spelled as a raw escape decodes to that letter. A container
that needs one byte sequence per string must not take payloads from elsewhere.

## Payload layout

The payload is the unit stream, five bits per unit, LSB-first within each byte,
**starting at bit 0 of the first byte**. There is no pad prefix and no pad count:
the unit count follows from the length alone.

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

## Two things the specification left open

**Stream termination.** Every 5-bit value is a legal opcode, so trailing zero
padding would decode as extra `a`s. The encoder pads the stream to the unit grid
with a trailing `CASE_TOGGLE_SIMPLE` instead. A simple toggle applies to the next
letter, so one with no letter after it decodes to nothing — a no-op the alphabet
already had. It costs no bytes, because the gap exists exactly when the rounding
of `5 * units` bits up to a whole byte leaves room for one more unit.

**Raw escape.** Opcode 30 with operand 31, then one unit holding `n-1` for `n` in
1..4, then two units per raw byte — the low five bits, then the high three:
`3 + 2n` units. The escape carries *bytes*, not runes. That is what makes the
codec byte-exact for input that is not valid UTF-8, and it lets the encoder
amortise the three-unit header over a run of up to four bytes instead of paying
it per rune.

## The encoder is a single greedy scan

One left-to-right pass, fused with the packer:

- an opposite-case run of one or two letters takes a `CASE_TOGGLE_SIMPLE` each;
  three or more takes a `CASE_TOGGLE_LONG`.
- a decimal run takes the longest prefix the number token can legally carry —
  except that a *lone* digit takes the symbol table instead, since 10 bits beat 15.
- a byte with no token of its own is escaped, merged with the following
  unrepresentable bytes up to the escape's four-byte limit.

Nothing is priced before the scan runs. The case mode is the first letter's
case, decided by a peek. It is never worse than starting lower: a leading run of
`k` opposite-case letters costs `2k` units from lower for `k` of one or two and
`1+k` from three up, where starting in that mode costs `k` plus the one toggle
that returns — equal at `k>=3` and strictly better below it, with both encoders
in the same state afterwards.

### How far from optimal is it?

The size-optimal encoder for this format is a shortest path over
`(offset, case mode)` nodes. This package deliberately does not ship one, but
`encode_test.go` keeps a brute-force reference and uses it as an oracle, so the
distance from optimal is measured and enforced rather than asserted:

- **On every corpus of realistic short strings, the scan is byte-for-byte
  identical to the optimum.** `TestOptimalOnRealisticStrings` fails if a change
  ever costs a single byte on names, SKUs, Spanish text, sentences and paragraphs.
- On synthetic input built to hurt it, the gap is small and bounded by
  `TestGapFromOptimal` (`go test ./packed5 -run TestGapFromOptimal -v`; 20000
  strings of 1–30 symbols per pool, payload bytes):

```text
pool          strings hurt   bytes lost   worst case
words                0.00%        0.00%   +0
alphabet             1.93%        0.12%   +3 on "/Z=a%z@%a+.b+a€9z€ñ%á < 0"
digits              11.37%        0.93%   +5 on "-A900919a90a90a1a9aAa9a9909a0a"
case-heavy          22.93%        1.97%   +4 on "CABcbbCCaBAbACbABaACcCcAAcBBB"
case+symbol         14.90%        1.14%   +3 on "-aAA.AABABB-AAbb-ba"
binary               0.01%        0.00%   +1 on "aaaZZ"
```

The shape it misses is dense case changes interleaved with symbols, where a
run-length rule cannot see far enough. `TestKnownGapCaseAcrossSymbols` bounds it:
in `abab-CD-EF-ghgh` the four uppercase letters form two runs of two, so the scan
spends four simple toggles where two long toggles spanning the symbols would cost
two units less.

## Sizes

What representative strings cost, pinned by `TestPayloadSizes`. "stored" is the
payload, or the raw bytes when the string does not pack; the container's own
framing is not included.

```text
 raw  stored  mode  input
   5       4  p5    "hello"
  10       7  p5    "helloWorld"
  10       8  p5    "fooBARTest"
  10       7  p5    "product123"
  12      12  raw   "SKU-00042-XL"
  22      14  p5    "el niño comió jamón"
  19      12  p5    "the quick brown fox"
  19      12  p5+U  "THE QUICK BROWN FOX"
  21      15  p5    "user.name@example.com"
  24      22  p5    "{\"id\":1023,\"name\":\"ana\"}"
  10       7  p5    "€1023.45"
   7       5  p5+U  "Bogotá"
  25      19  p5+U  "Móvil Samsung Galaxy S23"
  17      12  p5+U  "Factura 2024-1023"
```

`SKU-00042-XL` is the format's weak spot: a leading zero cannot go through a
number token — `"00042"` would decode as `"42"` — so it shreds into lone digits at
two units each, and the string does not pack. A fixed-width three-digit token
fixes that case and was measured: worth 0.06% on a SKU corpus and a loss wherever
digit runs are one or two long, so it is not in the format.

`go test ./packed5 -bench .` measures both directions per call.

## Tests

`go test ./packed5/` runs in under a second and covers every statement.

- **Roundtrip.** A table of 56 named cases; all 256 single-byte inputs; all 65536
  two-byte inputs exhaustively; all 17576 three-symbol strings over a 26-symbol
  alphabet; every length from 0 to 600 for seven repeating units; random strings
  from five character pools; random bytes and runes. Every one is decoded twice,
  with slack after the payload and with the buffer ending exactly at it.
- **A second implementation.** `tokens_test.go` builds every payload the long way
  round — tokens, then units, then bits one at a time — and
  `TestFusedMatchesReference` pins `AppendPayload` against it over 24000 strings.
  `TestGroupPackerMatchesBitPacker` isolates the kernel from the tokeniser.
- **Distance from optimal.** The scan against a brute-force shortest path, as
  above: byte-identical on realistic corpora, bounded elsewhere. Every random
  input is seeded per pool, so the figures above reproduce exactly.
- **Structure.** Token-level assertions through a disassembler: which toggle a run
  of each length uses, that the case mode follows the first letter and the
  stream never opens by toggling, that escape runs merge, that every table entry
  reaches its own opcode, and the exact unit cost of worked examples.
- **The leading-zero rule.** `00123` must not come back as `123`. Checked
  exhaustively over every decimal string up to six digits.
- **The grid pad.** That a trailing simple toggle decodes to nothing in either
  case mode, over every payload length.
- **Decoder robustness.** Every documented error path, including negative and
  oversized sizes; every truncation prefix; every single-bit flip of seven valid
  payloads in both case modes; 200000 random payloads; slack that is never
  decoded; and a decompression-bomb bound driven by the densest token.
- **Fuzzing.** `FuzzRoundtrip`, with no spare capacity in any buffer, and
  `FuzzAppendString`, over arbitrary bytes, sizes and case modes.

## Status

The format is complete. Extension table indices 28, 29 and 30 are reserved and
rejected by the decoder today, so claiming them later is a clean format change
rather than a silent reinterpretation.

`rust/src/packed5.rs` is the same codec in Rust, and the browser module,
`rust/wasm`, links it; there is no second implementation. `rust/vectors` holds the
two to the same bytes through `wire`'s packed string fields.
