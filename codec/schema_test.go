package codec

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// TestFieldOpsArePinned is the test the schema section makes necessary.
//
// An op is one byte on the wire now. Reordering the const block would retype
// every field of every section already written — silently, because every byte is
// a valid op. If this fails, the fix is never to change the numbers here: it is
// to put the new op at the end of the block where it belongs.
func TestFieldOpsArePinned(t *testing.T) {
	for name, pinned := range map[string]struct {
		op   fieldOp
		want uint8
	}{
		"opBool":    {opBool, 0},
		"opInt8":    {opInt8, 1},
		"opInt16":   {opInt16, 2},
		"opInt32":   {opInt32, 3},
		"opInt64":   {opInt64, 4},
		"opUint8":   {opUint8, 5},
		"opUint16":  {opUint16, 6},
		"opUint32":  {opUint32, 7},
		"opUint64":  {opUint64, 8},
		"opFloat32": {opFloat32, 9},
		"opFloat64": {opFloat64, 10},
		"opString":  {opString, 11},
		"opBytes":   {opBytes, 12},
		"opInt8s":   {opInt8s, 13},
		"opInt16s":  {opInt16s, 14},
		"opInt32s":  {opInt32s, 15},
		"opInt64s":  {opInt64s, 16},
		"opUint16s": {opUint16s, 17},
		"opUint32s": {opUint32s, 18},
		"opUint64s": {opUint64s, 19},
		"opStrings": {opStrings, 20},
		"opStruct":  {opStruct, 21},
		"opStructs": {opStructs, 22},
		"opMap":     {opMap, 23},
		"opPointer": {opPointer, 24},
		"opAny":     {opAny, 25},
		"opAnys":    {opAnys, 26},
	} {
		if uint8(pinned.op) != pinned.want {
			t.Errorf("%s is %d, and the wire says %d", name, pinned.op, pinned.want)
		}
	}
	if opCount != 27 {
		t.Errorf("there are %d ops; a new one goes on the end and this number follows it",
			opCount)
	}
}

// The map kinds are on the wire for the same reason, and the two float widths
// are separate kinds because a section is the only thing that can say which.
func TestMapKindsArePinned(t *testing.T) {
	for name, pinned := range map[string]struct {
		kind mapKind
		want uint8
	}{
		"mapString":  {mapString, 0},
		"mapInt":     {mapInt, 1},
		"mapUint":    {mapUint, 2},
		"mapFloat64": {mapFloat64, 3},
		"mapBool":    {mapBool, 4},
		"mapFloat32": {mapFloat32, 5},
		"mapAny":     {mapAny, 6},
	} {
		if uint8(pinned.kind) != pinned.want {
			t.Errorf("%s is %d, and the wire says %d", name, pinned.kind, pinned.want)
		}
	}
}

// recursive is the type struct hoisting exists for: it cannot be described by
// inlining, because inlining it does not terminate.
type recursive struct {
	Name string      `cb:"1"`
	Kids []recursive `cb:"2"`
}

// schemaCases is every shape the section has a rule for.
func schemaCases() []any {
	return []any{
		charge{},
		everyShape{},
		outer{},
		wideOuter{},
		pointers{},
		widePointers{},
		withMaps{},
		recursive{},
		bare{},
	}
}

// The property the whole of schema_plan.go exists for: bytes in, the same plan
// out. If this holds for a type, a reader with the section knows exactly what a
// reader with the Go type knows, minus the layout.
func TestSchemaRoundTripsThroughItsSection(t *testing.T) {
	for _, value := range schemaCases() {
		built, err := SchemaOf(value)
		if err != nil {
			t.Fatalf("SchemaOf(%T): %v", value, err)
		}
		parsed, err := ParseSchema(built.Bytes())
		if err != nil {
			t.Fatalf("ParseSchema(%T): %v", value, err)
		}
		samePlan(t, fmt.Sprintf("%T", value), parsed.plan, built.plan,
			map[[2]*typePlan]bool{})
		if !parsed.plan.fromSchema {
			t.Fatalf("%T: a parsed plan is not marked as one, and its offsets are zero",
				value)
		}
		// Serialising what was parsed has to give the bytes back, or the two
		// sides do not agree on what the section says.
		if again := newSectionBuilder(parsed.plan).bytes(); !bytes.Equal(again, built.section) {
			t.Fatalf("%T: re-serialising the parsed plan gave\n%x\nwant\n%x",
				value, again, built.section)
		}
	}
}

// samePlan compares a parsed plan against the reflective one it came from,
// ignoring exactly the three things a section deliberately drops: offset, stride
// and the slice type.
func samePlan(t *testing.T, path string, got, want *typePlan, seen map[[2]*typePlan]bool) {
	t.Helper()
	if seen[[2]*typePlan{got, want}] {
		return // a cycle, which is what the struct table is for
	}
	seen[[2]*typePlan{got, want}] = true

	if len(got.fields) != len(want.fields) {
		t.Fatalf("%s has %d fields, want %d", path, len(got.fields), len(want.fields))
	}
	if got.isWide != want.isWide {
		t.Fatalf("%s: key width is wide=%v, want %v", path, got.isWide, want.isWide)
	}
	for index := range want.fields {
		gotField, wantField := &got.fields[index], &want.fields[index]
		where := fmt.Sprintf("%s.%s", path, want.names[index])
		if got.names[index] != want.names[index] {
			t.Fatalf("%s: field %d is called %q, want %q",
				path, index, got.names[index], want.names[index])
		}
		if gotField.key != wantField.key || gotField.op != wantField.op ||
			gotField.elemOp != wantField.elemOp ||
			gotField.keyKind != wantField.keyKind ||
			gotField.valueKind != wantField.valueKind {
			t.Fatalf("%s: %+v, want %+v", where, gotField, wantField)
		}
		if (gotField.sub == nil) != (wantField.sub == nil) {
			t.Fatalf("%s: one side has a child plan and the other does not", where)
		}
		if gotField.sub != nil {
			samePlan(t, where, gotField.sub, wantField.sub, seen)
		}
	}
}

// A section is bytes with a shape, and the shape is the contract. This pins it
// for one small type so that a change to the layout has to be deliberate.
func TestSchemaGoldenBytes(t *testing.T) {
	type row struct {
		ID   uint32 `cb:"2"`
		Name string `cb:"3"`
	}
	schema, err := SchemaFor[row]()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		15,                // byteLength: everything after this byte
		1,                 // structCount
		0,                 // flags: narrow keys
		2,                 // fieldCount
		1, 2, 'I', 'D', 7, // key 1, "ID", opUint32
		2, 4, 'N', 'a', 'm', 'e', 11, // key 2, "Name", opString
	}
	if !bytes.Equal(schema.Bytes(), want) {
		t.Fatalf("the section is\n%v\nwant\n%v", schema.Bytes(), want)
	}
}

// Bytes hands out a copy, because the schema behind it is cached per type and
// shared with every other caller.
func TestSchemaBytesIsACopy(t *testing.T) {
	schema, err := SchemaFor[charge]()
	if err != nil {
		t.Fatal(err)
	}
	first := schema.Bytes()
	first[0] = 0xFF
	if second := schema.Bytes(); second[0] == 0xFF {
		t.Fatal("editing the returned section edited the cached one")
	}
}

// A section arrives from a peer, so every one of these has to be an error rather
// than a panic or a plausible-looking plan.
func TestParseSchemaRefusesWhatItCannotRead(t *testing.T) {
	schema, err := SchemaFor[outer]()
	if err != nil {
		t.Fatal(err)
	}
	good := schema.Bytes()

	for cut := range len(good) {
		if _, err := ParseSchema(good[:cut]); err == nil {
			t.Fatalf("a section truncated to %d bytes was accepted", cut)
		}
	}
	for _, broken := range []struct {
		what    string
		section []byte
		wants   string
	}{
		{"nothing at all", nil, "cannot hold"},
		{"no structs", []byte{1, 0}, "describes no struct"},
		{"more structs than bytes", []byte{2, 1, 200}, "bytes of definitions"},
	} {
		_, err := ParseSchema(broken.section)
		if err == nil {
			t.Fatalf("%s was accepted", broken.what)
		}
		if !strings.Contains(err.Error(), broken.wants) {
			t.Fatalf("%s said %q, wanted it to mention %q", broken.what, err, broken.wants)
		}
	}

	// An op or a map kind this version does not assign is a section from
	// something newer, and reading it would retype a field silently.
	unassigned := append([]byte(nil), good...)
	replaced := false
	for at := range unassigned {
		if unassigned[at] == uint8(opStruct) {
			unassigned[at] = uint8(opCount) + 3
			replaced = true
			break
		}
	}
	if !replaced {
		t.Fatal("the fixture no longer holds a struct field")
	}
	if _, err := ParseSchema(unassigned); err == nil ||
		!strings.Contains(err.Error(), "does not assign") {
		t.Fatalf("an unassigned op gave %v", err)
	}
}

// Fuzzing the parser is cheap and the payoff is the one that matters: a section
// is untrusted input, and nothing in it may reach a panic.
func FuzzParseSchema(f *testing.F) {
	for _, value := range schemaCases() {
		if schema, err := SchemaOf(value); err == nil {
			f.Add(schema.Bytes())
		}
	}
	f.Fuzz(func(t *testing.T, section []byte) {
		schema, err := ParseSchema(section)
		if err != nil {
			return
		}
		// A section that parses must describe something walkable, so run a
		// message through it too rather than stopping at the parse.
		_, _ = ToJSON(schema, []byte{rootStructNarrow})
		_, _ = ToJSON(schema, []byte{rootStructWide})
	})
}

// Phase 7's property: the section is additive. The body behind it is byte for
// byte what Marshal writes, and the typed decoder takes either form.
func TestSelfDescribingIsMarshalWithAPrefix(t *testing.T) {
	for _, value := range []any{
		&charge{CompanyID: 7, UserID: 42, Access2: 9},
		&outer{Head: 7, One: inner{ID: 1, Name: "one"}, Many: []inner{{ID: 2}}},
		&bare{SensorID: 9124, Unit: "C", Valid: true},
	} {
		plain, err := Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		described, err := MarshalSelfDescribing(value)
		if err != nil {
			t.Fatal(err)
		}
		schema, err := SchemaOf(value)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := len(described), 1+schema.Size()+len(plain)-1; got != want {
			t.Fatalf("%T: a self-describing message is %d bytes, want %d", value, got, want)
		}
		if !bytes.Equal(described[1+schema.Size():], plain[1:]) {
			t.Fatalf("%T: the body differs from Marshal's", value)
		}
		if described[0]&rootSchema == 0 {
			t.Fatalf("%T: root byte %#02x does not say it carries a schema",
				value, described[0])
		}
		if described[0]&rootWide != plain[0]&rootWide {
			t.Fatalf("%T: the key width changed: %#02x against %#02x",
				value, described[0], plain[0])
		}

		// It decodes into the Go type, which is the property that makes the
		// section additive rather than a second format.
		back := reflect.New(reflect.TypeOf(value).Elem())
		if err := Unmarshal(described, back.Interface()); err != nil {
			t.Fatalf("%T: Unmarshal of a self-describing message: %v", value, err)
		}
		if !reflect.DeepEqual(back.Interface(), value) {
			t.Fatalf("%T: round-tripped as %+v, want %+v", value, back.Interface(), value)
		}
	}
}

// A root byte inside colbin's range that this version does not assign stays
// refused. Taking 0x04 for the schema must not have opened the other twelve.
func TestUnassignedRootBytesAreStillRefused(t *testing.T) {
	for root := RootFirst; root <= RootLast; root++ {
		switch root {
		case rootStructNarrow, rootStructWide, rootStructNarrowSchema, rootStructWideSchema:
			continue
		}
		var back charge
		if err := Unmarshal([]byte{root}, &back); err == nil {
			t.Fatalf("root %#02x was accepted", root)
		}
	}
}
