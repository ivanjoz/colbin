package packed5

// What the decoder does with input the encoder did not write.
//
// Every frame here is hand-built from units, because the encoder cannot produce
// the shapes that matter: a reserved table index, an escape count past four, a
// length prefix in its non-canonical form. The rule the whole file holds the
// decoder to is that any byte string either decodes to some string or returns an
// error — never a panic, never a read past the buffer, never an allocation
// driven by an attacker's length.

import (
	"bytes"
	"errors"
	"math/rand/v2"
	"strings"
	"testing"
)

// packFrame builds a packed frame from a unit sequence, padding to the grid the
// way the encoder does so the unit count is recoverable from the length.
func packFrame(units []uint8, upper bool) []byte {
	u := append([]uint8(nil), units...)
	if payloadUnits(payloadBytes(len(u))) > len(u) {
		u = append(u, opCaseSimple)
	}
	flags := byte(flagPacked5)
	if upper {
		flags |= flagUppercase
	}
	return append(appendHeader(nil, flags, payloadBytes(len(u))), packSlow(u)...)
}

func TestDecodeErrors(t *testing.T) {
	for _, c := range []struct {
		name string
		in   []byte
		want error
	}{
		{"empty buffer", nil, ErrTruncated},
		{"raw payload truncated", []byte{0<<0 | 5<<lenShift, 'a', 'b'}, ErrTruncated},
		{"packed payload truncated", []byte{flagPacked5 | 5<<lenShift, 0x01}, ErrTruncated},
		{"reserved header bit", []byte{flagPacked5 | flagReserved | 1<<lenShift, 0}, ErrBadHeader},
		{"uvarint truncated", []byte{flagPacked5 | lenEscape<<lenShift, 0x80}, ErrTruncated},
		{"uvarint overlong", append([]byte{flagPacked5 | lenEscape<<lenShift},
			append([]byte{0x80}, bytes.Repeat([]byte{0x80}, 9)...)...), ErrBadLength},
		{"uvarint re-encodes an inline length",
			[]byte{flagPacked5 | lenEscape<<lenShift, 5, 0, 0, 0, 0, 0}, ErrBadLength},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := Decode(c.in); !errors.Is(err, c.want) {
				t.Errorf("got %v, want %v", err, c.want)
			}
		})
	}
}

func TestDecodeReservedSymbol(t *testing.T) {
	for idx := extReserved; idx < extEscape; idx++ {
		buf := packFrame([]uint8{'a' - 'a', opExt, uint8(idx)}, false)
		if _, _, err := Decode(buf); !errors.Is(err, ErrReservedSymbol) {
			t.Errorf("extTable[%d]: got %v, want ErrReservedSymbol", idx, err)
		}
	}
	// Every assigned index must decode instead.
	for idx := range extReserved {
		buf := packFrame([]uint8{opExt, uint8(idx)}, false)
		got, _, err := Decode(buf)
		if err != nil {
			t.Errorf("extTable[%d]: %v", idx, err)
		} else if got != extTable[idx] {
			t.Errorf("extTable[%d]: decoded %q, want %q", idx, got, extTable[idx])
		}
	}
	// symTable has no reserved entries: all 32 operands are characters.
	for idx := range 32 {
		buf := packFrame([]uint8{opSymbol, uint8(idx)}, false)
		got, _, err := Decode(buf)
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
		// A truncation has to be built on the unit grid, or packFrame's pad
		// supplies the very unit the case is meant to be missing.
		{"truncated bytes", []uint8{opExt, extEscape, 3, 1, 0}, ErrTruncated},
		{"truncated count", []uint8{0, opExt, extEscape}, ErrTruncated},
		{"truncated operand", []uint8{opExt}, ErrTruncated},
		{"truncated symbol operand", []uint8{opSymbol}, ErrTruncated},
		{"truncated number", []uint8{0, 0, opNumber, 1}, ErrTruncated},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := Decode(packFrame(c.units, false)); !errors.Is(err, c.want) {
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
		got, _, err := Decode(packFrame(units, false))
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
			got, _, err := Decode(packFrame(units, upper))
			if err != nil || got != string(want) {
				t.Fatalf("upper=%v n=%d: %q, %v", upper, n, got, err)
			}
		}
	}
}

func TestRawModeIgnoresCaseFlag(t *testing.T) {
	for _, flags := range []byte{0, flagUppercase} {
		buf := append([]byte{flags | 3<<lenShift}, "abc"...)
		got, n, err := Decode(buf)
		if err != nil || got != "abc" || n != 4 {
			t.Errorf("flags %#02x: %q, %d, %v", flags, got, n, err)
		}
	}
}

func TestDecodeIgnoresTrailingBytes(t *testing.T) {
	buf := Append(nil, "hello world")
	want := len(buf)
	buf = append(buf, "trailing garbage"...)
	got, n, err := Decode(buf)
	if err != nil || got != "hello world" || n != want {
		t.Errorf("%q, %d, %v; want %q, %d", got, n, err, "hello world", want)
	}
}

// TestDecodeTruncatedAtEveryPrefix walks every prefix of a valid frame. Each one
// must fail cleanly rather than return a short string.
func TestDecodeTruncatedAtEveryPrefix(t *testing.T) {
	for _, s := range []string{"hello", "el niño comió jamón", "SKU-4217-hola",
		"\x01\x02\x03\x04", strings.Repeat("ab ", 60)} {
		buf := Append(nil, s)
		for n := range len(buf) {
			got, _, err := Decode(buf[:n])
			if err == nil && got == s {
				t.Errorf("%q: prefix of %d bytes decoded in full", s, n)
			}
		}
	}
}

// TestDecodeBitFlipsDoNotPanic flips every bit of a set of valid frames. A
// corrupt frame may decode to anything or fail; it may not panic or read out of
// bounds, which the race and bounds checks catch.
func TestDecodeBitFlipsDoNotPanic(t *testing.T) {
	for _, s := range []string{"hello", "helloWorld", "el niño comió jamón",
		"SKU-4217-hola", "\x01\x02\x03\x04", "1023", strings.Repeat("ab ", 20)} {
		buf := Append(nil, s)
		for i := range buf {
			for bit := range 8 {
				bad := append([]byte(nil), buf...)
				bad[i] ^= 1 << bit
				_, _, _ = Decode(bad)
				_, _, _ = AppendDecoded(nil, bad)
			}
		}
	}
}

func TestDecodeGarbageDoesNotPanic(t *testing.T) {
	rng := rand.New(rand.NewPCG(21, 22))
	decoded, packedPath := 0, 0
	for range 200000 {
		buf := make([]byte, rng.IntN(40))
		for i := range buf {
			buf[i] = byte(rng.IntN(256))
		}
		if _, _, err := Decode(buf); err == nil {
			decoded++
			if len(buf) > 0 && buf[0]&flagPacked5 != 0 {
				packedPath++
			}
		}
	}
	t.Logf("%d of 200000 random buffers decoded, %d through the packed path",
		decoded, packedPath)
	if packedPath == 0 {
		t.Error("no random buffer exercised the packed path")
	}
}

// TestMaxDecodedLenIsSound is the bound Decode sizes its scratch from. If a
// payload could ever decode to more than this, that scratch would have to grow
// and the single-allocation claim would be wrong.
func TestMaxDecodedLenIsSound(t *testing.T) {
	// The densest token is an ext carrying the three-byte '€': two units for
	// three bytes. Build payloads made only of those.
	for pairs := 1; pairs <= 200; pairs++ {
		units := make([]uint8, 0, 2*pairs)
		for range pairs {
			units = append(units, opExt, uint8(extEuro))
		}
		buf := packFrame(units, false)
		h, err := frame(buf)
		if err != nil {
			t.Fatal(err)
		}
		got, _, err := Decode(buf)
		if err != nil {
			t.Fatal(err)
		}
		if bound := maxDecodedLen(len(h.payload)); len(got) > bound {
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
		buf := packFrame(units, false)
		h, err := frame(buf)
		if err != nil {
			continue
		}
		got, _, err := Decode(buf)
		if err != nil {
			continue
		}
		if bound := maxDecodedLen(len(h.payload)); len(got) > bound {
			t.Fatalf("units %v: decoded %d bytes, bound is %d", units, len(got), bound)
		}
	}
}

// TestDecodeExpansionIsBounded is the decompression-bomb check: a frame cannot
// name a payload larger than the buffer, so the work a peer can ask for is
// bounded by the bytes it sent.
func TestDecodeExpansionIsBounded(t *testing.T) {
	// A uvarint claiming a huge payload must be refused, not allocated for.
	buf := append([]byte{flagPacked5 | lenEscape<<lenShift}, 0xFF, 0xFF, 0xFF, 0xFF, 0x07)
	if _, _, err := Decode(buf); !errors.Is(err, ErrTruncated) {
		t.Errorf("oversized length: got %v, want ErrTruncated", err)
	}
	// And the honest bound holds for real frames.
	for _, s := range []string{"€€€€€€€€", "1023102310231023", strings.Repeat("ñ", 40)} {
		b := Append(nil, s)
		h, err := frame(b)
		if err != nil {
			t.Fatal(err)
		}
		if h.packed && len(s) > maxDecodedLen(len(h.payload)) {
			t.Errorf("%q: %d bytes from a %d-byte payload, bound is %d",
				s, len(s), len(h.payload), maxDecodedLen(len(h.payload)))
		}
	}
}

// TestAppendStringRejectsShortBuffer pins the embedded decoder's own bound.
func TestAppendStringRejectsShortBuffer(t *testing.T) {
	payload, n, upper, ok := AppendPayload(nil, "el niño comió jamón")
	if !ok {
		t.Fatal("expected the sample to pack")
	}
	if _, err := AppendString(nil, payload[:n-1], n, upper); !errors.Is(err, ErrTruncated) {
		t.Errorf("short buffer: got %v, want ErrTruncated", err)
	}
}

func FuzzDecode(f *testing.F) {
	for _, s := range []string{"", "a", "hello", "el niño comió jamón", "SKU-4217-hola",
		"\x01\x02\x03\x04", strings.Repeat("ab ", 40)} {
		f.Add(Append(nil, s))
	}
	f.Add([]byte{flagPacked5 | lenEscape<<lenShift, 0x80, 0x01})
	f.Fuzz(func(t *testing.T, buf []byte) {
		got, n, err := Decode(buf)
		if err != nil {
			return
		}
		if n > len(buf) {
			t.Fatalf("consumed %d of %d", n, len(buf))
		}
		// Whatever came out must be something the frame really said: re-encoding
		// it and decoding that again has to give the same string back.
		again, _, err := Decode(Append(nil, got))
		if err != nil || again != got {
			t.Fatalf("%q did not survive a re-encode: %q, %v", got, again, err)
		}
	})
}

func TestUvarintRoundtrip(t *testing.T) {
	for _, v := range []int{0, 1, 127, 128, 300, 16383, 16384, 1 << 20, maxLen} {
		buf := appendUvarint(nil, v)
		if got := uvarintLen(v); got != len(buf) {
			t.Errorf("%d: uvarintLen = %d, wrote %d", v, got, len(buf))
		}
		back, n, err := readUvarint(buf)
		if err != nil || back != v || n != len(buf) {
			t.Errorf("%d: got %d, %d, %v", v, back, n, err)
		}
	}
	if _, _, err := readUvarint(nil); !errors.Is(err, ErrTruncated) {
		t.Errorf("empty: got %v", err)
	}
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
