// Growable output and bounds-checked input.
//
// The Reader is where PLAN.md §4.3 is implemented. Go's column readers index
// the buffer directly and rely on the runtime to panic on a bad index, which
// DecodeJSON then recovers. AssemblyScript has neither half of that: an
// unchecked read returns adjacent memory and a checked one aborts, and neither
// can be turned into a diagnostic. So every read here tests the bound itself and
// records a failure instead of trapping or fabricating a byte.

export const ERR_NONE: i32 = 0
export const ERR_TRUNCATED: i32 = 1
export const ERR_CORRUPT: i32 = 2

export class Writer {
  buf: Uint8Array
  len: i32

  constructor(capacity: i32 = 256) {
    this.buf = new Uint8Array(capacity)
    this.len = 0
  }

  @inline ensure(extra: i32): void {
    const need = this.len + extra
    if (need <= this.buf.length) return
    let capacity = this.buf.length > 0 ? this.buf.length : 256
    while (capacity < need) capacity <<= 1
    const next = new Uint8Array(capacity)
    memory.copy(next.dataStart, this.buf.dataStart, <usize>this.len)
    this.buf = next
  }

  @inline writeByte(b: u8): void {
    this.ensure(1)
    unchecked((this.buf[this.len++] = b))
  }

  writeBytes(src: Uint8Array, offset: i32, length: i32): void {
    if (length <= 0) return
    this.ensure(length)
    memory.copy(this.buf.dataStart + <usize>this.len, src.dataStart + <usize>offset, <usize>length)
    this.len += length
  }

  /** A copy of exactly what was written. */
  take(): Uint8Array {
    const out = new Uint8Array(this.len)
    memory.copy(out.dataStart, this.buf.dataStart, <usize>this.len)
    return out
  }
}

export class Reader {
  buf: Uint8Array
  pos: i32
  err: i32

  constructor(buf: Uint8Array, pos: i32 = 0) {
    this.buf = buf
    this.pos = pos
    this.err = ERR_NONE
  }

  @inline get ok(): bool {
    return this.err == ERR_NONE
  }

  @inline get remaining(): i32 {
    return this.buf.length - this.pos
  }

  @inline fail(code: i32): void {
    if (this.err == ERR_NONE) this.err = code
  }

  /** Reads one byte, or records ERR_TRUNCATED and returns 0. Never traps. */
  @inline readByte(): u8 {
    if (this.pos >= this.buf.length) {
      this.fail(ERR_TRUNCATED)
      return 0
    }
    return unchecked(this.buf[this.pos++])
  }

  /** True when n more bytes are actually present; records the failure if not. */
  @inline has(n: i32): bool {
    if (n < 0 || n > this.remaining) {
      this.fail(ERR_TRUNCATED)
      return false
    }
    return true
  }
}
