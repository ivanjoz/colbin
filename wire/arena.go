package wire

import (
	"unsafe"

	"github.com/ivanjoz/colbin/packed5"
)

// arenaChunk is the most an Arena allocates at once, and arenaOwn the longest
// string it copies into a chunk: a longer one takes its own allocation, so that
// it neither wastes the rest of a chunk nor pins one when kept on its own.
const (
	arenaChunk = 16 << 10
	arenaOwn   = arenaChunk / 8
)

// Arena copies the strings of one decode into shared chunks, so that a message
// of a million short strings is a few hundred allocations rather than a million,
// and the collector has that many fewer objects to mark. It is the row path's
// version of what Strings does for a list.
//
// The price is retention, bounded by the chunk: one string kept alive keeps the
// up to 16 KiB it was copied beside. A chunk is never larger than the bytes of
// the message left to give one, so a small message costs one small allocation.
//
// The zero Arena is ready, and allocates a string at a time until told the
// message's size by Reset.
type Arena struct {
	// free is the unused tail of the current chunk. Bytes before it are the
	// strings already handed out, and are never written again.
	free []byte
	// left is the message's bytes not yet given a chunk: no decode's strings
	// add up to more than its message, packed ones aside.
	left int
}

// Reset readies the arena for a message of size bytes. The chunks of the
// previous one stay with the strings made from them.
func (a *Arena) Reset(size int) { *a = Arena{left: size} }

// String returns a copy of b.
func (a *Arena) String(b []byte) string {
	if len(b) > len(a.free) && !a.grow(len(b)) {
		return string(b)
	}
	if len(b) == 0 {
		return ""
	}
	n := copy(a.free, b)
	s := unsafe.String(&a.free[0], n)
	a.free = a.free[n:]
	return s
}

// grow starts a chunk with room for n bytes, or reports false for a string to
// be given its own allocation.
func (a *Arena) grow(n int) bool {
	if n > arenaOwn {
		return false
	}
	size := min(arenaChunk, max(a.left, n))
	a.left -= size
	a.free = make([]byte, size)
	return true
}

// StringIn is String with the copy made in a: for a decode that reads many
// strings and can say how big its message is.
func (r *Reader) StringIn(a *Arena) string {
	at := r.at
	if at+2 <= len(r.buffer) {
		nibble, next := r.buffer[at]&0b1111, r.buffer[at+1]
		if nibble == nibbleLength|flagRaw && next <= inlineLength {
			if size := int(next); size <= len(r.buffer)-at-2 {
				r.at = at + 2 + size
				return a.String(r.buffer[at+2 : at+2+size])
			}
		} else if size := int(nibble) - sizedBias; size > 0 && size <= maxSized && size <= len(r.buffer)-at-1 {
			r.at = at + 1 + size
			return a.String(r.buffer[at+1 : at+1+size])
		}
	}
	return r.stringWideIn(a)
}

// stringWideIn is stringWide with the raw forms copied into a. A packed string
// is decoded into its own allocation already, which is kept as the string.
func (r *Reader) stringWideIn(a *Arena) string {
	nibble, start, size, ok := r.field()
	switch {
	case !ok:
		return ""
	case nibble < nibbleSized || nibble == nibbleLength|flagBits:
		r.fail(ErrBadEscape)
		return ""
	case nibble <= nibbleLength|flagRaw:
		return a.String(r.buffer[start : start+size])
	}
	out, err := packed5.AppendString(nil, r.buffer[start:], size, nibble&flagBits == flagPackedUpper)
	if err != nil {
		r.fail(err)
		return ""
	}
	return unsafe.String(unsafe.SliceData(out), len(out))
}
