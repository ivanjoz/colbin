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
//	0xD0 .. 0xDF   a root byte. Sixteen detail combinations.
//	everything else  a typed narrow message's first field.
//
// A typed narrow message — four key bits, no schema section, no dictionary —
// has no root byte: protobuf spends nothing on one, and most messages are one.
// Its key run is the whole message, and an empty message is the record with
// every field zero. One whose first field has key 13, and so a header in the
// range, keeps 0xD0 in front (wire/root.go).
//
// Four detail bits, allocated:
//
//	0x08  wide     eight-bit keys inside, rather than four
//	0x04  schema   a schema section precedes the body (see schema.go)
//	0x02  dict     a dictionary section precedes the body (see dictionary.go)
//	0x01  dynamic  the body is a dynamic document, not a key run (compact.go)
//
// A dynamic root has no key width and no dictionaries, so under it the wide
// bit is unassigned and the dict bit says which document it is:
//
//	0xD1  a map[string]any: its entries, compact, to the end of the message
//	0xD3  a []any: one compact value
//	0xD5, 0xD7  the same behind a schema section, for the structs a TYPED
//	      value inside names
//
// # What it gave up
//
// The range used to be a guarantee: no colbin message began outside it, so an
// application could take the other 240 values for its own framing. A typed
// narrow message now begins with almost any byte, so that guarantee is gone,
// and a stream that carries colbin beside other data tags it itself. It was a
// byte on every message, which on a configuration document of twenty bytes
// was what protobuf won by.
//
// Inside the range, a combination this version does not assign is still
// refused (rootParts), which is the room the format has to grow into.
const (
	// RootFirst and RootLast bound every first byte colbin writes or accepts.
	RootFirst byte = 0xD0
	RootLast  byte = 0xDF
)

// Root detail bits, the low nibble of the root byte.
const (
	rootWide byte = 0x08
	// rootSchema says a schema section sits between this byte and the body, so
	// that the message describes its own type. MarshalSelfDescribing writes it
	// and rootOf steps over it. See schema.go.
	rootSchema byte = 0x04
	// rootDynamicMap is the root of a message that is a map[string]any, and
	// rootDynamicList, with rootDynamicListBit, of one that is a []any. A
	// document at the root costs the root byte and nothing else: a map's
	// entries need no count when the message ends where they do. See compact.go.
	rootDynamicMap     byte = RootFirst | 0x01
	rootDynamicListBit byte = 0x02
	rootDynamicList         = rootDynamicMap | rootDynamicListBit
)

// What a plan's root is when it is a dynamic document: see typePlan.dynamicRoot.
const (
	dynamicRootMap  uint8 = 1
	dynamicRootList uint8 = 2
)

// IsColbin reports whether data begins with a root byte. A typed narrow message
// usually does not (see the file comment), so this says which kind of message
// data is, not whether it is colbin.
//
// It is a range check and not a validation: a message can start correctly and
// still be truncated or corrupt further in.
func IsColbin(data []byte) bool {
	return len(data) > 0 && data[0] >= RootFirst && data[0] <= RootLast
}
