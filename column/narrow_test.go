package column

import (
	"bytes"
	"math"
	"math/rand"
	"testing"
)

// An int32 column must encode to the bytes its widened int64 twin does,
// whichever kernels run: shapes that make each transform win, odd lengths for
// the kernels' tails, and the extremes of the type.
func TestAppendArrayInt32MatchesInt64(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	shapes := map[string]func(i int) int32{
		"constant":  func(int) int32 { return 42 },
		"small":     func(int) int32 { return int32(rng.Intn(50)) },
		"signed":    func(int) int32 { return int32(rng.Intn(2001) - 1000) },
		"ascending": func(i int) int32 { return int32(i*3 + rng.Intn(3)) },
		"dates":     func(int) int32 { return int32(8000 + rng.Intn(2500)) },
		"wide":      func(int) int32 { return int32(rng.Uint32()) },
		"extremes": func(i int) int32 {
			return [...]int32{math.MinInt32, math.MaxInt32, 0, -1}[rng.Intn(4)]
		},
	}
	for name, shape := range shapes {
		for _, n := range []int{0, 1, 5, 32, 33, 127, 128, 129, 257, 1000, 4099} {
			narrow := make([]int32, n)
			wide := make([]int64, n)
			for i := range narrow {
				narrow[i] = shape(i)
				wide[i] = int64(narrow[i])
			}
			got := AppendArray([]byte{9}, narrow)
			want := AppendArray([]byte{9}, wide)
			if !bytes.Equal(got, want) {
				t.Fatalf("%s n=%d: int32 column encodes differently from int64", name, n)
			}
		}
	}
}
