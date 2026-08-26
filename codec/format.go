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

// jsonFormatVersion prefixes a self-describing message written by MarshalJSON.
// Its body is byte-for-byte the body a formatVersion message would carry; the
// only difference is the schema section sitting between the version byte and the
// body:
//
//	[jsonFormatVersion:1] [schemaLen:uvarint] schema body
//
// A reader that has the Go type skips the schema and decodes as usual (see
// Unmarshal); a reader that does not uses the schema to produce JSON (see
// DecodeAny / DecodeJSON).
const jsonFormatVersion byte = 0x03

// Scalar kinds recorded in a schema descriptor for ftInt/ftFloat columns. The
// wire carries only the ftInt class, so width and signedness — needed both to
// find the column's end and to render the value — come from here. Bool is a
// kind rather than a type class because it travels as an ftInt column.
const (
	skBool    uint8 = 0
	skInt8    uint8 = 1
	skInt16   uint8 = 2
	skInt32   uint8 = 3
	skInt64   uint8 = 4
	skInt     uint8 = 5
	skUint8   uint8 = 6
	skUint16  uint8 = 7
	skUint32  uint8 = 8
	skUint64  uint8 = 9
	skUint    uint8 = 10
	skFloat32 uint8 = 11
	skFloat64 uint8 = 12
)

// Schema-section flag bits (the section's first byte).
const (
	schRecords      byte = 1 << 0 // body is [recordCount:uvarint] subTable
	schSingleStruct byte = 1 << 1 // records mode over a lone struct: render as an object, not an array
)

// Descriptor flag bits (a descriptor's first byte). The low three bits hold the
// ft* class; nullable and cyclic are the two type properties the columnar layout
// depends on but never writes (see null_map.go and elideEmpty).
const (
	descNullable byte = 1 << 3
	descCyclic   byte = 1 << 4
)
