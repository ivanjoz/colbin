package wire

import (
	"strings"
	"testing"
)

// A packed string and a raw one are the same field to a reader: the descriptor
// says which, so nothing has to be configured and nothing can be wrong about it.
func TestPackedStringRoundTrip(t *testing.T) {
	cases := []string{
		"", "A", "USUARIO1", "RESPONSES-GO-539", "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789",
		"lower case is not what packed5 is for",
		strings.Repeat("PACKED", 200),
		"\x00\xff binary", "ñ non-ascii",
	}
	for _, value := range cases {
		writer := Writer8{}
		writer.PackedString(3, value)
		if value == "" {
			if len(writer.Buffer) != 0 {
				t.Fatalf("empty string wrote %d bytes", len(writer.Buffer))
			}
			continue
		}
		reader := NewReader8(writer.Buffer)
		if got := reader.String(); got != value {
			t.Fatalf("%q round-tripped as %q", value, got)
		}
		if err := reader.Err(); err != nil {
			t.Fatalf("%q: %v", value, err)
		}
		// A raw writer's field must read back through the packed reader too.
		writer = Writer8{}
		writer.String(3, value)
		reader = NewReader8(writer.Buffer)
		if got := reader.String(); got != value {
			t.Fatalf("%q written raw round-tripped as %q", value, got)
		}
	}
}

// Packing is chosen per string, so it can never cost anything: a token that does
// not pack smaller is written raw.
func TestPackedStringNeverCostsMore(t *testing.T) {
	for _, value := range []string{
		"USUARIO1", "lower case", "mixed Case 123", "ñ", strings.Repeat("x", 100),
	} {
		packed := Writer8{}
		packed.PackedString(1, value)
		raw := Writer8{}
		raw.String(1, value)
		if len(packed.Buffer) > len(raw.Buffer) {
			t.Fatalf("%q: packed %d bytes, raw %d", value, len(packed.Buffer), len(raw.Buffer))
		}
	}
	// And on the shape it is for, it actually wins.
	value := "RESPONSES-GO-539"
	packed := Writer8{}
	packed.PackedString(1, value)
	raw := Writer8{}
	raw.String(1, value)
	if len(packed.Buffer) >= len(raw.Buffer) {
		t.Fatalf("%q: packed %d bytes, raw %d — expected a saving",
			value, len(packed.Buffer), len(raw.Buffer))
	}
}

// A packed field is still skippable, because the length is in the same place.
func TestPackedStringIsSkippable(t *testing.T) {
	writer := Writer8{}
	writer.PackedString(1, "USUARIO1")
	writer.U32(2, 42)
	reader := NewReader8(writer.Buffer)
	if !reader.Skip() {
		t.Fatalf("skip: %v", reader.Err())
	}
	if got := reader.U32(); got != 42 {
		t.Fatalf("after a skipped packed string: %d", got)
	}
}

// The narrow packed string rides in the length form's flags, so it reads back
// through the same String a raw one does, and steps over like any other field.
func TestNarrowPackedStringRoundTrip(t *testing.T) {
	cases := []string{
		"A", "USUARIO1", "RESPONSES-GO-539", "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789",
		"lower case is not what packed5 is for", "Mixed Case Opening",
		strings.Repeat("PACKED", 200), strings.Repeat("PACKED", 20_000),
		"\x00\xff binary", "ñ non-ascii",
	}
	for _, value := range cases {
		writer := Writer{}
		writer.PackedString(3, value)
		writer.U16(4, 9)
		reader := NewReader(writer.Buffer)
		if got := reader.String(); got != value {
			t.Fatalf("%q round-tripped as %q", value, got)
		}
		if got := reader.U16(); got != 9 || reader.Err() != nil {
			t.Fatalf("%q: the field after it read %d, %v", value, got, reader.Err())
		}
		reader = NewReader(writer.Buffer)
		if !reader.Skip() || reader.U16() != 9 {
			t.Fatalf("%q could not be stepped over: %v", value, reader.Err())
		}
	}
	empty := Writer{}
	empty.PackedString(3, "")
	if len(empty.Buffer) != 0 {
		t.Fatalf("an empty string wrote % x", empty.Buffer)
	}
}

// A narrow string is packed only when the whole field comes out strictly
// smaller, which is not the same comparison as the payloads: a raw string of up
// to eight bytes carries no length, and a packed one always does.
func TestNarrowPackedStringIsStrictlySmaller(t *testing.T) {
	for _, value := range []string{
		"A", "AB", "USUARIO1", "lower case", "mixed Case 123", "ñ",
		strings.Repeat("x", 100), "RESPONSES-GO-539", strings.Repeat("Z", 300),
	} {
		packed := Writer{}
		packed.PackedString(1, value)
		raw := Writer{}
		raw.String(1, value)
		isPacked := packed.Buffer[0]&flagBits != flagRaw && packed.Buffer[0]&0b1111 >= nibbleLength
		switch {
		case len(packed.Buffer) > len(raw.Buffer):
			t.Fatalf("%q: packed %d bytes, raw %d", value, len(packed.Buffer), len(raw.Buffer))
		case isPacked && len(packed.Buffer) == len(raw.Buffer):
			t.Fatalf("%q: packed at no saving", value)
		case !isPacked && string(packed.Buffer) != string(raw.Buffer):
			t.Fatalf("%q: not packed, and not the raw field either: % x", value, packed.Buffer)
		}
	}
	// The flags say which case the stream opens in.
	for _, expect := range []struct {
		value string
		flags uint8
	}{
		{"RESPONSES-GO-539", flagPackedUpper},
		{"responses-go-539-and-more", flagPackedLower},
	} {
		writer := Writer{}
		writer.PackedString(1, expect.value)
		if got := writer.Buffer[0] & 0b1111; got != nibbleLength|expect.flags {
			t.Fatalf("%q: nibble %04b, want %04b", expect.value, got, nibbleLength|expect.flags)
		}
	}
}
