package codec

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// pagedType builds a struct with one field per id, of a few kinds in turn, so
// that every page holds strings and numbers of several widths.
func pagedType(ids []int) reflect.Type {
	kinds := []reflect.Type{
		reflect.TypeFor[int32](), reflect.TypeFor[string](), reflect.TypeFor[uint64](),
		reflect.TypeFor[bool](), reflect.TypeFor[float64](),
	}
	fields := make([]reflect.StructField, len(ids))
	for index, id := range ids {
		fields[index] = reflect.StructField{
			Name: fmt.Sprintf("F%d", id),
			Type: kinds[id%len(kinds)],
			Tag:  reflect.StructTag(fmt.Sprintf(`cb:"%d"`, id)),
		}
	}
	return reflect.StructOf(fields)
}

// pagedValue is a pointer to a pagedType value with the fields for set holding
// a value each id picks, and every other field zero.
func pagedValue(structType reflect.Type, set []int) reflect.Value {
	value := reflect.New(structType)
	for _, id := range set {
		field := value.Elem().FieldByName(fmt.Sprintf("F%d", id))
		switch field.Kind() {
		case reflect.Int32:
			field.SetInt(int64(-id))
		case reflect.String:
			field.SetString(fmt.Sprintf("value %d", id))
		case reflect.Uint64:
			field.SetUint(uint64(id) << 40)
		case reflect.Bool:
			field.SetBool(true)
		case reflect.Float64:
			field.SetFloat(float64(id) + 0.5)
		}
	}
	return value
}

func span(from, to int) []int {
	ids := make([]int, 0, to-from+1)
	for id := from; id <= to; id++ {
		ids = append(ids, id)
	}
	return ids
}

// roundTrips encodes value, decodes it into a fresh one of its type and
// returns the message.
func roundTrips(t *testing.T, value reflect.Value) []byte {
	t.Helper()
	message, err := Marshal(value.Interface())
	if err != nil {
		t.Fatal(err)
	}
	back := reflect.New(value.Elem().Type())
	if err := Unmarshal(message, back.Interface()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.Elem().Interface(), value.Elem().Interface()) {
		t.Fatalf("round trip changed the value\n got %+v\nwant %+v", back.Elem(), value.Elem())
	}
	return message
}

// Six hundred fields is three pages: ids 1..255, 256..510 and 511..600.
func TestAPagedTypeRoundTrips(t *testing.T) {
	structType := pagedType(span(1, 600))
	for _, set := range [][]int{span(1, 600), {1}, {255, 256}, {300}, {510, 511}, {600}, nil} {
		roundTrips(t, pagedValue(structType, set))
	}
}

// A page with nothing set is not written, and neither is a link to one, so a
// paged type whose later pages are zero is the bytes its first page alone
// would be.
func TestEmptyTrailingPagesAreNotWritten(t *testing.T) {
	paged, single := pagedType(span(1, 600)), pagedType(span(1, 255))
	set := []int{1, 17, 200, 255}
	got := roundTrips(t, pagedValue(paged, set))
	want := roundTrips(t, pagedValue(single, set))
	if string(got) != string(want) {
		t.Fatalf("a paged type with its pages empty is\n%x\nand one page alone is\n%x", got, want)
	}
}

// The last page's keys may all fit four bits, and it then uses them: a page's
// width is its own, like any nested run's.
func TestALastPageCanBeNarrow(t *testing.T) {
	structType := pagedType(append(span(1, 20), span(256, 260)...))
	plan, err := planFor(structType)
	if err != nil {
		t.Fatal(err)
	}
	last := plan.fields[len(plan.fields)-1].sub
	if !plan.isWide || last == nil || !last.page || last.isWide {
		t.Fatalf("pages are not the shape expected: first wide %v, last %+v", plan.isWide, last)
	}
	roundTrips(t, pagedValue(structType, []int{2, 257, 260}))
}

// The JSON walk writes a page's fields into the object of the type, so the
// document is what encoding/json writes for the same value — pages and all, and
// a page the message left out as the zeros it means.
func TestJSONMergesThePages(t *testing.T) {
	big, narrowLast := pagedType(span(1, 600)), pagedType(append(span(1, 20), span(256, 260)...))
	for _, testCase := range []struct {
		structType reflect.Type
		set        []int
	}{
		{big, span(1, 600)},
		{big, []int{3}},
		{big, []int{599}},
		{narrowLast, []int{257}},
		{narrowLast, []int{2}},
	} {
		value := pagedValue(testCase.structType, testCase.set)
		message := must(Marshal(value.Interface()))
		schema := must(SchemaOf(value.Interface()))
		want := must(json.Marshal(value.Interface()))

		sameJSON(t, must(ToJSON(schema, message)), want)
		sameJSON(t, must(ToJSON(must(ParseSchema(schema.Bytes())), message)), want)
		sameJSON(t, must(ToJSON(nil, must(MarshalSelfDescribing(value.Interface())))), want)

		decoded, ok := must(DecodeAny(schema, message)).(map[string]any)
		if !ok || len(decoded) != testCase.structType.NumField() {
			t.Fatalf("DecodeAny gave %d fields for %d", len(decoded), testCase.structType.NumField())
		}
	}
}

func TestFieldIDsReportsIDsOnEveryPage(t *testing.T) {
	ids := must(FieldIDs(reflect.New(pagedType(span(1, 600))).Interface()))
	if len(ids) != 600 {
		t.Fatalf("%d ids for 600 fields", len(ids))
	}
	for _, id := range span(1, 600) {
		if got := ids[fmt.Sprintf("F%d", id)]; int(got) != id {
			t.Fatalf("F%d reported as id %d", id, got)
		}
	}
}

// A page the reader does not know is a struct under a key it does not know,
// which a wide run steps over; a page the writer did not have is one it never
// wrote. Either way the fields both sides know come through.
func TestPagesEvolve(t *testing.T) {
	older, newer := pagedType(span(1, 300)), pagedType(span(1, 600))
	shared := []int{1, 255, 256, 300}

	fromNewer := must(Marshal(pagedValue(newer, append(shared, 400, 600)).Interface()))
	back := reflect.New(older)
	if err := Unmarshal(fromNewer, back.Interface()); err != nil {
		t.Fatal(err)
	}
	if want := pagedValue(older, shared); !reflect.DeepEqual(back.Elem().Interface(), want.Elem().Interface()) {
		t.Fatal("an older reader lost a field it knows")
	}

	fromOlder := must(Marshal(pagedValue(older, shared).Interface()))
	back = reflect.New(newer)
	if err := Unmarshal(fromOlder, back.Interface()); err != nil {
		t.Fatal(err)
	}
	if want := pagedValue(newer, shared); !reflect.DeepEqual(back.Elem().Interface(), want.Elem().Interface()) {
		t.Fatal("a newer reader got a field the message did not hold")
	}
}

// A paged type inside another: as a field, as the element of a slice — which
// stays a list, since a link is not a column — and behind a pointer.
func TestAPagedTypeNests(t *testing.T) {
	inner := pagedType(span(1, 300))
	outer := reflect.StructOf([]reflect.StructField{
		{Name: "ID", Type: reflect.TypeFor[int32](), Tag: `cb:"1"`},
		{Name: "One", Type: inner, Tag: `cb:"2"`},
		{Name: "Many", Type: reflect.SliceOf(inner), Tag: `cb:"3"`},
		{Name: "Maybe", Type: reflect.PointerTo(inner), Tag: `cb:"4"`},
	})
	value := reflect.New(outer)
	value.Elem().Field(0).SetInt(7)
	value.Elem().Field(1).Set(pagedValue(inner, []int{1, 299}).Elem())
	many := reflect.MakeSlice(reflect.SliceOf(inner), 2*tableThreshold, 2*tableThreshold)
	for index := range many.Len() {
		many.Index(index).Set(pagedValue(inner, []int{index + 1, 256 + index}).Elem())
	}
	value.Elem().Field(2).Set(many)
	value.Elem().Field(3).Set(pagedValue(inner, []int{300}))

	message := roundTrips(t, value)
	sameJSON(t, must(ToJSON(must(SchemaOf(value.Interface())), message)), must(json.Marshal(value.Interface())))
}

// sparsePages has ids on its first and third pages and none on its second,
// which holds nothing but the link to the third.
type sparsePages struct {
	ID   int32  `cb:"1"`
	Name string `cb:"2"`
	Late int64  `cb:"600"`
}

func TestASparsePagedType(t *testing.T) {
	codec := MustCodec[sparsePages]()
	for _, value := range []sparsePages{{ID: 1, Name: "n", Late: -5}, {Late: 9}, {ID: 2}, {}} {
		message := must(codec.Encode(&value))
		var back sparsePages
		if err := codec.Unmarshal(message, &back); err != nil || back != value {
			t.Fatalf("%+v came back as %+v (%v)", value, back, err)
		}
		checkPrefixes(t, &value, unmarshalInto)
	}
	if ids := codec.FieldIDs(); ids["Late"] != 600 || ids["Name"] != 2 {
		t.Fatalf("ids %v", ids)
	}
}

func TestPagedTypesAreRefusedWhereTheyCannotWork(t *testing.T) {
	int32Type := reflect.TypeFor[int32]()
	unnumbered := make([]reflect.StructField, 257)
	for index := range unnumbered {
		unnumbered[index] = reflect.StructField{Name: fmt.Sprintf("F%d", index), Type: int32Type}
	}
	for name, testCase := range map[string]struct {
		fields []reflect.StructField
		wants  string
	}{
		"an unnumbered field beside a paged one": {[]reflect.StructField{
			{Name: "A", Type: int32Type, Tag: `cb:"300"`},
			{Name: "B", Type: int32Type},
		}, "has no id"},
		"more fields than a run holds, unnumbered": {unnumbered, "pages"},
		"one id twice, past the first page": {[]reflect.StructField{
			{Name: "A", Type: int32Type, Tag: `cb:"300"`},
			{Name: "B", Type: int32Type, Tag: `cb:"300"`},
		}, "is on both"},
	} {
		_, err := Marshal(reflect.New(reflect.StructOf(testCase.fields)).Interface())
		if err == nil || !strings.Contains(err.Error(), testCase.wants) {
			t.Errorf("%s: got %v, want it to mention %q", name, err, testCase.wants)
		}
	}
	if _, err := GenerateString("pages", sparsePages{}); err == nil || !strings.Contains(err.Error(), "pages") {
		t.Errorf("Generate on a paged type: %v", err)
	}
}

// A walk merges a page into the object that links it, so a section that puts
// one anywhere else describes a document nothing can write.
func TestParseSchemaRefusesAMisplacedPage(t *testing.T) {
	page := &typePlan{page: true, fields: []planField{{key: 0, op: opInt32}}, names: []string{"x"}}
	linkedBy := func(field planField, wide bool) *typePlan {
		return &typePlan{isWide: wide, fields: []planField{field}, names: []string{"link"}}
	}
	for name, root := range map[string]*typePlan{
		"at the root":      page,
		"under key 3":      linkedBy(planField{key: 3, op: opStruct, sub: page}, true),
		"in a list":        linkedBy(planField{key: pageLink, op: opStructs, sub: page}, true),
		"behind a pointer": linkedBy(planField{key: pageLink, op: opPointerStruct, sub: page}, true),
		"in a narrow run":  linkedBy(planField{key: 3, op: opStruct, sub: page}, false),
	} {
		if _, err := ParseSchema(newSectionBuilder(root).bytes()); err == nil {
			t.Errorf("a page %s was accepted", name)
		}
	}
	linked := linkedBy(planField{key: pageLink, op: opStruct, sub: page}, true)
	if _, err := ParseSchema(newSectionBuilder(linked).bytes()); err != nil {
		t.Fatalf("a page where an encoder puts one was refused: %v", err)
	}
}
