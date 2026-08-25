package varint

import (
	"math"
	"math/bits"
	"math/rand/v2"
	"testing"
)

// ---------------------------------------------------------------- primitives

func TestCapBitsFormula(t *testing.T) {
	for k := minK; k <= maxK; k++ {
		for _, d := range dCodes {
			m := k + d
			for l := k; l <= m; l++ {
				got := capBits(l, k, m)
				var want uint8
				if l == m {
					want = 7*m + k // both ends flag-free
				} else {
					want = 8*(k-1) + 7*(l-k+1) // k-1 leading flag-free bytes
				}
				if got != want {
					t.Fatalf("cap(l=%d,k=%d,m=%d) = %d, want %d", l, k, m, got, want)
				}
				// A byte can never carry more than 8 payload bits.
				if got > 8*l {
					t.Fatalf("cap(l=%d,k=%d,m=%d) = %d exceeds %d physical bits", l, k, m, got, 8*l)
				}
			}
		}
	}
}

func TestCapBitsMonotonic(t *testing.T) {
	for k := minK; k <= maxK; k++ {
		for _, d := range dCodes {
			m := k + d
			for l := k + 1; l <= m; l++ {
				if capBits(l, k, m) <= capBits(l-1, k, m) {
					t.Fatalf("cap not increasing at l=%d (k=%d,m=%d): %d <= %d",
						l, k, m, capBits(l, k, m), capBits(l-1, k, m))
				}
			}
		}
	}
}

// encLen must return the *smallest* length that fits, and 0 only when the
// value genuinely exceeds cap(m).
func TestEncLenIsTight(t *testing.T) {
	for k := minK; k <= maxK; k++ {
		for _, d := range dCodes {
			m := k + d
			for b := 0; b <= 64; b++ {
				nb := uint8(b)
				l := encLen(nb, k, m)
				if l == 0 {
					if nb <= capBits(m, k, m) {
						t.Fatalf("k=%d m=%d bits=%d: reported no fit but cap(m)=%d", k, m, b, capBits(m, k, m))
					}
					continue
				}
				if l < k || l > m {
					t.Fatalf("k=%d m=%d bits=%d: length %d out of range", k, m, b, l)
				}
				if nb > capBits(l, k, m) {
					t.Fatalf("k=%d m=%d bits=%d: length %d holds only %d bits", k, m, b, l, capBits(l, k, m))
				}
				if l > k && nb <= capBits(l-1, k, m) {
					t.Fatalf("k=%d m=%d bits=%d: used %d bytes but %d would fit", k, m, b, l, l-1)
				}
			}
		}
	}
}

func TestEncLenMonotonicInBits(t *testing.T) {
	for k := minK; k <= maxK; k++ {
		for _, d := range dCodes {
			m := k + d
			prev := uint8(0)
			for b := 0; b <= 64; b++ {
				l := encLen(uint8(b), k, m)
				if l == 0 {
					break
				}
				if l < prev {
					t.Fatalf("k=%d m=%d: length shrank from %d to %d at bits=%d", k, m, prev, l, b)
				}
				prev = l
			}
		}
	}
}

func TestZigzagRoundtrip(t *testing.T) {
	vals := []int64{0, 1, -1, 2, -2, 63, -64, math.MaxInt64, math.MinInt64,
		math.MaxInt64 - 1, math.MinInt64 + 1, 1 << 40, -(1 << 40)}
	for _, v := range vals {
		if got := unzigzag(zigzag(v)); got != v {
			t.Fatalf("zigzag roundtrip %d -> %d", v, got)
		}
	}
	// Small magnitudes must stay small: |v| <= 2^62 keeps the encoding short.
	if zigzag(-1) != 1 || zigzag(1) != 2 || zigzag(0) != 0 {
		t.Fatalf("zigzag ordering broken: 0->%d 1->%d -1->%d", zigzag(0), zigzag(1), zigzag(-1))
	}
}

// ------------------------------------------------------------ varint codec

// checkKM round-trips one value and verifies its encoded length matches
// the model that the parameter search relies on.
func checkKM(t *testing.T, v uint64, k, m uint8) {
	t.Helper()
	want := encLen(uint8(bits.Len64(v)), k, m)
	if want == 0 {
		return // not representable under these params; the search would reject them
	}
	buf := appendKM(nil, v, k, m)
	if len(buf) != int(want) {
		t.Fatalf("v=%d k=%d m=%d: wrote %d bytes, model says %d", v, k, m, len(buf), want)
	}
	got, pos, err := getKM(buf, 0, k, m)
	if err != nil {
		t.Fatalf("v=%d k=%d m=%d: decode error %v", v, k, m, err)
	}
	if got != v {
		t.Fatalf("v=%d k=%d m=%d: decoded %d", v, k, m, got)
	}
	if pos != len(buf) {
		t.Fatalf("v=%d k=%d m=%d: consumed %d of %d bytes", v, k, m, pos, len(buf))
	}
}

func TestKMRoundtripSmallExhaustive(t *testing.T) {
	for k := minK; k <= maxK; k++ {
		for _, d := range dCodes {
			m := k + d
			for v := range uint64(20000) {
				checkKM(t, v, k, m)
			}
		}
	}
}

// Every capacity boundary is a potential off-by-one: test the exact value that
// fills a length, the first that overflows it, and the neighbours of 2^b.
func TestKMRoundtripBoundaries(t *testing.T) {
	for k := minK; k <= maxK; k++ {
		for _, d := range dCodes {
			m := k + d
			for l := k; l <= m; l++ {
				c := capBits(l, k, m)
				if c >= 64 {
					checkKM(t, math.MaxUint64, k, m)
					continue
				}
				full := uint64(1)<<c - 1 // largest value fitting in l bytes
				checkKM(t, full, k, m)
				checkKM(t, full-1, k, m)
				checkKM(t, full+1, k, m) // first value needing l+1
			}
			for b := 0; b <= 64; b++ {
				var v uint64
				if b == 64 {
					v = math.MaxUint64
				} else {
					v = uint64(1) << b
				}
				checkKM(t, v, k, m)
				checkKM(t, v-1, k, m)
				checkKM(t, v+1, k, m)
			}
		}
	}
}

func TestKMRoundtripRandom(t *testing.T) {
	r := rand.New(rand.NewPCG(0x5eed, 0xf00d))
	for k := minK; k <= maxK; k++ {
		for _, d := range dCodes {
			m := k + d
			for range 20000 {
				nb := r.UintN(65)
				var v uint64
				if nb > 0 {
					v = r.Uint64() >> (64 - nb)
				}
				checkKM(t, v, k, m)
			}
		}
	}
}

// Consecutive values must be independently framed: decoding a stream returns
// each one and lands exactly on the next.
func TestKMStreamFraming(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 11))
	for k := minK; k <= maxK; k++ {
		for _, d := range dCodes {
			m := k + d
			capBits := capBits(m, k, m)
			vals := make([]uint64, 200)
			var buf []byte
			for i := range vals {
				nb := r.UintN(uint(min(capBits, 64)) + 1)
				if nb > 0 {
					vals[i] = r.Uint64() >> (64 - nb)
				}
				if encLen(uint8(bits.Len64(vals[i])), k, m) == 0 {
					vals[i] = 0
				}
				buf = appendKM(buf, vals[i], k, m)
			}
			pos := 0
			for i, want := range vals {
				got, p, err := getKM(buf, pos, k, m)
				if err != nil {
					t.Fatalf("k=%d m=%d idx=%d: %v", k, m, i, err)
				}
				if got != want {
					t.Fatalf("k=%d m=%d idx=%d: got %d want %d", k, m, i, got, want)
				}
				pos = p
			}
			if pos != len(buf) {
				t.Fatalf("k=%d m=%d: stream ended at %d of %d", k, m, pos, len(buf))
			}
		}
	}
}

func TestKMTruncated(t *testing.T) {
	for k := minK; k <= maxK; k++ {
		for _, d := range dCodes {
			m := k + d
			v := uint64(1)<<min(capBits(m, k, m), 63) - 1
			buf := appendKM(nil, v, k, m)
			for cut := range buf {
				if _, _, err := getKM(buf[:cut], 0, k, m); err == nil {
					t.Fatalf("k=%d m=%d: truncation to %d/%d bytes not detected", k, m, cut, len(buf))
				}
			}
		}
	}
}
