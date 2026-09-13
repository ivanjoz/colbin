package wire

// The opt-in string encoding.
//
// A BLOB's descriptor carries a two-bit `enc` field, and packed5 owns two of its
// codes: about five bits per character on alphanumerics, against eight for a raw
// blob. It is a per-field code rather than a mode, so a message can hold both
// and a reader never has to be told which to expect — the field says.
//
// It is not the default. A raw blob is a sub-slice of the message on the way out
// and a memcpy on the way in; a packed one is a pass over every character on
// both sides. The trade is a third of a short token against that pass, which is
// worth taking on a wire that is size-bound and not on one that is not.
//
// # No frame header
//
// packed5 can frame itself: byte 0 of a frame says whether the payload is packed
// and which case mode it opens in, and carries the payload length. Embedded in a
// BLOB, all three are duplicates — the descriptor already says the encoding and
// the size — so this writes the bare unit stream and spends the descriptor's
// spare `enc` code on the one bit that is genuinely new:
//
//	enc 0   raw bytes
//	enc 1   packed5, stream opens in lowercase
//	enc 2   dictionary reference (reserved here)
//	enc 3   packed5, stream opens in uppercase
//
// That is one byte off every packed string field, which on a column of names is
// about 8% — more than the packing kernel itself is worth, and it costs nothing
// to encode because the byte it removes was one this writer had to patch.

import (
	"encoding/binary"

	"github.com/ivanjoz/colbin/packed5"
)

// The BLOB descriptor's enc codes, in bits 3-2.
const (
	encRaw        uint8 = 0 << 2
	encPacked5    uint8 = 1 << 2
	encDictionary uint8 = 2 << 2 // not written here; reserved for a column dictionary
	encPacked5Up  uint8 = 3 << 2

	encMask uint8 = 3 << 2
)

// PackedString writes a string in the packed5 encoding when that is smaller than
// the raw bytes, and raw when it is not. Nothing is written for an empty string.
//
// Choosing per string rather than per message is what keeps the encoding from
// ever costing anything: a token that does not pack is written raw, and the
// descriptor says so.
func (w *Writer8) PackedString(key uint8, value string) {
	if len(value) == 0 {
		return
	}
	// The payload's length is not known until it is written, so the header is
	// reserved and filled in after. One byte covers every size up to 255, which
	// is every string this encoding is for; a longer one shifts the payload up
	// to make room, which costs a memmove on a value that is already large.
	start := len(w.Buffer)
	w.Buffer = append(w.Buffer, key, 0, 0)
	buf, size, upper, ok := packed5.AppendPayload(w.Buffer, value)
	if !ok {
		w.Buffer = buf[:start]
		w.String(key, value)
		return
	}
	w.Buffer = buf

	enc := encPacked5
	if upper {
		enc = encPacked5Up
	}
	code, width := lengthCodeFor(uint64(size))
	w.Buffer[start+1] = descriptor(classBlob, enc|code)
	if width == 1 {
		w.Buffer[start+2] = uint8(size)
		return
	}
	for range width - 1 {
		w.Buffer = append(w.Buffer, 0)
	}
	copy(w.Buffer[start+2+width:], w.Buffer[start+3:start+3+size])
	// The size goes in by hand rather than through appendMagnitude, which lays
	// down a whole uint64 and trims the length afterwards — here that would
	// write six zero bytes over the payload that was just shifted up.
	for i := range width {
		w.Buffer[start+2+i] = byte(size >> (8 * i))
	}
}

// PackedString reads a string written by PackedString or by String: the
// descriptor's enc code says which, so a reader needs no configuration and
// cannot be wrong about it.
func (r *Reader8) PackedString() string {
	if r.at+2 > len(r.buffer) {
		r.fail(ErrTruncated)
		return ""
	}
	enc := r.buffer[r.at+1] & encMask
	raw := r.Bytes()
	if enc == encRaw {
		return string(raw)
	}
	if enc == encDictionary {
		r.fail(ErrBadDescriptor)
		return ""
	}
	// Bytes leaves the cursor just past the payload, so the payload began at
	// r.at-len(raw) and everything from there to the end of the message is
	// readable. packed5 takes a whole word per group and wants that slack; given
	// only the payload it falls back to a bounded copy of the last few bytes,
	// which is correct but slower.
	out, err := packed5.AppendString(nil, r.buffer[r.at-len(raw):], len(raw), enc == encPacked5Up)
	if err != nil {
		r.fail(err)
		return ""
	}
	return string(out)
}

// PackedString writes a string in the packed5 encoding when that is smaller than
// the raw bytes, and raw when it is not. Nothing is written for an empty string.
//
// The narrow header has no enc field, so the encoding rides in the blob header's
// escape code — see escapePacked1Lo. That keeps a packed string's header at two
// bytes, the same as a raw one's, and is what lets packed5 run under four-bit
// keys at all: before this it forced a message to eight-bit keys, which cost a
// byte on every field rather than on the strings.
func (w *Writer) PackedString(key uint8, value string) {
	if len(value) == 0 {
		return
	}
	// The payload's length is not known until it is written, so two header bytes
	// are reserved and filled in after. A payload past 255 bytes needs three more,
	// which shifts it up; that is a memmove on a value already large enough to
	// have earned one.
	start := len(w.Buffer)
	w.Buffer = append(w.Buffer, key<<4|moreSizeFlag, 0)
	buf, size, upper, ok := packed5.AppendPayload(w.Buffer, value)
	if !ok {
		w.Buffer = buf[:start]
		w.String(key, value)
		return
	}
	w.Buffer = buf

	if size <= 0xFF {
		code := uint8(escapePacked1Lo)
		if upper {
			code = escapePacked1Up
		}
		w.Buffer[start] = key<<4 | moreSizeFlag | code
		w.Buffer[start+1] = uint8(size)
		return
	}
	code := uint8(escapePacked4Lo)
	if upper {
		code = escapePacked4Up
	}
	w.Buffer = append(w.Buffer, 0, 0, 0)
	copy(w.Buffer[start+5:], w.Buffer[start+2:start+2+size])
	w.Buffer[start] = key<<4 | moreSizeFlag | code
	binary.LittleEndian.PutUint32(w.Buffer[start+1:], uint32(size))
}

// PackedString reads a string written by PackedString or by String. The header's
// escape code says which, so a reader needs no configuration and cannot be wrong
// about it.
//
// The overwhelmingly common field is a raw blob with an inline size — the shape
// String reads — so that path is spelled out here, small enough for the inliner,
// and everything else is one call away. Delegating the whole thing to a single
// larger function measured 9% on a narrow record decode, which is a cost every
// message would pay for an encoding most of them do not use.
func (r *Reader) PackedString() string {
	at := r.at
	if at+2 <= len(r.buffer) && r.buffer[at]&moreSizeFlag == 0 {
		size := int(r.buffer[at]&0b111)<<8 | int(r.buffer[at+1])
		if size <= len(r.buffer)-at-2 {
			r.at = at + 2 + size
			return string(r.buffer[at+2 : at+2+size])
		}
	}
	return r.packedStringWide()
}

// packedStringWide handles every header the inline raw path above does not: the
// packed escapes, the wide raw sizes, and every way a header can be malformed.
func (r *Reader) packedStringWide() string {
	header, ok := r.header()
	if !ok {
		return ""
	}
	var size, start int
	packed, upper := false, false
	if header&moreSizeFlag == 0 {
		if r.at+2 > len(r.buffer) {
			r.fail(ErrTruncated)
			return ""
		}
		size, start = int(header&0b111)<<8|int(r.buffer[r.at+1]), r.at+2
	} else {
		code := header & 0b111
		switch code {
		case escapePacked1Lo, escapePacked1Up:
			if r.at+2 > len(r.buffer) {
				r.fail(ErrTruncated)
				return ""
			}
			size, start, packed = int(r.buffer[r.at+1]), r.at+2, true
			upper = code == escapePacked1Up
		case escapePacked4Lo, escapePacked4Up:
			if r.at+5 > len(r.buffer) {
				r.fail(ErrTruncated)
				return ""
			}
			size, start, packed = int(binary.LittleEndian.Uint32(r.buffer[r.at+1:])), r.at+5, true
			upper = code == escapePacked4Up
			if size <= 0xFF { // one size has one encoding
				r.fail(ErrBadEscape)
				return ""
			}
		default:
			if size, start, ok = r.escapedSize(code); !ok {
				return ""
			}
		}
	}
	if size > len(r.buffer)-start {
		r.fail(ErrTruncated)
		return ""
	}
	r.at = start + size
	if !packed {
		return string(r.buffer[start : start+size])
	}
	// packed5 takes a whole word per group, so it is handed the rest of the
	// message and reads the slack that follows the payload for free.
	out, err := packed5.AppendString(nil, r.buffer[start:], size, upper)
	if err != nil {
		r.fail(err)
		return ""
	}
	return string(out)
}
