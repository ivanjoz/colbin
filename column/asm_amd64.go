//go:build amd64 && !purego && !goexperiment.simd

package column

// AVX2 kernels for the encoder's scans, in assembly (asm_amd64.s), for every
// amd64 build that is not purego. They are the scans simd_amd64.go writes
// with the experimental simd/archsimd package, which a build with
// GOEXPERIMENT=simd uses instead; this is the same work for everyone else.
//
// The assembly does only the vector loops: it runs over a whole number of
// four-value vectors and stores its accumulators, and the Go here folds their
// lanes and does the values left over. Each function returns exactly what its
// scalar twin does, which the tests hold them to, so the bytes do not change.

import "math/bits"

// useSIMD is whether this CPU runs the kernels: AVX2, and an operating system
// that saves the YMM registers, decided once.
var useSIMD = hasAVX2()

// unpackVectorised: the block read is not in assembly. See simd_amd64.go.
const unpackVectorised = false

func hasAVX2() bool {
	maxLeaf, _, _, _ := cpuid(0, 0)
	if maxLeaf < 7 {
		return false
	}
	_, _, features, _ := cpuid(1, 0)
	const osxsave, avx = 1 << 27, 1 << 28
	if features&osxsave == 0 || features&avx == 0 {
		return false
	}
	// XCR0 bits 1 and 2: the OS saves the XMM and YMM state.
	if xgetbv()&0b110 != 0b110 {
		return false
	}
	_, extended, _, _ := cpuid(7, 0)
	return extended&(1<<5) != 0
}

func cpuid(leaf, subleaf uint32) (eax, ebx, ecx, edx uint32)

func xgetbv() uint32

// The vector loops. n is a positive multiple of four.

// orAVX2 ORs n values into out's four lanes.
//
//go:noescape
func orAVX2(values *uint64, n int, out *[4]uint64)

// statsAVX2 stores into out the lanes of the minimum, of the OR of every
// value XOR the first, and of subOverflows' sign word for every value from
// the fifth on against the one before it.
//
//go:noescape
func statsAVX2(values *int64, n int, out *[12]int64)

// blockSetsAVX2 stores into out the lanes of the OR of the raw residuals,
// zigzagged when zz, and then of the frame-of-reference ones against base.
//
//go:noescape
func blockSetsAVX2(values *int64, n int, base int64, zz bool, out *[8]uint64)

// stepsAVX2 stores into out the lanes of the OR of zigzag(values[i+1] -
// values[i]) for i below n, so values holds n+1.
//
//go:noescape
func stepsAVX2(values *int64, n int, out *[4]uint64)

// residualsAVX2 stores the residuals of n values under mode (residualMode)
// into dst, and the lanes of their OR into out. For delta, values holds n+1.
//
//go:noescape
func residualsAVX2(values *int64, n int, base int64, mode int, dst *uint64, out *[4]uint64)

// residualsSIMD fills block with the residuals of vals' first len(block)
// values, and returns their OR, which is the block's width.
func residualsSIMD(vals []int64, transform uint8, base int64, zz bool, block []uint64) uint64 {
	var set uint64
	full := len(block) &^ 3
	if full > 0 {
		var lanes [4]uint64
		residualsAVX2(&vals[0], full, base, residualMode(transform, zz), &block[0], &lanes)
		set = lanes[0] | lanes[1] | lanes[2] | lanes[3]
	}
	for index := full; index < len(block); index++ {
		block[index] = residualAt(vals, index, transform, base, zz)
		set |= block[index]
	}
	return set
}

// statsSIMD is stats for []int64.
func statsSIMD(vals []int64) (minimum int64, constant, deltaOverflows bool) {
	n := len(vals)
	full := n &^ 3
	minimum = vals[0]
	var spread int64
	if full > 0 {
		var lanes [12]int64
		statsAVX2(&vals[0], full, &lanes)
		minimum = min(lanes[0], lanes[1], lanes[2], lanes[3])
		spread = lanes[4] | lanes[5] | lanes[6] | lanes[7]
		deltaOverflows = lanes[8]|lanes[9]|lanes[10]|lanes[11] < 0
	}
	// The first vector's differences, which have no value before the
	// vector to be loaded from, and the tail.
	for at := 1; at < min(4, n); at++ {
		deltaOverflows = deltaOverflows || subOverflows(vals[at], vals[at-1])
	}
	for index := full; index < n; index++ {
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
	n := len(vals)
	frame, steps = 8, 8 // the base, in the clear
	for start := 0; start < n; start += blockSize {
		end := min(start+blockSize, n)
		var rawSet, frameSet uint64
		index := start
		if full := (end - start) &^ 3; full > 0 {
			var lanes [8]uint64
			blockSetsAVX2(&vals[start], full, base, zz, &lanes)
			rawSet = lanes[0] | lanes[1] | lanes[2] | lanes[3]
			frameSet = lanes[4] | lanes[5] | lanes[6] | lanes[7]
			index += full
		}
		for ; index < end; index++ {
			value := vals[index]
			rawSet |= encode(value, zz)
			frameSet |= uint64(value) - uint64(base)
		}
		raw += 1 + blockBytes(end-start, bits.Len64(rawSet))
		frame += 1 + blockBytes(end-start, bits.Len64(frameSet))
		// Delta has one residual fewer, so its blocks end one value early.
		if stepsEnd := min(start+blockSize, n-1); delta && start < stepsEnd {
			var stepsSet uint64
			index := start
			if full := (stepsEnd - start) &^ 3; full > 0 {
				var lanes [4]uint64
				stepsAVX2(&vals[start], full, &lanes)
				stepsSet = lanes[0] | lanes[1] | lanes[2] | lanes[3]
				index += full
			}
			for ; index < stepsEnd; index++ {
				stepsSet |= zigzag(vals[index+1] - vals[index])
			}
			steps += 1 + blockBytes(stepsEnd-start, bits.Len64(stepsSet))
		}
	}
	return raw, frame, steps
}

// orSIMD ORs a block of residuals together, which is widthOf's scan.
func orSIMD(block []uint64) uint64 {
	var set uint64
	full := len(block) &^ 3
	if full > 0 {
		var lanes [4]uint64
		orAVX2(&block[0], full, &lanes)
		set = lanes[0] | lanes[1] | lanes[2] | lanes[3]
	}
	for _, residual := range block[full:] {
		set |= residual
	}
	return set
}

// unpackSIMD reads nothing in this build; unpackRun does it all.
func unpackSIMD(buf []byte, dst []uint64, w int) int { return 0 }
