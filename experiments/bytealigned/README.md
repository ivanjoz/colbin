# experiments/bytealigned

Not part of the library. This is the measurement behind §1 and §3 of
[`BYTE_ALIGNED_PLAN.md`](../../BYTE_ALIGNED_PLAN.md): what it costs in bytes, and
what it buys in time, to replace `varint`'s bit-level (k,M) codec with a
byte-aligned column.

```sh
go test ./experiments/bytealigned -run 'TestSizes|TestBitSizes' -v   # bytes per element
go test ./experiments/bytealigned -run TestFloatTrim -v              # §2.3's float fix
go test ./experiments/bytealigned -bench . -benchtime=300ms
```

Four codecs over five column shapes:

- **bit-varint** — the current `varint.AppendArray` / `DecodeArray`.
- **one width** — the same transforms (raw / delta / frame of reference), every
  residual at one byte width chosen from the widest. This is the naive port, and
  it is here to show why it is not the answer: a single outlier costs +100%.
- **128-blocks** — a byte width per 128 residuals, transform chosen by scoring
  each candidate's *blocked* size.
- **bit-blocks** (`bitblocked_test.go`) — an exact *bit* width per 128
  residuals. 128 values at `w` bits is `16w` bytes, so a block is still
  byte-aligned at both ends. Smaller than the bit varint on every shape and 2–3×
  faster to read, which is what the plan ends up proposing.

Each byte-width decode appears twice. The `-hoisted` variants take the width out
of the element loop; the others read it per element. The gap between them is
larger than the gap to the bit varint, which is the point.

`floattrim_test.go` prices the plan's float rule: trimming a float's zero bytes
from the same end as an integer's saves nothing, because the two have their zeros
at opposite ends of the word.
