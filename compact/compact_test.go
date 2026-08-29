package compact

import (
	"fmt"
	"math"
	"math/bits"
	"math/rand/v2"
	"strings"
	"testing"
)

// --- varint ------------------------------------------------------------------

// The two ladders must agree with the table in the package doc, since that table
// is the whole justification for spending unit 0's seventh bit on a selector.
func TestVarintSizeTable(t *testing.T) {
	want := map[int]int{
		0: 8, 6: 8, 7: 12, 8: 12, 10: 12, 11: 16, 13: 16,
		14: 20, 17: 20, 18: 24, 20: 24, 21: 28, 24: 28, 25: 32,
		28: 36, 29: 36, 31: 36, 32: 40, 64: 76,
	}
	for b, w := range want {
		if got, _ := varintSize(b); got != w {
			t.Errorf("varintSize(%d) = %d, want %d", b, got, w)
		}
	}
}

// Whatever varintSize promises is what putVarint must actually spend.
func TestVarintSizeMatchesEncoding(t *testing.T) {
	for b := 0; b <= 64; b++ {
		var v uint64 = math.MaxUint64
		if b < 64 {
			v = 1<<uint(b) - 1
		}
		if b == 0 {
			v = 0
		}
		w := &bitWriter{}
		w.putVarint(v)
		want, _ := varintSize(b)
		if got := w.bits(); got != want {
			t.Fatalf("b=%d: encoded %d bits, varintSize says %d", b, got, want)
		}
	}
}

func TestVarintRoundTrip(t *testing.T) {
	vals := []uint64{0, 1, 63, 64, 127, 128, 1023, 1234, 3333, 65535,
		1 << 20, 1234512345, 5000000000, 1700000000000, 1<<63 - 1, math.MaxUint64}
	rng := rand.New(rand.NewPCG(1, 2))
	for range 20000 {
		vals = append(vals, rng.Uint64()>>uint(rng.IntN(64)))
	}
	for _, v := range vals {
		w := &bitWriter{}
		w.putVarint(v)
		r := newBitReader(w.flush())
		if got := r.getVarint(); got != v {
			t.Fatalf("varint %d -> %d", v, got)
		}
		if r.err != nil {
			t.Fatalf("varint %d: %v", v, r.err)
		}
	}
}

// A run of values must decode back in order with no framing between them; that
// is what lets a record be a bare sequence of fields.
func TestVarintConcatenates(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	vals := make([]uint64, 500)
	w := &bitWriter{}
	for i := range vals {
		vals[i] = rng.Uint64() >> uint(rng.IntN(64))
		w.putVarint(vals[i])
	}
	r := newBitReader(w.flush())
	for i, want := range vals {
		if got := r.getVarint(); got != want {
			t.Fatalf("value %d: %d, want %d", i, got, want)
		}
	}
	if r.err != nil {
		t.Fatal(r.err)
	}
}

// The whole point of the scheme: it must beat LEB128 over a realistic mix, and
// lose only where the doc says it does.
func TestVarintBeatsLEB128(t *testing.T) {
	leb := func(b int) int {
		if b == 0 {
			return 8
		}
		return 8 * ((b + 6) / 7)
	}
	var wins, losses, ties int
	for b := 0; b <= 64; b++ {
		got, _ := varintSize(b)
		switch {
		case got < leb(b):
			wins++
		case got > leb(b):
			losses++
			if b%7 != 0 || b == 0 {
				t.Errorf("b=%d loses (%d vs %d) but is not an LEB128 boundary", b, got, leb(b))
			}
		default:
			ties++
		}
	}
	if wins <= losses {
		t.Fatalf("wins=%d losses=%d ties=%d: scheme does not pay", wins, losses, ties)
	}
	t.Logf("over bit-lengths 0..64: %d wins, %d losses, %d ties", wins, losses, ties)
}

// --- bitstream ---------------------------------------------------------------

func TestBitstreamRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 6))
	type item struct {
		v uint64
		w uint8
	}
	items := make([]item, 2000)
	w := &bitWriter{}
	for i := range items {
		width := uint8(rng.IntN(65))
		var v uint64
		if width > 0 {
			v = rng.Uint64() >> (64 - width)
		}
		items[i] = item{v, width}
		w.put(v, width)
	}
	r := newBitReader(w.flush())
	for i, it := range items {
		if got := r.get(it.w); got != it.v {
			t.Fatalf("item %d width %d: %d, want %d", i, it.w, got, it.v)
		}
	}
	if r.err != nil {
		t.Fatal(r.err)
	}
}

// Reading past the end must set the error and stay set, never panic.
func TestBitReaderTruncation(t *testing.T) {
	r := newBitReader([]byte{0xFF, 0xFF})
	r.get(16)
	if r.err != nil {
		t.Fatal("exact read should succeed")
	}
	if got := r.get(1); got != 0 || r.err != ErrTruncated {
		t.Fatalf("overrun: got %d err %v", got, r.err)
	}
	if got := r.get(8); got != 0 || r.err != ErrTruncated {
		t.Fatal("error must be sticky")
	}
}

func TestAlignedTail(t *testing.T) {
	buf := []byte{0b11010110, 0b10110011, 0b01110001, 0b11001010}
	for off := range 25 {
		r := newBitReader(buf)
		r.pos = off
		tail, _ := r.alignedTail(nil)
		// Every byte of the tail must equal the eight bits at that offset.
		for i := range len(tail) {
			probe := newBitReader(buf)
			probe.pos = off + 8*i
			want := probe.get(8)
			if probe.err != nil {
				break // past the end; the tail's last byte is zero padded
			}
			if uint64(tail[i]) != want {
				t.Fatalf("offset %d byte %d: %08b, want %08b", off, i, tail[i], want)
			}
		}
	}
}

// --- messages ----------------------------------------------------------------

type field struct {
	key  uint8
	kind Kind
	i    int64
	u    uint64
	b    bool
	f32  float32
	f64  float64
	s    string
	raw  []byte
}

func writeRecord(w *Writer, fs []field) {
	for _, f := range fs {
		w.Key(f.key)
		switch f.kind {
		case KindInt:
			w.Int(f.i)
		case KindUint:
			w.Uint(f.u)
		case KindBool:
			w.Bool(f.b)
		case KindFloat32:
			w.Float32(f.f32)
		case KindFloat64:
			w.Float64(f.f64)
		case KindString:
			w.Str(f.s)
		case KindBytes:
			w.Bytes(f.raw)
		}
	}
	w.End()
}

func readRecord(t *testing.T, r *Reader, fs []field) {
	t.Helper()
	for i, want := range fs {
		k := r.Key()
		if k != want.key {
			t.Fatalf("field %d: key %d, want %d", i, k, want.key)
		}
		switch want.kind {
		case KindInt:
			if got := r.Int(); got != want.i {
				t.Fatalf("field %d: int %d, want %d", i, got, want.i)
			}
		case KindUint:
			if got := r.Uint(); got != want.u {
				t.Fatalf("field %d: uint %d, want %d", i, got, want.u)
			}
		case KindBool:
			if got := r.Bool(); got != want.b {
				t.Fatalf("field %d: bool %v, want %v", i, got, want.b)
			}
		case KindFloat32:
			if got := r.Float32(); got != want.f32 {
				t.Fatalf("field %d: float32 %v, want %v", i, got, want.f32)
			}
		case KindFloat64:
			if got := r.Float64(); got != want.f64 {
				t.Fatalf("field %d: float64 %v, want %v", i, got, want.f64)
			}
		case KindString:
			if got := r.Str(); got != want.s {
				t.Fatalf("field %d: string %q, want %q", i, got, want.s)
			}
		case KindBytes:
			if got := r.Bytes(); string(got) != string(want.raw) {
				t.Fatalf("field %d: bytes %q, want %q", i, got, want.raw)
			}
		}
	}
	if k := r.Key(); k != TerminatorKey {
		t.Fatalf("expected terminator, got key %d", k)
	}
}

func usuario() []field {
	return []field{
		{key: 0x35, kind: KindInt, i: 1234},
		{key: 0x9a, kind: KindString, s: "Usuario1"},
		{key: 0x98, kind: KindInt, i: 22},
		{key: 0x32, kind: KindInt, i: 1234512345},
		{key: 0x5d, kind: KindInt, i: 3333},
	}
}

func TestMessageRoundTrip(t *testing.T) {
	for _, shape := range []Shape{ShapeStruct, ShapeArray1, ShapeArray2, ShapeArray3} {
		for _, allPos := range []bool{true, false} {
			for _, keys := range []KeyWidth{Keys8, Keys4} {
				rec := usuario()
				if keys == Keys4 { // ids a narrow key can hold
					for i := range rec {
						rec[i].key = uint8(i + 1)
					}
				}
				w := NewWriter(nil, shape, allPos, keys)
				for range shape.Records() {
					writeRecord(w, rec)
				}
				buf := w.Done()

				if !IsCompact(buf) {
					t.Fatal("IsCompact false on a compact message")
				}
				r, err := NewReader(buf)
				if err != nil {
					t.Fatal(err)
				}
				if r.Shape() != shape || r.AllPositive() != allPos || r.Keys() != keys {
					t.Fatalf("header: shape %d/%d allPositive %v/%v keys %d/%d",
						r.Shape(), shape, r.AllPositive(), allPos, r.Keys(), keys)
				}
				for range r.Records() {
					readRecord(t, r, rec)
				}
				if err := r.Err(); err != nil {
					t.Fatal(err)
				}
				t.Logf("shape=%d allPositive=%v keys=%d -> %d bytes", shape, allPos, keys, len(buf))
			}
		}
	}
}

// The narrow terminator is 15 on the wire, and every id below it must still come
// back as itself -- 14 in particular, which sits directly under it.
func TestNarrowKeyBoundary(t *testing.T) {
	w := NewWriter(nil, ShapeStruct, true, Keys4)
	for k := uint8(0); k <= MaxNarrowKey; k++ {
		w.Key(k)
		w.Uint(uint64(k))
	}
	w.End()

	r, err := NewReader(w.Done())
	if err != nil {
		t.Fatal(err)
	}
	if r.Keys() != Keys4 {
		t.Fatalf("keys %d, want %d", r.Keys(), Keys4)
	}
	for k := uint8(0); k <= MaxNarrowKey; k++ {
		if got := r.Key(); got != k {
			t.Fatalf("key %d, want %d", got, k)
		}
		if got := r.Uint(); got != uint64(k) {
			t.Fatalf("value %d, want %d", got, k)
		}
	}
	// Reported as TerminatorKey even though 15 went on the wire, so a caller's
	// end-of-record check reads the same at either width.
	if got := r.Key(); got != TerminatorKey {
		t.Fatalf("terminator reported as %d, want %d", got, TerminatorKey)
	}
	if err := r.Err(); err != nil {
		t.Fatal(err)
	}
}

// An id that does not fit the declared width would be truncated into a different
// field, or into the terminator. The writer refuses rather than emit that.
func TestNarrowKeyOutOfRange(t *testing.T) {
	for _, k := range []uint8{MaxNarrowKey + 1, 16, 0x9a, TerminatorKey} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("key %d: no panic", k)
				}
			}()
			NewWriter(nil, ShapeStruct, true, Keys4).Key(k)
		}()
	}
	// The same ids are ordinary fields at the wide width.
	w := NewWriter(nil, ShapeStruct, true, Keys8)
	w.Key(0x9a)
	w.Uint(1)
	w.End()
	r, _ := NewReader(w.Done())
	if got := r.Key(); got != 0x9a {
		t.Fatalf("key %d, want %d", got, 0x9a)
	}
}

// Every value form must survive, including the awkward ones: negatives without
// ALL_POSITIVE, uint64 above MaxInt64, NaN, empty and binary strings.
func TestAllKindsRoundTrip(t *testing.T) {
	fs := []field{
		{key: 1, kind: KindInt, i: -1},
		{key: 2, kind: KindInt, i: math.MinInt64},
		{key: 3, kind: KindInt, i: math.MaxInt64},
		{key: 4, kind: KindUint, u: math.MaxUint64},
		{key: 5, kind: KindBool, b: true},
		{key: 6, kind: KindBool, b: false},
		{key: 7, kind: KindFloat32, f32: float32(math.Inf(-1))},
		{key: 8, kind: KindFloat64, f64: math.Pi},
		{key: 9, kind: KindString, s: ""},
		{key: 10, kind: KindString, s: "el niño comió jamón"},
		{key: 11, kind: KindString, s: "\x00\xff\xfe binary"},
		{key: 12, kind: KindString, s: strings.Repeat("long ", 60)},
		{key: 13, kind: KindBytes, raw: []byte{}},
		{key: 14, kind: KindBytes, raw: []byte{0, 1, 2, 255}},
	}
	// The ids here run 1..14, so both key widths can carry this record.
	for _, keys := range []KeyWidth{Keys8, Keys4} {
		w := NewWriter(nil, ShapeStruct, false, keys)
		writeRecord(w, fs)
		r, err := NewReader(w.Done())
		if err != nil {
			t.Fatal(err)
		}
		readRecord(t, r, fs)
		if err := r.Err(); err != nil {
			t.Fatal(err)
		}
	}
}

// NaN needs its own check: it is not equal to itself, so readRecord cannot test it.
func TestNaNRoundTrip(t *testing.T) {
	w := NewWriter(nil, ShapeStruct, true, Keys8)
	w.Key(1)
	w.Float64(math.NaN())
	w.Key(2)
	w.Float32(float32(math.NaN()))
	w.End()
	r, _ := NewReader(w.Done())
	r.Key()
	if got := r.Float64(); !math.IsNaN(got) {
		t.Fatalf("float64 NaN -> %v", got)
	}
	r.Key()
	if got := r.Float32(); !math.IsNaN(float64(got)) {
		t.Fatalf("float32 NaN -> %v", got)
	}
}

// A field the reader's type does not know must be steppable, which is the whole
// reason keys are on the wire.
func TestSkipUnknownField(t *testing.T) {
	fs := []field{
		{key: 1, kind: KindInt, i: 1234},
		{key: 2, kind: KindString, s: "skipped entirely"},
		{key: 3, kind: KindFloat64, f64: 2.5},
		{key: 4, kind: KindBytes, raw: []byte("also skipped")},
		{key: 5, kind: KindInt, i: 99},
	}
	w := NewWriter(nil, ShapeStruct, true, Keys8)
	writeRecord(w, fs)
	r, _ := NewReader(w.Done())

	for _, want := range fs {
		k := r.Key()
		if k == 1 || k == 5 {
			if got := r.Int(); got != want.i {
				t.Fatalf("key %d: %d, want %d", k, got, want.i)
			}
			continue
		}
		r.Skip(want.kind) // pretend the type has no such field
	}
	if k := r.Key(); k != TerminatorKey {
		t.Fatalf("terminator lost after skipping: key %d", k)
	}
	if err := r.Err(); err != nil {
		t.Fatal(err)
	}
}

// Omitting a zero-valued field is how compact mode pays nothing for absence.
func TestOmittedFields(t *testing.T) {
	w := NewWriter(nil, ShapeStruct, true, Keys8)
	w.Key(0x32)
	w.Int(1236000001)
	w.Key(0x9a)
	w.Str("Ana")
	w.End()
	buf := w.Done()

	r, _ := NewReader(buf)
	if k := r.Key(); k != 0x32 {
		t.Fatalf("key %d", k)
	}
	if got := r.Int(); got != 1236000001 {
		t.Fatalf("int %d", got)
	}
	if k := r.Key(); k != 0x9a {
		t.Fatalf("key %d", k)
	}
	if got := r.Str(); got != "Ana" {
		t.Fatalf("string %q", got)
	}
	if k := r.Key(); k != TerminatorKey {
		t.Fatalf("terminator: %d", k)
	}
	t.Logf("three omitted fields -> %d bytes", len(buf))
}

func TestNotCompact(t *testing.T) {
	for _, buf := range [][]byte{nil, {}, {0x02}, {0x04, 0xff}} {
		if IsCompact(buf) {
			t.Fatalf("%x reported compact", buf)
		}
		if _, err := NewReader(buf); err != ErrNotCompact {
			t.Fatalf("%x: err %v, want ErrNotCompact", buf, err)
		}
	}
}

// Truncated and corrupt input must produce an error, never a panic.
func TestCorruptInput(t *testing.T) {
	w := NewWriter(nil, ShapeArray3, true, Keys8)
	for range 3 {
		writeRecord(w, usuario())
	}
	full := w.Done()

	for cut := 1; cut < len(full); cut++ {
		buf := full[:cut]
		r, err := NewReader(buf)
		if err != nil {
			continue
		}
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("cut at %d panicked: %v", cut, p)
				}
			}()
			for range r.Records() {
				for range 32 {
					if r.Key() == TerminatorKey || r.Err() != nil {
						break
					}
					r.Int()
					r.Str()
				}
			}
		}()
	}
}

func FuzzReader(f *testing.F) {
	w := NewWriter(nil, ShapeStruct, true, Keys8)
	writeRecord(w, usuario())
	f.Add(w.Done())
	f.Add([]byte{0x01})
	f.Add([]byte{0x0f, 0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, buf []byte) {
		r, err := NewReader(buf)
		if err != nil {
			return
		}
		for range r.Records() {
			for range 64 {
				if r.Err() != nil || r.Key() == TerminatorKey {
					break
				}
				r.Int()
				r.Uint()
				r.Str()
				r.Bytes()
			}
		}
	})
}

// --- size ---------------------------------------------------------------------

// The headline claim, kept honest: a lone Usuario in compact mode against what
// the same values cost as plain LEB128 with the same keys.
func TestSizeAgainstLEB128(t *testing.T) {
	w := NewWriter(nil, ShapeStruct, true, Keys8)
	writeRecord(w, usuario())
	// Before Done, so the two are compared over the same span: the final pad is
	// an artefact of where the record happens to end, not of either encoding.
	spent := w.Bits()
	buf := w.Done()

	leb := headerBits
	for _, f := range usuario() {
		leb += 8
		switch f.kind {
		case KindInt:
			b := bits.Len64(uint64(f.i))
			if b == 0 {
				b = 1
			}
			leb += 8 * ((b + 6) / 7)
		case KindString:
			leb += 8 * 8 // packed5 frame for "Usuario1"
		}
	}
	leb += 8 // terminator
	fmt.Printf("Usuario n=1: compact %d B (%d bits), LEB128-with-keys %d bits\n",
		len(buf), spent, leb)
	if spent > leb {
		t.Fatalf("compact %d bits is worse than LEB128 %d bits", spent, leb)
	}
}

// The same record with narrow keys, which is what an explicitly numbered struct
// gets: five keys and a terminator at four bits each instead of eight, against
// the one bit the header flag costs.
func TestSizeNarrowKeys(t *testing.T) {
	fs := usuario()
	for i := range fs {
		fs[i].key = uint8(i + 1) // as if tagged cb:"1".."5"
	}
	wide := NewWriter(nil, ShapeStruct, true, Keys8)
	writeRecord(wide, fs)
	wideBits := wide.Bits()

	narrow := NewWriter(nil, ShapeStruct, true, Keys4)
	writeRecord(narrow, fs)
	narrowBits := narrow.Bits()

	fmt.Printf("Usuario n=1: Keys8 %d B (%d bits), Keys4 %d B (%d bits)\n",
		len(wide.Done()), wideBits, len(narrow.Done()), narrowBits)
	if want := wideBits - 4*(len(fs)+1); narrowBits != want {
		t.Fatalf("narrow spent %d bits, want %d", narrowBits, want)
	}
}
