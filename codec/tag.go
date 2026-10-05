package codec

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// noFieldID is what a tag carrying no number reports, and what a field is left
// holding until assignKeys derives a key for it. Zero cannot do the job: ids are
// one-based, so `cb:"0"` parses to zero and has to stay a named error rather
// than quietly deriving a key behind a reader's back.
const noFieldID = -1

// The `cb` struct tag, which is the whole of what a type declares to colbin.
//
//	Name string `cb:"3"`        field id 3
//	Name string `cb:"label,3"`  field id 3, called "label" by a reader that
//	                            needs a name
//	Name string `cb:"-"`        not encoded at all
//
// # Ids are one-based
//
// The first field is `cb:"1"`. Narrow keys hold ids 1..16 and wide keys hold
// 1..255; past that a type is split into pages of 255, up to maxFieldID (see
// pages.go).
//
// The wire key is the id minus one. It has to be: the key is a bare nibble or a
// bare byte with every value spoken for, so there is no spare encoding to give
// away to a reserved zero. `cb:"1"` therefore writes key 0, and `cb:"16"` writes
// key 15. Source counts from one and bytes count from zero: `FieldIDs` reports
// the id, as the tag says it, and the schema section carries the key, as the
// wire does.
//
// # An unnumbered field
//
// A field without a number gets a key from its name: `fnv8` of the name, then a
// linear probe past whatever is already taken. That is what lets a struct be
// encoded with no tags at all.
//
// The key is what goes on the wire, so a reader in another language needs to
// agree on it — either by numbering both sides, or by running the same hash.
//
// A derived key lands anywhere in 0..255, which four key bits cannot hold, so a
// type with any derived key uses eight-bit keys. Numbering the fields is
// therefore also how a type asks for the narrow width.
//
// An id outside 1..maxFieldID is returned as written rather than rejected here, so that
// planFor can name the offending field. A token that starts like a number and
// does not parse as one — `-1`, or a number past what an int holds — is refused
// rather than taken for a name.
func parseCbTag(field reflect.StructField) (name string, explicitID int, skip bool, err error) {
	name, explicitID = field.Name, noFieldID
	tag := field.Tag.Get("cb")
	if tag == "-" {
		return "", noFieldID, true, nil
	}
	if tag == "" {
		return field.Name, noFieldID, false, nil
	}
	named := false
	for _, token := range strings.Split(tag, ",") {
		if token == "" {
			continue
		}
		if numeric(token) {
			id, err := strconv.Atoi(token)
			if err != nil || id < 0 {
				return "", 0, false, fmt.Errorf("the tag `cb:%q` holds %q, which is not a field id", tag, token)
			}
			explicitID = id
		} else if !named {
			name, named = token, true
		}
	}
	return name, explicitID, false, nil
}

// numeric says a tag token is meant as a number: a digit, or a sign and one.
func numeric(token string) bool {
	if token[0] == '-' || token[0] == '+' {
		token = token[1:]
	}
	return token != "" && token[0] >= '0' && token[0] <= '9'
}

// fnv8 is FNV-1a folded to a byte. Every port derives untagged keys with it, so
// it is format rather than an implementation detail.
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
