//go:build amd64 && !purego

package column

import (
	"math"
	"math/rand/v2"
	"testing"
)

// unpackSIMD must read back what packRun wrote at every width, for every
// run length around a group and a block, whether the buffer ends at the run or
// carries more after it — which decides how far the kernel may go before
// leaving the rest to the scalar read.
func TestSIMDUnpackReadsWhatPackWrote(t *testing.T) {
	if !useSIMD {
		t.Skip("this CPU has no AVX2")
	}
	random := rand.New(rand.NewPCG(5, 6))
	for w := 1; w <= 64; w++ {
		mask := uint64(1)<<(w-1)<<1 - 1
		for _, n := range []int{1, 7, 8, 9, 15, 16, 17, 31, 64, 100, 127, 128} {
			want := make([]uint64, n)
			for index := range want {
				want[index] = random.Uint64() & mask
			}
			packed := packRun(nil, want, w)
			for _, slack := range []int{0, 3, 8, 40} {
				buf := append(append([]byte(nil), packed...), make([]byte, slack)...)
				got := make([]uint64, n)
				unpackRun(buf, got, w)
				for index := range want {
					if got[index] != want[index] {
						t.Fatalf("w=%d n=%d slack=%d: value %d is %x, want %x",
							w, n, slack, index, got[index], want[index])
					}
				}
			}
		}
		// And the kernel is what read them, rather than the scalar read
		// after it: all of a full block with room after it.
		if w <= wideWidth && unpackVectorised {
			block := make([]uint64, blockSize)
			buf := append(packRun(nil, block, w), make([]byte, 16)...)
			if done := unpackSIMD(buf, block, w); done != blockSize {
				t.Fatalf("w=%d: the kernel read %d of a block's %d values", w, done, blockSize)
			}
		}
	}
}

// residualsSIMD must write each transform's residuals and their OR exactly
// as residualAt makes them, at every block length around a vector.
func TestSIMDResidualsAgreeWithScalar(t *testing.T) {
	if !useSIMD {
		t.Skip("this CPU has no AVX2")
	}
	random := rand.New(rand.NewPCG(9, 10))
	for trial := range 6 {
		for n := 1; n <= blockSize+3; n++ {
			// One value more than the block, for delta's last step.
			vals := make([]int64, n+1)
			for index := range vals {
				vals[index] = int64(random.Uint64()) >> (trial * 10)
				if trial == 5 {
					vals[index] = []int64{math.MinInt64, math.MaxInt64, -1, 0}[random.IntN(4)]
				}
			}
			base := vals[random.IntN(len(vals))]
			for _, transform := range []uint8{trRaw, trFOR, trDelta} {
				for _, zz := range []bool{false, true} {
					got := make([]uint64, n)
					set := residualsSIMD(vals, transform, base, zz, got)
					var want uint64
					for index := range got {
						residual := residualAt(vals, index, transform, base, zz)
						want |= residual
						if got[index] != residual {
							t.Fatalf("trial %d n=%d transform %d zz=%v: residual %d is %x, want %x",
								trial, n, transform, zz, index, got[index], residual)
						}
					}
					if set != want {
						t.Fatalf("trial %d n=%d transform %d zz=%v: or %x, want %x", trial, n, transform, zz, set, want)
					}
				}
			}
		}
	}
}

// The kernels must answer exactly what the scalar loops do, or a SIMD build
// would choose a different transform or width and write different bytes. Every
// length around a vector and a block, over values that make each transform
// win, and the edges a lane trick could get wrong: the extremes, a negative
// zigzag, a delta that overflows.
func TestSIMDKernelsAgreeWithScalar(t *testing.T) {
	if !useSIMD {
		t.Skip("this CPU has no AVX2")
	}
	random := rand.New(rand.NewPCG(3, 4))
	shapes := map[string]func(index int) int64{
		"random":   func(int) int64 { return int64(random.Uint64()) },
		"small":    func(int) int64 { return random.Int64N(100) - 50 },
		"positive": func(int) int64 { return random.Int64N(1 << 20) },
		"run":      func(index int) int64 { return 1000 + int64(index)*7 },
		"constant": func(int) int64 { return -42 },
		"extremes": func(index int) int64 {
			return []int64{math.MinInt64, math.MaxInt64, 0, -1, 1}[random.IntN(5)]
		},
	}
	for name, shape := range shapes {
		for n := 1; n <= 300; n++ {
			vals := make([]int64, n)
			for index := range vals {
				vals[index] = shape(index)
			}
			minimum, constant, overflows := stats(vals)
			gotMinimum, gotConstant, gotOverflows := statsSIMD(vals)
			if gotMinimum != minimum || gotConstant != constant || gotOverflows != overflows {
				t.Fatalf("%s n=%d: stats (%d %v %v), want (%d %v %v)", name, n,
					gotMinimum, gotConstant, gotOverflows, minimum, constant, overflows)
			}
			for _, zz := range []bool{false, true} {
				delta := n > 1 && !overflows
				raw, frame, steps := blockedCosts(vals, minimum, zz, delta)
				gotRaw, gotFrame, gotSteps := blockedCostsSIMD(vals, minimum, zz, delta)
				if gotRaw != raw || gotFrame != frame || gotSteps != steps {
					t.Fatalf("%s n=%d zz=%v: costs (%d %d %d), want (%d %d %d)", name, n, zz,
						gotRaw, gotFrame, gotSteps, raw, frame, steps)
				}
			}
			residuals := make([]uint64, n)
			var want uint64
			for index, value := range vals {
				residuals[index] = uint64(value)
				want |= uint64(value)
			}
			if got := orSIMD(residuals); got != want {
				t.Fatalf("%s n=%d: or %x, want %x", name, n, got, want)
			}
		}
	}
}
