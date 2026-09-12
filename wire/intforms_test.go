package wire

import (
	"math"
	"math/rand"
	"testing"
)

// The two integer forms, one per key width.
//
// K4 reclaims the sign bit on unsigned fields, because a narrow reader has the
// schema and already knows the field is unsigned. K8 adds a varint under
// SPECIAL and writes it only when it is shorter than the byte-count form.
//
// What both changes have to be is *never larger*, which is what most of this
// file checks. The round trips are the other half: the same four bits now mean
// different things for a signed and an unsigned field, so every entry point has
// to stay paired with the reader that matches it.

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

// TestNarrowUnsignedRoundTrips walks every entry point that shares the reclaimed
// nibble, because each one encodes it by hand for its own width.
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
			if len(w16.Buffer) != len(writer.Buffer) {
				t.Fatalf("U16(%d) took %d bytes where Uint took %d",
					value, len(w16.Buffer), len(writer.Buffer))
			}
		}
		if value <= math.MaxUint32 {
			w32 := Writer{}
			w32.U32(1, uint32(value))
			r32 := NewReader(w32.Buffer)
			if got := r32.U32(); got != uint32(value) {
				t.Fatalf("U32(%d) round-tripped as %d (% x)", value, got, w32.Buffer)
			}
			if len(w32.Buffer) != len(writer.Buffer) {
				t.Fatalf("U32(%d) took %d bytes where Uint took %d",
					value, len(w32.Buffer), len(writer.Buffer))
			}
		}
	}
}

// TestNarrowSignedStillUsesTheSignBit guards the half that did not change. A
// signed nibble is [positive:1][size:3] and is read by Int, which must not pick
// up the unsigned table by accident.
func TestNarrowSignedStillUsesTheSignBit(t *testing.T) {
	for _, value := range interestingValues() {
		writer := Writer{}
		writer.Int(1, value)
		if value == 0 {
			continue
		}
		reader := NewReader(writer.Buffer)
		if got := reader.Int(); got != value {
			t.Fatalf("Int(%d) round-tripped as %d (% x)", value, got, writer.Buffer)
		}
	}
}

// TestNarrowUnsignedIsNeverLarger is the K4 half of the size property.
func TestNarrowUnsignedIsNeverLarger(t *testing.T) {
	for value := uint64(1); value < 70000; value++ {
		writer := Writer{}
		writer.Uint(1, value)
		// The old form was a sign bit and a byte count, so one byte of header
		// plus the magnitude's width, with values under 256 always paying one.
		_, width := sizeCodeFor(value)
		if got := len(writer.Buffer); got > 1+width {
			t.Fatalf("Uint(%d) wrote %d bytes, more than the %d the signed form took",
				value, got, 1+width)
		}
	}
}

// TestNarrowBoolAndFloatsShareTheUnsignedNibble: both are read through Uint, so
// both had to move with it.
func TestNarrowBoolAndFloatsShareTheUnsignedNibble(t *testing.T) {
	writer := Writer{}
	writer.Bool(1, true)
	writer.F64(2, 1.5)
	writer.F32(3, -2.25)
	// One byte for true, which now rides entirely in its nibble; three each for
	// the floats, whose reversed patterns are 0xF83F and 0x10C0 — two bytes of
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
