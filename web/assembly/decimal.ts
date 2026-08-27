// Correctly-rounded decimal to binary conversion.
//
// AssemblyScript's parseFloat is not correctly rounded: for
// 2.2250738585072011e-308 it returns the smallest normal where Go, JavaScript
// and IEEE-754 all give the largest subnormal — one ULP out, at the boundary
// that has broken parsers before. "Numbers are exact" is this module's central
// claim (PLAN.md §2.1), so the conversion is done here instead.
//
// The method is exact by construction rather than approximate-then-check: the
// literal is held as a rational num/den of arbitrary precision, the binary
// exponent E of the result is derived from bit lengths, and the significand is
// the correctly rounded quotient round(num / (den * 2^E)). Nothing is a table
// lookup, so there is no precomputed power-of-five table to carry, and nothing
// is a floating-point operation, so there is nothing to round twice.
//
// Speed is not a goal: the fast path in json.ts already covers the ordinary
// case (a significand under 2^53 and |exp| <= 22, where a single multiply or
// divide is exactly rounded). This runs only for the rest.

/** A natural number, little-endian u32 limbs, no trailing zero limbs. */
type Nat = Array<u32>

@inline function natIsZero(a: Nat): bool {
  return a.length == 0
}

function natTrim(a: Nat): void {
  while (a.length > 0 && unchecked(a[a.length - 1]) == 0) a.pop()
}

function natFromU64(v: u64): Nat {
  const a: Nat = []
  if (v == 0) return a
  a.push(<u32>v)
  if (v > 0xffffffff) a.push(<u32>(v >> 32))
  natTrim(a)
  return a
}

/** a = a*m + add */
function natMulAdd(a: Nat, m: u32, add: u32): void {
  let carry: u64 = <u64>add
  for (let i = 0; i < a.length; i++) {
    const cur = <u64>unchecked(a[i]) * <u64>m + carry
    unchecked((a[i] = <u32>cur))
    carry = cur >> 32
  }
  while (carry > 0) {
    a.push(<u32>carry)
    carry >>= 32
  }
}

/** 5^13 is the largest power of five below 2^32. */
const POW5_13: u32 = 1220703125

function natMulPow5(a: Nat, k: i32): void {
  if (natIsZero(a)) return
  while (k >= 13) {
    natMulAdd(a, POW5_13, 0)
    k -= 13
  }
  let m: u32 = 1
  for (let i = 0; i < k; i++) m *= 5
  if (m != 1) natMulAdd(a, m, 0)
}

function natShl(a: Nat, bits: i32): void {
  if (natIsZero(a) || bits <= 0) return
  const limbs = bits >> 5
  const rem = bits & 31
  if (rem != 0) {
    let carry: u32 = 0
    for (let i = 0; i < a.length; i++) {
      const v = unchecked(a[i])
      unchecked((a[i] = (v << rem) | carry))
      carry = <u32>(<u64>v >> (32 - rem))
    }
    if (carry != 0) a.push(carry)
  }
  if (limbs != 0) {
    for (let i = 0; i < limbs; i++) a.push(0)
    for (let i = a.length - 1; i >= limbs; i--) {
      unchecked((a[i] = unchecked(a[i - limbs])))
    }
    for (let i = 0; i < limbs; i++) unchecked((a[i] = 0))
  }
  natTrim(a)
}

/** a >>= 1 */
function natShr1(a: Nat): void {
  let carry: u32 = 0
  for (let i = a.length - 1; i >= 0; i--) {
    const v = unchecked(a[i])
    unchecked((a[i] = (v >> 1) | (carry << 31)))
    carry = v & 1
  }
  natTrim(a)
}

function natBitLen(a: Nat): i32 {
  if (natIsZero(a)) return 0
  const top = unchecked(a[a.length - 1])
  return (a.length - 1) * 32 + (32 - <i32>clz(top))
}

function natCmp(a: Nat, b: Nat): i32 {
  if (a.length != b.length) return a.length < b.length ? -1 : 1
  for (let i = a.length - 1; i >= 0; i--) {
    const x = unchecked(a[i])
    const y = unchecked(b[i])
    if (x != y) return x < y ? -1 : 1
  }
  return 0
}

/** a -= b, requiring a >= b. */
function natSub(a: Nat, b: Nat): void {
  let borrow: u64 = 0
  for (let i = 0; i < a.length; i++) {
    const bv = i < b.length ? <u64>unchecked(b[i]) : 0
    const cur = <u64>unchecked(a[i]) - bv - borrow
    unchecked((a[i] = <u32>cur))
    borrow = (cur >> 63) & 1
  }
  natTrim(a)
}

function natCopy(a: Nat): Nat {
  const out: Nat = []
  for (let i = 0; i < a.length; i++) out.push(unchecked(a[i]))
  return out
}

// divide()'s results, as module state: AssemblyScript has no tuples and this is
// called at most three times per number.
let divQuotient: u64 = 0
let divRemainder: Nat = []

/**
 * Long division, quotient in divQuotient and remainder in divRemainder.
 *
 * The caller has scaled num and den so the quotient is about 53 bits, so the
 * loop runs about as many times and the quotient fits a u64. Anything wider
 * would be a scaling bug, and returns false rather than silently truncating.
 */
function divide(num: Nat, den: Nat): bool {
  divQuotient = 0
  const shift = natBitLen(num) - natBitLen(den)
  if (shift < 0) {
    divRemainder = natCopy(num)
    return true
  }
  if (shift > 62) return false

  const rem = natCopy(num)
  const d = natCopy(den)
  natShl(d, shift)
  let q: u64 = 0
  for (let i = shift; i >= 0; i--) {
    if (natCmp(rem, d) >= 0) {
      natSub(rem, d)
      q |= (<u64>1) << i
    }
    natShr1(d)
  }
  divQuotient = q
  divRemainder = rem
  return true
}

const F64_MANT_BITS: i32 = 53
const F64_MIN_EXP: i32 = -1074 // the exponent of the smallest subnormal
const F64_MAX_EXP: i32 = 971 // 2^53 * 2^971 is just past the largest finite

export const DEC_OK: i32 = 0
export const DEC_OVERFLOW: i32 = 1
export const DEC_INTERNAL: i32 = 2

export let decStatus: i32 = DEC_OK

/**
 * The f64 nearest to mantDigits * 10^exp10, correctly rounded, ties to even.
 *
 * digits is the significand as decimal digit bytes ('0'..'9'); exp10 is the
 * power of ten it is multiplied by. Both come from the scanner, which has
 * already stripped the sign.
 *
 * sticky says digits were dropped past the end, so the true value is a little
 * larger than what is here. That can only change an exact tie, which it breaks
 * upwards: the dropped remainder is many orders below half an ULP, so it cannot
 * move a value that was not already exactly on the midpoint.
 */
export function decimalToFloat(digits: Uint8Array, digitCount: i32, exp10: i32, sticky: bool): f64 {
  decStatus = DEC_OK

  // The significand, at full precision. Nothing is dropped here: dropping a
  // digit is what makes a hard rounding case round the wrong way.
  const m: Nat = []
  let allZero = true
  for (let i = 0; i < digitCount; i++) {
    const d = unchecked(digits[i]) - 0x30
    if (d != 0) allZero = false
    natMulAdd(m, 10, <u32>d)
  }
  if (allZero || natIsZero(m)) return 0

  // value = num / den, exactly.
  let num = m
  let den = natFromU64(1)
  if (exp10 > 0) {
    natMulPow5(num, exp10)
    natShl(num, exp10)
  } else if (exp10 < 0) {
    natMulPow5(den, -exp10)
    natShl(den, -exp10)
  }

  // floor(log2(value)), to within one, from the bit lengths.
  let k = natBitLen(num) - natBitLen(den)

  // Cheap rejection before doing any work on a value that cannot be finite.
  if (k > F64_MAX_EXP + F64_MANT_BITS + 2) {
    decStatus = DEC_OVERFLOW
    return 0
  }
  if (k < F64_MIN_EXP - F64_MANT_BITS - 2) return 0

  // E is the exponent of the unit in the last place. Normals carry 53
  // significand bits; a subnormal carries whatever fits above 2^-1074, which is
  // why the precision is clamped rather than the result rounded twice.
  let E = k - F64_MANT_BITS
  if (E < F64_MIN_EXP) E = F64_MIN_EXP

  // Two adjustments at most: the estimate of k is off by at most one, and a
  // round-up can carry into an extra bit.
  for (let attempt = 0; attempt < 4; attempt++) {
    const n = natCopy(num)
    const d = natCopy(den)
    if (E >= 0) natShl(d, E)
    else natShl(n, -E)

    if (!divide(n, d)) {
      decStatus = DEC_INTERNAL
      return 0
    }
    let q = divQuotient
    const rem = divRemainder

    // Round half to even against the remainder, exactly: 2*rem versus den.
    const twice = natCopy(rem)
    natShl(twice, 1)
    let cmp = natCmp(twice, d)
    if (cmp == 0 && sticky) cmp = 1
    if (cmp > 0 || (cmp == 0 && (q & 1) == 1)) q++

    const bits = 64 - <i32>clz(q)
    if (bits > F64_MANT_BITS) {
      // Too much precision, or a carry out of the top: retry one place coarser.
      E++
      continue
    }
    if (bits < F64_MANT_BITS && E > F64_MIN_EXP) {
      // Too little: retry one place finer, unless already at the subnormal floor.
      E--
      continue
    }
    return ldexp(<f64>q, E)
  }

  decStatus = DEC_INTERNAL
  return 0
}

/**
 * q * 2^E.
 *
 * One multiply whenever the scale is a normal number. Below 2^-1022 it takes
 * two, ordered so the first is exact — q is an integer under 2^53, so shifting
 * it by a small negative power of two loses nothing — and only the second
 * rounds, which is what makes a subnormal result correctly rounded rather than
 * rounded twice.
 */
function ldexp(q: f64, e: i32): f64 {
  if (e >= -1022) return q * scaleOf(e)
  return (q * scaleOf(e + 1022)) * scaleOf(-1022)
}

/** 2^e, for -1022 <= e <= 1023, built from the exponent field directly. */
function scaleOf(e: i32): f64 {
  return reinterpret<f64>((<u64>(e + 1023)) << 52)
}
