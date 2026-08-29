package compact

import "math/bits"

// The compact varint. Unit 0 is one bit narrower than LEB128's byte, and that
// bit becomes a selector saying whether a four-bit nibble follows it:
//
//	unit 0    [selector:1] [cont:1] [payload:6]   (+ [payload:4] if selector)
//	unit i    [cont:1] [payload:7]
//
// cont on a unit says another unit follows. The nibble is a one-time offset on
// unit 0, so the two capacity ladders are 6+7k and 10+7k for k continuation
// units, at sizes 8+8k and 12+8k bits. The encoder takes whichever ladder holds
// the value in fewer bits.

const (
	unit0Bits   = 6 // payload bits in unit 0 without the nibble
	nibbleBits  = 4 // the selector's one-time offset
	unitBits    = 7 // payload bits per continuation unit
	unit0Nibble = unit0Bits + nibbleBits
)

// varintSize is the encoded bit length of a payload needing b significant bits,
// together with the selector that achieves it. Both ladders are walked because
// neither dominates: the nibble wins on b in 7..10, 14..17, 21..24 and so on,
// and loses by four bits exactly where LEB128's first unit was already full.
func varintSize(b int) (size int, selector bool) {
	plain := 8
	for cap := unit0Bits; b > cap; cap += unitBits {
		plain += 8
	}
	nib := 12
	for cap := unit0Nibble; b > cap; cap += unitBits {
		nib += 8
	}
	if nib < plain {
		return nib, true
	}
	return plain, false
}

// varintSelector[b] is the ladder varintSize picks for a payload of b bits.
// The choice depends on nothing but b, and b has 65 possible values, so it is a
// table rather than two loops walked on every integer written.
var varintSelector = func() (t [65]bool) {
	for b := range t {
		_, t[b] = varintSize(b)
	}
	return
}()

// putVarint writes v, choosing the smaller of the two forms.
//
// Each unit goes out as one call: unit 0 is its selector, its continuation bit
// and its payload assembled into a single 8- or 12-bit word, and each
// continuation unit is one byte-wide word. Writing the flags separately cost
// three trips through the accumulator for what is usually a one-unit value.
func (w *bitWriter) putVarint(v uint64) {
	b := bits.Len64(v)
	selector := varintSelector[b]

	shift := unit0Bits
	width := uint8(1 + 1 + unit0Bits)
	if selector {
		shift = unit0Nibble
		width += nibbleBits
	}
	more := b > shift

	// unit 0: [selector:1][cont:1][payload:6] (+[payload:4] if selector)
	unit := boolBit(selector) | boolBit(more)<<1 | (v&mask(unit0Bits))<<2
	if selector {
		unit |= ((v >> unit0Bits) & mask(nibbleBits)) << 8
	}
	w.chunk(unit, width)

	for more {
		more = b > shift+unitBits
		// unit i: [cont:1][payload:7]
		w.chunk(boolBit(more)|((v>>uint(shift))&mask(unitBits))<<1, 1+unitBits)
		shift += unitBits
	}
}

func boolBit(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

func mask(width uint) uint64 { return uint64(1)<<width - 1 }

// getVarint reverses putVarint. A shift of 64 or more yields zero in Go rather
// than being undefined, so a corrupt run of continuation units saturates instead
// of misbehaving; it can still run off the end, which the reader reports.
func (r *bitReader) getVarint() uint64 {
	// One read per unit, mirroring putVarint: the flags are unpacked from the
	// word rather than fetched as separate one-bit reads.
	u := r.get(1 + 1 + unit0Bits)
	selector, more := u&1 == 1, u&2 == 2
	v := u >> 2
	shift := uint(unit0Bits)
	if selector {
		v |= r.get(nibbleBits) << unit0Bits
		shift = unit0Nibble
	}
	for more && r.err == nil {
		u := r.get(1 + unitBits)
		more = u&1 == 1
		v |= (u >> 1) << shift
		shift += unitBits
	}
	return v
}

// zigzag maps a signed value onto an unsigned one that keeps small magnitudes
// small in both directions. Used when ALL_POSITIVE is clear.
func zigzag(v int64) uint64   { return uint64(v<<1) ^ uint64(v>>63) }
func unzigzag(u uint64) int64 { return int64(u>>1) ^ -int64(u&1) }
