// Package colbin implements a compact columnar binary format for homogeneous
// Go records. Integer columns use the adaptive varint codec and strings use
// self-delimiting packed5 frames; nested structs, slices, maps, pointers, and
// interface values are also supported.
//
// Marshal and Unmarshal derive the wire schema from the Go type and cb struct
// tags, so callers must decode with a compatible type.
package colbin
