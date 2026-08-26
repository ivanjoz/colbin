//go:build goexperiment.simd && amd64

package codec

import (
	"simd"
	"simd/archsimd"
)

const (
	floatZeroSIMDMin    = 1024
	floatZeroScalarHead = 32
)

// allZeroFloat64s preserves the scalar early exit for normal non-zero columns.
// SIMD is only used after a scalar prefix is zero and the remaining scan is
// long enough to amortize mask conversion.
func allZeroFloat64s(vals []float64) bool {
	if len(vals) < floatZeroSIMDMin || simd.Emulated() {
		return allZeroFloat64sScalar(vals)
	}
	head := min(len(vals), floatZeroScalarHead)
	if !allZeroFloat64sScalar(vals[:head]) {
		return false
	}

	vals = vals[head:]
	lanes := simd.VectorBitSize() / 64
	full := uint8((uint16(1) << uint(lanes)) - 1)
	zero := simd.BroadcastFloat64s(0)
	i := 0
	for ; i+lanes <= len(vals); i += lanes {
		if mask64Bits(simd.LoadFloat64s(vals[i:]).Equal(zero)) != full {
			return false
		}
	}
	return allZeroFloat64sScalar(vals[i:])
}

func mask64Bits(mask simd.Mask64s) uint8 {
	switch x := mask.ToArch().(type) {
	case archsimd.Mask64x2:
		return x.ToBits()
	case archsimd.Mask64x4:
		return x.ToBits()
	case archsimd.Mask64x8:
		return x.ToBits()
	default:
		panic("codec: unexpected amd64 SIMD mask width")
	}
}
