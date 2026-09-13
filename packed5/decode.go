package packed5

import (
	"encoding/binary"
	"strconv"
)

// Decode reads one frame from the front of buf and returns the string it holds
// together with the number of bytes consumed, so frames can be read back to
// back from a larger buffer.
//
// The returned string is a copy; it does not alias buf.
func Decode(buf []byte) (string, int, error) {
	h, err := frame(buf)
	if err != nil {
		return "", 0, err
	}
	if !h.packed {
		return string(h.payload), h.n, nil
	}
	// Size the buffer from the format's own expansion bound so appendStream
	// never has to grow it, leaving the final string conversion as the only
	// allocation. The scratch stays on the stack for short results: appendStream
	// derives its result only from the buffer handed in, and that result dies
	// here at the string copy, so escape analysis keeps the array in this frame.
	var stack [decodeScratchBytes]byte
	scratch := stack[:0]
	if need := maxDecodedLen(len(h.payload)); need > len(stack) {
		scratch = make([]byte, 0, need)
	}
	out, err := appendStream(scratch, h, h.upper)
	if err != nil {
		return "", 0, err
	}
	return string(out), h.n, nil
}

// AppendDecoded decodes one frame from the front of buf, appending the decoded
// bytes onto dst, and returns the extended dst with the frame length.
//
// It exists so a caller decoding a run of frames can gather them into a single
// backing array and slice the strings out of it, rather than pay one allocation
// per frame. Strings produced that way stay alive as long as any one of them
// does, which suits a column of values that is retained as a unit.
func AppendDecoded(dst, buf []byte) ([]byte, int, error) {
	h, err := frame(buf)
	if err != nil {
		return dst, 0, err
	}
	if !h.packed {
		return append(dst, h.payload...), h.n, nil
	}
	out, err := appendStream(dst, h, h.upper)
	if err != nil {
		return dst, 0, err
	}
	return out, h.n, nil
}

// AppendString appends the string held by a bare payload — a unit stream with no
// frame header, as AppendPayload writes it — onto dst.
//
// src must begin at the payload and may run past it; whatever follows is free
// slack for the group reader, so a caller with the rest of its buffer in hand
// should pass it. size is the payload's byte length, which the container the
// payload is embedded in is already carrying.
func AppendString(dst, src []byte, size int, upper bool) ([]byte, error) {
	if size > len(src) {
		return dst, ErrTruncated
	}
	return appendStream(dst, header{payload: src[:size], wide: src}, upper)
}

// header is one parsed frame prefix: the payload it delimits, the total frame
// length, and the flags that say how to read it.
type header struct {
	payload []byte
	// wide is the payload extended to the end of the caller's buffer. The group
	// reader takes a whole word per group, so it wants everything there is:
	// in a run of back-to-back frames the next frame supplies the overshoot, and
	// only the last one falls to the bounded tail path.
	wide          []byte
	n             int
	packed, upper bool
}

// frame splits off the header byte and length prefix.
func frame(buf []byte) (header, error) {
	if len(buf) == 0 {
		return header{}, ErrTruncated
	}
	hdr := buf[0]
	if hdr&flagReserved != 0 {
		return header{}, ErrBadHeader
	}
	pos := 1
	length := int(hdr >> lenShift)
	if length == lenEscape {
		v, k, err := readUvarint(buf[pos:])
		if err != nil {
			return header{}, err
		}
		// The escape must not re-encode a length the header could have held;
		// one length has one encoding, so a frame has one byte representation.
		if v <= lenInline {
			return header{}, ErrBadLength
		}
		length, pos = v, pos+k
	}
	if length > len(buf)-pos {
		return header{}, ErrTruncated
	}
	return header{
		payload: buf[pos : pos+length],
		wide:    buf[pos:],
		n:       pos + length,
		packed:  hdr&flagPacked5 != 0,
		upper:   hdr&flagUppercase != 0,
	}, nil
}

// maxDecodedLen bounds how many bytes a packed payload can decode to. The
// densest token in the format is the three-byte '€', three bytes per two units
// and so per ten bits, which is 2.4x. A number is next at four bytes per three
// units, and every other token is at or below one byte per unit.
func maxDecodedLen(payload int) int { return payload*12/5 + 4 }

// decodeScratchBytes is the starting size of Decode's scratch. It covers
// payloads up to 105 bytes, comfortably past the short strings Packed-5 targets,
// so the common case never allocates a second time.
const decodeScratchBytes = 256

// windowUnits is how many units the reader keeps in hand. A token spans at most
// eleven — a four-byte escape — so the decode loop can read a whole token
// without a bounds test as long as that many are buffered, and the window is
// refilled a group at a time above it.
const windowUnits = 32

const maxTokenUnits = 3 + 2*maxEscapeRun

// appendStream walks the packed unit stream, appending the decoded bytes to dst.
//
// Units arrive eight at a time through a small window rather than one at a time
// through a bit reader: one unaligned load and eight constant shifts per group,
// against a load, a shift and a mask per field. The window is a stack array of
// thirty-two bytes, small enough that clearing it costs nothing measurable,
// which is why the units are not simply materialised into a buffer the size of
// the payload.
//
// A pending simple toggle is cleared only by a letter, and a long toggle leaves
// it alone. The encoder emits CASE_TOGGLE_SIMPLE immediately before the letter
// it applies to, except for the one that pads the stream to the unit grid, which
// has no letter after it and so decodes to nothing — that is what makes it a
// legal pad.
func appendStream(dst []byte, h header, upper bool) ([]byte, error) {
	n := payloadUnits(len(h.payload))
	src := h.wide

	// The group loader takes a whole word at a time, so it reads past the last
	// group of a payload. In a run of back-to-back frames the next frame covers
	// that; for the last one, the final bytes are copied once into a buffer with
	// room to spare and the loader switches to it at tailAt. Doing this here
	// rather than per group is worth a few nanoseconds on a short string, which
	// is most of them.
	var tail [16]byte
	tailAt := len(h.payload)
	if len(src) < len(h.payload)+8 {
		tailAt = max(0, len(h.payload)-8)
		copy(tail[:], h.payload[tailAt:])
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
