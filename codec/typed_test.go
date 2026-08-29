package codec

import (
	"bytes"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/ivanjoz/colbin/compact"
)

// cmWideNumbered is cmWide with explicit small ids: every value form compact
// mode supports, carried by a type that also qualifies for the narrow key.
type cmWideNumbered struct {
	ID      int64     `cb:"1"`
	Active  bool      `cb:"2"`
	Ratio   float32   `cb:"3"`
	Amount  float64   `cb:"4"`
	Name    string    `cb:"5"`
	Blob    []byte    `cb:"6"`
	IDs     []int32   `cb:"7"`
	Tags    []string  `cb:"8"`
	Flags   []bool    `cb:"9"`
	Weights []float64 `cb:"10"`
	Small   []int8    `cb:"11"`
	Big     []uint64  `cb:"12"`
}

// A Codec is a faster door onto the same format, so its output must be the bytes
// the package functions already produce -- for a compact-eligible type and for
// one that has to stay columnar.
func TestCodecMatchesPackageFunctions(t *testing.T) {
	num := cmNumbered{480, 120, 12, 3, 24, 145900, 32000}
	numCodec := MustCodec[cmNumbered]()
	wide := cmNested{ID: 7, Name: "x"} // a nested struct: never compact
	wideCodec := MustCodec[cmNested]()

	for _, tc := range []struct {
		label string
		got   []byte
		want  []byte
	}{
		{"compact struct", mustAppend(t, numCodec, nil, &num), mustMarshal(t, num)},
		{"columnar struct", mustAppend(t, wideCodec, nil, &wide), mustMarshal(t, wide)},
		{"slice of 1", mustMarshalSlice(t, numCodec, []cmNumbered{num}), mustMarshal(t, []cmNumbered{num})},
		{"slice of 3", mustMarshalSlice(t, numCodec, []cmNumbered{num, num, num}), mustMarshal(t, []cmNumbered{num, num, num})},
		{"slice of 8", mustMarshalSlice(t, numCodec, make([]cmNumbered, 8)), mustMarshal(t, make([]cmNumbered, 8))},
	} {
		if !bytes.Equal(tc.got, tc.want) {
			t.Errorf("%s: codec wrote %d B, Marshal wrote %d B\n got %x\nwant %x",
				tc.label, len(tc.got), len(tc.want), tc.got, tc.want)
		}
	}
}

// Either side must read what the other wrote, in both modes.
func TestCodecInteropsWithPackageFunctions(t *testing.T) {
	in := cmNumbered{480, 0, 12, -3, 24, 145900, 32000}
	c := MustCodec[cmNumbered]()

	viaCodec := mustAppend(t, c, nil, &in)
	var out cmNumbered
	if err := Unmarshal(viaCodec, &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("Unmarshal of a codec message: got %#v, want %#v", out, in)
	}

	out = cmNumbered{}
	if err := c.Unmarshal(mustMarshal(t, in), &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("codec read of a Marshal message: got %#v, want %#v", out, in)
	}
}

// Append onto a buffer that already holds bytes must leave them alone, since
// that is the whole point of handing the same buffer back every time.
func TestCodecAppendPreservesPrefix(t *testing.T) {
	c := MustCodec[cmNumbered]()
	v := cmNumbered{Quantity: 5}
	head := []byte("HEADER")

	buf, err := c.Append(append([]byte(nil), head...), &v)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(buf, head) {
		t.Fatalf("prefix lost: %q", buf)
	}
	var out cmNumbered
	if err := c.Unmarshal(buf[len(head):], &out); err != nil {
		t.Fatal(err)
	}
	if out != v {
		t.Fatalf("got %#v, want %#v", out, v)
	}
}

// Reusing one buffer across many records must produce, each time, exactly what a
// fresh buffer would: the pooled writer and the size hint must not leak state
// from the record before.
func TestCodecReusedBufferMatchesFresh(t *testing.T) {
	c := MustCodec[cmWideNumbered]()
	vals := []cmWideNumbered{
		{ID: 1, Name: "Usuario1", Ratio: 1.5, IDs: []int32{1, 2, 3}},
		{},
		{ID: -9, Blob: []byte{0, 1, 2}, Tags: []string{"a", "bb"}, Flags: []bool{true, false}},
		{ID: 1 << 40, Name: strings.Repeat("long ", 40)},
	}
	buf := make([]byte, 0, 8)
	for i := range vals {
		var err error
		buf, err = c.Append(buf[:0], &vals[i])
		if err != nil {
			t.Fatal(err)
		}
		fresh, err := c.Marshal(&vals[i])
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(buf, fresh) {
			t.Fatalf("record %d: reused buffer %x, fresh %x", i, buf, fresh)
		}
		var out cmWideNumbered
		if err := c.Unmarshal(buf, &out); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(out, vals[i]) {
			t.Fatalf("record %d round trip: got %#v, want %#v", i, out, vals[i])
		}
	}
}

// A decode must not merge into whatever the destination held: compact mode omits
// zero-valued fields, so a stale value would survive under a field the message
// does not carry.
func TestCodecUnmarshalOverwrites(t *testing.T) {
	c := MustCodec[cmNumbered]()
	in := cmNumbered{Quantity: 7}
	buf := mustAppend(t, c, nil, &in)

	out := cmNumbered{Quantity: 999, SubDivisor: 24, TotalAmount: 5}
	if err := c.Unmarshal(buf, &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("stale values survived: got %#v, want %#v", out, in)
	}
}

func TestCodecSliceRoundTrip(t *testing.T) {
	c := MustCodec[cmNumbered]()
	base := cmNumbered{480, 120, 12, 3, 24, 145900, 32000}
	for _, n := range []int{0, 1, 2, 3, 4, 17} {
		in := make([]cmNumbered, n)
		for i := range in {
			in[i] = base
			in[i].Quantity = int32(i + 1)
		}
		buf, err := c.MarshalSlice(in)
		if err != nil {
			t.Fatal(err)
		}
		var out []cmNumbered
		if err := c.UnmarshalSlice(buf, &out); err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if n == 0 {
			if len(out) != 0 {
				t.Fatalf("n=0: got %#v", out)
			}
			continue
		}
		if !reflect.DeepEqual(out, in) {
			t.Fatalf("n=%d: got %#v, want %#v", n, out, in)
		}
	}
}

// The type facts a caller can ask the handle for, which are fixed at
// construction and are the reason the handle exists.
func TestCodecReportsTypeFacts(t *testing.T) {
	if c := MustCodec[cmNumbered](); !c.Compact() || c.Keys() != compact.Keys4 {
		t.Errorf("cmNumbered: compact %v keys %d, want true/%d", c.Compact(), c.Keys(), compact.Keys4)
	}
	if c := MustCodec[cmUser](); !c.Compact() || c.Keys() != compact.Keys8 {
		t.Errorf("cmUser: compact %v keys %d, want true/%d", c.Compact(), c.Keys(), compact.Keys8)
	}
	if c := MustCodec[cmNested](); c.Compact() {
		t.Error("cmNested: compact true, want false")
	}
}

func TestCodecRejectsNonStruct(t *testing.T) {
	if _, err := NewCodec[[]cmUser](); err == nil {
		t.Error("NewCodec[[]cmUser] returned no error")
	}
	if _, err := NewCodec[int](); err == nil {
		t.Error("NewCodec[int] returned no error")
	}
	c := MustCodec[cmNumbered]()
	if _, err := c.Append(nil, nil); err == nil {
		t.Error("Append(nil) returned no error")
	}
	if err := c.Unmarshal([]byte{1}, nil); err == nil {
		t.Error("Unmarshal(nil) returned no error")
	}
	if err := c.Unmarshal(mustMarshal(t, []cmNumbered{{}, {}}), new(cmNumbered)); err == nil {
		t.Error("two records into one struct returned no error")
	}
}

// The handle is read-only after construction and its writers and readers come
// from pools, so concurrent use must be safe -- this test is here for the race
// detector to watch.
func TestCodecConcurrent(t *testing.T) {
	c := MustCodec[cmNumbered]()
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			in := cmNumbered{Quantity: int32(g + 1), TotalAmount: int32(g * 1000)}
			buf := make([]byte, 0, 32)
			for range 200 {
				var err error
				if buf, err = c.Append(buf[:0], &in); err != nil {
					t.Error(err)
					return
				}
				var out cmNumbered
				if err := c.Unmarshal(buf, &out); err != nil {
					t.Error(err)
					return
				}
				if out != in {
					t.Errorf("got %#v, want %#v", out, in)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustAppend[T any](t *testing.T, c *Codec[T], dst []byte, v *T) []byte {
	t.Helper()
	b, err := c.Append(dst, v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustMarshalSlice[T any](t *testing.T, c *Codec[T], vs []T) []byte {
	t.Helper()
	b, err := c.MarshalSlice(vs)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
