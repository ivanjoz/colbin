// packed5 (INTERNALS.md §8): the opt-in 5-bit string codec, decode only.
// Mirrors `rust/src/packed5.rs` `append_string`. A packed string carries no
// header: the descriptor says it is packed and in which case it opens.

import { fail, BAD_DESCRIPTOR } from './fail.ts'
import { utf8 } from './strings.ts'

const BAD_PACKED = BAD_DESCRIPTOR
const SYM = '0123456789.,-/:;_()%#"\'!?@=+*&<>'
const EXT = 'ñáéíóúüÑÁÉÍÓÚ€$~`\\[]^{}|\n\t\r¿'
const encoder = { bytes: [] as number[] }

/** The text a packed payload holds. The units may carry raw bytes, so the text
 * is assembled as UTF-8 and decoded once at the end. */
export function packed5(b: Uint8Array, start: number, end: number, upper: boolean): string {
  const n = end - start
  const count = Math.floor((n * 8) / 5)
  const units = new Uint8Array(count + 3)
  for (let u = 0; u < count; u++) {
    const bit = u * 5
    const at = start + (bit >> 3)
    const word = b[at] | (at + 1 < end ? b[at + 1] << 8 : 0)
    units[u] = (word >> (bit & 7)) & 31
  }
  const out = encoder.bytes
  out.length = 0
  let cur = upper
  let pending = false
  let at = 0
  while (at < count) {
    const op = units[at++]
    if (op <= 25) {
      out.push((cur !== pending ? 65 : 97) + op)
      pending = false
    } else if (op === 26) out.push(32)
    else if (op === 27) pending = true
    else if (op === 28) cur = !cur
    else if (op === 29) {
      if (at >= count) fail(BAD_PACKED)
      out.push(SYM.charCodeAt(units[at++]))
    } else if (op === 30) {
      if (at >= count) fail(BAD_PACKED)
      const index = units[at++]
      if (index !== 31) {
        if (index >= 28) fail(BAD_PACKED)
        const c = EXT.charCodeAt(index)
        // The table is all BMP: one, two or three bytes of UTF-8.
        if (c < 0x80) out.push(c)
        else if (c < 0x800) out.push(0xc0 | (c >> 6), 0x80 | (c & 0x3f))
        else out.push(0xe0 | (c >> 12), 0x80 | ((c >> 6) & 0x3f), 0x80 | (c & 0x3f))
        continue
      }
      if (at >= count) fail(BAD_PACKED)
      const run = units[at++] + 1
      if (run > 4) fail(BAD_PACKED)
      if (at + 2 * run > count) fail(BAD_PACKED)
      for (let k = 0; k < run; k++) {
        const low = units[at]
        const high = units[at + 1]
        if (high > 7) fail(BAD_PACKED)
        out.push(low | (high << 5))
        at += 2
      }
    } else {
      if (at + 2 > count) fail(BAD_PACKED)
      const value = units[at] | (units[at + 1] << 5)
      at += 2
      const digits = String(value)
      for (let k = 0; k < digits.length; k++) out.push(digits.charCodeAt(k))
    }
  }
  const bytes = Uint8Array.from(out)
  return utf8(bytes, 0, bytes.length)
}
