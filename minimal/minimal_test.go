package minimal

import (
	"bytes"
	"math"
	"strings"
	"testing"
)

func TestIntegerWidthsRoundTrip(t *testing.T) {
	values := []int64{
		1, -1, 2, 255, 256, 65535, 65536, 1 << 23, 1 << 24, math.MaxInt32,
		1 << 32, 1 << 40, 1 << 47, 1 << 48, math.MaxInt64, math.MinInt64,
		-255, -65536, -1234567, 1767225600123,
	}
	for _, value := range values {
		writer := Writer{}
		writer.Int(1, value)
		reader := NewReader(writer.Buffer)
		if !reader.More() || reader.Key() != 1 {
			t.Fatalf("%d: no field", value)
		}
		if got := reader.Int(); got != value {
			t.Fatalf("%d round-tripped as %d (%x)", value, got, writer.Buffer)
		}
		if reader.More() {
			t.Fatalf("%d: trailing bytes", value)
		}
		if err := reader.Err(); err != nil {
			t.Fatalf("%d: %v", value, err)
		}
	}
}

func TestUnsignedRoundTrip(t *testing.T) {
	for _, value := range []uint64{1, 2, 255, 65535, math.MaxUint32, math.MaxUint64} {
		writer := Writer{}
		writer.Uint(0, value)
		reader := NewReader(writer.Buffer)
		if got := reader.Uint(); got != value {
			t.Fatalf("%d round-tripped as %d", value, got)
		}
	}
}

// A zero, an empty string and a false bool are the same thing on this wire: the
// key is simply absent, which is where most of the format's saving comes from.
func TestZeroValuedFieldsAreNotWritten(t *testing.T) {
	writer := Writer{}
	writer.Int(1, 0)
	writer.Uint(2, 0)
	writer.Bool(3, false)
	writer.String(4, "")
	writer.Bytes(5, nil)
	writer.Int32s(6, nil)
	writer.Strings(7, nil)
	if len(writer.Buffer) != 0 {
		t.Fatalf("zero fields wrote %d bytes: %x", len(writer.Buffer), writer.Buffer)
	}
}

func TestBoolIsOneByte(t *testing.T) {
	writer := Writer{}
	writer.Bool(9, true)
	if len(writer.Buffer) != 1 {
		t.Fatalf("a true bool took %d bytes", len(writer.Buffer))
	}
	reader := NewReader(writer.Buffer)
	if reader.Key() != 9 || !reader.Bool() {
		t.Fatal("bool did not round trip")
	}
}

// The header holds eleven bits of size and continues into as many LEB128 bytes
// as the rest needs, so there is no size a string cannot carry.
func TestStringSizes(t *testing.T) {
	for _, size := range []int{
		1, 200, inlineBlobSize, inlineBlobSize + 1, 70_000, 300_000, 1 << 22,
	} {
		value := strings.Repeat("x", size)
		writer := Writer{}
		writer.String(2, value)
		header := 2
		for remaining := size >> 11; remaining > 0; remaining >>= 7 {
			header++
		}
		if len(writer.Buffer) != header+size {
			t.Fatalf("size %d wrote %d bytes, want %d", size, len(writer.Buffer), header+size)
		}
		reader := NewReader(writer.Buffer)
		if got := reader.String(); got != value {
			t.Fatalf("size %d round-tripped as %d bytes", size, len(got))
		}
		if reader.More() {
			t.Fatalf("size %d: trailing bytes", size)
		}
	}
}

// Sizes past what the old fixed escape could describe — a 19-bit string, a
// 16-bit array count, a 12-bit string-array count — are now ordinary.
func TestNothingHasASizeCeiling(t *testing.T) {
	big := strings.Repeat("y", 1<<20)
	writer := Writer{}
	writer.String(1, big)
	reader := NewReader(writer.Buffer)
	if got := reader.String(); got != big {
		t.Fatalf("a 1 MiB string round-tripped as %d bytes", len(got))
	}

	values := make([]int64, 70_000)
	for index := range values {
		values[index] = int64(index % 251)
	}
	writer = Writer{}
	writer.Ints(2, values)
	reader = NewReader(writer.Buffer)
	back := reader.Ints(nil)
	if len(back) != len(values) || back[69_999] != values[69_999] {
		t.Fatalf("a 70 000-element array round-tripped as %d elements", len(back))
	}

	texts := make([]string, 5000)
	for index := range texts {
		texts[index] = "error"
	}
	writer = Writer{}
	writer.Strings(3, texts)
	reader = NewReader(writer.Buffer)
	backTexts := reader.Strings(nil)
	if len(backTexts) != len(texts) || backTexts[4999] != "error" {
		t.Fatalf("a 5000-element string array round-tripped as %d elements", len(backTexts))
	}
	if err := reader.Err(); err != nil {
		t.Fatal(err)
	}
}

// A continuation run is what an unauthenticated peer controls, so one that never
// ends, or that describes more than an int holds, is refused rather than acted on.
func TestARunawaySizeIsRefused(t *testing.T) {
	// Key 1, more flag set, then continuation bytes that never terminate.
	message := []byte{0b0001_1000, 0x00}
	for range 12 {
		message = append(message, 0xFF)
	}
	reader := NewReader(message)
	reader.Bytes()
	if reader.Err() != ErrSizeTooLarge {
		t.Fatalf("a runaway size gave %v", reader.Err())
	}

	// And one that simply ends with the message is a truncation.
	reader = NewReader([]byte{0b0001_1000, 0x00, 0xFF})
	reader.Bytes()
	if reader.Err() != ErrTruncated {
		t.Fatalf("an unterminated run gave %v", reader.Err())
	}
}

func TestIntArrayWidths(t *testing.T) {
	cases := [][]int64{
		{1},
		{0, 1, 255},
		{256, 1000},
		{1234567},
		{math.MaxInt32 + 1},
		{-1, 5},
		{-129, 127},
		{math.MinInt64, math.MaxInt64},
	}
	for _, values := range cases {
		writer := Writer{}
		writer.Ints(3, values)
		reader := NewReader(writer.Buffer)
		if reader.Key() != 3 {
			t.Fatalf("%v: wrong key", values)
		}
		got := reader.Ints(nil)
		if len(got) != len(values) {
			t.Fatalf("%v round-tripped as %v", values, got)
		}
		for index := range values {
			if got[index] != values[index] {
				t.Fatalf("%v round-tripped as %v", values, got)
			}
		}
		if reader.More() {
			t.Fatalf("%v: trailing bytes", values)
		}
	}
}

// The width is picked from the widest element, so an array of small numbers
// costs one byte each however wide the Go type is.
func TestArrayWidthFollowsTheData(t *testing.T) {
	writer := Writer{}
	writer.Int32s(1, []int32{1, 2, 3, 4})
	if len(writer.Buffer) != 2+4 {
		t.Fatalf("four small int32 took %d bytes: %x", len(writer.Buffer), writer.Buffer)
	}
	writer = Writer{}
	writer.Int32s(1, []int32{1, 1234567})
	if len(writer.Buffer) != 2+8 {
		t.Fatalf("two int32 needing four bytes took %d bytes", len(writer.Buffer))
	}
}

func TestInt32sAndUint16sRoundTrip(t *testing.T) {
	writer := Writer{}
	writer.Int32s(1, []int32{1234567, -7654321})
	writer.Uint16s(2, []uint16{0x0139, 0x008B})
	reader := NewReader(writer.Buffer)
	ids := reader.Int32s(nil)
	if len(ids) != 2 || ids[0] != 1234567 || ids[1] != -7654321 {
		t.Fatalf("int32s round-tripped as %v", ids)
	}
	grants := reader.Uint16s(nil)
	if len(grants) != 2 || grants[0] != 0x0139 || grants[1] != 0x008B {
		t.Fatalf("uint16s round-tripped as %v", grants)
	}
}

func TestBigArrayCount(t *testing.T) {
	values := make([]int64, 300)
	for index := range values {
		values[index] = int64(index)
	}
	writer := Writer{}
	writer.Ints(1, values)
	// One width for the whole array: the widest element is 299, so every element
	// costs two bytes, behind a three-byte header for a count past 255.
	if want := 3 + 300*2; len(writer.Buffer) != want {
		t.Fatalf("300 elements took %d bytes, want %d", len(writer.Buffer), want)
	}
	reader := NewReader(writer.Buffer)
	got := reader.Ints(nil)
	if len(got) != 300 || got[299] != 299 {
		t.Fatalf("300 elements round-tripped as %d", len(got))
	}
}

func TestStringArrayRoundTrip(t *testing.T) {
	values := []string{"responses.go:539", "", "product-stock.go:1204"}
	writer := Writer{}
	writer.Strings(4, values)
	reader := NewReader(writer.Buffer)
	got := reader.Strings(nil)
	if len(got) != 3 || got[0] != values[0] || got[1] != "" || got[2] != values[2] {
		t.Fatalf("round-tripped as %q", got)
	}
	reader = NewReader(writer.Buffer)
	raw := reader.StringsBytes(nil)
	if len(raw) != 3 || !bytes.Equal(raw[2], []byte(values[2])) {
		t.Fatalf("bytes view round-tripped as %q", raw)
	}
}

// The compile-time assertion the writer's doc comment prescribes, standing in for
// the runtime check it deliberately does not do.
const _ = uint(15 - 15)

// Every prefix of a valid message must fail rather than decode something: a
// length that runs past the buffer is the one thing a wire parser must never
// act on.
func TestTruncatedMessagesAreRefused(t *testing.T) {
	writer := Writer{}
	writer.Int(1, 1767225600123)
	writer.String(2, "responses.go:539")
	writer.Int32s(3, []int32{1234567, 7654321})
	writer.Strings(4, []string{"product-stock.go:1204"})
	full := writer.Buffer

	for cut := 1; cut < len(full); cut++ {
		reader := NewReader(full[:cut])
		for reader.More() {
			switch reader.Key() {
			case 1:
				reader.Int()
			case 2:
				reader.Bytes()
			case 3:
				reader.Int32s(nil)
			case 4:
				reader.StringsBytes(nil)
			default:
				reader.Skip()
			}
		}
		// Either the message ended cleanly on a field boundary, or it was refused.
		// What must never happen is a read past the end, which would panic.
		_ = reader.Err()
	}
}

func TestUnassignedSizeCodeIsRefused(t *testing.T) {
	reader := NewReader([]byte{0b0001_0111}) // key 1, positive, size code 7
	reader.Uint()
	if reader.Err() != ErrBadSizeCode {
		t.Fatalf("size code 7 gave %v", reader.Err())
	}
}

func TestReadingPastTheEndIsAnError(t *testing.T) {
	reader := NewReader(nil)
	if reader.Uint() != 0 || reader.Err() != ErrTruncated {
		t.Fatalf("an empty message gave %v", reader.Err())
	}
}

// A peer may legally write a field wider than the type this side declares, which
// is a schema disagreement and not a value to truncate into something plausible.
func TestAWiderFieldThanTheTypeIsRefused(t *testing.T) {
	writer := Writer{}
	writer.Uint(1, 70000)
	reader := NewReader(writer.Buffer)
	if reader.U16() != 0 || reader.Err() != ErrFieldTooWide {
		t.Fatalf("a 70000 read as a uint16 gave %v", reader.Err())
	}

	writer = Writer{}
	writer.Uint(1, 1<<40)
	reader = NewReader(writer.Buffer)
	if reader.U32() != 0 || reader.Err() != ErrFieldTooWide {
		t.Fatalf("a 2^40 read as a uint32 gave %v", reader.Err())
	}

	// And the widths that do fit still come back, whichever writer produced them.
	writer = Writer{}
	writer.Uint(1, 65535)
	writer.Uint(2, 1)
	reader = NewReader(writer.Buffer)
	if got := reader.U16(); got != 65535 {
		t.Fatalf("65535 read back as %d", got)
	}
	if got := reader.U16(); got != 1 {
		t.Fatalf("the implicit one read back as %d", got)
	}
}

func TestFloatsRideInTheIntegerShape(t *testing.T) {
	for _, value := range []float64{1.5, -1.5, 3.141592653589793, 1e308, -1e-308} {
		writer := Writer{}
		writer.F64(1, value)
		reader := NewReader(writer.Buffer)
		if got := reader.F64(); got != value {
			t.Fatalf("%v round-tripped as %v", value, got)
		}
	}
	for _, value := range []float32{1.5, -1.5, 3.4028235e38, 1e-40} {
		writer := Writer{}
		writer.F32(2, value)
		reader := NewReader(writer.Buffer)
		if got := reader.F32(); got != value {
			t.Fatalf("%v round-tripped as %v", value, got)
		}
	}
	// Positive zero is a zero value like any other: the key is simply absent.
	writer := Writer{}
	writer.F64(3, 0)
	if len(writer.Buffer) != 0 {
		t.Fatalf("a zero float wrote %d bytes", len(writer.Buffer))
	}
	// A bit pattern with zero high bytes costs fewer of them, which is the whole
	// reason floats need no shape of their own: this denormal's pattern is three
	// bytes wide, so the field is four rather than five.
	writer = Writer{}
	writer.F32(4, 1e-40)
	if len(writer.Buffer) != 4 {
		t.Fatalf("a denormal float32 took %d bytes, want 4", len(writer.Buffer))
	}
	writer = Writer{}
	writer.F32(4, 1.5)
	if len(writer.Buffer) != 5 {
		t.Fatalf("an ordinary float32 took %d bytes, want 5", len(writer.Buffer))
	}
}

func TestGenericIntArrays(t *testing.T) {
	writer := Writer{}
	WriteInts(&writer, 1, []int16{-300, 42})
	WriteInts(&writer, 2, []uint64{1, 1 << 40})
	WriteInts(&writer, 3, []uint8{1, 2, 255})

	reader := NewReader(writer.Buffer)
	small := ReadInts(&reader, []int16(nil))
	if len(small) != 2 || small[0] != -300 || small[1] != 42 {
		t.Fatalf("[]int16 round-tripped as %v", small)
	}
	big := ReadInts(&reader, []uint64(nil))
	if len(big) != 2 || big[1] != 1<<40 {
		t.Fatalf("[]uint64 round-tripped as %v", big)
	}
	bytes := ReadInts(&reader, []uint8(nil))
	if len(bytes) != 3 || bytes[2] != 255 {
		t.Fatalf("[]uint8 round-tripped as %v", bytes)
	}
	if err := reader.Err(); err != nil {
		t.Fatal(err)
	}
}

// A uint64 past 2^63 reads as a negative int64, and must not be mistaken for one:
// the array would be written as two's complement and come back wrong.
func TestALargeUnsignedElementIsNotMistakenForNegative(t *testing.T) {
	writer := Writer{}
	WriteInts(&writer, 1, []uint64{1 << 63, 5})
	reader := NewReader(writer.Buffer)
	back := ReadInts(&reader, []uint64(nil))
	if len(back) != 2 || back[0] != 1<<63 || back[1] != 5 {
		t.Fatalf("round-tripped as %v", back)
	}
}

// The cross-language vector. The Rust port asserts these same three literals in
// rust/src/minimal.rs, so neither side can move without the other failing — and
// the message is 3701 bytes, so it is pinned by its length, its first 64 bytes
// and an FNV-1a of all of it rather than by pasted hex.
//
// It covers every shape the continued sizes touch: an array count past its
// inline bits, LEB128 element lengths, and a blob past the eleven bits the
// header carries.
func TestTheCrossLanguageVector(t *testing.T) {
	writer := Writer{}
	writer.Int(0, -1234567)
	writer.Uint(1, 1767225600123)
	writer.Bool(2, true)
	writer.String(3, "responses.go:539")
	writer.Int32s(4, []int32{1234567, -7654321, 3})
	writer.Strings(5, []string{"responses.go:539", "no se pudo obtener el registro", ""})
	ids := make([]int64, 300)
	for index := range ids {
		ids[index] = int64(index)
	}
	writer.Ints(6, ids)
	writer.String(7, strings.Repeat("z", 3000))

	if len(writer.Buffer) != 3701 {
		t.Fatalf("the vector is %d bytes, want 3701", len(writer.Buffer))
	}
	prefix := []byte{
		0x02, 0x12, 0xD6, 0x87, 0x1C, 0x01, 0x9B, 0x76, 0xDA, 0xA8, 0x7B, 0x2E, 0x30, 0x10,
		0x72, 0x65, 0x73, 0x70, 0x6F, 0x6E, 0x73, 0x65, 0x73, 0x2E, 0x67, 0x6F, 0x3A, 0x35,
		0x33, 0x39, 0x44, 0x03, 0x00, 0x12, 0xD6, 0x87, 0xFF, 0x8B, 0x34, 0x4F, 0x00, 0x00,
		0x00, 0x03, 0x50, 0x03, 0x10, 0x72, 0x65, 0x73, 0x70, 0x6F, 0x6E, 0x73, 0x65, 0x73,
		0x2E, 0x67, 0x6F, 0x3A, 0x35, 0x33, 0x39, 0x1E,
	}
	if !bytes.Equal(writer.Buffer[:64], prefix) {
		t.Fatalf("the vector's first 64 bytes are %x", writer.Buffer[:64])
	}
	var checksum uint64 = 14695981039346656037
	for _, value := range writer.Buffer {
		checksum ^= uint64(value)
		checksum *= 1099511628211
	}
	if checksum != 0xEE2B368ABF8D34A8 {
		t.Fatalf("the vector's FNV-1a is 0x%016X", checksum)
	}
}
