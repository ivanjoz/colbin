// Port of github.com/ivanjoz/colbin/varint.
//
// Structural, not a paraphrase: the candidate order in appendArray and the
// iteration order in search decide ties, and a tie broken differently produces a
// different — still valid, still decodable — frame that would not match the Go
// vectors. Every strict `<` below is strict in the original for that reason.
//
// Go derives the element width from the type parameter. Here it is an argument,
// in BITS (8, 16, 32, 64) — the same unit colbin's appendIntColumn uses, so the
// column layer above can pass fm.bitWidth straight through. Go's varint counts
// bytes internally; byteWidth below is that conversion, kept in one place
// because mixing the two units silently disables the fixed-width fallback and
// only shows up on full-width random data.

import { Reader, Writer } from './bytes'

export const TR_RAW: u8 = 0
export const TR_DELTA: u8 = 1
export const TR_FOR: u8 = 2
export const TR_FIXED: u8 = 3

const MIN_K: u8 = 1
const MAX_K: u8 = 4

// d = M - k. Code 7 is 8 rather than 7 so k=1 can still reach M=9, which
// full-width 64-bit columns need.
const D_CODES: StaticArray<u8> = [0, 1, 2, 3, 4, 5, 6, 8]

@inline export function zigzag(v: i64): u64 {
  return <u64>((v << 1) ^ (v >> 63))
}

@inline export function unzigzag(u: u64): i64 {
  return <i64>(u >> 1) ^ -(<i64>(u & 1))
}

@inline function bitLen(v: u64): u8 {
  return <u8>(64 - <i32>clz(v))
}

@inline function enc(x: i64, zz: bool): u64 {
  return zz ? zigzag(x) : <u64>x
}

@inline function dec(u: u64, zz: bool): i64 {
  return zz ? unzigzag(u) : <i64>u
}

/** Payload bits available to a value occupying exactly l bytes under (k, m). */
@inline function capBits(l: u8, k: u8, m: u8): i32 {
  if (l == m) return 7 * <i32>m + <i32>k
  return 8 * (<i32>k - 1) + 7 * (<i32>l - <i32>k + 1)
}

/** Byte length of an nbits value under (k, m), or 0 if it does not fit. */
function encLen(nbits: u8, k: u8, m: u8): u8 {
  for (let l: u8 = k; l <= m; l++) {
    if (<i32>nbits <= capBits(l, k, m)) return l
  }
  return 0
}

function appendKM(w: Writer, v: u64, k: u8, m: u8): void {
  const l = encLen(bitLen(v), k, m)
  for (let i: u8 = 1; i <= l; i++) {
    if (i < k || i == m) {
      // Flag-free: a forced continuation, or the forced terminal byte.
      w.writeByte(<u8>v)
      v >>= 8
      continue
    }
    let b = <u8>(v & 0x7f)
    v >>= 7
    if (i < l) b |= 0x80
    w.writeByte(b)
  }
}

/**
 * Reads one value written by appendKM. On truncation r.err is set.
 *
 * Go writes this as an unbounded loop that leaves by one of two returns. Here it
 * is bounded by m, which is exactly equivalent — byte m is the declared maximum
 * and returns unconditionally — and makes termination a property of the loop
 * rather than of the data, which is what a decoder handed a corrupt buffer wants.
 */
function getKM(r: Reader, k: u8, m: u8): u64 {
  let v: u64 = 0
  let shift: u8 = 0
  for (let i: u8 = 1; i <= m; i++) {
    const b = r.readByte()
    if (!r.ok) return 0
    if (i < k) {
      // Guaranteed continuation: the whole byte is payload.
      v |= (<u64>b) << shift
      shift += 8
    } else if (i == m) {
      // Guaranteed terminal: the whole byte is payload.
      return v | ((<u64>b) << shift)
    } else {
      v |= (<u64>(b & 0x7f)) << shift
      shift += 7
      if ((b & 0x80) == 0) return v
    }
  }
  // Unreachable: i == m returns above. Present because the bound is now explicit.
  r.fail(2)
  return 0
}

/** A candidate (k, code) pair and the payload size it produces. */
class KM {
  k: u8 = 0
  code: u8 = 0
  m: u8 = 0
  size: i32 = 0
  found: bool = false
}

/** A fully evaluated encoding candidate. */
class Plan {
  transform: u8 = 0
  zz: bool = false
  base: i64 = 0
  km: KM = new KM()
  total: i32 = 0
  usable: bool = false
}

// Scratch shared by every evaluate() call. The histogram is rewritten each time
// and never read across calls.
const hist = new StaticArray<i32>(65)
const cumulative = new StaticArray<i32>(65)

/**
 * Picks the (k, code) pair minimising payload size for the residual bit-length
 * histogram, rejecting pairs that cannot represent the widest residual.
 *
 * Iteration is k ascending then code ascending, and the comparison is strictly
 * less-than, so the lowest (k, code) wins a tie. Go does the same and the frames
 * would differ if it did not.
 */
function search(): KM {
  let total: i32 = 0
  for (let b = 0; b < 65; b++) {
    total += unchecked(hist[b])
    unchecked((cumulative[b] = total))
  }

  const best = new KM()
  for (let k = MIN_K; k <= MAX_K; k++) {
    for (let code: u8 = 0; code < 8; code++) {
      const m = k + unchecked(D_CODES[code])
      const maxBits = capBits(m, k, m)
      if (maxBits < 64 && unchecked(cumulative[maxBits]) != total) continue

      let size = 0
      let previousCap = -1
      for (let l = k; l <= m; l++) {
        const hi = <i32>min(capBits(l, k, m), 64)
        let n = unchecked(cumulative[hi])
        if (previousCap >= 0) n -= unchecked(cumulative[previousCap])
        size += <i32>l * n
        previousCap = hi
        if (hi == 64) break
      }
      if (!best.found || size < best.size) {
        best.k = k
        best.code = code
        best.m = m
        best.size = size
        best.found = true
      }
    }
  }
  return best
}

@inline function residualCount(transform: u8, n: i32): i32 {
  return transform == TR_FOR ? n + 1 : n
}

/**
 * Residual i for the given transform. A FOR base is always zigzagged since it
 * alone may be negative; FOR's own offsets are non-negative by construction and
 * are never zigzagged whatever zz says.
 */
@inline function residual(vals: Int64Array, i: i32, transform: u8, zz: bool, base: i64): u64 {
  if (transform == TR_FOR) {
    if (i == 0) return zigzag(base)
    // Unsigned subtraction, which wraps correctly for spans above 2^63.
    return (<u64>unchecked(vals[i - 1])) - (<u64>base)
  }
  if (transform == TR_DELTA) {
    if (i == 0) return enc(unchecked(vals[0]), zz)
    return enc(unchecked(vals[i]) - unchecked(vals[i - 1]), zz)
  }
  return enc(unchecked(vals[i]), zz)
}

@inline function subOverflows(a: i64, b: i64): bool {
  const d = a - b
  return ((a ^ b) & (a ^ d)) < 0
}

/**
 * Scores one transform. Unusable only when a delta overflows int64, which needs
 * a span above 2^63 and so is reachable only for 64-bit input.
 */
function evaluate(vals: Int64Array, transform: u8, zz: bool, base: i64, bitWidth: u8): Plan {
  const p = new Plan()
  const n = vals.length
  if (transform == TR_DELTA && bitWidth == 64) {
    for (let i = 1; i < n; i++) {
      if (subOverflows(unchecked(vals[i]), unchecked(vals[i - 1]))) return p
    }
  }

  for (let b = 0; b < 65; b++) unchecked((hist[b] = 0))
  const count = residualCount(transform, n)
  for (let i = 0; i < count; i++) {
    unchecked(hist[bitLen(residual(vals, i, transform, zz, base))]++)
  }

  const km = search()
  if (!km.found) return p

  p.transform = transform
  p.zz = zz
  p.base = base
  p.km = km
  p.total = 1 + km.size
  p.usable = true
  return p
}

/** Encodes vals onto w. bitWidth is the element width in bits: 8, 16, 32 or 64. */
export function appendArray(w: Writer, vals: Int64Array, bitWidth: u8): void {
  const n = vals.length
  const byteWidth = <i32>bitWidth >> 3
  if (n == 0) {
    w.writeByte(TR_RAW) // k=1, code=0, no payload
    return
  }

  // Candidate 1: raw values, zigzagged only if the column has negatives.
  let hasNeg = false
  let minVal = unchecked(vals[0])
  for (let i = 0; i < n; i++) {
    const x = unchecked(vals[i])
    if (x < 0) hasNeg = true
    if (x < minVal) minVal = x
  }
  let best = evaluate(vals, TR_RAW, hasNeg, 0, bitWidth)

  // Candidate 2: delta of previous. Zigzag whenever any step goes backwards.
  let negStep = false
  for (let i = 1; i < n; i++) {
    const a = unchecked(vals[i])
    const b = unchecked(vals[i - 1])
    if (!subOverflows(a, b) && a < b) {
      negStep = true
      break
    }
  }
  const delta = evaluate(vals, TR_DELTA, negStep, 0, bitWidth)
  if (delta.usable && (!best.usable || delta.total < best.total)) best = delta

  // Candidate 3: frame of reference against the column minimum.
  const forPlan = evaluate(vals, TR_FOR, false, minVal, bitWidth)
  if (forPlan.usable && (!best.usable || forPlan.total < best.total)) best = forPlan

  // Candidate 4: uncompressed native words. Always available, and what bounds
  // the output at 1 + width*n.
  const fixedTotal = 1 + n * byteWidth
  if (!best.usable || fixedTotal < best.total) {
    best = new Plan()
    best.transform = TR_FIXED
    best.km.k = 1
    best.km.code = 0
    best.total = fixedTotal
    best.usable = true
  }

  const zzBit: u8 = best.zz ? <u8>(1 << 2) : 0
  w.writeByte(best.transform | zzBit | ((best.km.k - 1) << 3) | (best.km.code << 5))

  if (best.transform == TR_FIXED) {
    for (let i = 0; i < n; i++) {
      let u = <u64>unchecked(vals[i])
      for (let b = 0; b < byteWidth; b++) {
        w.writeByte(<u8>u)
        u >>= 8
      }
    }
    return
  }

  const count = residualCount(best.transform, n)
  for (let i = 0; i < count; i++) {
    appendKM(w, residual(vals, i, best.transform, best.zz, best.base), best.km.k, best.km.m)
  }
}

@inline function signExtend(v: u64, bitWidth: u8): i64 {
  if (bitWidth < 64 && (v & ((<u64>1) << (bitWidth - 1))) != 0) {
    v |= ~(((<u64>1) << bitWidth) - 1)
  }
  return <i64>v
}

/**
 * Reads n values into out. Returns true on success; on failure r.err says why
 * and out is not to be trusted.
 */
export function decodeArray(r: Reader, n: i32, out: Int64Array, bitWidth: u8): bool {
  const byteWidth = <i32>bitWidth >> 3
  if (n < 0 || out.length < n) {
    r.fail(2) // ERR_CORRUPT
    return false
  }
  const hdr = r.readByte()
  if (!r.ok) return false

  const transform = hdr & 0x03
  const zz = (hdr & 0x04) != 0
  const k = ((hdr >> 3) & 0x03) + 1
  const m = k + unchecked(D_CODES[hdr >> 5])
  if (n == 0) return true

  if (transform == TR_FIXED) {
    if (!r.has(n * byteWidth)) return false
    for (let i = 0; i < n; i++) {
      let u: u64 = 0
      for (let b = 0; b < byteWidth; b++) {
        u |= (<u64>r.readByte()) << (8 * b)
      }
      unchecked((out[i] = signExtend(u, bitWidth)))
    }
    return r.ok
  }

  if (transform == TR_FOR) {
    const base = unzigzag(getKM(r, k, m))
    if (!r.ok) return false
    for (let i = 0; i < n; i++) {
      const d = getKM(r, k, m)
      if (!r.ok) return false
      unchecked((out[i] = <i64>((<u64>base) + d))) // wraps to match the encoder
    }
    return true
  }

  if (transform == TR_DELTA) {
    let prev: i64 = 0
    for (let i = 0; i < n; i++) {
      const u = getKM(r, k, m)
      if (!r.ok) return false
      if (i == 0) prev = dec(u, zz)
      else prev += dec(u, zz)
      unchecked((out[i] = prev))
    }
    return true
  }

  for (let i = 0; i < n; i++) {
    const u = getKM(r, k, m)
    if (!r.ok) return false
    unchecked((out[i] = dec(u, zz)))
  }
  return true
}

/**
 * Test hook: total size of one candidate for vals.
 * which: 0 raw, 1 delta, 2 for, 3 fixed, 4 = bitLen of residual 0 under raw,
 * 5 = histogram total under raw. Returns -1 when the candidate is unusable.
 */
export function debugTotals(vals: Int64Array, bitWidth: u8, which: i32): i32 {
  const n = vals.length
  let hasNeg = false
  let minVal = n > 0 ? unchecked(vals[0]) : 0
  for (let i = 0; i < n; i++) {
    const x = unchecked(vals[i])
    if (x < 0) hasNeg = true
    if (x < minVal) minVal = x
  }
  let negStep = false
  for (let i = 1; i < n; i++) {
    const a = unchecked(vals[i])
    const b = unchecked(vals[i - 1])
    if (!subOverflows(a, b) && a < b) {
      negStep = true
      break
    }
  }
  if (which == 3) return 1 + n * (<i32>bitWidth >> 3)
  if (which == 4) return <i32>bitLen(residual(vals, 0, TR_RAW, hasNeg, 0))
  if (which == 5) {
    let sum = 0
    for (let i = 0; i < n; i++) sum += <i32>bitLen(residual(vals, i, TR_RAW, hasNeg, 0))
    return sum
  }
  const transform: u8 = which == 0 ? TR_RAW : which == 1 ? TR_DELTA : TR_FOR
  const zz = which == 0 ? hasNeg : which == 1 ? negStep : false
  const base = which == 2 ? minVal : 0
  const p = evaluate(vals, transform, zz, base, bitWidth)
  return p.usable ? p.total : -1
}
