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
//	field     := [key:1] [nameLen] name [desc]
//	desc      := [op:1] extra
//
// `extra` by op:
//
//	scalars, string, bytes, arrays   —   (the op names the element type)
//	opStruct, opStructs              [structIndex]
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

// schemaWideKeys is the structDef flag saying the run it describes uses
// eight-bit keys.
const schemaWideKeys uint8 = 0x01

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
	plan    *typePlan
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

// SchemaFor describes T, which must be a struct the format accepts.
func SchemaFor[T any]() (*Schema, error) {
	var zero T
	structType, err := structTypeOf(zero)
	if err != nil {
		return nil, err
	}
	return schemaForType(structType)
}

// SchemaOf describes the type of v, which must be a struct or a pointer to one.
func SchemaOf(v any) (*Schema, error) {
	structType, err := structTypeOf(v)
	if err != nil {
		return nil, err
	}
	return schemaForType(structType)
}

func schemaForType(structType reflect.Type) (*Schema, error) {
	if cached, ok := schemaCache.Load(structType); ok {
		if schema, ok := cached.(*Schema); ok {
			return schema, nil
		}
		return nil, cached.(error)
	}
	plan, err := planFor(structType)
	if err != nil {
		schemaCache.Store(structType, err)
		return nil, err
	}
	schema := &Schema{plan: plan, section: buildSection(plan)}
	schemaCache.Store(structType, schema)
	return schema, nil
}

// buildSection serialises a plan and everything it reaches.
func buildSection(root *typePlan) []byte {
	builder := sectionBuilder{index: make(map[*typePlan]int, 4)}
	builder.structIndex(root) // the root is index 0, by being asked for first

	body := wire.AppendLength(make([]byte, 0, 64), len(builder.defs))
	for _, def := range builder.defs {
		body = append(body, def...)
	}
	section := wire.AppendLength(make([]byte, 0, len(body)+5), len(body))
	return append(section, body...)
}

// sectionBuilder collects the struct table while the descriptors are emitted.
type sectionBuilder struct {
	index map[*typePlan]int
	defs  [][]byte
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

	flags := uint8(0)
	if plan.isWide {
		flags = schemaWideKeys
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
	case opStruct, opStructs:
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
	schema, err := schemaForType(value.Type())
	if err != nil {
		return nil, err
	}
	root := rootStructNarrowSchema
	if plan.isWide {
		root = rootStructWideSchema
	}
	dst := make([]byte, 0, 1+len(schema.section)+plan.sizeHint)
	dst = append(append(dst, root), schema.section...)
	return appendRun(dst, plan, unsafe.Pointer(value.UnsafeAddr())), nil
}

// appendRun writes a plan's key run with no root descriptor in front of it,
// which is what a message whose first byte is already spoken for needs.
//
// It is deliberately not appendPlan with the root byte hoisted out. appendPlan
// is the hot path and is shaped around escape analysis — a writer declared per
// branch, and the flat walk split out so that neither the scratch buffer nor the
// writer escapes. This one runs once per self-describing message, so it takes
// the general walk and none of that care.
func appendRun(dst []byte, plan *typePlan, record unsafe.Pointer) []byte {
	var buf scratch
	if plan.isWide {
		writer := wire.Writer8{Buffer: dst}
		appendWide(&writer, plan, record, &buf)
		return writer.Buffer
	}
	writer := wire.Writer{Buffer: dst}
	writePlan(&writer, plan, record, &buf)
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
