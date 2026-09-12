package wire

// The opt-in string encoding.
//
// A BLOB's descriptor carries a two-bit `enc` field, and code 1 is packed5:
// about five bits per character on upper-case alphanumerics, against eight for
// a raw blob. It is a per-field code rather than a mode, so a message can hold
// both and a reader never has to be told which to expect — the field says.
//
// It is not the default. A raw blob is a sub-slice of the message on the way out
// and a memcpy on the way in; a packed one is a pass over every character on
// both sides. The trade is a third of a short token against that pass, which is
// worth taking on a wire that is size-bound and not on one that is not.

import "github.com/ivanjoz/colbin/packed5"

// encPacked5 is the BLOB descriptor's enc code for a packed string, in bits 3-2.
const encPacked5 uint8 = 1 << 2

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
	// packed5.Size is exact, so the header can be written before the payload and
	// there is nothing to patch or shift afterwards.
	size := packed5.Size(value)
	if size >= len(value) {
		w.String(key, value)
		return
	}
	code, width := lengthCodeFor(uint64(size))
	w.Buffer = appendMagnitude(
		append(w.Buffer, key, descriptor(classBlob, encPacked5|code)), uint64(size), width)
	w.Buffer = packed5.Append(w.Buffer, value)
}

// PackedString reads a string written by PackedString or by String: the
// descriptor's enc code says which, so a reader needs no configuration and
// cannot be wrong about it.
func (r *Reader8) PackedString() string {
	if r.at+2 > len(r.buffer) {
		r.fail(ErrTruncated)
		return ""
	}
	packed := r.buffer[r.at+1]&encPacked5 != 0
	raw := r.Bytes()
	if !packed {
		return string(raw)
	}
	value, _, err := packed5.Decode(raw)
	if err != nil {
		r.fail(err)
		return ""
	}
	return value
}
