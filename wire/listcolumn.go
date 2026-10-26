package wire

// The list column: a table's []Struct field, as columns.
//
// A row that holds a slice of structs — an order and its lines — was not a
// column of anything, so a slice of such rows stayed a list of records, each
// with its own short list inside, however many there were. The list column
// writes the field the way a columnar store does: how many elements each row
// holds, and then every row's elements one after another as a table of their
// own, so the lines of every order are one table rather than a list per order.
//
//	wide    [key] [COL | 1000 | lw] [length] [children] [lengths] [columns]
//	narrow  [key | 1100]            [length] [children] [lengths] [columns]
//
// `children` is a count, the elements in all; `lengths` the blocked column
// codec over the rows' element counts, one per row, which is as long as it
// decodes to; and `columns` the element table's column run, to the end of the
// field, keyed at the width a table of the element type takes there. The
// counts add up to `children`, which a reader checks before it allocates.
//
// An empty slice and a nil one are the same here, as they are in a record,
// where an empty slice is not written.
//
// Neither form meant anything before under a slice's key in a table: a wide
// COL's detail bit 3 was clear everywhere, and a []Struct was never a column.

import (
	"errors"

	"github.com/ivanjoz/colbin/column"
)

// listColumnDetail is the wide COL detail bit of a list column.
const listColumnDetail uint8 = 0b1000

// ErrBadLengths is a list column whose lengths do not add up to its elements.
var ErrBadLengths = errors.New("wire: a list column's lengths do not add up to its elements")

// OpenListColumn begins the list column of a table of len(lengths) rows whose
// rows hold children elements between them. Write the element table's columns
// after it, then Close.
func (w *Writer8) OpenListColumn(key uint8, children int, lengths []int64) Mark {
	mark := w.openSized(key, classCol, listColumnDetail, children+len(lengths))
	w.Buffer = column.AppendArray(appendCount(w.Buffer, children), lengths)
	return mark
}

// OpenListColumn is Writer8.OpenListColumn with four key bits.
func (w *Writer) OpenListColumn(key uint8, children int, lengths []int64) Mark {
	mark := w.openNarrowSized(key, 0, children+len(lengths))
	w.Buffer = column.AppendArray(appendCount(w.Buffer, children), lengths)
	return mark
}

// ListColumn reads a list column of a table of rows rows: the elements in all,
// each row's count of them, decoded into dst's capacity, and the element
// table's column run, a sub-slice of the message.
func (r *Reader8) ListColumn(rows int, dst []int64) (int, []int64, []byte, bool) {
	if r.at+2 > len(r.buffer) {
		r.fail(ErrTruncated)
		return 0, nil, nil, false
	}
	if desc := r.buffer[r.at+1]; desc>>4 != 0b1000|classCol || desc&listColumnDetail == 0 {
		r.fail(ErrBadDescriptor)
		return 0, nil, nil, false
	}
	body, ok := r.compositeBody(classCol)
	if !ok {
		return 0, nil, nil, false
	}
	return r.listColumn(body, rows, dst)
}

func (r *Reader8) listColumn(body []byte, rows int, dst []int64) (int, []int64, []byte, bool) {
	children, lengths, run, err := listColumn(body, rows, dst)
	if err != nil {
		r.fail(err)
		return 0, nil, nil, false
	}
	return children, lengths, run, true
}

// ListColumn is Reader8.ListColumn with four key bits.
func (r *Reader) ListColumn(rows int, dst []int64) (int, []int64, []byte, bool) {
	nibble, start, size, ok := r.field()
	if !ok {
		return 0, nil, nil, false
	}
	if nibble != nibbleLength {
		r.fail(ErrBadEscape)
		return 0, nil, nil, false
	}
	children, lengths, run, err := listColumn(r.buffer[start:start+size], rows, dst)
	if err != nil {
		r.fail(err)
		return 0, nil, nil, false
	}
	return children, lengths, run, true
}

// listColumn splits a list column's body.
func listColumn(body []byte, rows int, dst []int64) (int, []int64, []byte, error) {
	children, at, ok := readCount(body)
	if !ok {
		return 0, nil, nil, ErrTruncated
	}
	if cap(dst) < rows {
		dst = make([]int64, rows)
	}
	lengths := dst[:rows]
	used, err := column.DecodeArray(body[at:], rows, lengths)
	if err != nil {
		return 0, nil, nil, err
	}
	if used > len(body)-at {
		return 0, nil, nil, ErrTruncated
	}
	left := children
	for _, length := range lengths {
		if length < 0 || length > int64(left) {
			return 0, nil, nil, ErrBadLengths
		}
		left -= int(length)
	}
	if left != 0 {
		return 0, nil, nil, ErrBadLengths
	}
	return children, lengths, body[at+used:], nil
}
