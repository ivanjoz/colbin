package packed5

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf8"
)

// slackBytes is eight bytes of garbage to put after a payload. 0xFF decodes to
// units of every kind, so a reader that took any of it as payload would show.
const slackBytes = "\xff\xff\xff\xff\xff\xff\xff\xff"

// roundtrip packs s and reads it back twice — with slack after the payload, the
// way a container hands it over, and with the buffer ending exactly at it, which
// sends the last group down the tail path — and checks that the packed form is
// only used when it is smaller than s.
func roundtrip(t *testing.T, s string) {
	t.Helper()
	buf, n, upper, ok := AppendPayload(nil, s)
	if !ok {
		if len(buf) != 0 {
			t.Fatalf("%q: did not pack, but appended %d bytes", s, len(buf))
		}
		return
	}
	if n != len(buf) || n >= len(s) {
		t.Fatalf("%q: payload %d bytes in a %d-byte buffer, raw is %d", s, n, len(buf), len(s))
	}
	exact := bytes.Clone(buf)
	slack := append(bytes.Clone(buf), slackBytes...)
	for _, src := range [][]byte{exact, slack} {
		got, err := AppendString(nil, src, n, upper)
		if err != nil || string(got) != s {
			t.Fatalf("%q: decoded %q, %v (payload %x upper=%v, %d bytes of slack)",
				s, got, err, buf, upper, len(src)-n)
		}
	}
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
// bytes is where the case-toggle choice, the two-digit number token and the
// two-byte escape run all first become reachable.
func TestRoundtripEveryBytePair(t *testing.T) {
	var buf [2]byte
	for a := range 256 {
		buf[0] = byte(a)
		for b := range 256 {
			buf[1] = byte(b)
			roundtrip(t, string(buf[:]))
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
				roundtrip(t, x+y+z)
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

// pool is a named set of symbols for randString. Tests range over a slice of
// them and seed one generator per pool, so every pool sees the same strings on
// every run, whatever else the test does.
type pool struct {
	name    string
	symbols []string
}

func TestRoundtripRandom(t *testing.T) {
	pools := []pool{
		{"alphabet", alphabet},
		{"letters", []string{"a", "b", "c", "X", "Y", "Z"}},
		{"digits", []string{"0", "1", "2", "9"}},
		{"symbols", []string{"<", ">", "/", "@", "€", "ñ", "-", "."}},
		{"words", []string{"hola", " ", "mundo", "Test", "123", "-", "ABC"}},
	}
	for i, p := range pools {
		t.Run(p.name, func(t *testing.T) {
			rng := rand.New(rand.NewPCG(1, uint64(i)))
			for range 2000 {
				roundtrip(t, randString(rng, p.symbols, rng.IntN(40)))
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

// TestRoundtripAllLengths walks every length up to 600 bytes for several
// character mixes, so every position of the final group is the last one.
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

// TestPayloadsBackToBack lays payloads end to end after a prefix, the way wire
// writes string fields, and reads each one back with the rest of the buffer as
// its slack.
func TestPayloadsBackToBack(t *testing.T) {
	inputs := []string{"hello", "WORLD", "ab-cd-ef", "ñandú", "product123",
		strings.Repeat("z", 300), "el niño comió jamón"}
	prefix := []byte("PREFIX")
	buf := bytes.Clone(prefix)
	type entry struct {
		at, n int
		upper bool
	}
	var entries []entry
	for _, s := range inputs {
		at := len(buf)
		var n int
		var upper, ok bool
		buf, n, upper, ok = AppendPayload(buf, s)
		if !ok {
			t.Fatalf("%q did not pack", s)
		}
		entries = append(entries, entry{at, n, upper})
	}
	if !bytes.Equal(buf[:len(prefix)], prefix) {
		t.Fatalf("prefix clobbered: %q", buf[:len(prefix)])
	}
	for i, e := range entries {
		got, err := AppendString(nil, buf[e.at:], e.n, e.upper)
		if err != nil || string(got) != inputs[i] {
			t.Fatalf("payload %d: %q, %v; want %q", i, got, err, inputs[i])
		}
	}
	if last := entries[len(entries)-1]; last.at+last.n != len(buf) {
		t.Fatalf("%d bytes past the last payload", len(buf)-last.at-last.n)
	}
}

// TestNeverInflates is the headline guarantee: a payload is only produced when
// it is strictly smaller than the string, and otherwise nothing is appended.
func TestNeverInflates(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 8))
	prefix := []byte("xy")
	check := func(s string) {
		t.Helper()
		buf, n, _, ok := AppendPayload(prefix, s)
		if ok && (n >= len(s) || len(buf) != len(prefix)+n) {
			t.Fatalf("%q: %d-byte payload (buffer %d) against %d raw", s, n, len(buf), len(s))
		}
		if !ok && len(buf) != len(prefix) {
			t.Fatalf("%q: did not pack, but the buffer grew to %d", s, len(buf))
		}
	}
	for _, symbols := range [][]string{alphabet, {"日", "🌍", "\xff"}, {"a", "A"}} {
		for range 3000 {
			check(randString(rng, symbols, rng.IntN(50)))
		}
	}
	for n := range 300 {
		check(strings.Repeat("\x01", n))
	}
}

// TestNoAllocations pins both directions as heap-free given room in the caller's
// buffers: the encoder has no scratch of its own, and the decoder appends.
func TestNoAllocations(t *testing.T) {
	for _, s := range []string{"hello", "helloWorld", "product123", "el niño comió jamón",
		strings.Repeat("ab", 200), strings.Repeat("Lima norte ", 400)} {
		out := make([]byte, 0, 16<<10)
		var n int
		var upper, ok bool
		if got := testing.AllocsPerRun(100, func() {
			out, n, upper, ok = AppendPayload(out[:0], s)
		}); got != 0 {
			t.Errorf("AppendPayload(%q...): %.1f allocs, want 0", s[:min(len(s), 12)], got)
		}
		if !ok {
			t.Fatalf("%q did not pack", s)
		}
		dst := make([]byte, 0, len(s))
		var err error
		if got := testing.AllocsPerRun(100, func() {
			dst, err = AppendString(dst[:0], out, n, upper)
		}); got != 0 {
			t.Errorf("AppendString(%q...): %.1f allocs, want 0", s[:min(len(s), 12)], got)
		}
		if err != nil || string(dst) != s {
			t.Fatalf("%q: %v", s, err)
		}
	}
}

// TestPayloadSizes pins what representative strings cost, packed or raw, and
// the case mode each opens in. packed5/README.md quotes this table.
func TestPayloadSizes(t *testing.T) {
	for _, c := range []struct {
		in    string
		size  int  // bytes stored: the payload, or len(in) when raw
		mode  byte // 'l' or 'U' when packed, 'r' when raw
		units int  // what the scan spends, before the grid pad
	}{
		{"hello", 4, 'l', 5},
		{"helloWorld", 7, 'l', 11},
		{"fooBARTest", 8, 'l', 12},
		{"product123", 7, 'l', 10},
		{"SKU-00042-XL", 12, 'r', 18},
		{"el niño comió jamón", 14, 'l', 22},
		{"the quick brown fox", 12, 'l', 19},
		{"THE QUICK BROWN FOX", 12, 'U', 19},
		{"user.name@example.com", 15, 'l', 24},
		{`{"id":1023,"name":"ana"}`, 22, 'l', 34},
		{"€1023.45", 7, 'l', 10},
		{"Bogotá", 5, 'U', 8},
		{"Móvil Samsung Galaxy S23", 19, 'U', 30},
		{"Factura 2024-1023", 12, 'U', 19},
	} {
		size, mode := len(c.in), byte('r')
		if _, n, upper, ok := AppendPayload(nil, c.in); ok {
			size, mode = n, 'l'
			if upper {
				mode = 'U'
			}
		}
		if units := scanUnits(c.in); size != c.size || mode != c.mode || units != c.units {
			t.Errorf("%q: %d bytes, mode %c, %d units; want %d, %c, %d",
				c.in, size, mode, units, c.size, c.mode, c.units)
		}
	}
}

func FuzzRoundtrip(f *testing.F) {
	for _, s := range []string{"", "a", "helloWorld", "product123", "00123", "ñandú",
		"<a href=\"x\">", "\xff\xfe", "THE QUICK BROWN", strings.Repeat("x", 300)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		// No spare capacity anywhere: the prefix AppendPayload appends to, the
		// payload AppendString reads and the dst it appends to all end exactly
		// at their length, so every growth path and the tail path run.
		prefix := []byte{0xAA}
		buf, n, upper, ok := AppendPayload(prefix[:1:1], s)
		if buf[0] != 0xAA {
			t.Fatalf("prefix clobbered: %x", buf[0])
		}
		if !ok {
			if len(buf) != 1 {
				t.Fatalf("%q: did not pack, but appended %d bytes", s, len(buf)-1)
			}
			return
		}
		if n >= len(s) || len(buf) != 1+n {
			t.Fatalf("%q: %d-byte payload in a %d-byte buffer, raw is %d", s, n, len(buf), len(s))
		}
		src := make([]byte, n)
		copy(src, buf[1:])
		dst := []byte{0xBB}
		got, err := AppendString(dst[:1:1], src, n, upper)
		if err != nil {
			t.Fatalf("%q: %v (payload %x upper=%v)", s, err, src, upper)
		}
		if got[0] != 0xBB || string(got[1:]) != s {
			t.Fatalf("%q -> %q (payload %x upper=%v)", s, got, src, upper)
		}
	})
}

// realistic builds n short strings of one shape: the workload this codec is for.
func realistic(kind string, n int) []string {
	rng := rand.New(rand.NewPCG(42, 43))
	words := []string{"hola", "mundo", "producto", "cliente", "factura", "norte", "sur",
		"Lima", "Bogota", "Santiago", "activo", "pendiente", "azul", "rojo"}
	word := func() string { return words[rng.IntN(len(words))] }
	out := make([]string, 0, n)
	for range n {
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
	}
	return out
}

var realisticKinds = []string{"name", "sku", "spanish", "sentence", "paragraph"}

var benchStrings = []string{"hello", "helloWorld", "product123", "el niño comió jamón",
	"the quick brown fox jumps over the lazy dog"}

func BenchmarkAppendPayload(b *testing.B) {
	for _, s := range benchStrings {
		b.Run(fmt.Sprintf("len%d", len(s)), func(b *testing.B) {
			out := make([]byte, 0, 256)
			b.ReportAllocs()
			for b.Loop() {
				out, _, _, _ = AppendPayload(out[:0], s)
			}
		})
	}
}

// BenchmarkAppendString reads with slack after the payload, as wire does.
func BenchmarkAppendString(b *testing.B) {
	for _, s := range benchStrings {
		buf, n, upper, ok := AppendPayload(nil, s)
		if !ok {
			b.Fatalf("%q did not pack", s)
		}
		buf = append(buf, slackBytes...)
		b.Run(fmt.Sprintf("len%d", len(s)), func(b *testing.B) {
			dst := make([]byte, 0, 256)
			b.ReportAllocs()
			for b.Loop() {
				var err error
				if dst, err = AppendString(dst[:0], buf, n, upper); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
