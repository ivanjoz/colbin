// Port of github.com/ivanjoz/colbin/wire/narrow.go and narrow_composite.go: the
// four-bit key width, K4.
//
//	integer        [key:4][positive:1][n:3]                  [magnitude: n bytes]
//	unsigned       [key:4][0..7 = the value | 8..15 = n bytes]
//	blob           [key:4][more:1][size hi:3] [size lo:8]    [bytes: size]
//	               [key:4][1][escape:3]       [size: 2|4|8]  [bytes: size]
//	vec            [key:4][positive:1][width:2][more:1] [count:8] [count × width]
//	strings        [key:4][more:1][count:11]  then [size:1, 0xFF → 4] bytes
//	struct         [key:4][k8:1][—:1][lw:2]   [len: lw] [key run]
//	list           [key:4][homog:1][—:1][lw:2] [len: lw] [count] ( [len][body] )*
//	map            [key:4][sub=0:1][—:1][lw:2] [len: lw] [count] ( key value )*
//	table          [key:4][sub=1:1][k8:1][lw:2] [len: lw] [rows] ( [key][desc][col] )*
//
// A narrow descriptor has no room for a class and does not need one: a K4 reader
// has the schema, so it already knows which of these it is looking at. What it
// needs from the wire is the same byte length the wide form needs, and the same
// four detail bits carry it.
//
// Two things follow, and both matter to this port. A narrow list's element is a
// length and a body with **no descriptor between them** — which is why a struct's
// key width is the schema section's to state. And an unknown key cannot be
// stepped over, because nothing on the wire says what shape it is.

import { Reader, Writer } from '../bytes'
import { decodeArray } from '../column'
import { appendString } from '../packed5'
import {
  ARRAY_POSITIVE_FLAG,
  ARRAY_WIDTH_SHIFT,
  ELEMENT_SIZE_ESCAPE,
  ESCAPE_2_BYTES,
  ESCAPE_4_BYTES,
  ESCAPE_8_BYTES,
  ESCAPE_PACKED_1_LO,
  ESCAPE_PACKED_1_UP,
  ESCAPE_PACKED_4_LO,
  ESCAPE_PACKED_4_UP,
  INT_POSITIVE_FLAG,
  LENGTH_WIDTH,
  MAGNITUDE_WIDTH,
  MAX_SIZE,
  MORE_ARRAY_LEN_FLAG,
  MORE_SIZE_FLAG,
  STRUCT_WIDE_KEYS,
  TABLE_FLAG,
  UINT_INLINE_MAX,
  UINT_WIDTH_BASE,
  W_BAD_COLUMN,
  W_BAD_ESCAPE,
  W_BAD_PACKED,
  W_NO_SKIP,
  W_OK,
  W_SIZE_TOO_LARGE,
  W_TRUNCATED,
  floatFromReversed32,
  floatFromReversed64,
  leUintAt,
} from './desc'

/** What four key bits buy: keys 0..15. */
export const MAX_FIELDS: i32 = 16

/**
 * A blob field, at four key bits.
 *
 * The only writer in this file so far: the encoder is phase 4, and this one is
 * here early because REFACTOR_PLAN.md §4.2 is going to change exactly these
 * bytes — the size ceiling drops from 2047 to 1022 and two header bits are
 * re-spent on the encoding — so the vectors need something to hold it still in
 * the meantime.
 *
 * An empty blob is not written at all. Omission is unconditional and there is no
 * flag for it.
 */
export function writeBlob(out: Writer, key: u8, value: Uint8Array): void {
  const size = value.length
  if (size == 0) return
  if (size <= INLINE_BLOB_SIZE) {
    out.writeByte((key << 4) | <u8>(size >> 8))
    out.writeByte(<u8>size)
  } else {
    // Past 2047 the three size bits carry nothing, so they name the width of the
    // size that follows instead: a 5 KB string pays one extra byte, not four.
    const escape: u8 = size <= 0xffff ? ESCAPE_2_BYTES : ESCAPE_4_BYTES
    const width = escape == ESCAPE_2_BYTES ? 2 : 4
    out.writeByte((key << 4) | MORE_SIZE_FLAG | escape)
    out.writeLE(<u64>size, width)
  }
  out.writeBytes(value, 0, size)
}

/** What a narrow blob header carries before an escape is needed. */
const INLINE_BLOB_SIZE: i32 = (1 << 11) - 1

/** A composite's body and the key width the run inside it uses. */
export class Body {
  bytes: Uint8Array = new Uint8Array(0)
  wide: bool = false
  ok: bool = false
}

/** A vector field's elements, with the width and sign its header declared. */
export class Vec {
  elements: Uint8Array = new Uint8Array(0)
  width: i32 = 0
  positive: bool = false
  ok: bool = false

  @inline get count(): i32 {
    return this.width == 0 ? 0 : this.elements.length / this.width
  }

  /** Element i, sign-extended when the header says two's complement. */
  at(index: i32): i64 {
    const offset = index * this.width
    const raw = leUintAt(this.elements, offset, this.width)
    if (this.positive) return <i64>raw
    if (this.width == 1) return <i64><i8>raw
    if (this.width == 2) return <i64><i16>raw
    if (this.width == 4) return <i64><i32>raw
    return <i64>raw
  }
}

export class NarrowReader {
  buf: Uint8Array
  at: i32
  err: i32

  constructor(buf: Uint8Array, at: i32 = 0) {
    this.buf = buf
    this.at = at
    this.err = W_OK
  }

  /**
   * Whether another field follows. It goes false on the first failure, so a
   * decode loop ends rather than spinning on a broken message: fail parks the
   * cursor at the end, so one comparison answers both questions.
   */
  @inline get more(): bool {
    return this.at < this.buf.length
  }

  /** The key the cursor is on. It does not advance: the typed read does. */
  @inline get key(): u8 {
    return this.at < this.buf.length ? unchecked(this.buf[this.at]) >> 4 : 0
  }

  @inline get ok(): bool {
    return this.err == W_OK
  }

  fail(code: i32): void {
    if (this.err == W_OK) this.err = code
    // Parking the cursor at the end is what stops `more` from looping.
    this.at = this.buf.length
  }

  /** The header byte the cursor is on, with a failure recorded past the end. */
  @inline private header(): u8 {
    if (this.at >= this.buf.length) {
      this.fail(W_TRUNCATED)
      return 0
    }
    return unchecked(this.buf[this.at])
  }

  // ---- integers -------------------------------------------------------------

  /** An unsigned field: the sixteen-code nibble, 0..7 inline. */
  uint(): u64 {
    const header = this.header()
    if (!this.ok) return 0
    const code = header & 0b1111
    if (code <= UINT_INLINE_MAX) {
      this.at++
      return <u64>code
    }
    const width = <i32>(code - UINT_WIDTH_BASE) + 1
    if (this.at + 1 + width > this.buf.length) {
      this.fail(W_TRUNCATED)
      return 0
    }
    const value = leUintAt(this.buf, this.at + 1, width)
    this.at += 1 + width
    return value
  }

  /**
   * A signed field: a sign bit and a magnitude.
   *
   * It does not go through uint(). A signed nibble is [positive:1][size:3] and
   * an unsigned one is a sixteen-code table, so the same four bits mean
   * different things and only the schema says which.
   */
  int(): i64 {
    const header = this.header()
    if (!this.ok) return 0
    const positive = (header & INT_POSITIVE_FLAG) != 0
    const magnitude = this.signedMagnitude()
    return positive ? <i64>magnitude : -(<i64>magnitude)
  }

  /** The [size:3] form: code 0 means one, 1..6 are that many bytes, 7 is eight. */
  private signedMagnitude(): u64 {
    const header = this.header()
    if (!this.ok) return 0
    const width = unchecked(MAGNITUDE_WIDTH[header & 0b111])
    if (width == 0) {
      this.at++
      return 1
    }
    if (this.at + 1 + width > this.buf.length) {
      this.fail(W_TRUNCATED)
      return 0
    }
    const value = leUintAt(this.buf, this.at + 1, width)
    this.at += 1 + width
    return value
  }

  bool(): bool {
    return this.uint() == 1
  }

  /**
   * A float rides the integer shape carrying its IEEE-754 bit pattern with the
   * bytes reversed: a float's zero bytes are its low mantissa bytes where an
   * integer's are its high ones, so reversing puts them where the width can
   * elide them.
   */
  f32(): f32 {
    return floatFromReversed32(<u32>this.uint())
  }

  f64(): f64 {
    return floatFromReversed64(this.uint())
  }

  // ---- blobs ----------------------------------------------------------------

  /**
   * A string field, packed or raw — the header's escape code says which, so a
   * reader needs no configuration and cannot be wrong about it.
   *
   * Returns the bytes directly when they are raw, which is the overwhelmingly
   * common case and costs nothing beyond `bytes()`. A packed payload is expanded
   * into `out` and the caller takes it from there, because there is nothing in
   * the message to hand back a view of.
   */
  packedString(out: Writer): Uint8Array {
    this.packedIntoOut = false
    const size = this.stringSpan()
    if (!this.ok) return new Uint8Array(0)
    const start = this.blobStart
    this.at = start + size
    if (!this.stringIsPacked) return this.buf.subarray(start, start + size)

    const from = out.len
    if (!appendString(out, this.buf, start, size, this.stringIsUpper)) {
      this.fail(W_BAD_PACKED)
      return new Uint8Array(0)
    }
    this.packedIntoOut = true
    this.packedFrom = from
    return new Uint8Array(0)
  }

  /**
   * Steps over a string field of either encoding without expanding it, which is
   * what a walk that wants the *span* rather than the characters needs.
   *
   * It exists because the alternative was the bug this port found in Go's own
   * schema walk: reading a string with `bytes()` works right up until the string
   * is packed, and then refuses a message the writer produced.
   */
  skipString(): void {
    const size = this.stringSpan()
    if (!this.ok) return
    this.at = this.blobStart + size
  }

  /**
   * A string field's header, of either encoding, leaving the payload's size and
   * `blobStart` behind it.
   *
   * A packed string names itself through the blob header's escape codes, because
   * a narrow descriptor has no `enc` field — under four key bits the schema says
   * what a field is, not the wire. The one thing the schema cannot know is the
   * case mode the unit stream opens in, so the escape carries that.
   */
  private stringSpan(): i32 {
    this.stringIsPacked = false
    this.stringIsUpper = false
    const header = this.header()
    if (!this.ok) return 0

    const code = header & 0b111
    const narrowSize = code == ESCAPE_PACKED_1_LO || code == ESCAPE_PACKED_1_UP
    const wideSize = code == ESCAPE_PACKED_4_LO || code == ESCAPE_PACKED_4_UP
    if ((header & MORE_SIZE_FLAG) == 0 || (!narrowSize && !wideSize)) {
      // A raw blob. blobSize leaves the size unchecked against the buffer,
      // because bytes() is what checks it — so this has to, or a truncated
      // message would hand back a span running past the end of itself.
      const size = this.blobSize()
      if (!this.ok) return 0
      if (size > this.buf.length - this.blobStart) {
        this.fail(W_TRUNCATED)
        return 0
      }
      return size
    }

    const width = narrowSize ? 1 : 4
    if (this.at + 1 + width > this.buf.length) {
      this.fail(W_TRUNCATED)
      return 0
    }
    const size = <i32>leUintAt(this.buf, this.at + 1, width)
    // One size has one encoding: a four-byte length that would have fitted in
    // one byte is a frame this writer cannot produce, so it is refused rather
    // than read.
    if (wideSize && size <= 0xff) {
      this.fail(W_BAD_ESCAPE)
      return 0
    }
    const start = this.at + 1 + width
    if (size > this.buf.length - start) {
      this.fail(W_TRUNCATED)
      return 0
    }
    this.blobStart = start
    this.stringIsPacked = true
    this.stringIsUpper = code == ESCAPE_PACKED_1_UP || code == ESCAPE_PACKED_4_UP
    return size
  }

  private stringIsPacked: bool = false
  private stringIsUpper: bool = false

  /** Whether the last packedString expanded into the writer rather than
   * returning a view, and where in it the expansion began. */
  packedIntoOut: bool = false
  packedFrom: i32 = 0

  /** The field's bytes, as a view onto the message rather than a copy. */
  bytes(): Uint8Array {
    const size = this.blobSize()
    if (!this.ok) return new Uint8Array(0)
    const start = this.blobStart
    if (size > this.buf.length - start) {
      this.fail(W_TRUNCATED)
      return new Uint8Array(0)
    }
    this.at = start + size
    return this.buf.subarray(start, start + size)
  }

  /** Where blobSize() left the payload. AssemblyScript has no tuple return. */
  private blobStart: i32 = 0

  /** A blob or string-array header's declared size, with blobStart set past it. */
  private blobSize(): i32 {
    const header = this.header()
    if (!this.ok) return 0
    if ((header & MORE_SIZE_FLAG) == 0) {
      if (this.at + 2 > this.buf.length) {
        this.fail(W_TRUNCATED)
        return 0
      }
      this.blobStart = this.at + 2
      return (<i32>(header & 0b111) << 8) | <i32>unchecked(this.buf[this.at + 1])
    }
    return this.escapedSize(header & 0b111)
  }

  /**
   * The wide size behind a header whose more flag is set. Its width is named by
   * the header rather than discovered byte by byte, so the only thing refused
   * here is a size this platform cannot address.
   */
  private escapedSize(escape: u8): i32 {
    let width = 0
    if (escape == ESCAPE_2_BYTES) width = 2
    else if (escape == ESCAPE_4_BYTES) width = 4
    else if (escape == ESCAPE_8_BYTES) width = 8
    else {
      this.fail(W_BAD_ESCAPE)
      return 0
    }
    if (this.at + 1 + width > this.buf.length) {
      this.fail(W_TRUNCATED)
      return 0
    }
    const value = leUintAt(this.buf, this.at + 1, width)
    if (value > MAX_SIZE) {
      this.fail(W_SIZE_TOO_LARGE)
      return 0
    }
    this.blobStart = this.at + 1 + width
    return <i32>value
  }

  // ---- vectors and string arrays --------------------------------------------

  vec(): Vec {
    const out = new Vec()
    const header = this.header()
    if (!this.ok) return out
    const count = this.arrayCount(header)
    if (!this.ok) return out
    const start = this.blobStart
    const width = 1 << ((header >> ARRAY_WIDTH_SHIFT) & 0b11)
    if (count < 0 || count > (this.buf.length - start) / width) {
      this.fail(W_TRUNCATED)
      return out
    }
    this.at = start + count * width
    out.elements = this.buf.subarray(start, start + count * width)
    out.width = width
    out.positive = (header & ARRAY_POSITIVE_FLAG) != 0
    out.ok = true
    return out
  }

  /** An integer array's element count, with blobStart set just past the header. */
  private arrayCount(header: u8): i32 {
    if ((header & MORE_ARRAY_LEN_FLAG) == 0) {
      if (this.at + 2 > this.buf.length) {
        this.fail(W_TRUNCATED)
        return 0
      }
      this.blobStart = this.at + 2
      return <i32>unchecked(this.buf[this.at + 1])
    }
    if (this.at + 5 > this.buf.length) {
      this.fail(W_TRUNCATED)
      return 0
    }
    this.blobStart = this.at + 5
    return <i32>leUintAt(this.buf, this.at + 1, 4)
  }

  /** Each element of a string array, as views onto the message. */
  strings(): Array<Uint8Array> {
    const out = new Array<Uint8Array>()
    const count = this.blobSize()
    if (!this.ok) return out
    let at = this.blobStart
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

  // ---- composites -----------------------------------------------------------

  /** A nested run's bytes and the key width it uses. */
  structBody(): Body {
    const out = new Body()
    if (this.at >= this.buf.length) {
      this.fail(W_TRUNCATED)
      return out
    }
    out.wide = (unchecked(this.buf[this.at]) & STRUCT_WIDE_KEYS) != 0
    out.bytes = this.compositeBody()
    out.ok = this.ok
    return out
  }

  /** Whether the field at the cursor is a table rather than a list or a map. */
  @inline isTable(): bool {
    return this.at < this.buf.length && (unchecked(this.buf[this.at]) & TABLE_FLAG) != 0
  }

  /** Whether a nested table's columns are keyed at eight bits. */
  @inline tableIsWide(): bool {
    return this.at < this.buf.length && (unchecked(this.buf[this.at]) & STRUCT_WIDE_KEYS) != 0
  }

  /** A narrow composite's length, with its body as a view and the cursor past it. */
  compositeBody(): Uint8Array {
    if (this.at + 2 > this.buf.length) {
      this.fail(W_TRUNCATED)
      return new Uint8Array(0)
    }
    const width = unchecked(LENGTH_WIDTH[unchecked(this.buf[this.at]) & 0b11])
    if (this.at + 1 + width > this.buf.length) {
      this.fail(W_TRUNCATED)
      return new Uint8Array(0)
    }
    const length = leUintAt(this.buf, this.at + 1, width)
    if (length > MAX_SIZE) {
      this.fail(W_SIZE_TOO_LARGE)
      return new Uint8Array(0)
    }
    const start = this.at + 1 + width
    if (<i32>length > this.buf.length - start) {
      this.fail(W_TRUNCATED)
      return new Uint8Array(0)
    }
    this.at = start + <i32>length
    return this.buf.subarray(start, start + <i32>length)
  }

  /**
   * The count at the head of a composite's body — a list's elements, a map's
   * entries, a table's rows — written the way this format writes every count:
   * one byte, escaping to four behind 0xFF.
   */
  countPrefix(): i32 {
    const value = this.elementSize(this.at)
    if (!this.ok) return 0
    this.at = this.elementStart
    return value
  }

  /** One narrow list element's body: a length and then a key run. */
  element(): Uint8Array {
    const size = this.elementSize(this.at)
    if (!this.ok) return new Uint8Array(0)
    const start = this.elementStart
    if (size > this.buf.length - start) {
      this.fail(W_TRUNCATED)
      return new Uint8Array(0)
    }
    this.at = start + size
    return this.buf.subarray(start, start + size)
  }

  /** A key-less unsigned value, which is what a narrow map's entries are made of. */
  elementUint(): u64 {
    if (this.at >= this.buf.length) {
      this.fail(W_TRUNCATED)
      return 0
    }
    const code = unchecked(this.buf[this.at]) & 0b1111
    if (code <= UINT_INLINE_MAX) {
      this.at++
      return <u64>code
    }
    const width = <i32>(code - UINT_WIDTH_BASE) + 1
    if (this.at + 1 + width > this.buf.length) {
      this.fail(W_TRUNCATED)
      return 0
    }
    const value = leUintAt(this.buf, this.at + 1, width)
    this.at += 1 + width
    return value
  }

  elementInt(): i64 {
    if (this.at >= this.buf.length) {
      this.fail(W_TRUNCATED)
      return 0
    }
    const positive = (unchecked(this.buf[this.at]) & INT_POSITIVE_FLAG) != 0
    const magnitude = this.elementMagnitude()
    return positive ? <i64>magnitude : -(<i64>magnitude)
  }

  /**
   * signedMagnitude for a key-less element. It exists for the same reason
   * elementInt does: the signed and unsigned nibbles are different tables over
   * the same four bits, and a map value of zero is legitimate where a field's
   * would simply be omitted.
   */
  private elementMagnitude(): u64 {
    if (this.at >= this.buf.length) {
      this.fail(W_TRUNCATED)
      return 0
    }
    const width = unchecked(MAGNITUDE_WIDTH[unchecked(this.buf[this.at]) & 0b111])
    if (this.at + 1 + width > this.buf.length) {
      this.fail(W_TRUNCATED)
      return 0
    }
    const value = width == 0 ? <u64>1 : leUintAt(this.buf, this.at + 1, width)
    this.at += 1 + width
    return value
  }

  elementBytes(): Uint8Array {
    if (this.at >= this.buf.length) {
      this.fail(W_TRUNCATED)
      return new Uint8Array(0)
    }
    const width = unchecked(LENGTH_WIDTH[unchecked(this.buf[this.at]) & 0b11])
    if (this.at + 1 + width > this.buf.length) {
      this.fail(W_TRUNCATED)
      return new Uint8Array(0)
    }
    const size = leUintAt(this.buf, this.at + 1, width)
    const start = this.at + 1 + width
    if (size > MAX_SIZE || <i32>size > this.buf.length - start) {
      this.fail(W_TRUNCATED)
      return new Uint8Array(0)
    }
    this.at = start + <i32>size
    return this.buf.subarray(start, start + <i32>size)
  }

  @inline get moreElements(): bool {
    return this.err == W_OK && this.at < this.buf.length
  }

  /**
   * A column, for a narrow-keyed table. The row count comes from the table,
   * which said it once for every column, and the element width from the schema.
   */
  column(rows: i32, out: Int64Array, width: i32): bool {
    const body = this.compositeBody()
    if (!this.ok) return false
    if (!decodeArray(new Reader(body), rows, out, width)) {
      this.fail(W_BAD_COLUMN)
      return false
    }
    return true
  }

  /**
   * Refused rather than guessed. The header says how wide a field is only once
   * the reader knows which of the four layouts it is reading, and that comes
   * from the key — so an unknown key is a record definition the two sides no
   * longer share.
   */
  skip(): void {
    this.fail(W_NO_SKIP)
  }
}
