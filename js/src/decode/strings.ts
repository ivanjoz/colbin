// UTF-8 bytes → JavaScript strings, which is where a decoder in this language
// spends most of its time. Three paths, by what the bytes are:
//
//   - short and ASCII: String.fromCharCode on the bytes, no decoder call;
//   - short with well-formed multi-byte sequences: decoded here, for the same
//     reason — TextDecoder costs a fixed few hundred nanoseconds per call;
//   - anything long, or malformed: TextDecoder, which replaces a malformed
//     sequence with U+FFFD as Go's JSON walk does.
//
// TextDecoder is built with `ignoreBOM`, so a string that begins with U+FEFF
// keeps it: the default strips it, and a string is not a stream.

const fcc = String.fromCharCode

const decoder: { decode(input: Uint8Array): string } | null =
  typeof TextDecoder === 'function' ? new TextDecoder('utf-8', { ignoreBOM: true }) : null

/** Up to this many bytes a string is decoded here rather than by TextDecoder,
 * whose call costs a fixed few hundred nanoseconds. */
const SHORT = 40

/** The string the bytes `b[start..end)` hold. */
export function utf8(b: Uint8Array, start: number, end: number): string {
  const n = end - start
  if (n > SHORT && decoder !== null) return decoder.decode(b.subarray(start, end))
  if (n <= 4) {
    // The shortest strings by argument: no array, no apply.
    const c0 = b[start]
    const c1 = b[start + 1]
    const c2 = b[start + 2]
    const c3 = b[start + 3]
    if (n === 0) return ''
    if ((n === 1 ? c0 : n === 2 ? c0 | c1 : n === 3 ? c0 | c1 | c2 : c0 | c1 | c2 | c3) < 0x80) {
      return n === 1 ? fcc(c0) : n === 2 ? fcc(c0, c1) : n === 3 ? fcc(c0, c1, c2) : fcc(c0, c1, c2, c3)
    }
  } else if (n <= SHORT) {
    // ASCII straight into a reused array of exactly its length.
    const a = codesOf(n)
    let k = 0
    while (k < n && b[start + k] < 0x80) {
      a[k] = b[start + k]
      k++
    }
    if (k === n) return fcc.apply(null, a)
  }
  return decodeHere(b, start, end)
}

/** One plain array per length, reused: String.fromCharCode.apply over a plain
 * array of exactly the string's units is several times faster than over a
 * typed array's subarray, and allocates nothing but the string. */
const codes: number[][] = []

function codesOf(n: number): number[] {
  return (codes[n] ??= new Array(n).fill(0))
}

/**
 * UTF-8 decoded here, as TextDecoder decodes it (WHATWG): a malformed sequence
 * is one U+FFFD per maximal subpart. Used for a short string that is not ASCII,
 * and for every string where there is no TextDecoder.
 */
export function decodeHere(b: Uint8Array, start: number, end: number): string {
  const out: number[] = []
  let parts = ''
  let i = start
  while (i < end) {
    const c = b[i++]
    let cp = c
    if (c >= 0x80) {
      cp = 0xfffd
      // The bytes a lead byte needs after it, and the first one's range.
      const need = c < 0xc2 ? 0 : c < 0xe0 ? 1 : c < 0xf0 ? 2 : c < 0xf5 ? 3 : 0
      let lower = c === 0xe0 ? 0xa0 : c === 0xf0 ? 0x90 : 0x80
      let upper = c === 0xed ? 0x9f : c === 0xf4 ? 0x8f : 0xbf
      let v = c & (0x3f >> need)
      let k = 0
      while (k < need && i < end && b[i] >= lower && b[i] <= upper) {
        v = (v << 6) | (b[i++] & 0x3f)
        lower = 0x80
        upper = 0xbf
        k++
      }
      if (need > 0 && k === need) cp = v
    }
    if (cp > 0xffff) out.push(0xd800 | ((cp - 0x10000) >> 10), 0xdc00 | (cp & 0x3ff))
    else out.push(cp)
    if (out.length > 4096) {
      parts += fcc.apply(null, out)
      out.length = 0
    }
  }
  return parts + fcc.apply(null, out)
}

const KEY_MAX = 32
const KEY_SLOTS = 4096
const keyStrings: (string | undefined)[] = new Array(KEY_SLOTS)
const keyBytes = new Uint8Array(KEY_SLOTS * KEY_MAX)
const keyLengths = new Uint8Array(KEY_SLOTS)

/** The cache slot of the last key() (negative when it was not cached): what
 * the caller hashes a key sequence by, without reading the key again. */
export const lastKey = { slot: -1 }

/** A map key: the string the bytes hold, the same string object each time the
 * same bytes come back. A property store with a string the engine has already
 * interned skips the string-table lookup a fresh string costs, and documents
 * repeat their keys. One direct-mapped slot per hash: a collision decodes. */
export function key(b: Uint8Array, start: number, end: number): string {
  const n = end - start
  if (n === 0 || n > KEY_MAX) {
    lastKey.slot = -1 - n
    return utf8(b, start, end)
  }
  // The length, the first four bytes and the last four pick the slot; the
  // comparison below is what makes a hit exact.
  let h = n
  if (n >= 4) {
    h = Math.imul(h ^ b[start] ^ (b[start + 1] << 8) ^ (b[start + 2] << 16) ^ (b[start + 3] << 24), 0x9e3779b1)
    h = Math.imul(h ^ b[end - 4] ^ (b[end - 3] << 8) ^ (b[end - 2] << 16) ^ (b[end - 1] << 24), 0x85ebca6b)
  } else for (let i = start; i < end; i++) h = Math.imul(h ^ b[i], 0x9e3779b1)
  const slot = (h ^ (h >>> 15)) & (KEY_SLOTS - 1)
  const base = slot * KEY_MAX
  lastKey.slot = slot
  const known = keyStrings[slot]
  if (known !== undefined && keyLengths[slot] === n) {
    let i = 0
    while (i < n && keyBytes[base + i] === b[start + i]) i++
    if (i === n) return known
  }
  const fresh = utf8(b, start, end)
  keyStrings[slot] = fresh
  keyLengths[slot] = n
  keyBytes.set(b.subarray(start, end), base)
  return fresh
}

/** The bytes `b[start..end)` decoded as one string, or null without a
 * TextDecoder. A run of length-prefixed strings is decoded with this once and
 * each string cut out with substring.
 *
 * When the result is exactly as long as the input, every byte decoded to one
 * unit and the strings sit at their byte offsets. Why the length test is
 * enough: the decoder turns every group of bytes (one ASCII byte, one
 * multi-byte sequence, one malformed run) into one unit, or two for a
 * four-byte sequence, so the result is never longer than the input, and is as
 * long only when every group is a single byte. Then a string's bytes decode to
 * the same units alone as inside the run. Otherwise see wellFormedUnits. */
export function wholeRun(b: Uint8Array, start: number, end: number): string | null {
  if (decoder === null) return null
  return decoder.decode(b.subarray(start, end))
}

/** How many UTF-16 units the well-formed UTF-8 `b[start..end)` decodes to, or
 * -1 when it is malformed (WHATWG's rules: no overlong forms, no surrogates,
 * nothing past U+10FFFF, no truncated sequence).
 *
 * This is how a run of strings that does not map byte for byte (wholeRun) is
 * still cut from one decoded string. A well-formed string ends on a complete
 * sequence and begins with a byte that is not a continuation, so the one-byte
 * length before it decodes to exactly one unit (itself, or U+FFFD) and nothing
 * on either side merges with it: its units sit in the run's text at the sum of
 * the units before it. */
export function wellFormedUnits(b: Uint8Array, start: number, end: number): number {
  let units = 0
  let i = start
  while (i < end) {
    const c = b[i]
    if (c < 0x80) {
      units++
      i++
      continue
    }
    if (c < 0xc2) return -1
    if (c < 0xe0) {
      if (i + 1 >= end || (b[i + 1] & 0xc0) !== 0x80) return -1
      units++
      i += 2
      continue
    }
    if (c < 0xf0) {
      if (i + 2 >= end) return -1
      const c1 = b[i + 1]
      if ((c1 & 0xc0) !== 0x80 || (b[i + 2] & 0xc0) !== 0x80) return -1
      if (c === 0xe0 ? c1 < 0xa0 : c === 0xed && c1 >= 0xa0) return -1
      units++
      i += 3
      continue
    }
    if (c < 0xf5) {
      if (i + 3 >= end) return -1
      const c1 = b[i + 1]
      if ((c1 & 0xc0) !== 0x80 || (b[i + 2] & 0xc0) !== 0x80 || (b[i + 3] & 0xc0) !== 0x80) return -1
      if (c === 0xf0 ? c1 < 0x90 : c === 0xf4 && c1 >= 0x90) return -1
      units += 2
      i += 4
      continue
    }
    return -1
  }
  return units
}

/** How many units the four bytes of a u32 length decode to inside a run: the
 * string after them begins with no continuation byte, so the same as alone. */
export function lengthUnits(b: Uint8Array, at: number): number {
  return decoder === null ? 4 : decoder.decode(b.subarray(at, at + 4)).length
}
