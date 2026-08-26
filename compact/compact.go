// Package compact implements colbin's compact mode: a bit-level wire format for
// a single struct, or an array of at most three, where the standard mode's
// columnar layout has nothing to amortise its per-column framing over.
//
//	import "github.com/ivanjoz/colbin/compact"
//
//	w := compact.NewWriter(nil, compact.ShapeStruct, true)
//	w.Key(0x35); w.Int(1234)
//	w.Key(0x9a); w.String("Usuario1")
//	w.End()
//	buf := w.Done()
//
// # Mode discrimination
//
// Bit 0 of byte 0 selects the format. The two branches share nothing else: a
// compact message has no version field, no record-count varint and no column
// count, because it needs none of them.
//
//	bit 0 == 0    standard mode: the byte is a version byte, columnar layout
//	bit 0 == 1    compact mode: the remaining bits are this package's header
//
// # Header
//
// Four bits, after which the message is one LSB-first bitstream with no
// alignment anywhere until a final pad to a byte boundary:
//
//	bit  0    1              compact mode
//	bit  1    ALL_POSITIVE   every integer in the message is >= 0
//	bits 2-3  shape          0 = lone struct, 1..3 = array of that many records
//
// Shape 0 and shape 1 both carry one record; they differ only in whether it is
// rendered as an object or as an array of one, which the binary path takes from
// the destination Go type but a JSON reader needs told.
//
// # Records
//
// A record is a run of [key:8][value] pairs closed by TerminatorKey. A field
// holding its zero value is omitted entirely, so an absent field costs nothing
// beyond the terminator the record already owes. The key is the same 8-bit field
// id the standard mode uses, so a reader resolves fields by id, not by position.
//
// # Integers
//
// ALL_POSITIVE decides how a signed value becomes the unsigned payload: with the
// flag set the payload is the magnitude, and without it the payload is the
// zigzag. Setting the flag is worth one payload bit on every integer in the
// message, and a message-wide pre-scan is free at three records.
//
// The payload is then written with a varint whose first unit is one bit narrower
// than LEB128's, spending that bit on a selector:
//
//	unit 0    [selector:1] [cont:1] [payload:6]   (+ [payload:4] if selector)
//	unit i    [cont:1] [payload:7]
//
// The selector's four bits are a one-time offset applied to the first unit only,
// not a per-unit addition. That gives two ladders of capacity — 6+7k without it
// and 10+7k with it — and the encoder takes whichever reaches the value first:
//
//	bits    0-6   7    8-10  11-13  14   15-17  18-20  21   22-24
//	LEB128    8   8      16     16   16     24     24   24      32
//	compact   8  12      12     16   20     20     24   28      28
//
// Three ties, three wins of four bits, one loss of four, per seven bit-lengths.
// The selector costs nothing outright: it takes the seventh payload bit of unit
// 0, which any value wider than six bits was going to spill anyway. The loss
// falls only where LEB128's first byte was exactly full.
package compact

import "errors"

var (
	// ErrNotCompact is returned when a buffer's first bit is clear, meaning it
	// holds a standard-mode message rather than a compact one.
	ErrNotCompact = errors.New("colbin: not a compact message")
	// ErrTruncated is returned when a read runs past the end of the bitstream.
	ErrTruncated = errors.New("colbin: compact message truncated")
	// ErrBadString is returned when a string field does not hold a valid packed5
	// frame.
	ErrBadString = errors.New("colbin: compact bad string frame")
)

// Shape says how many records the message holds and whether a lone record is an
// object or an array of one. It occupies bits 2-3 of the header.
type Shape uint8

const (
	// ShapeStruct is one record rendered as an object.
	ShapeStruct Shape = 0
	// ShapeArray1, ShapeArray2 and ShapeArray3 are arrays of that many records.
	ShapeArray1 Shape = 1
	ShapeArray2 Shape = 2
	ShapeArray3 Shape = 3
)

// Records is how many records the shape describes.
func (s Shape) Records() int {
	if s == ShapeStruct {
		return 1
	}
	return int(s)
}

// MaxRecords is the largest array compact mode can carry. Past it the columnar
// standard mode has enough rows to amortise its framing and wins outright.
const MaxRecords = 3

// TerminatorKey closes a record's key/value run. It matches the field id the
// standard mode reserves, so no real field can collide with it.
const TerminatorKey uint8 = 255

// Header bit widths, in the order they are written.
const (
	modeBits  = 1 // 1 == compact
	flagBits  = 1 // ALL_POSITIVE
	shapeBits = 2
	keyBits   = 8
)

// IsCompact reports whether buf holds a compact-mode message, which is the whole
// of the mode discrimination: bit 0 of byte 0.
func IsCompact(buf []byte) bool {
	return len(buf) > 0 && buf[0]&1 == 1
}
