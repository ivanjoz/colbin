package codec

import (
	"fmt"
	"reflect"
	"sync"
)

// A root that is not a struct.
//
// # The problem
//
// The format encodes a struct and nothing else, and root.go explains why that is
// worth keeping: the first byte of a message is an ordinary STRUCT descriptor,
// which is what reserves 0xD0..0xDF and lets an application own the other 240
// values. A root that could also be a list would spend one of the four detail
// bits, and every port — Rust, the browser module — would have to grow a second
// root shape before it could read one.
//
// But a slice of records is a real thing to store. An ORM that keeps a complex
// column as one blob has `[]Grant` in its hand, not a struct wrapping one, and
// so does any caller with a batch to write.
//
// # The envelope
//
// A non-struct root is encoded as a one-field message: key 0, the value, and
// nothing else. `Marshal([]Grant{...})` writes what `Marshal(struct{ V []Grant
// }{...})` writes, and `Unmarshal(data, &grants)` reads it back.
//
// The root stays a struct, so:
//
//   - the reserved range still means what root.go says it means,
//   - Rust and the browser read the blob today, with no port and no new shape:
//     they see an ordinary message with one field,
//   - the value itself is encoded by the machinery that already exists — a slice
//     of structs becomes a LIST or a TABLE exactly as it would in any field, so
//     a long one still transposes into columns.
//
// It costs two bytes against a hypothetical root list: the key and the
// descriptor. That is the whole price, and it buys not touching the format.
//
// # The wrapper is not part of the document
//
// A Go caller never sees it: Unmarshal fills the slice, not a struct holding
// one. A reader working from the schema would, because the section describes
// what is on the wire and what is on the wire is a one-field struct called
// `Value` — so `Marshal([]Grant{...})` would come back as `{"Value":[...]}`
// rather than as the array.
//
// So the envelope is marked in the section (schemaEnvelope) and every walker
// unwraps it: the single field's value *is* the document. That makes the two
// sides agree — Go gets the slice, JSON gets the array — and it costs one flag
// bit in a byte the structDef was already spending.
//
// # Why there is no copy
//
// A struct with one field at offset 0 has the address of that field, so the
// pointer a caller hands in *is* the pointer the envelope plan reads through.
// The synthetic type exists only to carry a plan; no value is ever moved into
// it, and encoding through an envelope costs one cached map lookup over
// encoding a struct directly.

// envelopeFieldName is what the *section* calls the synthetic field.
//
// It reaches no document, because every walker unwraps it — so what it is for is
// agreeing with the browser module, which builds the same envelope round a JSON
// array and names it this (web/assembly/infer.ts, ENVELOPE_FIELD). Two ports
// writing the same document have no business writing different bytes for it.
const envelopeFieldName = "rows"

// envelopeGoName is what Go calls the field. reflect.StructOf needs an exported
// name and the section needs the one above, so the tag carries the second and
// this is only ever seen in an error naming the synthetic type.
const envelopeGoName = "Value"

// envelopeField is the tag: the name a reader sees, and id one. The first id, so
// the key is zero and the envelope is the cheapest message the format can
// express — one narrow key and one descriptor.
const envelopeField = envelopeFieldName + ",1"

var (
	envelopeCache sync.Map // reflect.Type -> reflect.Type
	// envelopeTypes is the set of synthetic types envelopeFor has built, so that
	// a plan can be told it is an envelope rather than a type somebody wrote.
	//
	// A flag passed down into buildPlan would be the obvious alternative and is
	// worse: the plan cache is keyed on the struct type alone, so the flag would
	// have to be right on every path that reaches the same type, and setting it
	// on a plan already published to the cache is a race. A type is either
	// synthetic or it is not, and this says which.
	envelopeTypes sync.Map // reflect.Type -> struct{}
)

// envelopeFor returns the one-field struct type that carries valueType, building
// and caching it on first use.
func envelopeFor(valueType reflect.Type) reflect.Type {
	if cached, ok := envelopeCache.Load(valueType); ok {
		return cached.(reflect.Type)
	}
	envelope := reflect.StructOf([]reflect.StructField{{
		Name: envelopeGoName,
		Type: valueType,
		Tag:  reflect.StructTag(`cb:"` + envelopeField + `"`),
	}})
	// Recorded before the type is published, so that a plan built for it — which
	// can only happen after envelopeFor has returned — always finds the mark.
	envelopeTypes.Store(envelope, struct{}{})
	envelopeCache.Store(valueType, envelope)
	return envelope
}

// isEnvelope reports whether a struct type is one of the synthetic wrappers
// above rather than a type the caller declared.
func isEnvelope(structType reflect.Type) bool {
	_, ok := envelopeTypes.Load(structType)
	return ok
}

// planForRoot resolves the plan a message root is written through: the type's own
// plan when it is a struct, and its envelope's otherwise.
//
// The returned plan is always read through a pointer to rootType, which is sound
// for both cases — see the note on offsets above.
func planForRoot(rootType reflect.Type) (*typePlan, error) {
	if rootType.Kind() == reflect.Struct {
		return planFor(rootType)
	}
	if !envelopable(rootType) {
		return nil, fmt.Errorf(
			"colbin: the format encodes a struct, or a slice or map at the root, got %s",
			rootType.Kind())
	}
	plan, err := planFor(envelopeFor(rootType))
	if err != nil {
		// The error names the envelope's synthetic field, which the caller never
		// wrote. Say what they actually handed in instead.
		return nil, fmt.Errorf("colbin: cannot encode %s at the root: %w", rootType, err)
	}
	return plan, nil
}

// envelopable reports whether a root kind is one worth wrapping.
//
// Slices and maps are, because they are containers a caller genuinely holds. A
// bare int or string is not: a message carrying one scalar and no name for it is
// a byte array with extra steps, and refusing it keeps the error at the call
// site rather than in whatever reads the blob later.
func envelopable(rootType reflect.Type) bool {
	switch rootType.Kind() {
	case reflect.Slice, reflect.Array, reflect.Map:
		return true
	}
	return false
}
