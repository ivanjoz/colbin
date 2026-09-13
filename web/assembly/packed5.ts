// Port of github.com/ivanjoz/colbin/packed5: the Packed-5 string codec.
//
// This replaces the first port wholesale rather than amending it. The codec it
// ported had a bit accumulator, a three-bit pad prefix inside the payload, and a
// planning pass that priced two behavioural header flags before writing a bit.
// None of those survive: the alphabet is now 32 five-bit codes and **every token
// is a whole number of them**, which is the property everything else follows
// from.
//
// # Units, and why eight at a time
//
// Eight units are forty bits are five bytes exactly. So a group needs no padding
// and no accumulator that survives it: the reader takes one unaligned 64-bit
// load, shifts out eight units with constant shifts, and advances five bytes.
// That is the shape AssemblyScript is *better* at than the accumulator was —
// native u64 with Go's shift semantics is what the language was chosen for.
//
// # Wire format
//
// A standalone frame is a header byte and a payload:
//
//	bit  0     PACKED_5     0 = raw payload, 1 = packed unit stream
//	bit  1     UPPERCASE    the stream starts in uppercase mode
//	bit  2     reserved     must be zero
//	bits 3-7   length code  payload byte length, or 31 = an LEB128 uvarint follows
//
// Embedded in a colbin BLOB it has **no header at all**: the descriptor already
// says the encoding and the size, so all three of those are duplicates. The
// module only ever meets the embedded form, so only the payload reader is here.
//
// There is no pad prefix and no pad count. The unit count follows from the
// payload length alone — `units = size * 8 / 5` — because the encoder pads the
// stream to the grid with a trailing CASE_TOGGLE_SIMPLE, which applies to the
// next letter and so decodes to nothing when there is no letter after it.
//
// # Opcodes
//
//	0..25   letter a..z, cased by the current case mode
//	26      space
//	27      CASE_TOGGLE_SIMPLE   invert the case of the next letter only
//	28      CASE_TOGGLE_LONG     invert the case mode until the next 28
//	29      + 1 unit: index into SYM_TABLE  (digits and common punctuation)
//	30      + 1 unit: index into EXT_TABLE  (accents and rare punctuation),
//	                  or 31 to start a raw-byte escape
//	31      + 2 units: integer 0..1023, low five bits first

import { Writer, gather8 } from './bytes'

/** The alphabet. Every code is one unit; the operands below are units too. */
const OP_SPACE: u8 = 26
const OP_CASE_SIMPLE: u8 = 27
const OP_CASE_LONG: u8 = 28
const OP_SYMBOL: u8 = 29
const OP_EXT: u8 = 30
const OP_NUMBER: u8 = 31

/** The EXT_TABLE operand that introduces a run of raw bytes. */
const EXT_ESCAPE: u8 = 31

/** The first unassigned EXT_TABLE index. Rejected rather than guessed at, so
 * claiming one later is a clean format change and not a reinterpretation. */
const EXT_RESERVED: u8 = 28

/** How many raw bytes one escape can carry. The count occupies a unit but only
 * 1..4 are legal, so the three-unit header stays worth amortising. */
const MAX_ESCAPE_RUN: i32 = 4

/** The most units one token spans: a four-byte escape, at 3 + 2n. */
const MAX_TOKEN_UNITS: i32 = 3 + 2 * MAX_ESCAPE_RUN

/** How many units the reader keeps in hand. A token spans at most eleven, so
 * the loop can read a whole one without a bounds test while this many are
 * buffered, and the window refills a group at a time above it. */
const WINDOW_UNITS: i32 = 32

/** Failure codes. The decoder never throws: a packed string arrives from a wire
 * like everything else here. */
export const P5_OK: i32 = 0
export const P5_TRUNCATED: i32 = 1
export const P5_RESERVED_SYMBOL: i32 = 2
export const P5_BAD_ESCAPE: i32 = 3

export function packed5ErrorText(code: i32): string {
  if (code == P5_TRUNCATED) return 'a packed string ends inside a token'
  if (code == P5_RESERVED_SYMBOL) return 'a packed string names a reserved symbol index'
  if (code == P5_BAD_ESCAPE) return 'a packed string holds a malformed raw escape'
  return ''
}

/**
 * The opcode 29 operand table: what a short record is made of once letters and
 * spaces are accounted for. All 32 entries are assigned, so no operand of this
 * opcode can be invalid.
 */
// @ts-ignore: decorator
@lazy
const SYM_TABLE: StaticArray<u8> = [
  0x30, 0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x37, 0x38, 0x39, // 0-9
  0x2e, 0x2c, 0x2d, 0x2f, 0x3a, 0x3b, 0x5f, 0x28, 0x29, 0x25, // . , - / : ; _ ( ) %
  0x23, 0x22, 0x27, 0x21, 0x3f, 0x40, 0x3d, 0x2b, 0x2a, 0x26, 0x3c, 0x3e, // # " ' ! ? @ = + * & < >
]

/**
 * The opcode 30 operand table: everything else worth a token, as UTF-8.
 *
 * Held flattened — one byte blob and a start offset per index — because eleven
 * of the entries are multi-byte and an array of strings would cost a header per
 * entry for no gain. The accented characters are literals: the case mode does
 * not apply to them, which is why both cases appear.
 */
// @ts-ignore: decorator
@lazy
const EXT_BYTES: StaticArray<u8> = [
  0xc3, 0xb1, // ñ
  0xc3, 0xa1, // á
  0xc3, 0xa9, // é
  0xc3, 0xad, // í
  0xc3, 0xb3, // ó
  0xc3, 0xba, // ú
  0xc3, 0xbc, // ü
  0xc3, 0x91, // Ñ
  0xc3, 0x81, // Á
  0xc3, 0x89, // É
  0xc3, 0x8d, // Í
  0xc3, 0x93, // Ó
  0xc3, 0x9a, // Ú
  0xe2, 0x82, 0xac, // €
  0x24, // $
  0x7e, // ~
  0x60, // `
  0x5c, // \
  0x5b, // [
  0x5d, // ]
  0x5e, // ^
  0x7b, // {
  0x7d, // }
  0x7c, // |
  0x0a, // \n
  0x09, // \t
  0x0d, // \r
  0xc2, 0xbf, // ¿
]

/** Where each EXT_TABLE entry starts in EXT_BYTES, with a final sentinel so a
 * length is the difference between two of them. */
// @ts-ignore: decorator
@lazy
const EXT_AT: StaticArray<i32> = [
  0, 2, 4, 6, 8, 10, 12, 14, 16, 18, 20, 22, 24, 26, 29, 30, 31, 32, 33, 34, 35, 36, 37, 38, 39, 40,
  41, 42, 44,
]

/** How many units a payload of `size` bytes holds. */
@inline
export function payloadUnits(size: i32): i32 {
  return (size * 8) / 5
}

/** The decoder's last failure, so the caller can name it. */
export let packed5Error: i32 = P5_OK

/**
 * Appends the string held by a bare payload — a unit stream with no frame
 * header, which is the only form colbin embeds.
 *
 * `src` must begin at the payload and may run past it; whatever follows is free
 * slack for the group loader. `size` is the payload's byte length, which the
 * BLOB descriptor around it already carries. `upper` is the case mode the stream
 * opens in, which the descriptor's `enc` code says.
 *
 * Returns false with packed5Error set. It never traps: the group loader reads
 * through gather8, which stops at the end of the buffer rather than past it, so
 * a truncated payload is short units rather than adjacent memory.
 */
export function appendString(
  out: Writer,
  src: Uint8Array,
  from: i32,
  size: i32,
  upper: bool,
): bool {
  packed5Error = P5_OK
  if (from < 0 || size < 0 || from + size > src.length) {
    packed5Error = P5_TRUNCATED
    return false
  }

  const units = payloadUnits(size)
  const win = new StaticArray<u8>(WINDOW_UNITS + 8)
  let pos = 0
  let have = 0
  let got = 0
  let at = from

  let cur = upper
  let pending = false

  while (true) {
    if (have - pos < MAX_TOKEN_UNITS && got < units) {
      // Compact what is left of the window down, then refill a group at a time.
      let kept = 0
      for (let index = pos; index < have; index++) {
        unchecked((win[kept++] = unchecked(win[index])))
      }
      have = kept
      pos = 0
      while (have + 8 <= WINDOW_UNITS + 8 && got < units) {
        const word = gather8(src, at)
        unchecked((win[have + 0] = <u8>word & 31))
        unchecked((win[have + 1] = <u8>(word >> 5) & 31))
        unchecked((win[have + 2] = <u8>(word >> 10) & 31))
        unchecked((win[have + 3] = <u8>(word >> 15) & 31))
        unchecked((win[have + 4] = <u8>(word >> 20) & 31))
        unchecked((win[have + 5] = <u8>(word >> 25) & 31))
        unchecked((win[have + 6] = <u8>(word >> 30) & 31))
        unchecked((win[have + 7] = <u8>(word >> 35) & 31))
        have += 8
        got += 8
        at += 5
      }
      if (got > units) {
        // The final group overshoots the payload; those units are not real.
        have -= got - units
        got = units
      }
    }
    if (pos >= have) break

    const op = unchecked(win[pos])
    pos++

    if (op < OP_SPACE) {
      // Uppercase exactly when the two disagree: a simple toggle inverts the
      // mode for this letter only, and a letter is what clears it.
      out.writeByte(cur != pending ? 0x41 + op : 0x61 + op)
      pending = false
      continue
    }
    if (op == OP_SPACE) {
      out.writeByte(0x20)
      continue
    }
    if (op == OP_CASE_SIMPLE) {
      pending = true
      continue
    }
    if (op == OP_CASE_LONG) {
      cur = !cur
      continue
    }
    if (op == OP_SYMBOL) {
      if (pos >= have) {
        packed5Error = P5_TRUNCATED
        return false
      }
      out.writeByte(unchecked(SYM_TABLE[unchecked(win[pos])]))
      pos++
      continue
    }
    if (op == OP_EXT) {
      if (pos >= have) {
        packed5Error = P5_TRUNCATED
        return false
      }
      const index = unchecked(win[pos])
      pos++
      if (index != EXT_ESCAPE) {
        if (index >= EXT_RESERVED) {
          packed5Error = P5_RESERVED_SYMBOL
          return false
        }
        const start = unchecked(EXT_AT[index])
        const end = unchecked(EXT_AT[index + 1])
        for (let byte = start; byte < end; byte++) out.writeByte(unchecked(EXT_BYTES[byte]))
        continue
      }
      // A raw-byte escape: a count, then two units per byte. It carries bytes
      // rather than runes, which is what makes the codec byte-exact for input
      // that is not valid UTF-8.
      if (pos >= have) {
        packed5Error = P5_TRUNCATED
        return false
      }
      const count = <i32>unchecked(win[pos]) + 1
      pos++
      if (count > MAX_ESCAPE_RUN) {
        packed5Error = P5_BAD_ESCAPE
        return false
      }
      if (pos + 2 * count > have) {
        packed5Error = P5_TRUNCATED
        return false
      }
      for (let index = 0; index < count; index++) {
        const low = unchecked(win[pos])
        const high = unchecked(win[pos + 1])
        if (high > 7) {
          // A byte is eight bits: five and three.
          packed5Error = P5_BAD_ESCAPE
          return false
        }
        out.writeByte(low | (high << 5))
        pos += 2
      }
      continue
    }
    // OP_NUMBER: two units, low five bits first, 0..1023 as decimal.
    if (pos + 2 > have) {
      packed5Error = P5_TRUNCATED
      return false
    }
    const value = <u32>unchecked(win[pos]) | (<u32>unchecked(win[pos + 1]) << 5)
    pos += 2
    writeDecimal(out, value)
  }
  return true
}

/** `value` as decimal digits, value <= 1023. */
function writeDecimal(out: Writer, value: u32): void {
  if (value >= 1000) out.writeByte(0x30 + <u8>(value / 1000))
  if (value >= 100) out.writeByte(0x30 + <u8>((value / 100) % 10))
  if (value >= 10) out.writeByte(0x30 + <u8>((value / 10) % 10))
  out.writeByte(0x30 + <u8>(value % 10))
}
