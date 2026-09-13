// The K8 writer: port of wire/wide.go's, composite.go's and table.go's write
// half.
//
// Every field spends a byte on its key and a byte on a descriptor that names its
// class, and what that buys is skip: a reader that has never heard of a key can
// still step over the field. The writer's own job is one thing more than the
// narrow one's — choosing between the two integer forms.

import { Writer } from '../bytes'
import { appendArray } from '../column'
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
  INLINE_ELEMENT_SIZE,
  INT_POSITIVE_FLAG,
  LENGTH_WIDTH,
  LIST_HOMOGENEOUS,
  SPECIAL_VARINT,
  STRUCT_WIDE_KEYS,
  TABLE_FLAG,
  VARINT_BITS,
  arrayWidthCode,
  lengthCodeFor,
  magnitudeBytes,
  sizeCodeFor,
  varintLen,
  widthOfCode,
  zigzag,
} from './desc'

/** The largest integer a descriptor can be on its own. */
const MAX_INLINE_VALUE: u64 = 0x7f

const INLINE_COMPOSITE_LENGTH: i32 = 0xff

@inline
function descriptor(class_: u8, detail: u8): u8 {
  return DESC_EXPLICIT | (class_ << 4) | detail
}

export class WideWriter {
  out: Writer

  constructor(out: Writer) {
    this.out = out
  }

  @inline private byte(b: u8): void {
    this.out.writeByte(b)
  }

  // ---- integers -------------------------------------------------------------

  /**
   * An unsigned field, as a varint or as a byte count, whichever is shorter.
   *
   * A descriptor whose top bit is clear *is* the value, 0..127, so a small
   * integer costs two bytes — the same as under four-bit keys, where it costs a
   * header byte and a value byte. That is why the wide key is a byte worse only
   * above 127 rather than always.
   */
  uint(key: u8, value: u64): void {
    if (value == 0) return
    if (value <= MAX_INLINE_VALUE) {
      this.byte(key)
      this.byte(<u8>value)
      return
    }
    const code = sizeCodeFor(value)
    const width = widthOfCode(code)
    // An unsigned value is not zigzagged: it has no sign to fold, and folding it
    // would cost a bit for nothing.
    if (varintLen(value) < 2 + width) {
      this.varint(key, value)
      return
    }
    this.byte(key)
    this.byte(descriptor(CLASS_INT, INT_POSITIVE_FLAG | <u8>code))
    this.out.writeLE(value, width)
  }

  /**
   * A signed field, as a zigzag varint or as a sign and a magnitude, whichever
   * is shorter.
   *
   * A positive value does not go through uint(): the varint under SPECIAL is raw
   * when uint wrote it and zigzagged when this did. The schema picks the reader,
   * so the two never meet — but only if each writer stays on its own side.
   */
  int(key: u8, value: i64): void {
    if (value == 0) return
    if (value > 0 && <u64>value <= MAX_INLINE_VALUE) {
      this.byte(key)
      this.byte(<u8>value)
      return
    }
    let magnitude = <u64>value
    let detail: u8 = INT_POSITIVE_FLAG
    if (value < 0) {
      magnitude = 0 - <u64>value
      detail = 0
    }
    const code = sizeCodeFor(magnitude)
    const width = widthOfCode(code)
    const folded = zigzag(value)
    if (varintLen(folded) < 2 + width) {
      this.varint(key, folded)
      return
    }
    this.byte(key)
    this.byte(descriptor(CLASS_INT, detail | <u8>code))
    this.out.writeLE(magnitude, width)
  }

  /** Three value bits in the descriptor, then seven per byte after it. */
  private varint(key: u8, value: u64): void {
    this.byte(key)
    this.byte(descriptor(CLASS_SPECIAL, SPECIAL_VARINT | <u8>(value & 0b111)))
    let rest = value >> <u64>VARINT_BITS
    while (rest >= 0x80) {
      this.byte(<u8>rest | 0x80)
      rest >>= 7
    }
    this.byte(<u8>rest)
  }

  /** Two bytes when true and nothing when false: true rides in the inline form. */
  bool(key: u8, value: bool): void {
    if (!value) return
    this.byte(key)
    this.byte(1)
  }

  f64(key: u8, value: f64): void {
    this.uint(key, bswap<u64>(reinterpret<u64>(value)))
  }

  f32(key: u8, value: f32): void {
    this.uint(key, <u64>bswap<u32>(reinterpret<u32>(value)))
  }

  zeroUnsigned(key: u8): void {
    this.byte(key)
    this.byte(0)
  }

  /** The signed zero is the same two bytes: the inline form carries no sign, and
   * int() reads a descriptor under 0x80 as the value. */
  zeroSigned(key: u8): void {
    this.zeroUnsigned(key)
  }

  emptyString(key: u8): void {
    this.byte(key)
    this.byte(descriptor(CLASS_BLOB, 0))
    this.byte(0)
  }

  // ---- blobs ----------------------------------------------------------------

  blob(key: u8, value: Uint8Array): void {
    if (value.length == 0) return
    const code = lengthCodeFor(<u64>value.length)
    this.byte(key)
    this.byte(descriptor(CLASS_BLOB, <u8>code))
    this.out.writeLE(<u64>value.length, unchecked(LENGTH_WIDTH[code]))
    this.out.writeBytes(value, 0, value.length)
  }

  // ---- arrays ---------------------------------------------------------------

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
    const widthCode = arrayWidthCode(widest, allPositive)
    const width = 1 << widthCode
    const byteLength = count * width
    // A vector's length is in bytes, not elements, so a reader that does not
    // know the key can still skip it. The count is byteLength >> widthCode and
    // never goes on the wire.
    let lengthCode: u8 = 0
    let lengthBytes = 1
    if (byteLength > 0xff) {
      lengthCode = 1
      lengthBytes = 4
    }
    let detail = (<u8>widthCode << 2) | lengthCode
    if (allPositive) detail |= 0b10
    this.byte(key)
    this.byte(descriptor(CLASS_VEC, detail))
    this.out.writeLE(<u64>byteLength, lengthBytes)
    for (let index = 0; index < count; index++) {
      this.out.writeLE(<u64>unchecked(values[index]), width)
    }
  }

  /** A count and then each element behind its own length, inside a byte length
   * that lets the whole field be skipped. */
  strings(key: u8, values: Array<Uint8Array>): void {
    if (values.length == 0) return
    let payload = 0
    for (let index = 0; index < values.length; index++) {
      const size = unchecked(values[index]).length
      payload += size <= INLINE_ELEMENT_SIZE ? 1 + size : 5 + size
    }
    // The declared length covers the count as well as the elements, which is
    // what makes a skip uniform across every composite.
    const counted = payload + (values.length <= INLINE_ELEMENT_SIZE ? 1 : 5)
    const code = lengthCodeFor(<u64>counted)
    this.byte(key)
    this.byte(descriptor(CLASS_LIST, LIST_HOMOGENEOUS | <u8>code))
    this.out.writeLE(<u64>counted, unchecked(LENGTH_WIDTH[code]))
    this.count(values.length)
    for (let index = 0; index < values.length; index++) {
      const value = unchecked(values[index])
      if (value.length <= INLINE_ELEMENT_SIZE) {
        this.byte(<u8>value.length)
      } else {
        this.byte(ELEMENT_SIZE_ESCAPE)
        this.out.writeLE(<u64>value.length, 4)
      }
      this.out.writeBytes(value, 0, value.length)
    }
  }

  // ---- composites -----------------------------------------------------------

  private openComposite(key: u8, class_: u8, detail: u8): i32 {
    this.byte(key)
    this.byte(descriptor(class_, detail))
    this.byte(0)
    return this.out.len - 1
  }

  openStruct(key: u8, wideInside: bool): i32 {
    return this.openComposite(key, CLASS_STRUCT, wideInside ? STRUCT_WIDE_KEYS : 0)
  }

  openList(key: u8, count: i32): i32 {
    const mark = this.openComposite(key, CLASS_LIST, 0)
    this.count(count)
    return mark
  }

  openTable(key: u8, rows: i32): i32 {
    const mark = this.openComposite(key, CLASS_MAP, TABLE_FLAG)
    this.count(rows)
    return mark
  }

  /** A struct as a list element: a descriptor and a length, but no key. */
  openElementStruct(wideInside: bool): i32 {
    this.byte(descriptor(CLASS_STRUCT, wideInside ? STRUCT_WIDE_KEYS : 0))
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

  close(mark: i32): void {
    const body = this.out.len - (mark + 1)
    if (body < INLINE_COMPOSITE_LENGTH) {
      this.out.setByte(mark, <u8>body)
      return
    }
    this.out.insert(mark + 1, 3)
    this.out.setLE(mark, <u64>body, 4)
    this.out.setByte(mark - 1, this.out.buf[mark - 1] | 2)
  }

  // ---- columns --------------------------------------------------------------

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
    const mark = this.openComposite(key, CLASS_COL, 0)
    appendArray(this.out, values, count, width)
    this.close(mark)
  }

  stringColumn(key: u8, values: Array<Uint8Array>): void {
    for (let index = 0; index < values.length; index++) {
      if (unchecked(values[index]).length != 0) {
        this.strings(key, values)
        return
      }
    }
  }
}

/** magnitudeBytes is re-exported so the encoder can size a field without
 * reaching past this layer. */
export { magnitudeBytes }
