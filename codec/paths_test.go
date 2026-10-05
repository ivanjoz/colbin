package codec

import (
	"bytes"
	"fmt"
	"reflect"
	"testing"
)

// Every value op has one row in valueOps and is written by several walks: the
// flat one, the nested one, a pointer's, each at both key widths. They are
// generated from that row, which is what keeps them agreeing; this is what
// checks they do. For each op, a value goes through every path, and every path
// must write the same field bytes and read back the same value — with packed5
// off and on, because that is where two of the copies once disagreed.

// opSamples is a non-zero value of each row's type, so every op writes a field.
var opSamples = map[fieldOp]any{
	opBool: true, opInt8: int8(-5), opInt16: int16(-300), opInt32: int32(-70000),
	opInt64: int64(-1 << 40), opUint8: uint8(200), opUint16: uint16(60000),
	opUint32: uint32(1 << 31), opUint64: uint64(1 << 63), opFloat32: float32(1.5),
	opFloat64: -2.25, opString: "PACKED TEXT", opBytes: []byte{0, 1, 2},
	opInt8s: []int8{-1, 2}, opInt16s: []int16{-300, 2}, opInt32s: []int32{-70000, 1},
	opInt64s: []int64{-1 << 40, 1}, opUint16s: []uint16{1, 60000},
	opUint32s: []uint32{1, 1 << 31}, opUint64s: []uint64{1, 1 << 63},
	opStrings: []string{"PACKED", "", "text"},
}

func TestEveryValueOpAgreesOnEveryPath(t *testing.T) {
	defer SetPacked5(Packed5())
	for _, packed := range []bool{false, true} {
		SetPacked5(packed)
		for _, row := range valueOps {
			sample, ok := opSamples[row.op]
			if !ok {
				t.Fatalf("op %s has no sample", opName(row.op))
			}
			t.Run(fmt.Sprintf("%s/packed=%v", opName(row.op), packed), func(t *testing.T) {
				checkPaths(t, row, reflect.ValueOf(sample))
			})
		}
	}
}

func checkPaths(t *testing.T, row valueOp, sample reflect.Value) {
	// An empty map makes a plan that is not simple and writes nothing, so the
	// nested walk carries the field and the bytes have nowhere else to differ.
	empty := reflect.StructField{Name: "M", Type: reflect.TypeFor[map[string]int](), Tag: `cb:"2"`}
	wideEmpty := empty
	wideEmpty.Tag = `cb:"21"`

	type path struct {
		name   string
		fields []reflect.StructField
		set    func(field reflect.Value)
	}
	direct := func(field reflect.Value) { field.Set(sample) }
	pointer := func(field reflect.Value) {
		box := reflect.New(sample.Type())
		box.Elem().Set(sample)
		field.Set(box)
	}
	value := func(tag string) reflect.StructField {
		return reflect.StructField{Name: "V", Type: row.goType, Tag: reflect.StructTag(tag)}
	}
	pointed := func(tag string) reflect.StructField {
		return reflect.StructField{Name: "V", Type: reflect.PointerTo(row.goType), Tag: reflect.StructTag(tag)}
	}
	paths := []path{
		{"flat narrow", []reflect.StructField{value(`cb:"1"`)}, direct},
		{"nested narrow", []reflect.StructField{value(`cb:"1"`), empty}, direct},
		{"flat wide", []reflect.StructField{value(`cb:"20"`)}, direct},
		{"nested wide", []reflect.StructField{value(`cb:"20"`), wideEmpty}, direct},
	}
	// A pointer to a slice is a pointer to a composite, which is not carried.
	if row.goType.Kind() != reflect.Slice {
		paths = append(paths,
			path{"pointer narrow", []reflect.StructField{pointed(`cb:"1"`), empty}, pointer},
			path{"pointer wide", []reflect.StructField{pointed(`cb:"20"`), wideEmpty}, pointer},
		)
	}

	var narrowField, wideField []byte
	var narrowJSON, wideJSON []byte
	for _, one := range paths {
		structType := reflect.StructOf(one.fields)
		record := reflect.New(structType)
		one.set(record.Elem().Field(0))

		message, err := Marshal(record.Interface())
		if err != nil {
			t.Fatalf("%s: %v", one.name, err)
		}
		// The field's bytes: everything after the root byte.
		field := message[1:]
		reference, text := &narrowField, &narrowJSON
		if message[0] == rootStructWide {
			reference, text = &wideField, &wideJSON
		}
		if *reference == nil {
			*reference = field
		} else if !bytes.Equal(field, *reference) {
			t.Errorf("%s wrote %x, where the flat walk wrote %x", one.name, field, *reference)
		}

		back := reflect.New(structType)
		if err := Unmarshal(message, back.Interface()); err != nil {
			t.Fatalf("%s: Unmarshal: %v", one.name, err)
		}
		if !reflect.DeepEqual(back.Elem().Field(0).Interface(), record.Elem().Field(0).Interface()) {
			t.Errorf("%s read back %v", one.name, back.Elem().Field(0))
		}

		schema, err := SchemaOf(record.Interface())
		if err != nil {
			t.Fatal(err)
		}
		rendered, err := ToJSON(schema, message)
		if err != nil {
			t.Fatalf("%s: ToJSON: %v", one.name, err)
		}
		// Only the value: the map renders as null beside it on some paths.
		rendered = bytes.Replace(rendered, []byte(`,"M":null`), nil, 1)
		if *text == nil {
			*text = rendered
		} else if !bytes.Equal(rendered, *text) {
			t.Errorf("%s rendered %s, where the flat walk rendered %s", one.name, rendered, *text)
		}
	}
	if !bytes.Equal(narrowJSON, wideJSON) {
		t.Errorf("the widths render differently: %s and %s", narrowJSON, wideJSON)
	}
}
