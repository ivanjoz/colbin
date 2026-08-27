// LSB-first bit packing, matching packed5's convention.
//
// Go's writer drains four bytes at a time because tokens are 5-15 bits and a
// byte-at-a-time drain would touch the buffer on nearly every write. Draining
// one byte at a time here produces the identical byte sequence — LSB-first means
// the order is fixed regardless of drain granularity, and both end with
// ceil(bits/8) bytes — so the simpler loop is kept until a profile says
// otherwise.

import { Writer } from './bytes'

export class BitWriter {
  w: Writer
  acc: u64 = 0
  nbits: u8 = 0

  constructor(w: Writer) {
    this.w = w
  }

  /** Appends the low `width` bits of v. width <= 24. */
  writeBits(v: u32, width: u8): void {
    this.acc |= (<u64>(v & <u32>((1 << width) - 1))) << this.nbits
    this.nbits += width
    while (this.nbits >= 8) {
      this.w.writeByte(<u8>this.acc)
      this.acc >>= 8
      this.nbits -= 8
    }
  }

  /** Emits the pending bits, zero-padded in the high bits of the last byte. */
  flush(): void {
    if (this.nbits > 0) {
      this.w.writeByte(<u8>this.acc)
      this.acc = 0
      this.nbits = 0
    }
  }
}

export class BitReader {
  buf: Uint8Array
  offset: i32 // byte offset of the payload within buf
  acc: u64 = 0
  bit: i32 = 0 // payload bits consumed
  limit: i32 // total readable bits
  pos: i32 // next byte not yet loaded into acc
  nbits: u8 = 0
  ok: bool = true

  constructor(buf: Uint8Array, offset: i32, byteLen: i32) {
    this.buf = buf
    this.offset = offset
    this.pos = offset
    this.limit = byteLen * 8
  }

  @inline get remaining(): i32 {
    return this.limit - this.bit
  }

  /**
   * Next `width` bits, or 0 with ok cleared. The second bound test is not in the
   * Go original, which relies on limit never exceeding the payload; it is here
   * because a corrupt limit must not become an out-of-bounds read (PLAN.md §4.3).
   */
  read(width: u8): u32 {
    if (this.bit + <i32>width > this.limit) {
      this.ok = false
      return 0
    }
    while (this.nbits < width) {
      if (this.pos >= this.buf.length) {
        this.ok = false
        return 0
      }
      this.acc |= (<u64>unchecked(this.buf[this.pos])) << this.nbits
      this.pos++
      this.nbits += 8
    }
    const v = <u32>(this.acc & (((<u64>1) << width) - 1))
    this.acc >>= width
    this.nbits -= width
    this.bit += <i32>width
    return v
  }
}
