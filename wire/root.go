package wire

// A typed narrow message's root.
//
// Every other message starts with a root byte in 0xD0..0xDF that says what it
// is (codec/root.go). A struct at four key bits, with no schema section and no
// dictionary, is what nearly every message is, and protobuf spends nothing on
// one: so it spends nothing either, and its key run is the whole message. A
// reader tells the two apart by the first byte — one in the root range is a
// root, anything else the key run's first field — and an empty message is the
// struct with every field zero.
//
// A key run can begin in the root range itself: a first field of key 13 has a
// header of 0xD0..0xDF. That message keeps its root byte, 0xD0, which the
// reader steps over as it always did. Only a record whose keys 0 to 12 are all
// zero and whose key 13 is not pays it.
//
// What this gives up is recognising a message by its first byte: a typed
// narrow one can start with almost any byte, so a stream that carries colbin
// beside other data has to say which is which itself.

// RootNarrow is the root byte of a typed narrow message, written only when its
// key run would otherwise begin in the root range.
const RootNarrow = 0xD0

// rootLast is the last byte of the root range, which begins at RootNarrow.
const rootLast = 0xDF

// IsRoot reports whether b is a root byte rather than a key run's first field.
func IsRoot(b byte) bool { return b-RootNarrow <= rootLast-RootNarrow }

// FinishNarrow ends a typed narrow message whose key run was written from
// start on with no root byte in front: it puts the root byte back when the run
// begins in the root range, which a reader would take for a root.
//
// It is spelled to fit the inliner's budget, as every encode ends in it.
func FinishNarrow(buffer []byte, start int) []byte {
	if start < len(buffer) && buffer[start]-RootNarrow <= rootLast-RootNarrow {
		return insertRoot(buffer, start)
	}
	return buffer
}

// insertRoot is FinishNarrow's rare half, out of line so that the check stays
// inside the inliner's budget.
//
//go:noinline
func insertRoot(buffer []byte, start int) []byte {
	buffer = append(buffer, 0)
	copy(buffer[start+1:], buffer[start:])
	buffer[start] = RootNarrow
	return buffer
}

// NarrowBody is the key run of a typed narrow message, whether or not it has
// its root byte, and false for a message whose first byte is any other root.
func NarrowBody(data []byte) ([]byte, bool) {
	switch {
	case len(data) == 0 || !IsRoot(data[0]):
		return data, true
	case data[0] == RootNarrow:
		return data[1:], true
	}
	return nil, false
}
