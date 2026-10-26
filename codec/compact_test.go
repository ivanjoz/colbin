package codec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

// compactEdges is a document with a value at each edge of the compact code
// table: where a fix code ends, where a width grows, and the floats that are
// whole numbers and the ones that only look like it.
func compactEdges() map[string]any {
	entries := func(count int) map[string]any {
		out := map[string]any{}
		for index := range count {
			out[fmt.Sprint("k", index)] = float64(index)
		}
		return out
	}
	values := func(count int) []any {
		out := make([]any, count)
		for index := range out {
			out[index] = fmt.Sprint(index)
		}
		return out
	}
	return map[string]any{
		"floats": []any{0.0, 1.0, 15.0, 16.0, 127.0, 128.0, -1.0, -16.0, 0.1, 2.5, 1e15, 1e300, -1e300,
			math.Pow(2, 62), -math.Pow(2, 63), math.Pow(2, 63), math.Pow(2, 64), math.SmallestNonzeroFloat64,
			math.MaxFloat64, math.Inf(1), math.Inf(-1)},
		"single": []any{float32(0), float32(0.1), float32(-3), float32(math.MaxFloat32)},
		"ints": []any{0, 127, 128, 255, 256, -1, -128, -129, int64(math.MinInt64), int64(math.MaxInt64),
			int8(-5), uint16(70), uint32(1 << 31)},
		"uint": uint64(math.MaxUint64),
		"strings": []any{"", strings.Repeat("a", 31), strings.Repeat("b", 32), strings.Repeat("c", 255),
			strings.Repeat("d", 256), strings.Repeat("e", 70000)},
		"bytes":  []byte{0, 1, 2},
		"none":   nil,
		"yes":    true,
		"no":     false,
		"map15":  entries(15),
		"map16":  entries(16),
		"map300": entries(300),
		"list15": values(15),
		"list16": values(16),
		"list0":  []any{},
		"map0":   map[string]any{},
		"deep":   map[string]any{"a": []any{map[string]any{"b": []any{[]any{1.5, "x"}}}}},
		"":       "the empty key",
	}
}

// normalized is value as a dynamic decode gives it back: a signed integer as
// an int64, an unsigned one too unless it is past 2^63, a float32 as a
// float64.
func normalized(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, element := range typed {
			out[key] = normalized(element)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for index, element := range typed {
			out[index] = normalized(element)
		}
		return out
	case int:
		return int64(typed)
	case int8:
		return int64(typed)
	case uint16:
		return int64(typed)
	case uint32:
		return int64(typed)
	case float32:
		return float64(typed)
	}
	return value
}

// Every edge comes back as itself, through Unmarshal, a Codec and DecodeAny,
// and as the JSON encoding/json writes for it.
func TestCompactEdgesRoundTrip(t *testing.T) {
	value := compactEdges()
	want := normalized(value)
	data := must(Marshal(value))
	if data[0] != rootDynamicMap {
		t.Fatalf("a map[string]any's root is %#02x", data[0])
	}
	var back map[string]any
	if err := Unmarshal(data, &back); err != nil || !reflect.DeepEqual(back, want) {
		t.Fatalf("Unmarshal: %v\n got %v", err, back)
	}
	// Into a used map, which is cleared first.
	used := map[string]any{"stale": 1}
	if err := MustCodec[map[string]any]().Unmarshal(data, &used); err != nil || !reflect.DeepEqual(used, want) {
		t.Fatalf("Codec.Unmarshal into a used map: %v", err)
	}
	if decoded, err := DecodeAny(nil, data); err != nil || !reflect.DeepEqual(decoded, want) {
		t.Fatalf("DecodeAny: %v", err)
	}
	// JSON has no infinities, so the text is checked without them.
	finite := compactEdges()
	finite["floats"] = finite["floats"].([]any)[:19]
	text, err := ToJSON(nil, must(Marshal(finite)))
	if err != nil {
		t.Fatal(err)
	}
	expected, _ := json.Marshal(finite)
	sameJSON(t, text, expected)

	// A negative zero keeps its sign.
	data = must(Marshal(map[string]any{"z": math.Copysign(0, -1)}))
	if err := Unmarshal(data, &back); err != nil || !math.Signbit(back["z"].(float64)) {
		t.Fatalf("negative zero came back %v, %v", back["z"], err)
	}
	// A NaN is a NaN.
	data = must(Marshal(map[string]any{"n": math.NaN()}))
	if err := Unmarshal(data, &back); err != nil || !math.IsNaN(back["n"].(float64)) {
		t.Fatalf("NaN came back %v, %v", back["n"], err)
	}
}

// A dynamic document at the root costs its root byte and nothing else: a map
// is its entries to the end, a list one compact value.
func TestADynamicRootIsTheDocument(t *testing.T) {
	for _, check := range []struct {
		value any
		want  []byte
	}{
		{map[string]any{"a": 1.0}, []byte{0xD1, 0xA1, 'a', 0xF1}},
		{map[string]any{}, []byte{0xD1}},
		{[]any{1.0, "x", nil}, []byte{0xD3, 0x93, 0xF1, 0xA1, 'x', 0xC0}},
		{[]any{}, []byte{0xD3, 0x90}},
	} {
		data := must(Marshal(check.value))
		if !bytes.Equal(data, check.want) {
			t.Fatalf("%v: %x, want %x", check.value, data, check.want)
		}
	}
	list := []any{"a", map[string]any{"b": []any{true}}}
	var back []any
	if err := Unmarshal(must(Marshal(list)), &back); err != nil || !reflect.DeepEqual(back, list) {
		t.Fatalf("a []any root came back %v, %v", back, err)
	}
	// A document is not a struct, nor the other document.
	var record charge
	var asList []any
	var asMap map[string]any
	if Unmarshal(must(Marshal(map[string]any{"a": 1})), &record) == nil ||
		Unmarshal(must(Marshal(map[string]any{"a": 1})), &asList) == nil ||
		Unmarshal(must(Marshal(list)), &asMap) == nil {
		t.Fatal("a dynamic root decoded into the wrong type")
	}
}

// Cut inside a value, a document fails. (A map root cut between two entries
// is the document of the entries before the cut: a message ends where its
// buffer does, as every message's key run does.)
func TestACutDynamicDocumentFails(t *testing.T) {
	value := map[string]any{"key": []any{1.5, "two", map[string]any{"x": 300.25}}}
	data := must(Marshal(value))
	for cut := 2; cut < len(data); cut++ {
		var back map[string]any
		if Unmarshal(data[:cut], &back) == nil {
			t.Fatalf("a message cut at %d of %d decoded", cut, len(data))
		}
		if _, err := DecodeAny(nil, data[:cut]); err == nil {
			t.Fatalf("DecodeAny of a message cut at %d of %d", cut, len(data))
		}
	}
	list := must(Marshal([]any{1.5, "two"}))
	for cut := 1; cut < len(list); cut++ {
		var back []any
		if Unmarshal(list[:cut], &back) == nil {
			t.Fatalf("a list cut at %d of %d decoded", cut, len(list))
		}
	}
}

// A struct inside a dynamic document is written TYPED by MarshalSelfDescribing
// — the compact tag, then the record or the rows as a typed field writes them —
// and comes back as maps.
func TestATypedValueInsideACompactDocument(t *testing.T) {
	value := map[string]any{"one": row{ID: 1, Name: "a"}, "many": rows(10), "few": rows(2),
		"nested": []any{map[string]any{"in": rows(3)}}}
	data := must(MarshalSelfDescribing(value))
	if data[0] != rootDynamicMap|rootSchema {
		t.Fatalf("root %#02x", data[0])
	}
	decoded, err := DecodeAny(nil, data)
	if err != nil {
		t.Fatal(err)
	}
	expected, _ := json.Marshal(map[string]any{"one": map[string]any{"ID": 1, "Name": "a"},
		"many": rowMaps(10), "few": rowMaps(2), "nested": []any{map[string]any{"in": rowMaps(3)}}})
	text, _ := json.Marshal(decoded)
	sameJSON(t, text, expected)
	var back map[string]any
	if err := Unmarshal(data, &back); err != nil || len(back["many"].([]any)) != 10 {
		t.Fatalf("Unmarshal: %v %v", back, err)
	}
}

func rowMaps(count int) []any {
	out := make([]any, count)
	for index, record := range rows(count) {
		out[index] = map[string]any{"ID": record.ID, "Name": record.Name}
	}
	return out
}
