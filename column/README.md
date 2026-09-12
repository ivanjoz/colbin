# column

`github.com/ivanjoz/colbin/column`

colbin's column codec: a transform, then blocks of 128 residuals packed at an
exact bit width chosen per block.

```go
buf := varint.AppendArray(nil, []int32{100, 240, 250, 380})
out := make([]int32, 4)
n, err := varint.DecodeArray(buf, 4, out)
```

## Why blocks of 128

128 values at `w` bits occupy `128w/8 = 16w` bytes. That is a whole number of
bytes for **every** `w`, and a whole number of 64-bit words too. So:

- a block is byte-aligned at both ends, and no padding is ever wasted;
- no state crosses a block boundary, so a corrupt block cannot derail the next;
- the width leaves the element loop, which is what keeps the read fast;
- the ladder is still one bit fine, which is what keeps the output small.

Those last two are usually a trade and here they are not, which is the whole
reason this layout replaced the `(k, M)` bit varint that used to live here.

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
         bits 3-7  reserved

base     delta's first value, or the frame's minimum, zigzagged and in the clear
         at full width — for delta and FOR only

block   := [width: 1 byte, 0..64] [payload]

         a full block is 16 × width bytes; a final partial one is
         ceil(count × width / 8). Width 0 carries no payload at all.
```

A constant column has no blocks: the header is followed by the one value, nine
bytes for any length.

The element count is not stored. It comes from the record count, matching the
convention of the other colbin column codecs.

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
   column's widest residual. Scoring it the old way picks frame-of-reference for
   a column of small ids and loses 76%; scoring it this way picks delta.
3. **Constant is scored too, not taken on sight.** It is unbeatable on a long
   column and beaten on a short one — three small values are three bytes raw
   against the eight a base costs. And it loses to raw on a column of zeros,
   because a width-0 block carries nothing: 256 zeros are three bytes.

## Measured results

i7-1355U, Go 1.27. Sizes are `go test ./column -run TestArraySizeReport -v`;
throughput is `go test ./column -bench Array`, 1024 elements per column.

| column, 256 × int64 | raw | encoded | transform |
|---|---:|---:|---|
| tiny values 0..99 | 2048 B | 227 B (11.1%) | raw, width 7 |
| monotonic ids | 2048 B | 171 B (8.3%) | delta, width 5 |
| timestamps (sec) | 2048 B | 235 B (11.5%) | delta, width 7 |
| clustered ±500 | 2048 B | 331 B (16.2%) | FOR, width 10 |
| random int64 | 2048 B | 2051 B (100.1%) | raw, width 64 |
| all zeros | 2048 B | 3 B (0.1%) | raw, width 0 |
| negatives | 2048 B | 107 B (5.2%) | delta, width 3 |

Against the `(k, M)` bit varint this replaced, over the five shapes in
`experiments/bytealigned`: **smaller on every one of them**, from −59% to +1%.

| 1024 elements | old `(k, M)` | blocked | |
|---|---:|---:|---|
| encode int16 | 14.3 µs | 3.4 µs | 4.2× |
| encode int32 | 15.6 µs | 3.3 µs | 4.7× |
| encode int64 | 17.4 µs | 3.4 µs | 5.1× |
| decode int16 | 3.80 µs | 1.31 µs | 2.9× |
| decode int32 | 3.74 µs | 1.32 µs | 2.8× |
| decode int64 | 3.37 µs | 1.33 µs | 2.5× |

That is 1.3 ns per element to decode and 3.3 ns to encode. A short column costs
more per element — 16 elements decode in 47 ns, 3.0 ns each — because the header,
the base and the block setup are fixed and there is nothing to amortise them
over.

### What it costs

One byte per block, so **+0.1% on incompressible data**. That is what used to be
bounded by a `trFixed` fallback; the widest block width bounds it now, and the
fallback is gone. On a column of one to three elements the framing is a larger
share and the encoder can land a byte or two above the old codec.

## Element types

`int8`, `int16`, `int32`, `int64`, and defined types over them. The width comes
from `unsafe.Sizeof` on the type parameter, so encoder and decoder derive the
same width from the same type and it never reaches the wire — which is also why
`int` is excluded: its width is platform-dependent, so an `[]int` written on a
64-bit host would decode silently wrong on a 32-bit one.

Unsigned slices convert through the same-width signed type, which preserves the
bit pattern. See `codec/integer.go`.

## Tests

- **`TestPackRunRoundtrip`** — every width 0..64 at every partial-block length,
  read back both with slack after the run and with the buffer ending exactly at
  it, so the one-load path and the gather path must agree.
- **`TestAFullBlockIsAWholeNumberOfBytes`** — the arithmetic the layout rests on.
- **`TestArrayTransformChoiceIsOptimal`** — no other transform may produce a
  shorter encoding than the one chosen. This is what `TestArraySearchIsOptimal`
  checked for the old parameter search.
- **`TestArrayNeverExceedsTheElementWidth`** — the +0.1% bound, asserted.
- **`TestArrayElementTypes`** — each width end to end, at its extremes.
- **`TestArrayDecodeGarbage`**, **`TestArrayTruncated`**, **`FuzzArrayDecode`** —
  arbitrary and truncated input must never panic.

## Status

The Rust port has **not** been brought over and still implements the `(k, M)`
codec. The two ports disagree; see `wire/README.md` for the same note and what
re-enabling the cross-language check needs.
