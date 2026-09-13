package codec

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

// row is the element type a dynamic array of records is made of, which is the
// shape this feature exists for.
type row struct {
	ID   int32  `cb:"1"`
	Name string `cb:"2"`
}

func rows(count int) []row {
	out := make([]row, count)
	for index := range out {
		out[index] = row{ID: int32(index + 1), Name: fmt.Sprintf("r%d", index)}
	}
	return out
}

// selfDescribing encodes a value standalone and renders it back, which is the
// delivery a browser gets.
func selfDescribing(t *testing.T, value any) []byte {
	t.Helper()
	data, err := MarshalSelfDescribing(value)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := ToJSON(nil, data)
	if err != nil {
		t.Fatalf("to json: %v", err)
	}
	return out
}

// The whole feature in one test: a `map[string]any` renders as the object
// encoding/json would have written for it.
func TestADynamicMapRendersAsJSON(t *testing.T) {
	for _, value := range []map[string]any{
		{"n": 1, "text": "two", "ratio": 3.5, "flag": true, "nothing": nil},
		{"nested": map[string]any{"inner": []any{1, "a", false}}},
		{"empty map": map[string]any{}, "empty list": []any{}},
		{"blob": []byte{0x00, 0x7F, 0x80, 0xFF}},
		{"widths": []any{
			int8(-1), int16(-2), int32(-3), int64(-4),
			uint8(1), uint16(2), uint32(3), uint64(4),
			float32(1.5), float64(-0.25),
		}},
		{"big": uint64(math.MaxUint64), "small": int64(math.MinInt64)},
	} {
		want, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		sameJSON(t, selfDescribing(t, value), want)
	}
}

// The optimisation the shape needs: an array of records inside an `any` must not
// write its field names per row.
//
// The measure is the typed encoding of the same slice. A few bytes of framing
// over it is the tag and the map around it; anything near the JSON size means
// the rows went out as objects and the feature is not doing its job.
func TestADynamicArrayOfRecordsCostsWhatATypedOneDoes(t *testing.T) {
	for _, count := range []int{1, 3, 20, 1000} {
		payload := rows(count)

		typed, err := MarshalSelfDescribing(payload)
		if err != nil {
			t.Fatal(err)
		}
		dynamic, err := MarshalSelfDescribing(map[string]any{"rows": payload})
		if err != nil {
			t.Fatal(err)
		}
		// The map, its one key, the tag, and the envelope round the map.
		const framing = 24
		if over := len(dynamic) - len(typed); over > framing {
			t.Fatalf("%d rows: dynamic %d bytes against typed %d, %d over",
				count, len(dynamic), len(typed), over)
		}
		// And it is the same document either way.
		want, err := json.Marshal(map[string]any{"rows": payload})
		if err != nil {
			t.Fatal(err)
		}
		sameJSON(t, selfDescribing(t, map[string]any{"rows": payload}), want)
	}
}

// A `[]any` that happens to hold one record type is the same thousand records as
// a `[]Sale`, and has to cost the same.
//
// Without this it costs half as much again: a type tag on every element, and no
// table, because each record would be framed on its own. The elements are boxed
// and so not contiguous, which is the only reason it is not automatic.
func TestABoxedArrayOfRecordsCostsWhatATypedOneDoes(t *testing.T) {
	for _, count := range []int{1, 3, tableThreshold, 1000} {
		typed := rows(count)
		boxed := make([]any, count)
		pointers := make([]any, count)
		for index := range typed {
			boxed[index] = typed[index]
			pointers[index] = &typed[index]
		}

		want, err := MarshalSelfDescribing(map[string]any{"rows": typed})
		if err != nil {
			t.Fatal(err)
		}
		// Byte for byte, not merely close: the boxed run resolves to the same
		// plan, the same tag and the same layout, so any difference at all means
		// one of them took a path the other did not.
		for name, value := range map[string][]any{"[]any": boxed, "[]any of pointers": pointers} {
			got, err := MarshalSelfDescribing(map[string]any{"rows": value})
			if err != nil {
				t.Fatalf("%d rows, %s: %v", count, name, err)
			}
			if string(got) != string(want) {
				t.Fatalf("%d rows: %s wrote %d bytes and []row wrote %d",
					count, name, len(got), len(want))
			}
		}
	}
}

// And a sequence that is *not* uniform falls back to a list of dynamic values
// rather than forcing one type on the rest.
func TestAMixedArrayIsStillAListOfDynamicValues(t *testing.T) {
	uniform := rows(2)
	for name, value := range map[string][]any{
		"two types":     {uniform[0], struct{ X int32 }{7}},
		"a nil":         {uniform[0], nil},
		"a scalar":      {uniform[0], 42},
		"a nil pointer": {&uniform[0], (*row)(nil)},
	} {
		payload := map[string]any{"rows": value}
		want, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		sameJSON(t, selfDescribing(t, payload), want)
	}
}

// Past the threshold a typed slice is transposed into columns, and a dynamic one
// has to be too — the tag names the struct and what follows is an ordinary
// value, table included.
func TestADynamicArrayStillBecomesATable(t *testing.T) {
	below := rows(tableThreshold - 1)
	above := rows(tableThreshold)

	perRow := func(payload []row) float64 {
		data, err := MarshalSelfDescribing(map[string]any{"rows": payload})
		if err != nil {
			t.Fatal(err)
		}
		return float64(len(data)) / float64(len(payload))
	}
	// The transpose is worth about a third of a row at this size. Comparing the
	// two rates rather than pinning a byte count keeps this a test of the layout
	// and not of the corpus.
	if perRow(above) >= perRow(below) {
		t.Fatalf("%d rows cost %.1f bytes each and %d cost %.1f: the table did not engage",
			len(above), perRow(above), len(below), perRow(below))
	}
}

// Without a section there is nowhere to put a struct def, so a record goes out
// as a map of its field names. Both are the same document; one is much larger,
// and that is worth pinning so the difference cannot be discovered in production.
func TestAStructWithoutASectionIsAnObject(t *testing.T) {
	value := map[string]any{"rows": rows(20)}

	plain, err := Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	standalone, err := MarshalSelfDescribing(value)
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) <= len(standalone) {
		t.Fatalf("the named form is %d bytes and the tagged one %d: "+
			"the tag is meant to be the smaller", len(plain), len(standalone))
	}

	// The document is the same whichever way it went out, which is what makes
	// the size the only difference.
	schema, err := SchemaOf(value)
	if err != nil {
		t.Fatal(err)
	}
	fromPlain, err := ToJSON(schema, plain)
	if err != nil {
		t.Fatal(err)
	}
	fromStandalone, err := ToJSON(nil, standalone)
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, fromPlain, fromStandalone)

	want, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, fromPlain, want)
}

// A dynamic map is written in key order. Without that the same value encodes to
// different bytes every time, and no two implementations can be pinned against
// each other.
func TestADynamicMapEncodesTheSameBytesEveryTime(t *testing.T) {
	value := map[string]any{"z": 1, "a": 2, "m": 3, "b": 4, "y": 5, "c": 6}
	first, err := Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for range 32 {
		again, err := Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(first) {
			t.Fatalf("the same map encoded twice:\n%x\n%x", first, again)
		}
	}
	// And the order is the keys', not the insertion order or the hash order.
	if out := selfDescribing(t, value); string(out) != `{"a":2,"b":4,"c":6,"m":3,"y":5,"z":1}` {
		t.Fatalf("keys came out as %s", out)
	}
}

// The three field shapes, beside the map.
type carrier struct {
	Name  string         `cb:"1"`
	One   any            `cb:"2"`
	Many  []any          `cb:"3"`
	Extra map[string]any `cb:"4"`
}

func TestTheDynamicFieldShapesRoundTrip(t *testing.T) {
	value := carrier{
		Name:  "carrier",
		One:   map[string]any{"a": 1},
		Many:  []any{1, "two", nil, true, 4.5},
		Extra: map[string]any{"rows": rows(3), "total": 3},
	}
	want, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, selfDescribing(t, &value), want)
}

// An absent dynamic field is null, which is what a nil `any` and a nil slice
// both are — and what Unmarshal would leave in the struct.
func TestAnAbsentDynamicFieldIsNull(t *testing.T) {
	out := selfDescribing(t, &carrier{Name: "bare"})
	want, err := json.Marshal(carrier{Name: "bare"})
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, out, want)
}

// A dynamic value costs the wide key width, because a four-bit descriptor has no
// room to name a class. That is a property of the type, so it shows in the root
// byte of every message of it.
func TestADynamicFieldWidensTheKeys(t *testing.T) {
	narrow, err := Marshal(&struct {
		A int32 `cb:"1"`
	}{A: 1})
	if err != nil {
		t.Fatal(err)
	}
	if narrow[0]&rootWide != 0 {
		t.Fatalf("an ordinary type went wide: root %#02x", narrow[0])
	}
	wide, err := Marshal(&struct {
		A int32 `cb:"1"`
		B any   `cb:"2"`
	}{A: 1, B: 2})
	if err != nil {
		t.Fatal(err)
	}
	if wide[0]&rootWide == 0 {
		t.Fatalf("a type with an `any` stayed narrow: root %#02x", wide[0])
	}
}

// Both round trips back into Go, which is the property that keeps a
// self-describing message an ordinary message.
func TestADynamicMapUnmarshalsBackIntoGo(t *testing.T) {
	value := map[string]any{"rows": rows(3), "total": 3, "label": "batch"}

	for _, one := range []struct {
		name   string
		encode func(any) ([]byte, error)
	}{
		{"Marshal", Marshal},
		{"MarshalSelfDescribing", MarshalSelfDescribing},
	} {
		data, err := one.encode(value)
		if err != nil {
			t.Fatalf("%s: %v", one.name, err)
		}
		var back map[string]any
		if err := Unmarshal(data, &back); err != nil {
			t.Fatalf("%s: unmarshal: %v", one.name, err)
		}
		// The records come back as maps rather than as rows: the wire says what
		// they were, and `any` is where they landed.
		want := map[string]any{
			"label": "batch",
			"total": int64(3),
			"rows": []any{
				map[string]any{"ID": int64(1), "Name": "r0"},
				map[string]any{"ID": int64(2), "Name": "r1"},
				map[string]any{"ID": int64(3), "Name": "r2"},
			},
		}
		if !reflect.DeepEqual(back, want) {
			t.Fatalf("%s: round trip gave\n%#v\nwant\n%#v", one.name, back, want)
		}

		// DecodeAny walks the same wire and has to agree with it.
		schema, err := SchemaOf(value)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodeAny(schemaFor(one.name, schema), data)
		if err != nil {
			t.Fatalf("%s: decode any: %v", one.name, err)
		}
		if !reflect.DeepEqual(decoded, any(want)) {
			t.Fatalf("%s: DecodeAny gave\n%#v\nwant\n%#v", one.name, decoded, want)
		}
	}
}

// schemaFor passes the out-of-band schema for the delivery that needs one, and
// nil for the message that carries its own.
func schemaFor(delivery string, schema *Schema) *Schema {
	if delivery == "Marshal" {
		return schema
	}
	return nil
}

// An integer comes back signed where it fits and unsigned only past 2^63, which
// is what a caller who put an `int` in the map expects and what keeps a value
// int64 cannot hold.
func TestADynamicIntegerKeepsItsMagnitude(t *testing.T) {
	value := map[string]any{
		"negative": -1,
		"small":    7,
		"maxInt":   int64(math.MaxInt64),
		"pastInt":  uint64(math.MaxInt64) + 1,
		"maxUint":  uint64(math.MaxUint64),
	}
	data, err := MarshalSelfDescribing(value)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeAny(nil, data)
	if err != nil {
		t.Fatal(err)
	}
	back := decoded.(map[string]any)
	for name, want := range map[string]any{
		"negative": int64(-1),
		"small":    int64(7),
		"maxInt":   int64(math.MaxInt64),
		"pastInt":  uint64(math.MaxInt64) + 1,
		"maxUint":  uint64(math.MaxUint64),
	} {
		if back[name] != want {
			t.Errorf("%s came back %v (%T), want %v (%T)", name, back[name], back[name], want, want)
		}
	}
}

// A float in a dynamic position says which width it was, because there is no
// schema to say it — and a 32-bit pattern read as a 64-bit one prints its
// rounding error rather than failing.
func TestADynamicFloatKeepsItsWidth(t *testing.T) {
	out := selfDescribing(t, map[string]any{"single": float32(0.1), "double": float64(0.1)})
	if got := string(out); got != `{"double":0.1,"single":0.1}` {
		t.Fatalf("floats came out as %s", got)
	}
}

// The values JSON has no spelling for are refused by the text writer and handed
// back by the `any` one, which is the same split an ordinary float field has.
func TestADynamicNonFiniteFloat(t *testing.T) {
	data, err := MarshalSelfDescribing(map[string]any{"nan": math.NaN()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ToJSON(nil, data); err == nil {
		t.Fatal("JSON accepted a NaN")
	}
	decoded, err := DecodeAny(nil, data)
	if err != nil {
		t.Fatal(err)
	}
	if got := decoded.(map[string]any)["nan"].(float64); !math.IsNaN(got) {
		t.Fatalf("NaN came back %v", got)
	}
}

// `[]byte` in a dynamic position is base64, and a string is a string. The wire
// has to keep them apart, because only the schema does elsewhere.
func TestADynamicBlobIsNotAString(t *testing.T) {
	value := map[string]any{"blob": []byte("hi"), "text": "hi"}
	want, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, selfDescribing(t, value), want)
	if !strings.Contains(string(want), `"aGk="`) {
		t.Fatalf("the reference is not base64: %s", want)
	}
}

// A value that holds itself is a value a caller can build by accident, and the
// encoder has to end rather than run the stack out.
func TestADynamicCycleIsRefused(t *testing.T) {
	cycle := map[string]any{}
	cycle["self"] = cycle
	if _, err := Marshal(cycle); err == nil {
		t.Fatal("a map holding itself was encoded")
	} else if !strings.Contains(err.Error(), "holds itself") {
		t.Fatalf("the error does not say what happened: %v", err)
	}

	list := []any{nil}
	list[0] = list
	if _, err := Marshal(map[string]any{"list": list}); err == nil {
		t.Fatal("a list holding itself was encoded")
	}

	// The one the type cannot warn about: `Next` is an `any`, so planFor sees no
	// recursion at all and only the value walk can find it. Both deliveries, since
	// they reach a struct by different routes.
	type node struct {
		Name string `cb:"1"`
		Next any    `cb:"2"`
	}
	self := node{Name: "a"}
	self.Next = &self
	if _, err := Marshal(&self); err == nil {
		t.Fatal("a struct holding itself through an `any` was encoded")
	}
	if _, err := MarshalSelfDescribing(&self); err == nil {
		t.Fatal("a struct holding itself through an `any` was described")
	}

	// And nesting that is merely deep still goes.
	var deep any = 1
	for range maxDynamicDepth / 2 {
		deep = map[string]any{"d": deep}
	}
	if _, err := MarshalSelfDescribing(map[string]any{"deep": deep}); err != nil {
		t.Fatalf("a legitimately deep value was refused: %v", err)
	}
}

// A Go type with no form on the wire is named rather than dropped.
func TestADynamicValueTheFormatCannotCarry(t *testing.T) {
	_, err := Marshal(map[string]any{"f": func() {}})
	if err == nil {
		t.Fatal("a func was encoded")
	}
	if !strings.Contains(err.Error(), "func()") {
		t.Fatalf("the error does not name the type: %v", err)
	}
}

// `map[any]T` is refused: a key has to be renderable as a name, and a dynamic
// one could be a list as easily as a string.
func TestADynamicMapKeyIsRefused(t *testing.T) {
	_, err := Marshal(&struct {
		M map[any]string `cb:"1"`
	}{})
	if err == nil {
		t.Fatal("a map keyed by `any` was accepted")
	}
	if !strings.Contains(err.Error(), "`any` does not") {
		t.Fatalf("the error does not say why: %v", err)
	}
}

// An interface with methods is refused at plan time, because a decoded value
// cannot be promised to satisfy one.
func TestANonEmptyInterfaceIsRefused(t *testing.T) {
	_, err := Marshal(&struct {
		E error `cb:"1"`
	}{})
	if err == nil {
		t.Fatal("an `error` field was accepted")
	}
	if !strings.Contains(err.Error(), "has to be `any`") {
		t.Fatalf("the error does not say what is carried: %v", err)
	}
}

// A message that names a struct the reader has no table for must say so rather
// than index into nothing.
func TestATypedValueWithoutATableIsRefused(t *testing.T) {
	value := map[string]any{"rows": rows(2)}
	data, err := MarshalSelfDescribing(value)
	if err != nil {
		t.Fatal(err)
	}
	// The out-of-band schema for this type describes the map and nothing else —
	// the struct only exists in the value — so it cannot resolve the tag.
	schema, err := SchemaOf(value)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ToJSON(schema, data); err == nil {
		t.Fatal("a tag was resolved against a table that does not hold it")
	} else if !strings.Contains(err.Error(), "names struct") {
		t.Fatalf("the error does not say what failed: %v", err)
	}
}

// AppendChecked is where a Codec[T] hears about a dynamic failure, since Append
// has nowhere to put one.
func TestCodecAppendChecked(t *testing.T) {
	codec := MustCodec[carrier]()
	good := carrier{Name: "ok", One: 1}
	if _, err := codec.AppendChecked(nil, &good); err != nil {
		t.Fatalf("a carriable value failed: %v", err)
	}
	bad := carrier{One: make(chan int)}
	if _, err := codec.AppendChecked(nil, &bad); err == nil {
		t.Fatal("a chan was encoded")
	}
	// Append still writes something, and what it writes still decodes — it is
	// the error that is lost, not the message.
	if data := codec.Append(nil, &bad); len(data) == 0 {
		t.Fatal("Append wrote nothing at all")
	}
}
