package packed5

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// roundtrip encodes s, decodes it back, and checks every invariant the frame is
// supposed to hold: byte-exact recovery, an exact consumed-byte count, Size
// agreeing with Append, and the never-inflate guarantee.
func roundtrip(t *testing.T, s string) []byte {
	t.Helper()
	buf := Append(nil, s)
	got, n, err := Decode(buf)
	if err != nil {
		t.Fatalf("Decode(%q): %v (frame %x)", s, err, buf)
	}
	if got != s {
		t.Fatalf("roundtrip %q -> %q (frame %x)", s, got, buf)
	}
	if n != len(buf) {
		t.Fatalf("%q: consumed %d of %d bytes", s, n, len(buf))
	}
	if want := Size(s); want != len(buf) {
		t.Fatalf("%q: Size = %d, Append wrote %d", s, want, len(buf))
	}
	if max := frameOverhead(len(s)) + len(s); len(buf) > max {
		t.Fatalf("%q: frame %d bytes, raw framing would be %d", s, len(buf), max)
	}
	return buf
}

func TestRoundtripTable(t *testing.T) {
	cases := []struct{ name, in string }{
		{"empty", ""},
		{"single lower", "a"},
		{"single upper", "A"},
		{"single space", " "},
		{"single digit", "7"},
		{"single dash", "-"},
		{"alphabet", "abcdefghijklmnopqrstuvwxyz"},
		{"alphabet upper", "ABCDEFGHIJKLMNOPQRSTUVWXYZ"},
		{"hello", "hello"},
		{"spec camel", "helloWorld"},
		{"spec long run", "helloWORLDagain"},
		{"spec mixed", "fooBARTest"},
		{"spec product", "product123"},
		{"words", "the quick brown fox jumps over the lazy dog"},
		{"words upper", "THE QUICK BROWN FOX JUMPS OVER THE LAZY DOG"},
		{"title case", "The Quick Brown Fox"},
		{"leading zeros", "00123"},
		{"leading zero pair", "007"},
		{"zero", "0"},
		{"number boundary 1023", "1023"},
		{"number boundary 1024", "1024"},
		{"long number", "9876543210"},
		{"all symbols", `<>/"'%#|()!?$~` + "`" + `€@\[]^{}_`},
		{"spanish", "ñáéíóú"},
		{"spanish words", "el niño comió jamón"},
		{"spanish upper", "EL NIÑO COMIÓ JAMÓN"},
		{"simple symbols", "0123456789.-+*="},
		{"email", "user.name@example.com"},
		{"url", "https://example.com/path?q=1"},
		{"html", `<div class="x">hi</div>`},
		{"json", `{"id":1023,"name":"ana"}`},
		{"sku", "SKU-00042-XL"},
		{"price", "€1023.45"},
		{"cjk", "日本語"},
		{"emoji", "hello 🌍"},
		{"mixed unicode", "café ñandú 日本"},
		{"invalid utf8", "\xff\xfe\x00"},
		{"lone continuation", "\x80"},
		{"truncated rune", "\xc3"},
		{"truncated euro", "\xe2\x82"},
		{"nul bytes", "\x00\x00\x00\x00"},
		{"control chars", "\x01\x02\x03\x04\x05"},
		{"tab newline", "a\tb\nc"},
		{"repeated space", "          "},
		{"one of each case", "aAaAaAaA"},
		{"two run", "aaBBaa"},
		{"three run", "aaBBBaa"},
		{"case across symbol", "aa-BB-aa"},
		{"trailing toggle", "abcDEF"},
		{"leading toggle", "ABCdef"},
		{"digits and letters", "a1b2c3d4"},
		{"long digits", "12345678901234567890"},
		{"long text", strings.Repeat("the quick brown fox ", 20)},
		{"long upper", strings.Repeat("QUICK BROWN FOX ", 20)},
		{"long unicode", strings.Repeat("ñáéíóú", 40)},
		{"long binary", strings.Repeat("\xff\x00\xfe", 60)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { roundtrip(t, tc.in) })
	}
}

// TestRoundtripEverySingleByte covers all 256 one-byte inputs, which exercises
// every branch of the single-character edge set including invalid UTF-8.
func TestRoundtripEverySingleByte(t *testing.T) {
	for b := range 256 {
		roundtrip(t, string([]byte{byte(b)}))
	}
}

// TestRoundtripEveryBytePair is exhaustive over all 65536 two-byte inputs. Two
// bytes is where the case-toggle choice, the two-digit NUMBER_0_1023 token and
// the two-byte escape run all first become reachable.
func TestRoundtripEveryBytePair(t *testing.T) {
	var buf [2]byte
	for a := range 256 {
		buf[0] = byte(a)
		for b := range 256 {
			buf[1] = byte(b)
			s := string(buf[:])
			out := Append(nil, s)
			got, n, err := Decode(out)
			if err != nil || got != s || n != len(out) {
				t.Fatalf("pair %02x %02x: got %q n=%d err=%v (frame %x)", a, b, got, n, err, out)
			}
		}
	}
}

// alphabet is a representative slice of the input space: both cases, digits,
// space, table-29 symbols, table-30 symbols, an accented letter, a three-byte
// rune and an invalid byte.
var alphabet = []string{
	"a", "b", "z", "A", "B", "Z", " ", "0", "1", "9",
	".", "-", "+", "*", "=", "<", "/", "@", "_", "%",
	"ñ", "á", "€", "日", "\xff", "\x00",
}

// TestRoundtripEveryTriple is exhaustive over all three-symbol combinations of
// alphabet: 26^3 = 17576 strings of mixed byte widths.
func TestRoundtripEveryTriple(t *testing.T) {
	for _, x := range alphabet {
		for _, y := range alphabet {
			for _, z := range alphabet {
				s := x + y + z
				out := Append(nil, s)
				got, n, err := Decode(out)
				if err != nil || got != s || n != len(out) {
					t.Fatalf("%q: got %q n=%d err=%v (frame %x)", s, got, n, err, out)
				}
			}
		}
	}
}

// randString builds a string of n symbols drawn from pool.
func randString(rng *rand.Rand, pool []string, n int) string {
	var b strings.Builder
	for range n {
		b.WriteString(pool[rng.IntN(len(pool))])
	}
	return b.String()
}

func TestRoundtripRandom(t *testing.T) {
	pools := map[string][]string{
		"alphabet": alphabet,
		"letters":  {"a", "b", "c", "X", "Y", "Z"},
		"digits":   {"0", "1", "2", "9"},
		"symbols":  {"<", ">", "/", "@", "€", "ñ", "-", "."},
		"words":    {"hola", " ", "mundo", "Test", "123", "-", "ABC"},
	}
	rng := rand.New(rand.NewPCG(1, 2))
	for name, pool := range pools {
		t.Run(name, func(t *testing.T) {
			for range 2000 {
				roundtrip(t, randString(rng, pool, rng.IntN(40)))
			}
		})
	}
}

func TestRoundtripRandomBytes(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	buf := make([]byte, 200)
	for range 3000 {
		n := rng.IntN(len(buf) + 1)
		for i := range n {
			buf[i] = byte(rng.UintN(256))
		}
		roundtrip(t, string(buf[:n]))
	}
}

func TestRoundtripRandomRunes(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 6))
	var b strings.Builder
	for range 2000 {
		b.Reset()
		for range rng.IntN(30) {
			r := rune(rng.IntN(0x10000))
			if !utf8.ValidRune(r) {
				r = 'x'
			}
			b.WriteRune(r)
		}
		roundtrip(t, b.String())
	}
}

// TestRoundtripAllLengths walks every length across the stack/heap boundary and
// both length-prefix forms, for several character mixes.
func TestRoundtripAllLengths(t *testing.T) {
	units := []string{"a", "aB", "a1", "ñ", "x-", "\xff", "abc def "}
	for _, u := range units {
		for n := range 400 {
			s := strings.Repeat(u, n)
			if len(s) > 600 {
				break
			}
			roundtrip(t, s)
		}
	}
}

// TestLengthPrefixForms pins the two length encodings: inline in the header's
// five length bits up to 30 payload bytes, then a uvarint, growing to two bytes
// past 127.
func TestLengthPrefixForms(t *testing.T) {
	for _, tc := range []struct {
		n          int
		overInline bool
	}{{1, false}, {14, false}, {30, false}, {31, true}, {32, true}, {127, true}, {128, true}, {1000, true}} {
		s := strings.Repeat("\xff", tc.n) // never packs, so payload == n
		buf := roundtrip(t, s)
		if got := buf[0]>>lenShift == lenEscape; got != tc.overInline {
			t.Fatalf("n=%d: escape length prefix = %v, want %v", tc.n, got, tc.overInline)
		}
		if want := frameOverhead(tc.n) + tc.n; len(buf) != want {
			t.Fatalf("n=%d: frame %d bytes, want %d", tc.n, len(buf), want)
		}
	}
}

// TestAppendPreservesPrefix checks Append is a true appender and that frames
// read back one after another from a shared buffer.
func TestAppendPreservesPrefix(t *testing.T) {
	inputs := []string{"hello", "", "WORLD", "a-b-c", "ñandú", "\xff\xfe", "product123", strings.Repeat("z", 300)}
	prefix := []byte("PREFIX")
	buf := append([]byte(nil), prefix...)
	for _, s := range inputs {
		buf = Append(buf, s)
	}
	if !bytes.Equal(buf[:len(prefix)], prefix) {
		t.Fatalf("prefix clobbered: %q", buf[:len(prefix)])
	}
	rest := buf[len(prefix):]
	for _, want := range inputs {
		got, n, err := Decode(rest)
		if err != nil {
			t.Fatalf("%q: %v", want, err)
		}
		if got != want {
			t.Fatalf("got %q want %q", got, want)
		}
		rest = rest[n:]
	}
	if len(rest) != 0 {
		t.Fatalf("%d bytes left over", len(rest))
	}
}

// TestNeverInflates is the headline guarantee: a frame never costs more than
// the raw bytes plus their framing, whatever the input.
func TestNeverInflates(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 8))
	check := func(s string) {
		t.Helper()
		if got, max := Size(s), frameOverhead(len(s))+len(s); got > max {
			t.Fatalf("%q: %d bytes vs raw %d", s, got, max)
		}
	}
	for _, pool := range [][]string{alphabet, {"日", "🌍", "\xff"}, {"a", "A"}} {
		for range 3000 {
			check(randString(rng, pool, rng.IntN(50)))
		}
	}
	for n := range 300 {
		check(strings.Repeat("\x01", n))
	}
}

// TestNoAllocations pins the encoder's stack-only path for short strings: with
// room in the output slice, Append must not touch the heap.
func TestNoAllocations(t *testing.T) {
	for _, s := range []string{"hello", "helloWorld", "product123", "el niño comió jamón",
		strings.Repeat("ab", stackLimit/2)} {
		out := make([]byte, 0, 1024)
		got := testing.AllocsPerRun(100, func() {
			out = Append(out[:0], s)
		})
		if got != 0 {
			t.Errorf("Append(%q): %.1f allocs, want 0", s, got)
		}
		if _, _, err := Decode(out); err != nil {
			t.Fatalf("%q: %v", s, err)
		}
	}
}

func TestSizeMatchesAppendExhaustively(t *testing.T) {
	rng := rand.New(rand.NewPCG(9, 10))
	for range 5000 {
		s := randString(rng, alphabet, rng.IntN(30))
		if got, want := Size(s), len(Append(nil, s)); got != want {
			t.Fatalf("%q: Size = %d, Append = %d", s, got, want)
		}
	}
}

// TestDecodeDoesNotAliasInput mutates the source buffer after decoding; the
// returned string must be unaffected in both raw and packed mode.
func TestDecodeDoesNotAliasInput(t *testing.T) {
	for _, s := range []string{"hello world", "\xff\xfe\xfd"} {
		buf := Append(nil, s)
		got, _, err := Decode(buf)
		if err != nil {
			t.Fatal(err)
		}
		for i := range buf {
			buf[i] = 0x5A
		}
		if got != s {
			t.Fatalf("decoded string aliased the buffer: %q", got)
		}
	}
}

func FuzzRoundtrip(f *testing.F) {
	for _, s := range []string{"", "a", "helloWorld", "product123", "00123", "ñandú",
		"<a href=\"x\">", "\xff\xfe", "THE QUICK BROWN", strings.Repeat("x", 300)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		buf := Append(nil, s)
		got, n, err := Decode(buf)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if got != s {
			t.Fatalf("roundtrip %q -> %q", s, got)
		}
		if n != len(buf) {
			t.Fatalf("consumed %d of %d", n, len(buf))
		}
		if max := frameOverhead(len(s)) + len(s); len(buf) > max {
			t.Fatalf("inflated %q: %d > %d", s, len(buf), max)
		}
		if Size(s) != len(buf) {
			t.Fatalf("Size %d != %d", Size(s), len(buf))
		}
	})
}

// TestSizeReport is informational: it prints the packed size against the raw
// byte length for representative inputs.
func TestSizeReport(t *testing.T) {
	samples := []string{
		"hello", "helloWorld", "fooBARTest", "product123", "SKU-00042-XL",
		"el niño comió jamón", "the quick brown fox", "THE QUICK BROWN FOX",
		"user.name@example.com", `{"id":1023,"name":"ana"}`, "€1023.45",
		"Bogotá", "Móvil Samsung Galaxy S23", "Factura 2024-1023",
	}
	t.Log(" raw  enc  ratio  flags  input")
	for _, s := range samples {
		buf := Append(nil, s)
		flags := "raw"
		if buf[0]&flagPacked5 != 0 {
			flags = "p5"
			if buf[0]&flagUppercase != 0 {
				flags += "+U"
			}
			if buf[0]&flagNumber != 0 {
				flags += "+N"
			}
		}
		t.Log(fmt.Sprintf("%4d %4d  %.2f  %-6s %q", len(s), len(buf),
			float64(len(buf))/float64(max(len(s), 1)), flags, s))
	}
}

func BenchmarkAppend(b *testing.B) {
	for _, s := range []string{"hello", "helloWorld", "product123", "el niño comió jamón",
		"the quick brown fox jumps over the lazy dog"} {
		b.Run(fmt.Sprintf("len%d", len(s)), func(b *testing.B) {
			out := make([]byte, 0, 256)
			b.ReportAllocs()
			for b.Loop() {
				out = Append(out[:0], s)
			}
		})
	}
}

func BenchmarkDecode(b *testing.B) {
	for _, s := range []string{"hello", "helloWorld", "product123", "el niño comió jamón",
		"the quick brown fox jumps over the lazy dog"} {
		buf := Append(nil, s)
		b.Run(fmt.Sprintf("len%d", len(s)), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, _, err := Decode(buf); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// TestConcurrentUse exercises the long-string scratch pool from many goroutines
// at once. Run under -race, this is what says the pooled buffers are never
// shared between two in-flight encodes.
func TestConcurrentUse(t *testing.T) {
	inputs := []string{
		strings.Repeat("the quick brown fox jumps over the lazy dog ", 6),
		strings.Repeat("el niño comió jamón en Bogotá ", 9),
		strings.Repeat("SKU-1023-XL ", 20),
		strings.Repeat("\xff\x00 mixed Case 42 ", 15),
		"short",
	}
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 500 {
				s := inputs[(g+i)%len(inputs)]
				got, n, err := Decode(Append(nil, s))
				if err != nil || got != s {
					t.Errorf("goroutine %d: %v (n=%d)", g, err, n)
					return
				}
				if Size(s) != len(Append(nil, s)) {
					t.Errorf("goroutine %d: Size disagrees with Append", g)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// corpus builds roughly a megabyte of short strings of one shape, which is the
// workload this codec is actually for.
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

// msPerMB converts the benchmark's own timing into the throughput figure the
// README quotes.
func msPerMB(b *testing.B, bytes int) {
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(bytes)*(1<<20)/1e6, "ms/MB")
}

func BenchmarkThroughputEncode(b *testing.B) {
	for _, kind := range corpusKinds {
		c, n := corpus(kind)
		b.Run(kind, func(b *testing.B) {
			out := make([]byte, 0, 2<<20)
			b.SetBytes(int64(n))
			b.ReportAllocs()
			for b.Loop() {
				out = out[:0]
				for _, s := range c {
					out = Append(out, s)
				}
			}
			b.ReportMetric(float64(len(out))/float64(n), "ratio")
			msPerMB(b, n)
		})
	}
}

func BenchmarkThroughputDecode(b *testing.B) {
	for _, kind := range corpusKinds {
		c, n := corpus(kind)
		var enc []byte
		for _, s := range c {
			enc = Append(enc, s)
		}
		b.Run(kind, func(b *testing.B) {
			b.SetBytes(int64(n))
			b.ReportAllocs()
			for b.Loop() {
				for rest := enc; len(rest) > 0; {
					_, k, err := Decode(rest)
					if err != nil {
						b.Fatal(err)
					}
					rest = rest[k:]
				}
			}
			msPerMB(b, n)
		})
	}
}
