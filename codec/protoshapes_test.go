package codec

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The shapes protoc-gen-go emits that the format once refused: a repeated
// message as a slice of pointers, repeated floats, bools and blobs, a map keyed
// by a bool and a map of blobs. See plan/PROTOBUF_SHAPES_PLAN.md.

type shapeLine struct {
	SKU   string  `cb:"1"`
	Qty   int32   `cb:"2"`
	Price float64 `cb:"3"`
	Note  *string `cb:"4"`
}

// shapeOrder holds its lines by pointer and shapeOrderByValue by value, under
// the same ids: the two are one message.
type shapeOrder struct {
	ID    int64        `cb:"1"`
	Lines []*shapeLine `cb:"2"`
	Tail  string       `cb:"3"`
}

type shapeOrderByValue struct {
	ID    int64       `cb:"1"`
	Lines []shapeLine `cb:"2"`
	Tail  string      `cb:"3"`
}

// The same two at eight key bits.
type shapeOrderWide struct {
	ID    int64        `cb:"1"`
	Lines []*shapeLine `cb:"40"`
	Tail  string       `cb:"41"`
}

type shapeOrderWideByValue struct {
	ID    int64       `cb:"1"`
	Lines []shapeLine `cb:"40"`
	Tail  string      `cb:"41"`
}

func shapeLines(count int) []shapeLine {
	lines := make([]shapeLine, count)
	for index := range lines {
		lines[index] = shapeLine{
			SKU:   strings.Repeat("K", index%5) + "sku",
			Qty:   int32(index*7 - 3),
			Price: float64(index) + 0.25,
		}
		if index%3 == 0 {
			note := "note"
			lines[index].Note = &note
		}
	}
	return lines
}

// scattered points at copies of the lines made one at a time, so that no two
// share an array, and contiguous at the elements of the slice itself.
func scattered(lines []shapeLine) []*shapeLine {
	pointers := make([]*shapeLine, len(lines))
	for index := range lines {
		one := lines[index]
		pointers[index] = &one
	}
	return pointers
}

func contiguous(lines []shapeLine) []*shapeLine {
	pointers := make([]*shapeLine, len(lines))
	for index := range lines {
		pointers[index] = &lines[index]
	}
	return pointers
}

func deref(pointers []*shapeLine) []shapeLine {
	if len(pointers) == 0 {
		return nil
	}
	lines := make([]shapeLine, len(pointers))
	for index, pointer := range pointers {
		lines[index] = *pointer
	}
	return lines
}

// A slice of pointers is the bytes of the slice of what it points at, as a
// list and as a table, in groups past the group size, at both key widths —
// and each type reads the other's message.
func TestPointerSlicesAreTheirPointees(t *testing.T) {
	for _, count := range []int{0, 1, 2, 7, 8, 9, 300, 5000} {
		lines := shapeLines(count)
		for _, layout := range []struct {
			name     string
			pointers []*shapeLine
		}{{"scattered", scattered(lines)}, {"contiguous", contiguous(lines)}} {
			byValue, err := Marshal(&shapeOrderByValue{ID: 9, Lines: lines, Tail: "t"})
			if err != nil {
				t.Fatal(err)
			}
			byPointer, err := Marshal(&shapeOrder{ID: 9, Lines: layout.pointers, Tail: "t"})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(byValue, byPointer) {
				t.Fatalf("%d %s lines: the pointers wrote other bytes than the values", count, layout.name)
			}
			var back shapeOrder
			if err := Unmarshal(byValue, &back); err != nil {
				t.Fatalf("%d lines: %v", count, err)
			}
			if back.ID != 9 || back.Tail != "t" || !reflect.DeepEqual(deref(back.Lines), deref(layout.pointers)) {
				t.Fatalf("%d %s lines read back wrong", count, layout.name)
			}
			var values shapeOrderByValue
			if err := Unmarshal(byPointer, &values); err != nil {
				t.Fatal(err)
			}
			if len(values.Lines) != count {
				t.Fatalf("%d lines read back as %d values", count, len(values.Lines))
			}

			wideByValue, err := Marshal(&shapeOrderWideByValue{ID: 9, Lines: lines, Tail: "t"})
			if err != nil {
				t.Fatal(err)
			}
			wideByPointer, err := Marshal(&shapeOrderWide{ID: 9, Lines: layout.pointers, Tail: "t"})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(wideByValue, wideByPointer) {
				t.Fatalf("%d %s lines, wide: the pointers wrote other bytes than the values", count, layout.name)
			}
			var wideBack shapeOrderWide
			if err := Unmarshal(wideByValue, &wideBack); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(deref(wideBack.Lines), deref(layout.pointers)) || wideBack.Tail != "t" {
				t.Fatalf("%d %s lines, wide, read back wrong", count, layout.name)
			}
		}
	}
}

// The section calls the field a slice of structs, so a reader with no Go type
// renders it, and the two types have one section.
func TestPointerSlicesHaveTheSliceSection(t *testing.T) {
	byValue, err := SchemaOf(&shapeOrderByValue{})
	if err != nil {
		t.Fatal(err)
	}
	byPointer, err := SchemaOf(&shapeOrder{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(byValue.Bytes(), byPointer.Bytes()) {
		t.Fatalf("the sections differ:\n%x\n%x", byValue.Bytes(), byPointer.Bytes())
	}
	for _, count := range []int{2, 20} {
		message, err := MarshalSelfDescribing(&shapeOrder{ID: 1, Lines: scattered(shapeLines(count))})
		if err != nil {
			t.Fatal(err)
		}
		text, err := ToJSON(nil, message)
		if err != nil {
			t.Fatal(err)
		}
		var rendered struct{ Lines []map[string]any }
		if err := json.Unmarshal(text, &rendered); err != nil || len(rendered.Lines) != count {
			t.Fatalf("%d lines rendered as %s (%v)", count, text, err)
		}
	}
}

// A nil element is written as an empty struct and read back as a pointer to a
// zero one.
func TestNilElementsAreEmptyStructs(t *testing.T) {
	for _, count := range []int{3, 12} {
		pointers := scattered(shapeLines(count))
		pointers[1] = nil
		message, err := Marshal(&shapeOrder{Lines: pointers})
		if err != nil {
			t.Fatal(err)
		}
		var back shapeOrder
		if err := Unmarshal(message, &back); err != nil {
			t.Fatal(err)
		}
		if len(back.Lines) != count || back.Lines[1] == nil || *back.Lines[1] != (shapeLine{}) {
			t.Fatalf("%d lines with a nil one read back as %+v", count, back.Lines)
		}
		if !reflect.DeepEqual(back.Lines[2], pointers[2]) {
			t.Fatalf("the element after the nil one read back as %+v", back.Lines[2])
		}
	}
}

type shapeNode struct {
	Name string       `cb:"1"`
	Kids []*shapeNode `cb:"2"`
}

func shapeTree(depth, fan int) *shapeNode {
	node := &shapeNode{Name: strings.Repeat("n", depth+1)}
	if depth == 0 {
		return node
	}
	for range fan {
		node.Kids = append(node.Kids, shapeTree(depth-1, fan))
	}
	return node
}

// A tree held by pointers is written from scratch that nests: each level's
// rows are copied above the level they are being written from.
func TestPointerTreesRoundTrip(t *testing.T) {
	for _, fan := range []int{1, 3, 9} {
		tree := shapeTree(4, fan)
		message, err := Marshal(tree)
		if err != nil {
			t.Fatal(err)
		}
		var back shapeNode
		if err := Unmarshal(message, &back); err != nil {
			t.Fatal(err)
		}
		// A leaf's Kids reads back nil, as it was written.
		if !reflect.DeepEqual(&back, tree) {
			t.Fatalf("a tree of fan %d read back differently", fan)
		}
	}
}

// A handle decodes into the same value again and again, and a slice of
// pointers it filled before is not read into as rows.
func TestPointerSlicesThroughAHandle(t *testing.T) {
	codec, err := NewCodec[shapeOrder]()
	if err != nil {
		t.Fatal(err)
	}
	var back shapeOrder
	for _, count := range []int{20, 3, 0, 40, 1} {
		original := shapeOrder{ID: int64(count), Lines: scattered(shapeLines(count)), Tail: "t"}
		message, err := codec.Encode(&original)
		if err != nil {
			t.Fatal(err)
		}
		if err := codec.Unmarshal(message, &back); err != nil {
			t.Fatal(err)
		}
		if back.ID != int64(count) || len(back.Lines) != count ||
			!reflect.DeepEqual(deref(back.Lines), deref(original.Lines)) {
			t.Fatalf("%d lines through a handle read back as %d", count, len(back.Lines))
		}
	}
}

func TestPointerSliceAtTheRoot(t *testing.T) {
	original := scattered(shapeLines(11))
	message, err := Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var back []*shapeLine
	if err := Unmarshal(message, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, original) {
		t.Fatal("a slice of pointers at the root read back differently")
	}
}

func TestPointerSliceTruncationIsRefused(t *testing.T) {
	for _, count := range []int{3, 12} {
		original := shapeOrder{ID: 7, Lines: scattered(shapeLines(count)), Tail: "t"}
		checkPrefixes(t, &original, unmarshalInto[shapeOrder])
	}
}

func TestPointerToPointerSliceIsRefused(t *testing.T) {
	type held struct {
		Lines *[]*shapeLine `cb:"1"`
	}
	if _, err := Marshal(&held{}); err == nil {
		t.Fatal("a pointer to a slice of pointers was accepted")
	}
}

// --- arrays of floats, bools and blobs ---------------------------------------

type shapeArrays struct {
	F64   []float64 `cb:"1"`
	F32   []float32 `cb:"2"`
	Flags []bool    `cb:"3"`
	Blobs [][]byte  `cb:"4"`
	Tail  int32     `cb:"5"`
}

type shapeArraysWide struct {
	F64   []float64 `cb:"30"`
	F32   []float32 `cb:"31"`
	Flags []bool    `cb:"32"`
	Blobs [][]byte  `cb:"33"`
	Tail  int32     `cb:"34"`
}

func shapeArraysOf(count int) shapeArrays {
	value := shapeArrays{Tail: 5}
	for index := range count {
		value.F64 = append(value.F64, float64(index)*1.5-7)
		value.F32 = append(value.F32, float32(index)/3)
		value.Flags = append(value.Flags, index%3 == 0)
		value.Blobs = append(value.Blobs, bytes.Repeat([]byte{byte(index)}, index*40%700))
	}
	return value
}

func TestFixedArraysRoundTrip(t *testing.T) {
	// 0 and 1 are the absent field and the narrow list of one; 40 puts a float
	// array past a one-byte length, and 300 a bool array too.
	for _, count := range []int{0, 1, 2, 40, 300} {
		original := shapeArraysOf(count)
		message, err := Marshal(&original)
		if err != nil {
			t.Fatal(err)
		}
		var back shapeArrays
		if err := Unmarshal(message, &back); err != nil {
			t.Fatalf("%d elements: %v", count, err)
		}
		if !shapeArraysEqual(back, original) {
			t.Fatalf("%d elements read back as %+v", count, back)
		}
		wide := shapeArraysWide(original)
		message, err = Marshal(&wide)
		if err != nil {
			t.Fatal(err)
		}
		var wideBack shapeArraysWide
		if err := Unmarshal(message, &wideBack); err != nil {
			t.Fatalf("%d elements, wide: %v", count, err)
		}
		if !shapeArraysEqual(shapeArrays(wideBack), original) {
			t.Fatalf("%d elements, wide, read back as %+v", count, wideBack)
		}
		checkPrefixes(t, &original, unmarshalInto[shapeArrays])
		checkPrefixes(t, &wide, unmarshalInto[shapeArraysWide])
	}
}

// shapeArraysEqual compares two values with an empty blob equal to a nil one,
// which is all a blob of no bytes is on the wire.
func shapeArraysEqual(a, b shapeArrays) bool {
	if !reflect.DeepEqual(a.F64, b.F64) || !reflect.DeepEqual(a.F32, b.F32) ||
		!reflect.DeepEqual(a.Flags, b.Flags) || a.Tail != b.Tail || len(a.Blobs) != len(b.Blobs) {
		return false
	}
	for index := range a.Blobs {
		if !bytes.Equal(a.Blobs[index], b.Blobs[index]) {
			return false
		}
	}
	return true
}

// An array of floats is the floats' bits and nothing else: eight bytes each,
// whatever they hold, a NaN's payload included.
func TestFloatArraysAreTheirBits(t *testing.T) {
	payload := math.Float64frombits(0x7FF8_0000_DEAD_BEEF)
	original := shapeArrays{F64: []float64{payload, math.Inf(-1), math.Copysign(0, -1), 1}}
	message, err := Marshal(&original)
	if err != nil {
		t.Fatal(err)
	}
	// The key and its nibble, the length, and four floats.
	if len(message) != 1+1+4*8 {
		t.Fatalf("four floats took %d bytes: %x", len(message), message)
	}
	var back shapeArrays
	if err := Unmarshal(message, &back); err != nil {
		t.Fatal(err)
	}
	for index, value := range original.F64 {
		if math.Float64bits(back.F64[index]) != math.Float64bits(value) {
			t.Fatalf("element %d read back as %x", index, math.Float64bits(back.F64[index]))
		}
	}
}

// A decoded blob is the caller's: it does not alias the message, and appending
// to one does not reach the next.
func TestBlobArraysAreCopies(t *testing.T) {
	original := shapeArrays{Blobs: [][]byte{{1, 2}, {3}, {4, 5, 6}}}
	message, err := Marshal(&original)
	if err != nil {
		t.Fatal(err)
	}
	var back shapeArrays
	if err := Unmarshal(message, &back); err != nil {
		t.Fatal(err)
	}
	clear(message)
	grown := append(back.Blobs[0], 9)
	if !bytes.Equal(back.Blobs[0], []byte{1, 2}) || !bytes.Equal(back.Blobs[1], []byte{3}) ||
		!bytes.Equal(back.Blobs[2], []byte{4, 5, 6}) || !bytes.Equal(grown, []byte{1, 2, 9}) {
		t.Fatalf("the blobs read back as %v", back.Blobs)
	}
}

func TestFixedArraysRenderAsJSON(t *testing.T) {
	original := shapeArrays{
		F64: []float64{1.5, -2}, F32: []float32{0.25}, Flags: []bool{true, false},
		Blobs: [][]byte{{1, 2, 3}, {}},
	}
	for _, value := range []any{&original, func() any { wide := shapeArraysWide(original); return &wide }()} {
		message, err := MarshalSelfDescribing(value)
		if err != nil {
			t.Fatal(err)
		}
		text, err := ToJSON(nil, message)
		if err != nil {
			t.Fatal(err)
		}
		want, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		// encoding/json writes the zero Tail, which the message omits and the
		// walk renders last all the same.
		var got, expected any
		if json.Unmarshal(text, &got) != nil || json.Unmarshal(want, &expected) != nil ||
			!reflect.DeepEqual(got, expected) {
			t.Fatalf("rendered %s, and encoding/json says %s", text, want)
		}
	}
}

// A bool is 0 or 1, and an array of anything else is not an array of bools; a
// float array is a whole number of floats at the float's width.
func TestHostileFixedArraysAreRefused(t *testing.T) {
	type flags struct {
		Flags []bool `cb:"1"`
	}
	type wideFlags struct {
		Flags []bool `cb:"30"`
	}
	type floats struct {
		F64 []float64 `cb:"1"`
	}
	type wideFloats struct {
		F64 []float64 `cb:"30"`
	}
	type ints struct {
		F64 []int32 `cb:"1"`
	}
	type wideInts struct {
		F64 []int32 `cb:"30"`
	}
	for _, value := range []any{&flags{Flags: []bool{true, true, false}}, &wideFlags{Flags: []bool{true, true, false}}} {
		message, err := Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		at := bytes.LastIndexByte(message, 1)
		message[at] = 2
		into := reflect.New(reflect.TypeOf(value).Elem()).Interface()
		if err := Unmarshal(message, into); err == nil {
			t.Fatalf("%T: a bool of 2 was accepted", value)
		}
	}
	// An integer array under the same key is narrower than a float array.
	narrow, err := Marshal(&ints{F64: []int32{1, 2, 3}})
	if err != nil {
		t.Fatal(err)
	}
	if err := Unmarshal(narrow, &floats{}); err == nil {
		t.Fatal("three one-byte integers were read as floats")
	}
	wide, err := Marshal(&wideInts{F64: []int32{1, 2, 3}})
	if err != nil {
		t.Fatal(err)
	}
	if err := Unmarshal(wide, &wideFloats{}); err == nil {
		t.Fatal("three one-byte integers were read as floats, wide")
	}
}

// --- maps keyed by a bool, and maps of blobs ---------------------------------

type shapeMaps struct {
	ByFlag  map[bool]string     `cb:"1"`
	Blobs   map[string][]byte   `cb:"2"`
	Lines   map[bool]*shapeLine `cb:"3"`
	ByCount map[int32][]byte    `cb:"4"`
}

type shapeMapsWide struct {
	ByFlag  map[bool]string     `cb:"30"`
	Blobs   map[string][]byte   `cb:"31"`
	Lines   map[bool]*shapeLine `cb:"32"`
	ByCount map[int32][]byte    `cb:"33"`
}

func TestBoolKeysAndBlobValuesRoundTrip(t *testing.T) {
	original := shapeMaps{
		ByFlag:  map[bool]string{true: "yes", false: "no"},
		Blobs:   map[string][]byte{"a": {1, 2, 3}, "empty": {}, "big": bytes.Repeat([]byte{7}, 400)},
		Lines:   map[bool]*shapeLine{true: {SKU: "s", Qty: 2}},
		ByCount: map[int32][]byte{-4: {9}},
	}
	message, err := Marshal(&original)
	if err != nil {
		t.Fatal(err)
	}
	var back shapeMaps
	if err := Unmarshal(message, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, original) {
		t.Fatalf("read back as %+v", back)
	}
	wide := shapeMapsWide(original)
	message, err = Marshal(&wide)
	if err != nil {
		t.Fatal(err)
	}
	var wideBack shapeMapsWide
	if err := Unmarshal(message, &wideBack); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(shapeMaps(wideBack), original) {
		t.Fatalf("wide, read back as %+v", wideBack)
	}

	for _, value := range []any{&original, &wide} {
		message, err := MarshalSelfDescribing(value)
		if err != nil {
			t.Fatal(err)
		}
		text, err := ToJSON(nil, message)
		if err != nil {
			t.Fatal(err)
		}
		var rendered struct {
			ByFlag map[string]string
			Blobs  map[string][]byte
		}
		if err := json.Unmarshal(text, &rendered); err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		if rendered.ByFlag["true"] != "yes" || rendered.ByFlag["false"] != "no" ||
			!bytes.Equal(rendered.Blobs["a"], []byte{1, 2, 3}) || len(rendered.Blobs["big"]) != 400 {
			t.Fatalf("rendered as %s", text)
		}
	}
}

// A map of dynamic values is a dynamic container, whose keys are names.
func TestBoolKeyedDynamicMapIsRefused(t *testing.T) {
	type dynamic struct {
		M map[bool]any `cb:"1"`
	}
	if _, err := Marshal(&dynamic{}); err == nil {
		t.Fatal("a map[bool]any was accepted")
	}
}

// What protoc-gen-go writes for a message, minus its oneofs: unexported state,
// pointers for presence, and every repeated message a slice of pointers.
type shapeGenerated struct {
	state         [0]func()
	sizeCache     int32
	unknownFields []byte

	Name     string                 `protobuf:"bytes,1,opt,name=name,proto3"`
	Scores   []float64              `protobuf:"fixed64,2,rep,packed,name=scores,proto3"`
	Flags    []bool                 `protobuf:"varint,3,rep,packed,name=flags,proto3"`
	Chunks   [][]byte               `protobuf:"bytes,4,rep,name=chunks,proto3"`
	Children []*shapeGenerated      `protobuf:"bytes,5,rep,name=children,proto3"`
	ByFlag   map[bool]int64         `protobuf:"bytes,6,rep,name=by_flag"`
	Payloads map[string][]byte      `protobuf:"bytes,7,rep,name=payloads"`
	Parent   *shapeGenerated        `protobuf:"bytes,8,opt,name=parent,proto3"`
	Optional *int32                 `protobuf:"varint,9,opt,name=optional,proto3,oneof"`
	Lines    map[string]*shapeLine  `protobuf:"bytes,10,rep,name=lines"`
	Ratios   []float32              `protobuf:"fixed32,11,rep,packed,name=ratios,proto3"`
	Weights  map[uint64]float64     `protobuf:"bytes,12,rep,name=weights"`
	Enum     int32                  `protobuf:"varint,13,opt,name=enum,proto3,enum=x.E"`
	Nested   map[int32]*shapeNode   `protobuf:"bytes,14,rep,name=nested"`
	Ints     []int32                `protobuf:"varint,15,rep,packed,name=ints,proto3"`
	Texts    []string               `protobuf:"bytes,16,rep,name=texts,proto3"`
	Extra    map[string]*shapeOrder `protobuf:"bytes,17,rep,name=extra"`
}

func TestGeneratedMessageShapeRoundTrips(t *testing.T) {
	seven := int32(7)
	original := &shapeGenerated{
		Name:   "root",
		Scores: []float64{1.5, 2.5},
		Flags:  []bool{true, false},
		Chunks: [][]byte{{1}, {2, 3}},
		Children: []*shapeGenerated{
			{Name: "a", Ints: []int32{1, -2}},
			{Name: "b", Children: []*shapeGenerated{{Name: "c", Texts: []string{"x"}}}},
		},
		ByFlag:   map[bool]int64{true: 1, false: -1},
		Payloads: map[string][]byte{"k": {4, 5}},
		Parent:   &shapeGenerated{Name: "parent"},
		Optional: &seven,
		Lines:    map[string]*shapeLine{"l": {SKU: "s"}},
		Ratios:   []float32{0.5},
		Weights:  map[uint64]float64{3: 0.25},
		Enum:     2,
		Nested:   map[int32]*shapeNode{1: {Name: "n", Kids: []*shapeNode{{Name: "k"}}}},
		Extra:    map[string]*shapeOrder{"o": {ID: 1, Lines: scattered(shapeLines(10))}},
	}
	for _, encode := range []func(any) ([]byte, error){Marshal, MarshalSelfDescribing} {
		message, err := encode(original)
		if err != nil {
			t.Fatal(err)
		}
		back := &shapeGenerated{}
		if err := Unmarshal(message, back); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(back, original) {
			t.Fatalf("read back as %+v", back)
		}
	}
}

// BenchmarkPointerSlice sets a slice of pointers beside the slice of values it
// is on the wire: what the copy into scratch costs an encode, and the pointer
// array a decode.
func BenchmarkPointerSlice(b *testing.B) {
	for _, count := range []int{5, 1000} {
		lines := shapeLines(count)
		values := shapeOrderByValue{ID: 1, Lines: lines}
		pointers := shapeOrder{ID: 1, Lines: scattered(lines)}
		message, err := Marshal(&values)
		if err != nil {
			b.Fatal(err)
		}
		name := "rows=" + strconv.Itoa(count)
		b.Run(name+"/encode/values", func(b *testing.B) {
			var buffer []byte
			b.ReportAllocs()
			for b.Loop() {
				buffer, _ = Append(buffer[:0], &values)
			}
		})
		b.Run(name+"/encode/pointers", func(b *testing.B) {
			var buffer []byte
			b.ReportAllocs()
			for b.Loop() {
				buffer, _ = Append(buffer[:0], &pointers)
			}
		})
		b.Run(name+"/decode/values", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				var back shapeOrderByValue
				if err := Unmarshal(message, &back); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(name+"/decode/pointers", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				var back shapeOrder
				if err := Unmarshal(message, &back); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// With packed5 on, the row programs are off and a list goes through the plan
// walks: the pointers have to write the values' bytes there too.
func TestPointerSlicesUnderPacked5(t *testing.T) {
	defer SetPacked5(Packed5())
	SetPacked5(true)
	for _, count := range []int{1, 2, 7, 9} {
		lines := shapeLines(count)
		for index := range lines {
			lines[index].SKU = "PACKED SKU"
		}
		for _, pair := range [][2]any{
			{&shapeOrderByValue{Lines: lines}, &shapeOrder{Lines: scattered(lines)}},
			{&shapeOrderWideByValue{Lines: lines}, &shapeOrderWide{Lines: scattered(lines)}},
		} {
			byValue, err := Marshal(pair[0])
			if err != nil {
				t.Fatal(err)
			}
			byPointer, err := Marshal(pair[1])
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(byValue, byPointer) {
				t.Fatalf("%T, %d lines: the pointers wrote other bytes than the values", pair[1], count)
			}
		}
	}
}

// --- slices of pointers as list columns --------------------------------------

// The list-column types of listcolumn_test.go held by pointer at both levels:
// an order's lines and a line's parts.
type ptrLine struct {
	Number   int32       `cb:"1"`
	Quantity float64     `cb:"2"`
	Mode     string      `cb:"3"`
	Note     *string     `cb:"4"`
	Parts    []*listPart `cb:"5"`
}

type ptrOrder struct {
	ID     int64      `cb:"1"`
	Status string     `cb:"2"`
	Lines  []*ptrLine `cb:"3"`
	Total  float64    `cb:"4"`
}

type ptrWideOrder struct {
	ID    int64       `cb:"1"`
	Lines []*ptrLine  `cb:"20"`
	Tags  []*listPart `cb:"21"`
}

type ptrHolder struct {
	Head   int32           `cb:"1"`
	Orders []*ptrOrder     `cb:"2"`
	Wide   []*ptrWideOrder `cb:"3"`
}

type ptrWideHolder struct {
	Head   int32       `cb:"1"`
	Orders []*ptrOrder `cb:"40"`
}

// pointedHolder is holder with every slice of structs turned into a slice of
// pointers to copies, each allocated on its own.
func pointedHolder(holder listHolder) ptrHolder {
	parts := func(from []listPart) []*listPart {
		if from == nil {
			return nil
		}
		out := make([]*listPart, len(from))
		for index := range from {
			part := from[index]
			out[index] = &part
		}
		return out
	}
	lines := func(from []listLine) []*ptrLine {
		if from == nil {
			return nil
		}
		out := make([]*ptrLine, len(from))
		for index, line := range from {
			out[index] = &ptrLine{line.Number, line.Quantity, line.Mode, line.Note, parts(line.Parts)}
		}
		return out
	}
	out := ptrHolder{Head: holder.Head}
	for _, order := range holder.Orders {
		out.Orders = append(out.Orders, &ptrOrder{order.ID, order.Status, lines(order.Lines), order.Total})
	}
	for _, order := range holder.Wide {
		out.Wide = append(out.Wide, &ptrWideOrder{order.ID, lines(order.Lines), parts(order.Tags)})
	}
	return out
}

func TestPointerSlicesAreListColumns(t *testing.T) {
	// 12 rows are one table and 5000 are two groups of them.
	for _, count := range []int{3, 12, 5000} {
		values := makeListHolder(count)
		pointers := pointedHolder(values)
		byValue, err := Marshal(&values)
		if err != nil {
			t.Fatal(err)
		}
		byPointer, err := Marshal(&pointers)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(byValue, byPointer) {
			t.Fatalf("%d orders: the pointers wrote other bytes than the values", count)
		}
		var back ptrHolder
		if err := Unmarshal(byValue, &back); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(back, pointers) {
			t.Fatalf("%d orders read back differently", count)
		}
		// Each row's lines are a window capped at its own end: appending to one
		// row's must not write over the next row's first.
		for index, order := range back.Orders[:len(back.Orders)-1] {
			if next := back.Orders[index+1]; len(order.Lines) != 0 && len(next.Lines) != 0 {
				kept := next.Lines[0]
				_ = append(order.Lines, &ptrLine{})
				if next.Lines[0] != kept {
					t.Fatal("appending to one row's lines wrote over the next row's")
				}
			}
		}

		wideValues := listWideHolder{Head: values.Head, Orders: values.Orders}
		widePointers := ptrWideHolder{Head: pointers.Head, Orders: pointers.Orders}
		byValue, err = Marshal(&wideValues)
		if err != nil {
			t.Fatal(err)
		}
		byPointer, err = Marshal(&widePointers)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(byValue, byPointer) {
			t.Fatalf("%d orders, wide: the pointers wrote other bytes than the values", count)
		}
		var wideBack ptrWideHolder
		if err := Unmarshal(byValue, &wideBack); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(wideBack, widePointers) {
			t.Fatalf("%d orders, wide, read back differently", count)
		}
	}
}

// Under dictionaries the bytes are the values' as well. A column's dictionary
// is keyed by its row type's plan, so this holds only because a type has one
// plan whichever root reached it first (TestNestedTypeHasOnePlan).
func TestPointerListColumnsUnderDictionaries(t *testing.T) {
	values := makeListHolder(40)
	pointers := pointedHolder(values)
	withDictionary(t, func() {
		fromValues, fromPointers := must(Marshal(&values)), must(Marshal(&pointers))
		if !bytes.Equal(fromValues, fromPointers) {
			t.Fatalf("values %d bytes, pointers %d", len(fromValues), len(fromPointers))
		}
		for _, message := range [][]byte{fromValues, fromPointers} {
			var back ptrHolder
			if err := Unmarshal(message, &back); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(back, pointers) {
				t.Fatal("read back differently")
			}
		}
	})
}

// --- one plan a type ----------------------------------------------------------

type planLeaf struct {
	ID   int64  `cb:"1"`
	Name string `cb:"2"`
}

type planMiddle struct {
	Leaves []planLeaf `cb:"1"`
	One    planLeaf   `cb:"2"`
}

type planRootA struct {
	Middles []planMiddle        `cb:"1"`
	ByName  map[string]planLeaf `cb:"2"`
}

type planRootB struct {
	Leaves []*planLeaf `cb:"1"`
	Middle *planMiddle `cb:"2"`
}

// A nested type's plan is the one it has as a root and the one every other
// root holds, whichever was planned first.
func TestNestedTypeHasOnePlan(t *testing.T) {
	rootA := must(planFor(reflect.TypeFor[planRootA]()))
	rootB := must(planFor(reflect.TypeFor[planRootB]()))
	middle := must(planFor(reflect.TypeFor[planMiddle]()))
	leaf := must(planFor(reflect.TypeFor[planLeaf]()))

	if got := rootA.fields[0].sub; got != middle {
		t.Error("planRootA.Middles holds another plan of planMiddle")
	}
	if got := rootB.fields[1].sub; got != middle {
		t.Error("planRootB.Middle holds another plan of planMiddle")
	}
	for _, held := range []*typePlan{
		middle.fields[0].sub, middle.fields[1].sub, rootA.fields[1].sub, rootB.fields[0].sub,
	} {
		if held != leaf {
			t.Error("a holder has another plan of planLeaf")
		}
	}
}

type planCycleA struct {
	ID   int64        `cb:"1"`
	Kids []planCycleB `cb:"2"`
}

type planCycleB struct {
	ID     int64        `cb:"1"`
	Back   []planCycleA `cb:"2"`
	Leaves []planLeaf   `cb:"3"`
}

// A type in a cycle keeps the plan it gets as a root: the one finished inside
// another type's build derived what it did from a half-built plan, and is not
// cached. What the cycle holds outside itself is still shared.
func TestCyclicTypesKeepTheirRootPlan(t *testing.T) {
	cycleA := must(planFor(reflect.TypeFor[planCycleA]()))
	nestedB := cycleA.fields[1].sub
	if nestedB.fields[1].sub != cycleA {
		t.Fatal("the cycle does not close on its first type")
	}
	if nestedB.fields[2].sub != must(planFor(reflect.TypeFor[planLeaf]())) {
		t.Error("the cycle holds another plan of planLeaf")
	}
	cycleB := must(planFor(reflect.TypeFor[planCycleB]()))
	if cycleB == nestedB {
		t.Error("planCycleB as a root is the plan finished under planCycleA")
	}
	if cycleB.fields[1].sub != cycleA {
		t.Error("planCycleB as a root holds another plan of planCycleA")
	}

	// Both forms read each other's bytes.
	value := planCycleA{ID: 1, Kids: []planCycleB{{ID: 2, Back: []planCycleA{{ID: 3}}, Leaves: []planLeaf{{4, "x"}}}}}
	var back planCycleA
	if err := Unmarshal(must(Marshal(&value)), &back); err != nil || !reflect.DeepEqual(back, value) {
		t.Fatalf("round trip: %v", err)
	}
	inner := value.Kids[0]
	var innerBack planCycleB
	if err := Unmarshal(must(Marshal(&inner)), &innerBack); err != nil || !reflect.DeepEqual(innerBack, inner) {
		t.Fatalf("round trip from the inner type: %v", err)
	}
}

// Concurrent first uses build a type once.
func TestConcurrentPlansAreOne(t *testing.T) {
	type onlyHere struct {
		Rows []planMiddle `cb:"1"`
	}
	plans := make([]*typePlan, 16)
	var group sync.WaitGroup
	for index := range plans {
		group.Go(func() { plans[index] = must(planFor(reflect.TypeFor[onlyHere]())) })
	}
	group.Wait()
	for _, plan := range plans {
		if plan != plans[0] {
			t.Fatal("two plans of one type")
		}
	}
}

func TestPointerListColumnTruncationIsRefused(t *testing.T) {
	pointers := pointedHolder(makeListHolder(10))
	checkPrefixes(t, &pointers, unmarshalInto[ptrHolder])
}

// --- through a foreign section -----------------------------------------------

// A section's fields are matched by name, so these two types number their
// fields differently on purpose, which sends the decode through the section.
type foreignArraysSource struct {
	Texts  []string  `cb:"1"`
	Blob   []byte    `cb:"2"`
	Floats []float64 `cb:"3"`
	Flags  []bool    `cb:"4"`
	Blobs  [][]byte  `cb:"5"`
	Ints   []int64   `cb:"6"`
	Small  []float32 `cb:"7"`
	Counts []uint32  `cb:"8"`
}

type foreignArraysTarget struct {
	Counts []float32 `cb:"1"`
	Small  []float64 `cb:"2"`
	Ints   []float64 `cb:"9"`
	Blobs  [][]byte  `cb:"8"`
	Flags  []bool    `cb:"7"`
	Floats []float32 `cb:"6"`
	Blob   []byte    `cb:"5"`
	Texts  []string  `cb:"4"`
}

func TestForeignArraysConvert(t *testing.T) {
	message, err := MarshalSelfDescribing(&foreignArraysSource{
		Texts: []string{"x", ""}, Blob: []byte{1}, Floats: []float64{1.5, -2},
		Flags: []bool{true, false}, Blobs: [][]byte{{2}, {3, 4}}, Ints: []int64{3, -1 << 40},
		Small: []float32{0.25}, Counts: []uint32{7, 1 << 31},
	})
	if err != nil {
		t.Fatal(err)
	}
	var back foreignArraysTarget
	if err := Unmarshal(message, &back); err != nil {
		t.Fatal(err)
	}
	want := foreignArraysTarget{
		Counts: []float32{7, 1 << 31}, Small: []float64{0.25}, Ints: []float64{3, -1 << 40},
		Blobs: [][]byte{{2}, {3, 4}}, Flags: []bool{true, false}, Floats: []float32{1.5, -2},
		Blob: []byte{1}, Texts: []string{"x", ""},
	}
	if !reflect.DeepEqual(back, want) {
		t.Fatalf("read back as %+v", back)
	}
}

func TestForeignFloatArraysRefuse(t *testing.T) {
	type floats struct {
		V []float64 `cb:"1"`
	}
	type ints struct {
		V []int64 `cb:"2"`
	}
	type narrow struct {
		V []float32 `cb:"2"`
	}
	message, err := MarshalSelfDescribing(&floats{V: []float64{1}})
	if err != nil {
		t.Fatal(err)
	}
	if err := Unmarshal(message, &ints{}); err == nil || !strings.Contains(err.Error(), "never stored in an integer") {
		t.Fatalf("a float array went into an integer array: %v", err)
	}
	message, err = MarshalSelfDescribing(&floats{V: []float64{1, 1e300}})
	if err != nil {
		t.Fatal(err)
	}
	if err := Unmarshal(message, &narrow{}); err == nil || !strings.Contains(err.Error(), "element 1") {
		t.Fatalf("1e300 went into a float32: %v", err)
	}
}

// A browser's slice of records goes into a Go slice of pointers, through the
// section, as it goes into a slice of values.
func TestForeignSliceIntoPointers(t *testing.T) {
	type line struct {
		Qty int64  `cb:"1"`
		SKU string `cb:"2"`
	}
	type source struct {
		Lines []line `cb:"1"`
	}
	type goLine struct {
		SKU string `cb:"5"`
		Qty int16  `cb:"6"`
	}
	type target struct {
		Lines []*goLine `cb:"3"`
	}
	for _, count := range []int{2, 20} {
		lines := make([]line, count)
		for index := range lines {
			lines[index] = line{Qty: int64(index), SKU: strconv.Itoa(index)}
		}
		message, err := MarshalSelfDescribing(&source{Lines: lines})
		if err != nil {
			t.Fatal(err)
		}
		var back target
		if err := Unmarshal(message, &back); err != nil {
			t.Fatal(err)
		}
		if len(back.Lines) != count || back.Lines[count-1].Qty != int16(count-1) ||
			back.Lines[count-1].SKU != strconv.Itoa(count-1) {
			t.Fatalf("%d lines read back as %+v", count, back.Lines)
		}
	}
}

// BenchmarkPointerListColumn is the nested shape — a table of orders, each with
// its lines and their parts — held by value and by pointer at both levels.
func BenchmarkPointerListColumn(b *testing.B) {
	values := makeListHolder(1000)
	pointers := pointedHolder(values)
	message := must(Marshal(&values))
	b.Run("encode/values", func(b *testing.B) {
		var buffer []byte
		b.ReportAllocs()
		for b.Loop() {
			buffer, _ = Append(buffer[:0], &values)
		}
	})
	b.Run("encode/pointers", func(b *testing.B) {
		var buffer []byte
		b.ReportAllocs()
		for b.Loop() {
			buffer, _ = Append(buffer[:0], &pointers)
		}
	})
	b.Run("decode/values", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var back listHolder
			if err := Unmarshal(message, &back); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("decode/pointers", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var back ptrHolder
			if err := Unmarshal(message, &back); err != nil {
				b.Fatal(err)
			}
		}
	})
}
