//go:build !(goexperiment.simd && amd64) || purego

package column

// useSIMD: see simd_amd64.go. Without the SIMD experiment, off amd64, or
// under the purego tag it is a constant false, so every branch on it is dead
// code and the scalar loops are all that is compiled in.
const useSIMD = false

// The kernels' scalar twins, never called: they keep the dispatch compiling.

func statsSIMD(vals []int64) (int64, bool, bool) { return stats(vals) }

func blockedCostsSIMD(vals []int64, base int64, zz, delta bool) (int, int, int) {
	return blockedCosts(vals, base, zz, delta)
}

func orSIMD(block []uint64) uint64 {
	var set uint64
	for _, residual := range block {
		set |= residual
	}
	return set
}
