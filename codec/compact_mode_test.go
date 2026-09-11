package codec

import (
	"fmt"
	"math"
	"reflect"
	"testing"
	"unsafe"

	"github.com/ivanjoz/colbin/compact"
)

type cmUser struct {
	ID      int64  `cb:"id"`
	Name    string `cb:"name"`
	Age     int32  `cb:"age"`
	Updated int64  `cb:"updated"`
	GroupID int32  `cb:"groupID"`
}

// cmNumbered is the shape narrow keys are for: every field carries an explicit
// small id, so the whole type fits four-bit keys. cmUser, whose tags name fields
// rather than number them, gets hashed ids and stays on the wide key.
type cmNumbered struct {
	Quantity                int32 `cb:"1,quantity"`
	QuantityPendingDelivery int32 `cb:"2,quantityPendingDelivery"`
	SubQuantity             int16 `cb:"3,subQuantity"`
	SubQuantityPending      int16 `cb:"4,subQuantityPending"`
	SubDivisor              int16 `cb:"5,subDivisor"`
	TotalAmount             int32 `cb:"6,totalAmount"`
	TotalDebtAmount         int32 `cb:"7,totalDebtAmount"`
}

type cmWide struct {
	ID      int64
	Active  bool
	Ratio   float32
	Amount  float64
	Name    string
	Blob    []byte
	IDs     []int32
	Tags    []string
	Flags   []bool
	Weights []float64
	Small   []int8
	Big     []uint64
}

// cmNested and cmArrayOfStructs are composites: compact mode carries them, as a
// nested key run and as a counted run of them, but the encoder decides on size
// rather than taking compact outright the way it does for a flat struct.
type cmNested struct {
	ID   int64
	Sub  struct{ X int32 }
	Name string
}

type cmArrayOfStructs struct {
	ID   int64
	Rows []cmUser
}

type cmPointer struct {
	ID  int64
	Opt *int32 // nullable: compact only under omit-empty
}

type cmPlatformInt struct {
	ID   int64
	Nums []int // platform-width elements: never compact
}

// cmAny is the one exclusion the format cannot lift: an interface's concrete
// type is a property of the value, and the compact wire has no tag for it.
type cmAny struct {
	ID      int64
	Payload any
}

// cmCyclic can reach itself, and a compact plan is a tree of sub-plans.
type cmCyclic struct {
	ID   int64
	Kids []cmCyclic
}

// --- eligibility ---------------------------------------------------------------

func TestCompactEligibility(t *testing.T) {
	for _, tc := range []struct {
		v    any
		want bool
	}{
		{cmUser{}, true},
		{cmWide{}, true},
		{cmNumbered{}, true},
		{cmNested{}, true},         // nested key run
		{cmArrayOfStructs{}, true}, // counted run of key runs
		{cmPointer{}, false},       // omit-empty is off here
		{cmPlatformInt{}, false},
		{cmAny{}, false},
		{cmCyclic{}, false},
	} {
		ti, err := getTypeInfo(reflect.TypeOf(tc.v))
		if err != nil {
			t.Fatalf("%T: %v", tc.v, err)
		}
		if got := compactUsable(ti); got != tc.want {
			t.Errorf("%T: compactUsable = %v, want %v", tc.v, got, tc.want)
		}
		// The answer is memoised; asking twice must not change it.
		if got := compactUsable(ti); got != tc.want {
			t.Errorf("%T: memoised answer flipped to %v", tc.v, got)
		}
	}
}

// A type whose ids all fit under the narrow terminator takes the 4-bit key; one
// hashed id anywhere puts the type back on the 8-bit one.
func TestCompactKeyWidth(t *testing.T) {
	for _, tc := range []struct {
		v    any
		want compact.KeyWidth
	}{
		{cmNumbered{}, compact.Keys4},
		{cmUser{}, compact.Keys8},
		{cmWide{}, compact.Keys8},
	} {
		ti, err := getTypeInfo(reflect.TypeOf(tc.v))
		if err != nil {
			t.Fatalf("%T: %v", tc.v, err)
		}
		compactUsable(ti) // fills the memo compactKeys reads
		if got := compactKeys(ti); got != tc.want {
			t.Errorf("%T: compactKeys = %d, want %d", tc.v, got, tc.want)
		}
	}
}

// The header must say which width it used, and the record must survive it.
func TestNarrowKeysRoundTripAndSize(t *testing.T) {
	v := cmNumbered{
		Quantity: 480, QuantityPendingDelivery: 120, SubQuantity: 12,
		SubQuantityPending: 3, SubDivisor: 24, TotalAmount: 145900,
		TotalDebtAmount: 32000,
	}
	buf, err := Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if !compact.IsCompact(buf) {
		t.Fatal("not compact")
	}
	r, err := compact.NewReader(buf)
	if err != nil {
		t.Fatal(err)
	}
	if r.Keys() != compact.Keys4 {
		t.Fatalf("header says keys %d, want %d", r.Keys(), compact.Keys4)
	}

	var out cmNumbered
	if err := Unmarshal(buf, &out); err != nil {
		t.Fatal(err)
	}
	if out != v {
		t.Fatalf("round trip: got %#v, want %#v", out, v)
	}

	// Same values, same codecs, ids the narrow key cannot hold: the difference is
	// the key run alone, 4 bits on each of the seven fields and the terminator,
	// less the one bit the header flag costs.
	std, _ := marshalStandard(v)
	wide := appendCompactKeys(t, v, compact.Keys8)
	fmt.Printf("7-field numbered struct: narrow %d B, wide keys %d B, standard %d B\n",
		len(buf), len(wide), len(std))
	if len(buf) >= len(wide) {
		t.Fatalf("narrow %d B is not smaller than wide %d B", len(buf), len(wide))
	}
}

// appendCompactKeys re-encodes v at a forced key width, which is what the size
// comparison above needs and no production path wants.
func appendCompactKeys(t *testing.T, v any, keys compact.KeyWidth) []byte {
	t.Helper()
	rv := reflect.ValueOf(v)
	ti, err := getTypeInfo(rv.Type())
	if err != nil {
		t.Fatal(err)
	}
	p := reflect.New(rv.Type())
	p.Elem().Set(rv)
	ptrs := []unsafe.Pointer{p.UnsafePointer()}

	pl := compactPlanFor(ti)
	w := compact.NewWriter(nil, compact.ShapeStruct, compactAllPositivePlan(pl, ptrs), keys)
	compactWriteRecord(w, pl, ptrs[0])
	return w.Done()
}

// --- mode selection ------------------------------------------------------------

func TestMarshalPicksCompactAtOneRecord(t *testing.T) {
	u := cmUser{1234, "Usuario1", 22, 1234512345, 3333}
	for _, v := range []any{u, []cmUser{u}} {
		buf, err := Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if !compact.IsCompact(buf) {
			t.Fatalf("%T: not compact", v)
		}
		std, _ := marshalStandard(v)
		if len(buf) >= len(std) {
			t.Fatalf("%T: compact %d B is not smaller than standard %d B", v, len(buf), len(std))
		}
		fmt.Printf("%-14T compact %d B, standard %d B\n", v, len(buf), len(std))
	}
}

// Ineligible types must stay columnar and keep round-tripping.
func TestMarshalStaysStandardWhenIneligible(t *testing.T) {
	vals := []any{
		cmPointer{ID: 1},
		cmPlatformInt{ID: 1, Nums: []int{1, 2, 3}},
		cmAny{ID: 1, Payload: "x"},
		cmCyclic{ID: 1},
		map[string]int32{"a": 1}, // value mode
		[]cmUser{},               // zero records
		[]cmUser{{}, {}, {}, {}}, // four records, past MaxRecords
	}
	for _, v := range vals {
		buf, err := Marshal(v)
		if err != nil {
			t.Fatalf("%T: %v", v, err)
		}
		if compact.IsCompact(buf) {
			t.Fatalf("%T: chose compact mode", v)
		}
	}
}

// MarshalJSON must never produce a compact message: it has no room for a schema.
func TestMarshalJSONStaysStandard(t *testing.T) {
	u := cmUser{1234, "Usuario1", 22, 1234512345, 3333}
	buf, err := MarshalJSON(u)
	if err != nil {
		t.Fatal(err)
	}
	if compact.IsCompact(buf) {
		t.Fatal("MarshalJSON produced a compact message")
	}
	if buf[0] != jsonFormatVersion {
		t.Fatalf("version byte 0x%02x, want 0x%02x", buf[0], jsonFormatVersion)
	}
	got, err := DecodeJSON(buf)
	if err != nil {
		t.Fatal(err)
	}
	// Names come from the cb tags; DecodeJSON emits keys in sorted order.
	const want = `{"age":22,"groupID":3333,"id":1234,"name":"Usuario1","updated":1234512345}`
	if string(got) != want {
		t.Fatalf("DecodeJSON = %s, want %s", got, want)
	}
}

// Both standard version bytes must stay even, or a message would be mistaken for
// a compact one.
func TestStandardVersionBytesAreEven(t *testing.T) {
	for _, v := range []byte{formatVersion, jsonFormatVersion} {
		if v&1 != 0 {
			t.Fatalf("version byte 0x%02x is odd; bit 0 is the compact discriminator", v)
		}
	}
}

// --- round trips ---------------------------------------------------------------

func TestCompactRoundTrip(t *testing.T) {
	users := []cmUser{
		{1234, "Usuario1", 22, 1234512345, 3333},
		{1235, "Pedro Gomez", 41, 1234598765, 3333},
		{9871, "Ana", 0, 1236000001, 0},
	}
	for n := 1; n <= 3; n++ {
		in := users[:n]
		buf, err := Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		var out []cmUser
		if err := Unmarshal(buf, &out); err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if !reflect.DeepEqual(out, in) {
			t.Fatalf("n=%d: got %#v, want %#v", n, out, in)
		}
	}
	// A lone struct target.
	buf, _ := Marshal(users[0])
	var one cmUser
	if err := Unmarshal(buf, &one); err != nil {
		t.Fatal(err)
	}
	if one != users[0] {
		t.Fatalf("got %#v, want %#v", one, users[0])
	}
}

func TestCompactRoundTripAllKinds(t *testing.T) {
	in := cmWide{
		ID:      -1, // forces ALL_POSITIVE clear
		Active:  true,
		Ratio:   -2.5,
		Amount:  math.Pi,
		Name:    "el niño comió jamón",
		Blob:    []byte{0, 1, 2, 255},
		IDs:     []int32{101, 102, 103, 104},
		Tags:    []string{"alpha", "", "beta"},
		Flags:   []bool{true, false, true},
		Weights: []float64{1.5, -0.25},
		Small:   []int8{-128, 0, 127},
		Big:     []uint64{0, math.MaxUint64},
	}
	buf, err := Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !compact.IsCompact(buf) {
		t.Fatal("expected compact mode")
	}
	var out cmWide
	if err := Unmarshal(buf, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round trip lost data:\n got %#v\nwant %#v", out, in)
	}
}

// A zero-valued field is omitted, so it must come back zero rather than stale.
func TestCompactOmittedFieldsDecodeZero(t *testing.T) {
	in := cmUser{Updated: 1236000001, Name: "Ana"}
	buf, _ := Marshal(in)

	out := cmUser{ID: 999, Age: 88, GroupID: 77} // pre-populated destination
	if err := Unmarshal(buf, &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("stale values survived: got %#v, want %#v", out, in)
	}
	fmt.Printf("zero-heavy record: %d B\n", len(buf))
}

// Negative zero must survive, since it compares equal to zero but is not it.
func TestCompactNegativeZeroSurvives(t *testing.T) {
	in := cmWide{Amount: math.Copysign(0, -1), Ratio: float32(math.Copysign(0, -1))}
	buf, _ := Marshal(in)
	var out cmWide
	if err := Unmarshal(buf, &out); err != nil {
		t.Fatal(err)
	}
	if !math.Signbit(out.Amount) || !math.Signbit(float64(out.Ratio)) {
		t.Fatalf("negative zero lost: %v %v", out.Amount, out.Ratio)
	}
}

// An array field must cost what it costs in the columnar mode, so a long array
// does not push the message into a worse encoding.
func TestCompactLongArrayUsesColumnarCodec(t *testing.T) {
	type row struct {
		ID  int64
		IDs []int32
	}
	ids := make([]int32, 1000)
	cur := int32(100000)
	for i := range ids {
		cur += int32(i%7) + 1
		ids[i] = cur
	}
	in := row{ID: 5, IDs: ids}
	buf, err := Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	std, _ := marshalStandard(in)
	if len(buf) > len(std) {
		t.Fatalf("compact %d B is worse than standard %d B on a 1000-element array", len(buf), len(std))
	}
	var out row
	if err := Unmarshal(buf, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatal("array round trip lost data")
	}
	fmt.Printf("1000 sorted int32 in a struct: compact %d B, standard %d B\n", len(buf), len(std))
}

// A compact message aimed at the wrong type must error rather than mis-decode.
func TestCompactSchemaMismatch(t *testing.T) {
	buf, _ := Marshal(cmUser{ID: 1, Name: "x"})
	type other struct {
		Totally string `cb:"200"`
	}
	var out other
	if err := Unmarshal(buf, &out); err == nil {
		t.Fatal("expected a schema-mismatch error")
	}
}

func TestCompactTruncatedInput(t *testing.T) {
	full, _ := Marshal(cmUser{1234, "Usuario1", 22, 1234512345, 3333})
	for cut := 1; cut < len(full); cut++ {
		var out cmUser
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("cut at %d panicked: %v", cut, p)
				}
			}()
			_ = Unmarshal(full[:cut], &out)
		}()
	}
}
