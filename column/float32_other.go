//go:build !amd64 || purego

package column

import (
	"math"
	"unsafe"
)

// The float32 kernels' scalar twins, for the builds without float32_amd64.s:
// decimalOf32 and the decoder's narrowing, four values to a mask byte.

func decimalEncodeVec32(vals *float32, n int, power float64, ints *int64, misses *byte) {
	values, out := unsafe.Slice(vals, n), unsafe.Slice(ints, n)
	bitmap := unsafe.Slice(misses, n/4)
	for group := range n / 4 {
		var mask byte
		for lane := range 4 {
			index := group*4 + lane
			i, ok := decimalOfPower32(values[index], power)
			out[index] = i
			if !ok {
				mask |= 1 << lane
			}
		}
		bitmap[group] = mask
	}
}

func decimalNarrowVec(src *int64, n int, dst *float32) uint64 {
	values, out := unsafe.Slice(src, n), unsafe.Slice(dst, n)
	var special uint64
	for index, bits := range values {
		out[index] = float32(math.Float64frombits(uint64(bits)))
		if math.Float32bits(out[index])&0x7F800000 == 0x7F800000 {
			special = 1
		}
	}
	return special
}
