package wire

import (
	"math"
	"math/bits"
	"math/rand"
	"testing"
)

// The integer forms, one per key width.
//
// K4 sizes a field with its nibble and lets the type say what the bytes mean:
// an unsigned field's nibbles 0..3 are 1..4, a signed field's are 1..3 and −1,
// and both write a positive value in exactly the bytes it needs. K8 adds a
// varint under SPECIAL and writes it only when it is shorter than the
// byte-count form.
//
// What the varint has to be is *never larger*, which is what most of this file
// checks. The round trips are the other half: the same nibble means different
// things to a signed and an unsigned field, so every entry point has to stay
// paired with the reader that matches it.

// byteCountSize is the K8 field size before the varint existed, which is the
// bound the writer is not allowed to exceed.
func byteCountSize(value int64) int {
	if value >= 0 && value <= maxInlineValue {
		return 2
	}
	magnitude := uint64(value)
	if value < 0 {
		magnitude = -uint64(value)
	}
	_, width := sizeCodeFor(magnitude)
	return 2 + width
}

func interestingValues() []int64 {
	values := []int64{
		0, 1, 2, 7, 8, 15, 16, 63, 127, 128, 129, 255, 256, 300, 511, 512,
		1000, 2047, 2048, 65535, 65536, 1 << 20, 1<<24 - 1, 1 << 24,
		1 << 31, 1 << 32, 1 << 40, 1 << 48, 1 << 56,
		math.MaxInt64, math.MinInt64, math.MinInt64 + 1,
	}
	for _, v := range append([]int64{}, values...) {
		if v != math.MinInt64 {
			values = append(values, -v)
		}
	}
	random := rand.New(rand.NewSource(3))
	for range 4000 {
		values = append(values, random.Int63()-(1<<62), int64(random.Intn(70000)))
	}
	return values
}

// TestWideIntIsNeverLargerThanTheByteCountForm is the property the whole varint
// rests on: the writer emits it only when it wins, so adding it cannot have made
// any value cost more than it did before.
func TestWideIntIsNeverLargerThanTheByteCountForm(t *testing.T) {
	for _, value := range interestingValues() {
		writer := Writer8{}
		writer.Int(7, value)
		if value == 0 {
			continue // a zero field is not written at all
		}
		if got, bound := len(writer.Buffer), byteCountSize(value); got > bound {
			t.Fatalf("Int(%d) wrote %d bytes, more than the %d the byte-count form took",
				value, got, bound)
		}
	}
}

func TestWideIntRoundTrips(t *testing.T) {
	for _, value := range interestingValues() {
		writer := Writer8{}
		writer.Int(7, value)
		if value == 0 {
			continue
		}
		reader := NewReader8(writer.Buffer)
		if reader.Key() != 7 {
			t.Fatalf("Int(%d) wrote key %d", value, reader.Key())
		}
		if got := reader.Int(); got != value {
			t.Fatalf("Int(%d) round-tripped as %d (% x)", value, got, writer.Buffer)
		}
		if err := reader.Err(); err != nil {
			t.Fatalf("Int(%d): %v", value, err)
		}
	}
}

// TestWideUintRoundTrips covers the other side of the same descriptor: Uint
// writes the varint raw where Int zigzags it, and only the schema says which.
func TestWideUintRoundTrips(t *testing.T) {
	values := []uint64{0, 1, 7, 8, 127, 128, 255, 256, 300, 2047, 2048,
		65535, 65536, 1 << 24, 1 << 32, 1 << 48, math.MaxUint64}
	random := rand.New(rand.NewSource(5))
	for range 4000 {
		values = append(values, random.Uint64(), uint64(random.Intn(70000)))
	}
	for _, value := range values {
		writer := Writer8{}
		writer.Uint(7, value)
		if value == 0 {
			continue
		}
		reader := NewReader8(writer.Buffer)
		if got := reader.Uint(); got != value {
			t.Fatalf("Uint(%d) round-tripped as %d (% x)", value, got, writer.Buffer)
		}
		if got, bound := len(writer.Buffer), byteCountSize(int64(value)); value <= math.MaxInt64 && got > bound {
			t.Fatalf("Uint(%d) wrote %d bytes, more than %d", value, got, bound)
		}
	}
}

// TestWideWidthTypedWritersMatchUint is what lets U16 decide the varint with a
// comparison against a constant instead of computing both lengths. The
// hand-placed boundary is only as good as this: every uint16 there is, and the
// values around every crossing for a uint32, must leave the same bytes U16 and
// U32's generic sibling leaves.
func TestWideWidthTypedWritersMatchUint(t *testing.T) {
	for value := range 1 << 16 {
		typed, generic := Writer8{}, Writer8{}
		typed.U16(9, uint16(value))
		generic.Uint(9, uint64(value))
		if string(typed.Buffer) != string(generic.Buffer) {
			t.Fatalf("U16(%d) wrote % x where Uint wrote % x",
				value, typed.Buffer, generic.Buffer)
		}
		if value == 0 {
			continue
		}
		reader := NewReader8(typed.Buffer)
		if got := reader.U16(); got != uint16(value) || reader.Err() != nil {
			t.Fatalf("U16(%d) came back %d: %v", value, got, reader.Err())
		}
	}

	values := []uint32{0, 1, 127, 128, 255, 256, 0x3FF, 0x400, 0xFFFF, 0x1_0000,
		0x1_FFFF, 0x2_0000, 0xFF_FFFF, 0x100_0000, math.MaxUint32}
	random := rand.New(rand.NewSource(17))
	for range 20000 {
		values = append(values, random.Uint32(), uint32(random.Intn(1<<20)))
	}
	for _, value := range values {
		typed, generic := Writer8{}, Writer8{}
		typed.U32(9, value)
		generic.Uint(9, uint64(value))
		if string(typed.Buffer) != string(generic.Buffer) {
			t.Fatalf("U32(%d) wrote % x where Uint wrote % x",
				value, typed.Buffer, generic.Buffer)
		}
		if value == 0 {
			continue
		}
		reader := NewReader8(typed.Buffer)
		if got := reader.U32(); got != value || reader.Err() != nil {
			t.Fatalf("U32(%d) came back %d: %v", value, got, reader.Err())
		}
	}
}

// TestWideVarintIsSkippable is the reason the form lives under a class at all: a
// reader that does not know the key still has to be able to step over it.
func TestWideVarintIsSkippable(t *testing.T) {
	for _, value := range interestingValues() {
		if value == 0 {
			continue // not written at all, so there is nothing to skip
		}
		writer := Writer8{}
		writer.Int(3, value)
		writer.String(4, "after")
		reader := NewReader8(writer.Buffer)
		if !reader.Skip() {
			t.Fatalf("could not skip Int(%d): %v", value, reader.Err())
		}
		if reader.Key() != 4 || reader.String() != "after" {
			t.Fatalf("skipping Int(%d) landed wrong (% x)", value, writer.Buffer)
		}
	}
}

// TestWideVarintTruncationIsAnError checks that a continuation run cut short
// fails rather than reading past the buffer.
func TestWideVarintTruncationIsAnError(t *testing.T) {
	writer := Writer8{}
	writer.Int(1, 1<<20) // wide enough to take several continuation bytes
	for cut := 1; cut < len(writer.Buffer); cut++ {
		reader := NewReader8(writer.Buffer[:cut])
		_ = reader.Int()
		if reader.Err() == nil {
			t.Fatalf("cut to %d of %d decoded without an error",
				cut, len(writer.Buffer))
		}
	}
}

// TestNarrowUnsignedRoundTrips walks every entry point that writes the unsigned
// nibble, because each one encodes it by hand for its own width — and they have
// to agree byte for byte, not only in length.
func TestNarrowUnsignedRoundTrips(t *testing.T) {
	values := []uint64{1, 2, 7, 8, 9, 255, 256, 65535, 65536, 1 << 24,
		1 << 32, 1 << 40, 1 << 48, 1 << 56, math.MaxUint64}
	random := rand.New(rand.NewSource(11))
	for range 4000 {
		values = append(values, random.Uint64(), uint64(random.Intn(70000)))
	}
	for _, value := range values {
		writer := Writer{}
		writer.Uint(1, value)
		reader := NewReader(writer.Buffer)
		if got := reader.Uint(); got != value {
			t.Fatalf("Uint(%d) round-tripped as %d (% x)", value, got, writer.Buffer)
		}

		if value <= math.MaxUint16 {
			w16 := Writer{}
			w16.U16(1, uint16(value))
			r16 := NewReader(w16.Buffer)
			if got := r16.U16(); got != uint16(value) {
				t.Fatalf("U16(%d) round-tripped as %d (% x)", value, got, w16.Buffer)
			}
			if string(w16.Buffer) != string(writer.Buffer) {
				t.Fatalf("U16(%d) wrote % x where Uint wrote % x",
					value, w16.Buffer, writer.Buffer)
			}
		}
		if value <= math.MaxUint32 {
			w32 := Writer{}
			w32.U32(1, uint32(value))
			r32 := NewReader(w32.Buffer)
			if got := r32.U32(); got != uint32(value) {
				t.Fatalf("U32(%d) round-tripped as %d (% x)", value, got, w32.Buffer)
			}
			if string(w32.Buffer) != string(writer.Buffer) {
				t.Fatalf("U32(%d) wrote % x where Uint wrote % x",
					value, w32.Buffer, writer.Buffer)
			}
		}
	}
}

// TestNarrowSignedRoundTrips guards the signed table, which Int reads and which
// must not pick up the unsigned one by accident: the two part at nibble 3 and in
// the length form. I32 has to write what Int writes.
func TestNarrowSignedRoundTrips(t *testing.T) {
	for _, value := range interestingValues() {
		writer := Writer{}
		writer.Int(1, value)
		if value == 0 {
			if len(writer.Buffer) != 0 {
				t.Fatalf("Int(0) wrote % x", writer.Buffer)
			}
			continue
		}
		reader := NewReader(writer.Buffer)
		if got := reader.Int(); got != value || reader.Err() != nil || reader.More() {
			t.Fatalf("Int(%d) round-tripped as %d (% x): %v", value, got, writer.Buffer, reader.Err())
		}
		if value == int64(int32(value)) {
			w32 := Writer{}
			w32.I32(1, int32(value))
			if string(w32.Buffer) != string(writer.Buffer) {
				t.Fatalf("I32(%d) wrote % x where Int wrote % x", value, w32.Buffer, writer.Buffer)
			}
		}
	}
}

// TestNarrowUnsignedIsExact is the K4 half of the size property: a value costs
// the header and the fewest bytes that hold it, and 1..4 cost the header alone.
func TestNarrowUnsignedIsExact(t *testing.T) {
	for value := uint64(1); value < 70000; value++ {
		writer := Writer{}
		writer.Uint(1, value)
		want := 1 + (bits.Len64(value)+7)/8
		if value <= inlineValues {
			want = 1
		}
		if got := len(writer.Buffer); got != want {
			t.Fatalf("Uint(%d) wrote %d bytes, want %d", value, got, want)
		}
	}
}

// TestNarrowBoolAndFloatsShareTheUnsignedNibble: both are read through Uint, so
// both move with it.
func TestNarrowBoolAndFloatsShareTheUnsignedNibble(t *testing.T) {
	writer := Writer{}
	writer.Bool(1, true)
	writer.F64(2, 1.5)
	writer.F32(3, -2.25)
	// One byte for true, which rides entirely in its nibble; three each for the
	// floats, whose reversed patterns are 0xF83F and 0x10C0 — two bytes of
	// magnitude behind one byte of key and code.
	if len(writer.Buffer) != 1+3+3 {
		t.Fatalf("true, 1.5 and -2.25 took % x", writer.Buffer)
	}
	reader := NewReader(writer.Buffer)
	if !reader.Bool() {
		t.Fatal("true came back false")
	}
	if got := reader.F64(); got != 1.5 {
		t.Fatalf("1.5 came back %v", got)
	}
	if got := reader.F32(); got != -2.25 {
		t.Fatalf("-2.25 came back %v", got)
	}
	if err := reader.Err(); err != nil {
		t.Fatal(err)
	}
}
