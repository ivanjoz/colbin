package wire

// The columnar shape, as a field class rather than a mode.
//
//	table  [key][desc][bytelen][rowcount]  ( [key][desc][payload] )*
//
// A table's body is a key run like a struct's, except that each field is a
// *column* of rowcount values rather than one value: the key a row-wise list
// spends once per field per element is spent once per column.
//
// So an array of structs has two encodings and the writer picks, per field:
//
//   - a LIST of STRUCT, a key run per element, which wins while there are too
//     few rows to amortise the per-column framing;
//   - a TABLE, transposed, whose integer columns decode through the blocked
//     column codec at about a nanosecond per element.
//
// A column whose every value is zero is omitted entirely, and the reader takes
// an absent column key to mean exactly that.

import (
	"github.com/ivanjoz/colbin/column"
)

// tableFlag is the MAP class's sub bit: clear is a map, set is a table. They
// share a class because they are the same shape — a length, a count and a body —
// and differ only in what the body holds.
const tableFlag uint8 = 0b1000

// OpenTable begins a table of rows rows under key. Write one column per field
// with the column writers below, then Close.
func (w *Writer8) OpenTable(key uint8, rows int) Mark {
	// A row is at least a byte of some column, near enough to size the length.
	mark := w.openSized(key, classMap, tableFlag, rows)
	w.Buffer = appendCount(w.Buffer, rows)
	return mark
}

// Column writers. A column is an ordinary keyed field whose payload is the
// blocked column codec's output — the same codec an array field uses, reached
// here without the element count, which the table's rowcount already gives.

// Column writes an integer column under key, and nothing at all when every value
// is zero: an absent column key means a column of zeros.
func (w *Writer8) Column(key uint8, values []int64) {
	if allZero(values) {
		return
	}
	mark := w.openSized(key, classCol, 0, len(values))
	w.Buffer = column.AppendArray(w.Buffer, values)
	w.Close(mark)
}

// Column32 is Column for a column held as int32.
func (w *Writer8) Column32(key uint8, values []int32) {
	if allZero(values) {
		return
	}
	mark := w.openSized(key, classCol, 0, len(values))
	w.Buffer = column.AppendArray(w.Buffer, values)
	w.Close(mark)
}

// allZero reports whether a column has nothing to write, which an empty one
// does not either.
func allZero[T int32 | int64](values []T) bool {
	for _, value := range values {
		if value != 0 {
			return false
		}
	}
	return true
}

// StringColumn writes a string column, which is a list of blobs under the
// column's key. Strings have no residual to transform, so a column of them is
// the same shape a list of them is.
//
// A column of nothing but empty strings is omitted, for the same reason an
// all-zero integer column is: its absence already says so, and writing it would
// cost a byte per row to say nothing.
func (w *Writer8) StringColumn(key uint8, values []string) {
	if allEmpty(values) {
		return
	}
	w.Strings(key, values)
}

// StringColumnPayload is StringColumn for a caller that summed the payload
// as it gathered the values (StringPayload). A column of empty strings has
// exactly a byte of size for each, and is omitted.
func (w *Writer8) StringColumnPayload(key uint8, values []string, payload int) {
	if payload == len(values) {
		return
	}
	w.StringsPayload(key, values, payload)
}

// allEmpty is allZero for a string column.
func allEmpty(values []string) bool {
	for _, value := range values {
		if len(value) != 0 {
			return false
		}
	}
	return true
}

// Table returns the row count, the rows per group and the body: for a table
// written whole, groupRows 0 and the column run, an ordinary key run for
// NewReader8; for a grouped one, its groups (groups.go). As with Reader.Table,
// the row count is not bounded by the message and a caller allocating for it
// must budget rows itself.
func (r *Reader8) Table() (rows, groupRows int, body []byte, ok bool) {
	if r.at+2 > len(r.buffer) {
		r.fail(ErrTruncated)
		return 0, 0, nil, false
	}
	desc := r.buffer[r.at+1]
	if desc&tableFlag == 0 {
		r.fail(ErrBadDescriptor)
		return 0, 0, nil, false
	}
	whole, ok := r.compositeBody(classMap)
	if !ok {
		return 0, 0, nil, false
	}
	rows, groupRows, body, err := tableHead(whole, desc&groupedFlag != 0)
	if err != nil {
		r.fail(err)
		return 0, 0, nil, false
	}
	return rows, groupRows, body, true
}

// Column readers. Each takes the row count, because a column does not carry its
// own — the table said it once for every column.

func (r *Reader8) Column(rows int, dst []int64) []int64 {
	if r.IsNullable() {
		// Its values are fewer than the rows; NullableColumn reads it.
		r.fail(ErrBadDescriptor)
		return dst
	}
	body, ok := r.compositeBody(classCol)
	if !ok {
		return dst
	}
	dst, err := decodeColumn(r.buffer[r.at-len(body):], len(body), rows, dst)
	r.Fail(err)
	return dst
}

// decodeColumn decodes the column of size bytes that rest opens with into dst,
// reusing its capacity.
//
// It is handed the rest of the message rather than the column alone because
// the column codec reads a value with one eight-byte load wherever eight bytes
// follow it, and gathers the bytes one at a time where they do not. A table's
// columns are short more often than not, so cut at their end every one of them
// took the slow read throughout: a third of a 16-row column's decode. What
// follows the column is only ever loaded and masked off, never decoded, and a
// column that claims more than its size is refused here, as it was by the
// codec when it had only its own bytes.
func decodeColumn(rest []byte, size, rows int, dst []int64) ([]int64, error) {
	if cap(dst) < rows {
		dst = make([]int64, rows)
	}
	dst = dst[:rows]
	used, err := column.DecodeArray(rest, rows, dst)
	if err == nil && used > size {
		err = ErrTruncated
	}
	if err != nil {
		return nil, err
	}
	return dst, nil
}
