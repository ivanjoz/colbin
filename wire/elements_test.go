package wire

import (
	"bytes"
	"math/rand/v2"
	"testing"
)

// appendStringElements must write what the loop does, for every size class of
// the kernel's copy and either side of the size escape, values one at a time
// and mixed, more than a chunk of them, and never past what it was given.
func TestStringElementsAgreeWithTheLoop(t *testing.T) {
	random := rand.New(rand.NewPCG(11, 12))
	text := make([]byte, 400)
	for index := range text {
		text[index] = byte(random.Uint32())
	}
	value := func(size int) string {
		start := random.IntN(len(text) - size + 1)
		return string(text[start : start+size])
	}
	var cases [][]string
	for size := range 301 {
		cases = append(cases, []string{value(size)}, []string{value(size), value(size), "", value(size)})
	}
	for _, count := range []int{2, 9, 100, 5000 + 3} {
		values := make([]string, count)
		for index := range values {
			values[index] = value([]int{0, 1, 3, 7, 15, 31, 33, 64, 255, 300}[random.IntN(10)] % 301)
		}
		cases = append(cases, values)
	}
	for _, values := range cases {
		payload := 0
		for _, value := range values {
			payload += elementSizeBytes(len(value)) + len(value)
		}
		want := appendStringElementsLoop([]byte{0xAB}, values)
		// A guard byte where the output ends, which a stray store would hit.
		buffer := make([]byte, 1, 1+payload+1)
		buffer[0] = 0xAB
		full := buffer[:cap(buffer)]
		full[len(full)-1] = 0x5A
		got := appendStringElements(buffer, values, payload)
		if !bytes.Equal(got, want) {
			t.Fatalf("%d values, the first %d bytes: wrote\n% x\nwant\n% x",
				len(values), len(values[0]), got, want)
		}
		if full[len(full)-1] != 0x5A {
			t.Fatalf("%d values: wrote past the payload", len(values))
		}
	}
}
