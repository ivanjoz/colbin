package wire

// Composites at four key bits.
//
// A narrow composite is the length form of a field with a body in it, so a
// reader that does not know the key steps over one as it does any other field:
//
//	struct  [key:4][1 1 0 k8]    [length] [key run]
//	list    [key:4][1 1 0 0]     [length] [count] ( [len] [body] )*
//	table   [key:4][1 1 0 1]     [length] [rows]  column run
//	map     [key:4][1 1 0 0]     [length] [count] ( key value )*
//
// The flag bit f0 tells apart only what the schema cannot: a struct's own key
// width, and whether a []Struct went out as a list or a table. A struct and a
// map are other Go types, so their flags overlap nothing. f1 is refused.
//
// A table's column run uses the row type's key width, which the reader takes
// from the schema as it does a list element's shape: a four-bit integer column
// is [key:4][1100][length][column], a string column a string array with one
// element per row, and eight-bit ones are Writer8's.

import (
	"encoding/binary"
	"math"
	"math/bits"

	"github.com/ivanjoz/colbin/column"
)

// The composites' flag bit f0.
const (
	// narrowWideKeys is a struct's f0, set when the key run inside uses eight-bit
	// keys.
	narrowWideKeys uint8 = 0b01
	// narrowTable is a []Struct's f0, set when it went out as a table.
	narrowTable uint8 = 0b01
)

// openNarrowComposite writes a key, the length form's nibble and a one-byte
// length placeholder for Close to patch.
func (w *Writer) openNarrowComposite(key, flags uint8) Mark {
	w.Buffer = append(w.Buffer, key<<4|nibbleLength|flags, 0)
	return Mark{at: len(w.Buffer) - 1}
}

// OpenStruct begins a nested key run under key, at four key bits inside.
func (w *Writer) OpenStruct(key uint8) Mark {
	return w.openNarrowComposite(key, 0)
}

// OpenStructWide begins a nested run whose own keys are eight bits, which is how
// a narrow struct holds a type that needs more than sixteen ids.
func (w *Writer) OpenStructWide(key uint8) Mark {
	return w.openNarrowComposite(key, narrowWideKeys)
}

// OpenList begins a list of count elements under key. Each element is opened
// with OpenElement, because a narrow element carries a length and no descriptor
// — its shape is the schema's to know.
func (w *Writer) OpenList(key uint8, count int) Mark {
	mark := w.openNarrowComposite(key, 0)
	w.Buffer = appendCount(w.Buffer, count)
	return mark
}

// OpenMap begins a map of count entries under key.
func (w *Writer) OpenMap(key uint8, count int) Mark {
	mark := w.openNarrowComposite(key, 0)
	w.Buffer = appendCount(w.Buffer, count)
	return mark
}

// OpenTable begins a table of rows rows under key. Its columns follow in the row
// type's key width: Column for four bits, or a Writer8 over the same buffer for
// eight. Close it with Close.
func (w *Writer) OpenTable(key uint8, rows int) Mark {
	mark := w.openNarrowComposite(key, narrowTable)
	w.Buffer = appendCount(w.Buffer, rows)
	return mark
}

// OpenElement begins one element of a narrow list: a length and a body, with no
// descriptor between them. Close it with CloseElement, not with Close.
func (w *Writer) OpenElement() Mark {
	w.Buffer = append(w.Buffer, 0)
	return Mark{at: len(w.Buffer) - 1}
}

// Close patches a composite's length, widening the placeholder when the body
// outgrew it. It is the same backpatch the wide writer does, and the same
// reason: sizing the value first would cost a pass over every nested one.
//
// A body of up to 253 bytes fits the placeholder. A longer one moves up by two
// for the 0xFE escape and a u16, or by four for 0xFF and a u32 — the length's
// own escape says which, so nothing in front of the placeholder changes.
//
// It does not inline, where the single-escape version did: that one inlined a
// widen costing 47 of the budget's 80, and two escapes cost more than the
// budget has left, so the widen is a call and Close is one too. Measured as a
// third of a nanosecond per composite, which is what two bytes less than a lone
// u32 escape, on every body of 254 to 65 535 bytes, costs.
//
// It is for a *keyed* field only — a composite, or a packed string's payload. A list element's length is a count-style
// size, one byte up to 254 or 0xFF and a u32, and CloseElement is the one to
// call — see the comment there.
func (w *Writer) Close(mark Mark) {
	body := len(w.Buffer) - (mark.at + 1)
	if body <= inlineLength {
		w.Buffer[mark.at] = uint8(body)
		return
	}
	w.widenLength(mark, body)
}

// CloseElement patches a narrow list element's length.
//
// It is not Close, and the difference is the whole reason it exists. An
// element's size is the form a count and a string-array element take — one byte
// up to 254, or 0xFF and four — which is what Element reads, and Close writes a
// field's length, whose 0xFE escape means a u16. Calling Close on an element
// whose body reached 254 bytes wrote a length Element misreads.
//
// The old shape of this bug wrote a bare four-byte length and OR-ed a width code
// into the byte before the placeholder: a message Marshal produced and
// Unmarshal refused, for any narrow list whose element body reached 255 bytes —
// a `[]struct` under the table threshold holding a string of a couple of hundred
// characters, which is an ordinary record rather than a corner.
// TestNarrowListElementWidths pins it.
func (w *Writer) CloseElement(mark Mark) {
	body := len(w.Buffer) - (mark.at + 1)
	if body <= inlineElementSize {
		w.Buffer[mark.at] = uint8(body)
		return
	}
	// The escape Element reads: 0xFF and then four bytes, so the body shifts up
	// by the four the placeholder does not already hold.
	w.Buffer = append(w.Buffer, 0, 0, 0, 0)
	copy(w.Buffer[mark.at+5:], w.Buffer[mark.at+1:len(w.Buffer)-4])
	w.Buffer[mark.at] = elementSizeEscape
	binary.LittleEndian.PutUint32(w.Buffer[mark.at+1:], uint32(body))
}

// widenLength turns a one-byte length placeholder into three or five bytes,
// shifting the body up to make room. Out of line because it is the rare path.
func (w *Writer) widenLength(mark Mark, body int) {
	if body <= 0xFFFF {
		w.Buffer = append(w.Buffer, 0, 0)
		copy(w.Buffer[mark.at+3:], w.Buffer[mark.at+1:len(w.Buffer)-2])
		w.Buffer[mark.at] = length16
		binary.LittleEndian.PutUint16(w.Buffer[mark.at+1:], uint16(body))
		return
	}
	if uint64(body) > math.MaxUint32 {
		panic(errFieldTooLarge)
	}
	w.Buffer = append(w.Buffer, 0, 0, 0, 0)
	copy(w.Buffer[mark.at+5:], w.Buffer[mark.at+1:len(w.Buffer)-4])
	w.Buffer[mark.at] = length32
	binary.LittleEndian.PutUint32(w.Buffer[mark.at+1:], uint32(body))
}

// Element writers, the key-less values a narrow map's entries are made of. A
// narrow list's elements are whole key runs and go through OpenElement instead.
//
// They keep a code table of their own rather than the field nibble: an element
// sits inside a composite whose length already steps over it, so it does not
// need to size itself, and its code spends no bit on a length form it would
// never use.

// ElementUint writes an unsigned element: codes 0..7 are the value itself and
// 8..15 a magnitude of code−7 bytes.
func (w *Writer) ElementUint(value uint64) {
	// Zero is code 0, so a map value of zero is one byte and needs no special
	// case. It used to need one: the signed form's code 0 means "the value is
	// one", and a map value is not a field, so the omit-zero rule that keeps a
	// field away from that code does not cover it.
	if value <= elementInlineMax {
		w.Buffer = append(w.Buffer, uint8(value))
		return
	}
	width := (bits.Len64(value) + 7) / 8
	w.Buffer = appendMagnitude(
		append(w.Buffer, elementWidthBase+uint8(width)-1), value, width)
}

// ElementInt writes the signed [positive:1][size:3] form, and does not hand a
// positive value to ElementUint: the two codes are different tables, and
// ElementInt is what will read this back.
func (w *Writer) ElementInt(value int64) {
	if value >= 0 {
		magnitude := uint64(value)
		if magnitude == 0 {
			// Size code 0 means "the magnitude is one". A map value of zero is
			// legitimate, so it spends a byte saying so.
			w.Buffer = append(w.Buffer, intPositiveFlag|1, 0)
			return
		}
		code, width := sizeCodeFor(magnitude)
		w.Buffer = appendMagnitude(append(w.Buffer, intPositiveFlag|code), magnitude, width)
		return
	}
	magnitude := -uint64(value)
	code, width := sizeCodeFor(magnitude)
	w.Buffer = appendMagnitude(append(w.Buffer, code), magnitude, width)
}

func (w *Writer) ElementString(value string) {
	code, width := lengthCodeFor(uint64(len(value)))
	w.Buffer = appendMagnitude(append(w.Buffer, code), uint64(len(value)), width)
	w.Buffer = append(w.Buffer, value...)
}

// Reader side.

// compositeBody reads a narrow composite's framing and returns its body,
// advancing past the whole field. A composite is the length form with f1 clear;
// anything else is a field of some other shape.
func (r *Reader) compositeBody() (body []byte, flags uint8, ok bool) {
	nibble, start, size, ok := r.field()
	if !ok {
		return nil, 0, false
	}
	if nibble&^narrowTable != nibbleLength {
		r.fail(ErrBadEscape)
		return nil, 0, false
	}
	return r.buffer[start : start+size], nibble & narrowTable, true
}

// StructBody returns a nested run's bytes and the key width it uses.
func (r *Reader) StructBody() (body []byte, wideKeys, ok bool) {
	body, flags, ok := r.compositeBody()
	return body, flags == narrowWideKeys, ok
}

// IsTable reports whether the field at the cursor is a table rather than a list.
func (r *Reader) IsTable() bool {
	return r.at < len(r.buffer) && r.buffer[r.at]&0b1111 == nibbleLength|narrowTable
}

// List returns a list's element count and a reader over its elements. The count
// is checked against the body: every element is at least its length byte.
func (r *Reader) List() (int, Reader, bool) { return r.counted(1) }

// Map returns a map's entry count and a reader over its entries, which are at
// least two bytes each.
func (r *Reader) Map() (int, Reader, bool) { return r.counted(2) }

func (r *Reader) counted(minSize int) (int, Reader, bool) {
	body, _, ok := r.compositeBody()
	if !ok {
		return 0, Reader{}, false
	}
	count, at, ok := readCount(body)
	if !ok || count > (len(body)-at)/minSize {
		r.fail(ErrTruncated)
		return 0, Reader{}, false
	}
	return count, Reader{buffer: body[at:]}, true
}

// Table returns a table's row count and its column run, whose key width the
// caller knows from the row type: NewReader or NewReader8 over it accordingly.
//
// The row count is not bounded by the message — an absent column is a column of
// zeros, so a table of a million rows can be a few bytes — and a caller that
// allocates for it must budget rows itself.
func (r *Reader) Table() (rows int, columns []byte, ok bool) {
	body, flags, ok := r.compositeBody()
	if !ok {
		return 0, nil, false
	}
	if flags != narrowTable {
		r.fail(ErrBadDescriptor)
		return 0, nil, false
	}
	rows, at, ok := readCount(body)
	if !ok {
		r.fail(ErrTruncated)
		return 0, nil, false
	}
	return rows, body[at:], true
}

// Element returns one narrow list element's body: a length and then a key run.
func (r *Reader) Element() ([]byte, bool) {
	if r.at >= len(r.buffer) {
		r.fail(ErrTruncated)
		return nil, false
	}
	size := int(r.buffer[r.at])
	start := r.at + 1
	if size == elementSizeEscape {
		if r.at+5 > len(r.buffer) {
			r.fail(ErrTruncated)
			return nil, false
		}
		value := binary.LittleEndian.Uint32(r.buffer[r.at+1:])
		if uint64(value) > uint64(maxInt) {
			r.fail(ErrSizeTooLarge)
			return nil, false
		}
		size, start = int(value), r.at+5
	}
	if size > len(r.buffer)-start {
		r.fail(ErrTruncated)
		return nil, false
	}
	r.at = start + size
	return r.buffer[start : start+size], true
}

// Element readers, the mirror of the element writers.

func (r *Reader) ElementUint() uint64 {
	if r.at >= len(r.buffer) {
		r.fail(ErrTruncated)
		return 0
	}
	code := r.buffer[r.at] & 0b1111
	if code <= elementInlineMax {
		r.at++
		return uint64(code)
	}
	width := int(code) - elementWidthBase + 1
	rest := r.buffer[r.at+1:]
	if len(rest) < width {
		r.fail(ErrTruncated)
		return 0
	}
	r.at += 1 + width
	return leUint(rest, width)
}

func (r *Reader) ElementInt() int64 {
	if r.at >= len(r.buffer) {
		r.fail(ErrTruncated)
		return 0
	}
	positive := r.buffer[r.at]&intPositiveFlag != 0
	magnitude := r.elementMagnitude()
	return r.signed(positive, magnitude)
}

// signed applies a sign to a magnitude, refusing one no int64 holds: past 2^63-1
// when positive, past 2^63 when negative.
func (r *Reader) signed(positive bool, magnitude uint64) int64 {
	if positive {
		if magnitude > math.MaxInt64 {
			r.fail(ErrFieldTooWide)
			return 0
		}
		return int64(magnitude)
	}
	if magnitude > 1<<63 {
		r.fail(ErrFieldTooWide)
		return 0
	}
	return -int64(magnitude)
}

// elementMagnitude reads a signed element's [size:3] magnitude, which ElementInt
// needs rather than ElementUint's table: the two codes are different tables over
// the same four bits.
func (r *Reader) elementMagnitude() uint64 {
	if r.at >= len(r.buffer) {
		r.fail(ErrTruncated)
		return 0
	}
	width := magnitudeWidth[r.buffer[r.at]&0b111]
	rest := r.buffer[r.at+1:]
	if len(rest) < width {
		r.fail(ErrTruncated)
		return 0
	}
	r.at += 1 + width
	if width == 0 {
		return 1
	}
	return leUint(rest, width)
}

func (r *Reader) ElementString() string {
	if r.at >= len(r.buffer) {
		r.fail(ErrTruncated)
		return ""
	}
	width := lengthWidth[r.buffer[r.at]&0b11]
	rest := r.buffer[r.at+1:]
	if len(rest) < width {
		r.fail(ErrTruncated)
		return ""
	}
	size := leUint(rest, width)
	start := r.at + 1 + width
	if size > uint64(maxInt) || int(size) > len(r.buffer)-start {
		r.fail(ErrTruncated)
		return ""
	}
	r.at = start + int(size)
	return string(r.buffer[start : start+int(size)])
}

// Column writes an integer column under key, for a narrow-keyed table. It is
// Writer8.Column with four key bits: the column codec underneath is the same.
//
// The column is the length form with clear flags, so a reader that does not
// know the column's key steps over it.
func (w *Writer) Column(key uint8, values []int64) {
	if allZero(values) {
		return
	}
	mark := w.openNarrowComposite(key, 0)
	w.Buffer = column.AppendArray(w.Buffer, values)
	w.Close(mark)
}

// Column reads a column written by Column. The row count comes from the table,
// which said it once for every column.
func (r *Reader) Column(rows int, dst []int64) []int64 {
	body, _, ok := r.compositeBody()
	if !ok {
		return dst
	}
	dst, err := decodeColumn(body, rows, dst)
	r.Fail(err)
	return dst
}

// Fail records an error from a sub-reader on its parent.
func (r *Reader) Fail(err error) {
	if err != nil {
		r.fail(err)
	}
}
