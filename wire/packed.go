package wire

// The opt-in string encoding.
//
// A BLOB's descriptor says how its bytes are encoded, and packed5 owns four of
// its codes: about five bits per character on alphanumerics, against eight for
// a raw blob. It is a per-field code rather than a mode, so a message can hold
// both and a reader never has to be told which to expect — the field says.
//
// It is not the default. A raw blob is a sub-slice of the message on the way out
// and a memcpy on the way in; a packed one is a pass over every character on
// both sides. The trade is a third of a short token against that pass, which is
// worth taking on a wire that is size-bound and not on one that is not.
//
// # The field carries the frame
//
// packed5 writes a bare unit stream: the container says how long it is and
// which case mode it opens in. Under eight-bit keys that is the BLOB
// descriptor's detail:
//
//	0 0 lw:2      raw bytes, behind a length of 1, 2, 4 or 8 bytes
//	0 1 up lw:1   packed5, opening in lower or upper case, behind a length of
//	              1 or 2 bytes
//	1 size:3      raw bytes, size+1 of them, with no length
//
// and under four-bit keys it is the flags of the field's length form: `00` raw,
// `01` packed5 opening in lowercase, `10` opening in uppercase, `11` refused.
//
// The last row is the narrow nibble's sized string, at eight key bits: a code,
// a name, a version of up to eight bytes costs the key and the descriptor and
// nothing else, which is protobuf's tag and length. It used to cost a length
// byte on top, a byte per field that every type of more than sixteen fields
// paid on most of its strings. Its codes were a dictionary encoding, reserved
// and never written, and the packed lengths past 65 535 bytes, which no string
// this encoding is for reaches.

import (
	"unsafe"

	"github.com/ivanjoz/colbin/internal/wirefmt"
	"github.com/ivanjoz/colbin/packed5"
)

// The BLOB descriptor's detail bits.
const (
	blobPacked uint8 = 0b0100
	blobUpper  uint8 = 0b0010
	blobShort  uint8 = 0b1000
	// blobForm masks the bits that say which of the three a blob is: raw, packed
	// or short. A short blob is raw bytes, so only packed needs telling apart.
	blobForm uint8 = blobShort | blobPacked
	// maxShortBlob is the longest blob with no length.
	maxShortBlob = 8
)

// appendBlobHeader writes a blob's descriptor and length, the short form for
// one of up to maxShortBlob bytes. The short form is inline and the rest a
// call, so that a string field's write stays inside the inliner's budget.
func appendBlobHeader(buffer []byte, size int) []byte {
	if uint(size-1) < maxShortBlob {
		return append(buffer, descriptor(classBlob, blobShort|uint8(size-1)))
	}
	return appendBlobLength(buffer, size)
}

// appendBlobLength is appendBlobHeader past maxShortBlob bytes, or at none.
//
//go:noinline
func appendBlobLength(buffer []byte, size int) []byte {
	code, width := lengthCodeFor(uint64(size))
	return wirefmt.AppendMagnitude(append(buffer, descriptor(classBlob, code)), uint64(size), width)
}

// rawBlobSize is the whole field Writer8.String writes for size bytes: the
// key, the descriptor, the length when there is one, and the bytes.
func rawBlobSize(size int) int {
	if size <= maxShortBlob {
		return 2 + size
	}
	_, width := lengthCodeFor(uint64(size))
	return 2 + width + size
}

// isPacked reports whether a BLOB descriptor is a packed5 one.
func isPacked(desc uint8) bool { return desc&blobForm == blobPacked }

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
	code, width := lengthCodeFor(uint64(size))
	// The raw field is the comparison, since a raw string of up to eight bytes
	// carries no length and a packed one always does.
	if !ok || size > 0xFFFF || 2+width+size >= rawBlobSize(len(value)) {
		w.Buffer = buf[:start]
		w.String(key, value)
		return
	}
	w.Buffer = buf

	enc := blobPacked
	if upper {
		enc |= blobUpper
	}
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

// String reads a string written by String or by PackedString: the descriptor's
// enc code says which, so a reader needs no configuration and cannot be wrong
// about it.
func (r *Reader8) String() string { return r.StringIn(nil) }

// StringIn is String with the copy made in a, or in an allocation of its own
// for a nil a. See Reader.StringIn.
func (r *Reader8) StringIn(a *Arena) string {
	if r.at+2 > len(r.buffer) {
		r.fail(ErrTruncated)
		return ""
	}
	desc := r.buffer[r.at+1]
	size, start, ok := r.lengthOf(classBlob)
	if !ok {
		return ""
	}
	if size > len(r.buffer)-start {
		r.fail(ErrTruncated)
		return ""
	}
	r.at = start + size
	if !isPacked(desc) {
		if a == nil {
			return string(r.buffer[start : start+size])
		}
		return a.String(r.buffer[start : start+size])
	}
	// packed5 takes a whole word per group, so it is handed the rest of the
	// message and reads the slack that follows the payload for free.
	out, err := packed5.AppendString(nil, r.buffer[start:], size, desc&blobUpper != 0)
	if err != nil {
		r.fail(err)
		return ""
	}
	return unsafe.String(unsafe.SliceData(out), len(out))
}

// PackedString writes a string in the packed5 encoding when that makes the field
// smaller, and raw when it does not. Nothing is written for an empty string.
//
// The narrow header has no enc field, so the encoding rides in the length form's
// flags: `01` for a stream opening in lower case, `10` for upper. A packed string
// always carries a length, where a raw one of up to eight bytes carries none, so
// the comparison below is of whole fields rather than of payloads.
func (w *Writer) PackedString(key uint8, value string) {
	if len(value) == 0 {
		return
	}
	// The payload's length is not known until it is written, so one length byte
	// is reserved and patched after, exactly as a composite's is. A payload past
	// 253 bytes shifts up to make room, which is a memmove on a value already
	// large enough to have earned one.
	start := len(w.Buffer)
	w.Buffer = append(w.Buffer, key<<4|nibbleLength|flagPackedLower, 0)
	buf, size, upper, ok := packed5.AppendPayload(w.Buffer, value)
	if !ok || 1+lengthBytes(size)+size >= rawStringSize(len(value)) {
		w.Buffer = buf[:start]
		w.String(key, value)
		return
	}
	w.Buffer = buf
	if upper {
		w.Buffer[start] = key<<4 | nibbleLength | flagPackedUpper
	}
	w.Close(Mark{at: start + 1})
}

// rawStringSize is the whole field String writes for a string of size bytes.
func rawStringSize(size int) int {
	if size <= maxSized {
		return 1 + size
	}
	return 1 + lengthBytes(size) + size
}

// stringWide is String for every field its inline raw paths do not take: the
// packed forms, the wider lengths, and every way a field can be malformed.
func (r *Reader) stringWide() string {
	nibble, start, size, ok := r.field()
	switch {
	case !ok:
		return ""
	case nibble < nibbleSized || nibble == nibbleLength|flagBits:
		r.fail(ErrBadEscape)
		return ""
	case nibble <= nibbleLength|flagRaw:
		return string(r.buffer[start : start+size])
	}
	// packed5 takes a whole word per group, so it is handed the rest of the
	// message and reads the slack that follows the payload for free.
	out, err := packed5.AppendString(nil, r.buffer[start:], size, nibble&flagBits == flagPackedUpper)
	if err != nil {
		r.fail(err)
		return ""
	}
	return string(out)
}
