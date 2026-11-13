// The column codec (INTERNALS.md §7) and decimal columns (`column/float.go`),
// decode only.
//
//   column := [header:1] [base: 8 bytes]? block*
//   block  := [width:1] [⌈count·width / 8⌉ bytes]
//
// # Two arithmetics
//
// A column is int64 on the wire, and JavaScript has no int64 that is fast. So
// a column is read one of two ways, chosen before any value is touched:
//
//   - NUM: in doubles, when the header, the base and the blocks' widths prove
//     every value is within 2^53 (and for delta, each step checks it). This is
//     every id, count, price in cents and date a table holds.
//   - PAIR: in two uint32 lanes per value with carries, into a buffer that is
//     then viewed as Float64Array (a float column's bit patterns) or as
//     BigInt64Array (an integer column past 2^53). Exact, and still no BigInt
//     arithmetic.
//
// # Unpacking
//
// A block's bytes are copied into an aligned scratch so each residual is one
// or two 32-bit loads, a shift and a mask; the switch on the width happens
// once per block.

import { fail, TRUNCATED, TOO_WIDE } from './fail.ts'

const BLOCK = 128
const TR_RAW = 0
const TR_DELTA = 1
const TR_FOR = 2
const TR_CONSTANT = 3
const ZIGZAG = 4
const HEADER_DECIMAL = 0x08
const TWO32 = 4294967296
const SAFE = Number.MAX_SAFE_INTEGER

const POWERS = [
  1e0, 1e1, 1e2, 1e3, 1e4, 1e5, 1e6, 1e7, 1e8, 1e9, 1e10, 1e11, 1e12, 1e13, 1e14, 1e15, 1e16, 1e17, 1e18,
]

const BAD_HEADER = 'a malformed column'
const BAD_WIDTH = BAD_HEADER
export const OUT_OF_RANGE = TOO_WIDE

// The scratch a block is unpacked from: 16 · 64 bytes at most, and slack for
// the second and third word a straddling value reads.
const scratch8 = new Uint8Array(BLOCK * 8 + 16)
const scratch32 = new Uint32Array(scratch8.buffer)

/** Little-endian platforms only view a pair buffer as Float64Array directly. */
const LITTLE = new Uint8Array(new Uint32Array([1]).buffer)[0] === 1

/** What a decode produced. Exactly one of `num` and `pairs` is set. */
export class Ints {
  /** The values, when every one is a safe integer (or, for a decimal column,
   * the floats themselves). */
  num: Float64Array | null = null
  /** The values as [lo, hi] uint32 lanes, when they are not. */
  pairs: Uint32Array | null = null
  /** Whether `num` holds a decimal column's floats rather than integers. */
  decimal = false
  /** The offset just past the column. */
  next = 0
}

function headerWritten(header: number, n: number): boolean {
  if (n === 0) return header === TR_RAW
  return header === TR_RAW || header === TR_FOR || header === TR_CONSTANT || header === (TR_RAW | ZIGZAG) || header === (TR_DELTA | ZIGZAG)
}

/**
 * Reads `n` int64 values from `b[at..end)`. With `bits` the result is always
 * pairs — what a float column wants, its values being bit patterns.
 */
export function decodeInts(b: Uint8Array, at: number, end: number, n: number, bits: boolean): Ints {
  const out = new Ints()
  if (at >= end) fail(TRUNCATED, at)
  const header = b[at]
  if (header === HEADER_DECIMAL && n > 0) return decodeDecimal(b, at, end, n, bits, out)
  if (!headerWritten(header, n)) fail(BAD_HEADER, at)
  let p = at + 1
  if (n === 0) {
    if (bits) out.pairs = new Uint32Array(0)
    else out.num = new Float64Array(0)
    out.next = p
    return out
  }
  const transform = header & 3
  const zz = (header & ZIGZAG) !== 0

  // The base, zigzagged, as two lanes.
  let bLo = 0
  let bHi = 0
  if (transform !== TR_RAW) {
    if (p + 8 > end) fail(TRUNCATED, p)
    const uLo = (b[p] | (b[p + 1] << 8) | (b[p + 2] << 16) | (b[p + 3] << 24)) >>> 0
    const uHi = (b[p + 4] | (b[p + 5] << 8) | (b[p + 6] << 16) | (b[p + 7] << 24)) >>> 0
    const m = -(uLo & 1)
    bLo = (((uLo >>> 1) | (uHi << 31)) ^ m) >>> 0
    bHi = ((uHi >>> 1) ^ m) >>> 0
    p += 8
  }

  // Walk the block widths once: the widest decides the arithmetic, and the
  // walk is also the truncation check.
  const residuals = transform === TR_DELTA ? n - 1 : transform === TR_CONSTANT ? 0 : n
  let widest = 0
  let q = p
  for (let start = 0; start < residuals; start += BLOCK) {
    if (q >= end) fail(TRUNCATED, q)
    const w = b[q]
    if (w > 64) fail(BAD_WIDTH, q)
    if (w > widest) widest = w
    const count = Math.min(BLOCK, residuals - start)
    q += 1 + ((count * w + 7) >>> 3)
    if (q > end) fail(TRUNCATED, q)
  }
  out.next = q

  // The base as a double, and whether it is a safe one.
  const bHiS = bHi | 0
  const baseSafe = bHiS >= -0x200000 && bHiS < 0x200000 && !(bHiS === -0x200000 && bLo === 0)
  const base = bHiS * TWO32 + bLo

  if (!bits) {
    let num = false
    if (transform === TR_CONSTANT) num = baseSafe
    else if (transform === TR_RAW) num = zz ? widest <= 54 : widest <= 53
    else if (transform === TR_FOR) num = baseSafe && widest <= 53 && base + (2 ** widest - 1) <= SAFE
    else num = baseSafe && widest <= 54
    if (num) {
      const values = new Float64Array(n)
      if (numPass(b, p, values, n, transform, zz, base)) {
        out.num = values
        return out
      }
      // A delta column whose running sum left the safe range: read it again,
      // exactly.
    }
  }
  out.pairs = pairPass(b, p, n, transform, zz, bLo, bHi)
  return out
}

/** The double arithmetic. False when a delta's running sum leaves 2^53. */
function numPass(b: Uint8Array, p: number, out: Float64Array, n: number, transform: number, zz: boolean, base: number): boolean {
  if (transform === TR_CONSTANT) {
    out.fill(base)
    return true
  }
  let offset = 0
  let acc = base
  if (transform === TR_DELTA) {
    out[0] = base
    offset = 1
  }
  const residuals = n - offset
  for (let start = 0; start < residuals; start += BLOCK) {
    const w = b[p]
    const count = Math.min(BLOCK, residuals - start)
    const bytes = (count * w + 7) >>> 3
    const o = offset + start
    p++
    if (w === 0) {
      if (transform === TR_DELTA) {
        for (let i = 0; i < count; i++) out[o + i] = acc
      } else if (transform === TR_FOR) {
        for (let i = 0; i < count; i++) out[o + i] = base
      } else {
        for (let i = 0; i < count; i++) out[o + i] = 0
      }
      continue
    }
    load(b, p, bytes)
    p += bytes
    if (w <= 32) {
      const mask = w === 32 ? -1 : (1 << w) - 1
      let bit = 0
      if (transform === TR_FOR) {
        for (let i = 0; i < count; i++) {
          const k = bit >>> 5
          const s = bit & 31
          let v = scratch32[k] >>> s
          if (s + w > 32) v |= scratch32[k + 1] << (32 - s)
          out[o + i] = base + ((v & mask) >>> 0)
          bit += w
        }
      } else if (transform === TR_DELTA) {
        for (let i = 0; i < count; i++) {
          const k = bit >>> 5
          const s = bit & 31
          let v = scratch32[k] >>> s
          if (s + w > 32) v |= scratch32[k + 1] << (32 - s)
          v = (v & mask) >>> 0
          acc += (v >>> 1) ^ -(v & 1)
          if (acc > SAFE || acc < -SAFE) return false
          out[o + i] = acc
          bit += w
        }
      } else if (zz) {
        for (let i = 0; i < count; i++) {
          const k = bit >>> 5
          const s = bit & 31
          let v = scratch32[k] >>> s
          if (s + w > 32) v |= scratch32[k + 1] << (32 - s)
          v = (v & mask) >>> 0
          out[o + i] = (v >>> 1) ^ -(v & 1)
          bit += w
        }
      } else {
        for (let i = 0; i < count; i++) {
          const k = bit >>> 5
          const s = bit & 31
          let v = scratch32[k] >>> s
          if (s + w > 32) v |= scratch32[k + 1] << (32 - s)
          out[o + i] = (v & mask) >>> 0
          bit += w
        }
      }
      continue
    }
    // 33..54 bits: two lanes, combined exactly since the value is below 2^54
    // and, zigzagged, its magnitude below 2^53.
    const hmask = w >= 64 ? -1 : (1 << (w - 32)) - 1
    let bit = 0
    for (let i = 0; i < count; i++) {
      const k = bit >>> 5
      const s = bit & 31
      let lo: number
      let hi: number
      if (s === 0) {
        lo = scratch32[k]
        hi = scratch32[k + 1]
      } else {
        lo = (scratch32[k] >>> s) | (scratch32[k + 1] << (32 - s))
        hi = (scratch32[k + 1] >>> s) | (scratch32[k + 2] << (32 - s))
      }
      lo >>>= 0
      hi = (hi & hmask) >>> 0
      bit += w
      let value: number
      if (transform === TR_FOR) value = base + hi * TWO32 + lo
      else if (transform === TR_DELTA || zz) {
        const magnitude = hi * 2147483648 + (lo >>> 1)
        const step = (lo & 1) === 1 ? -magnitude - 1 : magnitude
        if (transform === TR_DELTA) {
          acc += step
          if (acc > SAFE || acc < -SAFE) return false
          value = acc
        } else value = step
      } else value = hi * TWO32 + lo
      out[o + i] = value
    }
  }
  return true
}

/** The exact arithmetic: wrapping int64 in two lanes. */
function pairPass(b: Uint8Array, p: number, n: number, transform: number, zz: boolean, bLo: number, bHi: number): Uint32Array {
  const out = new Uint32Array(2 * n)
  if (transform === TR_CONSTANT) {
    for (let i = 0; i < n; i++) {
      out[2 * i] = bLo
      out[2 * i + 1] = bHi
    }
    return out
  }
  let offset = 0
  let aLo = bLo
  let aHi = bHi
  if (transform === TR_DELTA) {
    out[0] = bLo
    out[1] = bHi
    offset = 1
  }
  const residuals = n - offset
  for (let start = 0; start < residuals; start += BLOCK) {
    const w = b[p]
    const count = Math.min(BLOCK, residuals - start)
    const bytes = (count * w + 7) >>> 3
    p++
    load(b, p, bytes)
    p += bytes
    const mask = w >= 32 ? -1 : (1 << w) - 1
    const hmask = w >= 64 ? -1 : w > 32 ? (1 << (w - 32)) - 1 : 0
    let bit = 0
    for (let i = 0; i < count; i++) {
      let lo = 0
      let hi = 0
      if (w !== 0) {
        const k = bit >>> 5
        const s = bit & 31
        if (s === 0) {
          lo = scratch32[k]
          hi = scratch32[k + 1]
        } else {
          lo = (scratch32[k] >>> s) | (scratch32[k + 1] << (32 - s))
          hi = (scratch32[k + 1] >>> s) | (scratch32[k + 2] << (32 - s))
        }
        lo = (lo & mask) >>> 0
        hi = (hi & hmask) >>> 0
        bit += w
      }
      const at = 2 * (offset + start + i)
      if (transform === TR_FOR) {
        const sum = bLo + lo
        out[at] = sum >>> 0
        out[at + 1] = (bHi + hi + (sum >= TWO32 ? 1 : 0)) >>> 0
      } else if (transform === TR_DELTA || zz) {
        const m = -(lo & 1)
        const sLo = (((lo >>> 1) | (hi << 31)) ^ m) >>> 0
        const sHi = ((hi >>> 1) ^ m) >>> 0
        if (transform === TR_DELTA) {
          const sum = aLo + sLo
          aLo = sum >>> 0
          aHi = (aHi + sHi + (sum >= TWO32 ? 1 : 0)) >>> 0
          out[at] = aLo
          out[at + 1] = aHi
        } else {
          out[at] = sLo
          out[at + 1] = sHi
        }
      } else {
        out[at] = lo
        out[at + 1] = hi
      }
    }
  }
  return out
}

/** Copies a block's bytes into the aligned scratch, zeroing the words a
 * straddling read looks at past them. */
function load(b: Uint8Array, p: number, bytes: number): void {
  if (bytes > 64) scratch8.set(b.subarray(p, p + bytes))
  else for (let i = 0; i < bytes; i++) scratch8[i] = b[p + i]
  // Up to three words past the last value's first one.
  const tail = bytes & ~3
  scratch32[tail >>> 2] &= bytes & 3 ? (1 << ((bytes & 3) * 8)) - 1 : 0
  scratch32[(tail >>> 2) + 1] = 0
  scratch32[(tail >>> 2) + 2] = 0
}

/**
 * A decimal column: `[0x08] [e] [exceptions:u32] [integers] ([positions] [bits])`,
 * each value `integer / 10^e` exactly, the exceptions' bit patterns in place.
 */
function decodeDecimal(b: Uint8Array, at: number, end: number, n: number, bits: boolean, out: Ints): Ints {
  if (end - at < 6) fail(TRUNCATED, at)
  const exponent = b[at + 1]
  const exceptions = (b[at + 2] | (b[at + 3] << 8) | (b[at + 4] << 16) | (b[at + 5] << 24)) >>> 0
  if (exponent > 18 || exceptions > n) fail(BAD_HEADER, at)
  let p = at + 6
  if (p < end && b[p] === HEADER_DECIMAL) fail(BAD_HEADER, p)
  const ints = decodeInts(b, p, end, n, false)
  p = ints.next
  const values = new Float64Array(n)
  const power = POWERS[exponent]
  if (ints.num === null) fail(OUT_OF_RANGE, at)
  const src = ints.num
  for (let i = 0; i < n; i++) {
    const v = src[i]
    // Each integer is in [−2^51, 2^51).
    if (v < -2251799813685248 || v >= 2251799813685248) fail(OUT_OF_RANGE, at)
    values[i] = v / power
  }
  if (exceptions > 0) {
    if (p < end && b[p] === HEADER_DECIMAL) fail(BAD_HEADER, p)
    const positions = decodeInts(b, p, end, exceptions, false)
    p = positions.next
    if (p < end && b[p] === HEADER_DECIMAL) fail(BAD_HEADER, p)
    const patterns = decodeInts(b, p, end, exceptions, true)
    p = patterns.next
    const where = positions.num
    if (where === null) fail(BAD_HEADER, at)
    const floats = pairsToFloat64(patterns.pairs as Uint32Array)
    let last = -1
    for (let k = 0; k < exceptions; k++) {
      const row = where[k]
      if (row <= last || row >= n) fail(BAD_HEADER, at)
      values[row] = floats[k]
      last = row
    }
  }
  out.next = p
  if (bits) {
    // A reader asking for bit patterns gets the floats' own.
    out.pairs = floatsToPairs(values)
    return out
  }
  out.num = values
  out.decimal = true
  return out
}

/** Lane pairs in the order a 64-bit view reads them: as they are on a
 * little-endian platform, each pair's two words swapped on a big-endian one. */
function swapped(pairs: Uint32Array): Uint32Array {
  if (LITTLE) return pairs
  const out = new Uint32Array(pairs.length)
  for (let i = 0; i < pairs.length; i += 2) {
    out[i] = pairs[i + 1]
    out[i + 1] = pairs[i]
  }
  return out
}

function floatsToPairs(values: Float64Array): Uint32Array {
  return swapped(new Uint32Array(values.buffer, values.byteOffset, values.length * 2))
}

/** A column of float64 bit patterns as the floats. */
export function pairsToFloat64(pairs: Uint32Array): Float64Array {
  const words = swapped(pairs)
  return new Float64Array(words.buffer, words.byteOffset, words.length >>> 1)
}

/** A column of int64 lanes as BigInt64Array (signed) or BigUint64Array. */
export function pairsToBig(pairs: Uint32Array, signed: boolean): BigInt64Array | BigUint64Array {
  const words = swapped(pairs)
  const n = words.length >>> 1
  return signed ? new BigInt64Array(words.buffer, words.byteOffset, n) : new BigUint64Array(words.buffer, words.byteOffset, n)
}

/** Whether every lane pair is a safe integer, read signed or unsigned. */
export function pairsSafe(pairs: Uint32Array, signed: boolean): boolean {
  for (let i = 1; i < pairs.length; i += 2) {
    const hi = pairs[i]
    if (signed) {
      const h = hi | 0
      if (h >= 0x200000 || h < -0x200000 || (h === -0x200000 && pairs[i - 1] === 0)) return false
    } else if (hi >= 0x200000) return false
  }
  return true
}

/** Safe lane pairs as doubles. */
export function pairsToNum(pairs: Uint32Array, signed: boolean): Float64Array {
  const n = pairs.length >>> 1
  const out = new Float64Array(n)
  for (let i = 0; i < n; i++) {
    const hi = signed ? pairs[2 * i + 1] | 0 : pairs[2 * i + 1]
    out[i] = hi * TWO32 + pairs[2 * i]
  }
  return out
}
