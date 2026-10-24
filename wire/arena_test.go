package wire

import (
	"strings"
	"testing"
)

// Every string an arena hands out keeps its bytes while later ones are copied
// beside it, across chunk boundaries and past the message size it was told, and
// a long one is copied on its own.
func TestArenaStrings(t *testing.T) {
	var arena Arena
	arena.Reset(100)
	var want, got []string
	for index := range 5000 {
		size := index % 40
		if index%997 == 0 {
			size = arenaOwn + 1
		}
		value := strings.Repeat(string(rune('a'+index%26)), size)
		source := []byte(value)
		got = append(got, arena.String(source))
		want = append(want, value)
		clear(source)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("string %d: %q, want %q", index, got[index], want[index])
		}
	}
}

// A message read with an arena reads as it does without one.
func TestStringInMatchesString(t *testing.T) {
	var w Writer
	values := []string{"", "x", "eight ch", "nine char", strings.Repeat("long ", 100), strings.Repeat("y", arenaOwn+5)}
	for index, value := range values {
		w.String(uint8(index), value)
	}
	w.PackedString(uint8(len(values)), "packedlowercasetoken")
	plain, shared := NewReader(w.Buffer), NewReader(w.Buffer)
	var arena Arena
	arena.Reset(len(w.Buffer))
	for plain.More() {
		if a, b := plain.String(), shared.StringIn(&arena); a != b {
			t.Fatalf("StringIn %q, String %q", b, a)
		}
	}
	if plain.Err() != nil || shared.Err() != nil || shared.More() {
		t.Fatal(plain.Err(), shared.Err())
	}
}
