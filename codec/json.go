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
//   - **K4 cannot skip.** A narrow message holding a key the schema does not
//     list ends the decode, as it does for a Go type. That is K4's standing
//     trade and the error says so.

import (
	"fmt"
	"math"
	"math/bits"
	"strconv"

	"github.com/ivanjoz/colbin/wire"
)

// maxSchemaDepth bounds how far a walk descends.
//
// Nesting is data here, not type: `[]Node` inside `Node` nests as deep as the
// message says, and a crafted message could otherwise run the stack out. A
// hundred and twenty-eight is far past anything a real record nests and far
// short of anything that hurts.
const maxSchemaDepth = 128

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
	"colbin: a table declares more than %d rows, which this decoder refuses to allocate for",
	maxTableRows)

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
}

func (w *walker) fail(err error) {
	if err != nil && w.err == nil {
		w.err = err
	}
}

func (w *walker) enter() bool {
	if w.depth >= maxSchemaDepth {
		w.fail(fmt.Errorf("colbin: a message nests more than %d deep", maxSchemaDepth))
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

// AppendJSON writes data as JSON onto dst, which may be nil.
//
// schema describes the message. Pass nil for a message written by
// MarshalSelfDescribing, which carries its own.
//
// The output is what encoding/json would have written for the same record: every
// field of the schema is present, a []byte is base64, a nil slice or map is
// null, and the numbers are spelled the same way. Key *order* is not promised —
// the fields the message carried come first, in wire order, and the ones it
// omitted follow.
//
// A NaN or an infinity is refused rather than turned into null. JSON has no
// spelling for either, and quietly writing null loses the difference between a
// value that was missing and one that was not a number. DecodeAny hands them
// back as they are.
func AppendJSON(dst []byte, schema *Schema, data []byte) ([]byte, error) {
	resolved, body, wide, err := resolveSchema(schema, data)
	if err != nil {
		return nil, err
	}
	out := jsonSink{buffer: dst}
	walk := walker{to: &out, plans: resolved.plans}
	walk.root(resolved.plan, body, wide)
	if walk.err != nil {
		return nil, walk.err
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
	walk := walker{to: &out, plans: resolved.plans}
	walk.root(resolved.plan, body, wide)
	if walk.err != nil {
		return nil, walk.err
	}
	return out.root, nil
}

// resolveSchema finds the plan and the body: from the schema the caller handed
// in, or from the message's own section when there is none.
func resolveSchema(schema *Schema, data []byte) (resolved *Schema, body []byte, wide bool, err error) {
	if len(data) == 0 {
		return nil, nil, false, errNoSection(data)
	}
	if schema != nil {
		body, wide, ok := rootOf(data)
		if !ok {
			return nil, nil, false, fmt.Errorf(
				"colbin: byte 0 is %#02x, which is not a root descriptor this version writes",
				data[0])
		}
		return schema, body, wide, nil
	}
	if data[0] != rootStructNarrowSchema && data[0] != rootStructWideSchema {
		return nil, nil, false, errNoSection(data)
	}
	section, body, ok := splitSchemaSection(data[1:])
	if !ok {
		return nil, nil, false, errShortSection
	}
	parsed, err := ParseSchema(section)
	if err != nil {
		return nil, nil, false, err
	}
	return parsed, body, data[0]&rootWide != 0, nil
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
	reader := wire.NewReader(body)
	var seen fieldSet
	w.to.beginObject()
	for reader.More() && w.err == nil {
		key := reader.Key()
		index := plan.findIndex(key)
		if index < 0 {
			// Four descriptor bits have no room for a class, so nothing can size
			// a field it cannot classify. See unmarshalNarrow.
			w.fail(fmt.Errorf(
				"colbin: message holds field id %d, which the schema does not declare, "+
					"and a narrow key cannot be skipped", key))
			return
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
	w.to.endObject()
}

// wideRun turns one eight-bit-keyed record into an object. A key the schema does
// not list is stepped over, which is what the wide width is for.
func (w *walker) wideRun(plan *typePlan, body []byte) {
	if !w.enter() {
		return
	}
	defer w.leave()
	reader := wire.NewReader8(body)
	var seen fieldSet
	w.to.beginObject()
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
		w.wideValue(&reader, &plan.fields[index])
	}
	w.fail(reader.Err())
	if w.err != nil {
		return
	}
	w.absent(plan, &seen)
	w.to.endObject()
}

// absent writes the fields the message left out.
//
// It is not an afterthought: omission *is* the encoding of a zero value, so a
// record that writes three of its nine fields still has nine, and a reader that
// printed three would be printing a different record.
func (w *walker) absent(plan *typePlan, seen *fieldSet) {
	for index := range plan.fields {
		if seen.has(index) {
			continue
		}
		w.to.key(plan.names[index])
		w.zero(&plan.fields[index])
	}
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
// The composites are here and the values are in narrowScalar, because a pointer
// field's payload *is* a value — the op it points at — and the two switches
// would otherwise be one switch written twice.
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

func (w *walker) narrowScalar(reader *wire.Reader, op fieldOp) {
	switch op {
	case opBool:
		w.to.boolean(reader.Bool())
	case opInt8, opInt16, opInt32, opInt64:
		w.to.signed(reader.Int())
	case opUint8, opUint16:
		w.to.unsigned(uint64(reader.U16()))
	case opUint32:
		w.to.unsigned(uint64(reader.U32()))
	case opUint64:
		w.to.unsigned(reader.Uint())
	case opFloat32:
		w.floatValue(float64(reader.F32()), 32)
	case opFloat64:
		w.floatValue(reader.F64(), 64)
	case opString:
		// PackedString rather than Bytes, for the reason the wide path says: the
		// header's escape code carries the encoding, so this needs no setting
		// and cannot be wrong about it.
		//
		// It used to be Bytes, and that was safe only because packed5 forced the
		// wide key — a narrow string could never be packed. It no longer does,
		// which made the case reachable and made this an error rather than a
		// tidiness: Marshal wrote a message ToJSON refused. See
		// TestPackedNarrowStringThroughEveryReader.
		w.to.text(reader.PackedString())
	case opBytes:
		w.to.blob(reader.Bytes())
	case opInt8s:
		signedArray(w, reader.Int8s(nil))
	case opInt16s:
		signedArray(w, reader.Int16s(nil))
	case opInt32s:
		signedArray(w, reader.Int32s(nil))
	case opInt64s:
		signedArray(w, reader.Ints(nil))
	case opUint16s:
		unsignedArray(w, reader.Uint16s(nil))
	case opUint32s:
		unsignedArray(w, reader.Uint32s(nil))
	case opUint64s:
		unsignedArray(w, reader.Uint64s(nil))
	case opStrings:
		w.textArray(reader.StringsBytes(nil))
	default:
		w.fail(errUnwalkableOp(op))
	}
}

// wideValue and wideScalar are narrowValue and narrowScalar at eight key bits.
// They are written out rather than shared behind an interface for the reason
// `wire` keeps the two widths in separate files: the readers are different types
// and boxing them would cost an allocation per field to save a switch.
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

func (w *walker) wideScalar(reader *wire.Reader8, op fieldOp) {
	switch op {
	case opBool:
		w.to.boolean(reader.Bool())
	case opInt8, opInt16, opInt32, opInt64:
		w.to.signed(reader.Int())
	case opUint8, opUint16:
		w.to.unsigned(uint64(reader.U16()))
	case opUint32:
		w.to.unsigned(uint64(reader.U32()))
	case opUint64:
		w.to.unsigned(reader.Uint())
	case opFloat32:
		w.floatValue(float64(reader.F32()), 32)
	case opFloat64:
		w.floatValue(reader.F64(), 64)
	case opString:
		// PackedString reads either encoding: the descriptor says which, so
		// packed5 costs this path nothing and needs no setting.
		w.to.text(reader.PackedString())
	case opBytes:
		w.to.blob(reader.Bytes())
	case opInt8s:
		signedArray(w, reader.Int8s(nil))
	case opInt16s:
		signedArray(w, reader.Int16s(nil))
	case opInt32s:
		signedArray(w, reader.Int32s(nil))
	case opInt64s:
		signedArray(w, reader.Ints(nil))
	case opUint16s:
		unsignedArray(w, reader.Uint16s(nil))
	case opUint32s:
		unsignedArray(w, reader.Uint32s(nil))
	case opUint64s:
		unsignedArray(w, reader.Uint64s(nil))
	case opStrings:
		w.textArray(reader.StringsBytes(nil))
	default:
		w.fail(errUnwalkableOp(op))
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
	count, elements, ok := reader.Counted()
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
// The row count is the message's claim, exactly as it is for the typed decoder
// in table.go: a peer that says four billion rows makes both of them allocate
// for four billion rows. That is the readers' standing behaviour rather than
// something this path introduces, and bounding it belongs in `wire` where both
// would get it.

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

func (w *walker) narrowTable(reader *wire.Reader, field *planField) {
	rows, columns, ok := reader.Counted()
	if !ok {
		w.fail(reader.Err())
		return
	}
	if rows > maxTableRows {
		w.fail(errTooManyRows)
		return
	}
	sub := field.sub
	if sub == nil {
		w.fail(errNoSubSchema)
		return
	}
	gathered := newTableColumns(len(sub.fields))
	for columns.More() {
		key := columns.Key()
		index := sub.findIndex(key)
		if index < 0 {
			// A narrow key cannot be skipped here either.
			w.fail(fmt.Errorf(
				"colbin: a table holds column %d, which the schema does not declare, "+
					"and a narrow key cannot be skipped", key))
			return
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

// structTable is wideTable over an opened one, for the reason structList is
// split out: a dynamic value reaches the same columns through a TYPED tag.
func (w *walker) structTable(rows int, columns *wire.Reader8, sub *typePlan) {
	if sub == nil {
		w.fail(errNoSubSchema)
		return
	}
	if rows > maxTableRows {
		w.fail(errTooManyRows)
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
// JSON objects are unordered and a Go map has no iteration order, so a
// schema-described message holding a map does not render to identical JSON bytes
// twice. That rules a map out of any golden-vector test and is worth writing
// down rather than discovering.
//
// An integer key becomes a quoted decimal, because a JSON object key is a
// string. Reading one back the other way is out of scope.

func (w *walker) narrowMap(reader *wire.Reader, field *planField) {
	count, entries, ok := reader.Counted()
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
		}
		if entries.Err() != nil {
			w.fail(entries.Err())
			return
		}
	}
	w.to.endObject()
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
		}
		if entries.Err() != nil {
			w.fail(entries.Err())
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
