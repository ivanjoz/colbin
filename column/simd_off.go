//go:build !(amd64 || arm64) || purego

package column

// useSIMD: see vec.go. Off amd64 and arm64, or under the
// purego tag, it is a constant false, so every branch on it is dead code and
// the scalar loops are all that is compiled in.
const useSIMD = false

// packVectorWidth is the widest w packSIMD writes; see vec.go.
const packVectorWidth = 0

func packSIMD(out []byte, block []uint64, w int) []byte { return out }

// The kernels' scalar twins, never called: they keep the dispatch compiling.

func statsSIMD(vals []int64) (int64, bool, bool) { return stats(vals) }

func blockedCostsSIMD(vals []int64, base int64, zz, delta bool) (int, int, int) {
	return blockedCosts(vals, base, zz, delta)
}

func residualsSIMD(vals []int64, transform uint8, base int64, zz bool, block []uint64) uint64 {
	var set uint64
	for index := range block {
		block[index] = residualAt(vals, index, transform, base, zz)
		set |= block[index]
	}
	return set
}

func orSIMD(block []uint64) uint64 {
	var set uint64
	for _, residual := range block {
		set |= residual
	}
	return set
}
