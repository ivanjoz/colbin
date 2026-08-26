// Package colbin implements a compact columnar binary format for homogeneous
// Go records. Integer columns use the adaptive varint codec and strings use
// self-delimiting packed5 frames; nested structs, slices, maps, pointers, and
// interface values are also supported.
//
// Marshal and Unmarshal derive the wire schema from the Go type and cb struct
// tags, so callers must decode with a compatible type.
//
// MarshalJSON writes the same payload behind a schema section naming the fields
// and recording what the columns leave out, so a reader with no matching Go type
// can turn the message into JSON with DecodeJSON or DecodeAny.
package colbin
