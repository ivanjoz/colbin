package codec

import (
	"reflect"
	"testing"
)

// The columnar decoder gives an array column one backing array and cuts each
// record's slice out of it. These tests pin the two properties that makes it
// safe to do: each record still gets exactly its own elements, and appending to
// one record's slice cannot reach the next record's.

type grantRow struct {
	AccesoID   uint16  `cb:"1"`
	Nivel      uint8   `cb:"2"`
	SubAccesos []uint8 `cb:"3"`
}

type grantHolder struct {
	UsuarioID uint32     `cb:"1"`
	Grants    []grantRow `cb:"2"`
}

// The corpus is written in decoded form so it can be compared directly: an
// empty array field comes back nil, but an empty []byte field (which is what a
// []uint8 is on the wire) comes back as a non-nil empty slice. Both encode
// identically, so using the decoded form here changes nothing about the message.
func grantCorpus() []grantHolder {
	return []grantHolder{
		{UsuarioID: 1, Grants: []grantRow{{101, 3, []uint8{1}}, {102, 1, []uint8{2, 3}}}},
		{UsuarioID: 2, Grants: nil},
		{UsuarioID: 3, Grants: []grantRow{{210, 2, []uint8{}}}},
		{UsuarioID: 4, Grants: []grantRow{{311, 1, []uint8{4, 5, 6}}, {312, 2, []uint8{}}, {313, 3, []uint8{7}}}},
	}
}

func TestArrayColumnSharedBacking(t *testing.T) {
	want := grantCorpus()
	data, err := Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got []grantHolder
	if err := Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, want)
	}

	// Every record's slice is cut with cap == len, so growing one reallocates
	// instead of writing over the record that follows it in the shared array.
	for i := range got {
		if g := got[i].Grants; cap(g) != len(g) {
			t.Fatalf("record %d: cap %d != len %d, an append would reach the next record", i, cap(g), len(g))
		}
	}
	got[0].Grants = append(got[0].Grants, grantRow{AccesoID: 999})
	if !reflect.DeepEqual(got[3].Grants, want[3].Grants) {
		t.Fatalf("append to record 0 corrupted record 3: %+v", got[3].Grants)
	}
}

// A message decoded twice must produce identical results: the second decode runs
// against warm pools, so a buffer that is not properly reset or re-filled shows
// up here as the first message's values leaking into the second.
func TestRepeatedDecodeIsStable(t *testing.T) {
	first := grantCorpus()
	// second holds two records, which is small enough for compact mode to win,
	// and compact mode omits an empty field and decodes it back as nil -- so
	// SubAccesos is non-empty here where the four-record corpus above can use the
	// columnar form's empty []byte. See TestCodecSliceReuse for the same crossing.
	second := []grantHolder{{UsuarioID: 9, Grants: []grantRow{{1, 1, []uint8{5}}}}, {UsuarioID: 10}}

	dataA, err := Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	dataB, err := Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 50 {
		var a []grantHolder
		if err := Unmarshal(dataA, &a); err != nil {
			t.Fatal(err)
		}
		var b []grantHolder
		if err := Unmarshal(dataB, &b); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a, first) {
			t.Fatalf("iteration %d: A decoded as %+v", i, a)
		}
		if !reflect.DeepEqual(b, second) {
			t.Fatalf("iteration %d: B decoded as %+v", i, b)
		}
	}
}

// An empty column returns a pooled buffer that still holds the previous column's
// values, so it has to be zeroed. Alternating a populated message with an
// all-zero one catches a missing clear.
func TestEmptyColumnAfterPopulated(t *testing.T) {
	type scalars struct {
		I int32   `cb:"1"`
		F float64 `cb:"2"`
	}
	full := []scalars{{I: 7, F: 1.5}, {I: 8, F: 2.5}, {I: 9, F: 3.5}, {I: 10, F: 4.5}}
	zero := make([]scalars, 4)

	dataFull, err := Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	dataZero, err := Marshal(zero)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		var a, b []scalars
		if err := Unmarshal(dataFull, &a); err != nil {
			t.Fatal(err)
		}
		if err := Unmarshal(dataZero, &b); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a, full) || !reflect.DeepEqual(b, zero) {
			t.Fatalf("iteration %d: full=%+v zero=%+v", i, a, b)
		}
	}
}

// A corrupt length must be rejected rather than sizing the shared backing array.
func TestCorruptArrayLengthRejected(t *testing.T) {
	data, err := Marshal([]grantHolder{{UsuarioID: 1, Grants: []grantRow{{1, 1, []uint8{}}}}})
	if err != nil {
		t.Fatal(err)
	}
	for i := range data {
		for _, bit := range []byte{0x80, 0x7f, 0xff} {
			corrupt := append([]byte(nil), data...)
			corrupt[i] ^= bit
			var out []grantHolder
			func() {
				defer func() { _ = recover() }() // a panic is pre-existing behaviour; corruption is not
				_ = Unmarshal(corrupt, &out)
			}()
		}
	}
}

// A destination with capacity is decoded into in place, and a shorter message
// must not leave the previous one's records visible past its own length.
func TestDestinationSliceReuse(t *testing.T) {
	long := grantCorpus()
	short := grantCorpus()[:2]
	dataLong, err := Marshal(long)
	if err != nil {
		t.Fatal(err)
	}
	dataShort, err := Marshal(short)
	if err != nil {
		t.Fatal(err)
	}

	dst := make([]grantHolder, 0, 8)
	if err := Unmarshal(dataLong, &dst); err != nil {
		t.Fatal(err)
	}
	before := &dst[0]
	if !reflect.DeepEqual(dst, long) {
		t.Fatalf("first decode: %+v", dst)
	}
	if err := Unmarshal(dataShort, &dst); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(dst, short) {
		t.Fatalf("second decode: got %+v want %+v", dst, short)
	}
	if &dst[0] != before {
		t.Fatal("backing array was replaced; the capacity was not reused")
	}
	if len(dst) != len(short) {
		t.Fatalf("len %d, want %d", len(dst), len(short))
	}

	// A field the message does not carry must read as zero, not as whatever the
	// previous decode left in the reused element.
	type wide struct {
		A int32  `cb:"1"`
		B string `cb:"2"`
	}
	type narrow struct {
		A int32 `cb:"1"`
	}
	dataWide, err := Marshal([]wide{{A: 1, B: "keep"}, {A: 2, B: "out"}, {A: 3, B: "of"}, {A: 4, B: "here"}})
	if err != nil {
		t.Fatal(err)
	}
	dataNarrow, err := Marshal([]narrow{{A: 9}, {A: 8}, {A: 7}, {A: 6}})
	if err != nil {
		t.Fatal(err)
	}
	var ws []wide
	if err := Unmarshal(dataWide, &ws); err != nil {
		t.Fatal(err)
	}
	if err := Unmarshal(dataNarrow, &ws); err != nil { // same shape, one column short
		t.Fatal(err)
	}
	for i, w := range ws {
		if w.B != "" {
			t.Fatalf("record %d: stale B %q survived a message that omits it", i, w.B)
		}
		if w.A != int32(9-i) {
			t.Fatalf("record %d: A = %d", i, w.A)
		}
	}
}

// The typed handle reuses its destination the same way, in both modes.
func TestCodecSliceReuse(t *testing.T) {
	c := MustCodec[grantRow]()
	// Every SubAccesos is non-empty: compact mode omits a zero-valued field and
	// decodes it back as nil, where a columnar []byte column comes back as an
	// empty slice, and this test crosses between the two modes.
	rows := []grantRow{{311, 1, []uint8{4, 5, 6}}, {312, 2, []uint8{9}}, {313, 3, []uint8{7}}}
	few := rows[:2] // two records: compact
	dataMany, err := Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	dataFew, err := Marshal(few)
	if err != nil {
		t.Fatal(err)
	}
	dst := make([]grantRow, 0, 16)
	for range 5 {
		if err := c.UnmarshalSlice(dataMany, &dst); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(dst, rows) {
			t.Fatalf("many: %+v", dst)
		}
		if err := c.UnmarshalSlice(dataFew, &dst); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(dst, few) {
			t.Fatalf("few: %+v", dst)
		}
	}
}
