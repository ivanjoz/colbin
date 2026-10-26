package column

import (
	"encoding/binary"
	"math/bits"
	"sync"
)

// Array codec: the integer input is mapped to a sequence of unsigned residuals
// by one of four transforms, and the residuals are then written as blocks of 128
// at a bit width chosen per block (see pack.go). The encoder scores every
// transform against what it would actually occupy in blocks and picks the
// smallest; the choice is recorded in the header byte.

// Signed is the set of element types the codec handles. Values are widened to
// int64 internally, which is free for every member, so the element type never
// reaches the wire.
//
// int is left out because its range differs between platforms: a column
// written from an []int on a 64-bit host could hold values a 32-bit host cannot.
// Convert to a fixed-width type at the call site instead.
type Signed interface {
	~int8 | ~int16 | ~int32 | ~int64
}

const (
	trRaw      uint8 = 0 // residual[i] = enc(vals[i])
	trDelta    uint8 = 1 // base = vals[0]; residual[i] = zigzag(vals[i+1]-vals[i])
	trFOR      uint8 = 2 // base = min; residual[i] = uint64(vals[i]) - uint64(min)
	trConstant uint8 = 3 // no blocks at all: every value is the base

	zigzagFlag uint8 = 1 << 2

	// constantCost is what a constant column's payload occupies: the one value,
	// in the clear. Its header byte is counted separately, like every other
	// transform's.
	constantCost = 8
)

// subOverflows reports whether a-b overflows int64.
func subOverflows(a, b int64) bool {
	difference := a - b
	return (a^b)&(a^difference) < 0
}

// stepOverflows reports whether any delta between neighbours overflows int64.
func stepOverflows[T Signed](vals []T) bool {
	for index := 1; index < len(vals); index++ {
		if subOverflows(int64(vals[index]), int64(vals[index-1])) {
			return true
		}
	}
	return false
}

// residualsUnder is how many residuals a transform emits for n values. Delta
// writes its first value in the clear as a base, so it has one fewer.
func residualsUnder(transform uint8, n int) int {
	if transform == trDelta {
		return n - 1
	}
	return n
}

// residualAt is the residual transform makes of value index: the kernels'
// scalar tail, and the definition they are held to.
func residualAt(vals []int64, index int, transform uint8, base int64, zz bool) uint64 {
	switch transform {
	case trFOR:
		return uint64(vals[index]) - uint64(base)
	case trDelta:
		return zigzag(vals[index+1] - vals[index])
	}
	return encode(vals[index], zz)
}

// residualMode is the kernels' number for a transform: raw, raw zigzagged,
// frame of reference, delta.
func residualMode(transform uint8, zz bool) int {
	switch {
	case transform == trFOR:
		return 2
	case transform == trDelta:
		return 3
	case zz:
		return 1
	}
	return 0
}

// stats is the one pass everything the transforms are scored on needs: the
// minimum for the frame of reference, and so whether anything is negative;
// whether every value is the same; and whether a delta could overflow. vals is
// not empty.
func stats[T Signed](vals []T) (minimum int64, constant, deltaOverflows bool) {
	minimum, constant = int64(vals[0]), true
	for index, value := range vals {
		x := int64(value)
		if x < minimum {
			minimum = x
		}
		if x != int64(vals[0]) {
			constant = false
		}
		// A delta overflows int64 only across a span above 2^63, which only
		// int64 input can have.
		if index > 0 && subOverflows(x, int64(vals[index-1])) {
			deltaOverflows = true
		}
	}
	return minimum, constant, deltaOverflows
}

// blockedCosts is the payload size raw, frame of reference and, when delta is
// set, delta would produce, block widths and all — which is what a transform
// has to be scored against. Scoring it by the column's widest residual instead
// picks frame-of-reference over delta for a column of small ids.
//
// One pass for all three: a block's values are loaded once and scored three
// ways while they are in cache, where a pass per transform walked the column
// three times. A block's width is taken by OR-ing its residuals together, so
// there is not even a compare inside. blockedCost, in the tests, is the pass
// per transform it must agree with.
func blockedCosts[T Signed](vals []T, base int64, zz, delta bool) (raw, frame, steps int) {
	n := len(vals)
	frame, steps = 8, 8 // the base, in the clear
	for start := 0; start < n; start += blockSize {
		end := min(start+blockSize, n)
		var setRaw, setFrame uint64
		if zz {
			for _, value := range vals[start:end] {
				setRaw |= zigzag(int64(value))
				setFrame |= uint64(int64(value)) - uint64(base)
			}
		} else {
			for _, value := range vals[start:end] {
				setRaw |= uint64(int64(value))
				setFrame |= uint64(int64(value)) - uint64(base)
			}
		}
		raw += 1 + blockBytes(end-start, bits.Len64(setRaw))
		frame += 1 + blockBytes(end-start, bits.Len64(setFrame))
		// Delta has one residual fewer, so its blocks end one value early and
		// its last block can be one raw has and it does not.
		if stepsEnd := min(start+blockSize, n-1); delta && start < stepsEnd {
			var setSteps uint64
			for index := start; index < stepsEnd; index++ {
				setSteps |= zigzag(int64(vals[index+1]) - int64(vals[index]))
			}
			steps += 1 + blockBytes(stepsEnd-start, bits.Len64(setSteps))
		}
	}
	return raw, frame, steps
}

// encode applies the header's zigzag bit.
func encode(x int64, zz bool) uint64 {
	if zz {
		return zigzag(x)
	}
	return uint64(x)
}

// AppendArray encodes vals onto out and returns out. The bytes depend only on
// the values: the same values encode identically whatever T is.
func AppendArray[T Signed](out []byte, vals []T) []byte {
	if len(vals) == 0 {
		return append(out, trRaw)
	}
	if len(vals) <= shortColumn && forcedTransform < 0 {
		return appendShort(out, vals)
	}
	return appendLong(out, vals)
}

// shortColumn is the longest column appendShort encodes. Past it the scans'
// kernels win; below it their fixed cost per call, four of them a column, is
// most of the work.
const shortColumn = 32

// appendShort is AppendArray for a column of one short block, in two scalar
// passes rather than the four of the general path, and to the same bytes.
//
// It needs no OR of each transform's residuals: the width of an OR is the bit
// length of its largest member, and the largest residual raw and frame of
// reference make is the one of the column's maximum or minimum. So the first
// pass takes the extremes and the OR of the steps, which have no such shortcut;
// the second writes the residuals at the width already chosen.
func appendShort[T Signed](out []byte, vals []T) []byte {
	return appendScanned(out, vals, scanShort(vals))
}

// Scan is the first pass appendShort makes over a column: its extremes and the
// OR of its zigzagged steps. A caller that reads the values anyway, as a table
// gathering its rows does, can take it as it goes (Step) and hand it to
// AppendScanned, which then makes the second pass only.
type Scan struct {
	Low, High int64
	Steps     uint64
}

// StartScan is the scan of a column whose first value is first.
func StartScan(first int64) Scan {
	return Scan{Low: first, High: first}
}

// Step adds x, which follows previous in the column, to the scan.
func (s *Scan) Step(x, previous int64) {
	s.Low, s.High = min(s.Low, x), max(s.High, x)
	s.Steps |= zigzag(x - previous)
}

// Zigzag is the step's form a Scan ORs together, for a caller taking one.
func Zigzag(step int64) uint64 { return zigzag(step) }

// Zero is whether every value scanned is zero.
func (s Scan) Zero() bool {
	return s.Low == 0 && s.High == 0
}

// Scanned reports whether a column of n values is encoded from a Scan: a
// column too long for one block goes through the kernels, and one under the
// ablation's forced transform through appendLong.
func Scanned(n int) bool {
	return n > 0 && n <= shortColumn && forcedTransform < 0
}

// AppendScanned is AppendArray for a column Scanned says is, with its scan
// already taken: the same bytes.
func AppendScanned[T Signed](out []byte, vals []T, scan Scan) []byte {
	return appendScanned(out, vals, scan)
}

func scanShort[T Signed](vals []T) Scan {
	first := int64(vals[0])
	low, high, previous := first, first, first
	var steps uint64
	for _, value := range vals[1:] {
		x := int64(value)
		low, high = min(low, x), max(high, x)
		steps |= zigzag(x - previous)
		previous = x
	}
	return Scan{Low: low, High: high, Steps: steps}
}

func appendScanned[T Signed](out []byte, vals []T, scan Scan) []byte {
	n := len(vals)
	first := int64(vals[0])
	low, high, steps := scan.Low, scan.High, scan.Steps
	// Every step lies between low−high and high−low, so none overflows while
	// high−low does not; only a column spanning more than 2^63 is walked again
	// to see whether one does.
	overflows := subOverflows(high, low) && stepOverflows(vals)

	zz := low < 0
	rawWidth := bits.Len64(uint64(high))
	if zz {
		rawWidth = bits.Len64(max(zigzag(low), zigzag(high)))
	}
	transform, zigzagged, w := trRaw, zz, rawWidth
	best := 1 + blockBytes(n, rawWidth)
	frameWidth := bits.Len64(uint64(high) - uint64(low))
	if frame := 8 + 1 + blockBytes(n, frameWidth); frame < best {
		transform, zigzagged, w, best = trFOR, false, frameWidth, frame
	}
	if !overflows && n > 1 {
		stepsWidth := bits.Len64(steps)
		if cost := 8 + 1 + blockBytes(n-1, stepsWidth); cost < best {
			transform, zigzagged, w, best = trDelta, true, stepsWidth, cost
		}
	}
	if low == high && constantCost < best {
		return binary.LittleEndian.AppendUint64(append(out, trConstant), zigzag(low))
	}

	header := transform
	if zigzagged {
		header |= zigzagFlag
	}
	out = append(out, header)
	var scratch [shortColumn]uint64
	block := scratch[:residualsUnder(transform, n)]
	switch transform {
	case trFOR:
		out = binary.LittleEndian.AppendUint64(out, zigzag(low))
		for index := range block {
			block[index] = uint64(int64(vals[index])) - uint64(low)
		}
	case trDelta:
		out = binary.LittleEndian.AppendUint64(out, zigzag(first))
		for index := range block {
			block[index] = zigzag(int64(vals[index+1]) - int64(vals[index]))
		}
	default:
		for index := range block {
			block[index] = encode(int64(vals[index]), zz)
		}
	}
	return packRun(append(out, byte(w)), block, w)
}

// widenedPool holds the one column appendWidened widens into.
var widenedPool sync.Pool // *[]int64

// appendWidened is appendLong for an int32 column where there are only int64
// kernels: the column widened into a pooled buffer and encoded from there, to
// the same bytes, since they depend only on the values.
func appendWidened(out []byte, vals []int32) []byte {
	held, _ := widenedPool.Get().(*[]int64)
	if held == nil {
		held = new([]int64)
	}
	if cap(*held) < len(vals) {
		*held = make([]int64, len(vals))
	}
	wide := (*held)[:len(vals)]
	for index, value := range vals {
		wide[index] = int64(value)
	}
	out = appendLong(out, wide)
	widenedPool.Put(held)
	return out
}

// appendLong is AppendArray for a column past shortColumn, or under the
// ablation's forced transform.
func appendLong[T Signed](out []byte, vals []T) []byte {
	if narrow, ok := any(vals).([]int32); ok && NarrowColumns && !narrowKernels && useSIMD && forcedTransform < 0 {
		return appendWidened(out, narrow)
	}
	var minimum int64
	var constant, deltaOverflows bool
	var best, frame, steps int
	wide, isWide := any(vals).([]int64)
	narrow, isNarrow := any(vals).([]int32)
	isNarrow = isNarrow && narrowKernels && useSIMD
	scanned := (isWide && useSIMD || isNarrow) && forcedTransform < 0
	switch {
	case scanned && isNarrow:
		minimum, constant, deltaOverflows, best, frame, steps = scanSIMD32(narrow)
	case scanned:
		// Both scans in one; see scanSIMD.
		minimum, constant, deltaOverflows, best, frame, steps = scanSIMD(wide)
	case isWide && useSIMD:
		minimum, constant, deltaOverflows = statsSIMD(wide)
	default:
		minimum, constant, deltaOverflows = stats(vals)
	}
	// Raw is zigzagged whenever the column has a negative, which is what keeps
	// -1 from becoming a 64-bit residual.
	zz := minimum < 0
	if forced := forcedTransform; forced >= 0 {
		return appendForced(out, vals, uint8(forced), minimum, zz, deltaOverflows)
	}
	transform, zigzagged := trRaw, zz
	withDelta := !deltaOverflows && len(vals) > 1
	if !scanned {
		best, frame, steps = blockedCosts(vals, minimum, zz, withDelta)
	}
	if frame < best {
		transform, zigzagged, best = trFOR, false, frame
	}
	if withDelta && steps < best {
		transform, zigzagged, best = trDelta, true, steps
	}
	// Constant is scored rather than short-circuited. It is unbeatable on a long
	// column and beaten on a short one: three small values are three bytes raw
	// against the eight a base costs, so taking it on sight would make the
	// smallest columns the most expensive.
	if constant && constantCost < best {
		return binary.LittleEndian.AppendUint64(append(out, trConstant), zigzag(minimum))
	}

	header := transform
	if zigzagged {
		header |= zigzagFlag
	}
	out = append(out, header)
	switch transform {
	case trFOR:
		out = binary.LittleEndian.AppendUint64(out, zigzag(minimum))
	case trDelta:
		out = binary.LittleEndian.AppendUint64(out, zigzag(int64(vals[0])))
	}
	return appendBlocks(out, vals, transform, minimum, zigzagged)
}

// appendBlocks materialises each block's residuals into a scratch array on the
// stack and packs it. 128 uint64 is a kilobyte, which is worth not allocating
// and worth not walking the input twice for.
func appendBlocks[T Signed](out []byte, vals []T, transform uint8, base int64, zz bool) []byte {
	var scratch [blockSize]uint64
	n := residualsUnder(transform, len(vals))
	wide, isWide := any(vals).([]int64)
	narrow, isNarrow := any(vals).([]int32)
	isNarrow = isNarrow && narrowKernels && useSIMD
	for start := 0; start < n; start += blockSize {
		end := min(start+blockSize, n)
		block := scratch[:end-start]
		if isNarrow {
			w := bits.Len64(residualsSIMD32(narrow[start:], transform, base, zz, block))
			out = packRun(append(out, byte(w)), block, w)
			continue
		}
		if isWide && useSIMD {
			// The residuals and the OR their width is taken from, in one go.
			w := bits.Len64(residualsSIMD(wide[start:], transform, base, zz, block))
			out = packRun(append(out, byte(w)), block, w)
			continue
		}
		switch transform {
		case trFOR:
			for index := range block {
				block[index] = uint64(int64(vals[start+index])) - uint64(base)
			}
		case trDelta:
			for index := range block {
				at := start + index
				block[index] = zigzag(int64(vals[at+1]) - int64(vals[at]))
			}
		default:
			for index := range block {
				block[index] = encode(int64(vals[start+index]), zz)
			}
		}
		w := widthOf(block)
		out = packRun(append(out, byte(w)), block, w)
	}
	return out
}

// DecodeArray reads n values written by AppendArray into out and returns the
// number of bytes consumed from buf.
//
// T need not be the type the column was written from: any element type that
// can hold the values will do, and a value outside T's range is an error rather
// than a truncation. A header the encoder never writes is an error too; the
// transform and block widths are not checked against the ones the encoder would
// have chosen.
func DecodeArray[T Signed](buf []byte, n int, out []T) (int, error) {
	if n < 0 {
		return 0, errNegativeCount
	}
	if len(out) < n {
		return 0, errShortBuffer
	}
	if len(buf) < 1 {
		return 0, errTruncated
	}
	header := buf[0]
	position := 1
	if !headerWritten(header, n) {
		return 0, errBadHeader
	}
	if n == 0 {
		return position, nil
	}
	transform := header & 0x03
	zz := header&zigzagFlag != 0

	var base int64
	if transform != trRaw {
		if len(buf) < position+8 {
			return 0, errTruncated
		}
		base = unzigzag(binary.LittleEndian.Uint64(buf[position:]))
		position += 8
		// Delta, frame of reference and constant all write one of the column's
		// own values as the base, so it has to fit T as well.
		if !fits[T](base) {
			return 0, errOutOfRange
		}
	}
	if transform == trConstant {
		for index := range n {
			out[index] = T(base)
		}
		return position, nil
	}

	// Delta's first value is the base itself; the blocks carry the steps.
	target := out[:n]
	if transform == trDelta {
		out[0] = T(base)
		target = out[1:n]
	}

	// Delta accumulates in an int64 rather than in out[i], because a step can
	// exceed T's range even when every value fits it: for int8, 127-(-128) is
	// 255. Storing the step first and summing afterwards would truncate it.
	accumulator := base

	var scratch [blockSize]uint64
	for start := 0; start < len(target); start += blockSize {
		end := min(start+blockSize, len(target))
		if position >= len(buf) {
			return 0, errTruncated
		}
		w := int(buf[position])
		position++
		if w > 64 {
			return 0, errBadWidth
		}
		bytes := blockBytes(end-start, w)
		if len(buf)-position < bytes {
			return 0, errTruncated
		}
		if w <= wideWidth && len(buf)-position >= bytes+8 {
			// Eight bytes past the block: every value is one load, so it is
			// read and transformed in one go, with no scratch between.
			spill := readFused(buf[position:], target[start:end], w, transform, zz, base, &accumulator)
			if spill != 0 {
				return 0, errOutOfRange
			}
			position += bytes
			continue
		}
		block := scratch[:end-start]
		unpackRun(buf[position:], block, w)
		position += bytes
		// spill collects the bits each value loses on its way into T, so the
		// range check is one test per block rather than a branch per value. For
		// int64 it is constant zero.
		var spill int64
		switch transform {
		case trFOR:
			for index, residual := range block {
				value := int64(uint64(base) + residual)
				target[start+index] = T(value)
				spill |= value ^ int64(T(value))
			}
		case trDelta:
			for index, residual := range block {
				accumulator += unzigzag(residual)
				target[start+index] = T(accumulator)
				spill |= accumulator ^ int64(T(accumulator))
			}
		default:
			for index, residual := range block {
				value := decode(residual, zz)
				target[start+index] = T(value)
				spill |= value ^ int64(T(value))
			}
		}
		if spill != 0 {
			return 0, errOutOfRange
		}
	}
	return position, nil
}

// headerWritten reports whether AppendArray writes header for a column of n
// values: the empty column's lone raw header, or one of the five headers a
// non-empty column can open with — raw with or without zigzag, delta (always
// zigzagged), frame of reference and constant (never).
func headerWritten(header uint8, n int) bool {
	if n == 0 {
		return header == trRaw
	}
	switch header {
	case trRaw, trRaw | zigzagFlag, trDelta | zigzagFlag, trFOR, trConstant:
		return true
	}
	return false
}

// fits reports whether v is in T's range.
func fits[T Signed](v int64) bool { return int64(T(v)) == v }

// readFused is unpackRun and DecodeArray's inverse transform as one pass: each
// residual is loaded, masked and turned back into its value in the same
// iteration, where the two passes wrote a block into scratch and read it back.
// buf must hold eight bytes past the block, and w be at most wideWidth. It
// returns the bits the values lost on their way into T, as DecodeArray's own
// loops do.
func readFused[T Signed](buf []byte, target []T, w int, transform uint8, zz bool, base int64, accumulator *int64) (spill int64) {
	mask := uint64(1)<<w - 1
	position := 0
	switch {
	case transform == trFOR:
		for index := range target {
			value := int64(uint64(base) + (binary.LittleEndian.Uint64(buf[position>>3:])>>(position&7))&mask)
			position += w
			target[index] = T(value)
			spill |= value ^ int64(T(value))
		}
	case transform == trDelta:
		sum := *accumulator
		for index := range target {
			sum += unzigzag((binary.LittleEndian.Uint64(buf[position>>3:]) >> (position & 7)) & mask)
			position += w
			target[index] = T(sum)
			spill |= sum ^ int64(T(sum))
		}
		*accumulator = sum
	case zz:
		for index := range target {
			value := unzigzag((binary.LittleEndian.Uint64(buf[position>>3:]) >> (position & 7)) & mask)
			position += w
			target[index] = T(value)
			spill |= value ^ int64(T(value))
		}
	default:
		for index := range target {
			value := int64((binary.LittleEndian.Uint64(buf[position>>3:]) >> (position & 7)) & mask)
			position += w
			target[index] = T(value)
			spill |= value ^ int64(T(value))
		}
	}
	return spill
}

// decode inverts encode.
func decode(u uint64, zz bool) int64 {
	if zz {
		return unzigzag(u)
	}
	return int64(u)
}
