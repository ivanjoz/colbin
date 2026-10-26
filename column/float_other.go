//go:build !amd64 || purego

package column

import (
	"math"
	"unsafe"
)

// The decimal kernels' scalar twins, for the builds without float_amd64.s:
// decimalOf and the decoder's conversion, four values to a mask byte.

func decimalEncodeVec(vals *int64, n int, power float64, ints *int64, misses *byte) {
	values, out := unsafe.Slice(vals, n), unsafe.Slice(ints, n)
	bitmap := unsafe.Slice(misses, n/4)
	for group := range n / 4 {
		var mask byte
		for lane := range 4 {
			index := group*4 + lane
			sum := float64(math.Float64frombits(uint64(values[index]))*power) + decimalMagic
			i := int64(math.Float64bits(sum)) - int64(math.Float64bits(decimalMagic))
			out[index] = i
			if uint64(i+decimalSpan) >= 2*decimalSpan ||
				int64(math.Float64bits((sum-decimalMagic)/power)) != values[index] {
				mask |= 1 << lane
			}
		}
		bitmap[group] = mask
	}
}

func decimalDecodeVec(values *int64, n int, power float64) uint64 {
	var outside uint64
	ints := unsafe.Slice(values, n)
	for index, i := range ints {
		outside |= uint64(i + decimalSpan)
		ints[index] = int64(math.Float64bits(float64(i) / power))
	}
	return outside
}
