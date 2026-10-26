//go:build amd64 && !purego

package column

import (
	"math"
	"math/bits"
)

// AVX2 kernels for the encoder's scans, in assembly (asm_amd64.s), for every
// amd64 build that is not purego. Their Go side, shared with arm64's NEON, is vec.go.

// useSIMD is whether this CPU runs the kernels: AVX2, and an operating system
// that saves the YMM registers, decided once.
var useSIMD = hasAVX2()

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

// scanVec is scanSIMD's pass over blocks runs of size values, size a positive
// multiple of four: each run's minimum, maximum and OR of zigzagged steps to
// out, three words a run. values holds blocks*size+1.
//
//go:noescape
func scanVec(values *int64, blocks, size int, out *int64)

// scanVec32 is scanVec over int32 values.
//
//go:noescape
func scanVec32(values *int32, blocks, size int, out *int64)

// residualsVec32 is residualsVec over int32 values.
//
//go:noescape
func residualsVec32(values *int32, n int, base int64, mode int, dst *uint64, out *[4]uint64)

// narrowKernels is whether []int32 columns have kernels of their own.
const narrowKernels = true

// NarrowKernels is narrowKernels for the codec: a table keeps an integer column at
// int32 only where there are kernels for it, since widening one for the int64
// kernels costs the pass the narrow scratch saves.
const NarrowKernels = narrowKernels

// scanCall is the scan kernel for T's width.
func scanCall[T int32 | int64](values *T, blocks, size int, out *int64) {
	switch pointer := any(values).(type) {
	case *int64:
		scanVec(pointer, blocks, size, out)
	case *int32:
		scanVec32(pointer, blocks, size, out)
	}
}

// scanSIMD32 is scanSIMD for a column of int32.
func scanSIMD32(vals []int32) (int64, bool, bool, int, int, int) { return scanSIMD(vals) }

// residualsSIMD32 is residualsSIMD for a column of int32.
func residualsSIMD32(vals []int32, transform uint8, base int64, zz bool, block []uint64) uint64 {
	var set uint64
	full := len(block) &^ 3
	if full > 0 {
		var lanes [4]uint64
		residualsVec32(&vals[0], full, base, residualMode(transform, zz), &block[0], &lanes)
		set = lanes[0] | lanes[1] | lanes[2] | lanes[3]
	}
	for index := full; index < len(block); index++ {
		var residual uint64
		switch value := int64(vals[index]); transform {
		case trFOR:
			residual = uint64(value) - uint64(base)
		case trDelta:
			residual = zigzag(int64(vals[index+1]) - value)
		default:
			residual = encode(value, zz)
		}
		block[index] = residual
		set |= residual
	}
	return set
}

// scanChunk is how many blocks one scanVec call summarises.
const scanChunk = 32

// scanSIMD is statsSIMD and then blockedCostsSIMD, with the costs scored as
// appendLong would, in one read of the column rather than two.
//
// It needs no OR of a block's raw or frame residuals: the width of an OR is the
// bit length of its largest member, and the largest raw or frame residual is
// the one of the block's maximum or minimum, as appendShort has it. So the
// pass takes each block's extremes and the OR of its steps, which have no
// such shortcut, and the costs are worked out from those. Frame's need the
// column's minimum, which is only known at the end, so each block's maximum is
// kept for them.
//
// Nor does it test every step for overflow: a step is at most the column's
// range, so none can overflow unless the range does, and only then, which
// takes values more than 2^63 apart, are the steps tested one by one.
func scanSIMD[T int32 | int64](vals []T) (minimum int64, constant, deltaOverflows bool, raw, frame, steps int) {
	n := len(vals)
	blocks := (n + blockSize - 1) / blockSize
	var spare [512]int64
	highs := spare[:0]
	if blocks > len(spare) {
		highs = make([]int64, 0, blocks)
	}
	minimum = int64(vals[0])
	maximum := minimum
	var plain, zigzagged, stepsCost int // raw unzigzagged and zigzagged, and delta's blocks
	add := func(low, high int64, stepSet uint64, count, stepCount int) {
		minimum, maximum = min(minimum, low), max(maximum, high)
		plain += 1 + blockBytes(count, bits.Len64(uint64(high)))
		zigzagged += 1 + blockBytes(count, bits.Len64(max(zigzag(low), zigzag(high))))
		if stepCount > 0 {
			stepsCost += 1 + blockBytes(stepCount, bits.Len64(stepSet))
		}
		highs = append(highs, high)
	}

	// Every block but the last has a value after it, so its steps are whole
	// and the kernel's.
	var summaries [scanChunk][3]int64
	whole := (n - 1) / blockSize
	for block := 0; block < whole; {
		count := min(scanChunk, whole-block)
		scanCall(&vals[block*blockSize], count, blockSize, &summaries[0][0])
		for _, summary := range summaries[:count] {
			add(summary[0], summary[1], uint64(summary[2]), blockSize, blockSize)
		}
		block += count
	}

	// The last block: the kernel as far as its steps run to whole vectors,
	// and the rest one value at a time.
	start := whole * blockSize
	low, high := int64(vals[start]), int64(vals[start])
	var stepSet uint64
	at := start
	if size := (n - 1 - start) &^ 3; size > 0 {
		scanCall(&vals[start], 1, size, &summaries[0][0])
		low, high, stepSet = summaries[0][0], summaries[0][1], uint64(summaries[0][2])
		at += size
	}
	for ; at < n; at++ {
		value := int64(vals[at])
		low, high = min(low, value), max(high, value)
		if at+1 < n {
			next := int64(vals[at+1])
			stepSet |= zigzag(next - value)
		}
	}
	add(low, high, stepSet, n-start, n-1-start)

	if uint64(maximum)-uint64(minimum) > math.MaxInt64 {
		for index := 1; index < n && !deltaOverflows; index++ {
			deltaOverflows = subOverflows(int64(vals[index]), int64(vals[index-1]))
		}
	}
	constant = minimum == maximum
	raw = plain
	if minimum < 0 {
		raw = zigzagged
	}
	frame, steps = 8, 8 // the base, in the clear
	for block, high := range highs {
		count := min(blockSize, n-block*blockSize)
		frame += 1 + blockBytes(count, bits.Len64(uint64(high)-uint64(minimum)))
	}
	if !deltaOverflows && n > 1 {
		steps += stepsCost
	}
	return minimum, constant, deltaOverflows, raw, frame, steps
}

func cpuid(leaf, subleaf uint32) (eax, ebx, ecx, edx uint32)

func xgetbv() uint32
