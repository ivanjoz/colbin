package codec

// Wire format constants shared by encoder and decoder.

// magic/version byte prefixing every message. Bump on any wire-format change.
const formatVersion byte = 0x02

// reserved field-id: 255 is never assigned, so a struct may have at most 254
// fields and the id space always has a free "terminator" slot.
const reservedFieldID uint8 = 255

// Field type codes occupy the low three bits of a column's type byte. Float
// columns also use precision and empty bits; other types own their payload format.
const (
	ftInt    uint8 = 0 // varint-compressed integer column; bool encoded here too
	ftFloat  uint8 = 1 // IEEE-754, width from precision (float32/64)
	ftString uint8 = 2 // consecutive self-delimiting packed5 frames
	ftBytes  uint8 = 3 // varint length column + concatenated raw bytes
	ftArray  uint8 = 4 // varint length column + flattened element sub-column
	ftStruct uint8 = 5 // nested sub-table of columns
	ftMap    uint8 = 6 // varint lengths + flattened keys column + flattened values column
	ftAny    uint8 = 7 // interface{}: N self-describing tagged values (see any.go)
)
