package packed5

import "strconv"

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
	// Size the buffer from the format's own expansion bound so appendStream never
	// has to grow it, leaving the final string conversion as the only allocation.
	// The scratch stays on the stack for short results: appendStream derives its
	// result only from the buffer handed in, and that result dies here at the
	// string copy, so escape analysis keeps the array in this frame.
	var stack [decodeScratchBytes]byte
	scratch := stack[:0]
	if need := maxDecodedLen(len(h.payload)); need > len(stack) {
		scratch = make([]byte, 0, need)
	}
	out, err := appendStream(scratch, h.payload, h.upper, h.number)
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
	out, err := appendStream(dst, h.payload, h.upper, h.number)
	if err != nil {
		return dst, 0, err
	}
	return out, h.n, nil
}

// header is one parsed frame prefix: the payload it delimits, the total frame
// length, and the flags that say how to read it.
type header struct {
	payload               []byte
	n                     int
	packed, upper, number bool
}

// frame splits off the header byte and length prefix.
func frame(buf []byte) (header, error) {
	if len(buf) == 0 {
		return header{}, ErrTruncated
	}
	hdr := buf[0]
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
		n:       pos + length,
		packed:  hdr&flagPacked5 != 0,
		upper:   hdr&flagUppercase != 0,
		number:  hdr&flagNumber != 0,
	}, nil
}

// maxDecodedLen bounds how many bytes a packed payload can decode to. The
// densest token in the format is the three-byte '€' symbol, 3 bytes per 10
// bits, so a payload cannot expand by more than 2.4x. NUMBER_0_1023 is next at
// 4 bytes per 15 bits, and every other token is at or below one byte per 5
// bits.
func maxDecodedLen(payload int) int { return payload*12/5 + 4 }

// decodeScratchBytes is the starting size of Decode's pooled scratch. It covers
// payloads up to 105 bytes, comfortably past the short strings Packed-5 targets,
// so the pool settles without growing.
const decodeScratchBytes = 256

// appendStream walks the packed bitstream, appending the decoded bytes to dst.
// It does not reserve capacity: one caller decodes a single frame into a buffer
// it has already sized exactly, and the other is filling a column-wide arena
// whose geometric growth would be undone by reserving the worst case per frame. Its whole state is the payload
// position, the current case mode, and whether a simple toggle is pending.
//
// A pending simple toggle is cleared only by a letter, and a long toggle leaves
// it alone. The encoder emits CASE_TOGGLE_SIMPLE only immediately before the
// letter it applies to, so neither case arises in a frame this package wrote;
// they are defined so that a hand-built or corrupt stream still decodes
// deterministically rather than by accident.
func appendStream(dst, payload []byte, upper, number bool) ([]byte, error) {
	r := bitReader{buf: payload, limit: len(payload) * 8}
	pad, ok := r.read(padBitsWidth)
	if !ok {
		return dst, ErrTruncated
	}
	if int(pad) > r.remaining() {
		return dst, ErrBadPadding
	}
	r.limit -= int(pad)

	out := dst
	cur, pending := upper, false

	for r.remaining() >= 5 {
		op, _ := r.read(5)
		switch {
		case op < opSpace:
			c := byte(op)
			if cur != pending { // uppercase exactly when the two disagree
				out = append(out, 'A'+c)
			} else {
				out = append(out, 'a'+c)
			}
			pending = false
		case op == opSpace:
			out = append(out, ' ')
		case op == opCaseSimple:
			pending = true
		case op == opCaseLong:
			cur = !cur
		case op == opSymbol:
			idx, ok := r.read(5)
			if !ok {
				return dst, ErrTruncated
			}
			if idx >= symReserved {
				return dst, ErrReservedSymbol
			}
			out = append(out, symTable[idx]...)
		case op == opSimple:
			v, ok := r.read(4)
			if !ok {
				return dst, ErrTruncated
			}
			if v != escapeCode {
				out = append(out, simpleTable[v])
				break
			}
			cnt, ok := r.read(2)
			if !ok {
				return dst, ErrTruncated
			}
			for range int(cnt) + 1 {
				b, ok := r.read(8)
				if !ok {
					return dst, ErrTruncated
				}
				out = append(out, byte(b))
			}
		default: // opNumber
			if !number {
				out = append(out, '-')
				break
			}
			v, ok := r.read(10)
			if !ok {
				return dst, ErrTruncated
			}
			out = strconv.AppendUint(out, uint64(v), 10)
		}
	}
	// Fewer than 5 bits left is the stream's end. Anything left over is the
	// encoder's own rounding within a token boundary, which cannot happen:
	// padBits accounts for it exactly.
	if r.remaining() != 0 {
		return dst, ErrTruncated
	}
	return out, nil
}
