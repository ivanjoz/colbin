package wire

import "testing"

func TestBitmapRoundTrip(t *testing.T) {
	writer := NewBitmapWriter(nil, 9)
	writer.U32(0, 7)
	writer.U32(1, 42)
	writer.U16(2, 103)
	writer.U16(3, 5)
	writer.U16(4, 0) // omitted
	writer.Bool(5, false)
	writer.U16(6, 0x0139)
	writer.String(8, "hi")

	reader := NewBitmapReader(writer.Buffer())
	type field struct {
		key   uint8
		value uint64
	}
	var got []field
	var text string
	for reader.More() {
		switch key := reader.Key(); key {
		case 8:
			text = reader.String()
		default:
			got = append(got, field{key, reader.Uint()})
		}
	}
	if err := reader.Err(); err != nil {
		t.Fatal(err)
	}
	want := []field{{0, 7}, {1, 42}, {2, 103}, {3, 5}, {6, 0x0139}}
	if len(got) != len(want) {
		t.Fatalf("read %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("read %v, want %v", got, want)
		}
	}
	if text != "hi" {
		t.Fatalf("string read as %q", text)
	}
}

// The arithmetic from BYTE_ALIGNED_PLAN.md §2.7, asserted rather than argued:
// against the benchmark record, the bitmap beats both keyed forms, and against
// a very sparse one it loses.
func TestBitmapAgainstBothKeyedForms(t *testing.T) {
	type sizes struct{ narrow, wide, bitmap int }
	measure := func(set func(n *Writer, w *Writer8, b *BitmapWriter)) sizes {
		narrow, wide := Writer{}, Writer8{}
		bitmap := NewBitmapWriter(nil, 9)
		set(&narrow, &wide, &bitmap)
		return sizes{len(narrow.Buffer), len(wide.Buffer), len(bitmap.Buffer())}
	}

	// Five of ten fields set, the benchmark record.
	got := measure(func(n *Writer, w *Writer8, b *BitmapWriter) {
		for _, f := range []struct {
			key   uint8
			value uint32
		}{{0, 7}, {1, 42}, {2, 103}, {3, 5}, {6, 0x0139}} {
			n.U32(f.key, f.value)
			w.U32(f.key, f.value)
			b.U32(f.key, f.value)
		}
	})
	// The bitmap used to win this outright at ten against narrow's eleven, by
	// having no key byte anywhere. Reclaiming the unsigned sign bit put two of
	// these five values (7 and 5) entirely inside their nibble, and narrow now
	// wins at nine — a key byte it does spend against a payload byte it no
	// longer does.
	if got != (sizes{9, 11, 10}) {
		t.Fatalf("five of ten: %+v, want narrow 9 wide 11 bitmap 10", got)
	}

	// All ten present and small. This used to be the bitmap's best case by a
	// wide margin — 13 against narrow's 20. Values 1..7 now ride in their nibble
	// and 8..10 spend one payload byte, so narrow ties it at 13 without giving
	// up the key.
	got = measure(func(n *Writer, w *Writer8, b *BitmapWriter) {
		for key := range uint8(10) {
			n.U32(key, uint32(key)+1)
			w.U32(key, uint32(key)+1)
			b.U32(key, uint32(key)+1)
		}
	})
	if got != (sizes{13, 20, 13}) {
		t.Fatalf("ten of ten: %+v, want narrow 13 wide 20 bitmap 13", got)
	}

	// Two of ten, which is the shape the bitmap loses on.
	got = measure(func(n *Writer, w *Writer8, b *BitmapWriter) {
		n.U32(0, 7)
		w.U32(0, 7)
		b.U32(0, 7)
		n.U32(1, 42)
		w.U32(1, 42)
		b.U32(1, 42)
	})
	if got.bitmap <= got.narrow {
		t.Fatalf("two of ten: %+v, expected the bitmap to lose to the narrow key", got)
	}
}

// An unknown field is still skippable, and still nameable by its bit position.
func TestBitmapSkipsAnUnknownField(t *testing.T) {
	writer := NewBitmapWriter(nil, 9)
	writer.U32(0, 7)
	writer.String(1, "responses.go:539")
	writer.Int(2, -1234567)
	writer.U32(3, 1<<20)

	reader := NewBitmapReader(writer.Buffer())
	keys := []uint8{}
	for reader.More() {
		keys = append(keys, reader.Key())
		if reader.Key() == 2 {
			if got := reader.Int(); got != -1234567 {
				t.Fatalf("key 2 read as %d", got)
			}
			continue
		}
		if !reader.Skip() {
			t.Fatalf("skip of key %d failed: %v", reader.Key(), reader.Err())
		}
	}
	if err := reader.Err(); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 4 || keys[0] != 0 || keys[3] != 3 {
		t.Fatalf("saw keys %v, want 0..3", keys)
	}
}

// Arbitrary and truncated bytes must never panic.
func TestBitmapGarbageIsRefused(t *testing.T) {
	writer := NewBitmapWriter(nil, 9)
	writer.U32(0, 7)
	writer.String(1, "responses.go:539")
	writer.Int(2, -1234567)
	full := writer.Buffer()

	for cut := range len(full) {
		reader := NewBitmapReader(full[:cut])
		for reader.More() {
			if !reader.Skip() {
				break
			}
		}
		_ = reader.Err()
	}
	for seed := range 5000 {
		message := make([]byte, seed%19)
		for index := range message {
			message[index] = byte(seed*29 + index*13)
		}
		reader := NewBitmapReader(message)
		for reader.More() {
			if !reader.Skip() {
				break
			}
		}
	}
}
