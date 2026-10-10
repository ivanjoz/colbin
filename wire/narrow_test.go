package wire

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

// A string of up to eight bytes is sized by its nibble and carries no length at
// all. Past that it is the length form, whose length is one byte to 253, then
// 0xFE and a u16, then 0xFF and a u32 — so there is no size a string cannot
// carry, and every step costs only what the size needs.
func TestStringSizes(t *testing.T) {
	for _, size := range []int{
		1, 8, 9, 200, inlineLength, inlineLength + 1, 0xFFFF, 0x1_0000, 300_000, 1 << 22,
	} {
		value := strings.Repeat("x", size)
		writer := Writer{}
		writer.String(2, value)
		header := 1 // [key|nibble], the nibble sizing the string
		switch {
		case size <= maxSized:
		case size <= inlineLength:
			header = 2 // [key|1100][length]
		case size <= 0xFFFF:
			header = 4 // [key|1100][0xFE][u16]
		default:
			header = 6 // [key|1100][0xFF][u32]
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
		reader = NewReader(writer.Buffer)
		if got := reader.Bytes(); string(got) != value {
			t.Fatalf("size %d read as bytes gave %d bytes", size, len(got))
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

// A size is what an unauthenticated peer controls. There is no continuation run
// to make unbounded — the length's first byte names its width outright — so what
// is left to refuse is a length the message does not contain, a length whose own
// bytes are not all there, and a form the read at hand does not take.
func TestARunawaySizeIsRefused(t *testing.T) {
	// Key 1, the length form, the u32 escape and a length past the message. The
	// length is kept under 2^31 so a 32-bit build, where a larger one is past
	// what the platform can address and refused as ErrSizeTooLarge first, tests
	// the same thing.
	reader := NewReader([]byte{0x1C, length32, 0xFF, 0xFF, 0xFF, 0x7F})
	reader.Bytes()
	if reader.Err() != ErrTruncated {
		t.Fatalf("a length past the message gave %v", reader.Err())
	}
	reader = NewReader([]byte{0x1C, length16, 0xFF, 0xFF, 0})
	reader.Bytes()
	if reader.Err() != ErrTruncated {
		t.Fatalf("a u16 length past the message gave %v", reader.Err())
	}

	// An escape whose own bytes are not all there.
	for _, message := range [][]byte{{0x1C, length32, 0xFF}, {0x1C, length16, 0xFF}, {0x1C}} {
		reader = NewReader(message)
		reader.Bytes()
		if reader.Err() != ErrTruncated {
			t.Fatalf("% x gave %v", message, reader.Err())
		}
	}

	// A sized field whose bytes are not all there.
	reader = NewReader([]byte{0x1B, 1, 2, 3})
	reader.Uint()
	if reader.Err() != ErrTruncated {
		t.Fatalf("an eight-byte field holding three gave %v", reader.Err())
	}

	// Forms the read does not take: a string with no payload, the flags `11` on a
	// string, a packed string read as bytes, a length form on an unsigned field
	// that is not an explicit zero, and an integer array sized by the nibble.
	for _, refused := range []struct {
		message []byte
		read    func(*Reader)
	}{
		{[]byte{0x10}, func(r *Reader) { _ = r.String() }},
		{[]byte{0x13}, func(r *Reader) { r.Bytes() }},
		{[]byte{0x1F, 1, 'x'}, func(r *Reader) { _ = r.String() }},
		{[]byte{0x1D, 1, 0}, func(r *Reader) { r.Bytes() }},
		{[]byte{0x1C, 1, 5}, func(r *Reader) { r.Uint() }},
		{[]byte{0x1D, 0}, func(r *Reader) { r.Uint() }},
		{[]byte{0x1D, 0}, func(r *Reader) { r.Int() }},
		{[]byte{0x14, 5}, func(r *Reader) { r.Ints(nil) }},
		{[]byte{0x1D, 0}, func(r *Reader) { r.Strings(nil) }},
		{[]byte{0x1E, 0}, func(r *Reader) { r.StructBody() }},
	} {
		reader = NewReader(refused.message)
		refused.read(&reader)
		if reader.Err() != ErrBadEscape {
			t.Fatalf("% x gave %v", refused.message, reader.Err())
		}
		// A form a read refuses is still a field a skip steps over.
		reader = NewReader(refused.message)
		if !reader.Skip() || reader.More() {
			t.Fatalf("% x could not be skipped: %v", refused.message, reader.Err())
		}
	}

	// A length that is not a whole number of elements.
	reader = NewReader([]byte{0x1D, 3, 1, 2, 3})
	reader.Int16s(nil)
	if reader.Err() != ErrTruncated {
		t.Fatalf("three bytes of two-byte elements gave %v", reader.Err())
	}

	// A negative magnitude wider than an int64.
	reader = NewReader([]byte{0x1C, 9, 1, 0, 0, 0, 0, 0, 0, 0, 0})
	reader.Int()
	if reader.Err() != ErrFieldTooWide {
		t.Fatalf("a nine-byte negative magnitude gave %v", reader.Err())
	}
	reader = NewReader([]byte{0x1C, 8, 1, 0, 0, 0, 0, 0, 0, 0x80})
	reader.Int()
	if reader.Err() != ErrFieldTooWide {
		t.Fatalf("-(2^63+1) gave %v", reader.Err())
	}
}

// Writers use the shortest length; readers take any. A length written longer
// than it needs to be reads the same, which is what lets a writer that cannot
// know a length in advance reserve room for it.
func TestALongerLengthFormReadsTheSame(t *testing.T) {
	for _, message := range [][]byte{
		{0x1C, 3, 'a', 'b', 'c'},
		{0x1C, length16, 3, 0, 'a', 'b', 'c'},
		{0x1C, length32, 3, 0, 0, 0, 'a', 'b', 'c'},
		{0x16, 'a', 'b', 'c'},
	} {
		reader := NewReader(message)
		if got := reader.String(); got != "abc" || reader.Err() != nil || reader.More() {
			t.Fatalf("% x read as %q, %v", message, got, reader.Err())
		}
	}
	for _, message := range [][]byte{{0x1C, 0}, {0x1C, length16, 0, 0}, {0x1C, length32, 0, 0, 0, 0}} {
		reader := NewReader(message)
		if got := reader.Uint(); got != 0 || reader.Err() != nil || reader.More() {
			t.Fatalf("% x read as %d, %v", message, got, reader.Err())
		}
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
	// costs two bytes, behind a four-byte header — the key and flags, then 0xFE
	// and a u16 for a length past 253.
	if want := 4 + 300*2; len(writer.Buffer) != want {
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

// Every prefix of a valid message must fail rather than decode something,
// unless it ends between two fields: a length that runs past the buffer is the
// one thing a wire parser must never act on.
func TestTruncatedMessagesAreRefused(t *testing.T) {
	writer := Writer{}
	between := map[int]bool{}
	writer.Int(1, 1767225600123)
	between[len(writer.Buffer)] = true
	writer.String(2, "responses.go:539")
	between[len(writer.Buffer)] = true
	writer.Int32s(3, []int32{1234567, 7654321})
	between[len(writer.Buffer)] = true
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
		if refused := reader.Err() != nil; refused == between[cut] {
			t.Fatalf("a cut at %d of %d: refused %v, between fields %v",
				cut, len(full), refused, between[cut])
		}
		// A reader that knows none of the keys steps over every field, and has
		// to refuse exactly the same cuts: a skip sizes a field as a read does.
		reader = NewReader(full[:cut])
		for reader.More() && reader.Skip() {
		}
		if refused := reader.Err() != nil; refused == between[cut] {
			t.Fatalf("skipping, a cut at %d of %d: refused %v, between fields %v",
				cut, len(full), refused, between[cut])
		}
	}
}

// The nibble is a size, and an unsigned value takes the fewest bytes that hold
// it: 1..4 ride in the nibble, and past that nibble 4..11 is exactly one to
// eight bytes. Zero is not written, except explicitly, as the length form with
// nothing in it.
func TestEverySizeCodeIsAssigned(t *testing.T) {
	for _, expect := range []struct {
		magnitude uint64
		nibble    uint8
		payload   int
	}{
		{0, 12, 1}, // the explicit zero: 1100 and a length of nothing
		{1, 0, 0},
		{4, 3, 0},
		{5, 4, 1}, // the first value that needs a byte
		{255, 4, 1},
		{1 << 8, 5, 2},
		{1 << 16, 6, 3},
		{1 << 24, 7, 4},
		{1 << 32, 8, 5},
		{1 << 40, 9, 6},
		{1 << 48, 10, 7}, // exact at every width, seven included
		{1 << 56, 11, 8},
	} {
		writer := Writer{}
		// Key 1 with a zero value writes nothing, so the zero case goes through
		// the explicit writer that a pointer to zero uses.
		if expect.magnitude == 0 {
			writer.Zero(1)
		} else {
			writer.Uint(1, expect.magnitude)
		}
		if got := writer.Buffer[0] & 0b1111; got != expect.nibble {
			t.Fatalf("%#x used nibble %d, want %d", expect.magnitude, got, expect.nibble)
		}
		if got := len(writer.Buffer) - 1; got != expect.payload {
			t.Fatalf("%#x wrote %d payload bytes, want %d", expect.magnitude, got, expect.payload)
		}
		reader := NewReader(writer.Buffer)
		if got := reader.Uint(); got != expect.magnitude || reader.Err() != nil {
			t.Fatalf("%#x round-tripped as %#x, %v", expect.magnitude, got, reader.Err())
		}
	}
}

// The signed table parts from the unsigned one at nibble 3, which is −1, and in
// the length form, which carries a negative magnitude.
func TestSignedForms(t *testing.T) {
	for _, expect := range []struct {
		value int64
		bytes []byte
	}{
		{1, []byte{0x10}},
		{3, []byte{0x12}},
		{-1, []byte{0x13}},
		{4, []byte{0x14, 4}},
		{255, []byte{0x14, 0xFF}},
		{256, []byte{0x15, 0, 1}},
		{-2, []byte{0x1C, 1, 2}},
		{-256, []byte{0x1C, 2, 0, 1}},
		{math.MaxInt64, []byte{0x1B, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x7F}},
		{math.MinInt64, []byte{0x1C, 8, 0, 0, 0, 0, 0, 0, 0, 0x80}},
	} {
		writer := Writer{}
		writer.Int(1, expect.value)
		if !bytes.Equal(writer.Buffer, expect.bytes) {
			t.Fatalf("Int(%d) wrote % x, want % x", expect.value, writer.Buffer, expect.bytes)
		}
		writer = Writer{}
		if expect.value == int64(int32(expect.value)) {
			writer.I32(1, int32(expect.value))
			if !bytes.Equal(writer.Buffer, expect.bytes) {
				t.Fatalf("I32(%d) wrote % x, want % x", expect.value, writer.Buffer, expect.bytes)
			}
		}
		reader := NewReader(expect.bytes)
		if got := reader.Int(); got != expect.value || reader.Err() != nil {
			t.Fatalf("% x read as %d, %v", expect.bytes, got, reader.Err())
		}
	}
	// The explicit zero is the same bytes for a signed field as for every other.
	writer := Writer{}
	writer.Zero(1)
	reader := NewReader(writer.Buffer)
	if got := reader.Int(); got != 0 || reader.Err() != nil || reader.More() {
		t.Fatalf("an explicit zero read as %d, %v", got, reader.Err())
	}
}

// An integer array's width comes from the element type and the values: a signed
// type is two's complement at the narrowest width that holds every element, an
// unsigned one magnitudes at the narrowest that holds the largest. The length is
// in bytes and the count is not written.
func TestArrayForms(t *testing.T) {
	for _, expect := range []struct {
		name  string
		write func(*Writer)
		bytes []byte
	}{
		{"int8 range", func(w *Writer) { w.Ints(1, []int64{127, -128}) }, []byte{0x1C, 2, 0x7F, 0x80}},
		{"past int8", func(w *Writer) { w.Ints(1, []int64{128}) }, []byte{0x1D, 2, 0x80, 0}},
		{"past int8 below", func(w *Writer) { w.Int16s(1, []int16{-129}) }, []byte{0x1D, 2, 0x7F, 0xFF}},
		{"uint16 to a byte", func(w *Writer) { w.Uint16s(1, []uint16{200}) }, []byte{0x1C, 1, 200}},
		{"uint64 to a byte", func(w *Writer) { w.Uint64s(1, []uint64{200}) }, []byte{0x1C, 1, 200}},
		{"uint64 top bit", func(w *Writer) { w.Uint64s(1, []uint64{1 << 63}) },
			[]byte{0x1F, 8, 0, 0, 0, 0, 0, 0, 0, 0x80}},
		{"int32 wide", func(w *Writer) { w.Int32s(1, []int32{-1, 1 << 20}) },
			[]byte{0x1E, 8, 0xFF, 0xFF, 0xFF, 0xFF, 0, 0, 0x10, 0}},
	} {
		writer := Writer{}
		expect.write(&writer)
		if !bytes.Equal(writer.Buffer, expect.bytes) {
			t.Fatalf("%s wrote % x, want % x", expect.name, writer.Buffer, expect.bytes)
		}
	}
	// A []uint64 is magnitudes, so one past 2^63 reads back as itself, and one
	// that fits a byte costs a byte.
	reader := NewReader([]byte{0x1F, 8, 0, 0, 0, 0, 0, 0, 0, 0x80, 0x2C, 1, 200})
	if got := reader.Uint64s(nil); len(got) != 1 || got[0] != 1<<63 {
		t.Fatalf("[]uint64{1<<63} read as %v", got)
	}
	if got := reader.Uint64s(nil); len(got) != 1 || got[0] != 200 {
		t.Fatalf("[]uint64{200} read as %v", got)
	}
	// And the same byte read as a signed type is sign-extended.
	reader = NewReader([]byte{0x1C, 1, 200})
	if got := reader.Int16s(nil); len(got) != 1 || got[0] != -56 {
		t.Fatalf("0xC8 read as an int16 gave %v", got)
	}
}

// A string array carries no count: the reader walks the sizes inside the field's
// length, and an element that runs past it is refused rather than read into the
// next field.
func TestStringArrayForm(t *testing.T) {
	writer := Writer{}
	writer.Strings(1, []string{"ab", "", "c"})
	if want := []byte{0x1C, 6, 2, 'a', 'b', 0, 1, 'c'}; !bytes.Equal(writer.Buffer, want) {
		t.Fatalf("wrote % x, want % x", writer.Buffer, want)
	}
	reader := NewReader([]byte{0x1C, 3, 2, 'a', 'b', 0x20})
	if got := reader.Strings(nil); len(got) != 1 || got[0] != "ab" || reader.Key() != 2 {
		t.Fatalf("read %q", got)
	}
	reader = NewReader([]byte{0x1C, 3, 3, 'a', 'b', 0x20})
	reader.Strings(nil)
	if reader.Err() != ErrTruncated {
		t.Fatalf("an element past its field gave %v", reader.Err())
	}
}

// Every shape a narrow field can take is stepped over by Skip, landing exactly
// on the field after it. This is the property the nibble exists for.
func TestSkipStepsOverEveryShape(t *testing.T) {
	writers := []func(*Writer){
		func(w *Writer) { w.Uint(3, 2) },
		func(w *Writer) { w.Uint(3, 1<<40) },
		func(w *Writer) { w.Int(3, -1) },
		func(w *Writer) { w.Int(3, -1_234_567) },
		func(w *Writer) { w.Bool(3, true) },
		func(w *Writer) { w.F64(3, 0.1) },
		func(w *Writer) { w.String(3, "abc") },
		func(w *Writer) { w.String(3, strings.Repeat("x", 300)) },
		func(w *Writer) { w.String(3, strings.Repeat("x", 70_000)) },
		func(w *Writer) { w.PackedString(3, "RESPONSES-GO-539") },
		func(w *Writer) { w.Int32s(3, []int32{-1, 1 << 20}) },
		func(w *Writer) { w.Strings(3, []string{"a", "", strings.Repeat("b", 300)}) },
		func(w *Writer) { w.Zero(3) },
		func(w *Writer) {
			mark := w.OpenStruct(3)
			w.String(0, strings.Repeat("s", 400))
			w.Close(mark)
		},
		func(w *Writer) {
			list := w.OpenList(3, 2)
			for range 2 {
				element := w.OpenElement()
				w.Uint(0, 9)
				w.CloseElement(element)
			}
			w.Close(list)
		},
		func(w *Writer) {
			table := w.OpenTable(3, 3)
			w.Column(0, []int64{1, 2, 3})
			w.Strings(1, []string{"x", "y", "z"})
			w.Close(table)
		},
		func(w *Writer) {
			entries := w.OpenMap(3, 1)
			w.ElementString("k")
			w.ElementInt(-5)
			w.Close(entries)
		},
	}
	for index, write := range writers {
		writer := Writer{}
		writer.U16(1, 7)
		write(&writer)
		writer.String(2, "after")
		reader := NewReader(writer.Buffer)
		if reader.U16() != 7 || reader.Key() != 3 {
			t.Fatalf("shape %d: the field before it read wrong", index)
		}
		if !reader.Skip() {
			t.Fatalf("shape %d: %v", index, reader.Err())
		}
		if !reader.More() || reader.Key() != 2 || reader.String() != "after" || reader.More() {
			t.Fatalf("shape %d: the skip landed wrong (% x)", index, writer.Buffer[:min(len(writer.Buffer), 32)])
		}
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
	// The reversal is what makes the trim reach a float at all. A float's zero
	// bytes are its low mantissa bytes, so reversing puts them where the size
	// code elides them: a round value costs two magnitude bytes, and only a
	// pattern that is busy all the way down costs eight.
	for _, expect := range []struct {
		value float64
		bytes int
	}{
		{1.0, 1 + 2},
		{1.5, 1 + 2},
		{-1.5, 1 + 2},
		{0.25, 1 + 2},
		// Exactly a float32, so 29 low bits are zero — which is three whole bytes
		// and five bits over, and only whole bytes trim.
		{float64(float32(0.7)), 1 + 5},
		{0.1, 1 + 8}, // nothing to trim
	} {
		writer = Writer{}
		writer.F64(4, expect.value)
		if len(writer.Buffer) != expect.bytes {
			t.Fatalf("%v took %d bytes, want %d", expect.value, len(writer.Buffer), expect.bytes)
		}
	}
	writer = Writer{}
	writer.F32(4, 1.5)
	if len(writer.Buffer) != 3 {
		t.Fatalf("a round float32 took %d bytes, want 3", len(writer.Buffer))
	}
}

func TestIntArrays(t *testing.T) {
	writer := Writer{}
	writer.Int16s(1, []int16{-300, 42})
	writer.Uint64s(2, []uint64{1, 1 << 40})
	writer.Uint16s(3, []uint16{1, 2, 65535})

	reader := NewReader(writer.Buffer)
	small := reader.Int16s(nil)
	if len(small) != 2 || small[0] != -300 || small[1] != 42 {
		t.Fatalf("[]int16 round-tripped as %v", small)
	}
	big := reader.Uint64s(nil)
	if len(big) != 2 || big[1] != 1<<40 {
		t.Fatalf("[]uint64 round-tripped as %v", big)
	}
	halves := reader.Uint16s(nil)
	if len(halves) != 3 || halves[2] != 65535 {
		t.Fatalf("[]uint16 round-tripped as %v", halves)
	}
	if err := reader.Err(); err != nil {
		t.Fatal(err)
	}
}

// A uint64 past 2^63 is the bit pattern of a negative int64, and must not be
// mistaken for one: the array would be written as two's complement and come back
// wrong.
func TestALargeUnsignedElementIsNotMistakenForNegative(t *testing.T) {
	writer := Writer{}
	writer.Uint64s(1, []uint64{1 << 63, 5})
	reader := NewReader(writer.Buffer)
	back := reader.Uint64s(nil)
	if len(back) != 2 || back[0] != 1<<63 || back[1] != 5 {
		t.Fatalf("round-tripped as %v", back)
	}
}

// The cross-language vector. The Rust port asserts these same three literals in
// rust/src/wire.rs, so neither side can move without the other failing — and
// the message is 3704 bytes, so it is pinned by its length, its first 64 bytes
// and an FNV-1a of all of it rather than by pasted hex.
//
// It covers every shape the lengths touch: a negative in the length form, a
// string past one byte of length, per-element sizes, and an array whose length
// takes the u16 escape.
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

	if len(writer.Buffer) != vectorLength {
		t.Fatalf("the vector is %d bytes, want %d", len(writer.Buffer), vectorLength)
	}
	if !bytes.Equal(writer.Buffer[:64], vectorPrefix) {
		t.Fatalf("the vector's first 64 bytes are %#v", writer.Buffer[:64])
	}
	var checksum uint64 = 14695981039346656037
	for _, value := range writer.Buffer {
		checksum ^= uint64(value)
		checksum *= 1099511628211
	}
	if checksum != vectorChecksum {
		t.Fatalf("the vector's FNV-1a is 0x%016X", checksum)
	}
}

const (
	vectorLength   = 3704
	vectorChecksum = 0x0960B36FDCDB8B59
)

var vectorPrefix = []byte{
	0x0C, 0x03, 0x87, 0xD6, 0x12, 0x19, 0x7B, 0xA8, 0xDA, 0x76, 0x9B, 0x01, 0x20, 0x3C,
	0x10, 0x72, 0x65, 0x73, 0x70, 0x6F, 0x6E, 0x73, 0x65, 0x73, 0x2E, 0x67, 0x6F, 0x3A,
	0x35, 0x33, 0x39, 0x4E, 0x0C, 0x87, 0xD6, 0x12, 0x00, 0x4F, 0x34, 0x8B, 0xFF, 0x03,
	0x00, 0x00, 0x00, 0x5C, 0x31, 0x10, 0x72, 0x65, 0x73, 0x70, 0x6F, 0x6E, 0x73, 0x65,
	0x73, 0x2E, 0x67, 0x6F, 0x3A, 0x35, 0x33, 0x39,
}

// TestNarrowListElementWidths pins the length form a list element takes at every
// size either side of the escape.
//
// The element is the one composite with no descriptor in front of it, so it
// cannot widen the way every other one does — there is nothing to put an `lw`
// code in. Closing it with Close instead of CloseElement wrote a bare four-byte
// length where Element reads the 0xFF escape, and OR-ed 2 into whatever byte
// preceded the placeholder: a message the writer produced and the reader
// refused, for any element body reaching 255 bytes.
func TestNarrowListElementWidths(t *testing.T) {
	for _, size := range []int{0, 1, 253, 254, 255, 256, 1000, 70000} {
		w := Writer{}
		list := w.OpenList(3, 1)
		element := w.OpenElement()
		w.Bytes(0, bytes.Repeat([]byte{'x'}, size))
		w.CloseElement(element)
		w.Close(list)

		r := NewReader(w.Buffer)
		if !r.More() || r.Key() != 3 {
			t.Fatalf("size %d: no list at key 3", size)
		}
		count, elements, ok := r.List()
		if !ok || count != 1 {
			t.Fatalf("size %d: count %d, ok %v", size, count, ok)
		}
		body, ok := elements.Element()
		if !ok {
			t.Fatalf("size %d: %v", size, elements.Err())
		}
		inner := NewReader(body)
		var got []byte
		if size > 0 {
			if !inner.More() {
				t.Fatalf("size %d: the element body is empty", size)
			}
			got = inner.Bytes()
		}
		if err := inner.Err(); err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if len(got) != size {
			t.Fatalf("size %d: read back %d bytes", size, len(got))
		}
	}
}
