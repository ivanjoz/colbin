# column

`github.com/ivanjoz/colbin/column`

colbin's column codec: a transform, then blocks of 128 residuals packed at an
exact bit width chosen per block.

```go
buf := column.AppendArray(nil, []int32{100, 240, 250, 380})
out := make([]int32, 4)
n, err := column.DecodeArray(buf, 4, out)
```

## Why blocks of 128

128 values at `w` bits occupy `128w/8 = 16w` bytes. That is a whole number of
bytes for **every** `w`, and a whole number of 64-bit words too. So:

- a block is byte-aligned at both ends, and no padding is ever wasted;
- no state crosses a block boundary, so a corrupt block cannot derail the next;
- the width leaves the element loop, which is what keeps the read fast;
- the ladder is still one bit fine, which is what keeps the output small.

Those last two are usually a trade, and here they are not.

## Reading a value is one load

Values are packed least-significant-bit first with no gap, so value `i` starts at
bit `i*w`:

```go
v = (binary.LittleEndian.Uint64(buf[pos>>3:]) >> (pos & 7)) & mask
```

One load, one shift, one mask, and **no dependency on the value before it** —
which is what lets the loop run wide. It holds for every `w ≤ 57`: a value that
starts anywhere in a byte and is at most 57 bits wide always finishes inside the
eight bytes that load spans. Widths 58–64 take an out-of-line path, and a column
that reaches them is incompressible anyway.

The cost is that the read touches up to eight bytes past the value it wants.
`DecodeArray` takes a slower gather for the tail of a buffer rather than
requiring the caller to pad it.

## Wire format

```text
column := [header:1] [base: 8 bytes]? block*

header   bits 0-1  transform: raw / delta / frame-of-reference / constant
         bit  2    zigzag applied to residuals
         bits 3-7  reserved, zero

base     delta's first value, or the frame's minimum, zigzagged and in the clear
         at full width — for delta, FOR and constant only

block   := [width: 1 byte, 0..64] [payload]

         a full block is 16 × width bytes; a final partial one is
         ceil(count × width / 8). Width 0 carries no payload at all.
```

A constant column has no blocks: the header is followed by the one value, nine
bytes for any length. An empty column is the single byte `00`.

The element count is not stored. The caller knows it — in colbin, from the
table's row count.

The encoder writes exactly five headers for a non-empty column — `00` and `04`
(raw, without and with zigzag), `05` (delta, always zigzagged), `02` (frame of
reference) and `03` (constant) — and `DecodeArray` refuses every other byte,
including any reserved bit. It does not check that the transform and the block
widths are the ones the encoder would have chosen.

## Transforms

| | residual | base |
|---|---|---|
| raw | `zigzag(v)` if the column has a negative, else `v` | — |
| delta | `zigzag(v[i+1] - v[i])` | `v[0]` |
| frame of reference | `v[i] - min` | `min` |
| constant | none | the value |

Three rules the measurements insisted on:

1. **The base goes in the clear, at full width.** Folding it into the residuals
   instead sets the first block's width from one element, which measured +97% on
   a column of timestamps.
2. **The transform is scored against its blocked cost**, not against the
   column's widest residual. Scoring it the other way picks frame-of-reference
   for a column of small ids and loses 76%; scoring it this way picks delta.
3. **Constant is scored too, not taken on sight.** It is unbeatable on a long
   column and beaten on a short one — three small values are three bytes raw
   against the eight a base costs. And it loses to raw on a column of zeros,
   because a width-0 block carries nothing: 256 zeros are three bytes.

## Sizes

`go test ./column -run TestArraySizeReport -v`, which also pins the transform
each shape selects:

| column, 256 × int64 | raw | encoded | transform |
|---|---:|---:|---|
| tiny values 0..99 | 2048 B | 227 B (11.1%) | raw, width 7 |
| monotonic ids | 2048 B | 171 B (8.3%) | delta, width 5 |
| timestamps (sec) | 2048 B | 235 B (11.5%) | delta, width 7 |
| clustered ±500 | 2048 B | 331 B (16.2%) | FOR, width 10 |
| steps of 200 | 2048 B | 298 B (14.6%) | delta, width 9 |
| random int64 | 2048 B | 2051 B (100.1%) | raw, width 64 |
| all zeros | 2048 B | 3 B (0.1%) | raw, width 0 |
| negatives | 2048 B | 107 B (5.2%) | delta, width 3 |

The framing is one header byte and one width byte per block, so **+0.1% on
incompressible data**: a column never costs more than its raw element width plus
that, which `TestArrayNeverExceedsTheElementWidth` asserts. On a column of one
to three elements the framing is a large share of a small number.

`go test ./column -bench Array` measures encode and decode per element width.

## Element types

`int8`, `int16`, `int32`, `int64`, and defined types over them. Values are
widened to `int64` internally, so **the encoding depends only on the values**: the
same values encode to the same bytes as `[]int8` or as `[]int64`, and decode into
any element type that can hold them. Decoding a value that does not fit the
destination type is an error, never a silent truncation.

`int` is left out because its range differs between platforms: a column written
from an `[]int` on a 64-bit host could hold values a 32-bit host cannot.

colbin's tables reach the codec through `[]int64`: `gatherInts` in
`codec/table.go` widens every integer, bool and float field of a row to `int64`
first — unsigned values by value, `uint64` and floats by bit pattern.

## Tests

- **`TestPackRunRoundtrip`** — every width 0..64 at every partial-block length,
  read back both with slack after the run and with the buffer ending exactly at
  it, so the one-load path and the gather path must agree.
- **`TestAFullBlockIsAWholeNumberOfBytes`** — the arithmetic the layout rests on.
- **`TestArrayTransformChoiceIsOptimal`** — every transform the encoder could
  have picked is written out in full and decoded back; none may be shorter than
  the one it chose.
- **`TestArrayEncodingIsIndependentOfType`** — the same values are the same bytes
  as every element type that holds them, and decode into each.
- **`TestArrayDecodeRejectsOutOfRange`** — a value too wide for the destination
  type is an error under every transform.
- **`TestArrayDecodeRejectsUnwrittenHeaders`** — all 256 header bytes: exactly
  the ones the encoder writes are accepted.
- **`TestArrayNeverExceedsTheElementWidth`** — the +0.1% bound, asserted.
- **`TestArrayElementTypes`** — each width end to end, at its extremes.
- **`TestArrayDecodeGarbage`**, **`TestArrayTruncated`**, **`FuzzArrayDecode`** —
  arbitrary and truncated input must never panic, and every narrower type must
  agree with `int64` on the same bytes.

## The Rust port

`rust/src/column.rs`, which mirrors this package: the same transforms, the same
blocked cost scoring, the same one-load unpack and the same tail gather.

It is pinned by the corpus rather than by description. `rust/vectors/main.go`
encodes fourteen column shapes with **this** codec — the transforms it actually
picks, across all four element widths, with a partial last block — and
`rust/tests/vectors.rs` asserts that Rust produces the same bytes and reads the
same values back. A change to the transform search or to the block layout fails
there until both sides move.
