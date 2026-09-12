package stringpack

// What the last group costs, and whether a narrower one would cost less.
//
// The intuition to check: a stream that ends mid-group wastes the bits between
// the last unit and the byte boundary, so perhaps the tail should be packed as
// uint16 triples (3 units in 2 bytes) rather than continuing the uint64 shape,
// with a header bit to say which.
//
// Neither half survives measurement. A uint16 holds 3 units in 16 bits and so
// throws away a bit by construction; a uint64 holds 8 in 40 and throws away
// none. The triple *creates* the waste it was meant to avoid, and the dense tail
// is never larger — see TestDenseTailDominates. And no bit is needed either way:
// the payload byte length already pins the unit count at floor(8L/w), so the
// decoder derives the tail from what the frame already carries.

import "testing"

// tripleLen is what a tail of r units would cost packed as uint16 triples.
func tripleLen(r int) int { return (r + 2) / 3 * 2 }

// TestDenseTailDominates pins that continuing the dense 5-bit packing is never
// worse than switching to uint16 triples, for every possible tail length. This
// is why the format carries no tail-shape bit: there is no choice to make.
func TestDenseTailDominates(t *testing.T) {
	for r := 1; r <= 7; r++ {
		if d, u := packedLen5(r), tripleLen(r); d > u {
			t.Errorf("tail of %d units: dense %d bytes, uint16 %d", r, d, u)
		}
	}
	// And it is strictly better somewhere, so the two are not interchangeable.
	if packedLen5(1) >= tripleLen(1) {
		t.Error("expected dense to win outright on a one-unit tail")
	}
}

// TestTailWasteVsHeader reports where the bits actually go. It asserts only the
// ordering that matters — the header costs more than the tail rounding — and
// logs the rest, since the exact figures are corpus-dependent.
func TestTailWasteVsHeader(t *testing.T) {
	t.Logf("%-10s %8s %8s %10s %9s %8s", "corpus", "frames", "encoded", "tailwaste", "u16tail", "header")
	for _, kind := range corpusKinds {
		c, _ := corpus(kind)
		var payload, units, triples, frames, enc int
		for _, s := range c {
			u := u5Tokenize(nil, s)
			if len(u) > 0 && u[0] == u5CaseLong {
				u = u[1:]
			}
			l := packedLen5(len(u))
			payload += l
			units += l * 8 / 5 // the count the decoder derives, after grid padding
			triples += len(u)/8*5 + tripleLen(len(u)%8)
			frames++
			enc += len(u5bAppend(nil, s))
		}
		waste := payload*8 - units*5
		header := enc - payload
		t.Logf("%-10s %8d %8d %9.2f%% %+8.2f%% %7.2f%%", kind, frames, enc,
			100*float64(waste)/float64(enc*8),
			100*float64(triples-payload)/float64(enc),
			100*float64(header)/float64(enc))
		if waste/8 >= header {
			t.Errorf("%s: tail waste %d bits exceeds header cost %d bytes", kind, waste, header)
		}
		if triples < payload {
			t.Errorf("%s: uint16 tails would be smaller (%d vs %d)", kind, triples, payload)
		}
	}
}
