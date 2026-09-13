// Port of github.com/ivanjoz/colbin/column.
//
// A transform, then blocks of 128 residuals packed at an exact bit width chosen
// per block. 128 values at w bits occupy 16w bytes — a whole number of bytes for
// every w — so a block is byte-aligned at both ends, no state crosses a boundary
// and the width leaves the element loop.
//
// This replaces varint.ts, and it is a smaller thing to port: the old codec
// searched a (k, M) parameter space per column and broke ties in an order that
// had to be reproduced exactly. Here the encoder scores four transforms against
// what each would actually occupy in blocks and takes the cheapest, so the only
// tie-break is the order the four are tried in — which is preserved below, since
// a tie broken the other way produces a valid frame that is not the Go one.
//
// Go derives the element width from a type parameter and never puts it on the
// wire. Here it is an argument, in BYTES (1, 2, 4, 8), and the caller has to
// pass the same one the encoder used — which the schema section is what says.

import { gather8, Reader, Writer } from './bytes'

export const TR_RAW: u8 = 0 // residual[i] = enc(vals[i])
export const TR_DELTA: u8 = 1 // base = vals[0]; residual[i] = zigzag(v[i+1]-v[i])
export const TR_FOR: u8 = 2 // base = min; residual[i] = uint64(v[i]) - uint64(min)
export const TR_CONSTANT: u8 = 3 // no blocks at all: every value is the base

export const ZIGZAG_FLAG: u8 = 1 << 2

/** How many residuals share one width byte. */
const BLOCK_SIZE: i32 = 128

/**
 * The largest width the one-load read covers. A value that starts anywhere in a
 * byte and is at most 57 bits wide always finishes inside the eight bytes that
 * load spans; past it the read needs a second byte.
 */
const WIDE_WIDTH: i32 = 57

/** What a constant column's payload occupies: the one value, in the clear. */
const CONSTANT_COST: i32 = 8

@inline
function zigzag(v: i64): u64 {
  return (<u64>(v << 1)) ^ (<u64>(v >> 63))
}

@inline
function unzigzag(u: u64): i64 {
  return <i64>(u >> 1) ^ -(<i64>(u & 1))
}

/** What a run of count residuals occupies at w bits. */
@inline
function blockBytes(count: i32, w: i32): i32 {
  return (count * w + 7) >> 3
}

/**
 * The width a set of residuals needs: the widest one's bit length, with no
 * rounding. OR-ing them together and taking one bit length is the same answer as
 * a running maximum, without the compare.
 */
@inline
function widthOfSet(set: u64): i32 {
  return 64 - <i32>clz(set)
}

/** Truncation to the element width, which is what Go's T(x) conversion does. */
@inline
function truncate(value: i64, width: i32): i64 {
  if (width == 1) return <i64><i8>value
  if (width == 2) return <i64><i16>value
  if (width == 4) return <i64><i32>value
  return value
}

@inline
function encodeOne(x: i64, zz: bool): u64 {
  return zz ? zigzag(x) : <u64>x
}

@inline
function decodeOne(u: u64, zz: bool): i64 {
  return zz ? unzigzag(u) : <i64>u
}

/** Whether a - b overflows i64. */
@inline
function subOverflows(a: i64, b: i64): bool {
  const difference = a - b
  return ((a ^ b) & (a ^ difference)) < 0
}

/** How many residuals a transform emits: delta writes its first value as a base. */
@inline
function residualsUnder(transform: u8, n: i32): i32 {
  return transform == TR_DELTA ? n - 1 : n
}

// ---- packing ----------------------------------------------------------------

/** Scratch for one block, so neither direction allocates per block. */
const scratch = new StaticArray<u64>(BLOCK_SIZE)

/**
 * Writes count residuals at w bits each, least significant bit first, with no
 * gap between them and none at the end beyond the final partial byte.
 *
 * A full block is 2w whole 64-bit words, so the accumulator flushes exactly and
 * only the last block of a column can end mid-word.
 */
function packRun(out: Writer, count: i32, w: i32): void {
  if (w == 0) return
  let accumulator: u64 = 0
  let accumulated = 0
  for (let index = 0; index < count; index++) {
    const residual = unchecked(scratch[index])
    if (accumulated + w < 64) {
      accumulator |= residual << <u64>accumulated
      accumulated += w
      continue
    }
    // This residual straddles the word boundary, or lands exactly on it.
    const low = 64 - accumulated
    accumulator |= residual << <u64>accumulated
    out.writeLE(accumulator, 8)
    accumulator = 0
    accumulated = 0
    if (low < w) {
      accumulator = residual >> <u64>low
      accumulated = w - low
    }
  }
  while (accumulated > 0) {
    out.writeByte(<u8>accumulator)
    accumulator >>= 8
    accumulated -= 8
  }
}

/**
 * Reads count residuals at w bits each into the scratch block.
 *
 * One load, one shift and one mask per value, with the width hoisted out of the
 * loop and no carry between iterations — which is what lets the loop run wide.
 * Whether the buffer holds eight bytes beyond the run decides which path is
 * taken, not whether the read succeeds.
 */
function unpackRun(buf: Uint8Array, from: i32, count: i32, w: i32): void {
  if (w == 0) {
    for (let index = 0; index < count; index++) unchecked((scratch[index] = 0))
    return
  }
  if (w > WIDE_WIDTH) {
    unpackWide(buf, from, count, w)
    return
  }
  const mask: u64 = (<u64>1 << <u64>w) - 1
  let position = 0
  if (from + blockBytes(count, w) + 8 <= buf.length) {
    const base = buf.dataStart + <usize>from
    for (let index = 0; index < count; index++) {
      unchecked(
        (scratch[index] = (load<u64>(base + <usize>(position >> 3)) >> <u64>(position & 7)) & mask),
      )
      position += w
    }
    return
  }
  // The tail of a buffer, where the eight-byte load would read past the end.
  // w is at most 57 here, so the value still ends within eight bytes of where it
  // starts and gathering those eight is enough.
  for (let index = 0; index < count; index++) {
    unchecked((scratch[index] = (gather8(buf, from + (position >> 3)) >> <u64>(position & 7)) & mask))
    position += w
  }
}

/**
 * Widths 58..64, where one eight-byte load can no longer hold a whole value:
 * starting at bit 7 of a byte, a 64-bit value reaches into a ninth. Out of line
 * because it is the rare width, and a column that reaches it is incompressible
 * anyway.
 */
function unpackWide(buf: Uint8Array, from: i32, count: i32, w: i32): void {
  // Built in two shifts so that w == 64 does not shift a u64 by 64.
  const mask: u64 = ((<u64>1 << <u64>(w - 1)) << 1) - 1
  let position = 0
  for (let index = 0; index < count; index++) {
    const at = from + (position >> 3)
    const shift = position & 7
    let value = gather8(buf, at) >> <u64>shift
    if (shift != 0 && at + 8 < buf.length) {
      value |= (<u64>unchecked(buf[at + 8])) << <u64>(64 - shift)
    }
    unchecked((scratch[index] = value & mask))
    position += w
  }
}

// ---- the array codec --------------------------------------------------------

/**
 * The payload size a transform would produce, block widths and all — which is
 * what a transform has to be scored against. Scoring it by the column's widest
 * residual instead picks frame-of-reference for a column of small ids and loses
 * 76%, where scoring it this way picks delta and loses 14%.
 */
function blockedCost(
  vals: Int64Array,
  count: i32,
  transform: u8,
  base: i64,
  zz: bool,
): i32 {
  const n = residualsUnder(transform, count)
  let total = transform != TR_RAW ? 8 : 0 // the base, in the clear
  for (let start = 0; start < n; start += BLOCK_SIZE) {
    const end = min(start + BLOCK_SIZE, n)
    let set: u64 = 0
    if (transform == TR_FOR) {
      for (let index = start; index < end; index++) {
        set |= <u64>unchecked(vals[index]) - <u64>base
      }
    } else if (transform == TR_DELTA) {
      for (let index = start; index < end; index++) {
        set |= zigzag(unchecked(vals[index + 1]) - unchecked(vals[index]))
      }
    } else {
      for (let index = start; index < end; index++) {
        set |= encodeOne(unchecked(vals[index]), zz)
      }
    }
    total += 1 + blockBytes(end - start, widthOfSet(set))
  }
  return total
}

/**
 * Encodes the first `count` values of vals onto w. `width` is the element width
 * in bytes; it comes from the schema and never reaches the wire.
 */
export function appendArray(out: Writer, vals: Int64Array, count: i32, width: i32): void {
  if (count == 0) {
    out.writeByte(TR_RAW)
    return
  }

  // One pass for everything the transforms need to be scored: the minimum for
  // the frame of reference, whether anything is negative, whether every value is
  // the same, and whether a delta could overflow.
  const first = unchecked(vals[0])
  let minimum = first
  let constant = true
  let deltaOverflows = false
  for (let index = 0; index < count; index++) {
    const x = unchecked(vals[index])
    if (x < minimum) minimum = x
    if (x != first) constant = false
    // A delta can only overflow i64 for 64-bit input, and only across a span
    // above 2^63.
    if (index > 0 && width == 8 && subOverflows(x, unchecked(vals[index - 1]))) {
      deltaOverflows = true
    }
  }

  // Raw is zigzagged whenever the column has a negative, which is what keeps -1
  // from becoming a 64-bit residual.
  const zz = minimum < 0
  let transform = TR_RAW
  let zigzagged = zz
  let best = blockedCost(vals, count, TR_RAW, 0, zz)

  const forCost = blockedCost(vals, count, TR_FOR, minimum, false)
  if (forCost < best) {
    transform = TR_FOR
    zigzagged = false
    best = forCost
  }
  if (!deltaOverflows && count > 1) {
    const deltaCost = blockedCost(vals, count, TR_DELTA, 0, true)
    if (deltaCost < best) {
      transform = TR_DELTA
      zigzagged = true
      best = deltaCost
    }
  }
  // Constant is scored rather than short-circuited. It is unbeatable on a long
  // column and beaten on a short one: three small values are three bytes raw
  // against the eight a base costs, so taking it on sight would make the
  // smallest columns the most expensive.
  if (constant && CONSTANT_COST < best) {
    out.writeByte(TR_CONSTANT)
    out.writeLE(zigzag(minimum), 8)
    return
  }

  out.writeByte(zigzagged ? transform | ZIGZAG_FLAG : transform)
  if (transform == TR_FOR) {
    out.writeLE(zigzag(minimum), 8)
  } else if (transform == TR_DELTA) {
    out.writeLE(zigzag(first), 8)
  }
  appendBlocks(out, vals, count, transform, minimum, zigzagged)
}

function appendBlocks(
  out: Writer,
  vals: Int64Array,
  count: i32,
  transform: u8,
  base: i64,
  zz: bool,
): void {
  const n = residualsUnder(transform, count)
  for (let start = 0; start < n; start += BLOCK_SIZE) {
    const end = min(start + BLOCK_SIZE, n)
    const size = end - start
    let set: u64 = 0
    if (transform == TR_FOR) {
      for (let index = 0; index < size; index++) {
        const residual = <u64>unchecked(vals[start + index]) - <u64>base
        unchecked((scratch[index] = residual))
        set |= residual
      }
    } else if (transform == TR_DELTA) {
      for (let index = 0; index < size; index++) {
        const at = start + index
        const residual = zigzag(unchecked(vals[at + 1]) - unchecked(vals[at]))
        unchecked((scratch[index] = residual))
        set |= residual
      }
    } else {
      for (let index = 0; index < size; index++) {
        const residual = encodeOne(unchecked(vals[start + index]), zz)
        unchecked((scratch[index] = residual))
        set |= residual
      }
    }
    const w = widthOfSet(set)
    out.writeByte(<u8>w)
    packRun(out, size, w)
  }
}

/**
 * Reads n values written by appendArray into out, advancing the reader past the
 * column. `width` must be the one the encoder used.
 *
 * Returns false with the reader's failure recorded, rather than trapping: the
 * counts and widths here come off a wire and a corrupt one must be a diagnostic.
 */
export function decodeArray(r: Reader, n: i32, out: Int64Array, width: i32): bool {
  if (n < 0 || out.length < n) {
    r.fail(2 /* ERR_CORRUPT */)
    return false
  }
  const header = r.readByte()
  if (!r.ok) return false
  const transform = header & 0x03
  const zz = (header & ZIGZAG_FLAG) != 0
  if (n == 0) return true

  let base: i64 = 0
  if (transform != TR_RAW) {
    base = unzigzag(r.readLE(8))
    if (!r.ok) return false
  }
  if (transform == TR_CONSTANT) {
    const value = truncate(base, width)
    for (let index = 0; index < n; index++) unchecked((out[index] = value))
    return true
  }

  // Delta's first value is the base itself; the blocks carry the steps.
  let at = 0
  let remaining = n
  if (transform == TR_DELTA) {
    unchecked((out[0] = truncate(base, width)))
    at = 1
    remaining = n - 1
  }

  // Delta accumulates in an i64 rather than in out[i], because a step can exceed
  // the element's range even when every value fits it: for an 8-bit column,
  // 127 - (-128) is 255. Truncating first and summing afterwards would lose it.
  let accumulator = base

  for (let start = 0; start < remaining; start += BLOCK_SIZE) {
    const size = min(start + BLOCK_SIZE, remaining) - start
    const w = <i32>r.readByte()
    if (!r.ok) return false
    if (w > 64) {
      r.fail(2 /* ERR_CORRUPT */)
      return false
    }
    const bytes = blockBytes(size, w)
    if (!r.has(bytes)) return false
    unpackRun(r.buf, r.pos, size, w)
    r.pos += bytes
    if (transform == TR_FOR) {
      for (let index = 0; index < size; index++) {
        const value = <i64>(<u64>base + unchecked(scratch[index]))
        unchecked((out[at + start + index] = truncate(value, width)))
      }
    } else if (transform == TR_DELTA) {
      for (let index = 0; index < size; index++) {
        accumulator += unzigzag(unchecked(scratch[index]))
        unchecked((out[at + start + index] = truncate(accumulator, width)))
      }
    } else {
      for (let index = 0; index < size; index++) {
        const value = decodeOne(unchecked(scratch[index]), zz)
        unchecked((out[at + start + index] = truncate(value, width)))
      }
    }
  }
  return true
}
