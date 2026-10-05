package packed5

// How far the greedy scan lands from the smallest payload the format allows, and
// what it chooses on the way there.
//
// The optimum is a shortest path over (offset, case mode) nodes, priced in
// units. It is kept here as an oracle rather than shipped because it is several
// times slower, for a gap that is zero on realistic strings and small elsewhere.
// What matters is that the gap is measured rather than asserted, and that it
// cannot widen without a test failing.

import (
	"math/rand/v2"
	"strings"
	"testing"
)

// Token costs, in units.
const (
	unitsLetter      = 1
	unitsLetterCased = 2 // CASE_TOGGLE_SIMPLE + the letter
	unitsSpace       = 1
	unitsToggleLong  = 1
	unitsSymbol      = 2
	unitsExt         = 2
	unitsNumber      = 3
	unitsEscapeBase  = 3
	unitsEscapeByte  = 2
)

// optimalUnits is the fewest units any encoder could spend on s, over both
// starting case modes — the container carries either at no cost.
//
// dist[i][m] is the cost of reaching offset i in mode m. Every edge below is a
// token the format can actually emit, so the result is a real encoding rather
// than a lower bound.
func optimalUnits(s string) int {
	const inf = 1 << 30
	dist := make([][2]int, len(s)+1)
	for i := range dist {
		dist[i] = [2]int{inf, inf}
	}
	dist[0] = [2]int{0, 0}

	for i := range len(s) {
		// A long toggle consumes no input, so it is an edge within an offset
		// rather than between them. Relaxing it in both directions before any
		// outgoing edge is what makes a single ascending pass correct.
		if dist[i][0]+unitsToggleLong < dist[i][1] {
			dist[i][1] = dist[i][0] + unitsToggleLong
		}
		if dist[i][1]+unitsToggleLong < dist[i][0] {
			dist[i][0] = dist[i][1] + unitsToggleLong
		}
		for m := range 2 {
			base := dist[i][m]
			if base >= inf {
				continue
			}
			upper := m == 1
			relax := func(to, mode, cost int) {
				if base+cost < dist[to][mode] {
					dist[to][mode] = base + cost
				}
			}

			c := s[i]
			switch {
			case isLetter(c):
				if isUpper(c) == upper {
					relax(i+1, m, unitsLetter)
				} else {
					relax(i+1, m, unitsLetterCased)
				}
			case c == ' ':
				relax(i+1, m, unitsSpace)
			}
			if isDigit(c) {
				// Every legal number length, not just the longest.
				v := 0
				for l := 1; l <= numberMaxDigits && i+l <= len(s); l++ {
					if !isDigit(s[i+l-1]) || (l > 1 && s[i] == '0') {
						break
					}
					v = v*10 + int(s[i+l-1]-'0')
					if v > numberMax {
						break
					}
					if l == 1 {
						relax(i+1, m, unitsSymbol)
					} else {
						relax(i+l, m, unitsNumber)
					}
				}
			} else if c < 0x80 {
				if asciiSym[c] >= 0 {
					relax(i+1, m, unitsSymbol)
				}
				if asciiExt[c] >= 0 {
					relax(i+1, m, unitsExt)
				}
			} else if k, w := extMulti(s, i); k >= 0 {
				relax(i+w, m, unitsExt)
			}
			// An escape may carry any run of one to four escapable bytes.
			for n := 1; n <= maxEscapeRun && i+n <= len(s); n++ {
				if !escapes(s, i+n-1) {
					break
				}
				relax(i+n, m, unitsEscapeBase+unitsEscapeByte*n)
			}
		}
	}
	end := dist[len(s)]
	return min(end[0], end[1])
}

// storedSize is what a container stores for s: the payload when it packs, the
// raw bytes when it does not.
func storedSize(s string) int {
	if _, n, _, ok := AppendPayload(nil, s); ok {
		return n
	}
	return len(s)
}

// optimalSize is storedSize for the optimum, so the comparison is in the
// currency that matters. The grid pad is free: it fits in the rounding or it is
// not emitted.
func optimalSize(s string) int {
	return min(payloadBytes(optimalUnits(s)), len(s))
}

// scanUnits is what the shipped encoder spends, counted the same way.
func scanUnits(s string) int {
	var stack [1024]byte
	buf := stack[:]
	if need := len(s)*5 + 8; need > len(buf) {
		buf = make([]byte, need)
	}
	w := writer{buf: buf, limit: len(buf) - 8}
	w.tokenize(s, opensUpper(s))
	return w.units
}

// oraclePools are the inputs the scan is measured against the optimum on.
var oraclePools = []pool{
	{"words", []string{"hola", " ", "mundo", "Test", "123", "-", "ABC"}},
	{"alphabet", alphabet},
	{"digits", []string{"0", "1", "9", "a", "-", "A"}},
	{"case-heavy", []string{"a", "A", "b", "B", "c", "C"}},
	{"case+symbol", []string{"a", "A", "B", "b", "-", "."}},
	{"binary", []string{"\x00", "\xff", "\xc3", "a", "Z"}},
}

// TestNeverBeatsOptimal is the sanity direction: the greedy scan cannot spend
// fewer units than the shortest path, or the shortest path is wrong.
func TestNeverBeatsOptimal(t *testing.T) {
	for i, p := range oraclePools {
		rng := rand.New(rand.NewPCG(11, uint64(i)))
		for range 3000 {
			s := randString(rng, p.symbols, rng.IntN(24))
			if got, want := scanUnits(s), optimalUnits(s); got < want {
				t.Fatalf("%s: %q: scan %d units, 'optimal' %d", p.name, s, got, want)
			}
		}
	}
}

// TestOptimalOnRealisticStrings is the claim the README makes: on the shapes
// this codec is for, the greedy scan is not merely close to the optimum, it is
// the optimum, byte for byte.
func TestOptimalOnRealisticStrings(t *testing.T) {
	for _, kind := range realisticKinds {
		for _, s := range realistic(kind, 8000) {
			if got, want := storedSize(s), optimalSize(s); got != want {
				t.Fatalf("%s: %q stores in %d bytes, optimal is %d\n tokens: %s",
					kind, s, got, want, opsOf(disassemble(t, s)))
			}
		}
	}
}

// TestGapFromOptimal measures where the scan does lose, and pins it. Each
// ceiling sits a little above what the scan measures today, so a change that
// makes the encoder worse fails here rather than in a ratio someone notices
// later. Run with -v for the table packed5/README.md quotes.
func TestGapFromOptimal(t *testing.T) {
	limits := map[string]struct{ hurt, lost float64 }{
		"words":       {0.1, 0.01},
		"alphabet":    {2.0, 0.15},
		"digits":      {11.5, 1.0},
		"case-heavy":  {23.0, 2.0},
		"case+symbol": {15.0, 1.2},
		"binary":      {0.1, 0.01},
	}
	t.Log("pool          strings hurt   bytes lost   worst case")
	for i, p := range oraclePools {
		rng := rand.New(rand.NewPCG(13, uint64(i)))
		var hurt, total, lost, bytes, worst int
		var worstStr string
		for range 20000 {
			s := randString(rng, p.symbols, 1+rng.IntN(30))
			got, want := storedSize(s), optimalSize(s)
			total++
			bytes += want
			if got > want {
				hurt++
				lost += got - want
				if got-want > worst {
					worst, worstStr = got-want, s
				}
			}
		}
		hurtPct := 100 * float64(hurt) / float64(total)
		lostPct := 100 * float64(lost) / float64(bytes)
		t.Logf("%-12s %11.2f%% %11.2f%%   +%d on %q", p.name, hurtPct, lostPct, worst, worstStr)
		if lim := limits[p.name]; hurtPct > lim.hurt || lostPct > lim.lost {
			t.Errorf("%s: %.2f%% hurt / %.2f%% lost exceeds %.2f / %.2f",
				p.name, hurtPct, lostPct, lim.hurt, lim.lost)
		}
	}
}

// TestKnownGapCaseAcrossSymbols bounds the one shape the run-length rule cannot
// see: case changes spread across symbols, where two long toggles spanning them
// beat four simple ones. A better encoder may close the gap; none may widen it.
func TestKnownGapCaseAcrossSymbols(t *testing.T) {
	const s = "abab-CD-EF-ghgh"
	got, want := scanUnits(s), optimalUnits(s)
	if got-want > 2 {
		t.Fatalf("%q: scan %d units against an optimal %d, a gap above 2 — %s",
			s, got, want, opsOf(disassemble(t, s)))
	}
}

// --- what the encoder chooses ----------------------------------------------

// TestCaseToggleSelection pins the run-length rule at its boundary: one or two
// opposite-case letters take a simple toggle each, three or more take a long one.
func TestCaseToggleSelection(t *testing.T) {
	for run := 1; run <= 5; run++ {
		s := "lower" + strings.Repeat("A", run) + "tail"
		toks := disassemble(t, s)
		simple, long := countKind(toks, "caseSimple"), countKind(toks, "caseLong")
		if run <= 2 {
			if simple != run || long != 0 {
				t.Errorf("run %d: %d simple, %d long — %s", run, simple, long, opsOf(toks))
			}
		} else if simple != 0 || long != 2 {
			t.Errorf("run %d: %d simple, %d long — %s", run, simple, long, opsOf(toks))
		}
	}
}

// TestUppercaseModeIsHoisted pins the case mode: a string whose first letter is
// uppercase opens the stream in uppercase mode, and spends no token saying so.
func TestUppercaseModeIsHoisted(t *testing.T) {
	for _, c := range []struct {
		in    string
		upper bool
	}{
		{"hello world", false},
		{"Hello world", true},
		{"HELLO WORLD", true},
		{"SKU-4217-hola", true},
		{"123-abc", false},
		{"123-ABC", true},
		{"-", false},
	} {
		if got := opensUpper(c.in); got != c.upper {
			t.Errorf("%q: opens upper = %v, want %v", c.in, got, c.upper)
		}
		_, _, upper, ok := AppendPayload(nil, c.in)
		if !ok {
			continue // stored raw; there is no case mode
		}
		if upper != c.upper {
			t.Errorf("%q: AppendPayload upper = %v, want %v", c.in, upper, c.upper)
		}
		// Whatever the mode, the stream must not open by toggling it.
		if toks := disassemble(t, c.in); len(toks) > 0 && toks[0].kind == "caseLong" {
			t.Errorf("%q: stream opens with a long toggle — %s", c.in, opsOf(toks))
		}
	}
}

// TestLoneDigitTakesSymbolToken pins the one place the greedy rule prefers the
// shorter token to the longer run: a single digit is two units, where a number
// would be three.
func TestLoneDigitTakesSymbolToken(t *testing.T) {
	for _, c := range []struct{ in, kind string }{
		{"aaaaaa1aaaaaa", "sym"},
		{"aaaaaa12aaaaaa", "num"},
		{"aaaaaa1234aaaaaa", "num"},
	} {
		toks := disassemble(t, c.in)
		found := ""
		for _, tk := range toks {
			if tk.kind == "sym" || tk.kind == "num" {
				found = tk.kind
				break
			}
		}
		if found != c.kind {
			t.Errorf("%q: took %s, want %s — %s", c.in, found, c.kind, opsOf(toks))
		}
	}
}

// TestNumberTokensNeverCarryLeadingZeros is the rule that keeps "00123" from
// coming back as "123". Checked over every decimal string up to six digits.
func TestNumberTokensNeverCarryLeadingZeros(t *testing.T) {
	var b, buf, dst []byte
	var digits func(n int)
	digits = func(n int) {
		if n == 0 {
			var size int
			var upper, ok bool
			if buf, size, upper, ok = AppendPayload(buf[:0], string(b)); !ok {
				return // stored raw, so there is no number token to check
			}
			var err error
			if dst, err = AppendString(dst[:0], buf, size, upper); err != nil || string(dst) != string(b) {
				t.Fatalf("%q: got %q, %v", b, dst, err)
			}
			return
		}
		for c := byte('0'); c <= '9'; c++ {
			b = append(b, c)
			digits(n - 1)
			b = b[:len(b)-1]
		}
	}
	for n := 1; n <= 6; n++ {
		digits(n)
	}
}

// TestEscapeRunsAreMerged pins that consecutive unrepresentable bytes share one
// escape header, up to the four-byte limit.
func TestEscapeRunsAreMerged(t *testing.T) {
	for n := 1; n <= 9; n++ {
		s := strings.Repeat("a", 30) + strings.Repeat("\x01", n) + strings.Repeat("b", 30)
		toks := disassemble(t, s)
		want := (n + maxEscapeRun - 1) / maxEscapeRun
		if got := countKind(toks, "esc"); got != want {
			t.Errorf("%d bytes: %d escapes, want %d — %s", n, got, want, opsOf(toks))
		}
	}
}

// TestSymbolTableCoverage walks both tables and checks each entry reaches its
// own token rather than the escape.
func TestSymbolTableCoverage(t *testing.T) {
	for i, c := range symTable {
		toks := disassemble(t, "aaaaaaaa"+string(c)+"aaaaaaaa")
		if countKind(toks, "sym") != 1 {
			t.Errorf("symTable[%d] = %q did not take a symbol token — %s", i, c, opsOf(toks))
			continue
		}
		for _, tk := range toks {
			if tk.kind == "sym" && tk.val != i {
				t.Errorf("symTable[%d] = %q encoded as index %d", i, c, tk.val)
			}
		}
	}
	for i, str := range extTable {
		if i >= extReserved {
			if str != "" {
				t.Errorf("extTable[%d] is reserved but holds %q", i, str)
			}
			continue
		}
		toks := disassemble(t, "aaaaaaaa"+str+"aaaaaaaa")
		if countKind(toks, "ext") != 1 {
			t.Errorf("extTable[%d] = %q did not take an ext token — %s", i, str, opsOf(toks))
			continue
		}
		for _, tk := range toks {
			if tk.kind == "ext" && tk.val != i {
				t.Errorf("extTable[%d] = %q encoded as index %d", i, str, tk.val)
			}
		}
	}
}

// TestTablesAreConsistent pins the tables against their inverses.
func TestTablesAreConsistent(t *testing.T) {
	seen := map[string]int{}
	for i, c := range symTable {
		if asciiSym[c] != int8(i) {
			t.Errorf("asciiSym[%q] = %d, want %d", c, asciiSym[c], i)
		}
		if prev, dup := seen[string(c)]; dup {
			t.Errorf("%q appears at symTable[%d] and [%d]", c, prev, i)
		}
		seen[string(c)] = i
	}
	for i, s := range extTable {
		if i >= extReserved {
			continue
		}
		if prev, dup := seen[s]; dup {
			t.Errorf("%q appears twice: index %d and extTable[%d]", s, prev, i)
		}
		seen[s] = i
		switch {
		case len(s) == 1:
			if asciiExt[s[0]] != int8(i) {
				t.Errorf("asciiExt[%q] = %d, want %d", s, asciiExt[s[0]], i)
			}
		default:
			k, w := extMulti(s, 0)
			if int(k) != i || w != len(s) {
				t.Errorf("extMulti(%q) = %d, %d; want %d, %d", s, k, w, i, len(s))
			}
		}
	}
	// A letter or a space must never also sit in a table, or two encodings of
	// the same string would exist.
	for c := range 128 {
		if isLetter(byte(c)) || byte(c) == ' ' {
			if asciiSym[c] >= 0 || asciiExt[c] >= 0 {
				t.Errorf("%q has both a base opcode and a table entry", byte(c))
			}
		}
	}
}

// TestPayloadLengthMatchesUnits pins the payload length to the units the scan
// spends: the grid pad never adds a byte, so the payload is exactly the bytes
// those units need.
func TestPayloadLengthMatchesUnits(t *testing.T) {
	for n := 1; n < 400; n++ {
		s := strings.Repeat("ab ", n)
		_, size, _, ok := AppendPayload(nil, s)
		if !ok {
			t.Fatalf("n=%d: did not pack", n)
		}
		if want := payloadBytes(scanUnits(s)); size != want {
			t.Errorf("n=%d: payload %d bytes, %d units need %d", n, size, scanUnits(s), want)
		}
	}
}

// TestPackedBeatsRawWhereExpected pins the fallback: shapes the format is for
// must pack, and shapes it is not for must not.
func TestPackedBeatsRawWhereExpected(t *testing.T) {
	for _, s := range []string{"hello", "helloWorld", "el niño comió jamón",
		"the quick brown fox", "SKU-4217-hola", "Factura 2024-1023",
		`{"id":1023,"name":"ana"}`} {
		if _, _, _, ok := AppendPayload(nil, s); !ok {
			t.Errorf("%q did not pack", s)
		}
	}
	for _, s := range []string{"\x00\x01\x02\x03", "\xff\xfe\xfd", "SKU-00042-XL"} {
		if _, _, _, ok := AppendPayload(nil, s); ok {
			t.Errorf("%q packed, but should have fallen back", s)
		}
	}
}

// TestSpecExampleCosts pins the unit cost of a few worked examples, so a change
// to the token widths shows up as a number rather than as a ratio.
func TestSpecExampleCosts(t *testing.T) {
	for _, c := range []struct {
		in    string
		units int
	}{
		{"hello", 5},                     // five letters
		{"hello world", 11},              // plus a space and five more
		{"helloWorld", 11},               // a simple toggle for the W
		{"HELLOWORLD", 10},               // all upper, mode from the container
		{"a1", 3},                        // letter plus a lone digit
		{"a12", 4},                       // letter plus a number
		{"a-b", 4},                       // the dash is a symbol token
		{"ñ", 2},                         // one ext token
		{"\x01", 5},                      // escape header plus one byte
		{"\x01\x02\x03\x04", 11},         // one escape carrying four
		{"\x01\x02\x03\x04\x05", 11 + 5}, // and the fifth starts another
	} {
		if got := scanUnits(c.in); got != c.units {
			t.Errorf("%q: %d units, want %d — %s", c.in, got, c.units,
				opsOf(scan(c.in, opensUpper(c.in))))
		}
	}
}

// TestAppendPayloadAfterPrefix pins the encoder's offset handling: a payload
// appended after existing bytes, as wire always does after a field header, is
// the same payload it writes onto an empty buffer, and the bytes before it are
// left alone.
func TestAppendPayloadAfterPrefix(t *testing.T) {
	prefix := []byte("key+header")
	for i, p := range oraclePools {
		rng := rand.New(rand.NewPCG(15, uint64(i)))
		for range 3000 {
			s := randString(rng, p.symbols, rng.IntN(40))
			alone, n, upper, ok := AppendPayload(nil, s)
			after, m, upperAfter, okAfter := AppendPayload(prefix, s)
			if ok != okAfter || n != m || upper != upperAfter {
				t.Fatalf("%q: alone %v/%d/%v, after a prefix %v/%d/%v",
					s, ok, n, upper, okAfter, m, upperAfter)
			}
			if string(after[:len(prefix)]) != string(prefix) {
				t.Fatalf("%q: prefix clobbered: %q", s, after[:len(prefix)])
			}
			if !ok {
				if len(after) != len(prefix) {
					t.Fatalf("%q: did not pack, but appended %d bytes", s, len(after)-len(prefix))
				}
				continue
			}
			if string(after[len(prefix):]) != string(alone[:n]) {
				t.Fatalf("%q:\n alone        %x\n after prefix %x", s, alone[:n], after[len(prefix):])
			}
		}
	}
}
