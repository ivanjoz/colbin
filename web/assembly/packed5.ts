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

// ---- the encoder ------------------------------------------------------------
//
// One greedy left-to-right scan, fused with the packer:
//
//   - an opposite-case run of one or two letters takes a CASE_TOGGLE_SIMPLE
//     each; three or more takes a CASE_TOGGLE_LONG.
//   - a decimal run takes the longest prefix the number token can legally carry,
//     except that a lone digit takes the symbol table instead, since ten bits
//     beat fifteen.
//   - a byte with no token of its own is escaped, merged with the following
//     unrepresentable bytes up to the escape's four-byte limit.
//
// There is no planning pass. The codec this replaced had two behavioural header
// flags — a default case and a number mode — which changed what the scan emitted
// and so had to be priced before it ran, in a second full walk that measured a
// third to a half of encode time. Both are gone: the number token is
// unconditional, and the case mode is an ordinary CASE_TOGGLE_LONG that
// opensUpper hoists into the descriptor when it would have landed first.
//
// It is not a size-optimal encoder. The optimum is a shortest path over
// (offset, case mode) nodes, which is exact and several times slower. What
// matters here is narrower and is what the vectors check: it must produce the
// same bytes Go's does, because two encoders that disagree about a string
// produce two different messages for one record.

/** The inverse tables, for the single-byte entries. -1 means "no operand in
 * that table". Built once, because a StaticArray literal of 128 mostly-minus-one
 * entries is worse to read than the loop that fills it. */
// @ts-ignore: decorator
@lazy
const ASCII_SYM = fillAscii(SYM_TABLE)
// @ts-ignore: decorator
@lazy
const ASCII_EXT = buildAsciiExt()
/** The multi-byte entries, keyed on the low six bits of the UTF-8 continuation
 * byte. Every non-ASCII entry is Latin-1 Supplement — a C2 or C3 lead — except
 * the euro sign, and continuation bytes run 0x80..0xBF, so within well-formed
 * input those six bits identify the character outright. Spanish text takes this
 * path on nearly every word. */
// @ts-ignore: decorator
@lazy
const LATIN_C2 = buildLatin(0xc2)
// @ts-ignore: decorator
@lazy
const LATIN_C3 = buildLatin(0xc3)
/** The euro sign's extTable index, which is the one three-byte entry. */
const EXT_EURO: i32 = 13

function fillAscii(table: StaticArray<u8>): StaticArray<i8> {
  const out = new StaticArray<i8>(128)
  for (let index = 0; index < 128; index++) unchecked((out[index] = -1))
  for (let index = 0; index < table.length; index++) {
    unchecked((out[unchecked(table[index])] = <i8>index))
  }
  return out
}

function buildAsciiExt(): StaticArray<i8> {
  const out = new StaticArray<i8>(128)
  for (let index = 0; index < 128; index++) unchecked((out[index] = -1))
  for (let index = 0; index < <i32>EXT_RESERVED; index++) {
    const start = unchecked(EXT_AT[index])
    if (unchecked(EXT_AT[index + 1]) - start != 1) continue
    out[unchecked(EXT_BYTES[start])] = <i8>index
  }
  return out
}

function buildLatin(lead: u8): StaticArray<i8> {
  const out = new StaticArray<i8>(64)
  for (let index = 0; index < 64; index++) unchecked((out[index] = -1))
  for (let index = 0; index < <i32>EXT_RESERVED; index++) {
    const start = unchecked(EXT_AT[index])
    if (unchecked(EXT_AT[index + 1]) - start != 2) continue
    if (unchecked(EXT_BYTES[start]) != lead) continue
    out[unchecked(EXT_BYTES[start + 1]) & 0x3f] = <i8>index
  }
  return out
}

@inline
function isLetter(c: u8): bool {
  return <u8>((c | 0x20) - 0x61) < 26
}

@inline
function isDigit(c: u8): bool {
  return <u8>(c - 0x30) < 10
}

/** The case of a byte already known to be an ASCII letter: bit 5 is the case bit. */
@inline
function isUpper(c: u8): bool {
  return (c & 0x20) == 0
}

@inline
function letterIndex(c: u8): u8 {
  return (c | 0x20) - 0x61
}

/** extMulti's two results, as module state: AssemblyScript has no tuple and this
 * runs on every non-ASCII byte of both the scan and the escape test. */
let multiIndex: i32 = -1
let multiWidth: i32 = 0

/** The extTable index of the multi-byte character at `at`, or -1. */
function extMulti(src: Uint8Array, at: i32): void {
  multiIndex = -1
  multiWidth = 0
  if (at + 1 >= src.length || (unchecked(src[at + 1]) & 0xc0) != 0x80) return
  const lead = unchecked(src[at])
  const low = unchecked(src[at + 1]) & 0x3f
  if (lead == 0xc2) {
    multiIndex = <i32>unchecked(LATIN_C2[low])
    multiWidth = 2
    return
  }
  if (lead == 0xc3) {
    multiIndex = <i32>unchecked(LATIN_C3[low])
    multiWidth = 2
    return
  }
  if (lead == 0xe2 && at + 2 < src.length && unchecked(src[at + 1]) == 0x82 &&
      unchecked(src[at + 2]) == 0xac) {
    multiIndex = EXT_EURO
    multiWidth = 3
  }
}

/** Whether the byte at `at` has no token of its own. */
function escapes(src: Uint8Array, at: i32): bool {
  const c = unchecked(src[at])
  if (isLetter(c) || c == 0x20) return false
  if (c < 0x80) return unchecked(ASCII_SYM[c]) < 0 && unchecked(ASCII_EXT[c]) < 0
  extMulti(src, at)
  return multiIndex < 0
}

/**
 * The case mode the stream starts in, which the descriptor carries for free in
 * one bit.
 *
 * The rule is the first letter's case, and it is never worse than starting
 * lower. Take the leading run of k letters in the opposite case: paying for it
 * from lower costs 2k units for k of one or two and 1+k from three up, where
 * starting in that mode costs k plus the one toggle that returns. Those are
 * equal at k >= 3 and strictly better below it, and after the run both encoders
 * are in the same mode with the same string left.
 */
export function opensUpper(src: Uint8Array, from: i32, size: i32): bool {
  for (let at = from; at < from + size; at++) {
    const c = unchecked(src[at])
    if (isLetter(c)) return isUpper(c)
  }
  return false
}

/**
 * The packer: eight five-bit units in, five bytes out.
 *
 * `limit` is the byte position the payload may not pass. The packed form is only
 * ever used when it beats the raw bytes, so bounding the buffer by the raw
 * length is both the allocation bound and the early-out — a stream that would
 * not have been kept stops being written the moment it grows past its own
 * budget, which costs a compare per group rather than a pass of its own.
 */
class UnitWriter {
  buf: Uint8Array
  at: i32
  limit: i32
  acc: u64 = 0
  pending: i32 = 0
  units: i32 = 0
  over: bool = false

  constructor(buf: Uint8Array, at: i32, limit: i32) {
    this.buf = buf
    this.at = at
    this.limit = limit
  }

  /** One unit. Every eighth stores a group; the rest are a shift and an or. */
  put(value: u8): void {
    this.acc |= (<u64>value) << <u64>(5 * this.pending)
    this.pending++
    this.units++
    if (this.pending == 8) {
      if (this.at > this.limit) {
        this.over = true
      } else {
        store<u64>(this.buf.dataStart + <usize>this.at, this.acc)
        this.at += 5
      }
      this.pending = 0
      this.acc = 0
    }
  }

  /**
   * Flushes the partial group and pads the stream to the unit grid.
   *
   * One trailing CASE_TOGGLE_SIMPLE lands it there when the byte rounding leaves
   * room for a unit the tokens did not fill. It applies to no letter, so it
   * decodes to nothing, and it cannot grow the payload: the gap exists exactly
   * when one more unit still fits the same number of bytes.
   */
  finish(): i32 {
    if (payloadUnits(payloadBytes(this.units)) > this.units) this.put(OP_CASE_SIMPLE)
    if (this.pending > 0) {
      if (this.at > this.limit) {
        this.over = true
      } else {
        store<u64>(this.buf.dataStart + <usize>this.at, this.acc)
        this.at += payloadBytes(this.pending)
        this.pending = 0
        this.acc = 0
      }
    }
    return this.over ? -1 : this.at
  }
}

/** How many bytes n units occupy. */
@inline
function payloadBytes(units: i32): i32 {
  return (units * 5 + 7) / 8
}

/** The greedy scan, emitting units as each token is decided. */
function tokenize(w: UnitWriter, src: Uint8Array, from: i32, size: i32, upper: bool): void {
  const end = from + size
  let cur = upper
  let at = from
  while (at < end && !w.over) {
    const c = unchecked(src[at])

    if (isLetter(c)) {
      const index = letterIndex(c)
      if (isUpper(c) == cur) {
        w.put(index)
        at++
        continue
      }
      // Two simple toggles cost what a pair of long ones does, so the long form
      // only pays from a run of three.
      let run = 0
      for (let scan = at; scan < end; scan++) {
        const next = unchecked(src[scan])
        if (!isLetter(next) || isUpper(next) == cur) break
        run++
      }
      if (run >= 3) {
        w.put(OP_CASE_LONG)
        cur = !cur
        continue // re-read the letter, now in the matching mode
      }
      w.put(OP_CASE_SIMPLE)
      w.put(index)
      at++
      continue
    }

    if (c == 0x20) {
      w.put(OP_SPACE)
      at++
      continue
    }

    if (isDigit(c)) {
      // The longest prefix ten bits can hold. A token may not carry a leading
      // zero, since it decodes as a plain decimal integer and "00123" must not
      // come back as "123"; a lone "0" is fine.
      let value = 0
      let best = 0
      let bestLen = 0
      for (let length = 1; length <= 4 && at + length <= end; length++) {
        const digit = unchecked(src[at + length - 1])
        if (!isDigit(digit) || (length > 1 && unchecked(src[at]) == 0x30)) break
        value = value * 10 + <i32>(digit - 0x30)
        if (value > 1023) break
        best = value
        bestLen = length
      }
      if (bestLen == 1) {
        w.put(OP_SYMBOL)
        w.put(c - 0x30)
      } else {
        w.put(OP_NUMBER)
        w.put(<u8>(best & 31))
        w.put(<u8>(best >> 5))
      }
      at += bestLen
      continue
    }

    if (c < 0x80) {
      const sym = unchecked(ASCII_SYM[c])
      if (sym >= 0) {
        w.put(OP_SYMBOL)
        w.put(<u8>sym)
        at++
        continue
      }
      const ext = unchecked(ASCII_EXT[c])
      if (ext >= 0) {
        w.put(OP_EXT)
        w.put(<u8>ext)
        at++
        continue
      }
    } else {
      extMulti(src, at)
      if (multiIndex >= 0) {
        w.put(OP_EXT)
        w.put(<u8>multiIndex)
        at += multiWidth
        continue
      }
    }

    // A raw escape: a count, then two units per byte. It carries bytes rather
    // than runes, which is what makes the codec byte-exact for input that is not
    // valid UTF-8, and lets the three-unit header be amortised over a run.
    let run = 1
    while (run < MAX_ESCAPE_RUN && at + run < end && escapes(src, at + run)) run++
    w.put(OP_EXT)
    w.put(EXT_ESCAPE)
    w.put(<u8>(run - 1))
    for (let index = 0; index < run; index++) {
      const byte = unchecked(src[at + index])
      w.put(byte & 31)
      w.put(byte >> 5)
    }
    at += run
  }
}

/** appendPayload's results, as module state. */
export let packedSize: i32 = 0
export let packedUpper: bool = false

/**
 * Appends the packed unit stream for a string onto `out` — no header, no length
 * — leaving its byte size in `packedSize` and the case mode it opens in in
 * `packedUpper`.
 *
 * Returns false when the packed form would not be smaller than the raw bytes,
 * with nothing appended. The encoder discovers that by writing into a buffer
 * bounded at the raw length and noticing when it runs out, so the answer costs a
 * bounds test per group rather than a pass of its own.
 *
 * The packer stores eight bytes per group and advances five, so it overshoots
 * its last group by three; the buffer is grown by eight past the limit to give
 * it room.
 */
export function appendPayload(out: Writer, src: Uint8Array, from: i32, size: i32): bool {
  packedSize = 0
  packedUpper = false
  if (size == 0) return false

  packedUpper = opensUpper(src, from, size)
  // A payload that reaches `size` has already lost to the raw bytes, so that is
  // both the buffer bound and the point the writer gives up at.
  out.ensure(size + 8)
  const start = out.len
  const w = new UnitWriter(out.buf, start, start + size)
  tokenize(w, src, from, size, packedUpper)
  const end = w.finish()
  if (end < 0 || end - start >= size) return false
  out.len = end
  packedSize = end - start
  return true
}
