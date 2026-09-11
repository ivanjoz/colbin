package codec

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/ivanjoz/colbin/compact"
)

// Compact mode's composite forms: a nested struct as another key run, an array
// as a count followed by its elements, a map as a count followed by key/value
// pairs. Nothing here goes through Marshal's size comparison -- these use
// MarshalForceCompact, so a failure is the compact path's and not a fallback to
// columnar quietly passing the round trip.

type ccInner struct {
	X int32  `cb:"1"`
	Y string `cb:"2"`
	Z bool   `cb:"3"`
}

type ccOuter struct {
	ID     int64                `cb:"1"`
	Inner  ccInner              `cb:"2"`
	Rows   []ccInner            `cb:"3"`
	ByName map[string]int32     `cb:"4"`
	ByID   map[int32]ccInner    `cb:"5"`
	Nested [][]int32            `cb:"6"`
	Lists  []map[string]string  `cb:"7"`
	Blobs  [][]byte             `cb:"8"`
	Deep   map[string][]ccInner `cb:"9"`
	Name   string               `cb:"10"`
}

// forceRoundTrip encodes with compact mode guaranteed, checks the message really
// is compact, and decodes it back into a fresh value of the same type.
func forceRoundTrip[T any](t *testing.T, in T) T {
	t.Helper()
	buf, err := MarshalForceCompact(in)
	if err != nil {
		t.Fatalf("MarshalForceCompact: %v", err)
	}
	if !compact.IsCompact(buf) {
		t.Fatalf("message is not compact: % x", buf)
	}
	var out T
	if err := Unmarshal(buf, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	return out
}

func TestCompactNestedStruct(t *testing.T) {
	type outer struct {
		ID   int64   `cb:"1"`
		Sub  ccInner `cb:"2"`
		Name string  `cb:"3"`
	}
	for _, in := range []outer{
		{ID: 7, Sub: ccInner{X: -3, Y: "inner", Z: true}, Name: "outer"},
		{ID: 7, Sub: ccInner{}, Name: "outer"},            // zero nested: omitted entirely
		{Sub: ccInner{Y: "only the nested field is set"}}, // everything else omitted
		{},
	} {
		if got := forceRoundTrip(t, in); !reflect.DeepEqual(got, in) {
			t.Errorf("round trip:\n got %+v\nwant %+v", got, in)
		}
	}
}

// A nested struct holding nothing but zeros is not named at all, so it costs the
// containing record nothing -- the same rule a zero scalar gets.
func TestCompactZeroNestedStructIsOmitted(t *testing.T) {
	type outer struct {
		ID  int64   `cb:"1"`
		Sub ccInner `cb:"2"`
	}
	empty, err := MarshalForceCompact(outer{ID: 5})
	if err != nil {
		t.Fatal(err)
	}
	full, err := MarshalForceCompact(outer{ID: 5, Sub: ccInner{X: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) >= len(full) {
		t.Fatalf("a zero nested struct cost %d B against %d B for a set one", len(empty), len(full))
	}
	// And an outer record that is entirely zero is its header plus its own
	// terminator: five bits and four, which spill into a second byte.
	bare, err := MarshalForceCompact(outer{})
	if err != nil {
		t.Fatal(err)
	}
	if len(bare) != 2 {
		t.Fatalf("an all-zero record took %d B: % x", len(bare), bare)
	}
}

func TestCompactArrayOfStructs(t *testing.T) {
	type outer struct {
		ID   int64     `cb:"1"`
		Rows []ccInner `cb:"2"`
	}
	for _, in := range []outer{
		{ID: 1, Rows: []ccInner{{X: 1, Y: "a"}, {X: -2, Y: "b", Z: true}, {}}},
		{ID: 1, Rows: []ccInner{{X: 9}}},
		{ID: 1, Rows: nil}, // omitted, decodes nil
		{},
	} {
		if got := forceRoundTrip(t, in); !reflect.DeepEqual(got, in) {
			t.Errorf("round trip:\n got %+v\nwant %+v", got, in)
		}
	}
}

func TestCompactMaps(t *testing.T) {
	type outer struct {
		S map[string]int32      `cb:"1"`
		I map[int32]string      `cb:"2"`
		V map[string]ccInner    `cb:"3"`
		L map[string][]int32    `cb:"4"`
		F map[float64]bool      `cb:"5"`
		N map[string]ccInner    `cb:"6"`
		D map[int64]([]ccInner) `cb:"7"`
	}
	in := outer{
		S: map[string]int32{"a": 1, "b": -2, "c": 0},
		I: map[int32]string{1: "one", -7: "neg", 0: ""},
		V: map[string]ccInner{"x": {X: 1, Y: "y", Z: true}, "zero": {}},
		L: map[string][]int32{"nums": {1, 2, 3}, "none": nil},
		F: map[float64]bool{1.5: true, -0.25: false},
		N: map[string]ccInner{}, // empty: omitted, decodes nil
		D: map[int64][]ccInner{9: {{X: 1}, {}}},
	}
	got := forceRoundTrip(t, in)
	want := in
	want.N = nil // an empty map is omitted, exactly as an empty slice is
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip:\n got %+v\nwant %+v", got, want)
	}
}

func TestCompactNestedSlices(t *testing.T) {
	type outer struct {
		II [][]int32          `cb:"1"`
		SS [][]string         `cb:"2"`
		BB [][]byte           `cb:"3"`
		MM []map[string]int32 `cb:"4"`
		DD [][][]int32        `cb:"5"`
	}
	in := outer{
		II: [][]int32{{1, 2, 3}, nil, {-4}},
		SS: [][]string{{"a", ""}, {}},
		BB: [][]byte{{1, 2}, {}},
		MM: []map[string]int32{{"k": 1}, nil},
		DD: [][][]int32{{{1}, {2, 3}}, nil},
	}
	got := forceRoundTrip(t, in)
	// An element is positional, so an empty inner slice is written -- as a count
	// of zero -- rather than omitted. What comes back is whatever its reader
	// returns for a count of zero, and the compact readers differ there: Bytes
	// allocates an empty slice, Strs and the integer codecs return nil. Both are
	// pre-existing, and the field-level rule is separate: an empty slice *field*
	// is omitted and always decodes nil.
	want := in
	want.SS = [][]string{{"a", ""}, nil}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip:\n got %+v\nwant %+v", got, want)
	}
}

// Everything at once, and to a depth the flat plan could not have reached.
func TestCompactEverythingNested(t *testing.T) {
	in := ccOuter{
		ID:     -12345,
		Inner:  ccInner{X: 42, Y: "inner", Z: true},
		Rows:   []ccInner{{X: 1}, {Y: "two"}, {Z: true}},
		ByName: map[string]int32{"alpha": 1, "beta": -2},
		ByID:   map[int32]ccInner{7: {X: 7, Y: "seven"}, -1: {}},
		Nested: [][]int32{{1, 2}, {3}},
		Lists:  []map[string]string{{"a": "b"}, {"c": "d"}},
		Blobs:  [][]byte{{0xde, 0xad}, {0xbe, 0xef}},
		Deep:   map[string][]ccInner{"k": {{X: 1, Y: "deep"}, {}}},
		Name:   "outer",
	}
	if got := forceRoundTrip(t, in); !reflect.DeepEqual(got, in) {
		t.Errorf("round trip:\n got %+v\nwant %+v", got, in)
	}
}

// A negative integer anywhere -- including inside a nested struct, an array
// element or a map key, where the pre-scan does not look -- must survive. That is
// what the plan's conservative signedReach buys.
func TestCompactNegativesInsideComposites(t *testing.T) {
	type outer struct {
		Sub  ccInner         `cb:"1"`
		Rows []ccInner       `cb:"2"`
		Keys map[int32]int64 `cb:"3"`
	}
	in := outer{
		Sub:  ccInner{X: -2147483648},
		Rows: []ccInner{{X: -1}, {X: -99999}},
		Keys: map[int32]int64{-5: -9223372036854775808, 5: 5},
	}
	buf, err := MarshalForceCompact(in)
	if err != nil {
		t.Fatal(err)
	}
	r, err := compact.NewReader(buf)
	if err != nil {
		t.Fatal(err)
	}
	if r.AllPositive() {
		t.Error("ALL_POSITIVE set on a message holding negative integers")
	}
	if got := forceRoundTrip(t, in); !reflect.DeepEqual(got, in) {
		t.Errorf("round trip:\n got %+v\nwant %+v", got, in)
	}
}

// A nested key run spends the containing message's key bits, so the narrow key
// needs every reachable struct tagged -- one untagged nested field widens every
// key in the message.
func TestCompactNestedKeyWidth(t *testing.T) {
	type tagged struct {
		A int32 `cb:"1"`
	}
	type untagged struct {
		A int32 // hashed id, anywhere in 0..254
	}
	type narrowOuter struct {
		Sub tagged `cb:"1"`
	}
	type wideOuter struct {
		Sub untagged `cb:"1"`
	}
	for _, tc := range []struct {
		v    any
		want compact.KeyWidth
	}{
		{narrowOuter{}, compact.Keys4},
		{wideOuter{}, compact.Keys8},
	} {
		ti, err := getTypeInfo(reflect.TypeOf(tc.v))
		if err != nil {
			t.Fatalf("%T: %v", tc.v, err)
		}
		if got := compactKeys(ti); got != tc.want {
			t.Errorf("%T: compactKeys = %d, want %d", tc.v, got, tc.want)
		}
	}
	// The narrow one really writes narrow keys, not just plans to.
	buf, err := MarshalForceCompact(narrowOuter{Sub: tagged{A: 3}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := compact.NewReader(buf)
	if err != nil {
		t.Fatal(err)
	}
	if r.Keys() != compact.Keys4 {
		t.Errorf("wrote %d-bit keys, want 4", r.Keys())
	}
}

// A pointer to a nested struct rides the same admission rule a scalar pointer
// does: omit-empty only.
func TestCompactNestedPointer(t *testing.T) {
	type outer struct {
		ID  int64    `cb:"1"`
		Sub *ccInner `cb:"2"`
	}
	if _, err := MarshalForceCompact(outer{ID: 1}); err == nil {
		t.Error("a pointer field was accepted with omit-empty off")
	}
	withOmitEmpty(t, true, func() {
		for _, in := range []outer{
			{ID: 1, Sub: &ccInner{X: 5, Y: "set"}},
			{ID: 1, Sub: nil},
		} {
			if got := forceRoundTrip(t, in); !reflect.DeepEqual(got, in) {
				t.Errorf("round trip:\n got %+v\nwant %+v", got, in)
			}
		}
		// The documented loss: a pointer to the zero value is absent, so it
		// comes back nil.
		got := forceRoundTrip(t, outer{ID: 1, Sub: &ccInner{}})
		if got.Sub != nil {
			t.Errorf("a pointer to the zero value came back as %+v, want nil", got.Sub)
		}
	})
}

// --- MarshalForceCompact -------------------------------------------------------

// The point of the function: a composite type where the encoder measures
// columnar as smaller still gets compact when it is asked for.
func TestForceCompactOverridesTheSizeChoice(t *testing.T) {
	// Enough sub-records that a hundred key runs beat one column per field --
	// which is exactly the case compact mode's premise does not cover.
	rows := make([]ccInner, 200)
	for i := range rows {
		rows[i] = ccInner{X: int32(i), Y: "row", Z: i%2 == 0}
	}
	in := struct {
		ID   int64     `cb:"1"`
		Rows []ccInner `cb:"2"`
	}{ID: 1, Rows: rows}

	auto, err := Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if compact.IsCompact(auto) {
		t.Fatal("Marshal chose compact for a 200-sub-record value; the size rule did not apply")
	}
	forced, err := MarshalForceCompact(in)
	if err != nil {
		t.Fatal(err)
	}
	if !compact.IsCompact(forced) {
		t.Fatal("MarshalForceCompact did not produce a compact message")
	}
	if len(forced) <= len(auto) {
		t.Logf("compact %d B, columnar %d B -- compact happened to win here", len(forced), len(auto))
	}
	// Forced or not, it has to decode.
	var out struct {
		ID   int64     `cb:"1"`
		Rows []ccInner `cb:"2"`
	}
	if err := Unmarshal(forced, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Error("forced compact message did not round trip")
	}
}

// For a flat type the two agree, since Marshal already takes compact there.
func TestForceCompactMatchesMarshalOnFlatTypes(t *testing.T) {
	for _, v := range []any{
		cmUser{ID: 1234, Name: "Usuario1", Age: 22},
		cmNumbered{Quantity: 3, TotalAmount: 900},
		[]cmUser{{ID: 1}, {ID: 2}},
	} {
		auto, err := Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		forced, err := MarshalForceCompact(v)
		if err != nil {
			t.Fatalf("%T: %v", v, err)
		}
		if !reflect.DeepEqual(auto, forced) {
			t.Errorf("%T: Marshal % x, forced % x", v, auto, forced)
		}
	}
}

// It errors rather than falling back, and the error names what is in the way.
func TestForceCompactErrors(t *testing.T) {
	for _, tc := range []struct {
		v    any
		want string
	}{
		{cmAny{ID: 1}, "interface"},
		{cmCyclic{ID: 1}, "reach its own type"},
		{cmPlatformInt{ID: 1}, "platform dependent"},
		{cmPointer{ID: 1}, "SetOmitEmpty"},
		{[]cmUser{{}, {}, {}, {}}, "4 records"},
		{[]cmUser{}, "0 records"},
		{map[string]int32{"a": 1}, "needs a struct or a slice of structs"},
		{int64(7), "needs a struct or a slice of structs"},
		{[]*cmUser{{}}, "needs a struct or a slice of structs"},
		{struct{}{}, "no encodable fields"},
	} {
		out, err := MarshalForceCompact(tc.v)
		if err == nil {
			t.Errorf("%T: no error, got % x", tc.v, out)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%T: error %q does not mention %q", tc.v, err, tc.want)
		}
	}
}

// Records past the shape's two bits, and a lone struct, both report clearly.
func TestForceCompactShapes(t *testing.T) {
	for n := 1; n <= compact.MaxRecords; n++ {
		in := make([]ccInner, n)
		for i := range in {
			in[i] = ccInner{X: int32(i + 1)}
		}
		buf, err := MarshalForceCompact(in)
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		r, err := compact.NewReader(buf)
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if r.Records() != n {
			t.Errorf("n=%d: header says %d records", n, r.Records())
		}
		var out []ccInner
		if err := Unmarshal(buf, &out); err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if !reflect.DeepEqual(out, in) {
			t.Errorf("n=%d: got %+v want %+v", n, out, in)
		}
	}
	if _, err := MarshalForceCompact(make([]ccInner, compact.MaxRecords+1)); err == nil {
		t.Error("four records were accepted")
	}
}

// A Codec and the package functions must write the same bytes for the same
// value, composite types included -- the handle changes what it costs to encode,
// not what comes out.
func TestCodecMatchesMarshalOnComposites(t *testing.T) {
	c := MustCodec[ccOuter]()
	in := ccOuter{
		ID:     3,
		Inner:  ccInner{X: 1, Y: "a"},
		Rows:   []ccInner{{X: 2}},
		ByName: map[string]int32{"k": 1},
	}
	viaCodec, err := c.Marshal(&in)
	if err != nil {
		t.Fatal(err)
	}
	viaMarshal, err := Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	// A single-entry map has one iteration order, so the bytes are comparable.
	if !reflect.DeepEqual(viaCodec, viaMarshal) {
		t.Errorf("codec % x, Marshal % x", viaCodec, viaMarshal)
	}
	var out ccOuter
	if err := c.Unmarshal(viaCodec, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Errorf("codec round trip:\n got %+v\nwant %+v", out, in)
	}
}

// A truncated composite message must error, not panic or return junk.
func TestCompactCompositeTruncation(t *testing.T) {
	in := ccOuter{
		ID:     1,
		Inner:  ccInner{X: 1, Y: "inner"},
		Rows:   []ccInner{{X: 2, Y: "row"}},
		ByName: map[string]int32{"a": 1},
		Deep:   map[string][]ccInner{"k": {{X: 3}}},
	}
	buf, err := MarshalForceCompact(in)
	if err != nil {
		t.Fatal(err)
	}
	// From one byte: an empty buffer panics in the columnar header read, which
	// predates compact mode and is what TestCorruptArrayLengthRejected already
	// notes.
	for cut := 1; cut < len(buf); cut++ {
		var out ccOuter
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("cut at %d panicked: %v", cut, r)
				}
			}()
			_ = Unmarshal(buf[:cut], &out) // an error is fine; a panic is not
		}()
	}
}

// A corrupt count must not be believed into a huge allocation.
func TestCompactCompositeCorruption(t *testing.T) {
	in := ccOuter{
		ID:     1,
		Rows:   []ccInner{{X: 2, Y: "row"}},
		ByName: map[string]int32{"a": 1},
	}
	buf, err := MarshalForceCompact(in)
	if err != nil {
		t.Fatal(err)
	}
	for i := range buf {
		for _, bit := range []byte{0x01, 0x80, 0x7f, 0xff} {
			corrupt := append([]byte(nil), buf...)
			corrupt[i] ^= bit
			var out ccOuter
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("byte %d ^ 0x%02x panicked: %v", i, bit, r)
					}
				}()
				_ = Unmarshal(corrupt, &out)
			}()
		}
	}
}

// The composite plan is a tree, and each struct's subs are its own -- an index
// must never be resolved against another plan's table.
func TestCompactPlanSubsAreLocal(t *testing.T) {
	ti, err := getTypeInfo(reflect.TypeFor[ccOuter]())
	if err != nil {
		t.Fatal(err)
	}
	pl := compactPlanFor(ti)
	if !pl.usable || !pl.hasComposite {
		t.Fatalf("usable %v composite %v", pl.usable, pl.hasComposite)
	}
	for i := range pl.ops {
		op := &pl.ops[i]
		if int(op.sub) > len(pl.subs) {
			t.Fatalf("op %d: sub %d past %d subs", i, op.sub, len(pl.subs))
		}
		switch op.kind {
		case opStruct, opArray, opMap:
			if op.sub == 0 {
				t.Errorf("op %d: composite kind %d with no sub", i, op.kind)
			}
		default:
			if op.sub != 0 {
				t.Errorf("op %d: scalar kind %d carries sub %d", i, op.kind, op.sub)
			}
		}
	}
	// The nested struct's own plan holds its own subs.
	if got := unsafe_Sizeof_compactOp(); got != 16 {
		t.Errorf("compactOp is %d bytes; the sub index was supposed to ride in the padding", got)
	}
}

func unsafe_Sizeof_compactOp() uintptr { return reflect.TypeFor[compactOp]().Size() }

// Skip cannot step over a composite, and says so rather than desynchronising the
// bitstream.
func TestCompactSkipRefusesComposites(t *testing.T) {
	buf, err := MarshalForceCompact(ccInner{X: 1, Y: "a"})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []compact.Kind{compact.KindStruct, compact.KindArray, compact.KindMap} {
		r, err := compact.NewReader(buf)
		if err != nil {
			t.Fatal(err)
		}
		r.Skip(k)
		if r.Err() == nil {
			t.Errorf("Skip(%v) reported no error", k)
		}
	}
}

func TestCompactCompositeSizes(t *testing.T) {
	in := ccOuter{
		ID:     -12345,
		Inner:  ccInner{X: 42, Y: "inner", Z: true},
		Rows:   []ccInner{{X: 1}, {Y: "two"}},
		ByName: map[string]int32{"alpha": 1},
		Name:   "outer",
	}
	forced, err := MarshalForceCompact(in)
	if err != nil {
		t.Fatal(err)
	}
	auto, err := Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("nested+array+map record: compact %d B, chosen %d B (%v)\n",
		len(forced), len(auto), map[bool]string{true: "compact", false: "columnar"}[compact.IsCompact(auto)])
}
