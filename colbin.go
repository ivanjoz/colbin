// Package colbin is a byte-aligned binary format for Go structs.
//
//	data, err := colbin.Marshal(&charge)
//	err = colbin.Unmarshal(data, &back)
//
// There is one format. A message is a sequence of [key][descriptor][payload]
// fields, nothing is packed across a byte boundary, no size is a varint, and a
// field holding its zero value is not written at all. What that buys and what it
// costs is in BYTE_ALIGNED_PLAN.md; the layout itself is in wire/README.md.
//
// # Layers
//
//	wire     the format: field framing, both key widths, composites, tables
//	column   the column codec: blocks of 128 residuals at a chosen bit width
//	codec    the reflection façade over both, which is this package's engine
//	packed5  an opt-in string packing, off by default (see SetPacked5)
//
// A caller that already knows its Go type can skip the reflection entirely and
// drive wire.Writer directly — that is what Codec.Append does under the hood, and
// what codec.Generate emits source for. The straight-line form is about three
// times faster than the reflective one, and the generator exists so that is not
// a reason to write it by hand.
package colbin

import "github.com/ivanjoz/colbin/codec"

// Marshal encodes v, which must be a struct or a pointer to one.
//
// Every field needs an explicit id in its `cb` tag, and the ids decide the key
// width: sixteen or fewer, all under sixteen, and the message uses four-bit
// keys; otherwise eight-bit ones.
func Marshal(v any) ([]byte, error) { return codec.Marshal(v) }

// Append encodes v onto dst, which may be nil. It is Marshal without the
// allocation, for a caller with a buffer to reuse.
func Append(dst []byte, v any) ([]byte, error) { return codec.Append(dst, v) }

// Unmarshal decodes a message into dst, a non-nil pointer to a struct.
//
// Every field of dst is set, including the ones the message omitted: an omitted
// key means the value was zero, so the destination is cleared first rather than
// left holding whatever it had.
func Unmarshal(data []byte, dst any) error { return codec.Unmarshal(data, dst) }

// FieldIDs reports the wire key of every field, for handing to a reader in
// another language. Reading them out of the tags by hand is how they drift.
func FieldIDs(v any) (map[string]uint8, error) { return codec.FieldIDs(v) }

// RootFirst and RootLast bound the first byte of every colbin message.
//
// A message always begins in 0xD0..0xDF, because the root is a struct and the
// root descriptor is an ordinary descriptor with STRUCT in its class bits. The
// other 240 values are guaranteed never to be written by colbin, so an
// application may use them as its own framing — an envelope tag, a compression
// marker, a protocol discriminator — and tell the two apart with IsColbin.
//
// Do not assume every byte *inside* the range is valid: twelve of the sixteen
// are unassigned and a reader refuses them, which is the room the format has to
// grow into.
const (
	RootFirst = codec.RootFirst
	RootLast  = codec.RootLast
)

// IsColbin reports whether data begins with a byte in colbin's reserved range.
// It is a dispatch check, not a validation: a message can start correctly and
// still be truncated further in.
func IsColbin(data []byte) bool { return codec.IsColbin(data) }

// Codec is a handle for one type, with the plan resolved once and held. It is
// what a hot path should use: encoding a record then costs neither the type
// lookup nor the reflect entry that Marshal repeats on every call, and it
// allocates nothing.
//
//	var chargeCodec = colbin.MustCodec[Charge]()
//
//	buf := make([]byte, 0, 64)
//	for _, charge := range charges {
//	    buf = chargeCodec.Append(buf[:0], &charge)
//	    send(buf)
//	}
//
// It is safe for concurrent use.
type Codec[T any] = codec.Codec[T]

// NewCodec builds the handle for T, which must be a struct the format accepts.
func NewCodec[T any]() (*Codec[T], error) { return codec.NewCodec[T]() }

// MustCodec is NewCodec for a package-level variable, where a type error is a
// programming error and there is nobody to return it to.
func MustCodec[T any]() *Codec[T] { return codec.MustCodec[T]() }

// SetPacked5 turns the packed5 string encoding on or off for every encoder in
// the process. It is **off** by default.
//
// packed5 spends about five bits per character on upper-case alphanumerics,
// which is worth roughly a third of a short token — and costs a pass over every
// string on both sides, where a raw blob is a sub-slice of the message and a
// memcpy. The format keeps it as a per-field encoding code rather than a mode,
// so turning it on changes what the encoder chooses and nothing about what a
// decoder can read: a decoder reads either, because the field says which it is.
//
// Turn it on for a wire that is size-bound over a slow link. Leave it off for
// one that is latency-bound, which is what this format is for.
//
// It is a process-wide setting and not safe to change concurrently with
// encoding. Set it once at startup.
func SetPacked5(on bool) { codec.SetPacked5(on) }

// Packed5 reports whether the packed5 string encoding is on.
func Packed5() bool { return codec.Packed5() }
