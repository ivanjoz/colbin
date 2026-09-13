// The descriptor vocabulary both key widths share.
//
// Ported from wire/wide.go's header and wire/narrow.go's constant block. A
// four-bit key shares its byte with the field's own detail nibble and takes the
// class from the schema; an eight-bit key spends a byte on the key and names the
// class in the descriptor. Everything below the class is the same in both, which
// is why it lives here rather than twice.
//
// Nothing here is a varint. Every size escalates to a width the header names, so
// no read is ever a loop whose trip count is data — which is also why there is no
// continuation run for an unauthenticated peer to make unbounded.

// ---- failure ----------------------------------------------------------------
//
// The reader defends against the network; the writer trusts its own program.
// AssemblyScript has neither of the two things Go leans on here — an
// unconditional bounds check, and a recover to turn the resulting panic into an
// error — so every count and offset is tested before it is used and a failure is
// a code rather than a trap (PLAN.md §4.3).

export const W_OK: i32 = 0
export const W_TRUNCATED: i32 = 1
export const W_SIZE_TOO_LARGE: i32 = 2
export const W_BAD_ESCAPE: i32 = 3
export const W_BAD_DESCRIPTOR: i32 = 4
export const W_NO_SKIP: i32 = 5
export const W_BAD_COLUMN: i32 = 6
export const W_BAD_PACKED: i32 = 7
export const W_UNSUPPORTED_ENC: i32 = 8

export function wireErrorText(code: i32): string {
  if (code == W_TRUNCATED) return 'the message ends inside a field'
  if (code == W_SIZE_TOO_LARGE) return 'a declared size is larger than this platform can address'
  if (code == W_BAD_ESCAPE) return 'unassigned size escape code'
  if (code == W_BAD_DESCRIPTOR) return 'unassigned or mismatched descriptor'
  if (code == W_NO_SKIP) return 'a four-bit-keyed field cannot be skipped without knowing its type'
  if (code == W_BAD_COLUMN) return 'a column is corrupt'
  if (code == W_BAD_PACKED) return 'a packed string is corrupt'
  if (code == W_UNSUPPORTED_ENC) return 'a string encoding this version does not assign'
  return ''
}

// ---- the eight-bit descriptor -----------------------------------------------

/** The top bit: set means the descriptor names a class, clear means it is the value. */
export const DESC_EXPLICIT: u8 = 0x80

export const CLASS_INT: u8 = 0
export const CLASS_BLOB: u8 = 1
export const CLASS_VEC: u8 = 2
export const CLASS_COL: u8 = 3
export const CLASS_LIST: u8 = 4
export const CLASS_STRUCT: u8 = 5
export const CLASS_MAP: u8 = 6
export const CLASS_SPECIAL: u8 = 7

/** SPECIAL details. Detail bit 3 is the varint, which takes the upper half. */
export const SPECIAL_NULL: u8 = 0
export const SPECIAL_TRUE: u8 = 1
export const SPECIAL_FALSE: u8 = 2
export const SPECIAL_VARINT: u8 = 0b1000

/** How much of a varint's value its descriptor's detail nibble carries. */
export const VARINT_BITS: i32 = 3

@inline
export function classOf(desc: u8): u8 {
  return (desc >> 4) & 0b111
}

// ---- detail bits ------------------------------------------------------------

/** The STRUCT descriptor's k8 bit, clear when the run inside uses four-bit keys. */
export const STRUCT_WIDE_KEYS: u8 = 0b1000

/** The MAP class's sub bit: clear is a map, set is a table. */
export const TABLE_FLAG: u8 = 0b1000

/** The LIST detail bit saying the elements carry no descriptor of their own. */
export const LIST_HOMOGENEOUS: u8 = 0b1000

/**
 * A size or count that did not fit its three inline bits, and a wider one
 * follows. Bit 3 in a blob header and bit 0 in an array header, because an array
 * spends bits 2..1 on its element width.
 */
export const MORE_SIZE_FLAG: u8 = 0b1000
export const MORE_ARRAY_LEN_FLAG: u8 = 0b0001
export const ARRAY_WIDTH_SHIFT: u8 = 1

/**
 * Bit 3 of a *signed* integer header, directly above the three size-code bits,
 * and bit 3 of an array header. Two constants rather than one because a single
 * value used for both would land on a size-code bit.
 */
export const INT_POSITIVE_FLAG: u8 = 0b1000
export const ARRAY_POSITIVE_FLAG: u8 = 0b1000

/**
 * An unsigned narrow field spends no sign bit, so all sixteen nibble codes carry
 * information: 0..7 is the value itself with no payload at all, and 8..15 is a
 * magnitude of (nibble - 7) bytes. A K4 reader has the schema and already knows
 * whether the field is signed, so the two forms can share the nibble and mean
 * different things in it.
 */
export const UINT_INLINE_MAX: u8 = 7
export const UINT_WIDTH_BASE: u8 = 8

/** Escape codes, which occupy a blob header's three size bits once more is set. */
export const ESCAPE_2_BYTES: u8 = 0
export const ESCAPE_4_BYTES: u8 = 1
export const ESCAPE_8_BYTES: u8 = 2

/**
 * The packed5 escapes.
 *
 * A narrow blob header has no `enc` field — under four-bit keys the schema says
 * what a field is, not the wire — so a packed string names itself here instead,
 * and carries the one bit the schema cannot know: the case mode its unit stream
 * opens in. Spending four codes on it buys a two-byte header, the same as a raw
 * blob's, where a separate flag byte would have cost three.
 */
export const ESCAPE_PACKED_1_LO: u8 = 3
export const ESCAPE_PACKED_1_UP: u8 = 4
export const ESCAPE_PACKED_4_LO: u8 = 5
export const ESCAPE_PACKED_4_UP: u8 = 6

/**
 * The BLOB descriptor's `enc` codes, in bits 3-2 of an eight-bit-keyed
 * descriptor. Code 2 is reserved for a column dictionary and is never written.
 */
export const ENC_RAW: u8 = 0
export const ENC_PACKED5: u8 = 1
export const ENC_DICTIONARY: u8 = 2
export const ENC_PACKED5_UP: u8 = 3

/** The largest element length a list writes in one byte; 0xFF escapes to four. */
export const INLINE_ELEMENT_SIZE: i32 = 0xfe
export const ELEMENT_SIZE_ESCAPE: u8 = 0xff

/** What a composite's reserved length byte holds before the body outgrows it. */
export const INLINE_COMPOSITE_LENGTH: i32 = 0xff

/**
 * The largest size this port will accept off a wire.
 *
 * Go bounds a declared size by `maxInt`, which is 2^63-1 there. Here the
 * addressable unit is a 32-bit offset into linear memory, so a size above what
 * an i32 holds is refused before anything is allocated rather than wrapped into
 * a small one whose bounds check would then pass.
 */
export const MAX_SIZE: u64 = 0x7fffffff

/** A magnitude size code to its byte count. Code 7 is eight, so a seven-byte
 * magnitude rounds up and every other width is exact. */
// @ts-ignore: decorator
@lazy
export const MAGNITUDE_WIDTH: StaticArray<i32> = [0, 1, 2, 3, 4, 5, 6, 8]

/** A two-bit lw code to its byte count. */
// @ts-ignore: decorator
@lazy
export const LENGTH_WIDTH: StaticArray<i32> = [1, 2, 4, 8]

// ---- shared reads -----------------------------------------------------------

/**
 * A magnitude's `width` bytes at `at`, little-endian.
 *
 * The caller has already checked the bound; this is the inner loop and testing
 * it twice is what the surrounding checks exist to avoid.
 */
@inline
export function leUintAt(buf: Uint8Array, at: i32, width: i32): u64 {
  let value: u64 = 0
  for (let index = width - 1; index >= 0; index--) {
    value = (value << 8) | <u64>unchecked(buf[at + index])
  }
  return value
}

/** The narrowest lw code that holds n, and the width it names. */
export function lengthCodeFor(n: u64): i32 {
  if (n <= 0xff) return 0
  if (n <= 0xffff) return 1
  if (n <= 0xffffffff) return 2
  return 3
}

/** The size code that describes a magnitude: its byte count, except at seven,
 * which rounds to eight so every other width stays exact. */
export function sizeCodeFor(magnitude: u64): i32 {
  const width = magnitudeBytes(magnitude)
  return width >= 7 ? 7 : width
}

/** How many bytes a magnitude actually occupies. */
@inline
export function magnitudeBytes(magnitude: u64): i32 {
  return (64 - <i32>clz(magnitude) + 7) >> 3
}

/** The byte count a size code names. */
@inline
export function widthOfCode(code: i32): i32 {
  return unchecked(MAGNITUDE_WIDTH[code])
}

/**
 * The narrowest element width that holds every element of an array, as a code:
 * 0 → 1 byte, 1 → 2, 2 → 4, 3 → 8.
 *
 * A two's complement array needs one more bit than its magnitude, which is what
 * the shift tests — and a magnitude already filling the top bit cannot gain one,
 * so it is answered before the shift wraps it to zero and picks a one-byte width
 * for the most negative value there is.
 */
export function arrayWidthCode(widest: u64, allPositive: bool): i32 {
  if (!allPositive) {
    if (widest > (<u64>1 << 62)) return 3
    widest <<= 1
  }
  if (widest <= 0xff) return 0
  if (widest <= 0xffff) return 1
  if (widest <= 0xffffffff) return 2
  return 3
}

/** What the varint form of a value would occupy, key and descriptor included. */
export function varintLen(value: u64): i32 {
  let rest = value >> <u64>VARINT_BITS
  let length = 2
  while (true) {
    length++
    if (rest < 0x80) return length
    rest >>= 7
  }
}

/** Folds a signed value so small negatives stay small, which is what makes the
 * varint worth using for a delta. */
@inline
export function zigzag(value: i64): u64 {
  return (<u64>(value << 1)) ^ (<u64>(value >> 63))
}

/** float32 from a reversed IEEE-754 bit pattern. */
@inline
export function floatFromReversed32(bits: u32): f32 {
  return reinterpret<f32>(bswap<u32>(bits))
}

/** float64 from a reversed IEEE-754 bit pattern. */
@inline
export function floatFromReversed64(bits: u64): f64 {
  return reinterpret<f64>(bswap<u64>(bits))
}
