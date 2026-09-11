package bytealigned

// §2.6 says a float rides in the integer shape "carrying its IEEE-754 pattern
// with trailing zero bytes trimmed". Under the little-endian rule the same
// section sets, that trims the *high* bytes — the exponent and sign, which are
// never zero for a non-zero float. So as written it saves nothing.
//
// The informative end of a float is the opposite of an integer's: an integer's
// high bytes are the zeros, a float's low bytes are. This measures both
// directions, so the fix is priced rather than assumed.

import (
	"math"
	"math/rand/v2"
	"testing"
)

// trimHigh is the rule as written: drop zero bytes from the most significant
// end, which is what an integer wants.
func trimHigh(pattern uint64) int {
	n := 0
	for pattern != 0 {
		n++
		pattern >>= 8
	}
	return n
}

// trimLow is the fix: drop zero bytes from the least significant end, which is
// the same as writing the pattern big-endian and trimming its tail.
func trimLow(pattern uint64) int {
	if pattern == 0 {
		return 0
	}
	n := 8
	for pattern&0xFF == 0 {
		n--
		pattern >>= 8
	}
	return n
}

func TestFloatTrim(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	n := 1000

	shapes := map[string][]float64{}

	round := make([]float64, n) // quantities, multipliers, percentages
	for i := range round {
		round[i] = float64(r.IntN(2000)) / 4
	}
	shapes["round quarters"] = round

	from32 := make([]float64, n) // a float64 field fed by float32 data
	for i := range from32 {
		from32[i] = float64(float32(r.Float64() * 1000))
	}
	shapes["float32 values"] = from32

	cents := make([]float64, n) // money as a float, two decimals
	for i := range cents {
		cents[i] = float64(r.IntN(5000000)) / 100
	}
	shapes["prices, 2dp"] = cents

	coords := make([]float64, n) // latitudes: nothing is round
	for i := range coords {
		coords[i] = r.Float64()*180 - 90
	}
	shapes["coordinates"] = coords

	for name, vals := range shapes {
		var high, low int
		for _, v := range vals {
			pattern := math.Float64bits(v)
			high += trimHigh(pattern)
			low += trimLow(pattern)
		}
		t.Logf("%-15s  as written (trim high) %.2f B/value   fixed (trim low) %.2f B/value",
			name, float64(high)/float64(n), float64(low)/float64(n))
	}
}
