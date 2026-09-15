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
// That is the old standard-vs-compact mode decision, except that it is now made
// per field rather than per message. A record can hold a three-element list and
// a ten-thousand-row table and get both right.
//
// # Not every slice can be a table
//
// A column has to be a column of something the column codec carries. A struct
// whose fields are integers, bools, floats and strings transposes; one with a
// nested struct or a slice inside it does not, and stays row-wise however long
// it gets. That is a property of the element type, so the plan resolves it once.

import (
	"math"
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

// scratch is what an encode carries beside the buffer it is writing into. One
// per message, threaded down the whole walk.
//
// Most of it is the transposition buffer: one column, reused across the columns
// of a table and across tables in the same message, because a column is read out
// of it and into the wire before the next is gathered.
//
// The rest is what a dynamic value needs, and it is here rather than in a second
// parameter because this is already the thing every writer is handed. See
// dynamic.go.
type scratch struct {
	ints    []int64
	strings []string
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
	// depth bounds how far a dynamic value is followed, because `m["self"] = m`
	// is a value a caller can build.
	depth int
	// err is the first failure. The writers cannot return one — they are shaped
	// around never needing to — so it is recorded here and the entry points in
	// codec.go hand it back.
	err error
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

// enter and leave bound a dynamic value's nesting. enter answers false at the
// limit, and the caller writes a null in place of what it could not follow —
// the message is abandoned anyway, since err is set.
func (buf *scratch) enter() bool {
	if buf.depth >= maxDynamicDepth {
		buf.fail(errDynamicDepth)
		return false
	}
	buf.depth++
	return true
}

func (buf *scratch) leave() { buf.depth-- }

// reserve grows the transposition buffers to hold one column of rows rows.
//
// Growing them column by column instead cost twenty-one allocations on a
// thousand-row table — the buffers are reused across columns and across tables,
// so the only growth that should ever happen is the first one.
func (buf *scratch) reserve(rows int) {
	if cap(buf.ints) < rows {
		buf.ints = make([]int64, 0, rows)
	}
	if cap(buf.strings) < rows {
		buf.strings = make([]string, 0, rows)
	}
}

// appendTableField writes a slice of structs transposed: one keyed column per
// field of the element type.
func appendTableField(writer *wire.Writer8, field *planField, at unsafe.Pointer, buf *scratch) {
	slice := (*sliceHeader)(at)
	rows := slice.len
	buf.reserve(rows)
	mark := writer.OpenTable(field.key, rows)
	for index := range field.sub.fields {
		column := &field.sub.fields[index]
		if column.op == opString {
			buf.strings = gatherStrings(buf.strings[:0], slice, field.stride, column.offset)
			writer.StringColumn(column.key, buf.strings)
			continue
		}
		buf.ints = gatherInts(buf.ints[:0], slice, field.stride, column)
		writer.Column(column.key, buf.ints)
	}
	writer.Close(mark)
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
	if rows > maxTableRows {
		reader.Fail(errTooManyRows)
		return
	}
	buf.reserve(rows)
	newSlice(field, at, rows)
	data := (*sliceHeader)(at).data

	for columns.More() {
		column := field.sub.find(columns.Key())
		if column == nil {
			if !columns.Skip() {
				reader.Fail(columns.Err())
				return
			}
			continue
		}
		if column.op == opString {
			buf.strings = columns.Strings(buf.strings[:0])
			scatterStrings(buf.strings, data, field.stride, column.offset, rows)
			continue
		}
		buf.ints = columns.Column(rows, buf.ints[:0])
		scatterInts(buf.ints, data, field.stride, column, rows)
	}
	reader.Fail(columns.Err())
}

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
