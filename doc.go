// Package colbin implements a compact columnar binary format for homogeneous
// Go records. Numeric columns use frame-of-reference delta encoding and
// bit-packing; nested structs, slices, maps, pointers, and interface values are
// also supported.
//
// Marshal and Unmarshal derive the wire schema from the Go type and cb struct
// tags, so callers must decode with a compatible type.
package colbin
