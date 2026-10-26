package column

// Decimal columns: a float64 column whose values are decimals, as nearly every
// double that came out of a CSV, a form or a database is.
//
// A float's bit pattern is noise to the integer transforms: 12.5 and 12.75
// differ in their low mantissa bits and their exponent alike, and a column of
// them costs its eight bytes a value whatever the transform. But 12.5 is
// 125 / 10, and 12.75 is 1275 / 100: at the exponent the column's values
// share, each is an integer, and a column of integers is what the transforms
// are good at. This is ALP's idea (Afroozeh, Kuffó and Boncz, SIGMOD 2024),
// cut down to the part that pays on the Public BI tables: one exponent per
// column, and the values it does not reach kept as they are.
//
//	[0x08] [e] [exceptions:u32] [integers] ( [positions] [bits] )
//
// `integers` is an ordinary column of n values, each in [−2^51, 2^51), with
// v = float64(i) / 10^e exactly; `positions` and `bits`, present when there
// are exceptions, are ordinary columns of the rows that are not, and their bit
// patterns. An exception's own integer is the one before it, so it costs the
// integers nothing.
//
// The decode is a conversion and one division, which IEEE 754 rounds the same
// way on every machine and no compiler fuses with anything; WebAssembly has it
// exactly, where it has no fused multiply-add. ALP decodes by multiplying by
// 10^−e instead, which is faster but inexact: on the Public BI tables it gets
// back 84% of the values where the division gets back 93%, and every value it
// misses is an exception of ten bytes. The kernels (float_amd64.s) divide four
// at a time.
//
// It is the encoder's choice, made per column and only for float64 columns
// (AppendFloat64), and only where it is smaller than the column's bits;
// DecodeArray reads it into an int64 column as the values' bit patterns, so a
// reader that reads a float column today reads this one too.

import (
	"encoding/binary"
	"math"
	"math/bits"
	"sync"
)

// headerDecimal is the header byte of a decimal column.
const headerDecimal uint8 = 0x08

// maxDecimalExponent is the largest e a decimal column takes: 10^18 is the
// largest power of ten an int64 holds, and every power of ten up to 10^22 is
// exact in a float64.
const maxDecimalExponent = 18

// powersOf10 are the divisors, as constants: every one is exact in a float64,
// and a port copies the table rather than computing it.
var powersOf10 = [maxDecimalExponent + 1]float64{
	1e0, 1e1, 1e2, 1e3, 1e4, 1e5, 1e6, 1e7, 1e8, 1e9,
	1e10, 1e11, 1e12, 1e13, 1e14, 1e15, 1e16, 1e17, 1e18,
}

// decimalMagic is 2^52 + 2^51. Added to a float64 s with |s| < 2^51 it lands
// in [2^52, 2^53), where a float64's step is exactly one, so the sum is s
// rounded to the nearest integer (ties to even) and its bit pattern is
// bits(decimalMagic) plus that integer. The encoder rounds this way because it
// is two vector instructions where AVX2 has no conversion to int64 at all; the
// decoder converts the same way back.
const decimalMagic = 1<<52 + 1<<51

// decimalSpan bounds a decimal column's integers: each lies in
// [−decimalSpan, decimalSpan), the range decimalMagic converts exactly.
const decimalSpan = 1 << 51

// decimalOf is the integer of v at exponent e, and whether it is one: whether
// float64(i) / 10^e gives back v bit for bit, which rules out -0, NaN and the
// infinities as well as the decimals e does not reach.
//
// This is the definition the kernels are held to. The product is converted
// explicitly because Go may otherwise fuse it with the addition into an FMA on
// arm64, and an encoder that rounds the product differently on one machine
// writes different bytes there.
func decimalOf(bits int64, e int) (int64, bool) {
	sum := float64(math.Float64frombits(uint64(bits))*powersOf10[e]) + decimalMagic
	i := int64(math.Float64bits(sum)) - int64(math.Float64bits(decimalMagic))
	inSpan := uint64(i+decimalSpan) < 2*decimalSpan
	return i, inSpan && int64(math.Float64bits((sum-decimalMagic)/powersOf10[e])) == bits
}

// decimalSample is how many values the exponent is chosen from.
const decimalSample = 32

// chooseExponent is the smallest exponent that reaches as many of a sample of
// the values as any does, and how many of the sample that is.
func chooseExponent(vals []int64) (exponent, reached, sampled int) {
	step := max(1, len(vals)/decimalSample)
	for e := 0; e <= maxDecimalExponent; e++ {
		count, seen := 0, 0
		for index := 0; index < len(vals) && seen < decimalSample; index += step {
			if _, ok := decimalOf(vals[index], e); ok {
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

// worthConverting is whether the sample says a decimal column could be smaller
// than the bits: its integers' spread and its exceptions, a position and the
// bits each, against the bits' own spread. Where it says no, the column is not
// converted only to be written as its bits after all, which on the columns of
// doubles printed to fifteen digits was most of their encode.
func worthConverting(vals []int64, exponent, reached, sampled int) bool {
	if reached*2 < sampled {
		return false
	}
	step := max(1, len(vals)/decimalSample)
	lowInt, highInt := int64(math.MaxInt64), int64(math.MinInt64)
	lowBits, highBits := int64(math.MaxInt64), int64(math.MinInt64)
	for index, seen := 0, 0; index < len(vals) && seen < decimalSample; index, seen = index+step, seen+1 {
		lowBits, highBits = min(lowBits, vals[index]), max(highBits, vals[index])
		if i, ok := decimalOf(vals[index], exponent); ok {
			lowInt, highInt = min(lowInt, i), max(highInt, i)
		}
	}
	// In bits per value: a frame of reference over each, and an exception at
	// 64 bits and a position's dozen or so.
	intBits := bits.Len64(uint64(highInt) - uint64(lowInt))
	floatBits := bits.Len64(uint64(highBits) - uint64(lowBits))
	// A tie is a sample that cannot tell, as one all of a sparse column's
	// zeros: that column is converted, and scored on the whole of it.
	misses := sampled - reached
	return intBits*sampled+76*misses <= floatBits*sampled
}

// constantColumn is whether every value is the first.
func constantColumn(vals []int64) bool {
	var varies int64
	for _, value := range vals {
		varies |= value ^ vals[0]
	}
	return varies == 0
}

// decimalScratch is what a decimal column is converted into before it is
// written, and its exceptions read into when it is decoded.
type decimalScratch struct {
	ints, positions, bits []int64
	// misses has a bit per value the exponent does not reach, as the encode
	// kernel leaves them: four to a byte, in its low nibble.
	misses []byte
}

var decimalPool sync.Pool // *decimalScratch

func getDecimalScratch() *decimalScratch {
	scratch, _ := decimalPool.Get().(*decimalScratch)
	if scratch == nil {
		scratch = new(decimalScratch)
	}
	return scratch
}

// AppendFloat64 is AppendArray for a column of float64 bit patterns: a decimal
// column when that is smaller, and AppendArray's own bytes otherwise, so a
// float column never costs more than it did before decimal columns.
func AppendFloat64(out []byte, vals []int64) []byte {
	if len(vals) == 0 || forcedTransform >= 0 {
		return AppendArray(out, vals)
	}
	exponent, reached, sampled := chooseExponent(vals)
	if !worthConverting(vals, exponent, reached, sampled) {
		return AppendArray(out, vals)
	}
	scratch := getDecimalScratch()
	defer decimalPool.Put(scratch)
	if cap(scratch.ints) < len(vals) {
		scratch.ints = make([]int64, len(vals))
		scratch.misses = make([]byte, len(vals)/4+1)
	}
	ints := scratch.ints[:len(vals)]
	positions, bits := decimalInts(vals, exponent, ints, scratch.misses, scratch.positions[:0], scratch.bits[:0])
	scratch.positions, scratch.bits = positions[:0], bits[:0]

	// Written whichever way is smaller, scored as the encoder would write it.
	//
	// Except where that is known without scoring the bits: a long column with
	// no exceptions, whose integers no transform can make cost more than its
	// bits. Each integer is |v|·10^e < 2^51, so 10^e is below 2^52/|v|, which
	// is a float's step count per unit at |v|: two values' integers are never
	// further apart than their bit patterns, and never further from zero. The
	// header's six bytes are what it can cost over the bits, and only on a
	// column whose bits are a constant, which is checked.
	if len(positions) == 0 && len(vals) > shortColumn {
		if constantColumn(ints) {
			return AppendArray(out, vals)
		}
	} else {
		decimal := 6 + arrayCost(ints)
		if len(positions) > 0 {
			decimal += arrayCost(positions) + arrayCost(bits)
		}
		if decimal >= arrayCost(vals) {
			return AppendArray(out, vals)
		}
	}
	out = append(out, headerDecimal, byte(exponent))
	out = binary.LittleEndian.AppendUint32(out, uint32(len(positions)))
	out = AppendArray(out, ints)
	if len(positions) > 0 {
		out = AppendArray(out, positions)
		out = AppendArray(out, bits)
	}
	return out
}

// decimalInts converts vals at exponent e into ints, the integer before it
// standing in for each value e does not reach, and appends those values'
// positions and bit patterns.
func decimalInts(vals []int64, e int, ints []int64, misses []byte, positions, bits []int64) ([]int64, []int64) {
	full := 0
	if useSIMD {
		full = len(vals) &^ 3
	}
	if full > 0 {
		decimalEncodeVec(&vals[0], full, powersOf10[e], &ints[0], &misses[0])
	}
	previous := int64(0)
	// The kernel leaves a missed value's integer as whatever it computed; the
	// bitmap says which, and is all zeros on a column with no exceptions, which
	// is eight values a load to skip.
	for group := 0; group < full/4; {
		if group+8 <= full/4 {
			if binary.LittleEndian.Uint64(misses[group:]) == 0 {
				previous = ints[(group+8)*4-1]
				group += 8
				continue
			}
		}
		mask := misses[group]
		for lane := range 4 {
			index := group*4 + lane
			if mask&(1<<lane) != 0 {
				ints[index] = previous
				positions = append(positions, int64(index))
				bits = append(bits, vals[index])
			} else {
				previous = ints[index]
			}
		}
		group++
	}
	for index := full; index < len(vals); index++ {
		if i, ok := decimalOf(vals[index], e); ok {
			ints[index], previous = i, i
			continue
		}
		ints[index] = previous
		positions = append(positions, int64(index))
		bits = append(bits, vals[index])
	}
	return positions, bits
}

// decodeDecimal reads the decimal column of n values that buf opens with into
// out, as their bit patterns, and returns the bytes it used.
func decodeDecimal[T Signed](buf []byte, n int, out []T) (int, error) {
	target, ok := any(out).([]int64)
	if !ok {
		// A narrower column takes the bit patterns when they fit it, as every
		// column does: decoded as int64, then narrowed.
		wide := make([]int64, n)
		used, err := decodeDecimal(buf, n, wide)
		if err != nil {
			return 0, err
		}
		for index, value := range wide {
			if !fits[T](value) {
				return 0, errOutOfRange
			}
			out[index] = T(value)
		}
		return used, nil
	}
	if len(buf) < 6 {
		return 0, errTruncated
	}
	exponent := int(buf[1])
	exceptions := int(binary.LittleEndian.Uint32(buf[2:]))
	if exponent > maxDecimalExponent || exceptions < 0 || exceptions > n {
		return 0, errBadHeader
	}
	position := 6
	used, err := decodeInner(buf[position:], n, target)
	if err != nil {
		return 0, err
	}
	position += used
	if !decimalsToBits(target[:n], powersOf10[exponent]) {
		return 0, errOutOfRange
	}
	if exceptions == 0 {
		return position, nil
	}
	scratch := getDecimalScratch()
	defer decimalPool.Put(scratch)
	if cap(scratch.positions) < exceptions {
		scratch.positions = make([]int64, exceptions)
		scratch.bits = make([]int64, exceptions)
	}
	positions, bits := scratch.positions[:exceptions], scratch.bits[:exceptions]
	for _, dst := range [][]int64{positions, bits} {
		used, err := decodeInner(buf[position:], exceptions, dst)
		if err != nil {
			return 0, err
		}
		position += used
	}
	last := int64(-1)
	for index, at := range positions {
		// In order, each once, and inside the column.
		if at <= last || at >= int64(n) {
			return 0, errBadHeader
		}
		target[at], last = bits[index], at
	}
	return position, nil
}

// decimalsToBits turns a decimal column's integers into the bit patterns of
// float64(i) / power, in place, and reports whether every one was in the span
// a decimal column's integers are drawn from.
func decimalsToBits(values []int64, power float64) bool {
	full := 0
	if useSIMD {
		full = len(values) &^ 3
	}
	var outside uint64
	if full > 0 {
		outside = decimalDecodeVec(&values[0], full, power)
	}
	for index := full; index < len(values); index++ {
		i := values[index]
		outside |= uint64(i + decimalSpan)
		values[index] = int64(math.Float64bits(float64(i) / power))
	}
	return bits.Len64(outside) <= 52
}

// decodeInner is DecodeArray for a column inside a decimal one, which is never
// decimal itself: one could nest another as deep as the message is long.
func decodeInner(buf []byte, n int, out []int64) (int, error) {
	if len(buf) > 0 && buf[0] == headerDecimal {
		return 0, errBadHeader
	}
	return DecodeArray(buf, n, out)
}
