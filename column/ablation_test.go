//go:build colbin_ablation

package column

import (
	"math"
	"slices"
	"testing"
)

// The ablation hook has to produce columns DecodeArray reads without being told,
// and has to put the column through the transform it names — or through raw,
// where that transform cannot carry it.
func TestForcedTransformRoundTrips(t *testing.T) {
	t.Cleanup(func() { forcedTransform = -1 })

	columns := map[string][]int64{
		"ascending": {100, 101, 103, 110, 111, 150, 151, 152, 160},
		"negative":  {-5, 3, -2, 7, 0, -1, 9},
		"constant":  {42, 42, 42, 42, 42, 42, 42, 42, 42, 42},
		"one value": {7},
		"overflow":  {math.MinInt64, math.MaxInt64, 0},
	}
	for _, force := range []uint8{trRaw, trDelta, trFOR} {
		forcedTransform = int8(force)
		for name, vals := range columns {
			encoded := AppendArray(nil, vals)
			want := force
			if force == trDelta && (len(vals) < 2 || name == "overflow") {
				want = trRaw
			}
			if got := transformOf(encoded); got != want {
				t.Errorf("forced %d, %s: wrote transform %d, want %d", force, name, got, want)
			}
			back := make([]int64, len(vals))
			if _, err := DecodeArray(encoded, len(vals), back); err != nil {
				t.Fatalf("forced %d, %s: %v", force, name, err)
			}
			if !slices.Equal(back, vals) {
				t.Errorf("forced %d, %s: %v came back as %v", force, name, vals, back)
			}
		}
	}
}
