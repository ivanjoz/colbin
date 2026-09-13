// Port of github.com/ivanjoz/colbin/wire/wide.go, composite.go and table.go: the
// eight-bit key width, K8.
//
//	0 vvvvvvv                   the value, 0..127, no payload
//	1 ccc dddd                  class ccc, detail dddd
//
//	class 0 INT      [pos:1][n:3]        n magnitude bytes, as K4
//	class 1 BLOB     [enc:2][lw:2]       [size: lw] then size bytes
//	class 2 VEC      [w:2][pos:1][lw:1]  [bytelen: lw] then the elements
//	class 3 COL      the column codec
//	class 4 LIST     [homog:1][-:1][lw:2] [bytelen: lw][count] then elements
//	class 5 STRUCT   [k8:1][-:1][lw:2]   [bytelen: lw] then a key run
//	class 6 MAP      [table:1][k8:1][lw:2] [bytelen: lw][count] then entries
//	class 7 SPECIAL  [detail:4]          null, true, false, and the varint
//
// What the extra byte buys is **skip**: every class either carries a byte length
// or has one derivable from the descriptor alone, so a reader that has never
// heard of a key can step over it. That is the whole of what K4 gives up, and it
// is why the walk in walk.ts can tolerate a message holding a field the schema
// does not list — under this width and not under the other.

import { Reader, Writer } from '../bytes'
import { decodeArray } from '../column'
import { appendString } from '../packed5'
import {
  CLASS_BLOB,
  CLASS_COL,
  CLASS_INT,
  CLASS_LIST,
  CLASS_MAP,
  CLASS_SPECIAL,
  CLASS_STRUCT,
  CLASS_VEC,
  DESC_EXPLICIT,
  ELEMENT_SIZE_ESCAPE,
  ENC_DICTIONARY,
  ENC_PACKED5_UP,
  ENC_RAW,
  INT_POSITIVE_FLAG,
  LENGTH_WIDTH,
  MAGNITUDE_WIDTH,
  MAX_SIZE,
  SPECIAL_VARINT,
  STRUCT_WIDE_KEYS,
  TABLE_FLAG,
  VARINT_BITS,
  W_BAD_COLUMN,
  W_BAD_DESCRIPTOR,
  W_BAD_PACKED,
  W_UNSUPPORTED_ENC,
  W_OK,
  W_SIZE_TOO_LARGE,
  W_TRUNCATED,
  classOf,
  lengthCodeFor,
  floatFromReversed32,
  floatFromReversed64,
  leUintAt,
} from './desc'
import { Body, Vec } from './narrow'

/** What eight key bits buy: keys 0..255. */
export const MAX_WIDE_FIELDS: i32 = 256

/**
 * A blob field, at eight key bits: a key, a BLOB descriptor whose low two bits
 * name the width of the size, and then the size and the bytes.
 *
 * Here for the same reason its narrow twin is — §4.2 of REFACTOR_PLAN.md moves
 * the `enc` field these two bits sit beside, and the vectors need something to
 * hold it still until then.
 */
export function writeBlob(out: Writer, key: u8, value: Uint8Array): void {
  const size = value.length
  if (size == 0) return
  const code = lengthCodeFor(<u64>size)
  out.writeByte(key)
  out.writeByte(DESC_EXPLICIT | (CLASS_BLOB << 4) | <u8>code)
  out.writeLE(<u64>size, unchecked(LENGTH_WIDTH[code]))
  out.writeBytes(value, 0, size)
}

/** A count and a reader over what follows it. */
export class Counted {
  count: i32 = 0
  reader: WideReader = new WideReader(new Uint8Array(0))
  ok: bool = false
}

@inline
function unzigzag(u: u64): i64 {
  return <i64>(u >> 1) ^ -(<i64>(u & 1))
}

export class WideReader {
  buf: Uint8Array
  at: i32
  err: i32

  constructor(buf: Uint8Array, at: i32 = 0) {
    this.buf = buf
    this.at = at
    this.err = W_OK
  }

  /** A key and a descriptor are two bytes, so one field needs at least that. */
  @inline get more(): bool {
    return this.at + 1 < this.buf.length
  }

  @inline get key(): u8 {
    return this.at < this.buf.length ? unchecked(this.buf[this.at]) : 0
  }

  @inline get ok(): bool {
    return this.err == W_OK
  }

  fail(code: i32): void {
    if (this.err == W_OK) this.err = code
    this.at = this.buf.length
  }

  /** The descriptor byte, which sits one past the key. */
  @inline private desc(): u8 {
    if (this.at + 2 > this.buf.length) {
      this.fail(W_TRUNCATED)
      return 0
    }
    return unchecked(this.buf[this.at + 1])
  }

  // ---- integers -------------------------------------------------------------

  /**
   * An unsigned field. A descriptor whose top bit is clear *is* the value, 0..127,
   * which is why the wide key is a byte worse only above 127 rather than always.
   */
  uint(): u64 {
    const desc = this.desc()
    if (!this.ok) return 0
    if (desc < DESC_EXPLICIT) {
      this.at += 2
      return <u64>desc
    }
    const class_ = classOf(desc)
    if (class_ != CLASS_INT) {
      if (class_ == CLASS_SPECIAL && (desc & SPECIAL_VARINT) != 0) {
        return this.varint()
      }
      this.fail(W_BAD_DESCRIPTOR)
      return 0
    }
    const width = unchecked(MAGNITUDE_WIDTH[desc & 0b111])
    if (width == 0) {
      this.at += 2
      return 1
    }
    if (this.at + 2 + width > this.buf.length) {
      this.fail(W_TRUNCATED)
      return 0
    }
    const value = leUintAt(this.buf, this.at + 2, width)
    this.at += 2 + width
    return value
  }

  /**
   * The K8 varint: three value bits in the descriptor, seven per byte after it.
   * The writer emits it only when it is shorter than the sign-and-magnitude
   * form, so nothing on the wire ever got bigger for it.
   */
  private varint(): u64 {
    const desc = unchecked(this.buf[this.at + 1])
    let value = <u64>(desc & 0b111)
    let shift = VARINT_BITS
    for (let at = this.at + 2; at < this.buf.length; at++) {
      const b = unchecked(this.buf[at])
      value |= (<u64>(b & 0x7f)) << <u64>shift
      if (b < 0x80) {
        this.at = at + 1
        return value
      }
      shift += 7
      if (shift > 63 + VARINT_BITS) {
        this.fail(W_TRUNCATED)
        return 0
      }
    }
    this.fail(W_TRUNCATED)
    return 0
  }

  /** A signed field. A varint written by the signed writer carries zigzag. */
  int(): i64 {
    const desc = this.desc()
    if (!this.ok) return 0
    if (desc >= DESC_EXPLICIT && classOf(desc) == CLASS_SPECIAL && (desc & SPECIAL_VARINT) != 0) {
      return unzigzag(this.uint())
    }
    const negative =
      desc >= DESC_EXPLICIT && classOf(desc) == CLASS_INT && (desc & INT_POSITIVE_FLAG) == 0
    const magnitude = this.uint()
    return negative ? -(<i64>magnitude) : <i64>magnitude
  }

  bool(): bool {
    return this.uint() == 1
  }

  f32(): f32 {
    return floatFromReversed32(<u32>this.uint())
  }

  f64(): f64 {
    return floatFromReversed64(this.uint())
  }

  // ---- lengths --------------------------------------------------------------

  /** Where lengthOf() left the payload. */
  private payloadStart: i32 = 0

  /**
   * The length a class's descriptor declares, with payloadStart set past it.
   *
   * It checks the class too, so a reader asking for a string and finding an
   * array is told rather than handed nonsense.
   */
  private lengthOf(want: u8): i32 {
    const desc = this.desc()
    if (!this.ok) return 0
    if (desc < DESC_EXPLICIT || classOf(desc) != want) {
      this.fail(W_BAD_DESCRIPTOR)
      return 0
    }
    let width = unchecked(LENGTH_WIDTH[desc & 0b11])
    if (want == CLASS_VEC) {
      width = (desc & 1) != 0 ? 4 : 1
    }
    if (this.at + 2 + width > this.buf.length) {
      this.fail(W_TRUNCATED)
      return 0
    }
    const value = leUintAt(this.buf, this.at + 2, width)
    if (value > MAX_SIZE) {
      this.fail(W_SIZE_TOO_LARGE)
      return 0
    }
    this.payloadStart = this.at + 2 + width
    return <i32>value
  }

  // ---- blobs ----------------------------------------------------------------

  /** The blob's encoding code, bits 3..2 of its descriptor. 0 is raw. */
  blobEncoding(): u8 {
    const desc = this.desc()
    if (!this.ok) return 0
    return (desc >> 2) & 0b11
  }

  /**
   * A string field, packed or raw. The descriptor's `enc` code says which — and
   * says the case mode the unit stream opens in, which is the one thing the
   * schema cannot know.
   *
   * Embedded in a BLOB the frame has no header of its own: the descriptor
   * already carries the encoding and the size, so all three of a standalone
   * frame's header fields are duplicates and the payload is the bare unit
   * stream.
   */
  packedString(out: Writer): Uint8Array {
    this.packedIntoOut = false
    const enc = this.blobEncoding()
    if (!this.ok) return new Uint8Array(0)
    if (enc == ENC_RAW) return this.bytes()
    if (enc == ENC_DICTIONARY) {
      // Reserved for a column dictionary and never written. Refused rather than
      // guessed at, so claiming it later is a clean format change.
      this.fail(W_UNSUPPORTED_ENC)
      return new Uint8Array(0)
    }
    const payload = this.bytes()
    if (!this.ok) return new Uint8Array(0)
    const from = out.len
    if (!appendString(out, payload, 0, payload.length, enc == ENC_PACKED5_UP)) {
      this.fail(W_BAD_PACKED)
      return new Uint8Array(0)
    }
    this.packedIntoOut = true
    this.packedFrom = from
    return new Uint8Array(0)
  }

  /** Whether the last packedString expanded into the writer rather than
   * returning a view, and where in it the expansion began. */
  packedIntoOut: bool = false
  packedFrom: i32 = 0

  /** Steps over a string field of either encoding without expanding it. The
   * wide descriptor sizes every class on its own, so this is `skip`. */
  skipString(): void {
    this.skip()
  }

  bytes(): Uint8Array {
    const size = this.lengthOf(CLASS_BLOB)
    if (!this.ok) return new Uint8Array(0)
    const start = this.payloadStart
    if (size > this.buf.length - start) {
      this.fail(W_TRUNCATED)
      return new Uint8Array(0)
    }
    this.at = start + size
    return this.buf.subarray(start, start + size)
  }

  // ---- vectors and string arrays --------------------------------------------

  vec(): Vec {
    const out = new Vec()
    const byteLength = this.lengthOf(CLASS_VEC)
    if (!this.ok) return out
    const start = this.payloadStart
    const desc = unchecked(this.buf[this.at + 1])
    const width = 1 << ((desc >> 2) & 0b11)
    if (byteLength % width != 0 || byteLength > this.buf.length - start) {
      this.fail(W_TRUNCATED)
      return out
    }
    this.at = start + byteLength
    out.elements = this.buf.subarray(start, start + byteLength)
    out.width = width
    out.positive = (desc & 0b10) != 0
    out.ok = true
    return out
  }

  strings(): Array<Uint8Array> {
    const out = new Array<Uint8Array>()
    const count = this.listHeader()
    if (!this.ok) return out
    let at = this.elementStart
    for (let index = 0; index < count; index++) {
      const size = this.elementSize(at)
      if (!this.ok) return out
      const next = this.elementStart
      if (size > this.buf.length - next) {
        this.fail(W_TRUNCATED)
        return out
      }
      out.push(this.buf.subarray(next, next + size))
      at = next + size
    }
    this.at = at
    return out
  }

  private elementStart: i32 = 0

  /** A list's byte length and count, with elementStart at the first element. */
  private listHeader(): i32 {
    const length = this.lengthOf(CLASS_LIST)
    if (!this.ok) return 0
    const start = this.payloadStart
    if (length > this.buf.length - start) {
      this.fail(W_TRUNCATED)
      return 0
    }
    const count = this.readCount(start)
    if (!this.ok) return 0
    return count
  }

  /** One list element length: one byte, or four more behind the escape. */
  private elementSize(at: i32): i32 {
    if (at >= this.buf.length) {
      this.fail(W_TRUNCATED)
      return 0
    }
    const size = unchecked(this.buf[at])
    if (size != ELEMENT_SIZE_ESCAPE) {
      this.elementStart = at + 1
      return <i32>size
    }
    if (at + 5 > this.buf.length) {
      this.fail(W_TRUNCATED)
      return 0
    }
    const value = leUintAt(this.buf, at + 1, 4)
    if (value > MAX_SIZE) {
      this.fail(W_SIZE_TOO_LARGE)
      return 0
    }
    this.elementStart = at + 5
    return <i32>value
  }

  /** A count written the way this format writes every count: one byte to four. */
  private readCount(at: i32): i32 {
    if (at >= this.buf.length) {
      this.fail(W_TRUNCATED)
      return 0
    }
    const value = unchecked(this.buf[at])
    if (value != ELEMENT_SIZE_ESCAPE) {
      this.elementStart = at + 1
      return <i32>value
    }
    if (at + 5 > this.buf.length) {
      this.fail(W_TRUNCATED)
      return 0
    }
    const wide = leUintAt(this.buf, at + 1, 4)
    if (wide > MAX_SIZE) {
      this.fail(W_SIZE_TOO_LARGE)
      return 0
    }
    this.elementStart = at + 5
    return <i32>wide
  }

  // ---- composites -----------------------------------------------------------

  structBody(): Body {
    const out = new Body()
    const desc = this.desc()
    if (!this.ok) return out
    out.wide = (desc & STRUCT_WIDE_KEYS) != 0
    out.bytes = this.compositeBody(CLASS_STRUCT)
    out.ok = this.ok
    return out
  }

  /** A composite's body as a view, with the cursor past the whole field. */
  compositeBody(want: u8): Uint8Array {
    const length = this.lengthOf(want)
    if (!this.ok) return new Uint8Array(0)
    const start = this.payloadStart
    if (length > this.buf.length - start) {
      this.fail(W_TRUNCATED)
      return new Uint8Array(0)
    }
    this.at = start + length
    return this.buf.subarray(start, start + length)
  }

  /** Whether the MAP-class field at the cursor is a table rather than a map. */
  @inline isTable(): bool {
    return this.at + 1 < this.buf.length && (unchecked(this.buf[this.at + 1]) & TABLE_FLAG) != 0
  }

  /** Whether a table's columns are keyed at eight bits. */
  @inline tableIsWide(): bool {
    return (
      this.at + 1 < this.buf.length && (unchecked(this.buf[this.at + 1]) & STRUCT_WIDE_KEYS) != 0
    )
  }

  list(): Counted {
    return this.countedBody(CLASS_LIST, true)
  }

  map(): Counted {
    return this.countedBody(CLASS_MAP, true)
  }

  /** A table's row count and a reader over its columns, which are keyed. */
  table(): Counted {
    const out = new Counted()
    if (!this.isTable()) {
      this.fail(W_BAD_DESCRIPTOR)
      return out
    }
    return this.countedBody(CLASS_MAP, false)
  }

  /**
   * compositeBody for the classes whose body begins with a count.
   *
   * `keyless` starts the element reader one byte in, so that each read can step
   * the cursor back to find its descriptor — a key-less value is one byte
   * shorter than a keyed one, and backing up is what lets both share the value
   * handling above instead of a third copy of it. A table's columns are keyed,
   * so they need no offset.
   */
  private countedBody(want: u8, keyless: bool): Counted {
    const out = new Counted()
    const body = this.compositeBody(want)
    if (!this.ok) return out
    const inner = new WideReader(body)
    const count = inner.readCount(0)
    if (!inner.ok) {
      this.fail(inner.err)
      return out
    }
    const at = inner.elementStart
    out.count = count
    out.reader = keyless
      ? new WideReader(body.subarray(at - 1), 1)
      : new WideReader(body.subarray(at))
    out.ok = true
    return out
  }

  // ---- key-less elements ----------------------------------------------------
  //
  // A value with a descriptor but no key is the same shape as a keyed one minus
  // its first byte, so each of these steps the cursor back and reuses the read
  // above rather than being a second copy of it.

  elementUint(): u64 {
    this.at--
    return this.uint()
  }

  elementInt(): i64 {
    this.at--
    return this.int()
  }

  elementBytes(): Uint8Array {
    this.at--
    return this.bytes()
  }

  elementF64(): f64 {
    this.at--
    return this.f64()
  }

  elementStructBody(): Body {
    this.at--
    return this.structBody()
  }

  @inline get moreElements(): bool {
    return this.err == W_OK && this.at < this.buf.length
  }

  /** The descriptor of the key-less value at the cursor. */
  @inline elementDesc(): u8 {
    if (this.at >= this.buf.length) {
      this.fail(W_TRUNCATED)
      return 0
    }
    return unchecked(this.buf[this.at])
  }

  // ---- columns --------------------------------------------------------------

  column(rows: i32, out: Int64Array, width: i32): bool {
    const body = this.compositeBody(CLASS_COL)
    if (!this.ok) return false
    if (!decodeArray(new Reader(body), rows, out, width)) {
      this.fail(W_BAD_COLUMN)
      return false
    }
    return true
  }

  // ---- skip -----------------------------------------------------------------

  /**
   * Steps over the field at the cursor without knowing what it is, which is the
   * capability the wide key exists for.
   */
  skip(): bool {
    const size = this.fieldSize()
    if (!this.ok) return false
    this.at += size
    return true
  }

  /**
   * The whole of the skip: every class either carries a byte length or has one
   * derivable from its descriptor.
   */
  private fieldSize(): i32 {
    const desc = this.desc()
    if (!this.ok) return 0
    if (desc < DESC_EXPLICIT) return 2 // the descriptor is the value
    const class_ = classOf(desc)
    if (class_ == CLASS_INT) {
      const width = unchecked(MAGNITUDE_WIDTH[desc & 0b111])
      if (this.buf.length - (this.at + 2) < width) {
        this.fail(W_TRUNCATED)
        return 0
      }
      return 2 + width
    }
    if (class_ == CLASS_SPECIAL) {
      if ((desc & SPECIAL_VARINT) == 0) return 2
      // A varint is self-delimiting, so a reader that does not know the key can
      // still step over it: walk the continuation bits to their end.
      const from = this.at
      this.varint()
      if (!this.ok) return 0
      const length = this.at - from
      this.at = from
      return length
    }
    // Every remaining class declares a byte length covering the whole of its
    // payload, which is the property that makes an unknown field skippable
    // without its sub-schema.
    const length = this.lengthOf(class_)
    if (!this.ok) return 0
    const start = this.payloadStart
    if (length > this.buf.length - start) {
      this.fail(W_TRUNCATED)
      return 0
    }
    return start + length - this.at
  }
}
