package codec

// Pointer columns: a table of rows with a `*int32`, a `*string`, a `*float64`
// field.
//
// A pointer to a scalar is how Go says a value may be absent, which is what
// every nullable column of a database table is. Before this, a row with one was
// not columnable and its slice stayed a list however long it got — and a real
// table has a nullable column more often than not: 44 of the Public BI
// Benchmark's 46 first tables do.
//
// So a pointer field is a column like any other, in one of three forms the
// writer picks per column (wire/nullable.go): absent when every row is nil,
// the ordinary column when none is — written even when every value is zero, as
// its absence would mean nil — and otherwise a nullable column, a bit of
// presence per row and then the values of the rows present only. A column
// whose rows are all present costs exactly what the plain field's would.
//
// The values are gathered densely, so the column codec sees only real values
// and a run of nils costs a bit a row. Decoding allocates one array per column
// and points each present row into it, rather than a pointee per row.

import (
	"math"
	"unsafe"

	"github.com/ivanjoz/colbin/wire"
)

// nullColumn is one pointer column as gathered: how many rows hold a value,
// which (presence), and those values, densely.
type nullColumn struct {
	present  int
	presence []byte
	ints     []int64
	strings  []string
	// payload is what the strings take as a list's elements.
	payload int
}

// pointerColumns is how many of a row type's fields are pointers to integers
// (or bools, or floats) and how many are pointers to strings.
func (plan *typePlan) pointerColumns() (ints, strings int) {
	for index := range plan.fields {
		if field := &plan.fields[index]; field.op == opPointer {
			if field.elemOp == opString {
				strings++
			} else {
				ints++
			}
		}
	}
	return ints, strings
}

// gatherPointers gathers a table's pointer columns into the buffer's nulls, in
// field order. A table with none leaves them empty.
func gatherPointers(plan *typePlan, slice *sliceHeader, stride uintptr, buf *scratch) {
	if nulls := preparePointers(plan, slice.len, buf); len(nulls) != 0 {
		gatherPointerChunk(plan, nulls, slice.data, stride, 0, slice.len)
	}
}

// preparePointers cuts a table's pointer columns out of the buffer, empty, for
// gatherPointerChunk to fill a chunk of rows at a time. A table with none
// leaves the buffer's nulls empty.
func preparePointers(plan *typePlan, rows int, buf *scratch) []nullColumn {
	columns := buf.columns
	columns.nulls = columns.nulls[:0]
	intColumns, stringColumns := plan.pointerColumns()
	if intColumns+stringColumns == 0 {
		return nil
	}
	size := wire.PresenceBytes(rows)
	columns.reservePointers(rows*intColumns, rows*stringColumns, size*(intColumns+stringColumns))
	ints, strings := columns.nullInts[:rows*intColumns], columns.nullStrings[:rows*stringColumns]
	presence := columns.presence[:size*(intColumns+stringColumns)]
	clear(presence)
	for index := range plan.fields {
		field := &plan.fields[index]
		if field.op != opPointer {
			continue
		}
		column := nullColumn{presence: presence[:size:size]}
		presence = presence[size:]
		if field.elemOp == opString {
			column.strings = strings[:0:rows]
			strings = strings[rows:]
		} else {
			column.ints = ints[:0:rows]
			ints = ints[rows:]
		}
		columns.nulls = append(columns.nulls, column)
	}
	return columns.nulls
}

// gatherChunkAligned holds gatherChunk to a multiple of eight rows, which is
// what lets a chunk's presence start on a byte.
var _ = [1]struct{}{}[gatherChunk%8]

// gatherPointerChunk gathers rows first to first+count of every pointer column
// onto what the columns hold already. data is the first of those rows.
//
// A chunk at a time for gatherTable's reason: a pointer column read whole
// walks every row of the table, and then every pointee, once per column.
func gatherPointerChunk(plan *typePlan, nulls []nullColumn, data unsafe.Pointer, stride uintptr, first, count int) {
	for index := range plan.fields {
		field := &plan.fields[index]
		if field.op != opPointer {
			continue
		}
		column := &nulls[0]
		nulls = nulls[1:]
		base := unsafe.Add(data, field.offset)
		// A chunk starts on a whole byte of presence: see gatherChunkAligned.
		presence := column.presence[first>>3:]
		if field.elemOp == opString {
			var payload int
			column.strings, payload = gatherPointerStrings(column.strings, presence, base, stride, count)
			column.payload += payload
		} else {
			column.ints = gatherPointerInts(column.ints, presence, base, stride, count, field.elemOp)
		}
		column.present = max(len(column.ints), len(column.strings))
	}
}

// reservePointers grows the pointer columns' buffers.
func (columns *columnBuffers) reservePointers(ints, strings, presence int) {
	columns.usedNullStrings = max(columns.usedNullStrings, strings)
	if cap(columns.nullInts) < ints {
		columns.nullInts = make([]int64, ints)
	}
	if cap(columns.nullStrings) < strings {
		columns.nullStrings = make([]string, strings)
	}
	if cap(columns.presence) < presence {
		columns.presence = make([]byte, presence)
	}
}

// gatherPointerInts gathers the pointees of one column of len(dst) rows into
// dst, densely, as gatherInts widens them, setting presence for each row that
// has one. It returns what it gathered.
func gatherPointerInts(dst []int64, presence []byte, base unsafe.Pointer, stride uintptr, count int, op fieldOp) []int64 {
	switch op {
	case opBool:
		return gatherPointees(dst, presence, base, stride, count, func(v bool) int64 {
			if v {
				return 1
			}
			return 0
		})
	case opInt8:
		return gatherPointees(dst, presence, base, stride, count, func(v int8) int64 { return int64(v) })
	case opInt16:
		return gatherPointees(dst, presence, base, stride, count, func(v int16) int64 { return int64(v) })
	case opInt32:
		return gatherPointees(dst, presence, base, stride, count, func(v int32) int64 { return int64(v) })
	case opInt64:
		return gatherPointees(dst, presence, base, stride, count, func(v int64) int64 { return v })
	case opUint8:
		return gatherPointees(dst, presence, base, stride, count, func(v uint8) int64 { return int64(v) })
	case opUint16:
		return gatherPointees(dst, presence, base, stride, count, func(v uint16) int64 { return int64(v) })
	case opUint32:
		return gatherPointees(dst, presence, base, stride, count, func(v uint32) int64 { return int64(v) })
	case opUint64:
		return gatherPointees(dst, presence, base, stride, count, func(v uint64) int64 { return int64(v) })
	case opFloat32:
		return gatherPointees(dst, presence, base, stride, count, func(v float32) int64 { return int64(math.Float32bits(v)) })
	case opFloat64:
		return gatherPointees(dst, presence, base, stride, count, func(v float64) int64 { return int64(math.Float64bits(v)) })
	}
	return dst[:0]
}

// gatherPointees appends to dst the pointees of count rows from base, and sets
// presence, which starts at the first of those rows, for each row that has
// one. dst has room for every row of the table.
//
// It has to stay inside the inliner's budget: out of line, widen is a call per
// value.
func gatherPointees[T any](dst []int64, presence []byte, base unsafe.Pointer, stride uintptr, count int, widen func(T) int64) []int64 {
	n := len(dst)
	dst = dst[:cap(dst)]
	for row := range count {
		if pointee := *(**T)(unsafe.Add(base, uintptr(row)*stride)); pointee != nil {
			presence[row>>3] |= 1 << (row & 7)
			dst[n] = widen(*pointee)
			n++
		}
	}
	return dst[:n]
}

func gatherPointerStrings(dst []string, presence []byte, base unsafe.Pointer, stride uintptr, count int) ([]string, int) {
	n, payload := len(dst), 0
	dst = dst[:cap(dst)]
	for row := range count {
		if pointee := *(**string)(unsafe.Add(base, uintptr(row)*stride)); pointee != nil {
			presence[row>>3] |= 1 << (row & 7)
			dst[n] = *pointee
			payload += wire.StringPayload(len(*pointee))
			n++
		}
	}
	return dst[:n], payload
}

// writePointerColumn writes a gathered pointer column of a table of rows rows
// in the form it takes: nothing when no row holds a value, the plain column
// when every one does, and a nullable column otherwise. A string column that
// tableIDs took, choice, goes out as ids in the same three forms.
//
// It is spelled once per key width rather than over columnWriter: a writer
// handed to an interface escapes, and the table's was then allocated on every
// encode of a table with a pointer column.
func writePointerColumn(writer *wire.Writer, key uint8, column *nullColumn, rows int, elemOp fieldOp, choice *dictColumn) {
	float := elemOp == opFloat64
	switch {
	case column.present == 0:
	case choice != nil && !choice.refused && column.present == rows:
		writer.DictColumn(key, choice.number, choice.ids)
	case choice != nil && !choice.refused:
		writer.NullableDictColumn(key, choice.number, column.presence, choice.ids)
	case column.present == rows && elemOp == opString:
		writer.StringColumnKeep(key, column.strings, column.payload)
	case column.present == rows:
		writer.ColumnKeep(key, column.ints, float)
	case elemOp == opString:
		writer.NullableStrings(key, column.presence, column.strings, column.payload)
	default:
		writer.NullableColumn(key, column.presence, column.ints, float)
	}
}

// writePointerColumnWide is writePointerColumn at eight key bits.
func writePointerColumnWide(writer *wire.Writer8, key uint8, column *nullColumn, rows int, elemOp fieldOp, choice *dictColumn) {
	float := elemOp == opFloat64
	switch {
	case column.present == 0:
	case choice != nil && !choice.refused && column.present == rows:
		writer.DictColumn(key, choice.number, choice.ids)
	case choice != nil && !choice.refused:
		writer.NullableDictColumn(key, choice.number, column.presence, choice.ids)
	case column.present == rows && elemOp == opString:
		writer.StringColumnKeep(key, column.strings, column.payload)
	case column.present == rows:
		writer.ColumnKeep(key, column.ints, float)
	case elemOp == opString:
		writer.NullableStrings(key, column.presence, column.strings, column.payload)
	default:
		writer.NullableColumn(key, column.presence, column.ints, float)
	}
}

// pointerColumnReader is what a pointer column is read through, at either key
// width.
type pointerColumnReader interface {
	IsNullable() bool
	Column(rows int, dst []int64) []int64
	Strings(dst []string) []string
	NullableColumn(rows int, dst []int64) ([]byte, []int64)
	NullableStrings(rows int, dst []string) ([]byte, []string)
	Err() error
	Fail(err error)
}

// readPointerColumn reads a pointer column of rows rows and points each row
// that holds a value into one array allocated for the column. The rows were
// zeroed before the table was read, so a row with none stays nil.
func readPointerColumn(reader pointerColumnReader, field *planField, data unsafe.Pointer, stride uintptr, rows int, buf *scratch) {
	var presence []byte
	if field.elemOp == opString {
		var values []string
		if reader.IsNullable() {
			presence, values = reader.NullableStrings(rows, nil)
		} else if values = reader.Strings(make([]string, 0, rows)); len(values) != rows && reader.Err() == nil {
			reader.Fail(errColumnLength)
		}
		if reader.Err() == nil {
			pointRows(values, presence, data, stride, field.offset, rows)
		}
		return
	}
	// The pointer columns' own buffer, so as not to land on a column the table
	// has decoded and not yet scattered.
	columns := buf.reserve(0, false)
	columns.reservePointers(rows, 0, 0)
	scratch := columns.nullInts[:0:rows]
	var values []int64
	if reader.IsNullable() {
		presence, values = reader.NullableColumn(rows, scratch)
	} else {
		values = reader.Column(rows, scratch)
	}
	if reader.Err() != nil {
		return
	}
	switch field.elemOp {
	case opBool:
		pointRows(narrowed(values, func(v int64) bool { return v == 1 }), presence, data, stride, field.offset, rows)
	case opInt8:
		pointRows(narrowed(values, func(v int64) int8 { return int8(v) }), presence, data, stride, field.offset, rows)
	case opInt16:
		pointRows(narrowed(values, func(v int64) int16 { return int16(v) }), presence, data, stride, field.offset, rows)
	case opInt32:
		pointRows(narrowed(values, func(v int64) int32 { return int32(v) }), presence, data, stride, field.offset, rows)
	case opInt64:
		pointRows(narrowed(values, func(v int64) int64 { return v }), presence, data, stride, field.offset, rows)
	case opUint8:
		pointRows(narrowed(values, func(v int64) uint8 { return uint8(v) }), presence, data, stride, field.offset, rows)
	case opUint16:
		pointRows(narrowed(values, func(v int64) uint16 { return uint16(v) }), presence, data, stride, field.offset, rows)
	case opUint32:
		pointRows(narrowed(values, func(v int64) uint32 { return uint32(v) }), presence, data, stride, field.offset, rows)
	case opUint64:
		pointRows(narrowed(values, func(v int64) uint64 { return uint64(v) }), presence, data, stride, field.offset, rows)
	case opFloat32:
		pointRows(narrowed(values, func(v int64) float32 { return math.Float32frombits(uint32(v)) }), presence, data, stride, field.offset, rows)
	case opFloat64:
		pointRows(narrowed(values, func(v int64) float64 { return math.Float64frombits(uint64(v)) }), presence, data, stride, field.offset, rows)
	}
}

// narrowed is the column's values as their Go type, in an array of their own:
// the one every row of the column will point into.
func narrowed[T any](values []int64, convert func(int64) T) []T {
	out := make([]T, len(values))
	for index, value := range values {
		out[index] = convert(value)
	}
	return out
}

// pointRows points each of rows rows that holds a value, in order, at the next
// of values: every row when presence is nil.
func pointRows[T any](values []T, presence []byte, data unsafe.Pointer, stride, offset uintptr, rows int) {
	base := unsafe.Add(data, offset)
	if presence == nil {
		for row := range min(rows, len(values)) {
			*(**T)(unsafe.Add(base, uintptr(row)*stride)) = &values[row]
		}
		return
	}
	next := 0
	for row := range rows {
		if wire.Present(presence, row) && next < len(values) {
			*(**T)(unsafe.Add(base, uintptr(row)*stride)) = &values[next]
			next++
		}
	}
}
