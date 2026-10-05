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
//
// The table is checked as a whole as well as byte by byte, because a few bytes
// of well-formed definitions can still describe something no encoder wrote and
// no reader can walk: a struct that holds itself by value, which is an infinite
// value, or one whose zero value — which a walk renders for every field a
// message omits — is exponentially large. See checkValueNesting.

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
	if err := checkValueNesting(plans); err != nil {
		return nil, err
	}
	if err := checkPages(plans); err != nil {
		return nil, err
	}
	for _, plan := range plans {
		plan.indexKeys()
	}
	// Bytes past the last definition are left alone rather than refused: the
	// byteLength is what a reader steps over, and room behind the definitions is
	// where a later version would put something this one does not know about.
	return &Schema{
		plans:   plans,
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
	// A flag this version does not assign could change what every field means,
	// so it is refused rather than ignored.
	if flags := data[0]; flags&^(schemaWideKeys|schemaEnvelope|schemaPage) != 0 {
		return nil, fmt.Errorf(
			"colbin: a schema section sets struct flags %#02x, which this version does not assign",
			flags)
	}
	plan.isWide = data[0]&schemaWideKeys != 0
	plan.envelope = data[0]&schemaEnvelope != 0
	plan.page = data[0]&schemaPage != 0
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
	var seen [wire.MaxWideFields / 64]uint64
	for index := range count {
		if len(data) < 2 {
			return nil, errShortSection
		}
		key := data[0]
		if !plan.isWide && int(key) >= wire.MaxFields {
			return nil, fmt.Errorf(
				"colbin: a schema section gives a four-bit run key %d", key)
		}
		if seen[key/64]&(1<<(key%64)) != 0 {
			return nil, fmt.Errorf("colbin: a schema section declares key %d twice", key)
		}
		seen[key/64] |= 1 << (key % 64)
		plan.fields[index].key = key
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
		// A dynamic value names its own class, which four descriptor bits
		// cannot: the encoder puts every type holding one on the wide path.
		if field := &plan.fields[index]; !plan.isWide && (field.op == opAny ||
			field.op == opAnys || (field.op == opMap && field.valueKind == mapAny)) {
			return nil, fmt.Errorf(
				"colbin: a schema section puts a dynamic value under a four-bit key")
		}
	}
	return data, nil
}

// maxZeroFields bounds the zero value of a parsed struct, counted in fields
// with every by-value struct expanded. A walk writes that zero for each struct
// field a message omits, so it is work a peer can ask for without sending the
// bytes for it; a million fields is far past any type a program declares.
const maxZeroFields = 1 << 20

// checkValueNesting refuses a struct table that holds a struct inside itself by
// value, at any distance — an infinite value, which no Go type can be and no
// encoder wrote — or whose by-value nesting is deeper than a walk goes or wider
// than maxZeroFields. A list, a table or a pointer of structs is not counted:
// each can be empty, which is where a recursive type ends.
func checkValueNesting(plans []*typePlan) error {
	measure := nesting{
		index: make(map[*typePlan]int, len(plans)),
		zero:  make([]int, len(plans)),
		depth: make([]int, len(plans)),
	}
	for index, plan := range plans {
		measure.index[plan] = index
	}
	for _, plan := range plans {
		if _, _, err := measure.of(plan); err != nil {
			return err
		}
	}
	return nil
}

// nesting is checkValueNesting's memo: each struct's zero-value size and
// by-value depth, once known.
type nesting struct {
	index map[*typePlan]int
	// zero is 0 while a struct is unmeasured and -1 while it is being measured,
	// which is what finds a cycle: meeting a -1 is meeting a struct inside
	// itself.
	zero  []int
	depth []int
}

func (measure *nesting) of(plan *typePlan) (zero, depth int, err error) {
	at := measure.index[plan]
	switch measure.zero[at] {
	case -1:
		return 0, 0, fmt.Errorf("colbin: a schema section holds a struct inside itself by value")
	case 0:
	default:
		return measure.zero[at], measure.depth[at], nil
	}
	measure.zero[at] = -1
	zero, depth = 1, 1
	for _, field := range plan.fields {
		if field.op != opStruct {
			zero++
			continue
		}
		subZero, subDepth, err := measure.of(field.sub)
		if err != nil {
			return 0, 0, err
		}
		zero += subZero
		depth = max(depth, subDepth+1)
		if zero > maxZeroFields || depth > maxSchemaDepth {
			return 0, 0, fmt.Errorf(
				"colbin: a schema section nests structs by value deeper or wider than a walk goes")
		}
	}
	measure.zero[at], measure.depth[at] = zero, depth
	return zero, depth, nil
}

// checkPages refuses a page reached any way but the one an encoder writes: as
// the struct under key 255 of a wide run. A walk merges a page into the object
// that links it, so a page at the root, in a list, behind a pointer or under any
// other key has no object to merge into.
func checkPages(plans []*typePlan) error {
	if plans[0].page {
		return fmt.Errorf("colbin: a schema section's root is a page")
	}
	for _, plan := range plans {
		for index := range plan.fields {
			field := &plan.fields[index]
			if field.sub == nil || !field.sub.page {
				continue
			}
			if field.op != opStruct || field.key != pageLink || !plan.isWide {
				return fmt.Errorf(
					"colbin: a schema section reaches a page other than through key %d of a wide struct",
					pageLink)
			}
		}
	}
	return nil
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
	case opStruct, opStructs, opPointerStruct:
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
