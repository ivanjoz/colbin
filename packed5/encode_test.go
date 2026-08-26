package packed5

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

// ------------------------------------------------------- the optimality oracle

// refBits is the smallest number of bits the format can represent s in, found
// by memoised recursion over every token choice. It is the encoder the package
// deliberately does not ship: exact, and roughly 2.3x slower than the greedy
// scan. Keeping it here is what turns "the scan is close enough to optimal"
// from a claim into a measured, enforced bound.
//
// best(i, mode) may open with a long toggle; fwd(i, mode) may not. Splitting
// them keeps the recursion acyclic, and loses nothing, since toggling twice in
// a row only ever costs 10 bits.
func refBits(s string, number bool) int {
	n := len(s)
	const unset = -1
	bestMemo := make([]int, 2*(n+1))
	fwdMemo := make([]int, 2*(n+1))
	for i := range bestMemo {
		bestMemo[i], fwdMemo[i] = unset, unset
	}

	var best, fwd func(i, mode int) int
	best = func(i, mode int) int {
		if v := bestMemo[i*2+mode]; v != unset {
			return v
		}
		v := min(fwd(i, mode), costToggleLong+fwd(i, 1-mode))
		if i == n {
			v = 0
		}
		bestMemo[i*2+mode] = v
		return v
	}
	fwd = func(i, mode int) int {
		if i == n {
			return 0
		}
		if v := fwdMemo[i*2+mode]; v != unset {
			return v
		}
		v := 1 << 30
		take := func(cost, width int) {
			if c := cost + best(i+width, mode); c < v {
				v = c
			}
		}
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
			if mode == 0 {
				take(costLetter, 1)
			} else {
				take(costLetterCased, 1)
			}
		case c >= 'A' && c <= 'Z':
			if mode == 1 {
				take(costLetter, 1)
			} else {
				take(costLetterCased, 1)
			}
		case c == ' ':
			take(costSpace, 1)
		}
		for k := range symReserved {
			if sym := symTable[k]; sym != "" && strings.HasPrefix(s[i:], sym) {
				take(costSymbol, len(sym))
			}
		}
		for k := range escapeCode {
			if simpleTable[k] == c {
				take(costSimple, 1)
			}
		}
		if !number && c == '-' {
			take(costDash, 1)
		}
		if number {
			for l := 1; l <= numberMaxDigits && i+l <= n; l++ {
				run := s[i : i+l]
				if !allDigits(run) || (l > 1 && run[0] == '0') || atoi(run) > numberMax {
					break
				}
				take(costNumber, l)
			}
		}
		for l := 1; l <= maxEscapeRun && i+l <= n; l++ {
			take(costEscapeBase+costEscapeByte*l, l)
		}
		fwdMemo[i*2+mode] = v
		return v
	}
	return min(best(0, 0), best(0, 1))
}

func allDigits(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func atoi(s string) int {
	v := 0
	for i := range len(s) {
		v = v*10 + int(s[i]-'0')
	}
	return v
}

// optimalBits is the best any encoder for this format could do, over the flag
// settings the shipped encoder is allowed to consider.
func optimalBits(s string) int {
	return min(refBits(s, false), refBits(s, true))
}

// optimalSize is optimalBits as a frame length, for comparison against Size.
func optimalSize(s string) int {
	if len(s) == 0 {
		return 1
	}
	payload := payloadBytes(optimalBits(s))
	return min(frameOverhead(payload)+payload, frameOverhead(len(s))+len(s))
}

// encodedBits runs the shipped scan and reports what it costs.
func encodedBits(s string, number bool) int {
	buf := make([]token, 0, 2*len(s)+2)
	_, bits := scan(buf, s, false, number)
	if _, up := scan(buf, s, true, number); up < bits {
		bits = up
	}
	return bits
}

// scanBits is what the shipped encoder actually charges for s, across all the
// candidate flag settings it tries.
func scanBits(s string) int {
	bits, _, _ := plan(s)
	return bits
}

// TestPlanMatchesCandidateScans checks that the single planning pass preserves
// both the exact cost and the strict tie-breaking order of the four former
// candidate scans, including arbitrary invalid UTF-8 input.
func TestPlanMatchesCandidateScans(t *testing.T) {
	rng := rand.New(rand.NewPCG(41, 43))
	for n := 0; n <= 512; n++ {
		for range 20 {
			b := make([]byte, n)
			for i := range b {
				b[i] = byte(rng.Uint32())
			}
			s := string(b)
			buf := make([]token, 0, 2*len(s)+2)
			_, wantBits := scan(buf, s, false, false)
			wantUpper, wantNumber := false, false
			for _, candidate := range []struct {
				upper, number bool
			}{
				{true, false},
				{false, true},
				{true, true},
			} {
				if _, bits := scan(buf, s, candidate.upper, candidate.number); bits < wantBits {
					wantBits = bits
					wantUpper, wantNumber = candidate.upper, candidate.number
				}
			}
			gotBits, gotUpper, gotNumber := plan(s)
			if gotBits != wantBits || gotUpper != wantUpper || gotNumber != wantNumber {
				t.Fatalf("length %d: plan = (%d,%v,%v), scans = (%d,%v,%v)",
					n, gotBits, gotUpper, gotNumber, wantBits, wantUpper, wantNumber)
			}
		}
	}
}

// ------------------------------------------------------- distance from optimal

// TestNeverBeatsOptimal is a sanity bound in the other direction: the greedy
// scan must never claim a stream cheaper than the format allows, which would
// mean the cost table and the token widths had drifted apart.
func TestNeverBeatsOptimal(t *testing.T) {
	check := func(s string) {
		t.Helper()
		if got, opt := scanBits(s), optimalBits(s); got < opt {
			t.Fatalf("%q: scan priced it at %d bits, the optimum is %d", s, got, opt)
		}
		if got, opt := Size(s), optimalSize(s); got < opt {
			t.Fatalf("%q: Size = %d, the optimum is %d", s, got, opt)
		}
	}
	check("")
	for _, x := range alphabet {
		check(x)
		for _, y := range alphabet {
			check(x + y)
			for _, z := range alphabet {
				check(x + y + z)
			}
		}
	}
	rng := rand.New(rand.NewPCG(11, 12))
	for _, pool := range [][]string{alphabet, {"a", "A", "b", "B"}, {"0", "1", "9", "-", "."},
		{"ñ", "é", "€", "日", "\xff", "a"}, {"x", "Y", "-", "7", "€", "\x00"}} {
		for range 400 {
			check(randString(rng, pool, rng.IntN(24)))
		}
	}
}

// TestOptimalOnRealisticStrings is the claim the design rests on: on strings of
// the shape this codec exists for, the greedy scan is not merely close to the
// exact shortest path, it is identical to it. If a change to the scan ever
// costs a byte on this data, this fails.
func TestOptimalOnRealisticStrings(t *testing.T) {
	for _, kind := range corpusKinds {
		c, _ := corpus(kind)
		gap, worst := 0, ""
		for _, s := range c {
			if d := Size(s) - optimalSize(s); d > 0 {
				gap += d
				if worst == "" {
					worst = s
				}
			}
		}
		if gap != 0 {
			t.Errorf("corpus %s: %d bytes above optimal, first at %q", kind, gap, worst)
		}
	}
	for _, s := range []string{
		"helloWorld", "fooBARTest", "aaBBcc", "McDonald's", "product123",
		"SKU-00042-XL", "Factura 2024-1023", "10231023", "el niño comió jamón",
		"user.name@example.com", "€1023.45", "the quick brown fox",
		"iPhone XS Max", "eBay UK Ltd", "Bogotá", "00123",
	} {
		if got, opt := Size(s), optimalSize(s); got != opt {
			t.Errorf("%q: %d bytes, optimum is %d", s, got, opt)
		}
	}
}

// TestGapFromOptimal pins how far the scan drifts on input designed to hurt it.
// Dense case changes interleaved with symbols are where a run-length rule
// cannot see far enough; the bounds here are the measured behaviour, so a
// regression that made the scan noticeably worse would trip them.
func TestGapFromOptimal(t *testing.T) {
	pools := []struct {
		name    string
		pool    []string
		maxPct  float64
		maxHurt float64
	}{
		{"words", []string{"hola", " ", "Mundo", "123", "-", "ABC", "ñ"}, 0.01, 0.01},
		{"binary", []string{"\xff", "\x01", "a", "@", "\x00"}, 0.01, 0.01},
		{"alphabet", alphabet, 1.0, 15},
		{"digits", []string{"0", "1", "9", "-", "a", "A"}, 1.5, 20},
		{"case-heavy", []string{"a", "A", "b", "B", "c", "C"}, 2.0, 25},
		{"case+symbol", []string{"a", "A", "-", ".", "b", "B"}, 3.0, 30},
	}
	rng := rand.New(rand.NewPCG(99, 100))
	t.Log("pool         strings hurt   bytes lost   worst case")
	for _, p := range pools {
		hurt, total, lost, bytes := 0, 0, 0, 0
		worst, worstS := 0, ""
		for range 20000 {
			s := randString(rng, p.pool, rng.IntN(30)+1)
			got, opt := Size(s), optimalSize(s)
			total, bytes = total+1, bytes+opt
			if d := got - opt; d > 0 {
				hurt, lost = hurt+1, lost+d
				if d > worst {
					worst, worstS = d, s
				}
			}
		}
		pctHurt := 100 * float64(hurt) / float64(total)
		pctLost := 100 * float64(lost) / float64(bytes)
		t.Log(fmt.Sprintf("%-12s %8.2f%%  %9.2f%%   %+d on %q", p.name, pctHurt, pctLost, worst, worstS))
		if pctLost > p.maxPct {
			t.Errorf("%s: %.2f%% of bytes lost, bound is %.2f%%", p.name, pctLost, p.maxPct)
		}
		if pctHurt > p.maxHurt {
			t.Errorf("%s: %.2f%% of strings hurt, bound is %.2f%%", p.name, pctHurt, p.maxHurt)
		}
	}
}

// TestKnownGapCaseAcrossSymbols is the one shape the scan is known to miss, kept
// as an explicit record rather than left for someone to rediscover. The four
// uppercase letters here are split by symbols into two runs of two, so the
// run-length rule spends four CASE_TOGGLE_SIMPLE (20 bits) where two
// CASE_TOGGLE_LONG spanning the symbols would cost 10.
func TestKnownGapCaseAcrossSymbols(t *testing.T) {
	const s = "ab-CD-EF-gh"
	toks := dumpTokens(t, Append(nil, s))
	if countOp(toks, "simple") == 0 {
		t.Fatalf("%q: expected the run-length rule to pick simple toggles, got %v", s, opsOf(toks))
	}
	if got, opt := scanBits(s), optimalBits(s); got-opt != 10 {
		t.Errorf("%q: scan costs %d bits against an optimum of %d; the known gap is 10", s, got, opt)
	}
	if got, opt := Size(s), optimalSize(s); got-opt != 1 {
		t.Errorf("%q: %d bytes against an optimum of %d", s, got, opt)
	}
}

// ------------------------------------------------------------- token dumping

type tok struct {
	op    string
	arg   int
	bytes string
}

// dumpTokens re-reads a packed frame at the token level so tests can assert on
// structure, not just on the decoded string.
func dumpTokens(t *testing.T, buf []byte) []tok {
	t.Helper()
	h, err := frame(buf)
	if err != nil {
		t.Fatalf("frame: %v", err)
	}
	if !h.packed {
		t.Fatalf("frame is raw, not packed: %x", buf)
	}
	r := bitReader{buf: h.payload, limit: len(h.payload) * 8}
	pad, ok := r.read(padBitsWidth)
	if !ok {
		t.Fatal("short frame")
	}
	r.limit -= int(pad)

	var out []tok
	for r.remaining() >= 5 {
		op, _ := r.read(5)
		switch {
		case op < opSpace:
			out = append(out, tok{op: "letter", arg: int(op)})
		case op == opSpace:
			out = append(out, tok{op: "space"})
		case op == opCaseSimple:
			out = append(out, tok{op: "simple"})
		case op == opCaseLong:
			out = append(out, tok{op: "long"})
		case op == opSymbol:
			v, _ := r.read(5)
			out = append(out, tok{op: "symbol", arg: int(v), bytes: symTable[v]})
		case op == opSimple:
			v, _ := r.read(4)
			if v != escapeCode {
				out = append(out, tok{op: "simpleSym", arg: int(v), bytes: string(simpleTable[v])})
				break
			}
			cnt, _ := r.read(2)
			var b []byte
			for range int(cnt) + 1 {
				x, _ := r.read(8)
				b = append(b, byte(x))
			}
			out = append(out, tok{op: "escape", arg: len(b), bytes: string(b)})
		default:
			if !h.number {
				out = append(out, tok{op: "dash"})
				break
			}
			v, _ := r.read(10)
			out = append(out, tok{op: "number", arg: int(v)})
		}
	}
	return out
}

func opsOf(toks []tok) []string {
	out := make([]string, len(toks))
	for i, tk := range toks {
		out[i] = tk.op
	}
	return out
}

func countOp(toks []tok, op string) int {
	n := 0
	for _, tk := range toks {
		if tk.op == op {
			n++
		}
	}
	return n
}

// ------------------------------------------------------------ structure tests

// TestCaseToggleSelection pins the cost table from the specification: an
// isolated opposite-case letter takes CASE_TOGGLE_SIMPLE, a run of three or
// more takes CASE_TOGGLE_LONG, and a run of two may take either but must not
// cost more than 10 extra bits.
func TestCaseToggleSelection(t *testing.T) {
	for _, tc := range []struct {
		run                  int
		wantSimple, wantLong int
	}{
		{1, 1, 0},
		{3, 0, 2},
		{4, 0, 2},
		{8, 0, 2},
	} {
		s := "aaaa" + strings.Repeat("B", tc.run) + "cccc"
		toks := dumpTokens(t, Append(nil, s))
		if got := countOp(toks, "simple"); got != tc.wantSimple {
			t.Errorf("%q: %d simple toggles, want %d (%v)", s, got, tc.wantSimple, opsOf(toks))
		}
		if got := countOp(toks, "long"); got != tc.wantLong {
			t.Errorf("%q: %d long toggles, want %d (%v)", s, got, tc.wantLong, opsOf(toks))
		}
	}
	two := encodedBits("aaaaBBcccc", false)
	if base := encodedBits("aaaabbcccc", false); two != base+10 {
		t.Errorf("two-letter run cost %d bits, want %d", two, base+10)
	}
}

// TestUppercaseDominantFlag checks the header bit follows the cheaper starting
// case. The encoder scans both rather than counting letters, which is what
// makes the mixed rows below come out right.
func TestUppercaseDominantFlag(t *testing.T) {
	for _, tc := range []struct {
		in    string
		upper bool
	}{
		{"hello world", false},
		{"HELLO WORLD", true},
		{"Hello World", false},
		{"HELLO world", true},
		{"hello WORLD", false},
		{"ABCDEFGHIJ", true},
		{"abcdefghij", false},
		{"SKU-0421-azul", true}, // 3 uppercase against 4 lowercase, but fewer runs
	} {
		buf := Append(nil, tc.in)
		if buf[0]&flagPacked5 == 0 {
			t.Fatalf("%q was stored raw", tc.in)
		}
		if got := buf[0]&flagUppercase != 0; got != tc.upper {
			t.Errorf("%q: UPPERCASE_DOMINANT = %v, want %v", tc.in, got, tc.upper)
		}
	}
}

// TestNumberModeFlag checks ENABLE_NUMBER_0_1023 is set exactly when it pays,
// and that opcode 31 means '-' when it is clear.
func TestNumberModeFlag(t *testing.T) {
	for _, tc := range []struct {
		in     string
		number bool
	}{
		{"product123", true},
		{"item1023", true},
		{"aaaa1bbbb2cccc3", false}, // no run of two, so the flag cannot pay
		{"low-cost-x", false},      // '-' is worth more than any integer here
		{"page7", false},
	} {
		buf := Append(nil, tc.in)
		if buf[0]&flagPacked5 == 0 {
			t.Fatalf("%q was stored raw", tc.in)
		}
		if got := buf[0]&flagNumber != 0; got != tc.number {
			t.Errorf("%q: ENABLE_NUMBER_0_1023 = %v, want %v", tc.in, got, tc.number)
		}
	}
	toks := dumpTokens(t, Append(nil, "aaaa-bbbb"))
	if countOp(toks, "dash") != 1 {
		t.Errorf(`"aaaa-bbbb": expected a bare opcode-31 dash, got %v`, opsOf(toks))
	}
	toks = dumpTokens(t, Append(nil, "product123"))
	if countOp(toks, "number") != 1 {
		t.Errorf(`"product123": expected one NUMBER_0_1023, got %v`, opsOf(toks))
	}
}

// TestLoneDigitTakesSimpleToken is the fix that keeps the scan optimal on
// zero-padded identifiers: a digit run whose longest legal integer prefix is a
// single digit costs 9 bits as a simple symbol, not 15 as an integer.
func TestLoneDigitTakesSimpleToken(t *testing.T) {
	for _, tc := range []struct {
		in            string
		number, plain int
	}{
		{"aaaa0421aaaa", 1, 1}, // leading zero splits it: '0' plain, 421 integer
		{"aaaa1421aaaa", 1, 1}, // over 1023: '1' plain, 421 integer
		{"aaaa1023aaaa", 1, 0},
		{"aaaa7aaaa", 0, 1},
	} {
		toks := dumpTokens(t, Append(nil, tc.in))
		if got := countOp(toks, "number"); got != tc.number {
			t.Errorf("%q: %d integer tokens, want %d (%v)", tc.in, got, tc.number, opsOf(toks))
		}
		if got := countOp(toks, "simpleSym"); got != tc.plain {
			t.Errorf("%q: %d simple digits, want %d (%v)", tc.in, got, tc.plain, opsOf(toks))
		}
	}
}

// TestNumberTokensNeverCarryLeadingZeros is the spec's "00123 must not become
// 123" rule, checked at the token level across an exhaustive digit space.
func TestNumberTokensNeverCarryLeadingZeros(t *testing.T) {
	var rec func(prefix string, depth int)
	rec = func(prefix string, depth int) {
		if prefix != "" {
			buf := Append(nil, prefix)
			if buf[0]&flagPacked5 != 0 && buf[0]&flagNumber != 0 {
				for _, tk := range dumpTokens(t, buf) {
					if tk.op == "number" && tk.arg > numberMax {
						t.Fatalf("%q: number token %d out of range", prefix, tk.arg)
					}
				}
			}
			got, _, err := Decode(buf)
			if err != nil || got != prefix {
				t.Fatalf("%q -> %q err=%v", prefix, got, err)
			}
		}
		if depth == 0 {
			return
		}
		for _, d := range []string{"0", "1", "2", "9"} {
			rec(prefix+d, depth-1)
		}
	}
	rec("", 6)
	for _, s := range []string{"00123", "007", "0", "00", "000", "0001023", "01023"} {
		got, _, err := Decode(Append(nil, s))
		if err != nil || got != s {
			t.Fatalf("%q -> %q err=%v", s, got, err)
		}
	}
}

// TestEscapeRunsAreMerged checks that consecutive escaped bytes share one
// escape header rather than paying 11 bits each, up to the 4-byte limit.
func TestEscapeRunsAreMerged(t *testing.T) {
	pad := strings.Repeat("a", 12)
	for _, tc := range []struct {
		in      string
		escapes int
	}{
		{"\x01", 1},
		{"\x01\x02", 1},
		{"\x01\x02\x03\x04", 1},
		{"\x01\x02\x03\x04\x05", 2},
		{"\x01\x02\x03\x04\x05\x06\x07\x08", 2},
		{"\x01\x02\x03\x04\x05\x06\x07\x08\x09", 3},
	} {
		// Padded with letters so the packed frame wins on size; escapes alone
		// always lose to raw bytes, which is the point of the fallback.
		in := pad + tc.in + pad
		toks := dumpTokens(t, Append(nil, in))
		if got := countOp(toks, "escape"); got != tc.escapes {
			t.Errorf("%q: %d escapes, want %d (%v)", in, got, tc.escapes, opsOf(toks))
		}
	}
}

// TestSymbolTableCoverage roundtrips every assigned entry of both operand
// tables and checks each reaches its own opcode.
func TestSymbolTableCoverage(t *testing.T) {
	for i := range symReserved {
		sym := symTable[i]
		s := "aaaaaa" + sym + "aaaaaa"
		toks := dumpTokens(t, Append(nil, s))
		found := false
		for _, tk := range toks {
			if tk.op == "symbol" && tk.arg == i {
				found = true
			}
		}
		if !found {
			t.Errorf("symTable[%d] = %q not reached: %v", i, sym, opsOf(toks))
		}
		roundtrip(t, s)
	}
	for i := range escapeCode {
		c := simpleTable[i]
		s := "aaaaaa" + string(c) + "aaaaaa"
		toks := dumpTokens(t, Append(nil, s))
		found := false
		for _, tk := range toks {
			if (tk.op == "simpleSym" && tk.arg == i) || (tk.op == "dash" && c == '-') {
				found = true
			}
		}
		if !found {
			t.Errorf("simpleTable[%d] = %q not reached: %v", i, string(c), opsOf(toks))
		}
		roundtrip(t, s)
	}
}

// TestSpecExampleCosts pins the bit costs the specification quotes, so a change
// to a token width shows up as a test failure rather than as a silent size
// regression.
func TestSpecExampleCosts(t *testing.T) {
	for _, tc := range []struct {
		in     string
		number bool
		bits   int
	}{
		// "hello" + CASE_TOGGLE_SIMPLE + "world": 10 letters, one toggle.
		{"helloWorld", false, 11 * 5},
		// "hello" + LONG + "world" + LONG + "again": 15 letters, two toggles.
		{"helloWORLDagain", false, 17 * 5},
		// "foo" + LONG + "bart" + LONG + "est": cheaper than the spec's own
		// walkthrough, which re-toggles for the T.
		{"fooBARTest", false, 12 * 5},
		// "product" + NUMBER_0_1023(123).
		{"product123", true, 7*5 + 15},
		// Same string with the flag off: three 9-bit digits instead.
		{"product123", false, 7*5 + 3*9},
	} {
		if got := encodedBits(tc.in, tc.number); got != tc.bits {
			t.Errorf("%q number=%v: %d bits, want %d", tc.in, tc.number, got, tc.bits)
		}
	}
}

// TestPackedBeatsRawWhereExpected records the inputs Packed-5 is built for and
// the ones it correctly declines.
func TestPackedBeatsRawWhereExpected(t *testing.T) {
	for _, tc := range []struct {
		in     string
		packed bool
	}{
		{"hello", true},
		{"helloWorld", true},
		{"the quick brown fox", true},
		{"el niño comió jamón", true},
		{"product123", true},
		{"a", false},        // one letter cannot beat one byte
		{"\xff\xfe", false}, // pure escape always loses
		{"日本語", false},      // three-byte runes always lose
		{"00123", false},    // leading zeros block the integer token
	} {
		buf := Append(nil, tc.in)
		if got := buf[0]&flagPacked5 != 0; got != tc.packed {
			t.Errorf("%q: packed = %v, want %v (%d bytes vs raw %d)",
				tc.in, got, tc.packed, len(buf), len(tc.in))
		}
	}
}

// TestReservedFlagNeverSet checks the encoder leaves bit 3 clear, which is what
// lets a future revision claim it.
func TestReservedFlagNeverSet(t *testing.T) {
	rng := rand.New(rand.NewPCG(15, 16))
	for range 5000 {
		s := randString(rng, alphabet, rng.IntN(30))
		if buf := Append(nil, s); buf[0]&flagReserved != 0 {
			t.Fatalf("%q: reserved flag set", s)
		}
	}
}

// TestTablesAreConsistent checks the reverse lookups built in init agree with
// the forward tables and that the two reserved symbol slots stay empty.
func TestTablesAreConsistent(t *testing.T) {
	for i := range symReserved {
		sym := symTable[i]
		if sym == "" {
			t.Fatalf("symTable[%d] is empty but below symReserved", i)
		}
		if len(sym) == 1 {
			if got := asciiSym[sym[0]]; got != int8(i) {
				t.Errorf("asciiSym[%q] = %d, want %d", sym, got, i)
			}
		} else if got := runeSymbol([]rune(sym)[0]); got != int8(i) {
			t.Errorf("runeSymbol(%q) = %d, want %d", sym, got, i)
		}
	}
	for i := symReserved; i < len(symTable); i++ {
		if symTable[i] != "" {
			t.Errorf("symTable[%d] should be reserved, got %q", i, symTable[i])
		}
	}
	seen := map[string]bool{}
	for i := range symReserved {
		if seen[symTable[i]] {
			t.Errorf("duplicate symTable entry %q", symTable[i])
		}
		seen[symTable[i]] = true
	}
	for i := range escapeCode {
		if got := asciiSimple[simpleTable[i]]; got != int8(i) {
			t.Errorf("asciiSimple[%q] = %d, want %d", string(simpleTable[i]), got, i)
		}
	}
}

// TestCostModelMatchesWrittenBits closes the loop between the two halves of the
// encoder: scan prices a tokenisation with the cost constants, and writeToken
// emits it with the field widths. If the two ever disagreed the payload length
// would be wrong, so this sums the widths actually written and checks them
// against the cost the scan reported.
func TestCostModelMatchesWrittenBits(t *testing.T) {
	width := func(tk tok) int {
		switch tk.op {
		case "letter", "space", "simple", "long", "dash":
			return 5
		case "symbol":
			return costSymbol
		case "simpleSym":
			return costSimple
		case "number":
			return costNumber
		case "escape":
			return costEscapeBase + costEscapeByte*tk.arg
		}
		t.Fatalf("unknown token %q", tk.op)
		return 0
	}
	rng := rand.New(rand.NewPCG(23, 24))
	checked := 0
	for _, pool := range [][]string{alphabet, {"a", "B", " ", "1", "2", "ñ", "-"}, {"x", "X", "0", "9", "@"}} {
		for range 2000 {
			s := randString(rng, pool, rng.IntN(40))
			buf := Append(nil, s)
			if buf[0]&flagPacked5 == 0 {
				continue
			}
			sum := 0
			for _, tk := range dumpTokens(t, buf) {
				sum += width(tk)
			}
			if want := scanBits(s); sum != want {
				t.Fatalf("%q: wrote %d bits, the scan priced it at %d", s, sum, want)
			}
			if got := payloadBytes(sum); got != len(buf)-frameOverhead(got) {
				t.Fatalf("%q: %d payload bytes for %d bits, frame is %d", s, got, sum, len(buf))
			}
			checked++
		}
	}
	if checked < 1000 {
		t.Fatalf("only %d packed frames checked", checked)
	}
}
