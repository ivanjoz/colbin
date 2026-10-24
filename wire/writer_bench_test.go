package wire

import "testing"

// BenchmarkNarrowRecord is a ten-field record through the narrow writer's
// methods, as a caller of the wire API writes one: the values a record holds
// most, a small count, a zero, an id, a negative, a code, a float.
func BenchmarkNarrowRecord(b *testing.B) {
	writer := Writer{Buffer: make([]byte, 0, 256)}
	b.ReportAllocs()
	for b.Loop() {
		writer.Buffer = writer.Buffer[:0]
		writer.Uint(0, 3)
		writer.Int(1, 0)
		writer.I32(2, 1_234_567)
		writer.Int(3, -42)
		writer.U32(4, 200)
		writer.U16(5, 7)
		writer.Bool(6, true)
		writer.String(7, "PEN")
		writer.F64(8, 12.5)
		writer.String(9, "a comment of some length")
	}
}
