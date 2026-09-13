// The K4 writer: port of wire/narrow.go's and narrow_composite.go's write half.
//
// It is a separate file from the reader for the reason Go keeps the two key
// widths apart — the two halves share nothing but the constants — and because
// the encoder arrived four phases after the decoder did.
//
// A write cannot fail. Sizes escalate rather than cap, so nothing a caller can
// hold in memory is too large to describe, and a key is a constant of the
// record definition rather than data. The reader defends against the network;
// the writer trusts its own program.

import { Writer } from '../bytes'
import { appendArray } from '../column'
import { appendPayload, packedSize, packedUpper } from '../packed5'
import {
  ARRAY_POSITIVE_FLAG,
  ELEMENT_SIZE_ESCAPE,
  ESCAPE_2_BYTES,
  ESCAPE_4_BYTES,
  ESCAPE_8_BYTES,
  ESCAPE_PACKED_1_LO,
  ESCAPE_PACKED_1_UP,
  ESCAPE_PACKED_4_LO,
  ESCAPE_PACKED_4_UP,
  INLINE_ELEMENT_SIZE,
  INT_POSITIVE_FLAG,
  MORE_ARRAY_LEN_FLAG,
  MORE_SIZE_FLAG,
  STRUCT_WIDE_KEYS,
  TABLE_FLAG,
  UINT_INLINE_MAX,
  UINT_WIDTH_BASE,
  arrayWidthCode,
  magnitudeBytes,
  sizeCodeFor,
  widthOfCode,
} from './desc'

/** What a narrow blob header carries before an escape is needed. */
const INLINE_BLOB_SIZE: i32 = (1 << 11) - 1
/** What an array header's inline count holds. */
const INLINE_ARRAY_COUNT: i32 = (1 << 8) - 1
/** What a composite's reserved length byte holds before it has to widen. */
const INLINE_COMPOSITE_LENGTH: i32 = 0xff

export class NarrowWriter {
  out: Writer

  constructor(out: Writer) {
    this.out = out
  }

  @inline private byte(b: u8): void {
    this.out.writeByte(b)
  }

  // ---- integers -------------------------------------------------------------

  /**
   * An unsigned field, and nothing at all when it is zero.
   *
   * All sixteen nibble codes carry information here, because an unsigned narrow
   * field spends no sign bit: 0..7 is the value itself with no payload, and
   * 8..15 is a magnitude of one to eight bytes. That is what makes a bool, a
   * small count or a flag a single byte, key included.
   */
  uint(key: u8, value: u64): void {
    if (value == 0) return
    if (value <= <u64>UINT_INLINE_MAX) {
      this.byte((key << 4) | <u8>value)
      return
    }
    const width = magnitudeBytes(value)
    this.byte((key << 4) | (UINT_WIDTH_BASE + <u8>width - 1))
    this.out.writeLE(value, width)
  }

  /** A signed field as a sign bit and a magnitude, and nothing when it is zero. */
  int(key: u8, value: i64): void {
    if (value == 0) return
    let header = (key << 4) | INT_POSITIVE_FLAG
    let magnitude = <u64>value
    if (value < 0) {
      header = key << 4
      // Negating through u64 rather than i64 keeps the most negative value,
      // whose positive counterpart does not exist as an i64.
      magnitude = 0 - <u64>value
    }
    this.magnitude(header, magnitude)
  }

  private magnitude(header: u8, magnitude: u64): void {
    if (magnitude == 1) {
      // Size code 0 carries no bytes and means the magnitude is one.
      this.byte(header)
      return
    }
    const code = sizeCodeFor(magnitude)
    this.byte(header | <u8>code)
    this.out.writeLE(magnitude, widthOfCode(code))
  }

  /** One byte when true and nothing when false: true is the inline value one. */
  bool(key: u8, value: bool): void {
    if (value) this.byte((key << 4) | 1)
  }

  /**
   * A float as its byte-reversed IEEE-754 bit pattern.
   *
   * A float's zero bytes are its low mantissa bytes where an integer's are its
   * high ones, so reversing puts them where the width can elide them: 1.0 costs
   * two bytes rather than eight.
   */
  f64(key: u8, value: f64): void {
    this.uint(key, bswap<u64>(reinterpret<u64>(value)))
  }

  f32(key: u8, value: f32): void {
    this.uint(key, <u64>bswap<u32>(reinterpret<u32>(value)))
  }

  /** An explicit zero in the unsigned form, for a field the omit rule would drop. */
  zeroUnsigned(key: u8): void {
    this.byte(key << 4)
  }

  /** An explicit zero in the *signed* form. The two nibbles are different tables,
   * so a zero has to be written in the one its reader will use. */
  zeroSigned(key: u8): void {
    this.byte((key << 4) | INT_POSITIVE_FLAG | 1)
    this.byte(0)
  }

  emptyString(key: u8): void {
    this.byte(key << 4)
    this.byte(0)
  }

  // ---- blobs ----------------------------------------------------------------

  blob(key: u8, value: Uint8Array): void {
    if (value.length == 0) return
    this.blobHeader(key, value.length)
    this.out.writeBytes(value, 0, value.length)
  }

  /** Two bytes holding an eleven-bit size, or one byte and a wider size when
   * that will not hold it. */
  private blobHeader(key: u8, size: i32): void {
    if (size <= INLINE_BLOB_SIZE) {
      this.byte((key << 4) | (<u8>(size >> 8) & 0b111))
      this.byte(<u8>size)
      return
    }
    // Past 2047 the three size bits carry nothing, so they name the width of
    // the size that follows instead: a 5 KB string pays one extra byte.
    if (size <= 0xffff) {
      this.byte((key << 4) | MORE_SIZE_FLAG | ESCAPE_2_BYTES)
      this.out.writeLE(<u64>size, 2)
    } else {
      this.byte((key << 4) | MORE_SIZE_FLAG | ESCAPE_4_BYTES)
      this.out.writeLE(<u64>size, 4)
    }
  }

  /**
   * A string in the packed encoding when that is smaller than the raw bytes, and
   * raw when it is not. Nothing is written for an empty string.
   *
   * The narrow header has no `enc` field, so the encoding rides in the blob
   * header's escape code. That keeps a packed string's header at two bytes, the
   * same as a raw one's, and is what lets the encoding run under four-bit keys
   * at all: before it, packed5 forced a message to eight-bit keys and cost a
   * byte on every field rather than on the strings.
   *
   * Choosing per string rather than per message is what keeps the encoding from
   * ever costing anything: a token that does not pack is written raw and the
   * header says so.
   */
  packedString(key: u8, value: Uint8Array): void {
    if (value.length == 0) return
    // The payload's length is not known until it is written, so two header bytes
    // are reserved and filled in after. A payload past 255 bytes needs three
    // more, which shifts it up — a memmove on a value already large enough to
    // have earned one.
    const start = this.out.len
    this.byte((key << 4) | MORE_SIZE_FLAG)
    this.byte(0)
    if (!appendPayload(this.out, value, 0, value.length)) {
      this.out.len = start
      this.blob(key, value)
      return
    }
    const size = packedSize
    if (size <= 0xff) {
      const code = packedUpper ? ESCAPE_PACKED_1_UP : ESCAPE_PACKED_1_LO
      this.out.setByte(start, (key << 4) | MORE_SIZE_FLAG | code)
      this.out.setByte(start + 1, <u8>size)
      return
    }
    const code = packedUpper ? ESCAPE_PACKED_4_UP : ESCAPE_PACKED_4_LO
    this.out.insert(start + 2, 3)
    this.out.setByte(start, (key << 4) | MORE_SIZE_FLAG | code)
    this.out.setLE(start + 1, <u64>size, 4)
  }

  // ---- arrays ---------------------------------------------------------------

  /**
   * An integer array at one width, chosen from the widest element.
   *
   * `signed` decides whether an element that reads as negative is written as a
   * two's complement value or as a magnitude, and it comes from the op rather
   * than from the values — a `[]uint64` past 2^63 reads as negative through an
   * i64 and must still travel as two's complement, which is lossless and is
   * what Go does.
   */
  ints(key: u8, values: Int64Array, count: i32, signed: bool): void {
    if (count == 0) return
    let allPositive = true
    let widest: u64 = 0
    for (let index = 0; index < count; index++) {
      const value = unchecked(values[index])
      if (signed && value < 0) {
        allPositive = false
        const magnitude = 0 - <u64>value
        if (magnitude > widest) widest = magnitude
        continue
      }
      if (<u64>value > widest) widest = <u64>value
    }
    const code = arrayWidthCode(widest, allPositive)
    const width = 1 << code
    let header = (key << 4) | (<u8>code << 1)
    if (allPositive) header |= ARRAY_POSITIVE_FLAG
    if (count <= INLINE_ARRAY_COUNT) {
      this.byte(header)
      this.byte(<u8>count)
    } else {
      this.byte(header | MORE_ARRAY_LEN_FLAG)
      this.out.writeLE(<u64>count, 4)
    }
    for (let index = 0; index < count; index++) {
      this.out.writeLE(<u64>unchecked(values[index]), width)
    }
  }

  /** A count and then each element behind its own length. */
  strings(key: u8, values: Array<Uint8Array>): void {
    if (values.length == 0) return
    this.blobHeader(key, values.length)
    for (let index = 0; index < values.length; index++) {
      this.element(unchecked(values[index]))
    }
  }

  private element(value: Uint8Array): void {
    if (value.length <= INLINE_ELEMENT_SIZE) {
      this.byte(<u8>value.length)
    } else {
      this.byte(ELEMENT_SIZE_ESCAPE)
      this.out.writeLE(<u64>value.length, 4)
    }
    this.out.writeBytes(value, 0, value.length)
  }

  // ---- composites -----------------------------------------------------------

  private openComposite(key: u8, detail: u8): i32 {
    this.byte((key << 4) | detail)
    this.byte(0)
    return this.out.len - 1
  }

  openStruct(key: u8, wideInside: bool): i32 {
    return this.openComposite(key, wideInside ? STRUCT_WIDE_KEYS : 0)
  }

  openList(key: u8, count: i32): i32 {
    const mark = this.openComposite(key, 0)
    this.count(count)
    return mark
  }

  openTable(key: u8, rows: i32): i32 {
    const mark = this.openComposite(key, TABLE_FLAG)
    this.count(rows)
    return mark
  }

  /** One list element: a length and a body, with no descriptor between them. */
  openElement(): i32 {
    this.byte(0)
    return this.out.len - 1
  }

  private count(value: i32): void {
    if (value <= INLINE_ELEMENT_SIZE) {
      this.byte(<u8>value)
      return
    }
    this.byte(ELEMENT_SIZE_ESCAPE)
    this.out.writeLE(<u64>value, 4)
  }

  /** Patches a keyed composite's length, widening the placeholder when the body
   * outgrew it. */
  close(mark: i32): void {
    const body = this.out.len - (mark + 1)
    if (body < INLINE_COMPOSITE_LENGTH) {
      this.out.setByte(mark, <u8>body)
      return
    }
    this.out.insert(mark + 1, 3)
    this.out.setLE(mark, <u64>body, 4)
    // lw code 2: a four-byte length, in the descriptor before the placeholder.
    this.out.setByte(mark - 1, this.out.buf[mark - 1] | 2)
  }

  /**
   * Patches a list element's length, which is *not* close().
   *
   * An element has no descriptor in front of it — that missing byte is what
   * makes a narrow list of small structs cheaper than a wide one — so there is
   * nowhere to put a wider length code and the escape goes in the length's own
   * first byte instead. Getting this wrong is a message the writer produces and
   * the reader refuses; see wire/narrow_composite.go's CloseElement, which had
   * exactly that bug until this port needed the same function and found it.
   */
  closeElement(mark: i32): void {
    const body = this.out.len - (mark + 1)
    if (body <= INLINE_ELEMENT_SIZE) {
      this.out.setByte(mark, <u8>body)
      return
    }
    this.out.insert(mark + 1, 4)
    this.out.setByte(mark, ELEMENT_SIZE_ESCAPE)
    this.out.setLE(mark + 1, <u64>body, 4)
  }

  // ---- columns --------------------------------------------------------------

  /** An integer column, and nothing at all when every value is zero: an absent
   * column key is the whole of what says so. */
  column(key: u8, values: Int64Array, count: i32, width: i32): void {
    if (count == 0) return
    let allZero = true
    for (let index = 0; index < count; index++) {
      if (unchecked(values[index]) != 0) {
        allZero = false
        break
      }
    }
    if (allZero) return
    const mark = this.openComposite(key, 0)
    appendArray(this.out, values, count, width)
    this.close(mark)
  }

  /** A string column, which is a list of blobs: strings have no residual to
   * transform, so a column of them is the shape a list of them is. */
  stringColumn(key: u8, values: Array<Uint8Array>): void {
    for (let index = 0; index < values.length; index++) {
      if (unchecked(values[index]).length != 0) {
        this.strings(key, values)
        return
      }
    }
  }
}
