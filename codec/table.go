package codec

// Choosing the columnar layout for a slice of structs.
//
// `wire` has both shapes: a LIST of STRUCT, which is a key run per element, and
// a TABLE, which is one key per *column*. The table wins from a few rows upward
// and by a lot further on — 7.8 bytes a row against 20.6 at four thousand — and
// it decodes through the blocked column codec rather than through a key run per
// record.
//
// # It is decided per value, not per type
//
// The row count is a property of the data, so the plan cannot decide it. The
// encoder does, at the one place that knows: `tableThreshold` rows. Both shapes
// are self-describing — a LIST descriptor and a TABLE descriptor are different
// classes — so the reader dispatches on what it finds and never has to be told
// which was chosen.
//
// A record can hold a three-element list and a ten-thousand-row table and get
// both right.
//
// # Column keys take the wider of two widths
//
// A table's columns are a key run, and its width is the wider of the enclosing
// run's and the row type's: under a wide parent the columns are always wide, and
// under a narrow one they are as wide as the row type needs — a row with an id
// past sixteen has a column key four bits cannot hold. A reader takes the row
// type's width from the schema, as it does a narrow list element's.
//
// # Not every slice can be a table
//
// A column has to be a column of something the column codec carries. A struct
// whose fields are integers, bools, floats and strings transposes; one with a
// nested struct or a slice inside it does not, and stays row-wise however long
// it gets. That is a property of the element type, so the plan resolves it once.

import (
	"errors"
	"math"
	"sync"
	"unsafe"

	"github.com/ivanjoz/colbin/wire"
)

// tableThreshold is where a table overtakes a list of structs. Measured on a
// four-field row it crosses at three; the value here is deliberately above that,
// because the table also costs a transposition pass and a scratch buffer, and
// those are worth paying only once there are rows enough to amortise them.
const tableThreshold = 8

// columnable reports whether a field's op is one the column codec carries.
// Floats travel as their bit patterns, which no transform helps but raw blocks
// carry at the element width.
func columnable(op fieldOp) bool {
	switch op {
	case opBool, opInt8, opInt16, opInt32, opInt64,
		opUint8, opUint16, opUint32, opUint64,
		opFloat32, opFloat64, opString:
		return true
	}
	return false
}

// transposable reports whether every field of a plan is columnable, which is
// what makes a slice of that struct a candidate for a table.
func (plan *typePlan) transposable() bool {
	for _, field := range plan.fields {
		if !columnable(field.op) {
			return false
		}
	}
	return len(plan.fields) > 0
}

// scratch is what an encode or a decode carries beside its buffer. One per
// message, threaded down the whole walk.
//
// It holds the transposition buffer — one column, reused across the columns of
// a table and across tables in the same message, because a column goes out of
// it before the next comes in — and the bounds a walk is held to: its depth, and
// on decode the rows its tables may still declare. The rest is what a dynamic
// value needs, and it is here rather than in a second parameter because this is
// already the thing every walk is handed. See dynamic.go.
type scratch struct {
	columns *columnBuffers
	// rowsLeft is a decode's row budget. See rowBudget.
	rowsLeft int
	// section is the struct table being built, when the message is carrying one.
	// A dynamic struct adds itself to it and writes the index; nil means there is
	// nowhere to put a def, and such a struct goes out as a map of names.
	section *sectionBuilder
	// rawSection is the other direction: the section a message arrived with, kept
	// unparsed because most decodes never look at it. A TYPED value is the only
	// thing that needs the table, so structPlans parses on first use.
	rawSection []byte
	plans      []*typePlan
	planned    bool
	// depth bounds how far a walk descends, because nesting is data: a decode
	// nests as deep as the message says, and an encode as deep as the value,
	// which `n.Next = n` or `m["self"] = m` makes forever.
	depth int
	// err is an encode's first failure. The writers cannot return one — they are
	// shaped around never needing to — so it is recorded here and the entry
	// points in codec.go hand it back. A decode records its failures on the
	// reader instead, and mirrors them here so that enter stops it too.
	err error
}

// columnBuffers is the transposition buffer, pooled across messages: a table of
// a thousand rows needs 24 KB of it, which a handle encoding one message after
// another would otherwise allocate per call.
type columnBuffers struct {
	ints    []int64
	strings []string
}

var columnPool sync.Pool // *columnBuffers

// maxPooledRows is the largest buffer the pool keeps, so that one huge table
// does not pin its memory for the life of the process.
const maxPooledRows = 1 << 16

// release hands the transposition buffer back. Every scratch that may have
// reserved one is released when its message is done.
func (buf *scratch) release() {
	columns := buf.columns
	if columns == nil {
		return
	}
	buf.columns = nil
	if cap(columns.ints) > maxPooledRows || cap(columns.strings) > maxPooledRows {
		return
	}
	// The strings are the caller's, and a pooled buffer must not keep them alive.
	clear(columns.strings[:cap(columns.strings)])
	columnPool.Put(columns)
}

// structPlans is the struct table a TYPED value indexes into, parsed from the
// message's own section the first time one is asked for.
//
// It answers nil with no error when the message carried no section. That is not
// a failure yet: a dynamic value only needs the table if it names a struct, and
// most do not. The walk says so if one does.
func (buf *scratch) structPlans() ([]*typePlan, error) {
	if buf.planned {
		return buf.plans, nil
	}
	buf.planned = true
	if len(buf.rawSection) == 0 {
		return nil, nil
	}
	schema, err := ParseSchema(buf.rawSection)
	if err != nil {
		return nil, err
	}
	buf.plans = schema.plans
	return buf.plans, nil
}

// fail records the first failure of an encode.
func (buf *scratch) fail(err error) {
	if err != nil && buf.err == nil {
		buf.err = err
	}
}

// enter and leave bound a walk's nesting. enter answers false at the limit, and
// once anything has failed: a walk that has failed is unwinding, and descending
// further would only do work — exponential work, for a value that reaches one
// thing twice. A caller that cannot follow writes nothing, or a null, in place;
// the message is abandoned anyway.
func (buf *scratch) enter() bool {
	if buf.err != nil {
		return false
	}
	if buf.depth >= maxSchemaDepth {
		buf.fail(errTooDeep)
		return false
	}
	buf.depth++
	return true
}

func (buf *scratch) leave() { buf.depth-- }

// reserve grows the transposition buffers to hold one column of rows rows, the
// strings one only when the table has a string column.
//
// Growing them column by column instead cost twenty-one allocations on a
// thousand-row table — the buffers are reused across columns and across tables,
// so the only growth that should ever happen is the first one.
func (buf *scratch) reserve(rows int, strings bool) *columnBuffers {
	columns := buf.columns
	if columns == nil {
		columns, _ = columnPool.Get().(*columnBuffers)
		if columns == nil {
			columns = &columnBuffers{}
		}
		buf.columns = columns
	}
	if cap(columns.ints) < rows {
		columns.ints = make([]int64, 0, rows)
	}
	if strings && cap(columns.strings) < rows {
		columns.strings = make([]string, 0, rows)
	}
	return columns
}

// hasStringColumn says a row type has a string field, which is what needs the
// strings half of the transposition buffer.
func (plan *typePlan) hasStringColumn() bool {
	for index := range plan.fields {
		if plan.fields[index].op == opString {
			return true
		}
	}
	return false
}

// appendTableField writes a slice of structs transposed: one keyed column per
// field of the element type.
func appendTableField(writer *wire.Writer8, field *planField, at unsafe.Pointer, buf *scratch) {
	slice := (*sliceHeader)(at)
	mark := writer.OpenTable(field.key, slice.len)
	appendColumns(writer, field.sub, slice, field.stride, buf)
	writer.Close(mark)
}

// appendColumns writes a table's columns with eight-bit keys, which is every
// table under a wide parent, one under a narrow parent whose rows are wide, and
// one in a dynamic value.
func appendColumns(writer *wire.Writer8, plan *typePlan, slice *sliceHeader, stride uintptr, buf *scratch) {
	columns := buf.reserve(slice.len, plan.hasStringColumn())
	for index := range plan.fields {
		column := &plan.fields[index]
		if column.op == opString {
			columns.strings = gatherStrings(columns.strings[:0], slice, stride, column.offset)
			writer.StringColumn(column.key, columns.strings)
			continue
		}
		columns.ints = gatherInts(columns.ints[:0], slice, stride, column)
		writer.Column(column.key, columns.ints)
	}
}

// gatherInts reads one field out of every row, widened to int64. A float's bit
// pattern travels as an integer: the column codec's transforms find nothing in
// it, and raw blocks at the element width are what it would have cost anyway.
func gatherInts(dst []int64, slice *sliceHeader, stride uintptr, column *planField) []int64 {
	for row := range slice.len {
		at := unsafe.Add(slice.data, uintptr(row)*stride+column.offset)
		var value int64
		switch column.op {
		case opBool:
			if *(*bool)(at) {
				value = 1
			}
		case opInt8:
			value = int64(*(*int8)(at))
		case opInt16:
			value = int64(*(*int16)(at))
		case opInt32:
			value = int64(*(*int32)(at))
		case opInt64:
			value = *(*int64)(at)
		case opUint8:
			value = int64(*(*uint8)(at))
		case opUint16:
			value = int64(*(*uint16)(at))
		case opUint32:
			value = int64(*(*uint32)(at))
		case opUint64:
			value = int64(*(*uint64)(at))
		case opFloat32:
			value = int64(math.Float32bits(*(*float32)(at)))
		case opFloat64:
			value = int64(math.Float64bits(*(*float64)(at)))
		}
		dst = append(dst, value)
	}
	return dst
}

func gatherStrings(dst []string, slice *sliceHeader, stride, offset uintptr) []string {
	for row := range slice.len {
		dst = append(dst, *(*string)(unsafe.Add(slice.data, uintptr(row)*stride+offset)))
	}
	return dst
}

// readTableField reads a table back into a slice of structs, one column at a
// time. Each column is decoded whole and then scattered across the rows, which
// is the order the column codec wants: it fills a run and the scatter is a
// strided store.
func readTableField(reader *wire.Reader8, field *planField, at unsafe.Pointer, buf *scratch) {
	rows, columns, ok := reader.Table()
	if !ok {
		return
	}
	readColumns(&columns, field, at, rows, buf)
	reader.Fail(columns.Err())
}

// readColumns fills a slice of structs from a table's eight-bit-keyed columns,
// after spending its rows from the message's budget.
func readColumns(columns *wire.Reader8, field *planField, at unsafe.Pointer, rows int, buf *scratch) {
	if err := takeRows(&buf.rowsLeft, rows, len(field.sub.fields)); err != nil {
		columns.Fail(err)
		return
	}
	scratchColumns := buf.reserve(rows, field.sub.hasStringColumn())
	newSlice(field, at, rows)
	data := (*sliceHeader)(at).data
	for columns.More() {
		column := field.sub.find(columns.Key())
		if column == nil {
			columns.Skip()
			continue
		}
		if column.op == opString {
			scratchColumns.strings = columns.Strings(scratchColumns.strings[:0])
			if columns.Err() == nil && len(scratchColumns.strings) != rows {
				columns.Fail(errColumnLength)
			}
			scatterStrings(scratchColumns.strings, data, field.stride, column.offset, rows)
			continue
		}
		scratchColumns.ints = columns.Column(rows, scratchColumns.ints[:0])
		scatterInts(scratchColumns.ints, data, field.stride, column, rows)
	}
}

var errColumnLength = errors.New("colbin: a table column holds a different number of values than the table has rows")

func scatterInts(values []int64, data unsafe.Pointer, stride uintptr, column *planField, rows int) {
	if len(values) < rows {
		return
	}
	for row := range rows {
		at := unsafe.Add(data, uintptr(row)*stride+column.offset)
		value := values[row]
		switch column.op {
		case opBool:
			*(*bool)(at) = value == 1
		case opInt8:
			*(*int8)(at) = int8(value)
		case opInt16:
			*(*int16)(at) = int16(value)
		case opInt32:
			*(*int32)(at) = int32(value)
		case opInt64:
			*(*int64)(at) = value
		case opUint8:
			*(*uint8)(at) = uint8(value)
		case opUint16:
			*(*uint16)(at) = uint16(value)
		case opUint32:
			*(*uint32)(at) = uint32(value)
		case opUint64:
			*(*uint64)(at) = uint64(value)
		case opFloat32:
			*(*float32)(at) = math.Float32frombits(uint32(value))
		case opFloat64:
			*(*float64)(at) = math.Float64frombits(uint64(value))
		}
	}
}

func scatterStrings(values []string, data unsafe.Pointer, stride, offset uintptr, rows int) {
	if len(values) < rows {
		return
	}
	for row := range rows {
		*(*string)(unsafe.Add(data, uintptr(row)*stride+offset)) = values[row]
	}
}
