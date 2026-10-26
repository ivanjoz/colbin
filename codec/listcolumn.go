package codec

// List columns: a table whose row holds a slice of structs.
//
// TPC-H's nested shape is an order holding its lines: a []Order of 150 000 rows
// each with one to seven LineItem. The order was not a column of anything, so the
// orders went out as records and each order's lines as a list of records of their
// own, too short to be tables — every field name a key per line, every string
// its own allocation on the way back.
//
// A []Struct field of a row is a column now (wire/listcolumn.go): each row's
// count, and then every row's elements as one table. The lines of every order
// are one table of 600 000 rows, transposed, dictionaried and decoded like
// TPC-H's normalized lineitem — and decoded into one array, which each order's
// slice is a window of, rather than an allocation per order.
//
// It nests: the element type may hold a slice of structs itself, which is a list
// column of the element table. A type that holds a slice of itself never ends,
// so it is not a table (transposable).

import (
	"unsafe"

	"github.com/ivanjoz/colbin/wire"
)

// maxListDepth is how many list columns deep a table may go: an order's lines'
// parts is two. It bounds transposable on a type that holds a slice of itself.
const maxListDepth = 4

// listsColumn says a field is a slice of structs a table writes as a list
// column: one held directly, of a type that is a table itself.
func (field *planField) listsColumn() bool {
	return field.op == opStructs && !field.indirect && field.sub != nil
}

// listBuffers is one level of list column's scratch: the rows' element counts,
// and their elements gathered into one run, as a table's rows are laid out.
type listBuffers struct {
	lengths []int64
	// rows holds the elements as bytes, copied out of each row's slice. It is
	// pointer-free memory holding the elements' pointers, which is sound for as
	// long as the encode runs: every one of them is held by the value as well.
	rows []uint64
}

// listLevel is the scratch of the list column depth levels down.
func (columns *columnBuffers) listLevel(depth int) *listBuffers {
	for len(columns.lists) <= depth {
		columns.lists = append(columns.lists, listBuffers{})
	}
	return &columns.lists[depth]
}

// appendListColumns writes a table's list columns, after its other columns,
// whose scratch the element tables gather into. wide says the table's columns
// are keyed at eight bits.
func appendListColumns(
	buffer []byte, plan *typePlan, slice *sliceHeader, stride uintptr, wide bool, buf *scratch, dict *dictionaries,
) []byte {
	for index := range plan.fields {
		if field := &plan.fields[index]; field.listsColumn() {
			buffer = appendListColumn(buffer, field, slice, stride, wide, buf, dict)
		}
	}
	return buffer
}

func appendListColumn(
	buffer []byte, field *planField, slice *sliceHeader, stride uintptr, wide bool, buf *scratch, dict *dictionaries,
) []byte {
	if !buf.enter() {
		return buffer
	}
	defer buf.leave()
	rows := slice.len
	level := buf.reserve(0, false).listLevel(buf.depth)
	if cap(level.lengths) < rows {
		level.lengths = make([]int64, rows)
	}
	lengths := level.lengths[:rows]
	children := 0
	base := unsafe.Add(slice.data, field.offset)
	for row := range rows {
		n := (*sliceHeader)(unsafe.Add(base, uintptr(row)*stride)).len
		lengths[row] = int64(n)
		children += n
	}
	if children == 0 {
		return buffer
	}
	size := uintptr(children) * field.stride
	if words := int((size + 7) / 8); cap(level.rows) < words {
		level.rows = make([]uint64, words)
	}
	elements := unsafe.Pointer(unsafe.SliceData(level.rows))
	at := uintptr(0)
	for row := range rows {
		held := (*sliceHeader)(unsafe.Add(base, uintptr(row)*stride))
		bytes := uintptr(held.len) * field.stride
		copy(unsafe.Slice((*byte)(unsafe.Add(elements, at)), bytes), unsafe.Slice((*byte)(held.data), bytes))
		at += bytes
	}
	elementRows := sliceHeader{data: elements, len: children}
	// The element table's keys are as wide as a table of the element type's
	// would be here: eight bits under eight, and the type's own under four.
	inner := wide || field.sub.isWide
	if wide {
		writer := wire.Writer8{Buffer: buffer}
		mark := writer.OpenListColumn(field.key, children, lengths)
		writer.Buffer = appendElementColumns(writer.Buffer, field, &elementRows, inner, buf, dict)
		writer.Close(mark)
		return writer.Buffer
	}
	writer := wire.Writer{Buffer: buffer}
	mark := writer.OpenListColumn(field.key, children, lengths)
	writer.Buffer = appendElementColumns(writer.Buffer, field, &elementRows, inner, buf, dict)
	writer.Close(mark)
	return writer.Buffer
}

// appendElementColumns writes the element table's columns at the key width.
func appendElementColumns(buffer []byte, field *planField, elements *sliceHeader, wide bool, buf *scratch, dict *dictionaries) []byte {
	if wide {
		writer := wire.Writer8{Buffer: buffer}
		appendColumns(&writer, field.sub, elements, field.stride, buf, dict)
		return writer.Buffer
	}
	writer := wire.Writer{Buffer: buffer}
	appendNarrowColumns(&writer, field.sub, elements, field.stride, buf)
	return writer.Buffer
}

// listColumnReader is what a list column is read through, at either key width.
type listColumnReader interface {
	ListColumn(rows int, dst []int64) (int, []int64, []byte, bool)
	Fail(err error)
}

// readListColumn reads a list column of a table of rows rows from data on into
// field, a []Struct of each row: the elements into one array, and each row's
// slice a window of it, capped at its own end. wide says the table's columns are
// keyed at eight bits.
func readListColumn(columns listColumnReader, field *planField, stride uintptr, data unsafe.Pointer, rows int, wide bool, buf *scratch) {
	if !buf.enter() {
		columns.Fail(buf.err)
		return
	}
	defer buf.leave()
	level := buf.reserve(0, false).listLevel(buf.depth)
	children, lengths, run, ok := columns.ListColumn(rows, level.lengths[:0])
	if !ok {
		return
	}
	level.lengths = lengths[:0]
	if err := takeRows(&buf.rowsLeft, children, len(field.sub.fields)); err != nil {
		columns.Fail(err)
		return
	}
	var elements sliceHeader
	if children != 0 {
		newSlice(field, unsafe.Pointer(&elements), children)
	}
	inner := wide || field.sub.isWide
	if err := readTableColumns(field, elements.data, children, run, inner, buf); err != nil {
		columns.Fail(err)
		return
	}
	base := unsafe.Add(data, field.offset)
	at := uintptr(0)
	for row, n := range lengths {
		held := (*sliceHeader)(unsafe.Add(base, uintptr(row)*stride))
		if n == 0 {
			*held = sliceHeader{}
			continue
		}
		*held = sliceHeader{data: unsafe.Add(elements.data, at), len: int(n), cap: int(n)}
		at += uintptr(n) * field.stride
	}
}
