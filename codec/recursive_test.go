package codec

import (
	"encoding/hex"
	"reflect"
	"sync"
	"testing"
)

// AstNode is the shape that first hit this: an HTML-ish tree whose children are
// nodes of the same type, so the type graph refers back to itself.
type AstNode struct {
	TagName    string            `json:"tagName,omitempty"`
	Css        string            `json:"css,omitempty"`
	Text       string            `json:"text,omitempty"`
	Children   []AstNode         `json:"children,omitempty"`
	Props      map[string]any    `json:"props,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

// PtrNode recurses through a pointer instead of a slice.
type PtrNode struct {
	Name string
	Next *PtrNode
}

// MapNode recurses through a map value.
type MapNode struct {
	Name  string
	Kids  map[string]MapNode
	Count int32
}

// mutualA / mutualB close the cycle across two types rather than one.
type mutualA struct {
	Label string
	Bs    []mutualB
}

type mutualB struct {
	Weight float64
	As     []mutualA
}

// selfSkipped's only self-reference is behind `cb:"-"`, and selfUnexported's is
// unexported — neither is encoded, so neither type counts as cyclic.
type selfSkipped struct {
	Name string
	Kids []selfSkipped `cb:"-"`
}

type selfUnexported struct {
	Name string
	kids []selfUnexported //nolint:unused // present to prove unexported edges are ignored
}

func roundTrip[T any](t *testing.T, in T) T {
	t.Helper()
	data, err := Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out T
	if err := Unmarshal(data, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	return out
}

func TestRecursiveSliceRoundTrip(t *testing.T) {
	in := []AstNode{{
		TagName: "section",
		Css:     "flex",
		Children: []AstNode{
			{TagName: "h1", Text: "Title", Attributes: map[string]string{"id": "t"}},
			{
				TagName: "div",
				Children: []AstNode{
					{TagName: "p", Text: "deep", Props: map[string]any{"n": int64(3), "ok": true}},
					{TagName: "span"}, // no children: the column that used to recurse forever
				},
			},
		},
	}, {
		TagName: "footer", // whole tree is one node
	}}

	out := roundTrip(t, in)
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round-trip mismatch:\n in=%+v\nout=%+v", in, out)
	}
}

// TestRecursiveEmpty covers the cases that made the encoder recurse with no data
// at all to consume: a zero node, an empty record set, and an empty child list.
func TestRecursiveEmpty(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   any
	}{
		{"zero node", []AstNode{{}}},
		{"no records", []AstNode{}},
		{"empty children", []AstNode{{TagName: "div", Children: []AstNode{}}}},
		{"single struct", AstNode{TagName: "div"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := Marshal(tc.in)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			out := reflect.New(reflect.TypeOf(tc.in))
			if err := Unmarshal(data, out.Interface()); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
		})
	}
}

// An empty Children slice decodes back as nil, matching the existing array
// behaviour (decodeArrayBody leaves zero-length slices at their zero value).
func TestRecursiveEmptySliceDecodesNil(t *testing.T) {
	out := roundTrip(t, []AstNode{{TagName: "div", Children: []AstNode{}}})
	if out[0].Children != nil {
		t.Fatalf("expected nil children, got %#v", out[0].Children)
	}
}

func TestRecursiveDeepChain(t *testing.T) {
	const depth = 200
	root := AstNode{TagName: "n0"}
	cur := &root
	for i := 1; i < depth; i++ {
		cur.Children = []AstNode{{TagName: "n" + string(rune('a'+i%26))}}
		cur = &cur.Children[0]
	}
	out := roundTrip(t, []AstNode{root})
	if !reflect.DeepEqual([]AstNode{root}, out) {
		t.Fatal("deep chain round-trip mismatch")
	}
	// Walk to the bottom to be sure the whole chain survived.
	n, d := &out[0], 1
	for len(n.Children) > 0 {
		n, d = &n.Children[0], d+1
	}
	if d != depth {
		t.Fatalf("chain depth = %d, want %d", d, depth)
	}
}

func TestRecursivePointer(t *testing.T) {
	in := []PtrNode{
		{Name: "a", Next: &PtrNode{Name: "b", Next: &PtrNode{Name: "c"}}},
		{Name: "lonely"}, // nil Next: the nullable column with nothing present
	}
	out := roundTrip(t, in)
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round-trip mismatch:\n in=%+v\nout=%+v", in, out)
	}
}

func TestRecursiveMapValue(t *testing.T) {
	in := []MapNode{{
		Name:  "root",
		Count: 2,
		Kids: map[string]MapNode{
			"x": {Name: "x", Kids: map[string]MapNode{"x1": {Name: "x1"}}},
			"y": {Name: "y", Count: 9},
		},
	}, {
		Name: "bare", // nil map: the value column with nothing in it
	}}
	out := roundTrip(t, in)
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round-trip mismatch:\n in=%+v\nout=%+v", in, out)
	}
}

func TestMutuallyRecursiveTypes(t *testing.T) {
	in := []mutualA{{
		Label: "a1",
		Bs: []mutualB{
			{Weight: 1.5, As: []mutualA{{Label: "a2"}}},
			{Weight: 2.5}, // empty As
		},
	}}
	out := roundTrip(t, in)
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round-trip mismatch:\n in=%+v\nout=%+v", in, out)
	}
}

func TestReachesCycle(t *testing.T) {
	for _, tc := range []struct {
		typ  any
		want bool
	}{
		{AstNode{}, true},
		{[]AstNode{}, true},
		{PtrNode{}, true},
		{MapNode{}, true},
		{mutualA{}, true},
		{mutualB{}, true},
		{ScalarRecord{}, false},
		{[]byte{}, false},
		{map[string]string{}, false},
		{selfSkipped{}, false},    // the self-reference is `cb:"-"`
		{selfUnexported{}, false}, // the self-reference is unexported
	} {
		typ := reflect.TypeOf(tc.typ)
		if got := reachesCycle(typ); got != tc.want {
			t.Errorf("reachesCycle(%s) = %v, want %v", typ, got, tc.want)
		}
	}
}

// Pin representative version-2 messages so future changes to the varint and
// packed5 integration are explicit wire-format decisions.
func TestWireFormatVersion2(t *testing.T) {
	type textLine struct{ Text, Css, Tag string }
	type content struct {
		Title     string
		TextLines []textLine
		IDs       []int32
		Limit     int32
	}
	type section struct {
		Type    string
		Content *content
		Css     map[string]string
		Attrs   map[string]any
		Vals    []string
	}

	cases := []struct {
		val  any
		want string
	}{
		{section{}, "0201053902005b0100050428024204000005035902ef021c02ae0400000000d00000ef060000000202a90600000002072c0400000002"},
		{section{Type: "hero", Vals: []string{"a", "b"}}, "02010539021939243a5b0100050428024204000005035902ef021c02ae0400000000d00000ef060000000202a90600000002072c040000020208610862"},
		{section{Type: "x", Content: &content{Title: "t", TextLines: []textLine{{Text: "l1"}}, IDs: []int32{1, 2, 3}, Limit: 7},
			Css: map[string]string{"k": "v"}, Attrs: map[string]any{"n": int64(4)}},
			"020105390208785b00050428020874420400000105035902106c31ef02001c0200ae040000030000010203d0000007ef0600000102086b020876a90600000102086e070300042c0400000002"},
		{[]textLine{}, "0200035902ef021c02"},
		{[]textLine{{Text: "a", Css: "c"}, {Tag: "p"}}, "0202035902086100ef020863001c02000870"},
		{map[string][]textLine{"z": {{Text: "q"}}}, "020600000102087a04000001050359020871ef02001c0200"},
		{map[string][]textLine{}, "02060000000204000005035902ef021c02"},
		{[]*content{nil, {Title: "p"}}, "02040000020102050428020870420400000005035902ef021c02ae040000000000d0000000"},
		{[]*content{}, "020400000000050428024204000005035902ef021c02ae0400000000d00000"},
	}
	for i, tc := range cases {
		got, err := Marshal(tc.val)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if hex.EncodeToString(got) != tc.want {
			t.Errorf("case %d wire bytes changed:\ngot  %s\nwant %s", i, hex.EncodeToString(got), tc.want)
		}
	}
}

// Recursive layouts are built once and shared, so concurrent first use must not
// publish a half-built typeInfo. Run with -race.
func TestRecursiveConcurrentBuild(t *testing.T) {
	type concNode struct {
		Name string
		Kids []concNode
		Next *concNode
	}
	in := []concNode{{Name: "r", Kids: []concNode{{Name: "k"}}, Next: &concNode{Name: "n"}}}

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			data, err := Marshal(in)
			if err != nil {
				t.Error(err)
				return
			}
			var out []concNode
			if err := Unmarshal(data, &out); err != nil {
				t.Error(err)
				return
			}
			if !reflect.DeepEqual(in, out) {
				t.Errorf("round-trip mismatch: %+v", out)
			}
		}()
	}
	wg.Wait()
}
