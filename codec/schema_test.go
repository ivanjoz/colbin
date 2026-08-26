package codec

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"reflect"
	"testing"
)

// jsonBody returns the part of a MarshalJSON message after the schema section —
// the bytes a plain Marshal message carries after its version byte.
func jsonBody(t *testing.T, msg []byte) []byte {
	t.Helper()
	if msg[0] != jsonFormatVersion {
		t.Fatalf("version byte = 0x%02x, want 0x%02x", msg[0], jsonFormatVersion)
	}
	schemaLen, m := binary.Uvarint(msg[1:])
	if m <= 0 {
		t.Fatal("bad schema length")
	}
	return msg[1+m+int(schemaLen):]
}

type jsonRow struct {
	ID      int64   `json:"id"`
	Company int32   `cb:"co" json:"company"`
	Ratio   float32 `json:"ratio"`
	Name    string  `json:"name"`
	Active  bool    `json:"active"`
	Big     uint64  `json:"big"`
	Small   uint8   `json:"small"`
	Blob    []byte  `json:"blob"`
	Tags    []string
	Sub     jsonSub          `json:"sub"`
	Counts  map[string]int32 `json:"counts"`
	Opt     *int32           `json:"opt"`
	Free    any              `json:"free"`
}

type jsonSub struct {
	Code  int16   `json:"code"`
	Label string  `json:"label"`
	Rate  float64 `json:"rate"`
}

func sampleJSONRows() []jsonRow {
	four := int32(4)
	return []jsonRow{
		{
			ID: 1, Company: -7, Ratio: 1.1, Name: "uno", Active: true,
			Big: math.MaxUint64, Small: 250, Blob: []byte{1, 2, 3},
			Tags:   []string{"a", "b"},
			Sub:    jsonSub{Code: -300, Label: "sub-uno", Rate: 2.5},
			Counts: map[string]int32{"x": 1, "y": 2},
			Opt:    &four,
			Free:   map[string]any{"k": "v", "n": int64(9)},
		},
		{
			ID: 2, Company: 8, Ratio: -0.25, Name: "dos", Active: false,
			Big: 0, Small: 1, Blob: []byte{9},
			Tags:   []string{"c"},
			Sub:    jsonSub{Code: 300, Label: "sub-dos", Rate: -0.5},
			Counts: map[string]int32{"z": 3},
			Opt:    nil,
			Free:   []any{int64(1), "two", true},
		},
	}
}

// deterministicJSONRows trims every map down to one entry. Map iteration order
// is random, so a multi-entry map column is not byte-comparable between two
// encodes of the same value — which the identity check below relies on.
func deterministicJSONRows() []jsonRow {
	rows := sampleJSONRows()
	for i := range rows {
		rows[i].Counts = map[string]int32{"only": int32(i)}
		rows[i].Free = map[string]any{"k": "v"}
	}
	return rows
}

// The whole point of the mode: the schema is additive, so the binary payload it
// wraps must be exactly the one Marshal already produces.
func TestMarshalJSONBodyIsIdenticalToMarshal(t *testing.T) {
	cases := []any{
		deterministicJSONRows(),
		deterministicJSONRows()[0],
		map[string]int32{"a": 1},
		[]*jsonSub{{Code: 1, Label: "x"}, nil},
		int64(42),
		[]string{"p", "q"},
	}
	for _, v := range cases {
		plain, err := Marshal(v)
		if err != nil {
			t.Fatalf("Marshal(%T): %v", v, err)
		}
		withSchema, err := MarshalJSON(v)
		if err != nil {
			t.Fatalf("MarshalJSON(%T): %v", v, err)
		}
		if got, want := jsonBody(t, withSchema), plain[1:]; !reflect.DeepEqual(got, want) {
			t.Fatalf("%T: body differs from Marshal\n got %v\nwant %v", v, got, want)
		}
		// The typed decoder must also accept the self-describing form.
		out := reflect.New(reflect.TypeOf(v))
		if err := Unmarshal(withSchema, out.Interface()); err != nil {
			t.Fatalf("Unmarshal(%T) of a JSON-mode message: %v", v, err)
		}
		if !reflect.DeepEqual(out.Elem().Interface(), v) {
			t.Fatalf("%T: round trip mismatch\n got %#v\nwant %#v", v, out.Elem().Interface(), v)
		}
	}
}

// DecodeJSON has no Go type to work from, so encoding/json over the original
// value is the reference: same keys, same numbers, same base64 blobs.
func TestDecodeJSONMatchesEncodingJSON(t *testing.T) {
	rows := sampleJSONRows()
	msg, err := MarshalJSON(rows)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeJSON(msg)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	assertSameJSON(t, got, want)
}

func TestDecodeJSONSingleStructIsAnObject(t *testing.T) {
	row := sampleJSONRows()[0]
	msg, err := MarshalJSON(row)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeJSON(msg)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0] != '{' {
		t.Fatalf("want a JSON object, got %s", got)
	}
	want, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	assertSameJSON(t, got, want)
}

func TestDecodeJSONValueMode(t *testing.T) {
	cases := []any{
		map[string]int32{"a": 1, "b": -2},
		[]*jsonSub{{Code: 1, Label: "x", Rate: 0.5}, nil},
		[]string{"p", "q"},
		int64(-42),
		map[int32]string{7: "seven", -8: "minus"},
		[][]int16{{1, 2}, {3}},
		[]map[string]int64{{"a": 1}, nil},
		[][]byte{{1, 2}, {3}},
		[]bool{true, false},
		[]float32{1.1, -0.5},
	}
	for _, v := range cases {
		msg, err := MarshalJSON(v)
		if err != nil {
			t.Fatalf("MarshalJSON(%T): %v", v, err)
		}
		got, err := DecodeJSON(msg)
		if err != nil {
			t.Fatalf("DecodeJSON(%T): %v", v, err)
		}
		want, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		assertSameJSON(t, got, want)
	}
}

// A self-referential type elides its empty columns, and the schema has to carry
// enough for the reader to expect that.
func TestDecodeJSONRecursiveType(t *testing.T) {
	type node struct {
		Name string `json:"name"`
		Kids []node `json:"kids"`
		Next *node  `json:"next"`
	}
	rows := []node{
		{Name: "root", Kids: []node{{Name: "a"}, {Name: "b", Kids: []node{{Name: "b1"}}}}},
		{Name: "lone", Next: &node{Name: "tail"}},
	}
	msg, err := MarshalJSON(rows)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeJSON(msg)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	assertSameJSON(t, got, want)
}

// json:"-" leaves the field in the payload (cb owns that decision), so it still
// needs a key: the cb name, else the Go field name.
func TestJSONNameFallbacks(t *testing.T) {
	type row struct {
		A int32 `json:"a_json" cb:"a_cb"`
		B int32 `cb:"b_cb"`
		C int32 `json:"-"`
		D int32 `json:",omitempty"`
		E int32
	}
	msg, err := MarshalJSON([]row{{A: 1, B: 2, C: 3, D: 4, E: 5}})
	if err != nil {
		t.Fatal(err)
	}
	v, err := DecodeAny(msg)
	if err != nil {
		t.Fatal(err)
	}
	rec := v.([]any)[0].(map[string]any)
	for key, want := range map[string]int64{"a_json": 1, "b_cb": 2, "C": 3, "D": 4, "E": 5} {
		if got, ok := rec[key]; !ok || got != want {
			t.Fatalf("key %q = %v (present %v), want %v", key, got, ok, want)
		}
	}
}

// A json tag must not move a field id, or adding one would break every reader
// holding the old Go type.
func TestJSONTagDoesNotChangeTheBinary(t *testing.T) {
	type plain struct {
		ID   int64
		Name string
	}
	type tagged struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	a, err := Marshal([]plain{{ID: 5, Name: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Marshal([]tagged{{ID: 5, Name: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("json tag changed the payload:\n%v\n%v", a, b)
	}
}

func TestDecodeJSONNonFiniteFloatsBecomeNull(t *testing.T) {
	type row struct {
		F float64 `json:"f"`
		G float32 `json:"g"`
	}
	msg, err := MarshalJSON([]row{{F: math.NaN(), G: float32(math.Inf(1))}, {F: 1.5, G: 2.5}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeJSON(msg)
	if err != nil {
		t.Fatal(err)
	}
	if want := `[{"f":null,"g":null},{"f":1.5,"g":2.5}]`; string(got) != want {
		t.Fatalf("got %s want %s", got, want)
	}
	// DecodeAny stays faithful: the NaN is still there.
	v, err := DecodeAny(msg)
	if err != nil {
		t.Fatal(err)
	}
	if f := v.([]any)[0].(map[string]any)["f"].(float64); !math.IsNaN(f) {
		t.Fatalf("DecodeAny f = %v, want NaN", f)
	}
}

func TestDecodeRejectsSchemalessAndCorruptMessages(t *testing.T) {
	plain, err := Marshal([]jsonSub{{Code: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeJSON(plain); err == nil {
		t.Fatal("want an error for a message without a schema")
	}
	if _, err := DecodeJSON(nil); err == nil {
		t.Fatal("want an error for an empty message")
	}
	msg, err := MarshalJSON([]jsonSub{{Code: 1, Label: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, cut := range []int{2, len(msg) / 2, len(msg) - 1} {
		if _, err := DecodeJSON(msg[:cut]); err == nil {
			t.Fatalf("want an error for a message truncated at %d", cut)
		}
	}
}

// assertSameJSON compares two JSON documents by value, so object key order and
// the exact spelling of numbers do not matter.
func assertSameJSON(t *testing.T, got, want []byte) {
	t.Helper()
	var gv, wv any
	if err := json.Unmarshal(got, &gv); err != nil {
		t.Fatalf("decoding produced invalid JSON: %v\n%s", err, got)
	}
	if err := json.Unmarshal(want, &wv); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gv, wv) {
		t.Fatalf("JSON mismatch\n got %s\nwant %s", got, want)
	}
}

func TestDecodeJSONNullableMapValues(t *testing.T) {
	one := int32(1)
	msg, err := MarshalJSON(map[string]*int32{"set": &one, "unset": nil})
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeJSON(msg)
	if err != nil {
		t.Fatal(err)
	}
	assertSameJSON(t, got, []byte(`{"set":1,"unset":null}`))
}

func TestDecodeJSONEmptyRecordSlice(t *testing.T) {
	msg, err := MarshalJSON([]jsonSub{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeJSON(msg)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "[]" {
		t.Fatalf("got %s, want []", got)
	}
}
