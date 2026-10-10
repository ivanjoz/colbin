package codec

// Walking a message with a schema where the Go type would be.
//
// The existing decoders write through `unsafe` pointers into a layout they were
// given. There is no layout here — the plan came off the wire, or from a type
// the reader may not have — so this is a second set of walkers rather than a
// flag on the first. It is the bulk of the schema feature and it is unavoidable.
//
// # One walk, two outputs
//
//	AppendJSON  →  JSON text, straight onto the caller's buffer
//	DecodeAny   →  map[string]any, []any, int64, string, …
//
// Both drive the same walk through a `sink`, and that is the point rather than a
// tidiness: the last version of this feature had a JSON encoder and an `any`
// encoder side by side with parallel switches, and they drifted. The walk knows
// the wire; a sink knows what to do with a bool. Neither knows the other.
//
// JSON text is the one worth having. The intermediate `map[string]any` is where
// all the allocation is, and a browser wants text anyway.
//
// # What the walk has to get right
//
//   - **An absent key is a zero value, not a missing field.** colbin writes
//     nothing for a zero, so the JSON has to put it back or the output is not
//     the record. Every field of the schema appears in the output; the ones the
//     message omitted come out as 0, "", false or null, which is what Unmarshal
//     would have left in the struct.
//   - **A slice of structs has two shapes**, and the writer picks per value. The
//     table is the harder one: it is keyed per column and JSON is per row, so
//     the decoder does a transpose the Go decoder gets for free by scattering.
//   - **Floats are byte-reversed bit patterns** in a scalar field and *plain*
//     bit patterns in a column. Reading one as the other is silent nonsense, so
//     the two paths are separate and each has a test.
//   - **An unknown key is stepped over, at either width.** A message from a
//     newer peer holds fields the schema does not list; the nibble beside a
//     narrow key sizes the field as the wide descriptor does, so the walk steps
//     over it and the document is the fields the schema knows, as it is for a Go
//     type.
//   - **The message is untrusted.** The walk descends at most maxSchemaDepth
//     levels, a message's tables declare at most rowBudget rows between them,
//     and nothing continues once a failure is recorded.

import (
	"fmt"
	"math"
	"math/bits"
	"strconv"

	"github.com/ivanjoz/colbin/wire"
)

// maxSchemaDepth bounds how far any walk descends — encode and decode, typed,
// dynamic and JSON alike — counting a level per struct, list, map and table.
//
// Nesting is data, not type: `[]Node` inside `Node` nests as deep as the value
// or the message says, and either could otherwise run the stack out. One bound
// for every path is what keeps Marshal from writing a message that ToJSON, or
// the Rust port (rust/src/walk.rs, MAX_DEPTH), would refuse.
const maxSchemaDepth = 128

var errTooDeep = fmt.Errorf("colbin: a value nests more than %d deep, or holds itself", maxSchemaDepth)

// maxTableRows bounds the row count a table may declare.
//
// It is the one number a message gives that decides an allocation on its own,
// and unlike every length here it cannot be checked against the bytes left: a
// constant column is nine bytes whatever its length, so a legitimate table of a
// million identical rows really does fit in a handful of them. The bound is
// therefore a budget rather than a proof, which is worth stating rather than
// hiding. Without it the row count's four-byte escape is a decompression bomb:
// one flipped bit in a three-hundred-row table asks for four billion rows, and
// 34 GB of int64, before anything has looked at a column.
//
// Four million rows is 32 MB per integer column, and about twice the largest
// fixture here (corpus.Large's two million metrics).
//
// The number is the Rust port's MAX_ROWS (rust/src/walk.rs) and has to stay
// equal to it. That is the constraint, rather than the size: a table the
// browser module refuses must not be one a Go service will hand it, so raising
// this means raising both. Rust has refused on it since it was written, which
// is why closing the hole here left every refusal count in the vector corpora
// exactly where it was.
const maxTableRows = 1 << 22

var errTooManyRows = fmt.Errorf(
	"colbin: a table declares more rows than this decoder allocates for: "+
		"%d in one table, or more than the message's size allows in all", maxTableRows)

// rowBudget is how many table cells — rows times columns — one message may
// declare between all its tables: maxTableRows, and sixty-four more for every
// byte of the message.
//
// maxTableRows bounds one table's rows, which leaves a message free to hold a
// thousand tables, or a table of a hundred columns, and ask for that many times
// the memory. A real table of many cells is many bytes — only an all-zero or
// constant column is not — so a budget that grows with the message admits
// real tables and holds a small message to a small total.
func rowBudget(messageLen int) int {
	return maxTableRows + 64*min(messageLen, (maxInt-maxTableRows)/64)
}

const maxInt = int(^uint(0) >> 1)

// takeRows spends a table's cells from a budget, refusing a table past either
// bound.
func takeRows(left *int, rows, columns int) error {
	if rows > maxTableRows || rows*max(columns, 1) > *left {
		return errTooManyRows
	}
	*left -= rows * max(columns, 1)
	return nil
}

// sink is where a walk puts what it finds. A JSON sink appends text; an `any`
// sink builds maps and slices. Nothing about the wire reaches this far.
type sink interface {
	beginObject()
	key(name string)
	// keyBytes is key for a name the walk is holding as a sub-slice of the
	// message, which every entry of a dynamic map is. It is textBytes' reason:
	// the JSON sink escapes straight out of the message and never makes the Go
	// string at all.
	keyBytes(name []byte)
	endObject()
	beginArray()
	endArray()

	null()
	boolean(value bool)
	signed(value int64)
	unsigned(value uint64)
	// float takes the width as well as the value, because a float32 read as a
	// float64 prints its rounding error — and because JSON has no NaN, which is
	// the one value a sink may refuse.
	float(value float64, width int) error
	text(value string)
	// textBytes is text for a string the walk is holding as bytes, which is most
	// of them: a raw blob is a sub-slice of the message, and turning it into a
	// Go string to hand over would be a copy neither sink wants. Measured on the
	// corpus users it is three allocations a record.
	textBytes(value []byte)
	blob(value []byte)
}

// walker carries the sink, the first failure and the depth. One per message.
type walker struct {
	to  sink
	err error
	// plans is the schema's struct table, which a dynamic value indexes into: a
	// TYPED tag names a struct by number rather than carrying one. Empty when
	// the walk came from a plan with no table behind it, which is only possible
	// for a schema that cannot hold a dynamic value either.
	plans []*typePlan
	depth int
	// rowsLeft is the message's row budget. See rowBudget.
	rowsLeft int
}

func (w *walker) fail(err error) {
	if err != nil && w.err == nil {
		w.err = err
	}
}

// enter takes a level of depth, and refuses once the walk has failed: a walk
// that has failed is unwinding, and descending further would only do work —
// exponential work, for a value that reaches one thing twice.
func (w *walker) enter() bool {
	if w.err != nil {
		return false
	}
	if w.depth >= maxSchemaDepth {
		w.fail(errTooDeep)
		return false
	}
	w.depth++
	return true
}

func (w *walker) leave() { w.depth-- }

// fieldSet marks which of a plan's fields the message actually carried, indexed
// by position rather than by key: a key is a byte and a position is bounded by
// the field count, so four words cover the widest type the format has.
type fieldSet [4]uint64

func (set *fieldSet) add(index int)      { set[index>>6] |= 1 << uint(index&63) }
func (set *fieldSet) has(index int) bool { return set[index>>6]&(1<<uint(index&63)) != 0 }

// AppendJSON writes data as JSON onto dst, which may be nil. On failure it
// returns dst as it was given.
//
// schema describes the message. A message written by MarshalSelfDescribing
// carries its own, which is used in preference to schema; pass nil for one.
//
// The output is equivalent to what encoding/json writes for the same record —
// the same fields holding the same values — though not always the same text:
// every field of the schema is present, a []byte is base64, a nil slice or map
// is null, and the numbers are spelled the same way, but key *order* is the
// message's, with the fields it omitted at the end of their object, and names
// are colbin's rather than any `json` tag's.
//
// A NaN or an infinity is refused rather than turned into null. JSON has no
// spelling for either, and quietly writing null loses the difference between a
// value that was missing and one that was not a number. DecodeAny hands them
// back as they are.
func AppendJSON(dst []byte, schema *Schema, data []byte) ([]byte, error) {
	resolved, body, wide, err := resolveSchema(schema, data)
	if err != nil {
		return dst, err
	}
	out := jsonSink{buffer: dst}
	walk := walker{to: &out, plans: resolved.plans, rowsLeft: rowBudget(len(data))}
	walk.root(resolved.plans[0], body, wide)
	if walk.err != nil {
		return dst[:len(dst):len(dst)], walk.err
	}
	return out.buffer, nil
}

// ToJSON is AppendJSON onto a fresh buffer.
func ToJSON(schema *Schema, data []byte) ([]byte, error) {
	return AppendJSON(nil, schema, data)
}

// DecodeAny decodes data into `map[string]any` and the shapes underneath it:
// `[]any` for every array, `int64` or `uint64` for an integer by its signedness,
// `float64` for either float width, `string`, `bool`, `[]byte` and nil.
//
// It is built on AppendJSON's walk rather than beside it, and it is the slower
// of the two: the intermediate map is where the allocation is. Use it when a
// caller wants to *look* at a field; use AppendJSON when the answer is going out
// as text anyway.
//
// Unlike AppendJSON it is faithful to a non-finite float, which JSON cannot be.
func DecodeAny(schema *Schema, data []byte) (any, error) {
	resolved, body, wide, err := resolveSchema(schema, data)
	if err != nil {
		return nil, err
	}
	out := anySink{}
	walk := walker{to: &out, plans: resolved.plans, rowsLeft: rowBudget(len(data))}
	walk.root(resolved.plans[0], body, wide)
	if walk.err != nil {
		return nil, walk.err
	}
	return out.root, nil
}

// resolveSchema finds the plan and the body: from the message's own section when
// it carries one, and from the schema the caller handed in otherwise.
//
// The message's own wins because it is the one that describes these bytes: a
// caller's schema is a guess about what the message holds, and a wrong guess
// reads every field as some other one.
func resolveSchema(schema *Schema, data []byte) (resolved *Schema, body []byte, wide bool, err error) {
	if len(data) == 0 {
		return nil, nil, false, errNoSection(data)
	}
	section, body, wide, ok := rootParts(data)
	if !ok {
		if data[0] == rootStructNarrowSchema || data[0] == rootStructWideSchema {
			return nil, nil, false, errShortSection
		}
		return nil, nil, false, fmt.Errorf(
			"colbin: byte 0 is %#02x, which is not a root descriptor this version writes",
			data[0])
	}
	if section != nil {
		parsed, err := ParseSchema(section)
		if err != nil {
			return nil, nil, false, err
		}
		return parsed, body, wide, nil
	}
	if schema == nil {
		return nil, nil, false, errNoSection(data)
	}
	return schema, body, wide, nil
}

// root walks the outermost run, which is the only place an envelope can be.
//
// A slice or a map at the root was wrapped in a one-field struct, because the
// root of a colbin message is a struct and nothing else (envelope.go). The
// section says so, so unwrapping is reading a fact rather than guessing at a
// field name — and the wrapper is framing rather than content, so the document
// is the array, not `{"rows":[...]}`.
//
// Only here, and never in `message`: a nested def carrying the flag is a section
// from somewhere else being strange, and the flag says something about the root
// of a document rather than about a struct.
func (w *walker) root(plan *typePlan, body []byte, wide bool) {
	if !plan.envelope || len(plan.fields) != 1 {
		w.message(plan, body, wide)
		return
	}
	w.envelopeRun(plan, body, wide)
}

// envelopeRun writes the wrapper's one field as the document itself: no object
// around it and no key in front of it.
//
// A key that is not the one the section declares leaves the field's zero, rather
// than failing. That is what an absent key means everywhere else in this walk,
// and it is what the Rust and browser ports do — three readers disagreeing about
// a malformed message would be worse than any of the three answers.
func (w *walker) envelopeRun(plan *typePlan, body []byte, wide bool) {
	if !w.enter() {
		return
	}
	defer w.leave()
	field := &plan.fields[0]
	if wide {
		reader := wire.NewReader8(body)
		if reader.More() && reader.Key() == field.key {
			w.wideValue(&reader, field)
		} else {
			w.zero(field)
		}
		w.fail(reader.Err())
		return
	}
	reader := wire.NewReader(body)
	if reader.More() && reader.Key() == field.key {
		w.narrowValue(&reader, field)
	} else {
		w.zero(field)
	}
	w.fail(reader.Err())
}

// message walks a whole record at whichever key width the run declared.
func (w *walker) message(plan *typePlan, body []byte, wide bool) {
	if wide {
		w.wideRun(plan, body)
		return
	}
	w.narrowRun(plan, body)
}

// body walks a nested run at the width its descriptor — or, for a narrow list
// element, its schema — declared.
func (w *walker) body(plan *typePlan, body []byte, wideKeys bool) {
	if plan == nil {
		w.fail(errNoSubSchema)
		return
	}
	w.message(plan, body, wideKeys)
}

var errNoSubSchema = fmt.Errorf(
	"colbin: the schema describes a composite field without saying what is inside it")

// narrowRun turns one four-bit-keyed record into an object.
func (w *walker) narrowRun(plan *typePlan, body []byte) {
	if !w.enter() {
		return
	}
	defer w.leave()
	w.to.beginObject()
	w.narrowFields(plan, body)
	if w.err == nil {
		w.to.endObject()
	}
}

// narrowFields writes a four-bit-keyed run's fields into the object already
// open, which is the run's own or, for a page, the one that links it.
func (w *walker) narrowFields(plan *typePlan, body []byte) {
	reader := wire.NewReader(body)
	var seen fieldSet
	for reader.More() && w.err == nil {
		index := plan.findIndex(reader.Key())
		if index < 0 {
			if !reader.Skip() {
				w.fail(reader.Err())
				return
			}
			continue
		}
		seen.add(index)
		w.to.key(plan.names[index])
		w.narrowValue(&reader, &plan.fields[index])
	}
	w.fail(reader.Err())
	if w.err != nil {
		return
	}
	w.absent(plan, &seen)
}

// wideRun turns one eight-bit-keyed record into an object. A key the schema does
// not list is stepped over, which is what the wide width is for.
func (w *walker) wideRun(plan *typePlan, body []byte) {
	if !w.enter() {
		return
	}
	defer w.leave()
	w.to.beginObject()
	w.wideFields(plan, body)
	if w.err == nil {
		w.to.endObject()
	}
}

// wideFields is narrowFields at eight key bits, which is the only width a page
// link can be under.
func (w *walker) wideFields(plan *typePlan, body []byte) {
	reader := wire.NewReader8(body)
	var seen fieldSet
	for reader.More() && w.err == nil {
		index := plan.findIndex(reader.Key())
		if index < 0 {
			if !reader.Skip() {
				w.fail(reader.Err())
				return
			}
			continue
		}
		seen.add(index)
		field := &plan.fields[index]
		if field.linksPage() {
			w.page(&reader, field)
			continue
		}
		w.to.key(plan.names[index])
		w.wideValue(&reader, field)
	}
	w.fail(reader.Err())
	if w.err != nil {
		return
	}
	w.absent(plan, &seen)
}

// page writes the next page of a paged type into the object its first page
// opened: the fields are the type's, and the page is only where they were put.
func (w *walker) page(reader *wire.Reader8, field *planField) {
	body, wideKeys, ok := reader.StructBody()
	if !ok {
		w.fail(reader.Err())
		return
	}
	if !w.enter() {
		return
	}
	defer w.leave()
	if wideKeys {
		w.wideFields(field.sub, body)
		return
	}
	w.narrowFields(field.sub, body)
}

// absent writes the fields the message left out.
//
// It is not an afterthought: omission *is* the encoding of a zero value, so a
// record that writes three of its nine fields still has nine, and a reader that
// printed three would be printing a different record. A page left out is the
// same thing at a larger scale: its fields go into this object, as zeros.
func (w *walker) absent(plan *typePlan, seen *fieldSet) {
	for index := range plan.fields {
		if w.err != nil {
			return
		}
		if seen.has(index) {
			continue
		}
		field := &plan.fields[index]
		if field.linksPage() {
			w.absentPage(field.sub)
			continue
		}
		w.to.key(plan.names[index])
		w.zero(field)
	}
}

func (w *walker) absentPage(page *typePlan) {
	if !w.enter() {
		return
	}
	defer w.leave()
	var none fieldSet
	w.absent(page, &none)
}

// zero is what an absent key means, per op — which is exactly what Unmarshal
// would have left in the struct, because it zeroes the record first.
func (w *walker) zero(field *planField) {
	switch field.op {
	case opBool:
		w.to.boolean(false)
	case opInt8, opInt16, opInt32, opInt64:
		w.to.signed(0)
	case opUint8, opUint16, opUint32, opUint64:
		w.to.unsigned(0)
	case opFloat32:
		w.floatValue(0, 32)
	case opFloat64:
		w.floatValue(0, 64)
	case opString:
		w.to.text("")
	case opStruct:
		// A nested struct is always written, empty body or not, so this is
		// unreachable from anything this encoder produces. A section from
		// somewhere else may still say it, and an object of zeros is what the
		// field would have held.
		w.zeroStruct(field.sub)
	default:
		// A blob, an array, a slice of structs, a map and a pointer are all nil
		// when the key is absent, and a nil one of those is null.
		w.to.null()
	}
}

func (w *walker) zeroStruct(plan *typePlan) {
	if plan == nil || !w.enter() {
		w.to.null()
		return
	}
	defer w.leave()
	var none fieldSet
	w.to.beginObject()
	w.absent(plan, &none)
	w.to.endObject()
}

func (w *walker) floatValue(value float64, width int) {
	w.fail(w.to.float(value, width))
}

// narrowValue reads one field at four key bits.
//
// The composites are here and the values are in narrowScalar (ops_gen.go),
// because a pointer field's payload *is* a value — the op it points at.
func (w *walker) narrowValue(reader *wire.Reader, field *planField) {
	switch field.op {
	case opStruct, opPointerStruct:
		// A present key is a body either way: a nil pointer wrote no key at all,
		// and absent() renders that as null.
		body, wideKeys, ok := reader.StructBody()
		if !ok {
			w.fail(reader.Err())
			return
		}
		w.body(field.sub, body, wideKeys)
	case opStructs:
		if reader.IsTable() {
			w.narrowTable(reader, field)
			return
		}
		w.narrowList(reader, field)
	case opMap:
		w.narrowMap(reader, field)
	case opPointer:
		w.narrowScalar(reader, field.elemOp)
	default:
		// opAny and opAnys land here and are refused by errUnwalkableOp, which is
		// right: a dynamic value names its own class and four descriptor bits
		// have no room for one, so a narrow run cannot hold one. A plan with one
		// goes wide, so this is only reachable from a section somewhere else.
		w.narrowScalar(reader, field.op)
	}
}

// wideValue is narrowValue at eight key bits. It is written out rather than
// shared behind an interface because the readers are different types and boxing
// them would cost an allocation per field to save a switch.
func (w *walker) wideValue(reader *wire.Reader8, field *planField) {
	switch field.op {
	case opStruct, opPointerStruct:
		body, wideKeys, ok := reader.StructBody()
		if !ok {
			w.fail(reader.Err())
			return
		}
		w.body(field.sub, body, wideKeys)
	case opStructs:
		if reader.IsTable() {
			w.wideTable(reader, field)
			return
		}
		w.wideList(reader, field)
	case opMap:
		w.wideMap(reader, field)
	case opPointer:
		w.wideScalar(reader, field.elemOp)
	case opAny, opAnys:
		// The key is a field's and everything behind it is a key-less value's,
		// so the cursor steps once and the dynamic walk takes it from there.
		if reader.Payload() {
			w.dynamicValue(reader)
		} else {
			w.fail(reader.Err())
		}
	default:
		w.wideScalar(reader, field.op)
	}
}

func errUnwalkableOp(op fieldOp) error {
	return fmt.Errorf("colbin: the schema puts field type %d where a value belongs", uint8(op))
}

// Arrays. Generic over the element type, and free functions because a method
// cannot take one.

func signedArray[T wire.Integer](w *walker, values []T) {
	w.to.beginArray()
	for _, value := range values {
		w.to.signed(int64(value))
	}
	w.to.endArray()
}

func unsignedArray[T wire.Integer](w *walker, values []T) {
	w.to.beginArray()
	for _, value := range values {
		w.to.unsigned(uint64(value))
	}
	w.to.endArray()
}

func (w *walker) textArray(values [][]byte) {
	w.to.beginArray()
	for _, value := range values {
		w.to.textBytes(value)
	}
	w.to.endArray()
}

// Lists of structs, the row-wise shape.

// narrowList walks a narrow list, whose element is a length and a key run with
// no descriptor between them. That missing descriptor is a byte saved per
// element and the reason a structDef states its key width: this is the one run
// on the wire whose width the wire does not say.
func (w *walker) narrowList(reader *wire.Reader, field *planField) {
	count, elements, ok := reader.List()
	if !ok {
		w.fail(reader.Err())
		return
	}
	if field.sub == nil {
		w.fail(errNoSubSchema)
		return
	}
	if !w.enter() {
		return
	}
	defer w.leave()
	w.to.beginArray()
	for range count {
		body, ok := elements.Element()
		if !ok {
			w.fail(elements.Err())
			return
		}
		w.body(field.sub, body, field.sub.isWide)
		if w.err != nil {
			return
		}
	}
	w.to.endArray()
	w.fail(elements.Err())
}

// wideList walks a wide list, whose elements carry a descriptor of their own —
// which is what lets a wide reader step over one it does not understand.
func (w *walker) wideList(reader *wire.Reader8, field *planField) {
	count, elements, ok := reader.List()
	if !ok {
		w.fail(reader.Err())
		return
	}
	w.structList(count, &elements, field.sub)
}

// structList walks an opened list of struct elements.
//
// It is split from wideList because a dynamic value reaches the same run by a
// different door — a TYPED tag names the plan, where a field's descriptor comes
// with one — and the rows behind both doors are byte for byte the same. See
// dynamic.go.
func (w *walker) structList(count int, elements *wire.Reader8, plan *typePlan) {
	if plan == nil {
		w.fail(errNoSubSchema)
		return
	}
	if !w.enter() {
		return
	}
	defer w.leave()
	w.to.beginArray()
	for range count {
		body, wideKeys, ok := elements.ElementStructBody()
		if !ok {
			w.fail(elements.Err())
			return
		}
		w.body(plan, body, wideKeys)
		if w.err != nil {
			return
		}
	}
	w.to.endArray()
	w.fail(elements.Err())
}

// Tables, the columnar shape.
//
// This is the transpose, and it is the one place the JSON decoder does real work
// the Go decoder does not. A table is one key per *column*; JSON is one object
// per *row*. The Go decoder scatters each column across a slice as it reads it
// and never holds two at once. Here every column has to be in memory before the
// first row can be written, because the first row needs the first value of every
// one of them.
//
// So a table costs its whole self in memory for the length of the walk, and the
// alternative — one pass over the message per row — is worse by the row count.
// The peak is the same buffer `scratch` already holds on the encode side, except
// that it is every column at once rather than one at a time, and it is
// documented rather than hidden.
//
// The row count is the message's claim, and nothing in the message bounds it:
// an absent column is a column of zeros. Both this walk and the typed decoder
// spend it from the message's row budget (rowBudget) before allocating for it.

// tableColumns is a decoded table, by field index rather than by key.
type tableColumns struct {
	ints [][]int64
	// A string column is held as sub-slices of the message rather than as Go
	// strings, for the reason textBytes exists: the walk only ever appends them.
	strings [][][]byte
	present []bool
}

func newTableColumns(fields int) *tableColumns {
	return &tableColumns{
		ints:    make([][]int64, fields),
		strings: make([][][]byte, fields),
		present: make([]bool, fields),
	}
}

// narrowTable walks a table under a narrow parent, whose columns are keyed at
// the row type's width — which, as for a narrow list's elements, the schema
// says and the wire does not.
func (w *walker) narrowTable(reader *wire.Reader, field *planField) {
	rows, body, ok := reader.Table()
	if !ok {
		w.fail(reader.Err())
		return
	}
	sub := field.sub
	if sub == nil {
		w.fail(errNoSubSchema)
		return
	}
	if sub.isWide {
		columns := wire.NewReader8(body)
		w.structTable(rows, &columns, sub)
		return
	}
	if err := takeRows(&w.rowsLeft, rows, len(sub.fields)); err != nil {
		w.fail(err)
		return
	}
	columns := wire.NewReader(body)
	gathered := newTableColumns(len(sub.fields))
	for columns.More() {
		index := sub.findIndex(columns.Key())
		if index < 0 {
			if !columns.Skip() {
				w.fail(columns.Err())
				return
			}
			continue
		}
		if sub.fields[index].op == opString {
			gathered.strings[index] = columns.StringsBytes(nil)
		} else {
			gathered.ints[index] = columns.Column(rows, nil)
		}
		gathered.present[index] = true
	}
	if err := columns.Err(); err != nil {
		w.fail(err)
		return
	}
	w.tableRows(sub, gathered, rows)
}

func (w *walker) wideTable(reader *wire.Reader8, field *planField) {
	rows, columns, ok := reader.Table()
	if !ok {
		w.fail(reader.Err())
		return
	}
	w.structTable(rows, &columns, field.sub)
}

// structTable walks a table whose columns are keyed at eight bits: under a wide
// parent, under a narrow one whose rows are wide, or reached by a dynamic value
// through a TYPED tag.
func (w *walker) structTable(rows int, columns *wire.Reader8, sub *typePlan) {
	if sub == nil {
		w.fail(errNoSubSchema)
		return
	}
	if err := takeRows(&w.rowsLeft, rows, len(sub.fields)); err != nil {
		w.fail(err)
		return
	}
	gathered := newTableColumns(len(sub.fields))
	for columns.More() {
		index := sub.findIndex(columns.Key())
		if index < 0 {
			if !columns.Skip() {
				w.fail(columns.Err())
				return
			}
			continue
		}
		if sub.fields[index].op == opString {
			gathered.strings[index] = columns.StringsBytes(nil)
		} else {
			gathered.ints[index] = columns.Column(rows, nil)
		}
		gathered.present[index] = true
	}
	if err := columns.Err(); err != nil {
		w.fail(err)
		return
	}
	w.tableRows(sub, gathered, rows)
}

// tableRows is the transpose itself.
func (w *walker) tableRows(sub *typePlan, gathered *tableColumns, rows int) {
	if !w.enter() {
		return
	}
	defer w.leave()
	w.to.beginArray()
	for row := range rows {
		if w.err != nil {
			return
		}
		w.to.beginObject()
		for index := range sub.fields {
			field := &sub.fields[index]
			w.to.key(sub.names[index])
			switch {
			case !gathered.present[index]:
				// A column whose every value was zero is not written at all, and
				// its absence is the whole of what says so.
				w.zero(field)
			case field.op == opString:
				w.to.textBytes(elementAt(gathered.strings[index], row))
			default:
				w.columnValue(field.op, elementAt(gathered.ints[index], row))
			}
		}
		w.to.endObject()
	}
	w.to.endArray()
}

// elementAt reads a column at a row, and answers zero past its end. A column
// shorter than the table's row count is a message that disagrees with itself;
// the row still has to be written, and a zero is what an absent column means
// everywhere else here.
func elementAt[T any](values []T, index int) T {
	var zero T
	if index < len(values) {
		return values[index]
	}
	return zero
}

// columnValue turns one gathered column value back into its type.
//
// A column carries a float as its **plain** bit pattern, where a scalar field
// carries the byte-reversed one: the reversal exists to move a float's zero
// bytes where the integer trim can reach them, and a column has no such trim to
// feed. See gatherInts. Reading either as the other yields a number rather than
// an error, which is why they do not share a line of code.
func (w *walker) columnValue(op fieldOp, raw int64) {
	switch op {
	case opBool:
		w.to.boolean(raw == 1)
	case opInt8, opInt16, opInt32, opInt64:
		w.to.signed(raw)
	case opUint8, opUint16, opUint32, opUint64:
		w.to.unsigned(uint64(raw))
	case opFloat32:
		w.floatValue(float64(math.Float32frombits(uint32(raw))), 32)
	case opFloat64:
		w.floatValue(math.Float64frombits(uint64(raw)), 64)
	default:
		// Nothing else is columnable, so nothing else can be here. See
		// columnable().
		w.fail(errUnwalkableOp(op))
	}
}

// Maps.
//
// Entries render in the order the message holds them, which the Go encoder
// sorts by key, so a map renders to the same text every time.
//
// An integer key becomes a quoted decimal, because a JSON object key is a
// string. Reading one back the other way is out of scope.

func (w *walker) narrowMap(reader *wire.Reader, field *planField) {
	count, entries, ok := reader.Map()
	if !ok {
		w.fail(reader.Err())
		return
	}
	if !mapKeyIsRenderable(field.keyKind) {
		w.fail(errBadMapKey(field.keyKind))
		return
	}
	if field.valueKind == mapAny {
		// A dynamic value names its own class, which four descriptor bits cannot:
		// a map of them is always wide, so a section saying otherwise is wrong.
		w.fail(errBadMapValue(field.valueKind))
		return
	}
	if !w.enter() {
		return
	}
	defer w.leave()
	w.to.beginObject()
	for range count {
		switch field.keyKind {
		case mapString:
			w.to.key(entries.ElementString())
		case mapInt:
			w.to.key(strconv.FormatInt(entries.ElementInt(), 10))
		case mapUint:
			w.to.key(strconv.FormatUint(entries.ElementUint(), 10))
		}
		switch field.valueKind {
		case mapString:
			w.to.text(entries.ElementString())
		case mapInt:
			w.to.signed(entries.ElementInt())
		case mapUint:
			w.to.unsigned(entries.ElementUint())
		case mapFloat32:
			w.floatValue(float32FromReversed(entries.ElementUint()), 32)
		case mapFloat64:
			w.floatValue(float64FromReversed(entries.ElementUint()), 64)
		case mapBool:
			w.to.boolean(entries.ElementUint() == 1)
		case mapStruct:
			// A narrow list's element: a length and a body, whose key width the
			// schema says because nothing on the wire does.
			if body, ok := entries.Element(); ok {
				w.body(field.sub, body, field.sub != nil && field.sub.isWide)
			}
		default:
			w.fail(errBadMapValue(field.valueKind))
		}
		w.fail(entries.Err())
		if w.err != nil {
			return
		}
	}
	w.to.endObject()
}

func errBadMapValue(kind mapKind) error {
	return fmt.Errorf("colbin: the schema gives a map values of kind %d, which this key width cannot hold", kind)
}

func (w *walker) wideMap(reader *wire.Reader8, field *planField) {
	count, entries, ok := reader.Map()
	if !ok {
		w.fail(reader.Err())
		return
	}
	if !mapKeyIsRenderable(field.keyKind) {
		w.fail(errBadMapKey(field.keyKind))
		return
	}
	if !w.enter() {
		return
	}
	defer w.leave()
	w.to.beginObject()
	for range count {
		switch field.keyKind {
		case mapString:
			w.to.key(entries.ElementString())
		case mapInt:
			w.to.key(strconv.FormatInt(entries.ElementInt(), 10))
		case mapUint:
			w.to.key(strconv.FormatUint(entries.ElementUint(), 10))
		}
		switch field.valueKind {
		case mapString:
			w.to.text(entries.ElementString())
		case mapInt:
			w.to.signed(entries.ElementInt())
		case mapUint:
			w.to.unsigned(entries.ElementUint())
		case mapFloat32:
			w.floatValue(float32FromReversed(entries.ElementUint()), 32)
		case mapFloat64:
			w.floatValue(float64FromReversed(entries.ElementUint()), 64)
		case mapBool:
			w.to.boolean(entries.ElementUint() == 1)
		case mapAny:
			// The entries of a `map[string]any`, which say what they are one at a
			// time rather than once in the schema. See dynamic.go.
			w.dynamicValue(&entries)
		case mapStruct:
			if body, wideKeys, ok := entries.ElementStructBody(); ok {
				w.body(field.sub, body, wideKeys)
			}
		default:
			w.fail(errBadMapValue(field.valueKind))
		}
		w.fail(entries.Err())
		if w.err != nil {
			return
		}
	}
	w.to.endObject()
}

// mapKeyIsRenderable says the kind is one the format accepts as a key. A float
// and a bool are refused at plan time, so only a section from somewhere else can
// name one — and a key that does not consume its bytes would misread every entry
// after it, so this is checked once before the loop rather than defaulted in it.
func mapKeyIsRenderable(kind mapKind) bool {
	switch kind {
	case mapString, mapInt, mapUint:
		return true
	}
	return false
}

func errBadMapKey(kind mapKind) error {
	return fmt.Errorf("colbin: the schema gives a map a key of kind %d, which is not a key", kind)
}

// float32FromReversed and float64FromReversed undo the byte reversal a map value
// travels under, which is the same one a scalar float field uses.
func float32FromReversed(raw uint64) float64 {
	return float64(math.Float32frombits(bits.ReverseBytes32(uint32(raw))))
}

func float64FromReversed(raw uint64) float64 {
	return math.Float64frombits(bits.ReverseBytes64(raw))
}
