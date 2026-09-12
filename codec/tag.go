package codec

import (
	"reflect"
	"strconv"
	"strings"
)

// The `cb` struct tag, which is the whole of what a type declares to colbin.
//
//	Name string `cb:"3"`        field id 3
//	Name string `cb:"label,3"`  field id 3, called "label" by a reader that
//	                            needs a name
//	Name string `cb:"-"`        not encoded at all
//
// A field without a number gets one from its name: `fnv8` of the name, then a
// linear probe past whatever is already taken. That is what lets a struct be
// encoded with no tags at all.
//
// The id is still what goes on the wire, so a reader in another language needs
// the same numbers — either by tagging both sides, or by running the same hash.
// `FieldIDs` prints whatever a type resolved to.
//
// A derived id lands anywhere in 0..255, which four key bits cannot hold, so a
// type with any derived id uses eight-bit keys. Numbering the fields is
// therefore also how a type asks for the narrow width.
func parseCbTag(field reflect.StructField) (name string, explicitID int, skip bool) {
	name, explicitID = field.Name, -1
	tag := field.Tag.Get("cb")
	if tag == "-" {
		return "", -1, true
	}
	if tag == "" {
		return field.Name, -1, false
	}
	named := false
	for _, token := range strings.Split(tag, ",") {
		if token == "" {
			continue
		}
		if id, err := strconv.Atoi(token); err == nil {
			explicitID = id
		} else if !named {
			name, named = token, true
		}
	}
	return name, explicitID, false
}

// fnv8 is FNV-1a folded to a byte, which is the hash the previous format used
// and therefore the one every already-written reader expects.
func fnv8(name string) uint8 {
	const (
		offset32 = 2166136261
		prime32  = 16777619
	)
	hash := uint32(offset32)
	for index := range len(name) {
		hash ^= uint32(name[index])
		hash *= prime32
	}
	// Folding the whole word down rather than taking the low byte: the low byte
	// of FNV-1a moves with only the last character or two.
	return uint8(hash ^ (hash >> 8) ^ (hash >> 16) ^ (hash >> 24))
}

// probeFieldID returns start, or the next free id after it. It wraps at 256,
// which terminates because a type is refused above 256 encodable fields.
func probeFieldID(start uint8, taken map[uint8]string) uint8 {
	id := start
	for _, used := taken[id]; used; _, used = taken[id] {
		id++
	}
	return id
}
