package codec

// The first byte of a colbin message, and the 240 values colbin promises never
// to write.
//
// # Why the root byte looks like it does
//
// A message is one value: a descriptor, then its payload. The root descriptor is
// an ordinary K8 descriptor — `1 ccc dddd` — and its class is STRUCT, because
// the format encodes a struct and nothing else. STRUCT is class 5, so:
//
//	1 101 dddd  =  0xD0 | detail
//
// That is the whole reason a colbin message starts with 0xD-something. It was
// not chosen as a magic number; it is what the descriptor rules produce. But it
// has the useful property of a magic number anyway, and this file makes that
// explicit rather than incidental.
//
// # The reservation
//
//	0xD0 .. 0xDF   colbin. Sixteen detail combinations, two used today.
//	everything else  never written by colbin, and rejected on read.
//
// Four detail bits, allocated:
//
//	0x08  wide     eight-bit keys inside, rather than four
//	0x04  schema   a schema section precedes the body (see JSON_MODE_PLAN.md)
//	0x02  —        unallocated
//	0x01  —        unallocated
//
// # What an application may do with the rest
//
// **Any first byte outside 0xD0..0xDF is guaranteed not to be colbin**, now or
// in any future version that keeps a struct at the root. An application may use
// those 240 values as its own framing — a envelope tag, a compression marker, a
// protocol discriminator — and `IsColbin` will tell the two apart with no
// ambiguity and no length prefix.
//
// A worked example: prefix a compressed payload with 0x1F (or any byte under
// 0xD0), leave colbin messages bare, and a reader dispatches on one comparison.
//
// The one thing an application must not assume is that *every* byte in
// 0xD0..0xDF is currently valid. Twelve of the sixteen are unassigned and a
// reader must refuse them, which is what rootOf does — that is what leaves room
// for the format to grow without an application's framing colliding with it.
const (
	// RootFirst and RootLast bound every first byte colbin writes or accepts.
	RootFirst byte = 0xD0
	RootLast  byte = 0xDF
)

// Root detail bits, the low nibble of the root byte.
const (
	rootWide   byte = 0x08
	rootSchema byte = 0x04 // reserved; not yet written. See JSON_MODE_PLAN.md.
)

// IsColbin reports whether data begins with a byte in colbin's reserved range.
//
// It is a range check and not a validation: a message can start correctly and
// still be truncated or corrupt further in. What it answers is the dispatch
// question — "is this mine, or is it the other thing my protocol carries?" — for
// which a first-byte comparison is enough and a full parse is not needed.
func IsColbin(data []byte) bool {
	return len(data) > 0 && data[0] >= RootFirst && data[0] <= RootLast
}
