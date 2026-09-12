package wire

// The columnar shape, as a field class rather than a mode.
//
//	table  [key][desc][bytelen][rowcount]  ( [key][desc][payload] )*
//
// A table's body is a key run like a struct's, except that each field is a
// *column* of rowcount values rather than one value. That is the whole of what
// the old standard mode was: the key that a row-wise list spends once per field
// per element is spent once per column.
//
// So an array of structs has two encodings and the writer picks:
//
//   - a LIST of STRUCT, a key run per element, which wins while there are too
//     few rows to amortise the per-column framing;
//   - a TABLE, transposed, which wins from some threshold upward and whose
//     columns decode through the blocked column codec at about a nanosecond per
//     element rather than the twenty-odd a key run costs per record.
//
// The threshold is a measurement, not a constant to argue about, and it is now a
// *per-field* decision rather than a per-message one: a record can hold a
// three-element list and a ten-thousand-row table and encode each the right way.
//
// # A column of nothing is not written
//
// A column whose every value is zero is omitted entirely, and the reader takes
// an absent column key to mean exactly that. There is no flag for it and no
// mode to turn it on, which is what the old format's omit-empty version byte
// was: the columns are keyed, so absence already says it.

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
	mark := w.openComposite(key, classMap, tableFlag)
	w.Buffer = appendCount(w.Buffer, rows)
	return mark
}

// Column writers. A column is an ordinary keyed field whose payload is the
// blocked column codec's output — the same codec an array field uses, reached
// here without the element count, which the table's rowcount already gives.

// Column writes an integer column under key, and nothing at all when every value
// is zero: an absent column key means a column of zeros.
func (w *Writer8) Column(key uint8, values []int64) { writeColumn(w, key, values) }

// Column8, Column16 and Column32 are Column for the slice a caller holds.
func (w *Writer8) Column8(key uint8, values []int8)   { writeColumn(w, key, values) }
func (w *Writer8) Column16(key uint8, values []int16) { writeColumn(w, key, values) }
func (w *Writer8) Column32(key uint8, values []int32) { writeColumn(w, key, values) }

func writeColumn[T column.Signed](w *Writer8, key uint8, values []T) {
	if len(values) == 0 || allZero(values) {
		return
	}
	mark := w.openComposite(key, classCol, 0)
	w.Buffer = column.AppendArray(w.Buffer, values)
	w.Close(mark)
}

// allZeroInts is allZero for the []int64 a narrow column gathers into.
func allZeroInts(values []int64) bool { return allZero(values) }

func allZero[T column.Signed](values []T) bool {
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
	for _, value := range values {
		if len(value) != 0 {
			w.Strings(key, values)
			return
		}
	}
}

// Table returns the row count and a reader over the columns, which are an
// ordinary key run.
func (r *Reader8) Table() (int, Reader8, bool) {
	if r.at+2 > len(r.buffer) {
		r.fail(ErrTruncated)
		return 0, Reader8{}, false
	}
	if r.buffer[r.at+1]&tableFlag == 0 {
		r.fail(ErrBadDescriptor)
		return 0, Reader8{}, false
	}
	body, ok := r.compositeBody(classMap)
	if !ok {
		return 0, Reader8{}, false
	}
	rows, at, ok := readCount(body)
	if !ok {
		r.fail(ErrTruncated)
		return 0, Reader8{}, false
	}
	// The columns are keyed, so unlike a list's elements they read as an
	// ordinary run and need no cursor offset.
	return rows, Reader8{buffer: body[at:]}, true
}

// Column readers. Each takes the row count, because a column does not carry its
// own — the table said it once for every column.

func (r *Reader8) Column(rows int, dst []int64) []int64 { return readColumn(r, rows, dst) }
func (r *Reader8) Column8(rows int, dst []int8) []int8  { return readColumn(r, rows, dst) }
func (r *Reader8) Column16(rows int, dst []int16) []int16 {
	return readColumn(r, rows, dst)
}
func (r *Reader8) Column32(rows int, dst []int32) []int32 {
	return readColumn(r, rows, dst)
}

func readColumn[T column.Signed](r *Reader8, rows int, dst []T) []T {
	body, ok := r.compositeBody(classCol)
	if !ok {
		return dst
	}
	if cap(dst) < rows {
		dst = make([]T, rows)
	}
	dst = dst[:rows]
	if _, err := column.DecodeArray(body, rows, dst); err != nil {
		r.fail(err)
		return nil
	}
	return dst
}
