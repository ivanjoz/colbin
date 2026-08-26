package varint

import (
	"math/bits"
	"unsafe"
)

// Array codec: the integer input is mapped to a sequence of unsigned residuals
// by one of four transforms, and the residuals are then written with the (k, M)
// varint from varint.go. The encoder scores every transform and picks the
// smallest; the choice is recorded in the header byte.
//
// M >= k always holds, so the header stores d = M-k rather than M, spending no
// space on impossible states. d is drawn from dCodes: the linear range 0..7
// would leave k=1 unreachable for bmax >= 58, forcing a 2-byte floor onto every
// small value in a wide-range int64 column, so code 7 maps to d=8 instead. The
// only cost is that M = k+7 is unreachable, which is strictly worse solely when
// bmax == 57 exactly.
//
// Header byte:
//
//	bits 0-1  transform (trRaw / trDelta / trFOR / trFixed)
//	bit  2    zigzag applied to residuals
//	bits 3-4  k-1        (k = 1..4; leading flag-free bytes = k-1)
//	bits 5-7  d code     (M = k + dCodes[code])
//
// The element count is not stored; it is known from the record count, matching
// the convention of the other colbin column codecs.

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

// widthOf is the element width in bytes. unsafe.Sizeof on a type parameter
// resolves per instantiation, so encoder and decoder derive the same width from
// the same type and it never goes on the wire.
func widthOf[T Signed]() uint8 {
	var zero T
	return uint8(unsafe.Sizeof(zero))
}

const (
	trRaw   uint8 = 0 // residual[i] = enc(vals[i])
	trDelta uint8 = 1 // residual[0] = enc(vals[0]); residual[i] = enc(vals[i]-vals[i-1])
	trFOR   uint8 = 2 // residual[0] = zigzag(min); residual[i+1] = uint64(vals[i]-min)
	trFixed uint8 = 3 // no varint: n little-endian words of the element width
)

// dCodes maps the 3-bit header code to d = M-k. Code 7 is 8 rather than 7 so
// that k=1 can still reach M=9, which full-width 64-bit columns need.
var dCodes = [8]uint8{0, 1, 2, 3, 4, 5, 6, 8}

// k is stored as k-1 in two bits.
const (
	minK uint8 = 1
	maxK uint8 = 4
)

// signExtend interprets the low width bits of v as a two's-complement signed
// integer and widens it to int64. This mirrors the helper of the same name in
// the parent colbin package; it is duplicated so that varint stays independent
// of its importer.
func signExtend(v uint64, width uint8) int64 {
	if width < 64 && v&(uint64(1)<<(width-1)) != 0 {
		v |= ^((uint64(1) << width) - 1)
	}
	return int64(v)
}

// enc and dec apply the header's zigzag bit. They are plain functions rather
// than closures so the residual loops stay allocation-free.
func enc(x int64, zz bool) uint64 {
	if zz {
		return zigzag(x)
	}
	return uint64(x)
}

func dec(u uint64, zz bool) int64 {
	if zz {
		return unzigzag(u)
	}
	return int64(u)
}

type kmParams struct {
	k, code, m uint8
	size       int // payload bytes, excluding the header
}

// search picks the (k, d-code) pair minimising total payload size for the
// residual bit-length histogram, rejecting pairs that cannot represent the
// widest residual. Returns false if no pair fits (unreachable for 64-bit input,
// since k=1 with code 7 gives cap(9) = 64).
func search(hist *[65]int32) (kmParams, bool) {
	// encLen is monotonic in the residual bit length. A cumulative histogram
	// lets each candidate score the ranges assigned to its k..M byte lengths,
	// instead of calling encLen for all 65 buckets and re-walking k..M each time.
	var cumulative [65]int32
	var totalCount int32
	for b, n := range hist {
		totalCount += n
		cumulative[b] = totalCount
	}

	var best kmParams
	found := false
	for k := minK; k <= maxK; k++ {
		for code := range uint8(8) {
			m := k + dCodes[code]
			maxBits := int(capBits(m, k, m))
			if maxBits < 64 && cumulative[maxBits] != totalCount {
				continue
			}

			size, previousCap := 0, -1
			for l := k; l <= m; l++ {
				hi := min(int(capBits(l, k, m)), 64)
				n := cumulative[hi]
				if previousCap >= 0 {
					n -= cumulative[previousCap]
				}
				size += int(l) * int(n)
				previousCap = hi
				if hi == 64 {
					break
				}
			}
			if !found || size < best.size {
				best, found = kmParams{k: k, code: code, m: m, size: size}, true
			}
		}
	}
	return best, found
}

// residualCount is how many residuals a transform emits for n input values.
// trFOR emits one extra, the frame-of-reference base at index 0.
func residualCount(transform uint8, n int) int {
	if transform == trFOR {
		return n + 1
	}
	return n
}

// residual returns residual i for the given transform. base is the column
// minimum and is used only by trFOR, whose deltas are non-negative by
// construction and so are never zigzagged regardless of zz; its base at index 0
// is always zigzagged, since it alone may be negative.
func residual[T Signed](vals []T, i int, transform uint8, zz bool, base int64) uint64 {
	switch transform {
	case trFOR:
		if i == 0 {
			return zigzag(base)
		}
		return uint64(int64(vals[i-1])) - uint64(base) // wraps correctly for spans > 2^63
	case trDelta:
		if i == 0 {
			return enc(int64(vals[0]), zz)
		}
		return enc(int64(vals[i])-int64(vals[i-1]), zz)
	default:
		return enc(int64(vals[i]), zz)
	}
}

// subOverflows reports whether a-b overflows int64.
func subOverflows(a, b int64) bool {
	d := a - b
	return (a^b)&(a^d) < 0
}

// plan is a fully evaluated encoding candidate.
type plan struct {
	transform uint8
	zz        bool
	base      int64
	params    kmParams
	total     int // header + payload
}

// evaluate scores one transform by histogramming its residual bit lengths.
// Returns false when the transform is unusable: trDelta is rejected if any
// consecutive difference overflows int64, which needs a span above 2^63 and so
// is only possible for int64 input.
func evaluate[T Signed](vals []T, transform uint8, zz bool, base int64) (plan, bool) {
	var hist [65]int32
	cnt := residualCount(transform, len(vals))
	if transform == trDelta && widthOf[T]() == 8 {
		for i := 1; i < len(vals); i++ {
			if subOverflows(int64(vals[i]), int64(vals[i-1])) {
				return plan{}, false
			}
		}
	}
	for i := range cnt {
		hist[bits.Len64(residual(vals, i, transform, zz, base))]++
	}
	p, ok := search(&hist)
	if !ok {
		return plan{}, false
	}
	return plan{transform: transform, zz: zz, base: base, params: p, total: 1 + p.size}, true
}

// AppendArray encodes vals onto out and returns out. The element width comes
// from T and is not stored, so DecodeArray must be instantiated with the same
// type.
func AppendArray[T Signed](out []byte, vals []T) []byte {
	n := len(vals)
	if n == 0 {
		return append(out, trRaw) // k=1, code=0, no payload
	}
	width := widthOf[T]()

	// Candidate 1: raw values, zigzagged only if the column has negatives.
	hasNeg := false
	minVal := int64(vals[0])
	for _, v := range vals {
		x := int64(v)
		if x < 0 {
			hasNeg = true
		}
		if x < minVal {
			minVal = x
		}
	}
	best, found := evaluate(vals, trRaw, hasNeg, 0)

	// Candidate 2: delta of previous. Zigzag whenever any step goes backwards.
	negStep := false
	for i := 1; i < n; i++ {
		a, b := int64(vals[i]), int64(vals[i-1])
		if !subOverflows(a, b) && a < b {
			negStep = true
			break
		}
	}
	if p, ok := evaluate(vals, trDelta, negStep, 0); ok && (!found || p.total < best.total) {
		best, found = p, true
	}

	// Candidate 3: frame of reference against the column minimum.
	if p, ok := evaluate(vals, trFOR, false, minVal); ok && (!found || p.total < best.total) {
		best, found = p, true
	}

	// Candidate 4: uncompressed native words. Wins on full-width random data,
	// where no varint scheme can beat the element width. Always available, since
	// every value fits its own type; this is what bounds output at 1 + width*n.
	if total := 1 + n*int(width); !found || total < best.total {
		best = plan{transform: trFixed, params: kmParams{k: 1, code: 0}, total: total}
	}

	var zzBit uint8
	if best.zz {
		zzBit = 1 << 2
	}
	out = append(out, best.transform|zzBit|(best.params.k-1)<<3|best.params.code<<5)
	if best.transform == trFixed {
		for _, v := range vals {
			u := uint64(int64(v))
			for range width {
				out = append(out, byte(u))
				u >>= 8
			}
		}
		return out
	}
	cnt := residualCount(best.transform, n)
	for i := range cnt {
		out = appendKM(out, residual(vals, i, best.transform, best.zz, best.base), best.params.k, best.params.m)
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
	hdr := buf[0]
	pos := 1
	transform := hdr & 0x03
	zz := hdr&0x04 != 0
	k := (hdr>>3)&0x03 + 1
	m := k + dCodes[hdr>>5]
	if n == 0 {
		return pos, nil
	}

	if transform == trFixed {
		width := widthOf[T]()
		if len(buf)-pos < n*int(width) {
			return 0, ErrTruncated
		}
		bitWidth := width * 8
		for i := range n {
			var u uint64
			for b := range width {
				u |= uint64(buf[pos]) << (8 * b)
				pos++
			}
			out[i] = T(signExtend(u, bitWidth))
		}
		return pos, nil
	}

	switch transform {
	case trFOR:
		raw, p, err := getKM(buf, pos, k, m)
		if err != nil {
			return 0, err
		}
		pos = p
		base := unzigzag(raw)
		for i := range n {
			d, p, err := getKM(buf, pos, k, m)
			if err != nil {
				return 0, err
			}
			pos = p
			out[i] = T(int64(uint64(base) + d)) // wraps to match the encoder
		}
	case trDelta:
		var prev int64
		for i := range n {
			u, p, err := getKM(buf, pos, k, m)
			if err != nil {
				return 0, err
			}
			pos = p
			if i == 0 {
				prev = dec(u, zz)
			} else {
				prev += dec(u, zz)
			}
			out[i] = T(prev)
		}
	default:
		for i := range n {
			u, p, err := getKM(buf, pos, k, m)
			if err != nil {
				return 0, err
			}
			pos = p
			out[i] = T(dec(u, zz))
		}
	}
	return pos, nil
}
