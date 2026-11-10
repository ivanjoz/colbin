package column

// Float32 columns: AppendFloat64's decimal idea for a column of float32.
//
// A float32 that came out of a form or a database is nearly always a short
// decimal, 12.5 or 0.1 or 3.99, and at the exponent its column shares it is an
// integer. The test is the float32's own: v is reached at exponent e when
//
//	float32(float64(i) / 10^e) == v    (bit for bit)
//
// for the integer i the product rounds to. 0.1f is not a decimal in float64
// (it is 0.100000001490116...), so AppendFloat64 over the widened values
// reaches almost none of them, where this reaches them all.
//
// What is written is the decimal column AppendFloat64 writes (see float.go), so
// the decode is the same one: its integers are divided in float64, and the
// result narrowed to float32, which is the very expression the encoder checked.
// A value no exponent reaches is an exception, kept as the bits of its float64
// widening. A column no smaller than its bits is written as them instead, the
// int32 bit patterns in an ordinary column, and DecodeFloat32 reads either by
// the first byte, as DecodeArray does.
//
// The widening of an exception is done on the bits where it is not a number, so
// a NaN keeps its payload and a signalling NaN its signal: the hardware
// conversion quiets it.

import (
	"encoding/binary"
	"math"
)

// maxFloat32Exponent is the largest exponent AppendFloat32 tries. A float32
// carries under eight significant digits, so an integer past 10^12 already
// needs more than the float's 24 bits and nothing is gained by looking further.
const maxFloat32Exponent = 12

// widenBits is the bit pattern of v as a float64, exact for every float32:
// a NaN keeps its payload, shifted into the float64's mantissa.
func widenBits(v uint32) uint64 {
	if v>>23&0xFF == 0xFF {
		return uint64(v>>31)<<63 | 0x7FF<<52 | uint64(v&0x7FFFFF)<<29
	}
	return math.Float64bits(float64(math.Float32frombits(v)))
}

// narrowBits is widenBits' inverse over what a decimal column decodes to, and
// whether bits was one it could be: a finite float64 is rounded to the float32
// the encoder checked it against, one that overflows float32 is refused, and a
// NaN or infinity must be a widened float32's.
func narrowBits(bits uint64) (uint32, bool) {
	if bits>>52&0x7FF == 0x7FF {
		mantissa := bits & (1<<52 - 1)
		if mantissa&(1<<29-1) != 0 {
			return 0, false
		}
		return uint32(bits>>63)<<31 | 0xFF<<23 | uint32(mantissa>>29), true
	}
	narrow := float32(math.Float64frombits(bits))
	if math.IsInf(float64(narrow), 0) {
		return 0, false
	}
	return math.Float32bits(narrow), true
}

// decimalOf32 is the integer of v at exponent e, and whether it is one: whether
// float32(float64(i) / 10^e) gives back v bit for bit, which rules out -0, NaN
// and the infinities as well as the decimals e does not reach. The arithmetic
// is decimalOf's, the explicit conversion included.
func decimalOf32(v float32, e int) (int64, bool) { return decimalOfPower32(v, powersOf10[e]) }

// decimalOfPower32 is decimalOf32 at the divisor power: the definition the
// kernels (float32_amd64.s) are held to.
func decimalOfPower32(v float32, power float64) (int64, bool) {
	sum := float64(float64(v)*power) + decimalMagic
	i := int64(math.Float64bits(sum)) - int64(math.Float64bits(decimalMagic))
	inSpan := uint64(i+decimalSpan) < 2*decimalSpan
	back := float32((sum - decimalMagic) / power)
	return i, inSpan && math.Float32bits(back) == math.Float32bits(v)
}

// decimalInts32 is decimalInts for float32s: vals converted at exponent e into
// ints, the integer before it standing in for each value e does not reach, and
// those values' positions and float64-widened bit patterns appended.
func decimalInts32(vals []float32, e int, ints []int64, misses []byte, positions, bits []int64) ([]int64, []int64) {
	full := 0
	if useSIMD {
		full = len(vals) &^ 3
	}
	if full > 0 {
		decimalEncodeVec32(&vals[0], full, powersOf10[e], &ints[0], &misses[0])
	}
	previous := int64(0)
	miss := func(index int) {
		ints[index] = previous
		positions = append(positions, int64(index))
		bits = append(bits, int64(widenBits(math.Float32bits(vals[index]))))
	}
	// As decimalInts: eight groups at a time are skipped when none missed.
	for group := 0; group < full/4; {
		if group+8 <= full/4 && binary.LittleEndian.Uint64(misses[group:]) == 0 {
			previous = ints[(group+8)*4-1]
			group += 8
			continue
		}
		mask := misses[group]
		for lane := range 4 {
			index := group*4 + lane
			if mask&(1<<lane) != 0 {
				miss(index)
			} else {
				previous = ints[index]
			}
		}
		group++
	}
	for index := full; index < len(vals); index++ {
		if i, ok := decimalOf32(vals[index], e); ok {
			ints[index], previous = i, i
			continue
		}
		miss(index)
	}
	return positions, bits
}

// chooseExponent32 is chooseExponent for float32s.
func chooseExponent32(vals []float32) (exponent, reached, sampled int) {
	step := max(1, len(vals)/decimalSample)
	for e := 0; e <= maxFloat32Exponent; e++ {
		count, seen := 0, 0
		for index := 0; index < len(vals) && seen < decimalSample; index += step {
			if _, ok := decimalOf32(vals[index], e); ok {
				count++
			}
			seen++
		}
		sampled = seen
		if count > reached {
			exponent, reached = e, count
		}
		if reached == seen {
			break
		}
	}
	return exponent, reached, sampled
}

// AppendFloat32 encodes a column of float32s onto out and returns out: a
// decimal column when that is smaller than the values' bits, and their bit
// patterns as an ordinary column otherwise, so a float32 column never costs
// more than its bits. DecodeFloat32 reads either.
func AppendFloat32(out []byte, vals []float32) []byte {
	scratch := getDecimalScratch()
	defer decimalPool.Put(scratch)
	if cap(scratch.raw) < len(vals) {
		scratch.raw = make([]int64, len(vals))
	}
	raw := scratch.raw[:len(vals)]
	for index, v := range vals {
		raw[index] = int64(int32(math.Float32bits(v)))
	}
	// The bytes depend only on the values, so the int64 column is the int32 one.
	asBits := func() []byte { return AppendArray(out, raw) }
	if len(vals) == 0 || forcedTransform >= 0 {
		return asBits()
	}
	exponent, reached, sampled := chooseExponent32(vals)
	if reached*2 < sampled {
		return asBits()
	}

	if cap(scratch.ints) < len(vals) {
		// AppendFloat64 sizes misses with ints and relies on it.
		scratch.ints = make([]int64, len(vals))
		scratch.misses = make([]byte, len(vals)/4+1)
	}
	ints := scratch.ints[:len(vals)]
	// An exception's own integer is the one before it, which costs the
	// integers nothing.
	positions, bits := decimalInts32(vals, exponent, ints, scratch.misses, scratch.positions[:0], scratch.bits[:0])
	scratch.positions, scratch.bits = positions[:0], bits[:0]

	decimal := 6 + arrayCost(ints)
	if len(positions) > 0 {
		decimal += arrayCost(positions) + arrayCost(bits)
	}
	if decimal >= arrayCost(raw) {
		return asBits()
	}
	out = append(out, headerDecimal, byte(exponent))
	out = append(out, byte(len(positions)), byte(len(positions)>>8), byte(len(positions)>>16), byte(len(positions)>>24))
	out = AppendArray(out, ints)
	if len(positions) > 0 {
		out = AppendArray(out, positions)
		out = AppendArray(out, bits)
	}
	return out
}

// DecodeFloat32 reads n float32s written by AppendFloat32 into out and returns
// the number of bytes consumed from buf. A column the encoder could not have
// written, or whose decimal values leave float32's range, is an error.
func DecodeFloat32(buf []byte, n int, out []float32) (int, error) {
	if n < 0 {
		return 0, errNegativeCount
	}
	if len(out) < n {
		return 0, errShortBuffer
	}
	if len(buf) < 1 {
		return 0, errTruncated
	}
	scratch := getDecimalScratch()
	defer decimalPool.Put(scratch)
	if cap(scratch.raw) < n {
		scratch.raw = make([]int64, n)
	}
	wide := scratch.raw[:n]
	used, err := DecodeArray(buf, n, wide)
	if err != nil {
		return 0, err
	}
	if n > 0 && buf[0] == headerDecimal {
		// The kernel narrows what is a plain conversion and says when a value
		// is a NaN or an infinity, which narrowBits then reads on its own.
		full := 0
		if useSIMD {
			full = n &^ 3
		}
		if full > 0 && decimalNarrowVec(&wide[0], full, &out[0]) == 0 {
			wide, out = wide[full:], out[full:]
		}
		for index, bits := range wide {
			v, ok := narrowBits(uint64(bits))
			if !ok {
				return 0, errOutOfRange
			}
			out[index] = math.Float32frombits(v)
		}
		return used, nil
	}
	// The bits as an int32 column: a value outside it is not a float32's.
	for index, bits := range wide {
		if int64(int32(bits)) != bits {
			return 0, errOutOfRange
		}
		out[index] = math.Float32frombits(uint32(bits))
	}
	return used, nil
}
