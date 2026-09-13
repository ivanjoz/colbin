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

/** The widest decimal a u64 has, and the scratch Writer.writeDecimalU64 fills
 * from the back. One buffer for the module: nothing here is re-entrant, and the
 * bytes are consumed before the next call can touch them. */
const DECIMAL_DIGITS: i32 = 20
const DECIMAL_SCRATCH = new StaticArray<u8>(DECIMAL_DIGITS)

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

  /**
   * `value` in decimal, written straight into the buffer.
   *
   * The obvious spelling — `this.ascii(value.toString())` — allocates a UTF-16
   * AssemblyScript string per number, walks it back out a `charCodeAt` at a
   * time, and leaves it for the collector. On a table of a thousand records that
   * is thousands of allocations inside the decode, and it measured as most of
   * the decode's cost: the JSON writer ran 9.5x slower than V8's own
   * `JSON.stringify` on the same records, which is not a gap a byte-at-a-time
   * copy explains on its own.
   *
   * Digits come out least significant first, so they go into a fixed scratch
   * from the back and reach the buffer in one `memory.copy`. Twenty bytes is the
   * widest decimal a u64 has.
   */
  writeDecimalU64(value: u64): void {
    let at = DECIMAL_DIGITS
    // Most values in a real record fit in 32 bits, and 32-bit division is
    // materially cheaper than 64-bit in WebAssembly, so the wide loop runs only
    // until the value is small enough for the narrow one.
    let wide = value
    while (wide > 0xffffffff) {
      const q = wide / 10
      unchecked((DECIMAL_SCRATCH[--at] = <u8>(0x30 + <u32>(wide - q * 10))))
      wide = q
    }
    let narrow = <u32>wide
    do {
      const q = narrow / 10
      unchecked((DECIMAL_SCRATCH[--at] = <u8>(0x30 + (narrow - q * 10))))
      narrow = q
    } while (narrow != 0)

    const length = DECIMAL_DIGITS - at
    this.ensure(length)
    memory.copy(
      this.buf.dataStart + <usize>this.len,
      changetype<usize>(DECIMAL_SCRATCH) + <usize>at,
      <usize>length,
    )
    this.len += length
  }

  /** `value` in decimal, with a leading `-` when it is negative. */
  writeDecimalI64(value: i64): void {
    if (value < 0) {
      this.writeByte(0x2d /* - */)
      // Negating i64.MIN_VALUE overflows back to itself, and the u64 conversion
      // of that is 2^63 — which is the magnitude wanted, so the general case
      // covers the one value that would otherwise need its own branch.
      this.writeDecimalU64(<u64>(0 - value))
      return
    }
    this.writeDecimalU64(<u64>value)
  }

  /**
   * The low `count` bytes of `value`, least significant first.
   *
   * Every multi-byte quantity in the format is little-endian, which is one
   * native load on every machine this runs on — so a width-taking store is the
   * shape every caller wants, rather than one method per width.
   */
  @inline writeLE(value: u64, count: i32): void {
    this.ensure(count)
    let at = this.len
    for (let index = 0; index < count; index++) {
      unchecked((this.buf[at++] = <u8>(value >> <u64>(index << 3))))
    }
    this.len = at
  }

  /** Overwrites one byte already written, which is how a composite's reserved
   * length placeholder is patched. */
  @inline setByte(at: i32, b: u8): void {
    unchecked((this.buf[at] = b))
  }

  /** Overwrites `count` little-endian bytes at an offset already written. */
  @inline setLE(at: i32, value: u64, count: i32): void {
    for (let index = 0; index < count; index++) {
      unchecked((this.buf[at + index] = <u8>(value >> <u64>(index << 3))))
    }
  }

  /**
   * Makes room for `count` bytes at `at`, shifting what follows up.
   *
   * This is the rare half of a composite's backpatch: the writer reserves one
   * byte for a length, writes the body, and widens only when the body outgrew
   * it — a memmove on a buffer already in cache, against a pass over every
   * nested value to size it first.
   */
  insert(at: i32, count: i32): void {
    this.ensure(count)
    const tail = this.len - at
    if (tail > 0) {
      memory.copy(
        this.buf.dataStart + <usize>(at + count),
        this.buf.dataStart + <usize>at,
        <usize>tail,
      )
    }
    this.len += count
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

  /**
   * `count` little-endian bytes as a u64, or 0 with the failure recorded.
   *
   * Sizes in this format never escalate past eight bytes, so one entry point
   * covers every length the wire can name.
   */
  @inline readLE(count: i32): u64 {
    if (!this.has(count)) return 0
    let value: u64 = 0
    for (let index = 0; index < count; index++) {
      value |= <u64>unchecked(this.buf[this.pos + index]) << <u64>(index << 3)
    }
    this.pos += count
    return value
  }

  /** The next n bytes as a view onto the same memory, without copying them. */
  slice(n: i32): Uint8Array {
    if (!this.has(n)) return new Uint8Array(0)
    const out = this.buf.subarray(this.pos, this.pos + n)
    this.pos += n
    return out
  }

  /** The byte at an absolute offset, without moving the cursor. */
  @inline at(offset: i32): u8 {
    if (offset < 0 || offset >= this.buf.length) {
      this.fail(ERR_TRUNCATED)
      return 0
    }
    return unchecked(this.buf[offset])
  }
}

/**
 * Eight little-endian bytes from `at`, stopping at the end of the buffer rather
 * than past it.
 *
 * This is the safe form of the column codec's one-load read. Go leans on the
 * runtime's unconditional bounds check for the same job and recovers the panic;
 * here the check has to be the code, because an unchecked load in
 * AssemblyScript returns whatever is adjacent in linear memory and says nothing
 * (PLAN.md §4.3).
 */
@inline
export function gather8(buf: Uint8Array, at: i32): u64 {
  if (at < 0) return 0
  if (at + 8 <= buf.length) return load<u64>(buf.dataStart + <usize>at)
  let value: u64 = 0
  for (let index = 0; at + index < buf.length && index < 8; index++) {
    value |= <u64>unchecked(buf[at + index]) << <u64>(index << 3)
  }
  return value
}
