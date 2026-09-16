package codec

// The schema section: the type, on the wire, for a reader that has not got it.
//
// colbin's speed argument is that the type is not on the wire. A field is a key
// and a payload; what that payload *means* — signed or unsigned, float or
// integer, string or blob, which field of which struct — comes from the schema
// both sides already have. A browser, a `jq`-style tool or a dynamically typed
// client has no such schema, and no amount of descriptor gets it one: a K8 INT
// descriptor says "an integer of n bytes", not "TaxCents, an int64". Skipping is
// not understanding.
//
// So the feature is to ship the schema, and it needs no new format to do it.
// `typePlan` already *is* the schema — it is what reflect is boiled down to
// before any encoding happens. Strip `offset`, `sliceType` and `stride`, the
// three things that exist only to write into a Go struct, and what is left is a
// key, a name and an op, plus a child plan for the composites. This file
// serialises that; schema_plan.go parses it back; json.go walks a message with
// it in place of reflect.
//
// # Layout
//
//	section   := [byteLength] [structCount] structDef{structCount}
//	structDef := [flags:1] [fieldCount] field{fieldCount}
//	flags     := [wideKeys:1] [envelope:1] [reserved:6]
//	field     := [key:1] [nameLen] name [desc]
//	desc      := [op:1] extra
//
// `extra` by op:
//
//	scalars, string, bytes, arrays   —   (the op names the element type)
//	opStruct, opStructs              [structIndex]
//	opPointerStruct                  [structIndex]
//	opMap                            [keyKind:1] [valueKind:1]
//	opPointer                        [elemOp:1]
//
// Every length is wire.AppendLength — one byte escaping to four, the format's
// own rule — rather than a varint, for the reason nothing else here is a varint.
// byteLength covers everything after itself, so a reader that wants the body and
// not the schema adds it to the cursor and is done.
//
// # Struct hoisting
//
// Structs are an indexed table rather than inlined, so that a self-referential
// type (`type Node struct{ Kids []Node }`) describes itself in finite space. The
// index is reserved before the fields are walked, so a back-edge resolves to an
// index already assigned — the same trick planForBuilding plays with `building`.
//
// # Why a structDef carries its key width
//
// Almost every run on the wire says its own width: rootOf for the root, the k8
// bit in the STRUCT descriptor for a nested one, the enclosing writer for a
// table's columns. One shape does not. A *narrow* list's element is a length and
// a body with no descriptor between them — deliberately, because that byte per
// element is what makes a narrow list of small structs smaller than a wide one —
// and its key width is the schema's to know. So the schema says it, for every
// struct rather than only that one, because one rule is cheaper to hold than an
// exception.
//
// That is also why SetPacked5 drops this cache: packed5 is one of the two things
// that decide a type's key width.
//
// # What it costs
//
// The `fieldOp` constants and the `mapKind` constants stop being an
// implementation detail and become format. See the comment on each.

import (
	"fmt"
	"reflect"
	"sync"
	"unsafe"

	"github.com/ivanjoz/colbin/wire"
)

// structDef flags.
const (
	// schemaWideKeys says the run it describes uses eight-bit keys.
	schemaWideKeys uint8 = 0x01
	// schemaEnvelope says this def is the synthetic one-field struct that carries
	// a slice or a map at the root, so a reader building a document should take
	// the field's value and drop the wrapper. See envelope.go.
	//
	// It is a flag on every structDef rather than on the section, for the reason
	// the key width is: one rule per definition is cheaper to hold than a rule
	// plus an exception about which definition it applies to.
	schemaEnvelope uint8 = 0x02
)

// Schema is a type described in bytes rather than in Go: what a decoder needs to
// turn a message into JSON without the type that wrote it.
//
// There are three ways to one, and they produce the same thing:
//
//	schema, err := codec.SchemaFor[Sale]()   // from the Go type
//	schema, err := codec.SchemaOf(sale)      // from a value
//	schema, err := codec.ParseSchema(bytes)  // from the wire
//
// Send Bytes() once per connection and then send ordinary messages, which is the
// delivery that costs nothing per message. MarshalSelfDescribing is the other
// one, for a document that has to stand alone.
//
// A Schema is immutable and safe for concurrent use.
type Schema struct {
	plan *typePlan
	// plans is the struct table, root first, in the order the section names
	// them. A walk needs it because a dynamic value can carry a struct *index*
	// rather than a type — that is what keeps an array of records inside an
	// `any` from writing its field names per row. See dynamic.go.
	plans   []*typePlan
	section []byte
}

// Bytes is the section, for sending to the other side. It is a copy: the schema
// behind it is cached per type and shared, and a caller that trimmed or appended
// to it would be editing every later caller's copy too.
func (schema *Schema) Bytes() []byte {
	return append([]byte(nil), schema.section...)
}

// Size is len(Bytes()) without the copy, for a caller counting bytes.
func (schema *Schema) Size() int { return len(schema.section) }

// schemaCache holds one section per root type. Building it walks the plan and
// allocates, and a section is a property of the type alone.
var schemaCache sync.Map // reflect.Type -> *Schema or error

// SchemaFor describes T, which must be a type the format accepts at the root: a
// struct, or a slice or map the envelope carries.
func SchemaFor[T any]() (*Schema, error) {
	var zero T
	rootType, err := rootTypeOf(zero)
	if err != nil {
		return nil, err
	}
	return schemaForRoot(rootType)
}

// SchemaOf describes the type of v, which must be a struct, a pointer to one, or
// a slice or map the envelope carries.
func SchemaOf(v any) (*Schema, error) {
	rootType, err := rootTypeOf(v)
	if err != nil {
		return nil, err
	}
	return schemaForRoot(rootType)
}

// schemaForRoot describes whatever a message's root would be, which is the root
// type's own plan when it is a struct and its envelope's otherwise.
//
// The cache is keyed on the type the caller named rather than on the envelope,
// so `[]Grant` and `Grant` are different entries and neither can be handed back
// for the other.
func schemaForRoot(rootType reflect.Type) (*Schema, error) {
	if cached, ok := schemaCache.Load(rootType); ok {
		if schema, ok := cached.(*Schema); ok {
			return schema, nil
		}
		return nil, cached.(error)
	}
	plan, err := planForRoot(rootType)
	if err != nil {
		schemaCache.Store(rootType, err)
		return nil, err
	}
	builder := newSectionBuilder(plan)
	schema := &Schema{plan: plan, plans: builder.plans, section: builder.bytes()}
	schemaCache.Store(rootType, schema)
	return schema, nil
}

// newSectionBuilder starts a table with root at index 0, by asking for it first.
func newSectionBuilder(root *typePlan) *sectionBuilder {
	builder := &sectionBuilder{index: make(map[*typePlan]int, 4)}
	builder.structIndex(root)
	return builder
}

// sectionBuilder collects the struct table while the descriptors are emitted.
//
// It is reachable from the *encoder* as well as from here, because a dynamic
// value can name a struct the type alone never mentions — `map[string]any`
// holding a `[]User` is described by walking the value, not the type. So the
// table grows while the body is written, and bytes() is called after. See
// marshalDynamic in dynamic.go.
type sectionBuilder struct {
	index map[*typePlan]int
	defs  [][]byte
	// plans is defs' parallel: what each index describes, for a reader that has
	// the plans already and wants the numbering rather than the bytes.
	plans []*typePlan
}

// bytes serialises the table as it stands.
func (builder *sectionBuilder) bytes() []byte {
	body := wire.AppendLength(make([]byte, 0, 64), len(builder.defs))
	for _, def := range builder.defs {
		body = append(body, def...)
	}
	section := wire.AppendLength(make([]byte, 0, len(body)+5), len(body))
	return append(section, body...)
}

// structIndex returns a plan's slot in the table, describing it on first use.
//
// The slot is reserved before the fields are walked, so a field that reaches
// back to this plan resolves to this index rather than recursing forever.
func (builder *sectionBuilder) structIndex(plan *typePlan) int {
	if at, ok := builder.index[plan]; ok {
		return at
	}
	at := len(builder.defs)
	builder.index[plan] = at
	builder.defs = append(builder.defs, nil) // reserved, filled in below
	builder.plans = append(builder.plans, plan)

	flags := uint8(0)
	if plan.isWide {
		flags |= schemaWideKeys
	}
	if plan.envelope {
		flags |= schemaEnvelope
	}
	def := wire.AppendLength([]byte{flags}, len(plan.fields))
	for index := range plan.fields {
		field := &plan.fields[index]
		name := plan.names[index]
		def = wire.AppendLength(append(def, field.key), len(name))
		def = builder.appendDesc(append(def, name...), field)
	}
	builder.defs[at] = def
	return at
}

// appendDesc writes one field's type: the op, and whatever the op does not say
// by itself.
func (builder *sectionBuilder) appendDesc(dst []byte, field *planField) []byte {
	dst = append(dst, uint8(field.op))
	switch field.op {
	case opStruct, opStructs, opPointerStruct:
		return wire.AppendLength(dst, builder.structIndex(field.sub))
	case opMap:
		return append(dst, uint8(field.keyKind), uint8(field.valueKind))
	case opPointer:
		return append(dst, uint8(field.elemOp))
	}
	// Everything else is named by its op alone: an array's element type is in
	// the op, and a string and a blob are different ops.
	return dst
}

// MarshalSelfDescribing encodes v with its schema section in front of it, so the
// message stands alone:
//
//	[0xD4 or 0xDC] [section] [the ordinary body]
//
// The body is byte for byte what Marshal writes, and Unmarshal accepts either
// form — it steps over a section it does not need. So a self-describing message
// decodes into the Go type *and* into JSON, which is the property worth keeping.
//
// v may be a slice or a map as well as a struct, and the envelope that carries
// one (envelope.go) does not appear in the JSON: the document is the array or
// the object, not a wrapper holding it.
//
// It is the wrong default. For the corpus Sale type the section is 173 bytes
// against a mean body of 90, so a stream that sends it per message sends the
// schema nearly twice over for every record. Send Schema.Bytes() once per
// connection instead, and keep this for the single document that has nowhere to
// put one. `go test ./corpus -run ReportSchema -v` prints the ratio per table.
func MarshalSelfDescribing(v any) ([]byte, error) {
	value, plan, err := addressable(v)
	if err != nil {
		return nil, err
	}
	record := unsafe.Pointer(value.UnsafeAddr())
	root := rootStructNarrowSchema
	if plan.isWide {
		root = rootStructWideSchema
	}
	if plan.hasDynamic {
		return marshalDynamic(root, plan, record)
	}
	schema, err := schemaForRoot(value.Type())
	if err != nil {
		return nil, err
	}
	dst := make([]byte, 0, 1+len(schema.section)+plan.sizeHint)
	dst = append(append(dst, root), schema.section...)
	body, err := appendRun(dst, plan, record)
	return body, err
}

// marshalDynamic is MarshalSelfDescribing for a type that can reach a struct the
// type itself never names.
//
// The section cannot be resolved from the type and then written, because a
// `map[string]any` holding a `[]Sale` says nothing about Sale until the value is
// walked. So the body goes into its own buffer with the table open, the walk
// adds every struct it meets, and the section is serialised afterwards — which
// is also why this one is not cached, while every static type's is.
func marshalDynamic(root byte, plan *typePlan, record unsafe.Pointer) ([]byte, error) {
	builder := newSectionBuilder(plan)
	buf := scratch{section: builder}
	body, err := appendRunInto(make([]byte, 0, plan.sizeHint), plan, record, &buf)
	if err != nil {
		return nil, err
	}
	section := builder.bytes()
	dst := make([]byte, 0, 1+len(section)+len(body))
	return append(append(append(dst, root), section...), body...), nil
}

// appendRun writes a plan's key run with no root descriptor in front of it,
// which is what a message whose first byte is already spoken for needs.
//
// It is deliberately not appendPlan with the root byte hoisted out. appendPlan
// is the hot path and is shaped around escape analysis — a writer declared per
// branch, and the flat walk split out so that neither the scratch buffer nor the
// writer escapes. This one runs once per self-describing message, so it takes
// the general walk and none of that care.
func appendRun(dst []byte, plan *typePlan, record unsafe.Pointer) ([]byte, error) {
	var buf scratch
	return appendRunInto(dst, plan, record, &buf)
}

// appendRunInto is appendRun with the caller's scratch, which is what carries
// the section table a dynamic value writes into.
func appendRunInto(
	dst []byte, plan *typePlan, record unsafe.Pointer, buf *scratch,
) ([]byte, error) {
	out := appendRunBytes(dst, plan, record, buf)
	if buf.err != nil {
		return nil, buf.err
	}
	return out, nil
}

func appendRunBytes(
	dst []byte, plan *typePlan, record unsafe.Pointer, buf *scratch,
) []byte {
	if plan.isWide {
		writer := wire.Writer8{Buffer: dst}
		appendWide(&writer, plan, record, buf)
		return writer.Buffer
	}
	writer := wire.Writer{Buffer: dst}
	writePlan(&writer, plan, record, buf)
	return writer.Buffer
}

// splitSchemaSection divides the bytes after a schema-carrying root byte into
// the section — its own length prefix included, so that ParseSchema takes what
// Schema.Bytes gives — and the message body behind it.
func splitSchemaSection(data []byte) (section, body []byte, ok bool) {
	length, width, ok := wire.ReadLength(data)
	if !ok || length > len(data)-width {
		return nil, nil, false
	}
	return data[:width+length], data[width+length:], true
}

// errNoSection is what a decode without a schema says when the message has none
// either. It names the two ways out rather than only the failure.
func errNoSection(data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("colbin: empty message")
	}
	return fmt.Errorf(
		"colbin: byte 0 is %#02x, which carries no schema section: "+
			"pass the schema, or encode with MarshalSelfDescribing", data[0])
}
