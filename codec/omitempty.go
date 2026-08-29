package codec

import (
	"sync/atomic"
	"unsafe"
)

// Omit-empty mode.
//
// A colbin column is positional: N records, N values, and a value that happens
// to be zero still occupies its slot. The varint array codec floors at one byte
// per element, so a thousand records of an untouched int32 field cost a thousand
// bytes to say "nothing here" -- and a struct carrying a dozen mostly-unset
// fields pays that a dozen times over.
//
// With omit-empty on, a column holding nothing but empty values is written as
// its type byte alone, with the empty bit set and no payload at all. Float
// columns have always done this; the flag extends it to the integer and string
// columns, and through them to every column that is framed by a length
// sub-column -- bytes, arrays and maps all reduce to an integer column of zeros.
//
// Compact mode already omits a zero-valued field, so for a flat struct of
// scalars the flag changes nothing there. What it adds is pointers: a nullable
// field can join a compact record once nil and a pointer to the zero value are
// allowed to mean the same thing, which is precisely what omit-empty says.
//
// That is the semantic price, and it is why this is a flag rather than the
// default:
//
//	a *T pointing at T's zero value decodes back as nil.
//
// Nothing else changes. Slices already lost nil-versus-empty in both directions
// before this flag existed, and a zero scalar is a zero scalar.
//
// A message written with the flag on carries its own version byte, so a decoder
// that predates the flag rejects it outright rather than reading the type byte,
// missing the empty bit and walking off into the next column.
var omitEmpty atomic.Bool

// SetOmitEmpty turns omit-empty encoding on or off. It affects encoding only:
// both forms decode either way, so a reader never has to be configured to match
// its writer.
//
// It is global and meant to be set once, at startup, before the first Marshal.
// Setting it is safe at any time -- the flag is atomic and the per-type caches it
// invalidates are rebuilt on demand -- but a value encoded before the change and
// one encoded after will differ, which is rarely what a caller wants mid-flight.
func SetOmitEmpty(on bool) {
	if omitEmpty.Swap(on) != on {
		// Compact-mode eligibility depends on the flag, since it decides whether
		// a nullable field can be carried at all. The plans that answered under
		// the old setting have to be rebuilt.
		invalidateCompactPlans()
	}
}

// OmitEmpty reports whether omit-empty encoding is on.
func OmitEmpty() bool { return omitEmpty.Load() }

// binaryPrefix and jsonVersionByte are the version a message gets, which records
// which of the two encodings produced it. Both prefixes are package-level so the
// columnar path does not allocate one per message.
func binaryPrefix() []byte {
	if omitEmpty.Load() {
		return omitEmptyPrefix
	}
	return densePrefix
}

func jsonVersionByte() byte {
	if omitEmpty.Load() {
		return jsonFormatVersionOmitEmpty
	}
	return jsonFormatVersion
}

var (
	densePrefix     = []byte{formatVersion}
	omitEmptyPrefix = []byte{formatVersionOmitEmpty}
)

// emptyColumnBit is set in a column's type byte to say the column holds nothing
// but empty values and carries no payload. It is bit 7, the bit float columns
// have always used for exactly this.
const emptyColumnBit byte = 1 << 7

// allZero reports whether a column of integers -- which is where bools live too
// -- holds nothing but zeros.
func allZero(vals []int64) bool {
	for _, v := range vals {
		if v != 0 {
			return false
		}
	}
	return true
}

// allEmptyStrings reports whether a string column holds nothing but "".
func allEmptyStrings(ptrs []unsafe.Pointer, offset uintptr) bool {
	for _, p := range ptrs {
		if len(*(*string)(unsafe.Add(p, offset))) != 0 {
			return false
		}
	}
	return true
}
