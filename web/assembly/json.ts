// A UTF-8 JSON scanner producing a flat value tree.
//
// Why this exists rather than JSON.parse on the host (PLAN.md §2.1): JSON.parse
// turns 7295013456321098765 into 7295013456321098800. colbin has an exact int64
// column, so the parse has to be exact too, and that means reading the digits
// here.
//
// The tree is parallel arrays rather than objects: one node is a kind, a 64-bit
// payload, two i32 fields whose meaning depends on the kind, and a source offset
// kept only so a later diagnostic can point at the value.

import { Diag, D_LIMIT, D_NUMBER, D_SYNTAX } from './diag'
import { Writer } from './bytes'
import { DEC_OK, DEC_OVERFLOW, decStatus, decimalToFloat } from './decimal'

export const K_NULL: u8 = 0
export const K_BOOL: u8 = 1
export const K_INT: u8 = 2
export const K_UINT: u8 = 3
export const K_FLOAT: u8 = 4
export const K_STRING: u8 = 5
export const K_ARRAY: u8 = 6
export const K_OBJECT: u8 = 7

/** PLAN.md §4.2. Both are proposals in the plan, and both are enforced here. */
export const MAX_DEPTH: i32 = 64
export const MAX_INPUT: i32 = 64 * 1024 * 1024

/**
 * Significand digits kept. Deciding a float64 rounding never needs more than
 * about 767, so this is past the point where another digit can change the
 * result; anything beyond only sets the sticky flag.
 */
export const MAX_SIGNIFICAND: i32 = 800

/**
 * Keys scanned when resolving a duplicate. Every object becomes a struct, and a
 * struct may hold at most 254 fields, so an object wider than this is refused
 * before its duplicates could matter.
 */
const DEDUP_SCAN_LIMIT: i32 = 512

export class Doc {
  src: Uint8Array
  kind: Array<u8> = []
  /** Int value, uint bits, bool as 0/1, or f64 bits — read with the kind. */
  num: Array<i64> = []
  /** String start in `text`, or the first child index. */
  a: Array<i32> = []
  /** String byte length, or the child count. */
  b: Array<i32> = []
  /** Source offset of the value, for diagnostics. */
  off: Array<i32> = []

  /** Child node indices of arrays and objects, contiguous per parent. */
  child: Array<i32> = []
  /** Key start and length in `text`, parallel to `child`; objects only. */
  keyA: Array<i32> = []
  keyB: Array<i32> = []

  /** Decoded string bytes: keys and values, escapes already resolved. */
  text: Writer = new Writer(256)

  root: i32 = -1

  constructor(src: Uint8Array) {
    this.src = src
  }

  @inline kindOf(node: i32): u8 {
    return unchecked(this.kind[node])
  }

  @inline count(node: i32): i32 {
    return unchecked(this.b[node])
  }

  @inline childAt(node: i32, i: i32): i32 {
    return unchecked(this.child[unchecked(this.a[node]) + i])
  }

  /** Bytes of a string node, as a view into the text arena. */
  @inline strOf(node: i32): Uint8Array {
    return this.text.buf.subarray(unchecked(this.a[node]), unchecked(this.a[node]) + unchecked(this.b[node]))
  }

  /** Bytes of the i-th key of an object node. */
  @inline keyOf(node: i32, i: i32): Uint8Array {
    const slot = unchecked(this.a[node]) + i
    return this.text.buf.subarray(unchecked(this.keyA[slot]), unchecked(this.keyA[slot]) + unchecked(this.keyB[slot]))
  }

  @inline floatOf(node: i32): f64 {
    return reinterpret<f64>(unchecked(this.num[node]))
  }
}

// Powers of ten that are exactly representable in f64. Both operands of the
// fast path below have to be exact for the single rounding to be correct.
const POW10: StaticArray<f64> = [
  1e0, 1e1, 1e2, 1e3, 1e4, 1e5, 1e6, 1e7, 1e8, 1e9, 1e10, 1e11, 1e12, 1e13, 1e14, 1e15, 1e16,
  1e17, 1e18, 1e19, 1e20, 1e21, 1e22,
]

export class Parser {
  src: Uint8Array
  pos: i32 = 0
  depth: i32 = 0
  doc: Doc
  diag: Diag

  constructor(src: Uint8Array, diag: Diag) {
    this.src = src
    this.doc = new Doc(src)
    this.diag = diag
  }

  @inline atEnd(): bool {
    return this.pos >= this.src.length
  }

  @inline peek(): u8 {
    return this.pos < this.src.length ? unchecked(this.src[this.pos]) : 0
  }

  fail(code: i32, offset: i32, message: string): void {
    this.diag.fail(code, offset, '', message)
  }

  skipSpace(): void {
    while (this.pos < this.src.length) {
      const c = unchecked(this.src[this.pos])
      if (c == 0x20 || c == 0x09 || c == 0x0a || c == 0x0d) this.pos++
      else break
    }
  }

  /** Appends a node and returns its index. */
  @inline node(kind: u8, num: i64, a: i32, b: i32, off: i32): i32 {
    const idx = this.doc.kind.length
    this.doc.kind.push(kind)
    this.doc.num.push(num)
    this.doc.a.push(a)
    this.doc.b.push(b)
    this.doc.off.push(off)
    return idx
  }

  parse(): bool {
    if (this.src.length > MAX_INPUT) {
      this.fail(D_LIMIT, 0, 'input is larger than the 64 MiB limit')
      return false
    }
    this.skipSpace()
    const root = this.value()
    if (!this.diag.ok) return false
    this.skipSpace()
    if (!this.atEnd()) {
      this.fail(D_SYNTAX, this.pos, 'trailing content after the top-level value')
      return false
    }
    this.doc.root = root
    return true
  }

  value(): i32 {
    if (this.atEnd()) {
      this.fail(D_SYNTAX, this.pos, 'unexpected end of input')
      return -1
    }
    const start = this.pos
    const c = this.peek()

    if (c == 0x7b) return this.object()
    if (c == 0x5b) return this.array()
    if (c == 0x22) {
      const at = this.doc.text.len
      const n = this.stringBytes()
      if (!this.diag.ok) return -1
      return this.node(K_STRING, 0, at, n, start)
    }
    if (c == 0x74) return this.literal('true', K_BOOL, 1)
    if (c == 0x66) return this.literal('false', K_BOOL, 0)
    if (c == 0x6e) return this.literal('null', K_NULL, 0)
    if (c == 0x2d || (c >= 0x30 && c <= 0x39)) return this.number()

    this.fail(D_SYNTAX, this.pos, 'expected a JSON value')
    return -1
  }

  literal(word: string, kind: u8, num: i64): i32 {
    const start = this.pos
    for (let i = 0; i < word.length; i++) {
      if (this.pos >= this.src.length || unchecked(this.src[this.pos]) != <u8>word.charCodeAt(i)) {
        this.fail(D_SYNTAX, start, 'expected ' + word)
        return -1
      }
      this.pos++
    }
    return this.node(kind, num, 0, 0, start)
  }

  object(): i32 {
    const start = this.pos
    if (++this.depth > MAX_DEPTH) {
      this.fail(D_LIMIT, start, 'nesting deeper than ' + MAX_DEPTH.toString() + ' levels')
      return -1
    }
    this.pos++ // {
    // Children are appended to shared arrays, so a nested value would interleave
    // with ours. They are gathered locally and copied in on the way out.
    const out = this.doc.text
    const keysA: Array<i32> = []
    const keysB: Array<i32> = []
    const kids: Array<i32> = []

    this.skipSpace()
    if (this.peek() == 0x7d) {
      this.pos++
      this.depth--
      return this.node(K_OBJECT, 0, this.doc.child.length, 0, start)
    }

    while (true) {
      this.skipSpace()
      if (this.peek() != 0x22) {
        this.fail(D_SYNTAX, this.pos, 'expected a string key')
        return -1
      }
      const keyAt = this.doc.text.len
      const keyLen = this.stringBytes()
      if (!this.diag.ok) return -1

      this.skipSpace()
      if (this.peek() != 0x3a) {
        this.fail(D_SYNTAX, this.pos, 'expected : after the key')
        return -1
      }
      this.pos++
      this.skipSpace()

      const v = this.value()
      if (!this.diag.ok) return -1

      // A duplicate key keeps its last value, as JSON.parse does. Resolving it
      // here rather than in every consumer is what keeps the rule in one place:
      // inference would otherwise see both values and call a changed type a
      // conflict, and the encoder would write whichever it happened to find
      // first. The scan is capped because any object this wide is refused by
      // the 254-field limit before it can be encoded, so its duplicate
      // semantics never reach the wire.
      let replaced = false
      if (kids.length <= DEDUP_SCAN_LIMIT) {
        for (let k = 0; k < kids.length; k++) {
          if (unchecked(keysB[k]) != keyLen) continue
          if (memory.compare(
                out.buf.dataStart + <usize>unchecked(keysA[k]),
                out.buf.dataStart + <usize>keyAt,
                <usize>keyLen) == 0) {
            unchecked((kids[k] = v))
            replaced = true
            break
          }
        }
      }
      if (!replaced) {
        keysA.push(keyAt)
        keysB.push(keyLen)
        kids.push(v)
      }

      this.skipSpace()
      const c = this.peek()
      if (c == 0x2c) {
        this.pos++
        continue
      }
      if (c == 0x7d) {
        this.pos++
        break
      }
      this.fail(D_SYNTAX, this.pos, 'expected , or } in an object')
      return -1
    }

    const at = this.doc.child.length
    for (let i = 0; i < kids.length; i++) {
      this.doc.child.push(unchecked(kids[i]))
      this.doc.keyA.push(unchecked(keysA[i]))
      this.doc.keyB.push(unchecked(keysB[i]))
    }
    this.depth--
    return this.node(K_OBJECT, 0, at, kids.length, start)
  }

  array(): i32 {
    const start = this.pos
    if (++this.depth > MAX_DEPTH) {
      this.fail(D_LIMIT, start, 'nesting deeper than ' + MAX_DEPTH.toString() + ' levels')
      return -1
    }
    this.pos++ // [
    const kids: Array<i32> = []

    this.skipSpace()
    if (this.peek() == 0x5d) {
      this.pos++
      this.depth--
      return this.node(K_ARRAY, 0, this.doc.child.length, 0, start)
    }

    while (true) {
      this.skipSpace()
      const v = this.value()
      if (!this.diag.ok) return -1
      kids.push(v)

      this.skipSpace()
      const c = this.peek()
      if (c == 0x2c) {
        this.pos++
        continue
      }
      if (c == 0x5d) {
        this.pos++
        break
      }
      this.fail(D_SYNTAX, this.pos, 'expected , or ] in an array')
      return -1
    }

    const at = this.doc.child.length
    for (let i = 0; i < kids.length; i++) this.doc.child.push(unchecked(kids[i]))
    // Keys are parallel to child, so array slots need placeholders to keep the
    // two arrays in step for any object that follows.
    for (let i = 0; i < kids.length; i++) {
      this.doc.keyA.push(0)
      this.doc.keyB.push(0)
    }
    this.depth--
    return this.node(K_ARRAY, 0, at, kids.length, start)
  }

  /**
   * Reads a string into the text arena and returns its byte length.
   *
   * Lone surrogates become U+FFFD, which is what encoding/json does; the bytes
   * handed to packed5 are then always valid UTF-8 and the oracle agrees
   * (PLAN.md §4.5).
   */
  stringBytes(): i32 {
    const out = this.doc.text
    const startLen = out.len
    this.pos++ // opening quote

    while (true) {
      if (this.atEnd()) {
        this.fail(D_SYNTAX, this.pos, 'unterminated string')
        return 0
      }
      const c = unchecked(this.src[this.pos])
      if (c == 0x22) {
        this.pos++
        return out.len - startLen
      }
      if (c == 0x5c) {
        this.pos++
        if (!this.escape()) return 0
        continue
      }
      if (c < 0x20) {
        this.fail(D_SYNTAX, this.pos, 'control character in a string must be escaped')
        return 0
      }
      out.writeByte(c)
      this.pos++
    }
  }

  escape(): bool {
    if (this.atEnd()) {
      this.fail(D_SYNTAX, this.pos, 'unterminated escape')
      return false
    }
    const out = this.doc.text
    const c = unchecked(this.src[this.pos])
    this.pos++
    if (c == 0x22) { out.writeByte(0x22); return true }
    if (c == 0x5c) { out.writeByte(0x5c); return true }
    if (c == 0x2f) { out.writeByte(0x2f); return true }
    if (c == 0x62) { out.writeByte(0x08); return true }
    if (c == 0x66) { out.writeByte(0x0c); return true }
    if (c == 0x6e) { out.writeByte(0x0a); return true }
    if (c == 0x72) { out.writeByte(0x0d); return true }
    if (c == 0x74) { out.writeByte(0x09); return true }
    if (c != 0x75) {
      this.fail(D_SYNTAX, this.pos - 1, 'unknown escape')
      return false
    }

    let cp = this.hex4()
    if (!this.diag.ok) return false
    if (cp >= 0xd800 && cp <= 0xdbff) {
      // A high surrogate is only meaningful paired with a low one.
      if (this.pos + 1 < this.src.length && unchecked(this.src[this.pos]) == 0x5c && unchecked(this.src[this.pos + 1]) == 0x75) {
        const save = this.pos
        this.pos += 2
        const lo = this.hex4()
        if (!this.diag.ok) return false
        if (lo >= 0xdc00 && lo <= 0xdfff) {
          cp = 0x10000 + ((cp - 0xd800) << 10) + (lo - 0xdc00)
        } else {
          this.pos = save
          cp = 0xfffd
        }
      } else {
        cp = 0xfffd
      }
    } else if (cp >= 0xdc00 && cp <= 0xdfff) {
      cp = 0xfffd // a lone low surrogate
    }
    writeUTF8(out, cp)
    return true
  }

  hex4(): i32 {
    let v = 0
    for (let i = 0; i < 4; i++) {
      if (this.atEnd()) {
        this.fail(D_SYNTAX, this.pos, 'truncated \\u escape')
        return 0
      }
      const c = unchecked(this.src[this.pos])
      let d = 0
      if (c >= 0x30 && c <= 0x39) d = <i32>c - 0x30
      else if (c >= 0x61 && c <= 0x66) d = <i32>c - 0x61 + 10
      else if (c >= 0x41 && c <= 0x46) d = <i32>c - 0x41 + 10
      else {
        this.fail(D_SYNTAX, this.pos, 'invalid hex digit in a \\u escape')
        return 0
      }
      v = v * 16 + d
      this.pos++
    }
    return v
  }

  /**
   * Parses one number exactly.
   *
   * An integer literal must land in int64 or uint64 with every digit intact; a
   * literal carrying '.' or an exponent is a real number and becomes f64, which
   * must be finite. So 1e30 is a float and 1000000000000000000000000000000 is an
   * error — the notation is what says which is meant, as it does in JSON itself.
   */
  number(): i32 {
    const start = this.pos
    let neg = false
    if (this.peek() == 0x2d) {
      neg = true
      this.pos++
    }

    const intStart = this.pos
    if (this.atEnd()) {
      this.fail(D_SYNTAX, start, 'truncated number')
      return -1
    }
    if (this.peek() == 0x30) {
      this.pos++
    } else if (this.peek() >= 0x31 && this.peek() <= 0x39) {
      while (!this.atEnd() && isDigitByte(this.peek())) this.pos++
    } else {
      this.fail(D_SYNTAX, start, 'expected a digit')
      return -1
    }
    const intEnd = this.pos

    let isFloat = false
    let fracStart = 0
    let fracEnd = 0
    if (!this.atEnd() && this.peek() == 0x2e) {
      isFloat = true
      this.pos++
      fracStart = this.pos
      if (this.atEnd() || !isDigitByte(this.peek())) {
        this.fail(D_SYNTAX, this.pos, 'expected a digit after the decimal point')
        return -1
      }
      while (!this.atEnd() && isDigitByte(this.peek())) this.pos++
      fracEnd = this.pos
    }

    let exp = 0
    if (!this.atEnd() && (this.peek() == 0x65 || this.peek() == 0x45)) {
      isFloat = true
      this.pos++
      let expNeg = false
      if (!this.atEnd() && (this.peek() == 0x2b || this.peek() == 0x2d)) {
        expNeg = this.peek() == 0x2d
        this.pos++
      }
      if (this.atEnd() || !isDigitByte(this.peek())) {
        this.fail(D_SYNTAX, this.pos, 'expected a digit in the exponent')
        return -1
      }
      while (!this.atEnd() && isDigitByte(this.peek())) {
        // Clamped: an exponent past this is infinity or zero either way, and the
        // clamp keeps the accumulator from overflowing on a long digit run.
        if (exp < 100000) exp = exp * 10 + (<i32>this.peek() - 0x30)
        this.pos++
      }
      if (expNeg) exp = -exp
    }

    if (!isFloat) return this.integer(start, intStart, intEnd, neg)

    const v = this.float(start, intStart, intEnd, fracStart, fracEnd, exp, neg)
    if (!this.diag.ok) return -1
    return this.node(K_FLOAT, reinterpret<i64>(v), 0, 0, start)
  }

  integer(start: i32, from: i32, to: i32, neg: bool): i32 {
    let mag: u64 = 0
    for (let i = from; i < to; i++) {
      const d = <u64>(unchecked(this.src[i]) - 0x30)
      // 1844674407370955161 = (2^64-1)/10, so anything above it overflows.
      if (mag > 1844674407370955161 || (mag == 1844674407370955161 && d > 5)) {
        this.fail(D_NUMBER, start, 'integer does not fit in int64 or uint64; write it as a float if that is what it is')
        return -1
      }
      mag = mag * 10 + d
    }
    if (neg) {
      if (mag > <u64>0x8000000000000000) {
        this.fail(D_NUMBER, start, 'integer is below the int64 minimum')
        return -1
      }
      const v: i64 = mag == <u64>0x8000000000000000 ? i64.MIN_VALUE : -(<i64>mag)
      return this.node(K_INT, v, 0, 0, start)
    }
    if (mag <= <u64>0x7fffffffffffffff) return this.node(K_INT, <i64>mag, 0, 0, start)
    return this.node(K_UINT, <i64>mag, 0, 0, start)
  }

  /**
   * Decimal to binary.
   *
   * Two paths. The fast one is the classic exactly-rounded case: a significand
   * under 2^53 and a power of ten under 10^22 are both exactly representable, so
   * one multiply or divide rounds once and is therefore correct. Everything else
   * goes to the exact rational conversion in decimal.ts, because the runtime's
   * parseFloat is not correctly rounded and this module's whole claim is that
   * numbers survive intact.
   */
  float(start: i32, intStart: i32, intEnd: i32, fracStart: i32, fracEnd: i32, exp: i32, neg: bool): f64 {
    // The significand is every digit of the literal, integer and fraction
    // together; the fraction's length moves the exponent. Nothing is discarded
    // here, because a discarded digit is what rounds a hard case the wrong way.
    const fracLen = fracEnd - fracStart
    let count = intEnd - intStart + fracLen
    const digits = new Uint8Array(count > MAX_SIGNIFICAND ? MAX_SIGNIFICAND : count)
    let exp10 = exp - fracLen
    let n = 0
    let sticky = false

    for (let i = intStart; i < intEnd; i++) {
      if (n < MAX_SIGNIFICAND) unchecked((digits[n++] = unchecked(this.src[i])))
      else {
        if (unchecked(this.src[i]) != 0x30) sticky = true
        exp10++
      }
    }
    for (let i = fracStart; i < fracEnd; i++) {
      if (n < MAX_SIGNIFICAND) unchecked((digits[n++] = unchecked(this.src[i])))
      else {
        if (unchecked(this.src[i]) != 0x30) sticky = true
        exp10++
      }
    }

    let out: f64
    if (!sticky && n <= 19 && exp10 >= -22 && exp10 <= 22) {
      let mant: u64 = 0
      for (let i = 0; i < n; i++) mant = mant * 10 + <u64>(unchecked(digits[i]) - 0x30)
      if (mant <= 9007199254740992) {
        const m = <f64>mant
        out = exp10 >= 0 ? m * unchecked(POW10[exp10]) : m / unchecked(POW10[-exp10])
        if (neg) out = -out
        return out
      }
    }

    out = decimalToFloat(digits, n, exp10, sticky)
    if (decStatus == DEC_OVERFLOW || !isFinite(out)) {
      this.fail(D_NUMBER, start, 'number overflows float64; JSON has no infinity and colbin would store it as null')
      return 0
    }
    if (decStatus != DEC_OK) {
      this.fail(D_NUMBER, start, 'number could not be converted exactly')
      return 0
    }
    return neg ? -out : out
  }
}

@inline function isDigitByte(c: u8): bool {
  return c >= 0x30 && c <= 0x39
}

/** UTF-8 encodes one code point. */
export function writeUTF8(out: Writer, cp: i32): void {
  if (cp < 0x80) {
    out.writeByte(<u8>cp)
  } else if (cp < 0x800) {
    out.writeByte(<u8>(0xc0 | (cp >> 6)))
    out.writeByte(<u8>(0x80 | (cp & 0x3f)))
  } else if (cp < 0x10000) {
    out.writeByte(<u8>(0xe0 | (cp >> 12)))
    out.writeByte(<u8>(0x80 | ((cp >> 6) & 0x3f)))
    out.writeByte(<u8>(0x80 | (cp & 0x3f)))
  } else {
    out.writeByte(<u8>(0xf0 | (cp >> 18)))
    out.writeByte(<u8>(0x80 | ((cp >> 12) & 0x3f)))
    out.writeByte(<u8>(0x80 | ((cp >> 6) & 0x3f)))
    out.writeByte(<u8>(0x80 | (cp & 0x3f)))
  }
}

/** Parses src, returning the document or null with diag set. */
export function parseJSON(src: Uint8Array, diag: Diag): Doc | null {
  const p = new Parser(src, diag)
  if (!p.parse()) return null
  return p.doc
}
