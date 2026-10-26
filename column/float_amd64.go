//go:build amd64 && !purego

package column

// decimalEncodeVec converts n values, n a positive multiple of four, at the
// divisor power as decimalOf does, writing each integer to ints and, for each
// four values, a byte whose low nibble has a bit set for each value the
// exponent does not reach. See float_amd64.s.
//
//go:noescape
func decimalEncodeVec(vals *int64, n int, power float64, ints *int64, misses *byte)

// decimalDecodeVec turns n integers, n a positive multiple of four, into the
// bit patterns of float64(i) / power in place, and returns the OR of i + 2^51
// over them, whose bits past the 52nd say one was outside the span.
//
//go:noescape
func decimalDecodeVec(values *int64, n int, power float64) uint64
