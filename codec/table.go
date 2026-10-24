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
//
// It is declared in threshold.go, as a constant, and in threshold_ablation.go,
// as a variable the colbin-benchmarks ablation moves. The wire does not depend
// on it: a reader dispatches on the descriptor it finds, so a message written
// under any threshold decodes under any other.

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
	// strings is where a nested decode copies its string fields. See wire.Arena.
	strings wire.Arena
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
	decoded []decodedColumn
	// usedInts and usedStrings are the most of each buffer the current message
	// has reserved, which is what release judges a large buffer by.
	usedInts, usedStrings int
}

var columnPool sync.Pool // *columnBuffers

// The pool keeps a buffer by its size and by what the message that held it
// used of it, counted in values: an encode holds a whole table in it, rows
// times columns.
//
// Up to maxPooledRows (4 MB of int64) a buffer is always kept. Past it, only
// while it is earning its size: a message must have used a quarter of it, so a
// large buffer survives a run of large tables and is dropped by the first small
// one, rather than being handed from small table to small table for the life
// of the process. Past maxPooledValues it is never kept.
//
// Dropping one large buffer early costs one allocation the next large table
// would have made anyway; keeping one late costs its memory. And a buffer the
// pool does keep is not kept forever: sync.Pool lets go of what was not taken
// within two collections, so an idle process returns it within two GCs.
const (
	maxPooledRows   = 1 << 19
	maxPooledValues = 1 << 23
)

// keepPooled is whether release puts back a buffer of capacity values of which
// the message used used.
func keepPooled(capacity, used int) bool {
	return capacity <= maxPooledRows || capacity <= maxPooledValues && used >= capacity/4
}

// release hands the transposition buffer back. Every scratch that may have
// reserved one is released when its message is done.
func (buf *scratch) release() {
	columns := buf.columns
	if columns == nil {
		return
	}
	buf.columns = nil
	if !keepPooled(cap(columns.ints), columns.usedInts) {
		columns.ints = nil
	}
	if !keepPooled(cap(columns.strings), columns.usedStrings) {
		columns.strings = nil
	}
	// The strings are the caller's, and a pooled buffer must not keep them
	// alive. Only what this message reserved can hold any: the rest was cleared
	// when the buffer last came back.
	clear(columns.strings[:min(columns.usedStrings, cap(columns.strings))])
	columns.usedInts, columns.usedStrings = 0, 0
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
	columns.usedInts = max(columns.usedInts, rows)
	if strings {
		columns.usedStrings = max(columns.usedStrings, rows)
	}
	if cap(columns.ints) < rows {
		columns.ints = make([]int64, 0, rows)
	}
	if strings && cap(columns.strings) < rows {
		columns.strings = make([]string, 0, rows)
	}
	return columns
}

// reserveTable grows the transposition buffers to hold a whole table at once:
// ints values for its integer columns and strings for its string columns.
func (buf *scratch) reserveTable(ints, strings int) *columnBuffers {
	columns := buf.reserve(0, false)
	columns.usedInts = max(columns.usedInts, ints)
	columns.usedStrings = max(columns.usedStrings, strings)
	if cap(columns.ints) < ints {
		columns.ints = make([]int64, 0, ints)
	}
	if cap(columns.strings) < strings {
		columns.strings = make([]string, 0, strings)
	}
	return columns
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
	ints, strings := gatherTable(plan, slice, stride, buf)
	rows := slice.len
	for index := range plan.fields {
		column := &plan.fields[index]
		if column.op == opString {
			writer.StringColumn(column.key, strings[:rows:rows])
			strings = strings[rows:]
			continue
		}
		writer.Column(column.key, ints[:rows:rows])
		ints = ints[rows:]
	}
}

// gatherChunk is how many rows gatherTable transposes at a time: few enough
// that their bytes stay in cache while every column is read out of them.
const gatherChunk = 256

// gatherTable transposes a whole table, every column, in one pass over its
// rows. It returns the integer columns back to back, rows values each, in
// field order, and the string columns likewise.
//
// Gathering a column at a time walked every row once per column, and a row of
// a wide table is a cache line or more: sixteen columns read the slice sixteen
// times, and the gathering was most of a table's encode. In chunks, a chunk's
// rows are read from memory once and each column is gathered out of cache —
// still with one specialised loop per column, never a dispatch per value.
func gatherTable(plan *typePlan, slice *sliceHeader, stride uintptr, buf *scratch) ([]int64, []string) {
	rows := slice.len
	intColumns, stringColumns := plan.columnCounts()
	columns := buf.reserveTable(rows*intColumns, rows*stringColumns)
	ints := columns.ints[:rows*intColumns]
	strings := columns.strings[:rows*stringColumns]
	for start := 0; start < rows; start += gatherChunk {
		end := min(start+gatherChunk, rows)
		chunk := sliceHeader{data: unsafe.Add(slice.data, uintptr(start)*stride), len: end - start}
		intAt, stringAt := 0, 0
		for index := range plan.fields {
			column := &plan.fields[index]
			if column.op == opString {
				gatherStrings(strings[stringAt+start:stringAt+end], &chunk, stride, column.offset)
				stringAt += rows
				continue
			}
			gatherInts(ints[intAt+start:intAt+end], &chunk, stride, column)
			intAt += rows
		}
	}
	return ints, strings
}

// columnCounts is how many of a row type's fields go out as integer columns
// and how many as string columns.
func (plan *typePlan) columnCounts() (ints, strings int) {
	for index := range plan.fields {
		if plan.fields[index].op == opString {
			strings++
		} else {
			ints++
		}
	}
	return ints, strings
}

// gatherInts reads one field out of every row, widened to int64. A float's bit
// pattern travels as an integer: the column codec's transforms find nothing in
// it, and raw blocks at the element width are what it would have cost anyway.
//
// The op is dispatched once per column, not once per value: each case is a
// loop that only loads and widens, which is what generated code would run.
// Switching inside the loop cost a third of a table's encode.
func gatherInts(dst []int64, slice *sliceHeader, stride uintptr, column *planField) []int64 {
	n := slice.len
	dst = dst[:n]
	base := unsafe.Add(slice.data, column.offset)
	switch column.op {
	case opBool:
		gather(dst, base, stride, func(v bool) int64 {
			if v {
				return 1
			}
			return 0
		})
	case opInt8:
		gather(dst, base, stride, func(v int8) int64 { return int64(v) })
	case opInt16:
		gather(dst, base, stride, func(v int16) int64 { return int64(v) })
	case opInt32:
		gather(dst, base, stride, func(v int32) int64 { return int64(v) })
	case opInt64:
		gather(dst, base, stride, func(v int64) int64 { return v })
	case opUint8:
		gather(dst, base, stride, func(v uint8) int64 { return int64(v) })
	case opUint16:
		gather(dst, base, stride, func(v uint16) int64 { return int64(v) })
	case opUint32:
		gather(dst, base, stride, func(v uint32) int64 { return int64(v) })
	case opUint64:
		gather(dst, base, stride, func(v uint64) int64 { return int64(v) })
	case opFloat32:
		gather(dst, base, stride, func(v float32) int64 { return int64(math.Float32bits(v)) })
	case opFloat64:
		gather(dst, base, stride, func(v float64) int64 { return int64(math.Float64bits(v)) })
	}
	return dst
}

// gather loads one field of type T from each of len(dst) rows stride bytes
// apart, starting at base, widened by widen. widen is a func literal at every
// call site, so it inlines into the instantiation and the loop has no call.
func gather[T any](dst []int64, base unsafe.Pointer, stride uintptr, widen func(T) int64) {
	for row := range dst {
		dst[row] = widen(*(*T)(unsafe.Add(base, uintptr(row)*stride)))
	}
}

func gatherStrings(dst []string, slice *sliceHeader, stride, offset uintptr) []string {
	dst = dst[:slice.len]
	base := unsafe.Add(slice.data, offset)
	for row := range dst {
		dst[row] = *(*string)(unsafe.Add(base, uintptr(row)*stride))
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
	newSlice(field, at, rows)
	table := startTable(field, (*sliceHeader)(at).data, rows, buf)
	for columns.More() {
		column := field.sub.find(columns.Key())
		if column == nil {
			if !columns.Skip() {
				break
			}
			continue
		}
		if column.op == opString {
			got := columns.Strings(table.stringSlot())
			if len(got) != rows {
				if columns.Err() == nil {
					columns.Fail(errColumnLength)
				}
				continue
			}
			table.decoded(column)
			continue
		}
		if got := columns.Column(rows, table.intSlot()); len(got) == rows {
			table.decoded(column)
		}
	}
	table.finish()
}

var errColumnLength = errors.New("colbin: a table column holds a different number of values than the table has rows")

// tableDecode is a table being decoded: every column is decoded first, into
// the transposition buffer, and only then scattered into the rows, a chunk of
// rows at a time — gatherTable run backwards, for the same reason. Scattering
// each column as it was decoded wrote across every row of the slice once per
// column, and that was most of a large table's decode.
//
// A table whose rows fit in cache is written from cache either way, and there
// deferring only costs a pass: its columns are scattered as they are decoded,
// through one column's worth of buffer.
//
// The loop over the columns stays with each key width's reader, so that the
// reader is a concrete type and stays on the stack; this is the bookkeeping
// both share.
type tableDecode struct {
	buffers         *columnBuffers
	ints            []int64
	strings         []string
	pending         []decodedColumn
	intAt, stringAt int
	slotIsString    bool
	direct          bool
	data            unsafe.Pointer
	stride          uintptr
	rows            int
}

// decodedColumn is a column waiting to be scattered: its field, and where its
// rows start in the buffer's ints or strings.
type decodedColumn struct {
	column *planField
	at     int
}

// cachedTableBytes is the size of slice below which a table's rows are taken
// to stay in cache through a column-at-a-time scatter: a core's share of L2.
const cachedTableBytes = 256 << 10

func startTable(field *planField, data unsafe.Pointer, rows int, buf *scratch) tableDecode {
	table := tableDecode{data: data, stride: field.stride, rows: rows,
		direct: uintptr(rows)*field.stride <= cachedTableBytes}
	intColumns, stringColumns := 1, 1
	if !table.direct {
		intColumns, stringColumns = field.sub.columnCounts()
	}
	table.buffers = buf.reserveTable(rows*intColumns, rows*stringColumns)
	table.ints = table.buffers.ints[:rows*intColumns]
	table.strings = table.buffers.strings[:rows*stringColumns]
	table.pending = table.buffers.decoded[:0]
	return table
}

// intSlot is where the next integer column decodes to: rows values, with no
// capacity past them, so that a column longer than the table cannot run into
// the next one's.
func (t *tableDecode) intSlot() []int64 {
	if t.intAt == len(t.ints) {
		t.flush()
	}
	t.slotIsString = false
	return t.ints[t.intAt : t.intAt : t.intAt+t.rows]
}

func (t *tableDecode) stringSlot() []string {
	if t.stringAt == len(t.strings) {
		t.flush()
	}
	t.slotIsString = true
	return t.strings[t.stringAt : t.stringAt : t.stringAt+t.rows]
}

// decoded keeps the column just decoded into the last slot handed out.
func (t *tableDecode) decoded(column *planField) {
	if t.direct {
		if t.slotIsString {
			scatterStrings(t.strings, t.data, t.stride, column.offset, t.rows)
		} else {
			scatterInts(t.ints, t.data, t.stride, column, t.rows)
		}
		return
	}
	if t.slotIsString {
		t.pending = append(t.pending, decodedColumn{column, t.stringAt})
		t.stringAt += t.rows
		return
	}
	t.pending = append(t.pending, decodedColumn{column, t.intAt})
	t.intAt += t.rows
}

// flush scatters what has been decoded so far. It runs early only for a column
// sent twice, which finds no room left: the first copy goes into the rows now,
// and the later one lands over it after, as it would have column by column.
func (t *tableDecode) flush() {
	for start := 0; start < t.rows; start += gatherChunk {
		end := min(start+gatherChunk, t.rows)
		chunk := unsafe.Add(t.data, uintptr(start)*t.stride)
		for _, entry := range t.pending {
			column := entry.column
			if column.op == opString {
				scatterStrings(t.strings[entry.at+start:entry.at+end], chunk, t.stride, column.offset, end-start)
				continue
			}
			scatterInts(t.ints[entry.at+start:entry.at+end], chunk, t.stride, column, end-start)
		}
	}
	t.pending, t.intAt, t.stringAt = t.pending[:0], 0, 0
}

func (t *tableDecode) finish() {
	t.flush()
	t.buffers.decoded = t.pending
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
