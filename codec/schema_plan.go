package codec

// Reading a schema section back into a plan.
//
// This is the other half of schema.go, and the shape it produces is the same
// `typePlan` the reflective side builds — minus the three fields that exist only
// to write into a Go struct. `offset`, `sliceType` and `stride` stay zero and
// `fromSchema` is set, because a plan parsed from bytes describes a type that
// may not exist in this program at all.
//
// # Everything here is untrusted
//
// A section arrives from a peer, so every length is checked against what is
// actually there and every code against what this version assigns. Nothing may
// panic, and nothing may allocate on a number the section merely claims: a
// struct table is bounded by the bytes left to hold it, which is the cheapest
// honest bound there is.

import (
	"fmt"

	"github.com/ivanjoz/colbin/wire"
)

// The smallest a definition can be, which is what bounds a declared count
// against the bytes actually left: a structDef is a flags byte and a field
// count, and a field is a key, a name length and an op.
const (
	minStructDefBytes = 2
	minFieldBytes     = 3
)

// ParseSchema reads a section written by Schema.Bytes, or the one carried in
// front of a self-describing message.
func ParseSchema(section []byte) (*Schema, error) {
	length, width, ok := wire.ReadLength(section)
	if !ok || length > len(section)-width {
		return nil, fmt.Errorf(
			"colbin: a schema section declares a length %d bytes cannot hold", len(section))
	}
	body := section[width : width+length]

	count, at, ok := wire.ReadLength(body)
	if !ok {
		return nil, errShortSection
	}
	rest := body[at:]
	if count == 0 {
		return nil, fmt.Errorf("colbin: a schema section describes no struct")
	}
	if count > len(rest)/minStructDefBytes {
		return nil, fmt.Errorf(
			"colbin: a schema section declares %d structs and has %d bytes of definitions",
			count, len(rest))
	}
	// Every plan exists before any is filled, so a field may point forward as
	// well as back — a struct table is an index, not an order.
	plans := make([]*typePlan, count)
	for index := range plans {
		plans[index] = &typePlan{fromSchema: true}
	}
	for _, plan := range plans {
		var err error
		if rest, err = parseStructDef(plan, plans, rest); err != nil {
			return nil, err
		}
	}
	for _, plan := range plans {
		plan.indexKeys()
	}
	// Bytes past the last definition are left alone rather than refused: the
	// byteLength is what a reader steps over, and room behind the definitions is
	// where a later version would put something this one does not know about.
	return &Schema{
		plan:    plans[0],
		section: append([]byte(nil), section[:width+length]...),
	}, nil
}

// errShortSection is the one every length check ends at. It is a single error
// rather than one per field because a section that ends early is corrupt or
// truncated, and which byte ran out first says nothing a caller can act on.
var errShortSection = fmt.Errorf("colbin: a schema section ends inside a definition")

// parseStructDef fills one plan and returns what is left of the table.
func parseStructDef(plan *typePlan, plans []*typePlan, data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, errShortSection
	}
	plan.isWide = data[0]&schemaWideKeys != 0
	data = data[1:]

	count, at, ok := wire.ReadLength(data)
	if !ok {
		return nil, errShortSection
	}
	data = data[at:]
	if count > len(data)/minFieldBytes {
		return nil, errShortSection
	}
	if count > wire.MaxWideFields {
		return nil, fmt.Errorf(
			"colbin: a schema section declares %d fields, and a one-byte key holds %d",
			count, wire.MaxWideFields)
	}
	plan.fields = make([]planField, count)
	plan.names = make([]string, count)
	for index := range count {
		if len(data) < 2 {
			return nil, errShortSection
		}
		plan.fields[index].key = data[0]
		nameLength, width, ok := wire.ReadLength(data[1:])
		if !ok || nameLength > len(data)-1-width {
			return nil, errShortSection
		}
		plan.names[index] = string(data[1+width : 1+width+nameLength])
		data = data[1+width+nameLength:]

		var err error
		if data, err = parseDesc(&plan.fields[index], plans, data); err != nil {
			return nil, err
		}
	}
	return data, nil
}

// parseDesc reads one field's type.
func parseDesc(field *planField, plans []*typePlan, data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, errShortSection
	}
	op, err := opCode(data[0])
	if err != nil {
		return nil, err
	}
	field.op = op
	data = data[1:]

	switch op {
	case opStruct, opStructs:
		index, width, ok := wire.ReadLength(data)
		if !ok {
			return nil, errShortSection
		}
		if index >= len(plans) {
			return nil, fmt.Errorf(
				"colbin: a schema section points at struct %d of %d", index, len(plans))
		}
		field.sub = plans[index]
		return data[width:], nil
	case opMap:
		if len(data) < 2 {
			return nil, errShortSection
		}
		if field.keyKind, err = mapKindCode(data[0]); err != nil {
			return nil, err
		}
		if field.valueKind, err = mapKindCode(data[1]); err != nil {
			return nil, err
		}
		return data[2:], nil
	case opPointer:
		if len(data) < 1 {
			return nil, errShortSection
		}
		if field.elemOp, err = opCode(data[0]); err != nil {
			return nil, err
		}
		return data[1:], nil
	}
	return data, nil
}

// opCode and mapKindCode refuse a number this version does not assign, rather
// than indexing a switch on it. Every byte is a plausible op, so an unassigned
// one is not a value to pass through — it is a section written by something
// newer, and reading it would retype a field silently.

func opCode(value uint8) (fieldOp, error) {
	if fieldOp(value) >= opCount {
		return 0, fmt.Errorf(
			"colbin: a schema section names field type %d, which this version does not assign",
			value)
	}
	return fieldOp(value), nil
}

func mapKindCode(value uint8) (mapKind, error) {
	if mapKind(value) >= mapKindCount {
		return 0, fmt.Errorf(
			"colbin: a schema section names map kind %d, which this version does not assign",
			value)
	}
	return mapKind(value), nil
}
