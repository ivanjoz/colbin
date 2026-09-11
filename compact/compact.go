// Package compact implements colbin's compact mode: a bit-level wire format for
// a single struct, or an array of at most three, where the standard mode's
// columnar layout has nothing to amortise its per-column framing over.
//
//	import "github.com/ivanjoz/colbin/compact"
//
//	w := compact.NewWriter(nil, compact.ShapeStruct, true, compact.Keys8)
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
// Five bits, after which the message is one LSB-first bitstream with no
// alignment anywhere until a final pad to a byte boundary:
//
//	bit  0    1              compact mode
//	bit  1    ALL_POSITIVE   every integer in the message is >= 0
//	bits 2-3  shape          0 = lone struct, 1..3 = array of that many records
//	bit  4    NARROW_KEYS    field ids are 4 bits wide rather than 8
//
// Shape 0 and shape 1 both carry one record; they differ only in whether it is
// rendered as an object or as an array of one, which the binary path takes from
// the destination Go type but a JSON reader needs told.
//
// # Records
//
// A record is a run of [key][value] pairs closed by the terminator key. A field
// holding its zero value is omitted entirely, so an absent field costs nothing
// beyond the terminator the record already owes. The key is the same field id
// the standard mode uses, so a reader resolves fields by id, not by position.
//
// # Key width
//
// A key is 8 bits by default, matching the standard mode's field id, with 255
// closing a record. When every id a message writes is 14 or less the header's
// NARROW_KEYS bit selects a 4-bit key instead, with 15 closing the record — the
// same arrangement one nibble down, so a reader still resolves fields by id and
// still steps over the ones it does not know.
//
// That halves the framing: a record of f present fields spends 4*(f+1) fewer
// bits, against the one bit the header flag costs the whole message. It pays for
// itself on the first field of the first record. Ids are what decide it, not the
// field count, so it wants explicit small ids in the struct tags (`cb:"1"`,
// `cb:"2"`, ...); a hashed id lands anywhere in 0..254 and forces the wide key.
//
// # Composites
//
// A record is a run of keyed values, and three of those value forms are
// themselves made of values. They recurse through the same bitstream, and none
// of them borrows anything from the standard mode: compact mode has no column
// framing to embed, so a nested struct is another key run, not a sub-table. The
// two modes share value codecs -- varint, packed5 -- and nothing else.
//
//	nested struct   [key] [ [key][value] ... [terminator] ]
//	array           [key] [count:varint] [value] [value] ...
//	map             [key] [count:varint] [key][value] [key][value] ...
//
// Every form is self delimiting given the schema, which is the only thing the
// format asks of a value: an array carries its count, a packed5 frame carries
// its own end, and a key run closes on the terminator. Nothing needs a byte
// length and nothing needs a type tag.
//
// What the schema cannot supply is a value's *dynamic* type, so an interface
// field has no compact form at all -- see codec/compact_plan.go, which is where
// the per-field rule lives.
//
// The omit-zero rule reaches into the composites the same way it reaches a
// scalar. A nested struct all of whose fields are zero is omitted entirely, and
// so is an array or map of length zero, which is why an empty-but-non-nil slice
// or map decodes back as nil -- exactly as it already did for a slice.
//
// One thing changes for the key width: a nested key run spends the same key bits
// as the record containing it, so NARROW_KEYS is a property of the whole message
// rather than of the root struct. Every struct reachable in the message must
// keep its ids at or below MaxNarrowKey, which in practice means tagging the
// nested structs too.
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
	// ErrSkipComposite is returned when Skip is asked to step over a nested
	// struct, array or map. Those forms are self delimiting only against their
	// own sub-schema, which Skip is not given -- see Reader.Skip.
	ErrSkipComposite = errors.New("colbin: compact composite cannot be skipped without its sub-schema")
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
//
// It is the id a caller sees in either key width: under Keys4 the terminator on
// the wire is 15, and Reader.Key reports it as TerminatorKey so that the check
// closing a record reads the same on both paths.
const TerminatorKey uint8 = 255

// KeyWidth is how many bits a field id occupies, chosen once per message and
// recorded in the header's NARROW_KEYS bit.
type KeyWidth uint8

const (
	// Keys8 is the default: ids 0..254, with 255 closing a record. It is the
	// standard mode's field id unchanged, so any struct can use it.
	Keys8 KeyWidth = 8
	// Keys4 is the narrow form: ids 0..MaxNarrowKey, with 15 closing a record.
	// It costs one header bit and saves four on every key, so it wins from the
	// first field onwards -- but only a struct whose ids are all small can use
	// it, which in practice means explicit `cb:"1"`-style tags.
	Keys4 KeyWidth = 4
)

// MaxNarrowKey is the largest field id Keys4 can carry. 15 is the terminator, so
// the ids stop one short of it, exactly as 255 stops the wide ones at 254.
const MaxNarrowKey uint8 = 14

// narrow reports whether k is the 4-bit width, which is what the header carries.
// Only Keys4 is narrow, so any other value -- including one a caller invented --
// resolves to the wide key rather than to a width nothing can read.
func (k KeyWidth) narrow() bool { return k == Keys4 }

// Bits is the width of one key. It is also the fewest bits a record can occupy,
// since an empty record is its terminator alone -- which is what bounds-checking
// a record count needs (see Reader.Count).
func (k KeyWidth) Bits() int { return int(k.bits()) }

// bits is the width of one key, and terminator is the id that closes a record at
// that width.
func (k KeyWidth) bits() uint8 {
	if k.narrow() {
		return 4
	}
	return 8
}

func (k KeyWidth) terminator() uint64 {
	if k.narrow() {
		return uint64(narrowTerminatorKey)
	}
	return uint64(TerminatorKey)
}

// narrowTerminatorKey closes a record under Keys4. It is TerminatorKey's role one
// nibble down: the top id at that width, reserved so no field can hold it.
const narrowTerminatorKey uint8 = 15

// Header bit widths, in the order they are written.
const (
	modeBits   = 1 // 1 == compact
	flagBits   = 1 // ALL_POSITIVE
	shapeBits  = 2
	narrowBits = 1 // NARROW_KEYS

	headerBits = modeBits + flagBits + shapeBits + narrowBits
)

// IsCompact reports whether buf holds a compact-mode message, which is the whole
// of the mode discrimination: bit 0 of byte 0.
func IsCompact(buf []byte) bool {
	return len(buf) > 0 && buf[0]&1 == 1
}
