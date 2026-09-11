package main

// The composite corpus: compact-mode messages carrying nested structs, arrays of
// structs, maps and nested slices.
//
// It is a second file rather than more cases in vectors.json because the schema
// representation differs. A flat schema names each field's kind with a string
// ("i32", "strs"); a composite one needs a tree, since a nested struct carries a
// whole field list of its own. Keeping them apart leaves the original corpus and
// its parser untouched, which is worth more than one uniform file: those 26
// messages pin the format as it was before composites existed.
//
// As in main.go, every expected value is read back out of the message by the
// real Go compact reader rather than derived from the input, so the corpus
// records what the format actually wrote -- presence included, which for compact
// mode is the whole of the omit-zero rule.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/ivanjoz/colbin"
	"github.com/ivanjoz/colbin/compact"
)

// --- the types ---------------------------------------------------------------

// ccInner is tagged so its ids fit the narrow key, which is what lets a nested
// message use 4-bit keys at all: the key width is message-wide, so every
// reachable struct has to be tagged.
type ccInner struct {
	X int32  `cb:"1"`
	Y string `cb:"2"`
	Z bool   `cb:"3"`
}

// ccUntagged has a hashed id, so any message reaching it pays 8-bit keys.
type ccUntagged struct {
	A int32
}

type ccNested struct {
	ID   int64   `cb:"1"`
	Sub  ccInner `cb:"2"`
	Name string  `cb:"3"`
}

type ccWideNested struct {
	Sub ccUntagged `cb:"1"`
}

type ccRows struct {
	ID   int64     `cb:"1"`
	Rows []ccInner `cb:"2"`
}

type ccMaps struct {
	S map[string]int32   `cb:"1"`
	I map[int32]string   `cb:"2"`
	V map[string]ccInner `cb:"3"`
	L map[string][]int32 `cb:"4"`
}

type ccNestedSlices struct {
	II [][]int32          `cb:"1"`
	SS [][]string         `cb:"2"`
	BB [][]byte           `cb:"3"`
	MM []map[string]int32 `cb:"4"`
}

// ccDeep is three struct levels plus a map of arrays of structs, so the
// recursion is exercised past the one level a single nested field proves.
type ccDeep struct {
	ID    int32                `cb:"1"`
	Mid   ccNested             `cb:"2"`
	Deep  map[string][]ccInner `cb:"3"`
	Lists [][]ccInner          `cb:"4"`
}

// --- output shapes -----------------------------------------------------------

// kindNode is a kind that may hold kinds. The scalar forms keep the same names
// main.go uses, so the two corpora agree on everything they share.
type kindNode struct {
	Kind   string      `json:"kind"`
	Fields []schemaOut `json:"fields,omitempty"` // kind == "struct"
	Elem   *kindNode   `json:"elem,omitempty"`   // kind == "array"
	Key    *kindNode   `json:"key,omitempty"`    // kind == "map"
	Val    *kindNode   `json:"val,omitempty"`    // kind == "map"
}

type schemaOut struct {
	Name string   `json:"name"`
	ID   int      `json:"id"`
	Kind kindNode `json:"k"`
}

type ccCaseOut struct {
	Name        string      `json:"name"`
	Shape       int         `json:"shape"`
	AllPositive bool        `json:"allPositive"`
	NarrowKeys  bool        `json:"narrowKeys"`
	Fields      []schemaOut `json:"fields"`
	Message     string      `json:"message"`
	Records     [][]ccField `json:"records"`
	// See caseOut.RustByteExact in main.go.
	RustByteExact bool `json:"rustByteExact"`
}

type ccField struct {
	ID int `json:"id"`
	V  any `json:"v"`
}

type ccEntry struct {
	K any `json:"k"`
	V any `json:"v"`
}

type ccCorpus struct {
	Cases []ccCaseOut `json:"cases"`
}

// writeComposites generates the corpus. Every case is forced through compact
// mode: Marshal picks the mode on size for a composite type, and a corpus case
// that silently came out columnar would test nothing here.
func writeComposites(out string) {
	var cases []ccCaseOut
	add := func(name string, value any) {
		cases = append(cases, compositeCase(name, value))
	}

	add("cc-nested-struct", ccNested{ID: 7, Sub: ccInner{X: -3, Y: "inner", Z: true}, Name: "outer"})
	// The nested struct holds nothing, so it is omitted entirely and the record
	// never names field 2.
	add("cc-nested-zero", ccNested{ID: 7, Name: "outer"})
	add("cc-nested-only", ccNested{Sub: ccInner{Y: "just the nested one"}})
	add("cc-nested-wide-keys", ccWideNested{Sub: ccUntagged{A: 5}})

	add("cc-rows", ccRows{ID: 1, Rows: []ccInner{{X: 1, Y: "a"}, {X: -2, Y: "b", Z: true}, {}}})
	add("cc-rows-one", ccRows{ID: 1, Rows: []ccInner{{X: 9}}})
	add("cc-rows-empty", ccRows{ID: 1})

	add("cc-maps", ccMaps{
		// One entry per map: Go's iteration order is unspecified, so a corpus
		// case with two would pin an order the encoder does not promise.
		S: map[string]int32{"alpha": -2},
		I: map[int32]string{-7: "neg"},
		V: map[string]ccInner{"x": {X: 1, Y: "y", Z: true}},
		L: map[string][]int32{"nums": {1, 2, 3}},
	})
	add("cc-maps-empty", ccMaps{S: map[string]int32{"only": 1}})

	add("cc-nested-slices", ccNestedSlices{
		II: [][]int32{{1, 2, 3}, nil, {-4}},
		SS: [][]string{{"a", ""}},
		BB: [][]byte{{1, 2}, {}},
		MM: []map[string]int32{{"k": 1}},
	})

	add("cc-deep", ccDeep{
		ID:    -12345,
		Mid:   ccNested{ID: 1, Sub: ccInner{X: 42, Y: "mid", Z: true}, Name: "middle"},
		Deep:  map[string][]ccInner{"k": {{X: 1, Y: "deep"}, {}}},
		Lists: [][]ccInner{{{X: 7}}, nil},
	})

	// Two and three records, so the array shapes carry composites too.
	add("cc-shape-2", []ccNested{
		{ID: 1, Sub: ccInner{X: 1}},
		{ID: 2, Name: "two"},
	})
	add("cc-shape-3", []ccRows{
		{ID: 1, Rows: []ccInner{{X: 1}}},
		{ID: 2},
		{ID: 3, Rows: []ccInner{{Y: "three"}, {Z: true}}},
	})

	body, err := json.MarshalIndent(ccCorpus{Cases: cases}, "", "  ")
	if err != nil {
		panic(err)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		panic(err)
	}
	if err := os.WriteFile(out, append(body, '\n'), 0o644); err != nil {
		panic(err)
	}
	fmt.Printf("%d composite messages -> %s\n", len(cases), out)
}

func compositeCase(name string, value any) ccCaseOut {
	data, err := colbin.MarshalForceCompact(value)
	if err != nil {
		panic(fmt.Sprintf("%s: %v", name, err))
	}
	elem := reflect.TypeOf(value)
	if elem.Kind() == reflect.Slice {
		elem = elem.Elem()
	}
	fields := compositeSchema(elem)

	r, err := compact.NewReader(data)
	if err != nil {
		panic(fmt.Sprintf("%s: %v", name, err))
	}
	c := ccCaseOut{
		Name:        name,
		Shape:       int(r.Shape()),
		AllPositive: r.AllPositive(),
		NarrowKeys:  r.Keys() == compact.Keys4,
		Fields:      fields,
		Message:     base64.StdEncoding.EncodeToString(data),

		RustByteExact: everyStringIsRaw(value),
	}
	for range r.Records() {
		c.Records = append(c.Records, readCompositeRecord(name, r, fields))
	}
	if err := r.Err(); err != nil {
		panic(fmt.Sprintf("%s: %v", name, err))
	}
	// The corpus is only as good as its round trip.
	dst := reflect.New(reflect.TypeOf(value))
	if err := colbin.Unmarshal(data, dst.Interface()); err != nil {
		panic(fmt.Sprintf("%s: round trip: %v", name, err))
	}
	return c
}

// --- the schema tree ---------------------------------------------------------

// compositeSchema is main.go's fieldsOf with a recursive kind. The ids still come
// from a schema section colbin wrote, one MarshalJSON per struct type, so nothing
// here re-derives the id hash.
func compositeSchema(t reflect.Type) []schemaOut {
	flat := fieldIDsOf(t)
	sfs := encodableFields(t)
	out := make([]schemaOut, len(flat))
	for i := range flat {
		out[i] = schemaOut{Name: flat[i].Name, ID: flat[i].ID, Kind: kindNodeOf(sfs[i].Type)}
	}
	return out
}

func kindNodeOf(t reflect.Type) kindNode {
	switch t.Kind() {
	case reflect.Struct:
		return kindNode{Kind: "struct", Fields: compositeSchema(t)}
	case reflect.Map:
		key, val := kindNodeOf(t.Key()), kindNodeOf(t.Elem())
		return kindNode{Kind: "map", Key: &key, Val: &val}
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return kindNode{Kind: "bytes"}
		}
		// A slice of scalars keeps its bulk-codec name; anything else is an
		// element-by-element array, which is the distinction the wire makes.
		if isScalarKind(t.Elem()) {
			return kindNode{Kind: kindName(t)}
		}
		elem := kindNodeOf(t.Elem())
		return kindNode{Kind: "array", Elem: &elem}
	}
	return kindNode{Kind: kindName(t)}
}

// isScalarKind reports whether a slice of t goes to the bulk codecs rather than
// being written element by element -- the same split compactValueOp makes.
func isScalarKind(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Bool, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64, reflect.String:
		return true
	}
	return false
}

// --- reading the message back ------------------------------------------------

func readCompositeRecord(name string, r *compact.Reader, fields []schemaOut) []ccField {
	byID := map[int]kindNode{}
	for _, f := range fields {
		byID[f.ID] = f.Kind
	}
	record := []ccField{}
	for {
		key := r.Key()
		if err := r.Err(); err != nil {
			panic(fmt.Sprintf("%s: %v", name, err))
		}
		if key == compact.TerminatorKey {
			return record
		}
		kind, ok := byID[int(key)]
		if !ok {
			panic(fmt.Sprintf("%s: message names field id %d, which the schema does not have", name, key))
		}
		record = append(record, ccField{ID: int(key), V: readCompositeValue(name, r, kind)})
	}
}

// readCompositeValue mirrors what the Rust decoder must do: dispatch on the
// schema, and recurse for the three composite forms.
func readCompositeValue(name string, r *compact.Reader, kind kindNode) any {
	switch kind.Kind {
	case "struct":
		return readCompositeRecord(name, r, kind.Fields)
	case "array":
		n, ok := r.Count(minBitsOf(r, *kind.Elem))
		if !ok {
			panic(fmt.Sprintf("%s: bad array count", name))
		}
		out := []any{}
		for range n {
			out = append(out, readCompositeValue(name, r, *kind.Elem))
		}
		return out
	case "map":
		n, ok := r.Count(minBitsOf(r, *kind.Key) + minBitsOf(r, *kind.Val))
		if !ok {
			panic(fmt.Sprintf("%s: bad map count", name))
		}
		out := []ccEntry{}
		for range n {
			k := readCompositeValue(name, r, *kind.Key)
			v := readCompositeValue(name, r, *kind.Val)
			out = append(out, ccEntry{K: k, V: v})
		}
		return out
	}
	return readCompact(r, kind.Kind)
}

// minBitsOf is compactElemMinBits, which the count bounds check needs.
func minBitsOf(r *compact.Reader, kind kindNode) int {
	switch kind.Kind {
	case "bool":
		return 1
	case "f32":
		return 32
	case "f64":
		return 64
	case "struct":
		return r.Keys().Bits()
	}
	return 8
}
