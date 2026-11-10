//go:build amd64 && !purego

package column

// decimalEncodeVec32 is decimalEncodeVec for float32s: n values, n a positive
// multiple of four, at the divisor power as decimalOf32 does, writing each
// integer to ints and, for each four values, a byte whose low nibble has a bit
// set for each value the exponent does not reach. See float32_amd64.s.
//
//go:noescape
func decimalEncodeVec32(vals *float32, n int, power float64, ints *int64, misses *byte)

// decimalNarrowVec turns n float64 bit patterns, n a positive multiple of
// four, into float32s as float32(f) does, and returns a nonzero value when any
// result is a NaN or an infinity: narrowBits' cases that are not a plain
// conversion, which the caller then redoes on the column. See float32_amd64.s.
//
//go:noescape
func decimalNarrowVec(src *int64, n int, dst *float32) uint64
