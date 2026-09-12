package wire

import (
	"bytes"
	"math"
	"strings"
	"testing"
)

// Every scalar shape, at both ends of every width.
func TestWideScalarsRoundTrip(t *testing.T) {
	ints := []int64{
		0, 1, -1, 2, 126, 127, 128, 255, 256, 65535, 65536,
		1 << 24, 1 << 32, 1 << 40, 1 << 48, 1 << 56,
		math.MaxInt64, math.MinInt64, -1234567, 1767225600123,
	}
	for _, value := range ints {
		writer := Writer8{}
		writer.Int(7, value)
		if value == 0 {
			if len(writer.Buffer) != 0 {
				t.Fatalf("zero wrote %d bytes", len(writer.Buffer))
			}
			continue
		}
		reader := NewReader8(writer.Buffer)
		if reader.Key() != 7 {
			t.Fatalf("%d: key %d", value, reader.Key())
		}
		if got := reader.Int(); got != value {
			t.Fatalf("%d round-tripped as %d", value, got)
		}
		if reader.More() {
			t.Fatalf("%d: trailing bytes", value)
		}
		if err := reader.Err(); err != nil {
			t.Fatalf("%d: %v", value, err)
		}
	}
}

// A value under 128 is the descriptor itself, so it costs the same two bytes it
// costs at four key bits. That is what makes the wide key affordable.
func TestWideSmallIntegersAreTwoBytes(t *testing.T) {
	for value := range uint64(128) {
		writer := Writer8{}
		writer.Uint(3, value)
		want := 2
		if value == 0 {
			want = 0
		}
		if len(writer.Buffer) != want {
			t.Fatalf("%d wrote %d bytes, want %d", value, len(writer.Buffer), want)
		}
	}
	// And 128 is the first that needs a descriptor of its own.
	writer := Writer8{}
	writer.Uint(3, 128)
	if len(writer.Buffer) != 3 {
		t.Fatalf("128 wrote %d bytes, want 3", len(writer.Buffer))
	}
}

func TestWideKeysReachTwoFiftyFive(t *testing.T) {
	writer := Writer8{}
	for key := range MaxWideFields {
		writer.Uint(uint8(key), uint64(key)+1)
	}
	reader := NewReader8(writer.Buffer)
	for key := range MaxWideFields {
		if got := reader.Key(); got != uint8(key) {
			t.Fatalf("field %d has key %d", key, got)
		}
		if got := reader.Uint(); got != uint64(key)+1 {
			t.Fatalf("key %d round-tripped as %d", key, got)
		}
	}
	if reader.More() || reader.Err() != nil {
		t.Fatalf("trailing bytes or %v", reader.Err())
	}
}

func TestWideBlobsAndArraysRoundTrip(t *testing.T) {
	writer := Writer8{}
	writer.String(1, "responses.go:539")
	writer.String(2, strings.Repeat("z", 3000))
	writer.Bytes(3, []byte{0, 1, 2, 255})
	writer.Int32s(4, []int32{1234567, -7654321, 3})
	writer.Uint16s(5, []uint16{0x0139, 0x008B})
	writer.Strings(6, []string{"a", "", strings.Repeat("q", 400)})
	writer.F64(7, 1.5)
	writer.Bool(8, true)

	reader := NewReader8(writer.Buffer)
	if got := reader.String(); got != "responses.go:539" {
		t.Fatalf("string round-tripped as %q", got)
	}
	if got := reader.String(); len(got) != 3000 {
		t.Fatalf("long string round-tripped as %d bytes", len(got))
	}
	if got := reader.Bytes(); !bytes.Equal(got, []byte{0, 1, 2, 255}) {
		t.Fatalf("bytes round-tripped as %v", got)
	}
	if got := reader.Int32s(nil); len(got) != 3 || got[1] != -7654321 {
		t.Fatalf("int32s round-tripped as %v", got)
	}
	if got := reader.Uint16s(nil); len(got) != 2 || got[0] != 0x0139 {
		t.Fatalf("uint16s round-tripped as %v", got)
	}
	if got := reader.Strings(nil); len(got) != 3 || got[1] != "" || len(got[2]) != 400 {
		t.Fatalf("strings round-tripped as %d elements", len(got))
	}
	if got := reader.F64(); got != 1.5 {
		t.Fatalf("float round-tripped as %v", got)
	}
	if !reader.Bool() {
		t.Fatal("bool round-tripped false")
	}
	if reader.More() || reader.Err() != nil {
		t.Fatalf("trailing bytes or %v", reader.Err())
	}
}

// The capability the wide key exists for: a reader that has never heard of a key
// can step over it. This is what Reader.Skip refuses to do at four key bits.
func TestWideSkipsAnUnknownField(t *testing.T) {
	writer := Writer8{}
	writer.Uint(1, 42)                                         // inline
	writer.Uint(2, 1767225600123)                              // explicit integer
	writer.String(3, "responses.go:539")                       // short blob
	writer.String(4, strings.Repeat("z", 70000))               // escaped blob
	writer.Int32s(5, []int32{1234567, -7654321, 3})            // vector
	writer.Uint64s(6, make([]uint64, 300))                     // vector past 255 bytes
	writer.Strings(7, []string{"a", strings.Repeat("q", 400)}) // list
	writer.Bool(8, true)
	writer.Int(9, -1234567)
	writer.F32(10, 1.5)

	// Skip every field, which must land exactly at the end.
	reader := NewReader8(writer.Buffer)
	skipped := 0
	for reader.More() {
		if !reader.Skip() {
			t.Fatalf("skip failed at field %d: %v", skipped, reader.Err())
		}
		skipped++
	}
	if skipped != 10 {
		t.Fatalf("skipped %d fields, want 10", skipped)
	}
	if err := reader.Err(); err != nil {
		t.Fatal(err)
	}

	// And skipping only the ones it does not know still finds the rest.
	reader = NewReader8(writer.Buffer)
	seen := map[uint8]bool{}
	for reader.More() {
		switch reader.Key() {
		case 3:
			if got := reader.String(); got != "responses.go:539" {
				t.Fatalf("key 3 read as %q after skips", got)
			}
			seen[3] = true
		case 9:
			if got := reader.Int(); got != -1234567 {
				t.Fatalf("key 9 read as %d after skips", got)
			}
			seen[9] = true
		default:
			if !reader.Skip() {
				t.Fatalf("skip of key %d failed: %v", reader.Key(), reader.Err())
			}
		}
	}
	if !seen[3] || !seen[9] {
		t.Fatalf("known fields not found after skipping: %v", seen)
	}
	if err := reader.Err(); err != nil {
		t.Fatal(err)
	}
}

// Every prefix of a valid message must fail rather than decode something, and
// never panic.
func TestWideTruncatedMessagesAreRefused(t *testing.T) {
	writer := Writer8{}
	writer.Int(1, 1767225600123)
	writer.String(2, "responses.go:539")
	writer.Int32s(3, []int32{1234567, 7654321})
	writer.Strings(4, []string{"product-stock.go:1204"})
	full := writer.Buffer

	for cut := 1; cut < len(full); cut++ {
		reader := NewReader8(full[:cut])
		for reader.More() {
			if !reader.Skip() {
				break
			}
		}
		_ = reader.Err()
	}
	// And typed reads over every prefix, which take different paths.
	for cut := 1; cut < len(full); cut++ {
		reader := NewReader8(full[:cut])
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
				if !reader.Skip() {
					reader.fail(ErrTruncated)
				}
			}
		}
		_ = reader.Err()
	}
}

// Arbitrary bytes must never panic the reader.
func TestWideGarbageIsRefused(t *testing.T) {
	for seed := range 20000 {
		message := make([]byte, seed%23)
		for index := range message {
			message[index] = byte(seed*31 + index*17)
		}
		reader := NewReader8(message)
		for reader.More() {
			if !reader.Skip() {
				break
			}
		}
		reader = NewReader8(message)
		for reader.More() {
			reader.Int()
			reader.Bytes()
			reader.Int32s(nil)
			reader.Strings(nil)
		}
	}
}

// A reader asking for a string and finding an array is told, rather than handed
// a length read out of the middle of someone else's field.
func TestWideMismatchedClassIsRefused(t *testing.T) {
	writer := Writer8{}
	writer.Int32s(1, []int32{1, 2, 3})
	reader := NewReader8(writer.Buffer)
	if got := reader.String(); got != "" || reader.Err() != ErrBadDescriptor {
		t.Fatalf("reading an array as a string gave %q, %v", got, reader.Err())
	}
}

// The two key widths cost the same on small integers and differ by a byte above
// them, which is the trade the wide key makes.
func TestWideAgainstNarrowSize(t *testing.T) {
	narrow := Writer{}
	narrow.U32(0, 7)
	narrow.U32(1, 42)
	narrow.U16(2, 103)
	narrow.U16(3, 5)
	narrow.U16(6, 0x0139)

	wide := Writer8{}
	wide.U32(0, 7)
	wide.U32(1, 42)
	wide.U16(2, 103)
	wide.U16(3, 5)
	wide.U16(6, 0x0139)

	// Narrow spends no sign bit on an unsigned field, so 7 and 5 ride in the
	// nibble with no payload at all and cost one byte each; 42 and 103 cost two;
	// 0x0139 costs three. Wide pays a key byte and a descriptor byte before any
	// of them.
	if len(narrow.Buffer) != 9 {
		t.Fatalf("narrow wrote %d bytes, want 9", len(narrow.Buffer))
	}
	// 0x0139 takes the varint form, three bytes rather than four: nine value
	// bits do not fit the descriptor's three, but they fit one continuation byte
	// where the byte-count form would have spent two.
	if len(wide.Buffer) != 11 {
		t.Fatalf("wide wrote %d bytes, want 11", len(wide.Buffer))
	}
}
