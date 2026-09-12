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
		if got := reader.PackedString(); got != value {
			t.Fatalf("%q round-tripped as %q", value, got)
		}
		if err := reader.Err(); err != nil {
			t.Fatalf("%q: %v", value, err)
		}
		// A raw writer's field must read back through the packed reader too.
		writer = Writer8{}
		writer.String(3, value)
		reader = NewReader8(writer.Buffer)
		if got := reader.PackedString(); got != value {
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
