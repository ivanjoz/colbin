# varint

`github.com/ivanjoz/colbin/varint`

Variable-length integer codec for `[]int8`, `[]int16`, `[]int32` and `[]int64`.
A standard varint spends one continuation flag per byte, yielding 7 payload bits
out of 8. This package reclaims some of those flags using two array-wide
parameters stored in a one-byte header, then picks between four transforms to
shrink the values before encoding them.

```go
vals := []int32{100, 240, 250, 380}

buf := varint.AppendArray(nil, vals)             // no widening, no conversion
out := make([]int32, len(vals))
n, err := varint.DecodeArray(buf, len(vals), out)
```

The element count is not stored; it comes from the record count, matching the
convention of colbin's other column codecs.

## Element types

```go
type Signed interface {
	~int8 | ~int16 | ~int32 | ~int64
}

func AppendArray[T Signed](out []byte, vals []T) []byte
func DecodeArray[T Signed](buf []byte, n int, out []T) (int, error)
```

Every width is handled natively: no widening at the call site and no scratch
`[]int64`. The element width is read from the type via `unsafe.Sizeof`, which
resolves per instantiation, so encoder and decoder derive the same width from
the same type and it never goes on the wire. Defined types work too — a
`type recordID int32` reports width 4, since nothing depends on a type switch.

`int` is deliberately **excluded**. Its width is platform-dependent, so an
`[]int` encoded on a 64-bit host would not decode on a 32-bit one, and because
the width is derived rather than stored, that mismatch would be silent. Convert
to a fixed-width type at the call site.

Unsigned types are not supported yet. `uint8`/`uint16`/`uint32` would widen
losslessly, but `uint64` above 2^63 does not, so they need their own path rather
than being folded into `Signed`.

`DecodeArray` must be instantiated with the same type used to encode. Corrupt
input cannot panic or read out of bounds, but it can decode to a value outside
the range of a narrower `T`, which is then truncated silently.

## Wire format

One header byte, then the payload:

| bits | field | meaning |
|---|---|---|
| 0-1 | transform | `00` raw · `01` delta-of-previous · `10` FOR · `11` fixed native width |
| 2 | zigzag | residuals are zigzag-encoded |
| 3-4 | `k-1` | `k = 1..4`; leading flag-free bytes = `k-1` |
| 5-7 | code | `d = dCodes[code]`, `M = k + d` |

`k` is the declared **minimum** encoded length. Every value occupies at least
`k` bytes, so bytes `1..k-1` are guaranteed to be followed by another byte:
their continuation flag is always `1` and carries no information, so it becomes
an 8th payload bit.

`M` is the declared **maximum**. No value exceeds `M` bytes, so byte `M` is
guaranteed to be last: its flag is always `0` and is reclaimed the same way.

Bytes `k..M-1` are the only ones that keep a flag, since they may or may not be
final. Capacity for a value occupying exactly `l` bytes:

```
cap(l) = 8(k-1) + 7(l-k+1)   for k <= l < M
cap(M) = 7M + k
```

Example with `k=3, M=4`:

```
byte1:   PPPPPPPP   8 payload, no flag: a byte 2 is guaranteed
byte2:   PPPPPPPP   8 payload, no flag: a byte 3 is guaranteed
byte3: C PPPPPPP    flag needed: may or may not be the last
byte4:   PPPPPPPP   8 payload, no flag: M=4 means it must be the last
```

`cap(3) = 23`, `cap(4) = 31`.

## Both parameters must be declared, not derived

This is the part that is easy to get wrong, and getting it wrong silently
produces a codec that never beats plain varint.

**Setting `M` to the max value's ordinary varint length gains exactly zero.**
If `k = ceil(b/7) = L₁(b)`, then `L_k(b) = k = L₁(b)` for all `b >= 7` — the
declared length collapses back to the plain-varint length. The same argument
applies to `k`. The useful choice is the smallest `M` satisfying
`cap(M) >= b_max`:

```
M = max(k, ceil((b_max - k) / 7))
```

With `k=1` this is one byte shorter than plain varint whenever `b_max ≡ 1 (mod 7)`
— bit-lengths 8, 15, 22, 29, and so on. Because `cap(M) = 7M + k`, raising `k`
widens that window: at `k=4` it covers `b_max mod 7 ∈ {1,2,3,4}`, four
residue classes instead of one.

Worked example (`[100, 240, 250, 380]`, deltas `[100, 140, 10, 130]`, `b_max = 8`):

| params | per-value bytes | total |
|---|---|---|
| `k=1, M=5` (plain varint) | 1, 2, 1, 2 | 6B |
| `k=1, M=2` (**derived** from max) | 1, 2, 1, 2 | 6B |
| `k=1, M=1` (**declared**) | 1, 1, 1, 1 | **4B** |

`140` needs 8 bits. With `M=1` there is never a continuation, so the single byte
carries all 8. Pinned by `TestArrayDeltaExampleIsOptimal`.

## Why `dCodes[7] = 8`

Since `M >= k` always holds, the header stores `d = M - k`, spending no space on
impossible states. A linear `d = 0..7` reaches the optimal `M` at every
bit-length 1..64 — but only by raising `k` once `b_max >= 58`, and `k >= 2` puts
a 2-byte floor under every small value. On skewed data (mostly small, occasional
huge — the normal shape for delta encoding) that costs 42%.

```
dCodes = [0, 1, 2, 3, 4, 5, 6, 8]
```

Remapping code 7 to `d=8` restores `k=1` across the full 64-bit range. The only
loss is that `M = k+7` becomes unreachable, which is strictly worse solely when
`b_max == 57` exactly.

## Transforms

The int64 input is mapped to unsigned residuals, then encoded with the `(k, M)`
varint. All four are scored and the smallest wins.

| transform | residuals |
|---|---|
| `trRaw` | `enc(vals[i])` |
| `trDelta` | `enc(vals[0])`, then `enc(vals[i] - vals[i-1])` |
| `trFOR` | `zigzag(min)`, then `uint64(vals[i] - min)` |
| `trFixed` | uncompressed native words, no varint |

`enc` applies zigzag when the header bit is set. `trFOR` deltas are non-negative
by construction and are never zigzagged; its base at index 0 always is, since it
alone may be negative.

`trFixed` always fits, since every value fits its own type by construction. That
bounds the output at `1 + width*n` — an invariant checked by
`TestArrayNeverExceedsFixed` and by `fuzzRoundtrip`.

It only earns its header code at width 8, though. For narrower types the varint
path reaches the same size on its own: `k = M = width` makes every byte
flag-free, giving exactly `8*width` bits in `width` bytes. Random `int16` data
lands on `k=M=2` and encodes to `1 + 2n`, matching fixed. At width 8 that is out
of reach, since `k` is capped at 4 — the best varint pair for 64-bit residuals
is `k=4, M=9`, costing 9 bytes per value against fixed's 8.

Two overflow cases are handled explicitly: `trDelta` is rejected when a
consecutive difference overflows int64 (needs a span above 2^63), and `trFOR`
relies on wrapping unsigned arithmetic, which stays correct across a full
`MinInt64..MaxInt64` span.

## Measured results

256-element arrays, from `TestArraySizeReport`:

| shape | raw | encoded | transform | params |
|---|---|---|---|---|
| tiny values 0..99 | 2048B | 257B (12.5%) | raw | k=1 M=1 |
| monotonic ids | 2048B | 262B (12.8%) | delta | k=1 M=6 |
| timestamps (sec) | 2048B | 261B (12.7%) | delta | k=1 M=5 |
| clustered ±500 | 2048B | 484B (23.6%) | FOR | k=1 M=5 |
| deltas b_max=8 | 2048B | 257B (12.5%) | delta | k=1 M=1 |
| random int64 | 2048B | 2049B (100.0%) | fixed | k=1 M=1 |
| all zeros | 2048B | 257B (12.5%) | raw | k=1 M=1 |
| negatives | 2048B | 257B (12.5%) | delta | k=1 M=1 |

### Element width

The same logical values encoded as different types, n=256:

| values | as `[]int16` | as `[]int32` | as `[]int64` |
|---|---|---|---|
| random int16 (incompressible) | 513B | 513B | 513B |
| narrow band (1000..1049) | 258B | 258B | 258B |

Output size is width-independent for values that fit the narrow type, because
`k` normalises it: random `int16` picks `k=M=2` at every declared width, and the
narrow band picks FOR with one byte per value. So native typing buys API
ergonomics and source-side memory, not smaller output —
`TestArrayNarrowerTypeNeverLarger` pins the ordering rather than a gap.

### Throughput

1024-element arrays, ns/op is per array:

| type | encode | decode |
|---|---|---|
| `int16` | 17.2 µs | 3.3 µs |
| `int32` | 20.1 µs | 3.2 µs |
| `int64` | 17.5 µs | 3.5 µs |

Decode of `int64` runs at 2.4 GB/s of source data; the per-element cost is flat
across widths, as the varint path is driven by residual magnitude rather than
element width.

### What `k` actually earns

Measured by re-running the search with `k` capped at 1 and comparing totals over
2000 random arrays per shape:

| shape | `k<=4` | `k=1` only | k saves |
|---|---|---|---|
| small 0..127 | 202024B | 202024B | 0.00% |
| monotonic ×1000 | 401902B | 401902B | 0.00% |
| 2^40 + small | 313402B | 313402B | 0.00% |
| timestamps | 209647B | 209656B | 0.00% |
| uniform < 2^57 | 1597162B | 1709057B | **6.55%** |
| high min, wide spread | 1393150B | 1451222B | **4.00%** |

`k` contributes nothing on small, monotonic, or clustered data, and 4–6.5% on
high-magnitude wide-spread data — exactly the regime where subtracting a base
cannot help, since the span stays wide. It earns its two header bits, but only
on that shape.

Selection frequency over 27000 random arrays across nine distributions. The
search tries `k=1` first and requires a strict improvement to move off it, so
`k>1` being chosen means `k>1` is genuinely smaller:

```
k=1  69.12%     raw    37.64%
k=2  10.06%     delta  31.00%
k=3   1.37%     FOR     9.08%
k=4  19.45%     fixed  22.27%
```

## Tests

24 tests, 2 fuzz targets, 6 benchmarks. 99.0% statement coverage; the only
uncovered lines are unreachable guards (`evaluate` returning no viable params,
`search` finding no fitting pair).

Beyond roundtrips, the non-obvious ones:

- **`TestCapBitsFormula`** — capacity matches the derivation for every
  `(k, M, l)` and never claims more than `8l` physical bits.
- **`TestEncLenIsTight`** — for every bit-length, the chosen length is the
  *smallest* that fits, and 0 is returned only when the value truly exceeds
  `cap(M)`.
- **`TestKMRoundtripBoundaries`** — every `cap(l)` edge (`full`, `full-1`,
  `full+1`) and every `2^b` neighbour, across all 32 parameter pairs.
- **`TestArraySearchIsOptimal`** — re-encodes with all 32 `(k, code)` pairs and
  asserts none beats the chosen one. This checks the histogram cost model
  against real encoding, which is where a silent size regression would hide.
- **`TestKMStreamFraming`** — 200 concatenated values decode individually and
  land exactly on the buffer end.
- **`TestKMTruncated` / `TestArrayTruncated`** — every prefix cut must error,
  never panic.
- **`TestArrayAllTransformsSelected`** — each of the four header codes is
  reachable, so none is dead weight.
- **`TestArrayElementTypes`** — a full sweep per element type: extremes, values
  pinned to each boundary, random arrays over the whole type range, and narrow
  bands where delta and FOR are the interesting transforms.
- **`TestArrayDefinedType`** — `type recordID int32` behaves identically, since
  the width comes from `unsafe.Sizeof` and not a type switch.
- **`TestArrayNarrowerTypeNeverLarger`** — the same values as `int16`, `int32`
  and `int64` produce non-decreasing encoded sizes.
- **`FuzzArrayRoundtrip`** — reinterprets the fuzz bytes as one of the four
  element types per input, and also asserts the `1 + width*n` bound.
- **`FuzzArrayDecode`** — arbitrary bytes must not panic the decoder, for every
  element type.

## Changes from the original sketch

The first design used eight flag bits differently. Three of them turned out to
be dead or worthless:

- *"bit 1 = any number < 0"* conflated signedness with varint payload widths.
  Deltas from a minimum are non-negative by construction, so the flag was dead
  in the mode most likely to be selected. It is now an explicit zigzag bit that
  the transform decides.
- *"bits 2-3 = minimum value in bytes"* derived `k` from the data, which is the
  one choice that provably gains nothing. `k` is now searched.
- *"bits 4-6 = maximum value in bytes"* had the same defect for `M`, and its
  3-bit range could not hold `M` for int64. Storing `d = M-k` fixes both.
- *"bit 7 = deltas of min vs deltas of first"* is subsumed by the 2-bit
  transform field, which also adds delta-of-previous — the strongest transform
  for monotonic data, and the one the original sketch lacked.

## Status

Not yet wired into colbin. `appendIntColumn` / `decodeIntColumn` still use the
bit-packer in `column_int.go`; switching over means a `ftInt` flag change and a
format-version bump.
