package packed5

import (
	"encoding/binary"
	"strconv"
)

// AppendString decodes a payload written by AppendPayload, appending the string
// it holds onto dst. size and upper are the payload's byte length and case mode,
// which the container the payload is embedded in carries.
//
// src must begin at the payload and may run past it. Whatever follows is read as
// slack by the group reader and never decoded, so a caller with the rest of its
// buffer in hand should pass it; without slack the last group is copied once
// into a small buffer instead.
//
// Any src, size and upper either decode to some string or return an error. The
// output is at most 2.4 bytes per payload byte, plus four.
func AppendString(dst, src []byte, size int, upper bool) ([]byte, error) {
	if size < 0 {
		return dst, ErrBadLength
	}
	if size > len(src) {
		return dst, ErrTruncated
	}
	return appendStream(dst, src, size, upper)
}

// windowUnits is how many units the reader keeps in hand. A token spans at most
// eleven — a four-byte escape — so the decode loop can read a whole token
// without a bounds test as long as that many are buffered, and the window is
// refilled a group at a time above it.
const windowUnits = 32

const maxTokenUnits = 3 + 2*maxEscapeRun

// appendStream walks the unit stream in src[:size], appending the decoded bytes
// to dst. Bytes of src past size are slack for the group loader and nothing more.
//
// Units arrive eight at a time through a small window on the stack: one
// unaligned load and eight constant shifts per group, against a load, a shift
// and a mask per field for a bit reader.
//
// A pending simple toggle is cleared only by a letter, and a long toggle leaves
// it alone. The encoder emits CASE_TOGGLE_SIMPLE immediately before the letter
// it applies to, except for the one that pads the stream to the unit grid, which
// has no letter after it and so decodes to nothing — that is what makes it a
// legal pad.
func appendStream(dst, src []byte, size int, upper bool) ([]byte, error) {
	n := payloadUnits(size)

	// The group loader takes a whole word at a time, so it reads past the last
	// group of a payload. When src has no eight bytes of slack, the final bytes
	// are copied once into a buffer with room to spare and the loader switches to
	// it at tailAt.
	var tail [16]byte
	tailAt := size
	if len(src) < size+8 {
		tailAt = max(0, size-8)
		copy(tail[:], src[tailAt:size])
	}

	var win [windowUnits + 8]uint8
	pos, have, got, p := 0, 0, 0, 0

	out := dst
	cur, pending := upper, false

	for {
		if have-pos < maxTokenUnits && got < n {
			// Compact what is left of the window down, then refill from src a
			// group at a time.
			have = copy(win[:], win[pos:have])
			pos = 0
			for have+8 <= windowUnits+8 && got < n {
				var w uint64
				if p < tailAt {
					w = binary.LittleEndian.Uint64(src[p:])
				} else {
					w = binary.LittleEndian.Uint64(tail[p-tailAt:])
				}
				win[have+0] = uint8(w) & 31
				win[have+1] = uint8(w>>5) & 31
				win[have+2] = uint8(w>>10) & 31
				win[have+3] = uint8(w>>15) & 31
				win[have+4] = uint8(w>>20) & 31
				win[have+5] = uint8(w>>25) & 31
				win[have+6] = uint8(w>>30) & 31
				win[have+7] = uint8(w>>35) & 31
				have += 8
				got += 8
				p += 5
			}
			if got > n { // the final group overshoots; those units are not real
				have -= got - n
				got = n
			}
		}
		if pos >= have {
			break
		}

		op := win[pos]
		pos++
		switch {
		case op < opSpace:
			if cur != pending { // uppercase exactly when the two disagree
				out = append(out, 'A'+op)
			} else {
				out = append(out, 'a'+op)
			}
			pending = false
		case op == opSpace:
			out = append(out, ' ')
		case op == opCaseSimple:
			pending = true
		case op == opCaseLong:
			cur = !cur
		case op == opSymbol:
			if pos >= have {
				return dst, ErrTruncated
			}
			out = append(out, symTable[win[pos]])
			pos++
		case op == opExt:
			if pos >= have {
				return dst, ErrTruncated
			}
			idx := win[pos]
			pos++
			if idx != extEscape {
				if idx >= extReserved {
					return dst, ErrReservedSymbol
				}
				out = append(out, extTable[idx]...)
				break
			}
			if pos >= have {
				return dst, ErrTruncated
			}
			count := int(win[pos]) + 1
			pos++
			if count > maxEscapeRun {
				return dst, ErrBadEscape
			}
			if pos+2*count > have {
				return dst, ErrTruncated
			}
			for range count {
				lo, hi := win[pos], win[pos+1]
				if hi > 7 { // a byte has eight bits; five and three
					return dst, ErrBadEscape
				}
				out = append(out, lo|hi<<5)
				pos += 2
			}
		default: // opNumber
			if pos+2 > have {
				return dst, ErrTruncated
			}
			v := uint64(win[pos]) | uint64(win[pos+1])<<5
			pos += 2
			out = strconv.AppendUint(out, v, 10)
		}
	}
	return out, nil
}
