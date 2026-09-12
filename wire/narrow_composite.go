package wire

// Composites at four key bits.
//
// A narrow descriptor has no room for a class, and it does not need one: a
// narrow reader has the schema, so it already knows whether the field it is
// looking at is a struct, a list, a map or a table. What it needs from the wire
// is the same thing the wide form needs — a byte length — and the same four
// detail bits carry it.
//
//	struct  [key:4][k8:1][—:1][lw:2]   [len: lw] [key run]
//	list    [key:4][homog:1][—:1][lw:2] [len: lw] [count] ( [len] [body] )*
//	map     [key:4][sub=0:1][—:1][lw:2] [len: lw] [count] ( key value )*
//	table   [key:4][sub=1:1][k8:1][lw:2] [len: lw] [rows] ( [key][desc][column] )*
//
// The detail nibble is bit for bit the wide one's. That is the whole of §2.5:
// the wide descriptor spends its extra nibble on the class, and K4 takes the
// class from the schema instead, so everything below the class is shared.
//
// # What this is worth
//
// Before it, a struct holding any composite had to use eight-bit keys — the
// composite needed a byte length, a byte length needed a class, and a class
// needed the wide descriptor. Every *scalar* in that struct then paid a byte it
// did not need. A four-field order with three nested lines went from 55 bytes to
// 51, against protobuf's 49, and the scalars around the composite went back to
// costing one byte of framing rather than two.
//
// # What it gives up
//
// The same thing K4 always gives up: a field it does not recognise cannot be
// stepped over, because nothing on the wire says what shape it is. A composite
// is skippable under K8 and not under K4, and that is the trade rather than an
// omission.

import (
	"encoding/binary"
	"math/bits"

	"github.com/ivanjoz/colbin/column"
)

// openNarrowComposite writes a key, a detail nibble and a one-byte length
// placeholder, exactly as the wide form does minus the class.
func (w *Writer) openNarrowComposite(key, detail uint8) Mark {
	w.Buffer = append(w.Buffer, key<<4|detail, 0)
	return Mark{at: len(w.Buffer) - 1}
}

// OpenStruct begins a nested key run under key, at four key bits inside.
func (w *Writer) OpenStruct(key uint8) Mark {
	return w.openNarrowComposite(key, 0)
}

// OpenStructWide begins a nested run whose own keys are eight bits, which is how
// a narrow struct holds a type that needs more than sixteen ids.
func (w *Writer) OpenStructWide(key uint8) Mark {
	return w.openNarrowComposite(key, structWideKeys)
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

// OpenTable begins a table of rows rows under key, its columns keyed at four
// bits.
func (w *Writer) OpenTable(key uint8, rows int) Mark {
	mark := w.openNarrowComposite(key, tableFlag)
	w.Buffer = appendCount(w.Buffer, rows)
	return mark
}

// OpenElement begins one element of a narrow list: a length and a body, with no
// descriptor between them.
func (w *Writer) OpenElement() Mark {
	w.Buffer = append(w.Buffer, 0)
	return Mark{at: len(w.Buffer) - 1}
}

// Close patches a composite's length, widening the placeholder when the body
// outgrew it. It is the same backpatch the wide writer does, and the same
// reason: sizing the value first would cost a pass over every nested one.
func (w *Writer) Close(mark Mark) {
	body := len(w.Buffer) - (mark.at + 1)
	if body < inlineCompositeLength {
		w.Buffer[mark.at] = uint8(body)
		return
	}
	w.widenLength(mark, body)
}

func (w *Writer) widenLength(mark Mark, body int) {
	w.Buffer = append(w.Buffer, 0, 0, 0)
	copy(w.Buffer[mark.at+4:], w.Buffer[mark.at+1:len(w.Buffer)-3])
	binary.LittleEndian.PutUint32(w.Buffer[mark.at:], uint32(body))
	w.Buffer[mark.at-1] |= 2 // lw code 2: a four-byte length
}

// Element writers, the key-less values a narrow map's entries are made of. A
// narrow list's elements are whole key runs and go through OpenElement instead.

func (w *Writer) ElementUint(value uint64) {
	// The unsigned nibble carries 0..7 outright, so a map value of zero is one
	// byte and needs no special case. It used to need one: the signed form's
	// code 0 means "the value is one", and a map value is not a field, so the
	// omit-zero rule that keeps a field away from that code does not cover it.
	if value <= uintInlineMax {
		w.Buffer = append(w.Buffer, uint8(value))
		return
	}
	width := (bits.Len64(value) + 7) / 8
	w.Buffer = appendMagnitude(
		append(w.Buffer, uintWidthBase+uint8(width)-1), value, width)
}

// ElementInt writes the signed [positive:1][size:3] form, and does not hand a
// positive value to ElementUint: the two nibbles are different tables, and
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

// compositeBody reads a narrow composite's length and returns its body, advancing
// past the whole field.
func (r *Reader) compositeBody() ([]byte, bool) {
	if r.at+2 > len(r.buffer) {
		r.fail(ErrTruncated)
		return nil, false
	}
	width := lengthWidth[r.buffer[r.at]&0b11]
	rest := r.buffer[r.at+1:]
	if len(rest) < width {
		r.fail(ErrTruncated)
		return nil, false
	}
	length := leUint(rest, width)
	if length > uint64(maxInt) {
		r.fail(ErrSizeTooLarge)
		return nil, false
	}
	start := r.at + 1 + width
	if int(length) > len(r.buffer)-start {
		r.fail(ErrTruncated)
		return nil, false
	}
	r.at = start + int(length)
	return r.buffer[start : start+int(length)], true
}

// StructBody returns a nested run's bytes and the key width it uses.
func (r *Reader) StructBody() (body []byte, wideKeys, ok bool) {
	if r.at >= len(r.buffer) {
		r.fail(ErrTruncated)
		return nil, false, false
	}
	wideKeys = r.buffer[r.at]&structWideKeys != 0
	body, ok = r.compositeBody()
	return body, wideKeys, ok
}

// IsTable reports whether the field at the cursor is a table rather than a list.
func (r *Reader) IsTable() bool {
	return r.at < len(r.buffer) && r.buffer[r.at]&tableFlag != 0
}

// Counted returns the element count of a list, map or table and a reader over
// what follows it.
func (r *Reader) Counted() (int, Reader, bool) {
	body, ok := r.compositeBody()
	if !ok {
		return 0, Reader{}, false
	}
	count, at, ok := readCount(body)
	if !ok {
		r.fail(ErrTruncated)
		return 0, Reader{}, false
	}
	return count, Reader{buffer: body[at:]}, true
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
	if code <= uintInlineMax {
		r.at++
		return uint64(code)
	}
	width := int(code) - uintWidthBase + 1
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
	if positive {
		return int64(magnitude)
	}
	return -int64(magnitude)
}

// elementMagnitude is signedMagnitude for a key-less element, which ElementInt
// needs for the same reason Int needs it: the signed and unsigned nibbles are
// different tables over the same four bits.
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

// MoreElements reports whether another key-less value follows.
func (r *Reader) MoreElements() bool { return r.err == nil && r.at < len(r.buffer) }

// Column writes an integer column under key, for a narrow-keyed table. It is
// Writer8.Column with four key bits: the column codec underneath is the same.
func (w *Writer) Column(key uint8, values []int64) {
	if len(values) == 0 || allZeroInts(values) {
		return
	}
	mark := w.openNarrowComposite(key, 0)
	w.Buffer = column.AppendArray(w.Buffer, values)
	w.Close(mark)
}

// Column reads a column written by Column. The row count comes from the table,
// which said it once for every column.
func (r *Reader) Column(rows int, dst []int64) []int64 {
	body, ok := r.compositeBody()
	if !ok {
		return dst
	}
	if cap(dst) < rows {
		dst = make([]int64, rows)
	}
	dst = dst[:rows]
	if _, err := column.DecodeArray(body, rows, dst); err != nil {
		r.fail(err)
		return nil
	}
	return dst
}

// Fail records an error from a sub-reader on its parent.
func (r *Reader) Fail(err error) {
	if err != nil {
		r.fail(err)
	}
}
