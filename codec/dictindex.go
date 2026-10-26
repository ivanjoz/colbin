package codec

// A dictionary's index while it is written: which id a string already has.
//
// It is its own pair of tables rather than a map[string]uint32. A column of
// TPC-H's lineitem looks up 600 000 strings of a few bytes each, and a map
// spent most of a dictionary encode hashing them — AES over every byte and a
// group probe — and then comparing through memequal.
//
// Most strings a dictionary is worth having for are short — a status, a
// country, a currency — and a string of up to sixteen bytes is its own key:
// two words, zero past its end, and its length. So `short` holds the key in
// the slot, and a probe is two integer compares that never read the string the
// slot names. dictKeys makes the keys for a chunk of a column at a time, in
// assembly where the architecture has it (one sixteen-byte load and a mask).
// Longer strings go to `long`, which hashes a few words of them and compares
// the strings themselves.
//
// A cheap hash has cheap collisions to find, and a table that met them would
// probe forever. So a probe is bounded: past maxDictProbe the column is
// refused and goes out as strings, which is always correct and only costs the
// bytes.

import (
	"math/bits"
	"unsafe"
)

const (
	// shortKeyBytes is the longest string that is its own key.
	shortKeyBytes = 16
	minDictSlots  = 16
	maxDictProbe  = 32
)

// dictEntries is one dictionary while it is written: its values, by id, and
// the index over them.
type dictEntries struct {
	values []string
	// short and long are open-addressed, each a power of two long and never
	// more than half full. A short slot holds its id+1, 0 when empty; so does
	// a long one. shortShift is 64 − log₂ len(short): the slot is the hash's
	// top bits.
	short      []dictShort
	shortShift uint8
	long       []uint32
	// shorts and longs are how many values each table indexes.
	shorts, longs int
	// broken is set when a rebuild ran past the probe bound: every lookup then
	// refuses, rather than index a table that does not hold every value.
	broken bool
	// number is the dictionary's index in the section, or -1 until a column
	// uses it.
	number int
}

// dictShort is a short table's slot. It is 32 bytes, padding included, so
// that the kernel keeps a slot's offset rather than its index and steps it
// with an add and a mask: see dictkernel_amd64.s.
type dictShort struct {
	lo, hi uint64
	n, id  uint32
	_      uint64
}

func newDictEntries() *dictEntries {
	entries := &dictEntries{number: -1}
	entries.rebuild()
	return entries
}

// shortKey is a string of up to sixteen bytes as two little-endian words,
// zero past its end: what a sixteen-byte load and a length mask give, made
// from loads that stay inside the string.
func shortKey(value string) (lo, hi uint64) {
	n := len(value)
	p := unsafe.Pointer(unsafe.StringData(value))
	switch {
	case n > shortKeyBytes:
		// Not a key: long values are indexed by dictHash.
	case n > 8:
		lo = load64(p)
		hi = load64(unsafe.Add(p, n-8)) >> (8 * (16 - n))
	case n == 8:
		lo = load64(p)
	case n >= 4:
		lo = uint64(load32(p)) | uint64(load32(unsafe.Add(p, n-4)))<<(8*(n-4))
	case n > 0:
		lo = uint64(*(*byte)(p)) | uint64(*(*byte)(unsafe.Add(p, n/2)))<<(8*(n/2)) |
			uint64(*(*byte)(unsafe.Add(p, n-1)))<<(8*(n-1))
	}
	return lo, hi
}

func shortHash(lo, hi uint64, n int) uint64 {
	return (lo ^ bits.RotateLeft64(hi, 31) ^ uint64(n)<<59) * 0x9e3779b97f4a7c15
}

// dictHash mixes a long string's length and three eight-byte words of it.
func dictHash(value string) uint64 {
	n := len(value)
	p := unsafe.Pointer(unsafe.StringData(value))
	h := load64(p) ^ bits.RotateLeft64(load64(unsafe.Add(p, n-8)), 29) ^
		bits.RotateLeft64(load64(unsafe.Add(p, n/2-4)), 47)
	// murmur3's finalizer: the slot is the hash's low bits, and a multiply
	// alone leaves those depending on the input's low bits — the first bytes
	// only — so the shifts fold the high half back down.
	h ^= uint64(n) << 56
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	h *= 0xc4ceb9fe1a85ec53
	return h ^ h>>33
}

func load64(p unsafe.Pointer) uint64 { return *(*uint64)(p) }
func load32(p unsafe.Pointer) uint32 { return *(*uint32)(p) }

// find is value's id, if it has one. lo and hi are its shortKey, and are
// ignored for a long value. A probe past maxDictProbe reports !ok: the caller
// gives the column up.
func (e *dictEntries) find(value string, lo, hi uint64) (id uint32, found, ok bool) {
	if e.broken {
		return 0, false, false
	}
	n := len(value)
	if n <= shortKeyBytes {
		mask := len(e.short) - 1
		slot := int(shortHash(lo, hi, n)>>e.shortShift) & mask
		for range maxDictProbe {
			s := &e.short[slot]
			if s.id == 0 {
				return 0, false, true
			}
			if s.lo == lo && s.hi == hi && s.n == uint32(n) {
				return s.id - 1, true, true
			}
			slot = (slot + 1) & mask
		}
		return 0, false, false
	}
	mask := len(e.long) - 1
	slot := int(dictHash(value)) & mask
	for range maxDictProbe {
		stored := e.long[slot]
		if stored == 0 {
			return 0, false, true
		}
		if e.values[stored-1] == value {
			return stored - 1, true, true
		}
		slot = (slot + 1) & mask
	}
	return 0, false, false
}

// insert adds value, which find did not find, as the next id. It reports false
// when the index can no longer hold it, and the caller gives the column up.
func (e *dictEntries) insert(value string, lo, hi uint64) bool {
	id := uint32(len(e.values))
	e.values = append(e.values, value)
	n := len(value)
	if n <= shortKeyBytes {
		if e.shorts++; 2*e.shorts > len(e.short) {
			return e.rebuild()
		}
		return e.placeShort(lo, hi, n, id)
	}
	if e.longs++; 2*e.longs > len(e.long) {
		return e.rebuild()
	}
	return e.placeLong(value, id)
}

func (e *dictEntries) placeShort(lo, hi uint64, n int, id uint32) bool {
	mask := len(e.short) - 1
	slot := int(shortHash(lo, hi, n)>>e.shortShift) & mask
	for range maxDictProbe {
		if e.short[slot].id == 0 {
			e.short[slot] = dictShort{lo: lo, hi: hi, n: uint32(n), id: id + 1}
			return true
		}
		slot = (slot + 1) & mask
	}
	return false
}

func (e *dictEntries) placeLong(value string, id uint32) bool {
	mask := len(e.long) - 1
	slot := int(dictHash(value)) & mask
	for range maxDictProbe {
		if e.long[slot] == 0 {
			e.long[slot] = id + 1
			return true
		}
		slot = (slot + 1) & mask
	}
	return false
}

// rebuild sizes both tables for the values they hold and indexes every value
// afresh, reporting false — and breaking the index — when a probe runs past
// the bound.
func (e *dictEntries) rebuild() bool {
	e.shorts, e.longs = 0, 0
	for _, value := range e.values {
		if len(value) <= shortKeyBytes {
			e.shorts++
		} else {
			e.longs++
		}
	}
	shortSize, longSize := minDictSlots, minDictSlots
	for 2*e.shorts > shortSize {
		shortSize *= 2
	}
	for 2*e.longs > longSize {
		longSize *= 2
	}
	if cap(e.short) >= shortSize {
		e.short = e.short[:shortSize]
		clear(e.short)
	} else {
		e.short = make([]dictShort, shortSize)
	}
	if cap(e.long) >= longSize {
		e.long = e.long[:longSize]
		clear(e.long)
	} else {
		e.long = make([]uint32, longSize)
	}
	e.shortShift = uint8(64 - bits.TrailingZeros(uint(shortSize)))
	e.broken = false
	for id, value := range e.values {
		var ok bool
		if n := len(value); n <= shortKeyBytes {
			lo, hi := shortKey(value)
			ok = e.placeShort(lo, hi, n, uint32(id))
		} else {
			ok = e.placeLong(value, uint32(id))
		}
		if !ok {
			e.broken = true
			return false
		}
	}
	return true
}

// rollback forgets the entries a column added before it was refused.
//
// Linear probing has no cheap delete, so the index is rebuilt from what is
// left. A refusal comes at most dictSampleRows into a column of new values or
// half-way into one, so this is no more work than the adding was.
func (e *dictEntries) rollback(before int) {
	if len(e.values) == before {
		return
	}
	clear(e.values[before:])
	e.values = e.values[:before]
	e.rebuild()
}
