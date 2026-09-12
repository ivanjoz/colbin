package column

import (
	"encoding/binary"
	"math/bits"
	"unsafe"
)

// Array codec: the integer input is mapped to a sequence of unsigned residuals
// by one of four transforms, and the residuals are then written as blocks of 128
// at a bit width chosen per block (see column.go). The encoder scores every
// transform against what it would actually occupy in blocks and picks the
// smallest; the choice is recorded in the header byte.

// Signed is the set of element types the codec handles natively. Values are
// widened to int64 internally, which is free for every member.
//
// int is deliberately excluded. Its width is platform-dependent, so an []int
// encoded on a 64-bit host would not decode on a 32-bit one; the width is
// derived from the type rather than stored, so that mismatch would be silent.
// Convert to a fixed-width type at the call site instead.
type Signed interface {
	~int8 | ~int16 | ~int32 | ~int64
}

// widthOfType is the element width in bytes. unsafe.Sizeof on a type parameter
// resolves per instantiation, so encoder and decoder derive the same width from
// the same type and it never goes on the wire.
func widthOfType[T Signed]() uint8 {
	var zero T
	return uint8(unsafe.Sizeof(zero))
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

// residualsUnder is how many residuals a transform emits for n values. Delta
// writes its first value in the clear as a base, so it has one fewer.
func residualsUnder(transform uint8, n int) int {
	if transform == trDelta {
		return n - 1
	}
	return n
}

// blockedCost is the payload size a transform would produce, block widths and
// all — which is what the transform has to be scored against. Scoring it by the
// column's widest residual instead picks frame-of-reference for a column of
// small ids and loses 76%, where scoring it this way picks delta and loses 14%.
//
// One pass over the data per candidate, with the residual inlined into the loop
// and the block's width taken by OR-ing its residuals together, so there is not
// even a compare inside.
func blockedCost[T Signed](vals []T, transform uint8, base int64, zz bool) int {
	n := residualsUnder(transform, len(vals))
	total := 0
	if transform != trRaw {
		total += 8 // the base, in the clear
	}
	for start := 0; start < n; start += blockSize {
		end := min(start+blockSize, n)
		var set uint64
		switch transform {
		case trFOR:
			for _, value := range vals[start:end] {
				set |= uint64(int64(value)) - uint64(base)
			}
		case trDelta:
			for index := start; index < end; index++ {
				set |= zigzag(int64(vals[index+1]) - int64(vals[index]))
			}
		default:
			for _, value := range vals[start:end] {
				set |= encode(int64(value), zz)
			}
		}
		total += 1 + blockBytes(end-start, bits.Len64(set))
	}
	return total
}

// encode applies the header's zigzag bit.
func encode(x int64, zz bool) uint64 {
	if zz {
		return zigzag(x)
	}
	return uint64(x)
}

// AppendArray encodes vals onto out and returns out. The element width comes
// from T and is not stored, so DecodeArray must be instantiated with the same
// type.
func AppendArray[T Signed](out []byte, vals []T) []byte {
	if len(vals) == 0 {
		return append(out, trRaw)
	}

	// One pass for everything the transforms need to be scored: the minimum for
	// the frame of reference, whether anything is negative, whether every value
	// is the same, and whether a delta could overflow.
	minimum, maximum := int64(vals[0]), int64(vals[0])
	constant := true
	deltaOverflows := false
	for index, value := range vals {
		x := int64(value)
		if x < minimum {
			minimum = x
		}
		if x > maximum {
			maximum = x
		}
		if x != int64(vals[0]) {
			constant = false
		}
		// A delta can only overflow int64 for int64 input, and only across a
		// span above 2^63.
		if index > 0 && widthOfType[T]() == 8 && subOverflows(x, int64(vals[index-1])) {
			deltaOverflows = true
		}
	}
	// Raw is zigzagged whenever the column has a negative, which is what keeps
	// -1 from becoming a 64-bit residual.
	zz := minimum < 0
	transform, zigzagged := trRaw, zz
	best := blockedCost(vals, trRaw, 0, zz)
	if cost := blockedCost(vals, trFOR, minimum, false); cost < best {
		transform, zigzagged, best = trFOR, false, cost
	}
	if !deltaOverflows && len(vals) > 1 {
		if cost := blockedCost(vals, trDelta, 0, true); cost < best {
			transform, zigzagged, best = trDelta, true, cost
		}
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
	for start := 0; start < n; start += blockSize {
		end := min(start+blockSize, n)
		block := scratch[:end-start]
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
// number of bytes consumed from buf. It must be instantiated with the same
// element type used to encode.
func DecodeArray[T Signed](buf []byte, n int, out []T) (int, error) {
	if n < 0 {
		return 0, ErrNegativeCount
	}
	if len(out) < n {
		return 0, ErrShortBuffer
	}
	if len(buf) < 1 {
		return 0, ErrTruncated
	}
	header := buf[0]
	position := 1
	transform := header & 0x03
	zz := header&zigzagFlag != 0
	if n == 0 {
		return position, nil
	}

	var base int64
	if transform != trRaw {
		if len(buf) < position+8 {
			return 0, ErrTruncated
		}
		base = unzigzag(binary.LittleEndian.Uint64(buf[position:]))
		position += 8
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
			return 0, ErrTruncated
		}
		w := int(buf[position])
		position++
		if w > 64 {
			return 0, ErrBadWidth
		}
		bytes := blockBytes(end-start, w)
		if len(buf)-position < bytes {
			return 0, ErrTruncated
		}
		block := scratch[:end-start]
		unpackRun(buf[position:], block, w)
		position += bytes
		switch transform {
		case trFOR:
			for index, residual := range block {
				target[start+index] = T(int64(uint64(base) + residual))
			}
		case trDelta:
			for index, residual := range block {
				accumulator += unzigzag(residual)
				target[start+index] = T(accumulator)
			}
		default:
			for index, residual := range block {
				target[start+index] = T(decode(residual, zz))
			}
		}
	}
	return position, nil
}

// decode inverts encode.
func decode(u uint64, zz bool) int64 {
	if zz {
		return unzigzag(u)
	}
	return int64(u)
}
