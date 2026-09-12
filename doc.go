// Package colbin is a byte-aligned binary format for Go structs.
//
// This file is the package's overview; colbin.go is its API.
//
// # One format
//
// A message is a root descriptor byte and then a sequence of fields:
//
//	[key][descriptor][payload]
//
// Nothing is packed across a byte boundary. No size is a varint — a header
// carries the common size and, when it does not fit, names the width of the one
// that follows, so no read is ever a loop whose trip count is data. A field
// holding its zero value is not written at all, which is where most of the
// saving comes from.
//
// There used to be three modes — a columnar one, a bitstream one and this — and
// a byte at the front to tell them apart. There is now one, and the byte at the
// front is the root value's own descriptor: it says the class and the key width,
// which is everything the version bytes carried that was not simply a
// consequence of the layout.
//
// # Key widths
//
// A field id is four bits or eight, chosen per key run rather than per message.
// Four is the default and the fast path. Eight costs a byte per present field
// and buys 256 ids, the ability to skip a field the reader has never heard of,
// and the packed5 string encoding.
//
// A type goes wide when it has a field id above fifteen, or when SetPacked5 is
// on and it has a string to spend it on. Nothing else changes.
//
// # Layers
//
//	wire     the format: field framing, both key widths, composites, tables
//	column   the column codec: blocks of 128 residuals at a chosen bit width
//	codec    the reflection façade, a source generator for the hot path, and
//	         the schema section that lets a reader without the Go type get JSON
//	packed5  an opt-in string packing, off by default
//
// A caller that knows its Go type can drive wire.Writer directly and skip the
// reflection: that is about three times faster than the façade, and
// codec.Generate emits the source so it does not have to be written by hand.
//
// # A reader without the Go type
//
// The type is not on the wire, so a reader that has not got it cannot name a
// field or tell a float from an integer. Schema is the type written out as
// bytes — send it once per connection and turn messages into JSON with ToJSON,
// or put it in front of one message with MarshalSelfDescribing. The body is
// unchanged either way, and so is everything an ordinary decode costs.
package colbin
