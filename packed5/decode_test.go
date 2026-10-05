package packed5

// What the decoder does with input the encoder did not write.
//
// Most payloads here are hand-built from units, because the encoder cannot
// produce the shapes that matter: a reserved table index, an escape count past
// four, a token cut off by the end of the stream. The rule the whole file holds
// AppendString to is that any src, size and case mode either decode to some
// string or return an error — never a panic, never a read past src, never output
// that is not bounded by the payload's length.

import (
	"errors"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
)

// packUnits builds a payload from a unit sequence, padding to the grid the way
// the encoder does so the unit count is recoverable from the length.
func packUnits(units []uint8) []byte {
	return packSlow(padToGrid(append([]uint8(nil), units...)))
}

// decodeUnits packs units and decodes them with no slack after the payload.
func decodeUnits(units []uint8, upper bool) (string, error) {
	payload := packUnits(units)
	out, err := AppendString(nil, payload, len(payload), upper)
	return string(out), err
}

// maxDecodedLen is the expansion bound AppendString documents. The densest token
// is the three-byte '€' in two units, which is 2.4 bytes per payload byte; a
// number is next at four bytes per three units, and every other token is at most
// one byte per unit.
func maxDecodedLen(payload int) int { return payload*12/5 + 4 }

func TestAppendStringRejectsBadSize(t *testing.T) {
	payload, n, upper, ok := AppendPayload(nil, "el niño comió jamón")
	if !ok {
		t.Fatal("expected the sample to pack")
	}
	for _, c := range []struct {
		name string
		src  []byte
		size int
		want error
	}{
		{"negative", payload, -1, ErrBadLength},
		{"most negative", payload, math.MinInt, ErrBadLength},
		{"one past the buffer", payload[:n-1], n, ErrTruncated},
		{"empty buffer", nil, 1, ErrTruncated},
		{"largest", payload, math.MaxInt, ErrTruncated},
	} {
		t.Run(c.name, func(t *testing.T) {
			dst := []byte("kept")
			got, err := AppendString(dst, c.src, c.size, upper)
			if !errors.Is(err, c.want) {
				t.Errorf("got %v, want %v", err, c.want)
			}
			if string(got) != "kept" {
				t.Errorf("dst changed on error: %q", got)
			}
		})
	}
	// Size zero is the empty payload, which decodes to nothing.
	if got, err := AppendString([]byte("x"), nil, 0, true); err != nil || string(got) != "x" {
		t.Errorf("size 0: %q, %v", got, err)
	}
}

func TestDecodeReservedSymbol(t *testing.T) {
	for idx := extReserved; idx < extEscape; idx++ {
		// A letter first, so the reserved index is met mid-stream.
		if _, err := decodeUnits([]uint8{letterIndex('a'), opExt, uint8(idx)}, false); !errors.Is(err, ErrReservedSymbol) {
			t.Errorf("extTable[%d]: got %v, want ErrReservedSymbol", idx, err)
		}
	}
	// Every assigned index must decode instead.
	for idx := range extReserved {
		got, err := decodeUnits([]uint8{opExt, uint8(idx)}, false)
		if err != nil {
			t.Errorf("extTable[%d]: %v", idx, err)
		} else if got != extTable[idx] {
			t.Errorf("extTable[%d]: decoded %q, want %q", idx, got, extTable[idx])
		}
	}
	// symTable has no reserved entries: all 32 operands are characters.
	for idx := range 32 {
		got, err := decodeUnits([]uint8{opSymbol, uint8(idx)}, false)
		if err != nil || got != string(symTable[idx]) {
			t.Errorf("symTable[%d]: %q, %v", idx, got, err)
		}
	}
}

func TestDecodeBadEscape(t *testing.T) {
	for _, c := range []struct {
		name  string
		units []uint8
		want  error
	}{
		{"count past four", []uint8{opExt, extEscape, 4, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, ErrBadEscape},
		{"count at maximum", []uint8{opExt, extEscape, 31, 0, 0, 0, 0, 0, 0}, ErrBadEscape},
		{"high half out of range", []uint8{opExt, extEscape, 0, 1, 8}, ErrBadEscape},
		// A truncation has to be built on the unit grid, or packUnits's pad
		// supplies the very unit the case is meant to be missing.
		{"truncated bytes", []uint8{opExt, extEscape, 3, 1, 0}, ErrTruncated},
		{"truncated count", []uint8{0, opExt, extEscape}, ErrTruncated},
		{"truncated operand", []uint8{opExt}, ErrTruncated},
		{"truncated symbol operand", []uint8{opSymbol}, ErrTruncated},
		{"truncated number", []uint8{0, 0, opNumber, 1}, ErrTruncated},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := decodeUnits(c.units, false); !errors.Is(err, c.want) {
				t.Errorf("got %v, want %v", err, c.want)
			}
		})
	}
	// A legal escape of each length must still decode.
	for n := 1; n <= maxEscapeRun; n++ {
		units := []uint8{opExt, extEscape, uint8(n - 1)}
		want := make([]byte, n)
		for k := range n {
			want[k] = byte(0x80 + k)
			units = append(units, want[k]&31, want[k]>>5)
		}
		got, err := decodeUnits(units, false)
		if err != nil || got != string(want) {
			t.Errorf("escape of %d: %q, %v", n, got, err)
		}
	}
}

// TestGridPadDecodesToNothing is the property the terminator rests on: the
// trailing simple toggle the encoder pads with must have no effect, whichever
// case mode the stream is in.
func TestGridPadDecodesToNothing(t *testing.T) {
	for _, upper := range []bool{false, true} {
		for n := 1; n <= 40; n++ {
			units := make([]uint8, n)
			for i := range units {
				units[i] = uint8(i % 26)
			}
			want := make([]byte, n)
			for i := range want {
				if upper {
					want[i] = 'A' + units[i]
				} else {
					want[i] = 'a' + units[i]
				}
			}
			got, err := decodeUnits(units, upper)
			if err != nil || got != string(want) {
				t.Fatalf("upper=%v n=%d: %q, %v", upper, n, got, err)
			}
		}
	}
}

// TestSlackIsNeverDecoded: bytes after the payload are read by the group loader
// but must never reach the output, whatever they hold.
func TestSlackIsNeverDecoded(t *testing.T) {
	for _, s := range []string{"hello world", "el niño comió jamón", strings.Repeat("ab ", 40)} {
		buf, n, upper, ok := AppendPayload(nil, s)
		if !ok {
			t.Fatalf("%q did not pack", s)
		}
		for _, trailing := range []string{"", "x", slackBytes, "trailing garbage of some length"} {
			got, err := AppendString(nil, append(buf[:n:n], trailing...), n, upper)
			if err != nil || string(got) != s {
				t.Errorf("%q with %q after it: %q, %v", s, trailing, got, err)
			}
		}
	}
}

// TestDecodeTruncatedAtEveryPrefix walks every prefix of a valid payload. With
// the original size it must be refused; read at its own shorter size it must not
// come back as the original string.
func TestDecodeTruncatedAtEveryPrefix(t *testing.T) {
	for _, s := range []string{"hello", "el niño comió jamón", "SKU-4217-hola",
		strings.Repeat("a", 20) + "\x01\x02\x03\x04", strings.Repeat("ab ", 60)} {
		buf, n, upper, ok := AppendPayload(nil, s)
		if !ok {
			t.Fatalf("%q did not pack", s)
		}
		for k := range n {
			prefix := buf[:k:k]
			if _, err := AppendString(nil, prefix, n, upper); !errors.Is(err, ErrTruncated) {
				t.Errorf("%q: %d of %d bytes at the full size: got %v, want ErrTruncated", s, k, n, err)
			}
			if got, err := AppendString(nil, prefix, k, upper); err == nil && string(got) == s {
				t.Errorf("%q: a %d-byte prefix of %d decoded in full", s, k, n)
			}
		}
	}
}

// TestDecodeBitFlipsDoNotPanic flips every bit of a set of valid payloads and
// decodes each in both case modes, with and without slack. A corrupt payload may
// decode to anything or fail; it may not panic or read out of bounds.
func TestDecodeBitFlipsDoNotPanic(t *testing.T) {
	for _, s := range []string{"hello", "helloWorld", "el niño comió jamón",
		"SKU-4217-hola", strings.Repeat("a", 20) + "\x01\x02\x03\x04", "1023 1023",
		strings.Repeat("ab ", 20)} {
		buf, n, _, ok := AppendPayload(nil, s)
		if !ok {
			t.Fatalf("%q did not pack", s)
		}
		for i := range n {
			for bit := range 8 {
				bad := append([]byte(nil), buf[:n]...)
				bad[i] ^= 1 << bit
				for _, upper := range []bool{false, true} {
					_, _ = AppendString(nil, bad, n, upper)
					_, _ = AppendString(nil, append(bad, slackBytes...), n, upper)
				}
			}
		}
	}
}

func TestDecodeGarbageDoesNotPanic(t *testing.T) {
	rng := rand.New(rand.NewPCG(21, 22))
	decoded, refused := 0, 0
	for range 200000 {
		buf := make([]byte, rng.IntN(40))
		for i := range buf {
			buf[i] = byte(rng.IntN(256))
		}
		size := rng.IntN(len(buf) + 1)
		if _, err := AppendString(nil, buf, size, rng.IntN(2) == 1); err == nil {
			decoded++
		} else {
			refused++
		}
	}
	if decoded == 0 || refused == 0 {
		t.Errorf("%d random payloads decoded and %d were refused; want some of each", decoded, refused)
	}
}

// TestExpansionIsBounded is the decompression-bomb check: the output is bounded
// by the payload, so the work a peer can ask for is bounded by the bytes it sent.
func TestExpansionIsBounded(t *testing.T) {
	// The densest token is an ext carrying the three-byte '€': two units for
	// three bytes. Build payloads made only of those.
	for pairs := 1; pairs <= 200; pairs++ {
		units := make([]uint8, 0, 2*pairs)
		for range pairs {
			units = append(units, opExt, uint8(extEuro))
		}
		payload := packUnits(units)
		got, err := decodeUnits(units, false)
		if err != nil {
			t.Fatal(err)
		}
		if bound := maxDecodedLen(len(payload)); len(got) > bound {
			t.Fatalf("%d pairs: decoded %d bytes, bound is %d", pairs, len(got), bound)
		}
	}
	// And over random unit streams, which mix every token.
	rng := rand.New(rand.NewPCG(23, 24))
	for range 20000 {
		units := make([]uint8, rng.IntN(60))
		for i := range units {
			units[i] = uint8(rng.IntN(32))
		}
		got, err := decodeUnits(units, false)
		if err != nil {
			continue
		}
		if bound := maxDecodedLen(len(packUnits(units))); len(got) > bound {
			t.Fatalf("units %v: decoded %d bytes, bound is %d", units, len(got), bound)
		}
	}
}

func FuzzAppendString(f *testing.F) {
	for _, s := range []string{"a", "hello", "el niño comió jamón", "SKU-4217-hola",
		"\x01\x02\x03\x04", strings.Repeat("ab ", 40)} {
		buf, n, upper, _ := AppendPayload(nil, s)
		f.Add(buf, n, upper)
	}
	f.Add([]byte{}, 0, false)
	f.Add([]byte{0xFF, 0xFF}, -1, true)
	f.Add([]byte{0x01}, 5, false)
	f.Fuzz(func(t *testing.T, src []byte, size int, upper bool) {
		got, err := AppendString(nil, src, size, upper)
		if err != nil {
			return
		}
		if bound := maxDecodedLen(size); len(got) > bound {
			t.Fatalf("%d-byte payload decoded to %d bytes, bound is %d", size, len(got), bound)
		}
		// The slack after the payload must not change what it says.
		exact, err := AppendString(nil, src[:size:size], size, upper)
		if err != nil || string(exact) != string(got) {
			t.Fatalf("without slack: %q, %v; with it: %q", exact, err, got)
		}
		// And whatever came out must survive a round trip of its own.
		roundtrip(t, string(got))
	})
}

// TestPayloadGeometry pins the two length functions against each other, which is
// what makes the unit count recoverable from the payload alone.
func TestPayloadGeometry(t *testing.T) {
	for units := range 5000 {
		b := payloadBytes(units)
		if b*8 < units*5 {
			t.Fatalf("%d units do not fit in %d bytes", units, b)
		}
		if units > 0 && (b-1)*8 >= units*5 {
			t.Fatalf("%d units fit in %d bytes, payloadBytes said %d", units, b-1, b)
		}
		// The pad is what closes the gap, and one unit always suffices.
		if got := payloadUnits(b); got != units && got != units+1 {
			t.Fatalf("%d units in %d bytes reads back as %d", units, b, got)
		}
	}
}
