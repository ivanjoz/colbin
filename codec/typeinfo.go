package codec

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/viant/xunsafe"
)

// fieldMeta describes one struct field: its wire id, type class, how to read/write
// its scalar value via xunsafe, and (later phases) nested array/struct descriptors.
type fieldMeta struct {
	id       uint8          // wire field-id (FNV of name, linear-probed)
	name     string         // name used for the hash (cb tag override or Go name)
	fType    uint8          // ft* type class
	offset   uintptr        // byte offset of the field within the struct
	xf       *xunsafe.Field // xunsafe accessor for fast scalar get/set (struct fields only)
	goKind   reflect.Kind   // exact kind for int/uint/float dispatch
	bitWidth uint8          // native scalar bit width (8/16/32/64)

	// Composite descriptors. Elements of an array have no struct field,
	// so scalar elements are accessed by direct pointer cast (see value_elem.go).
	sub       *typeInfo    // ftStruct: nested sub-table (also for struct elements)
	elem      *fieldMeta   // ftArray: element descriptor; ptr: the pointee descriptor
	sliceType reflect.Type // ftArray: the slice type (to build slices on decode)
	elemSize  uintptr      // ftArray: size of one element

	// Nullability. A pointer field is nullable: elem describes the pointee,
	// accessed like an array element, and pointeeType allocates backing on decode.
	nullable    bool
	pointeeType reflect.Type

	// Maps. K is restricted to scalar/string; V may be any supported type.
	mapKey, mapVal         *fieldMeta
	mapKeyType, mapValType reflect.Type
	mapType                reflect.Type

	// Interface (ftAny). The static interface type, needed to read/set the slot
	// via reflect; the concrete value is self-describing on the wire (see any.go).
	ifaceType reflect.Type

	// jsonName is the key this field gets in JSON mode. It is read once per type
	// when the schema section is built, never on an encode or decode path, so it
	// sits at the end rather than among the fields the per-record loops touch.
	jsonName string

	// cyclic reports that this descriptor's type can reach itself — a
	// self-referential type such as `type Node struct{ Kids []Node }`. Empty
	// columns of such types are elided on the wire (see elideEmpty), since the
	// schema-driven layout would otherwise nest sub-tables forever.
	cyclic bool
}

// typeInfo is the cached, ordered field layout for a struct type.
type typeInfo struct {
	rtype  reflect.Type
	size   uintptr
	fields []fieldMeta          // in declaration order
	byID   map[uint8]*fieldMeta // wire id -> field, for decode

	// bytesPerRecord is what the last encode of this type actually cost, per
	// record. The output buffer is sized from it, which is the difference
	// between one allocation and a dozen regrows: the field count alone is a
	// hopeless estimate for anything but a flat struct of small scalars, and
	// each regrow copies the whole payload so far.
	//
	// It is a capacity hint, so a stale or wildly wrong value is only ever a
	// wasted guess — no correctness stake, and no lock needed.
	bytesPerRecord atomic.Uint32
}

var typeInfoCache sync.Map // reflect.Type -> *typeInfo

// topLevelIsRecords reports whether a top-level Marshal/Unmarshal type uses the
// columnar records layout (a struct, or a slice of structs). Everything else —
// maps, []*struct, scalars, [][]T — uses value mode. Both encoder and decoder
// derive the mode from the type, so no wire marker is needed.
func topLevelIsRecords(t reflect.Type) bool {
	if t.Kind() == reflect.Struct {
		return true
	}
	return t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Struct
}

// buildState carries the types whose layout is being built right now, so a
// self-referential type resolves to the typeInfo already under construction
// instead of recursing forever. It is created per top-level build and never
// shared, so nothing here is visible to other goroutines; completed layouts are
// published to typeInfoCache in one go once the whole build succeeds, which is
// why a half-built typeInfo can never be observed (or cached after an error).
type buildState struct {
	inFlight map[reflect.Type]*typeInfo
	done     []*typeInfo
}

// publish moves every layout completed during this build into the shared cache.
func (st *buildState) publish() {
	for _, ti := range st.done {
		typeInfoCache.Store(ti.rtype, ti)
	}
}

// getTypeInfo returns cached field layout for a struct type, building it once.
func getTypeInfo(t reflect.Type) (*typeInfo, error) {
	if cached, ok := typeInfoCache.Load(t); ok {
		return cached.(*typeInfo), nil
	}
	var st buildState
	ti, err := st.typeInfo(t)
	if err != nil {
		return nil, err
	}
	st.publish()
	return ti, nil
}

// typeInfo resolves t within an in-progress build: cache, then the in-flight set
// (a back-edge into a type still being built — that is the recursion base case),
// then a fresh build.
func (st *buildState) typeInfo(t reflect.Type) (*typeInfo, error) {
	if cached, ok := typeInfoCache.Load(t); ok {
		return cached.(*typeInfo), nil
	}
	if ti, ok := st.inFlight[t]; ok {
		return ti, nil // cycle: hand back the layout being built one frame up
	}
	return st.build(t)
}

// build introspects a struct type and assigns each field a wire id.
// Explicit ids (`cb:"5"`) are reserved first; the remaining fields get an FNV hash
// of their name, linear-probed around the already-used slots.
func (st *buildState) build(t reflect.Type) (*typeInfo, error) {
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("colbin: expected struct, got %s", t.Kind())
	}
	xs := xunsafe.NewStruct(t)
	ti := &typeInfo{rtype: t, size: t.Size()}

	// Register before describing the fields: a field that refers back to t (via a
	// slice, pointer or map) resolves to this same ti, which is fully populated by
	// the time the build returns.
	if st.inFlight == nil {
		st.inFlight = make(map[reflect.Type]*typeInfo, 4)
	}
	st.inFlight[t] = ti
	defer delete(st.inFlight, t)

	explicitIDs := make([]int, 0) // parallel to ti.fields: >=0 explicit, -1 hashed
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if sf.PkgPath != "" { // unexported
			continue
		}
		name, explicitID, skip := parseCbTag(sf)
		if skip {
			continue
		}
		if len(ti.fields) >= 254 {
			return nil, fmt.Errorf("colbin: %s exceeds 254 encodable fields", t.Name())
		}
		if explicitID > 254 { // 255 is reserved
			return nil, fmt.Errorf("colbin: field %s.%s id %d out of range 0..254", t.Name(), sf.Name, explicitID)
		}
		fm, err := st.describe(sf.Type)
		if err != nil {
			return nil, fmt.Errorf("colbin: field %s.%s: %w", t.Name(), sf.Name, err)
		}
		fm.name = name
		fm.jsonName = jsonFieldName(sf, name)
		fm.offset = sf.Offset
		fm.xf = &xs.Fields[i]
		ti.fields = append(ti.fields, fm)
		explicitIDs = append(explicitIDs, explicitID)
	}

	used := map[uint8]bool{reservedFieldID: true} // 255 is reserved
	for k, id := range explicitIDs {              // pass 1: reserve explicit ids
		if id < 0 {
			continue
		}
		if used[uint8(id)] {
			return nil, fmt.Errorf("colbin: %s has duplicate field id %d", t.Name(), id)
		}
		used[uint8(id)] = true
		ti.fields[k].id = uint8(id)
	}
	for k, id := range explicitIDs { // pass 2: hash + probe the rest
		if id >= 0 {
			continue
		}
		hid := probeFieldID(fnv8(ti.fields[k].name), used)
		used[hid] = true
		ti.fields[k].id = hid
	}

	ti.byID = make(map[uint8]*fieldMeta, len(ti.fields))
	for i := range ti.fields {
		ti.byID[ti.fields[i].id] = &ti.fields[i]
	}
	st.done = append(st.done, ti)
	return ti, nil
}

// parseCbTag reads the `cb` struct tag. Comma-separated tokens: an integer token
// sets the explicit field id (no hashing); the first non-integer token overrides
// the hashed name. `cb:"-"` skips the field; empty/absent falls back to the Go
// field name with a hashed id. Examples: `cb:"5"` (id 5), `cb:"id,5"` (name "id",
// id 5), `cb:"id"` (hashed id from "id").
func parseCbTag(sf reflect.StructField) (name string, explicitID int, skip bool) {
	name, explicitID = sf.Name, -1
	tag := sf.Tag.Get("cb")
	if tag == "-" {
		return "", -1, true
	}
	if tag == "" {
		return sf.Name, -1, false
	}
	nameSet := false
	for _, tok := range strings.Split(tag, ",") {
		if tok == "" {
			continue
		}
		if id, err := strconv.Atoi(tok); err == nil {
			explicitID = id // integer token -> explicit id
		} else if !nameSet {
			name, nameSet = tok, true // first non-integer token -> name
		}
	}
	return name, explicitID, false
}

// jsonFieldName is the key a field gets in JSON mode. A `json` tag name wins,
// then the cb name, then the Go field name — so one struct can serve both
// encoding/json and colbin's JSON mode. It never feeds the field-id hash, so
// adding or changing a json tag leaves the binary payload untouched.
//
// Only the tag's name matters here: `omitempty` cannot be honoured (a column is
// dense, every record carries a value) and `json:"-"` does not drop the field
// (it is in the payload regardless, so it needs a key) — use `cb:"-"` to leave a
// field out entirely.
func jsonFieldName(sf reflect.StructField, cbName string) string {
	tag := sf.Tag.Get("json")
	if tag == "" || tag == "-" {
		return cbName
	}
	if name, _, _ := strings.Cut(tag, ","); name != "" {
		return name
	}
	return cbName
}

// describeType maps a Go type to its wire type class, recursively resolving
// array element and nested struct descriptors. Used for both struct fields and
// array elements (the caller fills id/name/offset/xf for actual struct fields).
func describeType(t reflect.Type) (fieldMeta, error) {
	var st buildState
	fm, err := st.describe(t)
	if err != nil {
		return fieldMeta{}, err
	}
	st.publish()
	return fm, nil
}

// describe is describeType within an in-progress build (see buildState).
func (st *buildState) describe(t reflect.Type) (fieldMeta, error) {
	fm, err := st.describeKind(t)
	if err != nil {
		return fieldMeta{}, err
	}
	fm.cyclic = reachesCycle(t)
	return fm, nil
}

func (st *buildState) describeKind(t reflect.Type) (fieldMeta, error) {
	switch k := t.Kind(); k {
	case reflect.Int8, reflect.Uint8, reflect.Bool:
		return fieldMeta{fType: ftInt, goKind: k, bitWidth: 8}, nil
	case reflect.Int16, reflect.Uint16:
		return fieldMeta{fType: ftInt, goKind: k, bitWidth: 16}, nil
	case reflect.Int32, reflect.Uint32:
		return fieldMeta{fType: ftInt, goKind: k, bitWidth: 32}, nil
	case reflect.Int64, reflect.Uint64, reflect.Int, reflect.Uint:
		return fieldMeta{fType: ftInt, goKind: k, bitWidth: 64}, nil
	case reflect.Float32:
		return fieldMeta{fType: ftFloat, goKind: k, bitWidth: 32}, nil
	case reflect.Float64:
		return fieldMeta{fType: ftFloat, goKind: k, bitWidth: 64}, nil
	case reflect.String:
		return fieldMeta{fType: ftString, goKind: k}, nil
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return fieldMeta{fType: ftBytes, goKind: k}, nil // []byte
		}
		et := t.Elem()
		em, err := st.describe(et)
		if err != nil {
			return fieldMeta{}, err
		}
		return fieldMeta{fType: ftArray, goKind: k, sliceType: t, elemSize: et.Size(), elem: &em}, nil
	case reflect.Struct:
		sub, err := st.typeInfo(t) // recurse (nested struct becomes a sub-table)
		if err != nil {
			return fieldMeta{}, err
		}
		return fieldMeta{fType: ftStruct, goKind: k, sub: sub}, nil
	case reflect.Ptr:
		if t.Elem().Kind() == reflect.Ptr {
			return fieldMeta{}, fmt.Errorf("pointer-to-pointer %s not supported", t)
		}
		pd, err := st.describe(t.Elem()) // pointee accessed like an array element
		if err != nil {
			return fieldMeta{}, err
		}
		return fieldMeta{fType: pd.fType, goKind: pd.goKind, bitWidth: pd.bitWidth,
			sub: pd.sub, nullable: true, pointeeType: t.Elem(), elem: &pd}, nil
	case reflect.Map:
		kd, err := st.describe(t.Key())
		if err != nil {
			return fieldMeta{}, err
		}
		if kd.fType != ftInt && kd.fType != ftFloat && kd.fType != ftString {
			return fieldMeta{}, fmt.Errorf("map key %s must be scalar or string", t.Key())
		}
		vd, err := st.describe(t.Elem())
		if err != nil {
			return fieldMeta{}, err
		}
		return fieldMeta{fType: ftMap, mapType: t, mapKeyType: t.Key(), mapValType: t.Elem(),
			mapKey: &kd, mapVal: &vd}, nil
	case reflect.Interface:
		// Any interface value is encoded as a self-describing tagged value. The
		// concrete type is resolved per value at encode time (see any.go), so all
		// three sites — `any` field, `[]any` element, `map[K]any` value — collapse
		// to this one descriptor.
		return fieldMeta{fType: ftAny, goKind: k, ifaceType: t}, nil
	default:
		return fieldMeta{}, fmt.Errorf("unsupported type %s", t)
	}
}

var cyclicCache sync.Map // reflect.Type -> bool

// reachesCycle reports whether the encodable type graph rooted at t can reach a
// struct that (transitively) contains itself. It walks exactly the edges
// describeKind walks — unexported and `cb:"-"` fields are skipped, []byte is a
// leaf, interfaces are opaque — so a type that encodes without recursing is
// never reported as cyclic, and its wire layout stays unchanged.
func reachesCycle(t reflect.Type) bool {
	return walkCycle(t, map[reflect.Type]bool{})
}

// walkCycle is reachesCycle's DFS; path holds the structs on the current chain.
// Both outcomes are independent of the path taken to reach t — hitting an
// ancestor X means X -> ... -> t -> ... -> X, so t itself sits on a real cycle —
// which is what makes memoizing a completed node safe.
func walkCycle(t reflect.Type, path map[reflect.Type]bool) bool {
	switch t.Kind() {
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return false // []byte is a leaf (ftBytes)
		}
		return walkCycle(t.Elem(), path)
	case reflect.Ptr:
		return walkCycle(t.Elem(), path)
	case reflect.Map:
		return walkCycle(t.Key(), path) || walkCycle(t.Elem(), path)
	case reflect.Struct:
		if path[t] {
			return true // back-edge: t contains itself
		}
		if v, ok := cyclicCache.Load(t); ok {
			return v.(bool)
		}
		path[t] = true
		found := false
		for i := 0; i < t.NumField() && !found; i++ {
			sf := t.Field(i)
			if sf.PkgPath != "" { // unexported: never encoded
				continue
			}
			if _, _, skip := parseCbTag(sf); skip {
				continue
			}
			found = walkCycle(sf.Type, path)
		}
		delete(path, t)
		cyclicCache.Store(t, found)
		return found
	default:
		return false // scalars, strings, interfaces (ftAny is value-driven)
	}
}

// elideEmpty reports that an element column carrying n values can be left off the
// wire entirely. Columns are schema-driven, so an empty one still writes the
// nested sub-tables of its element type — which never terminates when that type
// refers back to itself. Omitting the column at n == 0 cuts the recursion, and
// both sides agree on it: n comes from the length/presence sub-column that
// precedes the elided column, and cyclic is a property of the Go type. Only
// self-referential types are elided, so every other column is byte-for-byte what
// it always was.
func elideEmpty(elem *fieldMeta, n int) bool {
	return n == 0 && elem.cyclic
}

// fnv8 computes FNV-1a 32-bit over s, xor-folded down to 8 bits.
func fnv8(s string) uint8 {
	const (
		offset32 = 2166136261
		prime32  = 16777619
	)
	h := uint32(offset32)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= prime32
	}
	return uint8(h ^ (h >> 8) ^ (h >> 16) ^ (h >> 24))
}

// probeFieldID returns start, or the next free id (wrapping, skipping used slots).
func probeFieldID(start uint8, used map[uint8]bool) uint8 {
	id := start
	for used[id] {
		id++ // wraps at 256; reservedFieldID(255) is pre-marked so it's skipped
	}
	return id
}
