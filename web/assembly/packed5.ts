// Port of github.com/ivanjoz/colbin/packed5.
//
// Operates on UTF-8 bytes throughout, which is what the Go version does too —
// its input is a Go string, and every classifier indexes it by byte. That is
// what makes the codec byte-exact for input that is not valid UTF-8, and it is
// why the module takes bytes rather than a host string (PLAN.md §2.1).

import { Reader, Writer, ERR_CORRUPT, ERR_TRUNCATED } from './bytes'
import { BitReader, BitWriter } from './bitstream'

// Header flag bits, in byte 0 of every frame.
const FLAG_PACKED5: u8 = 1 << 0
const FLAG_UPPERCASE: u8 = 1 << 1
const FLAG_NUMBER: u8 = 1 << 2
const LEN_SHIFT: u8 = 3
const LEN_INLINE: i32 = 30
const LEN_ESCAPE: i32 = 31

// Base alphabet opcodes above the 26 letters.
const OP_SPACE: u32 = 26
const OP_CASE_SIMPLE: u32 = 27
const OP_CASE_LONG: u32 = 28
const OP_SYMBOL: u32 = 29
const OP_SIMPLE: u32 = 30
const OP_NUMBER: u32 = 31

const ESCAPE_CODE: u32 = 15
const MAX_ESCAPE_RUN: i32 = 4
const NUMBER_MAX: i32 = 1023
const NUMBER_MAX_DIGITS: i32 = 4
const SYM_RESERVED: u32 = 30
const PAD_BITS_WIDTH: u8 = 3

// Token bit costs.
const COST_LETTER: i32 = 5
const COST_LETTER_CASED: i32 = 10
const COST_SPACE: i32 = 5
const COST_TOGGLE_LONG: i32 = 5
const COST_SYMBOL: i32 = 10
const COST_SIMPLE: i32 = 9
const COST_DASH: i32 = 5
const COST_NUMBER: i32 = 15
const COST_ESCAPE_BASE: i32 = 11
const COST_ESCAPE_BYTE: i32 = 8

// symTable, flattened. Entries 15 (€) and 24-29 (ñ á é í ó ú) are multi-byte,
// so the table is a byte blob plus a start offset per index rather than an
// array of characters.
const SYM_BYTES: StaticArray<u8> = [
  0x3c, 0x3e, 0x2f, 0x22, 0x27, 0x25, 0x23, 0x7c, 0x28, 0x29, 0x21, 0x3f, 0x24, 0x7e, 0x60,
  0xe2, 0x82, 0xac, // €
  0x40, 0x5c, 0x5b, 0x5d, 0x5e, 0x7b, 0x7d, 0x5f,
  0xc3, 0xb1, // ñ
  0xc3, 0xa1, // á
  0xc3, 0xa9, // é
  0xc3, 0xad, // í
  0xc3, 0xb3, // ó
  0xc3, 0xba, // ú
]
const SYM_OFFSET: StaticArray<i32> = [
  0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 18, 19, 20, 21, 22, 23, 24, 25, 26, 28,
  30, 32, 34, 36, 38,
]

// simpleTable. Index 15 is not a character; it is ESCAPE_CODE.
const SIMPLE_TABLE: StaticArray<u8> = [
  0x30, 0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x37, 0x38, 0x39, 0x2e, 0x2d, 0x2b, 0x2a, 0x3d, 0,
]

// Inverse tables for the single-byte entries. -1 means "no operand in that
// table". The multi-byte symTable entries are matched by symbolRune instead.
const asciiSym = new StaticArray<i8>(128)
const asciiSimple = new StaticArray<i8>(128)

function initTables(): void {
  for (let i = 0; i < 128; i++) {
    unchecked((asciiSym[i] = -1))
    unchecked((asciiSimple[i] = -1))
  }
  for (let i: i32 = 0; i < <i32>SYM_RESERVED; i++) {
    const start = unchecked(SYM_OFFSET[i])
    if (unchecked(SYM_OFFSET[i + 1]) - start == 1) {
      unchecked((asciiSym[unchecked(SYM_BYTES[start])] = <i8>i))
    }
  }
  for (let i = 0; i < 16; i++) {
    if (<u32>i != ESCAPE_CODE) unchecked((asciiSimple[unchecked(SIMPLE_TABLE[i])] = <i8>i))
  }
}
initTables()

/**
 * symTable index for a multi-byte sequence at i, or -1, with the byte width in
 * symWidth.
 *
 * Go decodes a rune here and looks it up. Matching the seven byte sequences
 * directly is exactly equivalent: each is a valid UTF-8 encoding, so Go's
 * decoder accepts precisely these and rejects everything else with w == 1.
 */
let symWidth: i32 = 0

function symbolRune(s: Uint8Array, i: i32): i32 {
  const n = s.length
  const c = unchecked(s[i])
  if (c == 0xe2 && i + 2 < n && unchecked(s[i + 1]) == 0x82 && unchecked(s[i + 2]) == 0xac) {
    symWidth = 3
    return 15
  }
  if (c == 0xc3 && i + 1 < n) {
    const d = unchecked(s[i + 1])
    symWidth = 2
    if (d == 0xb1) return 24
    if (d == 0xa1) return 25
    if (d == 0xa9) return 26
    if (d == 0xad) return 27
    if (d == 0xb3) return 28
    if (d == 0xba) return 29
  }
  symWidth = 1
  return -1
}

@inline function isLetter(c: u8): bool {
  return <u8>((c | 0x20) - 0x61) < 26
}

@inline function isDigit(c: u8): bool {
  return <u8>(c - 0x30) < 10
}

@inline function isUpper(c: u8): bool {
  return (c & 0x20) == 0
}

@inline function letterIndex(c: u8): u32 {
  return <u32>(c | 0x20) - 0x61
}

// symbolAt's multi-value return, as module state: it runs on every input byte in
// both passes and a per-call allocation would dominate the encoder.
const SYM_KIND_DASH: u8 = 0
const SYM_KIND_SYMBOL: u8 = 1
const SYM_KIND_SIMPLE: u8 = 2

let symKind: u8 = 0
let symPay: u32 = 0
let symByteWidth: i32 = 0
let symCost: i32 = 0

/** Non-letter, non-space token for the byte at i. False when it must be escaped. */
function symbolAt(s: Uint8Array, i: i32, number: bool): bool {
  const c = unchecked(s[i])
  if (c < 0x80) {
    // With number mode off, opcode 31 carries '-' for 5 bits rather than the 9
    // the simple table would charge.
    if (!number && c == 0x2d) {
      symKind = SYM_KIND_DASH
      symByteWidth = 1
      symCost = COST_DASH
      return true
    }
    const sym = unchecked(asciiSym[c])
    if (sym >= 0) {
      symKind = SYM_KIND_SYMBOL
      symPay = <u32>sym
      symByteWidth = 1
      symCost = COST_SYMBOL
      return true
    }
    const simple = unchecked(asciiSimple[c])
    if (simple >= 0) {
      symKind = SYM_KIND_SIMPLE
      symPay = <u32>simple
      symByteWidth = 1
      symCost = COST_SIMPLE
      return true
    }
    return false
  }
  const idx = symbolRune(s, i)
  if (idx >= 0) {
    symKind = SYM_KIND_SYMBOL
    symPay = <u32>idx
    symByteWidth = symWidth
    symCost = COST_SYMBOL
    return true
  }
  return false
}

/** Whether the byte at i has no token of its own. */
function mustEscape(s: Uint8Array, i: i32, number: bool): bool {
  const c = unchecked(s[i])
  if (isLetter(c) || c == 0x20) return false
  return !symbolAt(s, i, number)
}

/** The greedy walk, emitting each token as bits at the point it is decided. */
function writeStream(bw: BitWriter, s: Uint8Array, upper: bool, number: bool): void {
  const n = s.length
  let cur = upper
  let i = 0
  while (i < n) {
    const c = unchecked(s[i])

    if (isLetter(c)) {
      const isUp = isUpper(c)
      const idx = letterIndex(c)
      if (isUp == cur) {
        bw.writeBits(idx, 5)
        i++
        continue
      }
      // Two simple toggles cost what a pair of long ones does, so the long form
      // only wins from three.
      let run = 0
      for (let j = i; j < n && isLetter(unchecked(s[j])) && isUpper(unchecked(s[j])) != cur; j++) {
        run++
      }
      if (run >= 3) {
        bw.writeBits(OP_CASE_LONG, 5)
        cur = !cur
        continue // re-read the letter, now in the matching mode
      }
      bw.writeBits(OP_CASE_SIMPLE, 5)
      bw.writeBits(idx, 5)
      i++
      continue
    }

    if (c == 0x20) {
      bw.writeBits(OP_SPACE, 5)
      i++
      continue
    }

    if (number && isDigit(c)) {
      // The longest prefix that fits in ten bits. A token may not carry a
      // leading zero: it decodes as a plain decimal integer, so "00123" must not
      // come back as "123". A lone "0" is fine.
      let v = 0
      let best = 0
      let bestLen = 0
      for (let l = 1; l <= NUMBER_MAX_DIGITS && i + l <= n; l++) {
        const d = unchecked(s[i + l - 1])
        if (!isDigit(d) || (l > 1 && unchecked(s[i]) == 0x30)) break
        v = v * 10 + <i32>(d - 0x30)
        if (v > NUMBER_MAX) break
        best = v
        bestLen = l
      }
      if (bestLen == 1) {
        bw.writeBits(OP_SIMPLE, 5)
        bw.writeBits(<u32>unchecked(asciiSimple[c]), 4)
      } else {
        bw.writeBits(OP_NUMBER, 5)
        bw.writeBits(<u32>best, 10)
      }
      i += bestLen
      continue
    }

    if (symbolAt(s, i, number)) {
      const kind = symKind
      const pay = symPay
      const width = symByteWidth
      if (kind == SYM_KIND_DASH) {
        bw.writeBits(OP_NUMBER, 5)
      } else if (kind == SYM_KIND_SYMBOL) {
        bw.writeBits(OP_SYMBOL, 5)
        bw.writeBits(pay, 5)
      } else {
        bw.writeBits(OP_SIMPLE, 5)
        bw.writeBits(pay, 4)
      }
      i += width
      continue
    }

    let run = 1
    while (run < MAX_ESCAPE_RUN && i + run < n && mustEscape(s, i + run, number)) run++
    bw.writeBits(OP_SIMPLE, 5)
    bw.writeBits(ESCAPE_CODE, 4)
    bw.writeBits(<u32>run - 1, 2)
    for (let k = 0; k < run; k++) bw.writeBits(<u32>unchecked(s[i + k]), 8)
    i += run
  }
}

// plan's three results, as module state for the same reason symbolAt's are.
let planBits: i32 = 0
let planUpper: bool = false
let planNumber: bool = false

/**
 * Exact greedy-scan cost for all four flag settings in one pass.
 *
 * The flags are not guessed: UPPERCASE_DOMINANT and ENABLE_NUMBER_0_1023 change
 * what the scan emits. Counting letters to pick the dominant case is the obvious
 * shortcut and it is wrong often enough to matter — what decides it is the
 * number of case *runs*.
 */
function plan(s: Uint8Array): void {
  const n = s.length
  let lowerCaseBits = 0
  let upperCaseBits = 0
  let lowerMode = false
  let upperMode = true
  let plainBits = 0
  let numberBits = 0

  let i = 0
  while (i < n) {
    const c = unchecked(s[i])

    if (isLetter(c)) {
      const runUpper = isUpper(c)
      let j = i + 1
      while (j < n && isLetter(unchecked(s[j])) && isUpper(unchecked(s[j])) == runUpper) j++
      const run = j - i

      if (runUpper == lowerMode) {
        lowerCaseBits += COST_LETTER * run
      } else if (run >= 3) {
        lowerCaseBits += COST_TOGGLE_LONG + COST_LETTER * run
        lowerMode = runUpper
      } else {
        lowerCaseBits += COST_LETTER_CASED * run
      }

      if (runUpper == upperMode) {
        upperCaseBits += COST_LETTER * run
      } else if (run >= 3) {
        upperCaseBits += COST_TOGGLE_LONG + COST_LETTER * run
        upperMode = runUpper
      } else {
        upperCaseBits += COST_LETTER_CASED * run
      }
      i = j
      continue
    }

    if (c == 0x20) {
      plainBits += COST_SPACE
      numberBits += COST_SPACE
      i++
      continue
    }

    if (isDigit(c)) {
      let j = i + 1
      while (j < n && isDigit(unchecked(s[j]))) j++
      plainBits += COST_SIMPLE * (j - i)
      while (i < j) {
        let v = 0
        let bestLen = 0
        for (let l = 1; l <= NUMBER_MAX_DIGITS && i + l <= j; l++) {
          if (l > 1 && unchecked(s[i]) == 0x30) break
          v = v * 10 + <i32>(unchecked(s[i + l - 1]) - 0x30)
          if (v > NUMBER_MAX) break
          bestLen = l
        }
        if (bestLen == 1) numberBits += COST_SIMPLE
        else numberBits += COST_NUMBER
        i += bestLen
      }
      continue
    }

    if (symbolAt(s, i, false)) {
      plainBits += symCost
      if (c == 0x2d) numberBits += COST_SIMPLE
      else numberBits += symCost
      i += symByteWidth
      continue
    }

    let run = 1
    while (run < MAX_ESCAPE_RUN && i + run < n && mustEscape(s, i + run, false)) run++
    const cost = COST_ESCAPE_BASE + COST_ESCAPE_BYTE * run
    plainBits += cost
    numberBits += cost
    i += run
  }

  planBits = lowerCaseBits + plainBits
  planUpper = false
  planNumber = false
  let candidate = upperCaseBits + plainBits
  if (candidate < planBits) {
    planBits = candidate
    planUpper = true
  }
  candidate = lowerCaseBits + numberBits
  if (candidate < planBits) {
    planBits = candidate
    planUpper = false
    planNumber = true
  }
  candidate = upperCaseBits + numberBits
  if (candidate < planBits) {
    planBits = candidate
    planUpper = true
    planNumber = true
  }
}

@inline function payloadBytes(bits: i32): i32 {
  return (<i32>PAD_BITS_WIDTH + bits + 7) / 8
}

@inline function uvarintLen(v: i32): i32 {
  let n = 1
  while (v >= 0x80) {
    v >>= 7
    n++
  }
  return n
}

function appendUvarint(w: Writer, v: i32): void {
  while (v >= 0x80) {
    w.writeByte(<u8>(v | 0x80))
    v >>= 7
  }
  w.writeByte(<u8>v)
}

@inline function frameOverhead(n: i32): i32 {
  return n <= LEN_INLINE ? 1 : 1 + uvarintLen(n)
}

function appendHeader(w: Writer, flags: u8, payloadLen: i32): void {
  if (payloadLen <= LEN_INLINE) {
    w.writeByte(flags | (<u8>payloadLen << LEN_SHIFT))
    return
  }
  w.writeByte(flags | (<u8>LEN_ESCAPE << LEN_SHIFT))
  appendUvarint(w, payloadLen)
}

/**
 * Encodes s as one self-delimiting frame appended to w.
 *
 * The packed form is used only when its frame is strictly smaller than the raw
 * one, so this never inflates: the result is never longer than s plus framing.
 */
export function append(w: Writer, s: Uint8Array): void {
  if (s.length == 0) {
    w.writeByte(0) // raw, empty payload
    return
  }
  plan(s)
  const payload = payloadBytes(planBits)
  if (frameOverhead(payload) + payload >= frameOverhead(s.length) + s.length) {
    appendHeader(w, 0, s.length)
    w.writeBytes(s, 0, s.length)
    return
  }

  let flags = FLAG_PACKED5
  if (planUpper) flags |= FLAG_UPPERCASE
  if (planNumber) flags |= FLAG_NUMBER
  appendHeader(w, flags, payload)

  const bw = new BitWriter(w)
  bw.writeBits(<u32>(payload * 8 - <i32>PAD_BITS_WIDTH - planBits), PAD_BITS_WIDTH)
  writeStream(bw, s, planUpper, planNumber)
  bw.flush()
}

/** Bytes append() would add for s, without encoding it. */
export function size(s: Uint8Array): i32 {
  if (s.length == 0) return 1
  plan(s)
  const payload = payloadBytes(planBits)
  const raw = frameOverhead(s.length) + s.length
  const packed = frameOverhead(payload) + payload
  return packed < raw ? packed : raw
}

/** Reads an LEB128 length, rejecting overlong forms and impossible values. */
function readUvarint(r: Reader): i32 {
  let v: u64 = 0
  let shift: u32 = 0
  for (let i = 0; i < 9; i++) {
    const b = r.readByte()
    if (!r.ok) return -1
    v |= (<u64>(b & 0x7f)) << shift
    if (b < 0x80) {
      if (v > <u64>0x7fffffff) {
        r.fail(ERR_CORRUPT)
        return -1
      }
      return <i32>v
    }
    shift += 7
  }
  r.fail(ERR_CORRUPT)
  return -1
}

/**
 * Decodes one frame from r, appending the decoded bytes to out.
 * Returns false with r.err set on failure.
 */
export function decode(r: Reader, out: Writer): bool {
  const hdr = r.readByte()
  if (!r.ok) return false

  let length = <i32>(hdr >> LEN_SHIFT)
  if (length == LEN_ESCAPE) {
    const v = readUvarint(r)
    if (v < 0) return false
    // The escape must not re-encode a length the header could have held: one
    // length has one encoding, so a frame has one byte representation.
    if (v <= LEN_INLINE) {
      r.fail(ERR_CORRUPT)
      return false
    }
    length = v
  }
  if (!r.has(length)) return false

  const start = r.pos
  r.pos += length

  if ((hdr & FLAG_PACKED5) == 0) {
    out.writeBytes(r.buf, start, length)
    return true
  }
  return decodeStream(r, out, start, length, (hdr & FLAG_UPPERCASE) != 0, (hdr & FLAG_NUMBER) != 0)
}

/**
 * Walks the packed bitstream.
 *
 * A pending simple toggle is cleared only by a letter, and a long toggle leaves
 * it alone. The encoder emits CASE_TOGGLE_SIMPLE only immediately before the
 * letter it applies to, so neither case arises in a frame this codec wrote; they
 * are defined so a hand-built or corrupt stream decodes deterministically rather
 * than by accident.
 */
function decodeStream(
  r: Reader,
  out: Writer,
  start: i32,
  length: i32,
  upper: bool,
  number: bool
): bool {
  const br = new BitReader(r.buf, start, length)
  const pad = br.read(PAD_BITS_WIDTH)
  if (!br.ok) {
    r.fail(ERR_TRUNCATED)
    return false
  }
  if (<i32>pad > br.remaining) {
    r.fail(ERR_CORRUPT)
    return false
  }
  br.limit -= <i32>pad

  let cur = upper
  let pending = false

  while (br.remaining >= 5) {
    const op = br.read(5)
    if (!br.ok) {
      r.fail(ERR_TRUNCATED)
      return false
    }

    if (op < OP_SPACE) {
      // Uppercase exactly when the mode and a pending toggle disagree.
      const base: u8 = cur != pending ? 0x41 : 0x61
      out.writeByte(base + <u8>op)
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
      const idx = br.read(5)
      if (!br.ok) {
        r.fail(ERR_TRUNCATED)
        return false
      }
      if (idx >= SYM_RESERVED) {
        r.fail(ERR_CORRUPT)
        return false
      }
      const from = unchecked(SYM_OFFSET[idx])
      const to = unchecked(SYM_OFFSET[idx + 1])
      for (let k = from; k < to; k++) out.writeByte(unchecked(SYM_BYTES[k]))
      continue
    }
    if (op == OP_SIMPLE) {
      const v = br.read(4)
      if (!br.ok) {
        r.fail(ERR_TRUNCATED)
        return false
      }
      if (v != ESCAPE_CODE) {
        out.writeByte(unchecked(SIMPLE_TABLE[v]))
        continue
      }
      const cnt = br.read(2)
      if (!br.ok) {
        r.fail(ERR_TRUNCATED)
        return false
      }
      for (let k: u32 = 0; k <= cnt; k++) {
        const b = br.read(8)
        if (!br.ok) {
          r.fail(ERR_TRUNCATED)
          return false
        }
        out.writeByte(<u8>b)
      }
      continue
    }

    // OP_NUMBER
    if (!number) {
      out.writeByte(0x2d)
      continue
    }
    const v = br.read(10)
    if (!br.ok) {
      r.fail(ERR_TRUNCATED)
      return false
    }
    writeDecimal(out, v)
  }

  // Fewer than 5 bits left is the stream's end. Anything else is a token the
  // pad count did not account for, which the encoder cannot produce.
  if (br.remaining != 0) {
    r.fail(ERR_TRUNCATED)
    return false
  }
  return true
}

/** v as decimal digits, v <= 1023. */
function writeDecimal(out: Writer, v: u32): void {
  if (v >= 1000) out.writeByte(<u8>(0x30 + v / 1000))
  if (v >= 100) out.writeByte(<u8>(0x30 + (v / 100) % 10))
  if (v >= 10) out.writeByte(<u8>(0x30 + (v / 10) % 10))
  out.writeByte(<u8>(0x30 + (v % 10)))
}
