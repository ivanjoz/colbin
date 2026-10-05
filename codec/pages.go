package codec

// Pages: a struct with more fields than one key run holds.
//
// A key is a byte, so a run has 256 of them. A type numbered past 255 is split
// into pages of 255 fields,
//
//	page = (id-1) / 255
//	key  = (id-1) % 255
//
// and key 255 of each page holds the next page, as an ordinary nested struct.
// So the wire needs nothing new: a reader that knows no pages reads a struct
// under key 255, and one that does merges it into the object around it, which
// the schema section's page flag tells it to do.
//
// Each page is a plan over the same Go record, and the link is an opStruct field
// at offset 0 — the next page's fields carry the record's own offsets. The
// encoder and the typed decoder therefore treat it as any nested struct, with
// one difference: an empty page is not written, and since the link is a page's
// last field, a page holding nothing but an empty link is empty too. Trailing
// pages cost nothing until a field on them is set.
//
// Every field of a paged type needs a number. A derived key lands anywhere in
// 0..255, including on the link.
//
// A page is a level of nesting to every walk, so their number is bounded well
// inside maxSchemaDepth.

import (
	"fmt"
	"reflect"

	"github.com/ivanjoz/colbin/wire"
)

const (
	// pageFields is how many fields a page holds: keys 0..254.
	pageFields = 255
	// pageLink is the key that holds the next page.
	pageLink uint8 = 255
	// maxPages bounds a paged type, and maxFieldID is the id that fills it.
	maxPages   = 16
	maxFieldID = maxPages * pageFields
)

// paged says the declared ids need more than one page.
func paged(declared []int) bool {
	for _, id := range declared {
		if id > pageFields {
			return true
		}
	}
	return false
}

// linksPage says a field is the link to the next page of its type.
func (field *planField) linksPage() bool {
	return field.op == opStruct && field.sub != nil && field.sub.page
}

// paginate spreads a plan's fields over pages, the plan itself being the first.
// It is assignKeys for a paged type, and leaves the plan ready for finish.
func (plan *typePlan) paginate(structType reflect.Type, names, fieldNames []string, declared []int) error {
	pageCount := 0
	owner := make(map[int]string, len(declared))
	for index, id := range declared {
		if id == noFieldID {
			return fmt.Errorf(
				"colbin: %s numbers a field past %d, which puts it on pages, "+
					"and a paged type numbers every field: %s has no id",
				typeName(structType), pageFields, fieldNames[index])
		}
		if other, clash := owner[id]; clash {
			return fmt.Errorf("colbin: field id %d is on both %s.%s and %s.%s",
				id, typeName(structType), other, typeName(structType), fieldNames[index])
		}
		owner[id] = fieldNames[index]
		pageCount = max(pageCount, (id-1)/pageFields+1)
	}

	pages := make([]*typePlan, pageCount)
	pages[0] = plan
	for at := 1; at < pageCount; at++ {
		pages[at] = &typePlan{page: true}
	}
	fields := plan.fields
	plan.fields, plan.names = nil, nil
	plan.ids = make([]uint16, len(declared))
	for index, id := range declared {
		page := pages[(id-1)/pageFields]
		field := fields[index]
		field.key = uint8((id - 1) % pageFields)
		page.fields = append(page.fields, field)
		page.names = append(page.names, names[index])
		plan.ids[index] = uint16(id)
	}

	// Last page first, so that each link points at a page already finished. The
	// link goes last in its page, which is what lets an empty one take the page
	// with it.
	dynamic := false
	for at := pageCount - 1; at >= 0; at-- {
		page := pages[at]
		if at+1 < pageCount {
			page.fields = append(page.fields, planField{key: pageLink, op: opStruct, sub: pages[at+1]})
			page.names = append(page.names, "")
		}
		for _, field := range page.fields {
			if field.key >= wire.MaxFields {
				page.anyKeyPastNarrow = true
			}
		}
		page.hasDynamic = anyDynamic(page.fields)
		dynamic = dynamic || page.hasDynamic
		if at > 0 {
			page.finish()
		}
	}
	// The first page answers for the type: MarshalSelfDescribing asks it whether
	// a value can reach a struct the type never names. It is wide already, so
	// this cannot change its width.
	plan.hasDynamic = dynamic
	return nil
}
