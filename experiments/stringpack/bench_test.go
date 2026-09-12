package stringpack

// Where the time actually goes, and what byte alignment is worth.
//
// Three questions, in the order they matter:
//
//  1. How much of packed5's encode time is the planning pass? BenchmarkPlanSplit
//     runs packed5.Size, which plans and then throws the plan away, against
//     packed5.Append, which plans and writes. The gap is the writing.
//  2. What does the packing kernel itself cost? BenchmarkPack and BenchmarkUnpack
//     in kernel_test.go price dense, wide64 and triple16 on the same units, with
//     no tokeniser in the way.
//  3. Does any of it survive end to end? BenchmarkEncode and BenchmarkDecode run
//     the whole codecs over packed5's own corpora and report the same ratio and
//     ms/MB figures its README quotes.

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/ivanjoz/colbin/packed5"
)

// corpus is packed5's, verbatim, so the numbers line up with its README.
func corpus(kind string) ([]string, int) {
	rng := rand.New(rand.NewPCG(42, 43))
	words := []string{"hola", "mundo", "producto", "cliente", "factura", "norte", "sur",
		"Lima", "Bogota", "Santiago", "activo", "pendiente", "azul", "rojo"}
	word := func() string { return words[rng.IntN(len(words))] }
	var out []string
	total := 0
	for total < 1<<20 {
		var s string
		switch kind {
		case "name":
			s = word() + " " + word()
		case "sku":
			s = fmt.Sprintf("SKU-%04d-%s", rng.IntN(10000), word())
		case "spanish":
			s = "el niño comió jamón " + word()
		case "sentence":
			s = word() + " " + word() + " " + word() + " " + word() + " " + word()
		case "paragraph":
			s = strings.Repeat("the quick brown fox jumps over the lazy dog ", 6)
		}
		out = append(out, s)
		total += len(s)
	}
	return out, total
}

var corpusKinds = []string{"name", "sku", "spanish", "sentence", "paragraph"}

func msPerMB(b *testing.B, bytes int) {
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(bytes)*(1<<20)/1e6, "ms/MB")
}

// codec is one encoder/decoder pair under test.
type codec struct {
	name   string
	encode func([]byte, string) []byte
	decode func([]byte, []byte) ([]byte, int, error)
}

var codecs = []codec{
	{"packed5", packed5.Append, packed5.AppendDecoded},
	{"u5", u5Append, u5Decode},
	{"u5fused", u5AppendFused, u5Decode},
	{"u5b", u5bAppend, u5bDecode},
	{"u5btab", u5bAppendTab, u5bDecode},
	{"p6", p6Append, p6Decode},
}

// awkward strings that exercise the paths the corpora do not.
var edgeCases = []string{
	"", "a", "A", "Z", " ", "  ", "0", "00", "007", "1023", "1024", "99999",
	"SKU-0042-hola", "Lima norte", "HELLO WORLD", "McDonald's", "aB", "AbC",
	"el niño comió jamón", "€100", "¿qué?", "a\x00b", "\xff\xfe", "\xc3",
	"tab\there", "line\nbreak", "mixed 123 ABC xyz -_-",
	strings.Repeat("x", 40), strings.Repeat("Ñ", 20), strings.Repeat("ab 12 ", 30),
}

func allStrings() []string {
	out := slices.Clone(edgeCases)
	for _, kind := range corpusKinds {
		c, _ := corpus(kind)
		out = append(out, c[:min(len(c), 400)]...)
	}
	return out
}

func TestCodecsRoundTrip(t *testing.T) {
	inputs := allStrings()
	for _, c := range codecs {
		if c.name == "packed5" {
			continue // its own package already pins this
		}
		t.Run(c.name, func(t *testing.T) {
			for _, s := range inputs {
				buf := c.encode(nil, s)
				buf = append(buf, make([]byte, slack)...) // reader slack
				got, n, err := c.decode(nil, buf)
				if err != nil {
					t.Fatalf("%q: %v", s, err)
				}
				if n != len(buf)-slack {
					t.Fatalf("%q: consumed %d of %d", s, n, len(buf)-slack)
				}
				if string(got) != s {
					t.Fatalf("%q: round trip gave %q", s, got)
				}
			}
		})
	}
}

// TestCodecsBackToBack decodes a whole run of frames out of one buffer, which
// is the shape a column actually has and the one that supplies reader slack
// from the following frame rather than from padding.
func TestCodecsBackToBack(t *testing.T) {
	inputs := allStrings()
	for _, c := range codecs[1:] {
		t.Run(c.name, func(t *testing.T) {
			var enc []byte
			for _, s := range inputs {
				enc = c.encode(enc, s)
			}
			total := len(enc)
			enc = append(enc, make([]byte, slack)...)
			var out []byte
			var bounds []int
			for p := 0; p < total; {
				var n int
				var err error
				out, n, err = c.decode(out, enc[p:])
				if err != nil {
					t.Fatal(err)
				}
				bounds = append(bounds, len(out))
				p += n
			}
			if len(bounds) != len(inputs) {
				t.Fatalf("decoded %d frames, want %d", len(bounds), len(inputs))
			}
			prev := 0
			for i, end := range bounds {
				if string(out[prev:end]) != inputs[i] {
					t.Fatalf("frame %d: got %q want %q", i, out[prev:end], inputs[i])
				}
				prev = end
			}
		})
	}
}

// TestU5FusedMatches pins the two u5 encoders against each other: fusing the
// packer into the tokeniser must be a pure speed change.
func TestU5FusedMatches(t *testing.T) {
	for _, s := range allStrings() {
		a, b := u5Append(nil, s), u5AppendFused(nil, s)
		if string(a) != string(b) {
			t.Fatalf("%q: fused %x != staged %x", s, b, a)
		}
	}
}

// TestU5BTabMatches pins the table-driven classifier against the comparison
// chain. They must agree bit for bit, so the benchmark gap between u5b and
// u5btab is classification and nothing else.
func TestU5BTabMatches(t *testing.T) {
	for _, s := range allStrings() {
		a, b := u5bAppend(nil, s), u5bAppendTab(nil, s)
		if string(a) != string(b) {
			t.Fatalf("%q: table %x != chain %x", s, b, a)
		}
	}
}

// TestNeverInflates holds every codec to packed5's guarantee.
func TestNeverInflates(t *testing.T) {
	for _, c := range codecs {
		for _, s := range allStrings() {
			if got, want := len(c.encode(nil, s)), len(s)+frameOverhead(len(s)); got > want {
				t.Fatalf("%s: %q encoded to %d bytes, raw frame is %d", c.name, s, got, want)
			}
		}
	}
}

func BenchmarkEncode(b *testing.B) {
	for _, c := range codecs {
		for _, kind := range corpusKinds {
			in, n := corpus(kind)
			b.Run(c.name+"/"+kind, func(b *testing.B) {
				out := make([]byte, 0, 2<<20)
				b.SetBytes(int64(n))
				b.ReportAllocs()
				for b.Loop() {
					out = out[:0]
					for _, s := range in {
						out = c.encode(out, s)
					}
				}
				b.ReportMetric(float64(len(out))/float64(n), "ratio")
				msPerMB(b, n)
			})
		}
	}
}

func BenchmarkDecode(b *testing.B) {
	for _, c := range codecs {
		for _, kind := range corpusKinds {
			in, n := corpus(kind)
			var enc []byte
			for _, s := range in {
				enc = c.encode(enc, s)
			}
			enc = append(enc, make([]byte, slack)...)
			total := len(enc) - slack
			b.Run(c.name+"/"+kind, func(b *testing.B) {
				out := make([]byte, 0, 2<<20)
				b.SetBytes(int64(n))
				b.ReportAllocs()
				for b.Loop() {
					out = out[:0]
					for p := 0; p < total; {
						var k int
						var err error
						out, k, err = c.decode(out, enc[p:])
						if err != nil {
							b.Fatal(err)
						}
						p += k
					}
				}
				msPerMB(b, n)
			})
		}
	}
}

// BenchmarkPlanSplit separates packed5's two passes. Size is plan-only; Append
// is plan plus write. u5tokenize is u5's single classification pass with the
// units thrown away, for scale.
func BenchmarkPlanSplit(b *testing.B) {
	for _, kind := range corpusKinds {
		in, n := corpus(kind)
		b.Run("packed5.Size/"+kind, func(b *testing.B) {
			b.SetBytes(int64(n))
			for b.Loop() {
				sink := 0
				for _, s := range in {
					sink += packed5.Size(s)
				}
				sizeSink = sink
			}
			msPerMB(b, n)
		})
		b.Run("packed5.Append/"+kind, func(b *testing.B) {
			out := make([]byte, 0, 2<<20)
			b.SetBytes(int64(n))
			for b.Loop() {
				out = out[:0]
				for _, s := range in {
					out = packed5.Append(out, s)
				}
			}
			msPerMB(b, n)
		})
		b.Run("u5.tokenize/"+kind, func(b *testing.B) {
			units := make([]uint8, 0, 1<<12)
			b.SetBytes(int64(n))
			for b.Loop() {
				for _, s := range in {
					units = u5Tokenize(units[:0], s)
				}
			}
			unitSink = len(units)
			msPerMB(b, n)
		})
	}
}

var (
	sizeSink int
	unitSink int
)
