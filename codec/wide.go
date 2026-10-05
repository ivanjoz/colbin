package codec

// The wide-key path, and the root descriptor that says which path a message
// took.
//
// A message is one value: a descriptor byte, then its payload. The root
// descriptor names the class — always a struct, here — the key width used
// inside it, and whether a schema section comes first.
//
// # Choosing the width
//
// Narrow keys are the default: a byte less per field, and a faster decode. A
// type goes wide when it has to:
//
//   - a field id above sixteen, whose key four bits cannot carry;
//   - an untagged field, whose key is derived from its name and lands anywhere
//     in 0..255; or
//   - a dynamic value, whose descriptor has to name its own class and cannot do
//     that in four bits (dynamic.go).
//
// The choice is a property of the type, not of the values, so it is resolved
// once per type and then read from the plan.

import (
	"fmt"
	"reflect"
	"unsafe"

	"github.com/ivanjoz/colbin/wire"
)

// Root descriptors; INTERNALS.md §2.1 lists them. They are the STRUCT class of
// an ordinary K8 descriptor, which is what puts every colbin message in
// 0xD0..0xDF — see root.go for the reservation that follows from it, and for the
// 240 first bytes an application may claim.
const (
	rootStructNarrow byte = RootFirst
	rootStructWide   byte = RootFirst | rootWide
	// The same two with a schema section in front of the body, which is what
	// MarshalSelfDescribing writes. See schema.go.
	rootStructNarrowSchema byte = RootFirst | rootSchema
	rootStructWideSchema   byte = RootFirst | rootWide | rootSchema
)

// wide reports whether a plan must use eight-bit keys.
func (plan *typePlan) wide() bool {
	return plan.anyKeyPastNarrow || plan.derivedKeys || plan.hasDynamic
}

// appendWide is writePlan for the wide key width.
func appendWide(writer *wire.Writer8, plan *typePlan, record unsafe.Pointer, buf *scratch) {
	if !buf.enter() {
		return
	}
	defer buf.leave()
	packed := Packed5()
	for index := range plan.fields {
		field := &plan.fields[index]
		at := unsafe.Add(record, field.offset)
		if field.indirect {
			if at = *(*unsafe.Pointer)(at); at == nil {
				continue
			}
		}
		switch field.op {
		case opStruct:
			appendStructField(writer, field, at, buf)
		case opStructs:
			appendStructsField(writer, field, at, buf)
		case opMap:
			appendMapField(writer, field, at, buf)
		case opPointer:
			appendPointerWide(writer, field, at, packed)
		case opPointerStruct:
			appendPointerStructField(writer, field, at, buf)
		case opAny:
			appendAnyField(writer, field, at, buf)
		case opAnys:
			appendAnysField(writer, field, at, buf)
		default:
			if !writeValueWide(writer, field.key, field.op, at, packed) {
				buf.fail(errNotValueOp(field.op))
			}
		}
	}
}

// readWideField is readField for the wide key width.
func readWideField(reader *wire.Reader8, field *planField, record unsafe.Pointer, buf *scratch) {
	at := unsafe.Add(record, field.offset)
	if field.indirect {
		at = newPointee(field, at)
	}
	switch field.op {
	case opStruct:
		readStructField(reader, field, at, buf)
	case opStructs:
		readStructsField(reader, field, at, buf)
	case opMap:
		readMapField(reader, field, at, buf)
	case opPointer:
		readPointerWide(reader, field, at)
	case opPointerStruct:
		readPointerStructField(reader, field, at, buf)
	case opAny:
		// The key is a field's and everything behind it is a key-less value's,
		// so the cursor steps onto the value first.
		if reader.Payload() {
			*(*any)(at) = readAnyValue(reader, buf)
		}
	case opAnys:
		if reader.Payload() {
			*(*[]any)(at) = readAnyList(reader, buf)
		}
	default:
		if !readValueWide(reader, field.op, at) {
			reader.Fail(errNotValueOp(field.op))
		}
	}
}

// unmarshalWide decodes a wide-key message into an already-zeroed record.
//
// A key the plan does not declare is *skipped* rather than refused, which is the
// whole point of the wide width: the descriptor sizes the field, so a reader can
// step over something a newer peer added. The narrow path cannot, and says so.
func unmarshalWide(body, section []byte, plan *typePlan, record unsafe.Pointer) error {
	// A separate reader per branch, for the reason appendPlan declares a separate
	// writer: escape analysis is per variable, and sharing one with the composite
	// walk would heap it on the flat path too.
	if plan.simple {
		reader := wire.NewReader8(body)
		readScalarsWide(&reader, plan, record)
		return reader.Err()
	}
	return unmarshalWideRun(body, section, plan, record)
}

// unmarshalWideRun is unmarshalWide for a plan that nests, out of line for the
// reason unmarshalNarrowRun is.
func unmarshalWideRun(body, section []byte, plan *typePlan, record unsafe.Pointer) error {
	reader := wire.NewReader8(body)
	buf := scratch{rawSection: section, rowsLeft: rowBudget(len(body))}
	defer buf.release()
	readRun(&reader, plan, record, &buf)
	return reader.Err()
}

// rootOf reads a message's root descriptor and returns the body after it.
//
// It reports failure as a bool and leaves the message to the caller, rather than
// taking the value to name it in an error. Taking it as an `any` boxed the
// record on every decode — one allocation and about 8 ns on a ten-field one,
// entirely to describe a failure that does not happen.
func rootOf(data []byte) (body []byte, wide, ok bool) {
	_, body, wide, ok = rootParts(data)
	return body, wide, ok
}

// rootParts is rootOf with the section kept rather than stepped over.
//
// A typed decode does not need it — it has the Go type — which is what keeps a
// self-describing message an ordinary message to everyone else. One thing does:
// a dynamic value can name a struct the *section* describes, and nothing in the
// Go type says what that struct is. So the bytes are carried along unparsed and
// looked at only if such a value turns up. See scratch.structPlans.
func rootParts(data []byte) (section, body []byte, wide, ok bool) {
	if len(data) == 0 {
		return nil, nil, false, false
	}
	switch data[0] {
	case rootStructNarrow:
		return nil, data[1:], false, true
	case rootStructWide:
		return nil, data[1:], true, true
	case rootStructNarrowSchema, rootStructWideSchema:
		section, body, ok := splitSchemaSection(data[1:])
		return section, body, data[0]&rootWide != 0, ok
	default:
		return nil, nil, false, false
	}
}

// errBadRoot names what rootOf refused. It is out of line so that the happy path
// carries neither the formatting nor the type it would need.
func errBadRoot(data []byte, what reflect.Type) error {
	if len(data) == 0 {
		return fmt.Errorf("colbin: %s: empty message", what)
	}
	return fmt.Errorf(
		"colbin: %s: byte 0 is %#02x, which is not a root descriptor this version writes",
		what, data[0])
}

// rootTypeOf resolves the type behind a value or a pointer to one. What may
// stand at a root is planForRoot's business, not this one's.
func rootTypeOf(v any) (reflect.Type, error) {
	rootType := reflect.TypeOf(v)
	for rootType != nil && rootType.Kind() == reflect.Pointer {
		rootType = rootType.Elem()
	}
	if rootType == nil {
		return nil, fmt.Errorf("colbin: expected a struct, got nil")
	}
	return rootType, nil
}
