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
//
// Codec[T] is the same format through a cached, typed handle: the field layout
// and the mode decision are resolved once instead of on every call, and encoding
// onto a reused buffer allocates nothing. Use it for many small messages.
//
// SetOmitEmpty turns on omit-empty encoding, where a column holding nothing but
// empty values is written as its type byte alone rather than as a slot per
// record. It also lets compact mode carry pointer fields, at the price of nil
// and a pointer to the zero value becoming the same thing.
package colbin
