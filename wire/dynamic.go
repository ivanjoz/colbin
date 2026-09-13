package wire

// Values that say what they are.
//
// Everything else in this package is schema-driven: a key names a field and the
// schema says the payload is a float, an unsigned integer, a string. That is
// where the size and the speed come from, and it is the right default for a
// message whose type both ends agree on in advance.
//
// It is not available to a `map[string]any`. Its values have no declared type —
// only the one each of them happens to have — so the wire has to carry that.
//
// # Most of it already works
//
// A key-less element under eight-bit keys is `[descriptor][payload]`, and a
// descriptor names its class. An integer, a blob, a list, a map and a struct are
// therefore self-describing today: the LIST detail bit `listHomogeneous` exists
// precisely so that a list can hold elements of unlike shapes, and `Skip` sizes
// every class without being told what it is.
//
// Three things are missing, and this file adds them as SPECIAL details — the
// class that was set aside for exactly this and had `null`, `true` and `false`
// declared in it since the format was written:
//
//	3  FLOAT64   the bits, byte-reversed, as an ordinary integer value
//	4  FLOAT32   the same at the narrower width
//	5  BYTES     an ordinary blob, but opaque bytes rather than text
//	7  TYPED     a struct-table index, then an ordinary value
//
// # Why a float needs a detail at all
//
// A float rides in the integer shape with its bytes reversed, so that a round
// value costs two bytes rather than eight (see F64). With a schema that is free:
// the schema says the field is a float and the reader reverses it back. Without
// one, `float64(2)` and `uint64(64)` are the same four bits and the same
// payload, and a reader guessing between them is a reader producing silent
// nonsense. The detail byte is what makes the guess unnecessary — and the
// reversal is kept, so `2.0` in a dynamic position is still two bytes.
//
// # TYPED is what keeps an array of records cheap
//
// The expensive way to put `[]User` inside an `any` is a list of maps, which
// writes every field *name* on every row — the one thing this format exists not
// to do. TYPED says "what follows is struct number three, which the section
// already describes", and then the rows are written by the ordinary composite
// path: a LIST, or a TABLE with the columns transposed. Two bytes for the whole
// array, and the rows cost what they cost in a typed field.
//
// This is the `ANY` door BYTE_ALIGNED_PLAN.md §2.6 reserved, spent on the thing
// it was reserved for.
//
// # None of it exists at four key bits
//
// A narrow descriptor is a nibble with no room for a class, so a narrow map's
// entries take their type from the schema and cannot say one. A scope holding a
// dynamic value therefore uses eight-bit keys — which is the sharpest statement
// of what the two widths are for, and is why this file has no `Writer` half.

import (
	"math"
	"math/bits"
)

// The SPECIAL details this file assigns. Details 0..2 are null, true and false;
// 6 is unspent; 8..15 are the varint integer.
//
// # These values are on the wire
//
// A detail is four bits and every combination is a valid descriptor, so moving
// one of these retypes every dynamic value already written — a float read as a
// blob, with nothing looking wrong until the bytes come out. New details take
// the spare code and none of these ever moves.
const (
	specialFloat64 uint8 = 3
	specialFloat32 uint8 = 4
	specialBytes   uint8 = 5
	specialTyped   uint8 = 7
)

// Kind is what a value's descriptor says it is, for a reader that has no schema
// to ask.
//
// It is deliberately coarser than the class table: KindInt covers the inline
// form, both signs and the varint, because a reader building a document wants
// "an integer" and not "an integer in one of four framings". The ones that are
// genuinely different documents — a string against a blob, a list against a map
// — stay apart.
type Kind uint8

const (
	// KindInvalid is a descriptor this version does not assign. It is zero so
	// that a zero Kind is never mistaken for a value.
	KindInvalid Kind = iota
	KindNull
	KindBool
	KindInt
	KindFloat64
	KindFloat32
	KindString
	KindBytes
	KindList
	KindStruct
	KindMap
	KindTable
	// KindTyped is a value whose type the schema names: the index of a struct
	// def, and then an ordinary struct, list or table of it.
	KindTyped
)

// Writer side. Each of these writes a key-less value, which is what a list
// element, a map entry and a dynamic field's payload all are.

// Key writes a field's key with nothing behind it yet.
//
// Every schema-driven writer folds the key into the typed call, because it knows
// what follows before it starts. A dynamic field does not: its shape comes from
// the value, so the key goes down first and one of the element writers below
// completes the field.
func (w *Writer8) Key(key uint8) {
	w.Buffer = append(w.Buffer, key)
}

// ElementNull writes the null value.
func (w *Writer8) ElementNull() {
	w.Buffer = append(w.Buffer, descriptor(classSpecial, specialNull))
}

// ElementBool writes true or false, each one descriptor byte and no payload.
func (w *Writer8) ElementBool(value bool) {
	detail := specialFalse
	if value {
		detail = specialTrue
	}
	w.Buffer = append(w.Buffer, descriptor(classSpecial, detail))
}

// ElementFloat64 writes a float that says it is one.
//
// The payload is ElementUint over the reversed bits, so every property of a
// scalar float field holds here too: a round value is one inline byte behind the
// detail, and the magnitude is trimmed to what the mantissa actually uses.
func (w *Writer8) ElementFloat64(value float64) {
	w.Buffer = append(w.Buffer, descriptor(classSpecial, specialFloat64))
	w.ElementUint(bits.ReverseBytes64(math.Float64bits(value)))
}

// ElementFloat32 is ElementFloat64 at the narrower width. The width is on the
// wire because a reader without a schema has no other way to know it, and a
// 32-bit pattern read as a 64-bit one is not an error, only wrong.
func (w *Writer8) ElementFloat32(value float32) {
	w.Buffer = append(w.Buffer, descriptor(classSpecial, specialFloat32))
	w.ElementUint(uint64(bits.ReverseBytes32(math.Float32bits(value))))
}

// ElementBytes writes an opaque blob: the same framing as ElementString behind
// one byte that says the contents are bytes and not text.
//
// The distinction is the reader's, not the wire's — a JSON writer spells one as
// a string and the other as base64 — and it costs a byte only in a dynamic
// position, where there is no schema to carry it for free.
func (w *Writer8) ElementBytes(value []byte) {
	w.Buffer = append(w.Buffer, descriptor(classSpecial, specialBytes))
	code, width := lengthCodeFor(uint64(len(value)))
	w.Buffer = appendMagnitude(
		append(w.Buffer, descriptor(classBlob, code)), uint64(len(value)), width)
	w.Buffer = append(w.Buffer, value...)
}

// ElementTyped writes the tag that names a struct def. The value itself follows:
// OpenElementStruct for one record, OpenElementList or OpenElementTable for many.
func (w *Writer8) ElementTyped(index int) {
	w.Buffer = appendCount(
		append(w.Buffer, descriptor(classSpecial, specialTyped)), index)
}

// OpenElementList begins a key-less list of count values. Close it with Close.
func (w *Writer8) OpenElementList(count int) Mark {
	w.Buffer = append(w.Buffer, descriptor(classList, 0), 0)
	mark := Mark{at: len(w.Buffer) - 1}
	w.Buffer = appendCount(w.Buffer, count)
	return mark
}

// OpenElementMap begins a key-less map of count entries, each entry a key value
// then a value value.
func (w *Writer8) OpenElementMap(count int) Mark {
	w.Buffer = append(w.Buffer, descriptor(classMap, 0), 0)
	mark := Mark{at: len(w.Buffer) - 1}
	w.Buffer = appendCount(w.Buffer, count)
	return mark
}

// OpenElementTable begins a key-less table of rows rows.
func (w *Writer8) OpenElementTable(rows int) Mark {
	w.Buffer = append(w.Buffer, descriptor(classMap, tableFlag), 0)
	mark := Mark{at: len(w.Buffer) - 1}
	w.Buffer = appendCount(w.Buffer, rows)
	return mark
}

// Reader side. Like every other element reader these sit on the descriptor,
// which is one byte past where a keyed read expects it.

// Payload steps the cursor from a field's key onto its value, so that the
// element readers below can read what a keyed field holds.
//
// It is Key's mirror. A dynamic field is the one place where the framing and the
// value are read by different code — the key is a field's and everything behind
// it is a key-less value's — and moving the cursor once is cheaper to hold in
// the head than a keyed twin of every reader in this file.
func (r *Reader8) Payload() bool {
	if r.at >= len(r.buffer) {
		r.fail(ErrTruncated)
		return false
	}
	r.at++
	return true
}

// ElementKind says what the value at the cursor is, without advancing.
//
// It answers KindInvalid for a descriptor this version does not assign, and the
// caller is expected to fail on that rather than to guess: every byte is a
// plausible descriptor, so an unassigned one is a message from something newer
// and not a value to skip past quietly.
func (r *Reader8) ElementKind() Kind {
	if r.err != nil || r.at >= len(r.buffer) {
		return KindInvalid
	}
	return kindOf(r.buffer[r.at])
}

// kindOf classifies one descriptor byte.
func kindOf(desc uint8) Kind {
	if desc < descExplicit {
		return KindInt // the descriptor is a small positive value
	}
	switch (desc >> 4) & 0b111 {
	case classInt:
		return KindInt
	case classBlob:
		return KindString
	case classList:
		return KindList
	case classStruct:
		return KindStruct
	case classMap:
		if desc&tableFlag != 0 {
			return KindTable
		}
		return KindMap
	case classSpecial:
		if desc&specialVarint != 0 {
			return KindInt
		}
		switch desc & 0b1111 {
		case specialNull:
			return KindNull
		case specialTrue, specialFalse:
			return KindBool
		case specialFloat64:
			return KindFloat64
		case specialFloat32:
			return KindFloat32
		case specialBytes:
			return KindBytes
		case specialTyped:
			return KindTyped
		}
	}
	return KindInvalid
}

// ElementNegative reports whether the integer at the cursor is a negative one,
// without advancing.
//
// A reader with a schema never asks: the field is signed or it is not. A reader
// without one has to, because a magnitude past 2^63 is a value this format
// carries and int64 is not where it fits — so the sign decides which of
// ElementInt and ElementUint reads it back whole.
//
// The varint form answers false, and that is a don't-care rather than a claim:
// Uint writes it plain and Int writes it zigzagged, so the sign is not in the
// framing — and no element writer here emits one.
func (r *Reader8) ElementNegative() bool {
	if r.at >= len(r.buffer) {
		return false
	}
	desc := r.buffer[r.at]
	return desc >= descExplicit &&
		(desc>>4)&0b111 == classInt && desc&intPositiveFlag == 0
}

// ElementNull steps over a null.
func (r *Reader8) ElementNull() {
	if r.at < len(r.buffer) {
		r.at++
		return
	}
	r.fail(ErrTruncated)
}

// ElementBool reads true or false.
func (r *Reader8) ElementBool() bool {
	if r.at >= len(r.buffer) {
		r.fail(ErrTruncated)
		return false
	}
	desc := r.buffer[r.at]
	r.at++
	return desc == descriptor(classSpecial, specialTrue)
}

// ElementFloat64 reads a float64 written by ElementFloat64.
func (r *Reader8) ElementFloat64() float64 {
	return math.Float64frombits(bits.ReverseBytes64(r.elementFloatBits()))
}

// ElementFloat32 reads a float32 written by ElementFloat32.
func (r *Reader8) ElementFloat32() float32 {
	return math.Float32frombits(bits.ReverseBytes32(uint32(r.elementFloatBits())))
}

// elementFloatBits steps over the detail byte and reads the integer behind it.
func (r *Reader8) elementFloatBits() uint64 {
	if r.at >= len(r.buffer) {
		r.fail(ErrTruncated)
		return 0
	}
	r.at++
	return r.ElementUint()
}

// ElementBytes returns an opaque blob as a sub-slice of the message, which stays
// valid only as long as the message buffer does.
func (r *Reader8) ElementBytes() []byte {
	if r.at >= len(r.buffer) {
		r.fail(ErrTruncated)
		return nil
	}
	r.at++
	return r.ElementBlob()
}

// ElementBlob is ElementBytes for a blob with no detail byte in front of it,
// which is what a string element is.
func (r *Reader8) ElementBlob() []byte {
	r.at--
	return r.Bytes()
}

// ElementTypedIndex reads the tag that names a struct def and leaves the cursor
// on the value behind it.
func (r *Reader8) ElementTypedIndex() (int, bool) {
	if r.at >= len(r.buffer) {
		r.fail(ErrTruncated)
		return 0, false
	}
	index, width, ok := readCount(r.buffer[r.at+1:])
	if !ok {
		r.fail(ErrTruncated)
		return 0, false
	}
	r.at += 1 + width
	return index, true
}

// ElementList returns the element count and a reader over a key-less list.
func (r *Reader8) ElementList() (int, Reader8, bool) {
	r.at--
	return r.List()
}

// ElementMap returns the entry count and a reader over a key-less map.
func (r *Reader8) ElementMap() (int, Reader8, bool) {
	r.at--
	return r.Map()
}

// ElementTable returns the row count and a reader over a key-less table's
// columns.
func (r *Reader8) ElementTable() (int, Reader8, bool) {
	r.at--
	return r.Table()
}

// SkipElement steps over one key-less value of any kind, which is what lets a
// reader walk past a dynamic value it has no use for.
func (r *Reader8) SkipElement() bool {
	size, err := valueSize(r.buffer, r.at)
	if err != nil {
		r.fail(err)
		return false
	}
	r.at += size
	return true
}

// specialSize is valueSize's SPECIAL arm: the details that carry nothing, the
// varint, and the three that are a detail byte in front of an ordinary value.
func specialSize(buffer []byte, at int, desc uint8) (int, error) {
	if desc&specialVarint != 0 {
		// A varint is self-delimiting, so a reader that does not know the key can
		// still step over it: walk the continuation bits to their end.
		_, length, ok := readVarintAt(buffer, at, desc)
		if !ok {
			return 0, ErrTruncated
		}
		return length, nil
	}
	switch desc & 0b1111 {
	case specialFloat64, specialFloat32, specialBytes:
		// A detail byte and then one ordinary value, so the size is one plus that
		// value's — which is what keeps a dynamic field skippable by a reader that
		// has never heard of these details.
		size, err := valueSize(buffer, at+1)
		if err != nil {
			return 0, err
		}
		return 1 + size, nil
	case specialTyped:
		_, width, ok := readCount(buffer[at+1:])
		if !ok {
			return 0, ErrTruncated
		}
		size, err := valueSize(buffer, at+1+width)
		if err != nil {
			return 0, err
		}
		return 1 + width + size, nil
	}
	return 1, nil
}
