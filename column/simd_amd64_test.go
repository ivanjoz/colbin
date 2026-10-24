//go:build goexperiment.simd && amd64 && !purego

package column

import (
	"math"
	"math/rand/v2"
	"testing"
)

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
