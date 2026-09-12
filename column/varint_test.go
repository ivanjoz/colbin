package column

import (
	"math"
	"math/rand/v2"
	"testing"
)

func TestZigzagRoundtrip(t *testing.T) {
	values := []int64{
		0, 1, -1, 2, -2, 63, -64, 64, -65,
		math.MaxInt32, math.MinInt32, math.MaxInt64, math.MinInt64,
	}
	for _, value := range values {
		if got := unzigzag(zigzag(value)); got != value {
			t.Fatalf("zigzag(%d) round-tripped as %d", value, got)
		}
	}
	// Small magnitudes must stay small, which is the whole point.
	for _, expect := range []struct {
		value int64
		coded uint64
	}{{0, 0}, {-1, 1}, {1, 2}, {-2, 3}, {2, 4}} {
		if got := zigzag(expect.value); got != expect.coded {
			t.Fatalf("zigzag(%d) = %d, want %d", expect.value, got, expect.coded)
		}
	}
}

// A block of 128 values at w bits is exactly 16w bytes, which is the arithmetic
// the whole layout rests on: every width lands on a byte boundary, so no padding
// is ever wasted and no state crosses a block.
func TestAFullBlockIsAWholeNumberOfBytes(t *testing.T) {
	for w := range 65 {
		if got := blockBytes(blockSize, w); got != 16*w {
			t.Fatalf("128 values at %d bits took %d bytes, want %d", w, got, 16*w)
		}
	}
}

// Every width, packed and unpacked, at every alignment a final partial block can
// land on — including the buffer ending exactly at the run, which is what sends
// the read down the tail path.
func TestPackRunRoundtrip(t *testing.T) {
	random := rand.New(rand.NewPCG(1, 2))
	for w := range 65 {
		for _, count := range []int{1, 2, 7, 8, 63, 64, 127, blockSize} {
			block := make([]uint64, count)
			for index := range block {
				if w == 0 {
					continue
				}
				block[index] = random.Uint64() >> (64 - w)
			}

			packed := packRun(nil, block, w)
			if got, want := len(packed), blockBytes(count, w); got != want {
				t.Fatalf("w=%d count=%d packed into %d bytes, want %d", w, count, got, want)
			}

			// Once with nothing after the run, which takes the tail path, and
			// once with slack, which takes the one-load path. Both must agree.
			for _, slack := range []int{0, 8} {
				buffer := append(append([]byte(nil), packed...), make([]byte, slack)...)
				got := make([]uint64, count)
				unpackRun(buffer, got, w)
				for index := range block {
					if got[index] != block[index] {
						t.Fatalf("w=%d count=%d slack=%d: element %d round-tripped as %#x, want %#x",
							w, count, slack, index, got[index], block[index])
					}
				}
			}
		}
	}
}

// The widest residual decides the width, and OR-ing the block together is the
// same answer as a running maximum.
func TestWidthOfIsTheWidestResidual(t *testing.T) {
	for _, expect := range []struct {
		block []uint64
		width int
	}{
		{nil, 0},
		{[]uint64{0, 0, 0}, 0},
		{[]uint64{1}, 1},
		{[]uint64{1, 2, 3}, 2},
		{[]uint64{255}, 8},
		{[]uint64{256, 1}, 9},
		{[]uint64{math.MaxUint64}, 64},
	} {
		if got := widthOf(expect.block); got != expect.width {
			t.Fatalf("widthOf(%v) = %d, want %d", expect.block, got, expect.width)
		}
	}
}
