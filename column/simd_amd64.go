//go:build goexperiment.simd && amd64 && !purego

package column

// AVX2 kernels for the encoder's scans, through the experimental simd/archsimd
// package: built only with GOEXPERIMENT=simd, on amd64, and not under the
// purego tag, which is how two builds of the same code are compared.
//
// They are the scans the Go compiler leaves scalar, since it does not
// vectorise: an OR over a block, a minimum, a compare against the first value,
// and the zigzag and differences the transforms are scored on. Four int64
// lanes at a time, with a scalar tail. Each returns exactly what its scalar
// twin does, which the tests hold them to, so the bytes do not change.
//
// AVX2 has no 64-bit minimum and no 64-bit arithmetic shift, so the minimum is
// a compare and a blend, and a zigzag's sign is a compare against zero.

import (
	"math/bits"
	"simd/archsimd"
)

// useSIMD is whether this CPU runs the kernels, decided once.
var useSIMD = archsimd.X86.AVX2()

// unpackVectorised says unpackSIMD reads blocks in this build; the assembly
// build leaves them to the scalar read.
const unpackVectorised = true

// packVectorWidth: the block write is the assembly build's only.
const packVectorWidth = 0

func packSIMD(out []byte, block []uint64, w int) []byte { return out }

// statsSIMD is stats for []int64.
func statsSIMD(vals []int64) (minimum int64, constant, deltaOverflows bool) {
	defer archsimd.ClearAVXUpperBits()
	n := len(vals)
	first := archsimd.BroadcastInt64x4(vals[0])
	low := first
	var differs, overflow archsimd.Int64x4
	index := 0
	for ; index+4 <= n; index += 4 {
		value := archsimd.LoadInt64x4(vals[index:])
		low = value.IfElse(value.Less(low), low)
		differs = differs.Or(value.Xor(first))
		if index > 0 {
			// subOverflows, a lane at a time: a sign bit set in
			// (a^b)&(a^(a-b)) is an overflow.
			previous := archsimd.LoadInt64x4(vals[index-1:])
			overflow = overflow.Or(value.Xor(previous).And(value.Xor(value.Sub(previous))))
		}
	}
	var lanes [4]int64
	low.StoreArray(&lanes)
	minimum = min(lanes[0], lanes[1], lanes[2], lanes[3])
	if index == 0 {
		minimum = vals[0]
	}
	var spread int64
	differs.StoreArray(&lanes)
	spread = lanes[0] | lanes[1] | lanes[2] | lanes[3]
	overflow.StoreArray(&lanes)
	deltaOverflows = (lanes[0] | lanes[1] | lanes[2] | lanes[3]) < 0
	// The first vector's differences, which the loop skipped for want of a
	// value before it, and the tail.
	for at := 1; at < min(4, n); at++ {
		deltaOverflows = deltaOverflows || subOverflows(vals[at], vals[at-1])
	}
	for ; index < n; index++ {
		value := vals[index]
		minimum = min(minimum, value)
		spread |= value ^ vals[0]
		if index >= 4 {
			deltaOverflows = deltaOverflows || subOverflows(value, vals[index-1])
		}
	}
	return minimum, spread == 0, deltaOverflows
}

// blockedCostsSIMD is blockedCosts for []int64.
func blockedCostsSIMD(vals []int64, base int64, zz, delta bool) (raw, frame, steps int) {
	defer archsimd.ClearAVXUpperBits()
	n := len(vals)
	frame, steps = 8, 8 // the base, in the clear
	baseVector := archsimd.BroadcastInt64x4(base)
	var zero archsimd.Int64x4
	for start := 0; start < n; start += blockSize {
		end := min(start+blockSize, n)
		var setRaw, setFrame archsimd.Int64x4
		index := start
		for ; index+4 <= end; index += 4 {
			value := archsimd.LoadInt64x4(vals[index:])
			if zz {
				setRaw = setRaw.Or(zigzagLanes(value, zero))
			} else {
				setRaw = setRaw.Or(value)
			}
			setFrame = setFrame.Or(value.Sub(baseVector))
		}
		rawSet, frameSet := orLanes(setRaw), orLanes(setFrame)
		for ; index < end; index++ {
			value := vals[index]
			rawSet |= encode(value, zz)
			frameSet |= uint64(value) - uint64(base)
		}
		raw += 1 + blockBytes(end-start, bits.Len64(rawSet))
		frame += 1 + blockBytes(end-start, bits.Len64(frameSet))
		// Delta has one residual fewer, so its blocks end one value early.
		if stepsEnd := min(start+blockSize, n-1); delta && start < stepsEnd {
			var setSteps archsimd.Int64x4
			index := start
			for ; index+4 <= stepsEnd; index += 4 {
				next := archsimd.LoadInt64x4(vals[index+1:])
				setSteps = setSteps.Or(zigzagLanes(next.Sub(archsimd.LoadInt64x4(vals[index:])), zero))
			}
			stepsSet := orLanes(setSteps)
			for ; index < stepsEnd; index++ {
				stepsSet |= zigzag(vals[index+1] - vals[index])
			}
			steps += 1 + blockBytes(stepsEnd-start, bits.Len64(stepsSet))
		}
	}
	return raw, frame, steps
}

// zigzagLanes is zigzag on each lane: the value shifted up, XORed with its
// sign smeared across the lane, which is a compare against zero here.
func zigzagLanes(value, zero archsimd.Int64x4) archsimd.Int64x4 {
	return value.ShiftAllLeft(1).Xor(zero.Greater(value).ToInt64x4())
}

// orLanes ORs a vector's four lanes together.
func orLanes(vector archsimd.Int64x4) uint64 {
	var lanes [4]int64
	vector.StoreArray(&lanes)
	return uint64(lanes[0] | lanes[1] | lanes[2] | lanes[3])
}

// residualsSIMD fills block with the residuals of vals' first len(block)
// values, and returns their OR, which is the block's width.
func residualsSIMD(vals []int64, transform uint8, base int64, zz bool, block []uint64) uint64 {
	defer archsimd.ClearAVXUpperBits()
	baseVector := archsimd.BroadcastInt64x4(base)
	var zero, set archsimd.Int64x4
	index := 0
	for ; index+4 <= len(block); index += 4 {
		value := archsimd.LoadInt64x4(vals[index:])
		var residual archsimd.Int64x4
		switch {
		case transform == trFOR:
			residual = value.Sub(baseVector)
		case transform == trDelta:
			residual = zigzagLanes(archsimd.LoadInt64x4(vals[index+1:]).Sub(value), zero)
		case zz:
			residual = zigzagLanes(value, zero)
		default:
			residual = value
		}
		residual.AsUint64x4().Store(block[index:])
		set = set.Or(residual)
	}
	result := orLanes(set)
	for ; index < len(block); index++ {
		block[index] = residualAt(vals, index, transform, base, zz)
		result |= block[index]
	}
	return result
}

// unpackSIMD is unpackRun's read, four values to an instruction, for w up to
// wideWidth. It unpacks whole groups of eight values while the loads stay
// inside buf, and reports how many values it did; unpackRun does the rest.
//
// Eight values at w bits are w bytes, so value 8j+k starts at byte j*w +
// k*w/8 and bit k*w%8, whatever j is: the layout repeats every group, and all
// that changes from one to the next is the group's first byte. Two values
// next to each other span at most sixteen bytes from the first's byte, so a
// 16-byte load holds a pair, a byte shuffle within each 128-bit half moves
// each value's eight bytes into its own lane, and a shift per lane and a mask
// finish it. The shuffle, the shifts and where the loads start are the same
// for every group of a width, and are worked out once, in unpackTables.
func unpackSIMD(buf []byte, dst []uint64, w int) int {
	defer archsimd.ClearAVXUpperBits()
	table := &unpackTables[w]
	mask := archsimd.BroadcastUint64x4(uint64(1)<<w - 1)
	first, second := &table[0], &table[1]
	firstShuffle := archsimd.LoadInt8x32Array(&first.shuffle)
	secondShuffle := archsimd.LoadInt8x32Array(&second.shuffle)
	firstShifts := archsimd.LoadUint64x4Array(&first.shifts)
	secondShifts := archsimd.LoadUint64x4Array(&second.shifts)
	var empty archsimd.Uint8x32
	done, at := 0, 0
	// The furthest load starts before the group's last byte and reads
	// sixteen, so a group is safe while its w bytes and sixteen more are there.
	for done+8 <= len(dst) && at+w+16 <= len(buf) {
		pairs := empty.SetLo(archsimd.LoadUint8x16(buf[at+first.lo:])).
			SetHi(archsimd.LoadUint8x16(buf[at+first.hi:]))
		pairs.PermuteOrZeroGrouped(firstShuffle).AsUint64x4().
			ShiftRight(firstShifts).And(mask).Store(dst[done:])
		pairs = empty.SetLo(archsimd.LoadUint8x16(buf[at+second.lo:])).
			SetHi(archsimd.LoadUint8x16(buf[at+second.hi:]))
		pairs.PermuteOrZeroGrouped(secondShuffle).AsUint64x4().
			ShiftRight(secondShifts).And(mask).Store(dst[done+4:])
		done += 8
		at += w
	}
	return done
}

// unpackHalf is how unpackSIMD reads four of a group's eight values: the
// bytes, from the group's first, its two loads start at; the shuffle that
// moves each value's bytes into its lane; and each lane's shift.
type unpackHalf struct {
	lo, hi  int
	shuffle [32]int8
	shifts  [4]uint64
}

// unpackTables is every width's two halves.
var unpackTables = func() (tables [wideWidth + 1][2]unpackHalf) {
	for w := 1; w <= wideWidth; w++ {
		for half := range 2 {
			table := &tables[w][half]
			// The half's values are k, k+1 from the low load and k+2, k+3
			// from the high one.
			k := 4 * half
			table.lo, table.hi = k*w/8, (k+2)*w/8
			for lane := range 4 {
				value := k + lane
				load := table.lo
				if lane >= 2 {
					load = table.hi
				}
				// Within its 128-bit half the shuffle indexes the half's
				// own sixteen bytes, which start at load.
				from := value*w/8 - load
				for b := range 8 {
					table.shuffle[lane*8+b] = int8(from + b)
				}
				table.shifts[lane] = uint64(value * w % 8)
			}
		}
	}
	return tables
}()

// orSIMD ORs a block of residuals together, which is widthOf's scan.
func orSIMD(block []uint64) uint64 {
	defer archsimd.ClearAVXUpperBits()
	var set archsimd.Uint64x4
	index := 0
	for ; index+4 <= len(block); index += 4 {
		set = set.Or(archsimd.LoadUint64x4(block[index:]))
	}
	var lanes [4]uint64
	set.StoreArray(&lanes)
	result := lanes[0] | lanes[1] | lanes[2] | lanes[3]
	for ; index < len(block); index++ {
		result |= block[index]
	}
	return result
}
