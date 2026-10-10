// Package wire is colbin's field framing: a byte-aligned run of
// [key][descriptor][payload] fields where a field holding its zero value is not
// written at all.
//
//	w := wire.Writer{}          // four-bit keys
//	w.U32(0, companyID)
//	w.String(1, name)
//	send(w.Buffer)
//
//	r := wire.NewReader(message)
//	for r.More() {
//	    switch r.Key() {
//	    case 0:
//	        companyID = r.U32()
//	    case 1:
//	        name = r.String()
//	    default:
//	        r.Skip() // a field this reader does not know: step over it
//	    }
//	}
//	err := r.Err()
//
// # Two key widths
//
//	Writer / Reader     narrow.go  4-bit key sharing a byte with a 4-bit nibble
//	Writer8 / Reader8   wide.go    8-bit key and a descriptor byte of its own
//
// A narrow nibble has no room for a class, so it spends its four bits on the
// one thing a reader needs before it knows the type — how long the field is —
// and a narrow reader takes what the bytes mean from the schema. Both widths
// therefore step over a key they do not know. A wide descriptor names its class
// as well, which is what a self-describing value (dynamic.go) needs. The width
// is chosen per key run: a nested struct says its own in the field that opens
// it.
//
// The two widths are separate types rather than one type with a flag because a
// width the compiler cannot see is a width it cannot fold. The value encodings
// under them — magnitudes, blobs, arrays — are shared.
//
// # Composites
//
// A nested struct, a list, a map and a table each carry a byte length, so a
// reader at either width can skip one without its sub-schema. A table is a slice of structs
// transposed: its body is a key run of columns, each one encoded by the column
// codec, with the row count stated once. A table's column keys use the wider of
// the enclosing run's width and the row type's; a reader takes the row type's
// width from the schema, as it does for a narrow list's elements.
//
// # Reading untrusted input
//
// The writer trusts its caller: it checks no key and cannot fail. The reader
// trusts nothing. Every size is checked against the bytes left, a declared
// element count may not exceed the bytes that would hold it, and a size this
// platform cannot address is refused rather than wrapped. The one number not
// bounded by the message is a table's row count — a column of identical values
// is a few bytes however long — so a caller allocating for a table must budget
// rows itself; codec does.
//
// Lengths and counts are at most four bytes wide, so a single narrow field,
// composite body, list or string array is limited to 2^32-1 bytes or elements.
// The narrow writer panics past that rather than write a field nothing can read;
// the wide writer does not check.
package wire
