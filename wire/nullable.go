package wire

// The nullable column: a table column of a pointer field (`*int32`,
// `*string`, …) in which some rows are nil.
//
//	wide integer  [key] [0xB4 | lw] [length] [presence] [column]
//	wide string   [key] [0xCC | lw] [length] [presence] [elements]
//	narrow        [key | 1110]      [length] [presence] [column | elements]
//
// `presence` is a bit per row of the table (or group), ⌈rows/8⌉ bytes, bit
// row%8 of byte row/8 set where the row holds a value; bits past the last row
// are clear. Only the rows that hold one follow: `column` is the blocked column
// codec over them, `elements` their strings each behind its size, as a string
// column's are — so the count of either is the presence's population, and is
// never stored.
//
// It is the third form of a pointer column, and the writer's to choose:
//
//   - no row holds a value: the column is absent, which under a pointer field
//     means nil in every row;
//   - every row does: the ordinary column, written even when every value is
//     zero, since its absence would mean nil;
//   - some do: this.
//
// The wide forms set the detail bit COL and LIST left free (`[—:2][lw:2]`,
// `[homog:1][—:1][lw:2]`), and the narrow one the flag `10`, which a reader
// from before them refuses: its compositeBody admits `1100` and `1101` only.
// All are the length form, so a reader that does not know the key steps over
// one like any other field.

import (
	"errors"
	"math/bits"

	"github.com/ivanjoz/colbin/column"
)

// ErrBadPresence is a nullable column whose presence and values disagree.
var ErrBadPresence = errors.New("wire: a nullable column's presence does not match its values")

const (
	// nullableDetail is the wide COL and LIST detail bit of a nullable column.
	nullableDetail uint8 = 0b0100
	// narrowNullable is a narrow column's flags when it is nullable.
	narrowNullable uint8 = 0b10
)

// PresenceBytes is the size of a presence bitmap over rows rows.
func PresenceBytes(rows int) int { return (rows + 7) >> 3 }

// Present reports whether row holds a value in presence.
func Present(presence []byte, row int) bool { return presence[row>>3]&(1<<(row&7)) != 0 }

// ColumnKeep is Column for a pointer column in which every row holds a value:
// written even when every value is zero, because its absence would mean nil.
func (w *Writer8) ColumnKeep(key uint8, values []int64) {
	mark := w.openSized(key, classCol, 0, len(values))
	w.Buffer = column.AppendArray(w.Buffer, values)
	w.Close(mark)
}

// StringColumnKeep is StringColumnPayload for a pointer column in which every
// row holds a value, written even when every value is empty.
func (w *Writer8) StringColumnKeep(key uint8, values []string, payload int) {
	w.StringsPayload(key, values, payload)
}

// NullableColumn writes the integer column of a pointer field in which some rows
// are nil: their presence, then the values of the rows present.
func (w *Writer8) NullableColumn(key uint8, presence []byte, values []int64) {
	mark := w.openSized(key, classCol, nullableDetail, len(presence)+len(values))
	w.Buffer = column.AppendArray(append(w.Buffer, presence...), values)
	w.Close(mark)
}

// NullableStrings writes the string column of a pointer field in which some
// rows are nil: their presence, then the values of the rows present, payload
// bytes of elements (StringPayload).
func (w *Writer8) NullableStrings(key uint8, presence []byte, values []string, payload int) {
	mark := w.openSized(key, classList, listHomogeneous|nullableDetail, len(presence)+payload)
	w.Buffer = appendStringElements(append(w.Buffer, presence...), values, payload)
	w.Close(mark)
}

// ColumnKeep is Writer8.ColumnKeep with four key bits.
func (w *Writer) ColumnKeep(key uint8, values []int64) {
	mark := w.openNarrowSized(key, 0, len(values))
	w.Buffer = column.AppendArray(w.Buffer, values)
	w.Close(mark)
}

// StringColumnKeep is Writer8.StringColumnKeep with four key bits.
func (w *Writer) StringColumnKeep(key uint8, values []string, payload int) {
	w.StringsPayload(key, values, payload)
}

// NullableColumn is Writer8.NullableColumn with four key bits.
func (w *Writer) NullableColumn(key uint8, presence []byte, values []int64) {
	mark := w.openNarrowSized(key, narrowNullable, len(presence)+len(values))
	w.Buffer = column.AppendArray(append(w.Buffer, presence...), values)
	w.Close(mark)
}

// NullableStrings is Writer8.NullableStrings with four key bits.
func (w *Writer) NullableStrings(key uint8, presence []byte, values []string, payload int) {
	mark := w.openNarrowSized(key, narrowNullable, len(presence)+payload)
	w.Buffer = appendStringElements(append(w.Buffer, presence...), values, payload)
	w.Close(mark)
}

// IsNullable reports whether the field at the cursor is a nullable column.
func (r *Reader8) IsNullable() bool {
	if r.at+1 >= len(r.buffer) {
		return false
	}
	desc := r.buffer[r.at+1]
	class := desc >> 4 & 0b111
	return desc >= descExplicit && (class == classCol || class == classList) && desc&nullableDetail != 0
}

// IsNullable reports whether the field at the cursor is a nullable column.
func (r *Reader) IsNullable() bool {
	return r.at < len(r.buffer) && r.buffer[r.at]&0b1111 == nibbleLength|narrowNullable
}

// NullableColumn reads a nullable integer column of a table of rows rows: the
// presence, a sub-slice of the message, and the values of the rows present,
// decoded into dst's capacity.
func (r *Reader8) NullableColumn(rows int, dst []int64) ([]byte, []int64) {
	body, ok := r.compositeBody(classCol)
	if !ok {
		return nil, dst
	}
	presence, values, err := nullableColumn(r.buffer[r.at-len(body):], len(body), rows, dst)
	r.Fail(err)
	return presence, values
}

// NullableColumn is Reader8.NullableColumn with four key bits.
func (r *Reader) NullableColumn(rows int, dst []int64) ([]byte, []int64) {
	body, ok := r.nullableBody()
	if !ok {
		return nil, dst
	}
	presence, values, err := nullableColumn(r.buffer[r.at-len(body):], len(body), rows, dst)
	r.Fail(err)
	return presence, values
}

// nullableColumn splits a nullable integer column of size bytes, at the head
// of rest, into its presence and its values.
func nullableColumn(rest []byte, size, rows int, dst []int64) ([]byte, []int64, error) {
	presence, present, err := readPresence(rest[:size], rows)
	if err != nil {
		return nil, nil, err
	}
	at := len(presence)
	values, err := decodeColumn(rest[at:], size-at, present, dst)
	return presence, values, err
}

// readPresence reads a presence bitmap of rows rows at the head of body and
// counts the rows it says are present. It refuses a bitmap the body cannot
// hold, a bit set past the last row, and a column with no row present, which
// the writer leaves out.
func readPresence(body []byte, rows int) ([]byte, int, error) {
	size := PresenceBytes(rows)
	if size > len(body) {
		return nil, 0, ErrTruncated
	}
	presence := body[:size]
	if rows&7 != 0 && presence[size-1]>>(rows&7) != 0 {
		return nil, 0, ErrBadPresence
	}
	present := 0
	for _, b := range presence {
		present += bits.OnesCount8(b)
	}
	if present == 0 {
		return nil, 0, ErrBadPresence
	}
	return presence, present, nil
}

// NullableStrings reads a nullable string column of a table of rows rows: the
// presence, and the strings of the rows present appended to dst.
func (r *Reader8) NullableStrings(rows int, dst []string) ([]byte, []string) {
	body, ok := r.compositeBody(classList)
	if !ok {
		return nil, dst
	}
	presence, elements, present, ok := r.nullableElements(body, rows)
	if !ok {
		return nil, dst
	}
	dst, ok = elements.appendStrings(dst, present)
	if !ok || elements.More() {
		r.fail(errOr(elements.err, ErrBadPresence))
		return nil, dst
	}
	return presence, dst
}

// NullableStringsBytes is NullableStrings appending sub-slices of the message.
func (r *Reader8) NullableStringsBytes(rows int, dst [][]byte) ([]byte, [][]byte) {
	body, ok := r.compositeBody(classList)
	if !ok {
		return nil, dst
	}
	presence, elements, present, ok := r.nullableElements(body, rows)
	if !ok {
		return nil, dst
	}
	dst, ok = elements.appendBytes(dst, present)
	if !ok {
		r.fail(errOr(elements.err, ErrBadPresence))
		return nil, dst
	}
	return presence, dst
}

func (r *Reader8) nullableElements(body []byte, rows int) ([]byte, Reader, int, bool) {
	presence, present, err := readPresence(body, rows)
	if err != nil {
		r.fail(err)
		return nil, Reader{}, 0, false
	}
	return presence, Reader{buffer: body[len(presence):]}, present, true
}

// NullableStrings is Reader8.NullableStrings with four key bits.
func (r *Reader) NullableStrings(rows int, dst []string) ([]byte, []string) {
	body, ok := r.nullableBody()
	if !ok {
		return nil, dst
	}
	presence, present, err := readPresence(body, rows)
	if err != nil {
		r.fail(err)
		return nil, dst
	}
	elements := Reader{buffer: body[len(presence):]}
	dst, ok = elements.appendStrings(dst, present)
	if !ok || elements.More() {
		r.fail(errOr(elements.err, ErrBadPresence))
		return nil, dst
	}
	return presence, dst
}

// NullableStringsBytes is NullableStrings appending sub-slices of the message.
func (r *Reader) NullableStringsBytes(rows int, dst [][]byte) ([]byte, [][]byte) {
	body, ok := r.nullableBody()
	if !ok {
		return nil, dst
	}
	presence, present, err := readPresence(body, rows)
	if err != nil {
		r.fail(err)
		return nil, dst
	}
	elements := Reader{buffer: body[len(presence):]}
	dst, ok = elements.appendBytes(dst, present)
	if !ok {
		r.fail(errOr(elements.err, ErrBadPresence))
		return nil, dst
	}
	return presence, dst
}

// nullableBody reads a narrow nullable column's framing and returns its body,
// advancing past the whole field.
func (r *Reader) nullableBody() ([]byte, bool) {
	nibble, start, size, ok := r.field()
	if !ok {
		return nil, false
	}
	if nibble != nibbleLength|narrowNullable {
		r.fail(ErrBadEscape)
		return nil, false
	}
	return r.buffer[start : start+size], true
}

// appendBytes appends exactly count elements of r, which must be all of them,
// as sub-slices of its buffer.
func (r *Reader) appendBytes(dst [][]byte, count int) ([][]byte, bool) {
	for range count {
		element, ok := r.Element()
		if !ok {
			return dst, false
		}
		dst = append(dst, element)
	}
	return dst, !r.More()
}

func errOr(err, otherwise error) error {
	if err != nil {
		return err
	}
	return otherwise
}
