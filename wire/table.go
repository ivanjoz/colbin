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
	mark := w.openComposite(key, classMap, tableFlag)
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
	mark := w.openComposite(key, classCol, 0)
	w.Buffer = column.AppendArray(w.Buffer, values)
	w.Close(mark)
}

// allZero reports whether a column has nothing to write, which an empty one
// does not either.
func allZero(values []int64) bool {
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
// ordinary key run. As with Reader.Table, the row count is not bounded by the
// message and a caller allocating for it must budget rows itself.
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

func (r *Reader8) Column(rows int, dst []int64) []int64 {
	body, ok := r.compositeBody(classCol)
	if !ok {
		return dst
	}
	dst, err := decodeColumn(body, rows, dst)
	r.Fail(err)
	return dst
}

// decodeColumn decodes one column's body into dst, reusing its capacity.
func decodeColumn(body []byte, rows int, dst []int64) ([]int64, error) {
	if cap(dst) < rows {
		dst = make([]int64, rows)
	}
	dst = dst[:rows]
	if _, err := column.DecodeArray(body, rows, dst); err != nil {
		return nil, err
	}
	return dst, nil
}
