// A message → JavaScript values, against a schema section. Mirrors the walk in
// `rust/src/walk.rs` (itself `codec/json.go`), producing values rather than
// JSON text. Go is the specification: INTERNALS.md §2–§13.
//
// One Walker reads one message. Its cursor is `p`, the end of the run being
// read is `e`, and a nested body narrows `e` for its duration: there is no
// sub-reader to allocate per struct. A failure throws, so nothing here checks
// an error flag. A narrow run (four-bit keys, §3) and a wide one (one-byte
// keys, §4) go through the same methods, told apart by a `wide` argument.
//
// The values are what `JSON.parse(codec.toJSONText(bytes))` gives, except where
// JSON text loses something: an integer past 2^53 is a bigint, a []byte is a
// Uint8Array, NaN and ±Inf are kept. Every field the schema lists is present,
// an absent one holding its zero (§12), in the schema's order.

import {
  fail, TRUNCATED, BAD_DESCRIPTOR, TOO_WIDE, TOO_DEEP, TOO_MANY_ROWS, BAD_SECTION, BAD_TABLE, BAD_DICTIONARY,
} from './fail.ts'
import {
  type Schema, type Plan, type Field,
  OP_BOOL, OP_INT8, OP_INT32, OP_INT64, OP_UINT64,
  OP_FLOAT32, OP_FLOAT64, OP_STRING, OP_BYTES, OP_INT8S, OP_INT64S,
  OP_UINT64S, OP_STRINGS, OP_STRUCT, OP_STRUCTS, OP_MAP, OP_POINTER,
  OP_ANY, OP_ANYS, OP_POINTER_STRUCT,
  MAP_STRING, MAP_INT, MAP_UINT, MAP_FLOAT64, MAP_BOOL, MAP_FLOAT32, MAP_ANY, MAP_STRUCT,
  MAP_STRUCTS, MAP_LIST, MAX_DEPTH,
} from './schema.ts'
import { key, lastKey, lengthUnits, utf8, wellFormedUnits, wholeRun } from './strings.ts'
import { object } from './shapes.ts'
import { packed5 } from './packed5.ts'
import { decodeInts, type Ints, pairsToFloat64, pairsToBig, pairsSafe, pairsToNum } from './column.ts'
import { arrayFactory, rowBuilder, put, K_CONST, K_VAL, K_BOOL, K_DICT, type RowColumn } from './build.ts'

/** The most rows one table may claim (`maxTableRows`). */
const MAX_ROWS = 1 << 22
/** How many list columns deep a table may go. */
const MAX_LIST_DEPTH = 4
const TWO32 = 4294967296
const SAFE_HI = 0x200000

// Narrow nibbles (§3.1).
const NIB_INLINE_LAST = 3
const NIB_LENGTH = 12
const NIB_NULLABLE = 14
const NIB_DICT = 13
const NIB_NULLABLE_DICT = 15

// Wide classes (§4.1).
const CLASS_INT = 0
const CLASS_BLOB = 1
const CLASS_VEC = 2
const CLASS_COL = 3
const CLASS_LIST = 4
const CLASS_STRUCT = 5
const CLASS_MAP = 6
const CLASS_SPECIAL = 7
const SPECIAL_VARINT = 0x08
const LENGTH_WIDTH = [1, 2, 4, 8]
const MAGNITUDE_WIDTH = [0, 1, 2, 3, 4, 5, 6, 8]
/** A narrow integer of op k (k ≤ OP_UINT32) is below this, and for a signed
 * one at least its negative. */
const LIMIT = [0, 0x80, 0x8000, 0x80000000, 0, 0x100, 0x10000, TWO32]

// Dynamic kinds (wire/dynamic.rs `Kind`).
const KIND_INVALID = 0
const KIND_NULL = 1
const KIND_BOOL = 2
const KIND_INT = 3
const KIND_FLOAT64 = 4
const KIND_FLOAT32 = 5
const KIND_STRING = 6
const KIND_BYTES = 7
const KIND_LIST = 8
const KIND_STRUCT = 9
const KIND_MAP = 10
const KIND_TABLE = 11
const KIND_TYPED = 12
/** A descriptor's kind by its class, and a SPECIAL one's by its detail. */
const CLASS_KIND = [KIND_INT, KIND_STRING, KIND_INVALID, KIND_INVALID, KIND_LIST, KIND_STRUCT, KIND_MAP, KIND_INVALID]
const SPECIAL_KIND = [KIND_NULL, KIND_BOOL, KIND_BOOL, KIND_FLOAT64, KIND_FLOAT32, KIND_BYTES, KIND_INVALID, KIND_TYPED]

// What items() reads each element as.
const ITEM_COMPACT = 0
const ITEM_STRUCT = 1
const ITEM_MAP_VALUE = 2

/** A field type the section puts where the walk cannot render it. */
const UNWALKABLE = BAD_SECTION

export type Options = {
  /** `'auto'` (the default): an integer past 2^53 is a bigint, per column in a
   * table. `'number'`: always a Number, rounded as JSON.parse would. */
  int64?: 'auto' | 'number'
}

/** One table column, gathered before the rows are built. */
type Col = {
  present: boolean
  /** How a row reads it, and the arrays it reads (see build.ts). */
  kind: number
  values: unknown
  dict: unknown
  /** A paged row type's next page, by its link's field index. */
  page: Col[] | null
  /** A nullable column's presence bitmap; `values` then holds the present
   * rows only, spread over the rows when they are built. */
  valid: Uint8Array | null
}

const scratch = new DataView(new ArrayBuffer(8))

/** The shortest decimal that reads back as this float32, which is what Go
 * prints for one (strconv.FormatFloat(f, 'g', -1, 32)). */
export function shortest32(x: number): number {
  if (x === 0 || x !== x || x === Infinity || x === -Infinity) return x
  for (let p = 1; p < 10; p++) {
    const y = Number(x.toPrecision(p))
    if (Math.fround(y) === x) return y
  }
  return x
}

/** What an absent field of op `op` holds (§12); a by-value struct is built. */
function zeroOf(op: number): unknown {
  return op === OP_BOOL ? false : op <= OP_FLOAT64 ? 0 : op === OP_STRING ? '' : null
}

export class Walker {
  #b: Uint8Array
  #p = 0
  #e = 0
  #schema: Schema
  #plans: Plan[]
  #depth = 0
  #rowsLeft: number
  /** A dictionary-section message's dictionaries, decoded once. */
  #dicts: string[][] | null = null
  #bigint: boolean

  // A narrow field's nibble and payload start, from nhead().
  #nib = 0
  #fs = 0
  // An integer's two lanes and, from the wide INT form, its sign.
  #lo = 0
  #hi = 0
  #neg = false
  // A wide length's payload start, from declared().
  #ls = 0

  constructor(b: Uint8Array, schema: Schema, options: Options | undefined) {
    this.#b = b
    this.#e = b.length
    this.#schema = schema
    this.#plans = schema.plans
    this.#rowsLeft = MAX_ROWS + 64 * b.length
    this.#bigint = options?.int64 !== 'number'
  }

  #enter(): void {
    if (this.#depth >= MAX_DEPTH) fail(TOO_DEEP, this.#p)
    this.#depth++
  }

  #plan(at: number): Plan {
    const plan = this.#plans[at]
    if (plan === undefined) fail(BAD_SECTION, this.#p)
    return plan
  }

  #takeRows(rows: number, columns: number): void {
    const cells = rows * Math.max(columns, 1)
    if (rows > MAX_ROWS || cells > this.#rowsLeft) fail(TOO_MANY_ROWS, this.#p)
    this.#rowsLeft -= cells
  }

  // ---- the root -------------------------------------------------------------

  /** The root run at `[start, end)`, where an envelope is unwrapped. */
  root(start: number, end: number, wide: boolean): unknown {
    const plan = this.#plan(0)
    this.#p = start
    this.#e = end
    if (!plan.envelope || plan.fields.length !== 1) return this.#record(0, start, end, wide)
    this.#enter()
    const field = plan.fields[0]
    const p = this.#p
    const here = wide ? p + 1 < end && this.#b[p] === field.key : p < end && this.#b[p] >> 4 === field.key
    const value = here ? this.#value(field, wide) : this.#zero(field)
    this.#depth--
    return value
  }

  /**
   * The dictionary section at `at` (INTERNALS.md §6.3), decoded for the
   * dictionary columns; returns where the body starts.
   *
   *   section := [byteLength] [dictCount] ( [entryCount] ( [size] [bytes] )* )*
   */
  dictionaries(at: number): number {
    const b = this.#b
    const size = this.#count(at, b.length)
    const end = this.#p + size
    if (size > b.length - this.#p) fail(BAD_DICTIONARY, at)
    const dicts: string[][] = []
    for (let d = this.#counted(end); d > 0; d--) {
      const values: string[] = []
      for (let k = this.#counted(end); k > 0; k--) {
        const length = this.#counted(end)
        values.push(utf8(b, this.#p, this.#p + length))
        this.#p += length
      }
      dicts.push(values)
    }
    if (this.#p !== end) fail(BAD_DICTIONARY, this.#p)
    this.#dicts = dicts
    return end
  }

  /** A count (or a size) at the cursor, no more than the bytes left to `end`. */
  #counted(end: number): number {
    const n = this.#count(this.#p, end)
    if (n > end - this.#p) fail(BAD_DICTIONARY, this.#p)
    return n
  }

  // ---- records --------------------------------------------------------------

  /** A struct's run at `[start, end)` as an object; the cursor ends at `end`.
   * An empty run is the struct's zero. */
  #record(at: number, start: number, end: number, wide: boolean): Record<string, unknown> {
    this.#enter()
    this.#plan(at)
    const cache = this.#cacheOf(at)
    const values = cache.zeros.slice()
    const saved = this.#e
    this.#p = start
    this.#e = end
    this.#fields(at, values, cache, wide)
    this.#e = saved
    this.#p = end
    // A by-value struct the message left out is an object of zeros (§12); the
    // zeros hold null in its place until here.
    const structs = cache.structs
    for (let k = 0; k < structs.length; k += 2) {
      if (values[structs[k]] === null) values[structs[k]] = this.#zeroStruct(structs[k + 1])
    }
    this.#depth--
    return makeObject(cache, values)
  }

  #zeroStruct(at: number): Record<string, unknown> {
    return this.#record(at, this.#p, this.#p, false)
  }

  #cacheOf(at: number): Cache {
    const plan = this.#plans[at]
    let cache = plan.cache as Cache | null
    if (cache === null) {
      cache = buildCache(this.#plans, at)
      plan.cache = cache
    }
    return cache
  }

  /** A run's fields into `values`, each at its slot in `cache`. */
  #fields(at: number, values: unknown[], cache: Cache, wide: boolean): void {
    const plan = this.#plans[at]
    const slots = cache.slots.get(at) as Int32Array
    const b = this.#b
    // A wide field is a key byte and at least a descriptor; a narrow key is
    // the high nibble.
    const tail = wide ? 1 : 0
    const shift = wide ? 0 : 4
    while (this.#p + tail < this.#e) {
      const index = plan.byKey[b[this.#p] >> shift]
      if (index < 0) {
        // Every field is sized by its header, so an unknown key steps over.
        this.#skip(wide)
        continue
      }
      const field = plan.fields[index]
      if (field.pageLink) {
        // The next page's fields go into this object.
        const pageWide = this.#structHead()
        const end = this.#p
        const saved = this.#e
        this.#enter()
        this.#p = this.#ls
        this.#e = end
        this.#fields(field.sub, values, cache, pageWide)
        this.#depth--
        this.#e = saved
        this.#p = end
        continue
      }
      // A bool or a number, the commonest field, goes straight to #numeric.
      const op = field.op
      values[slots[index]] = op <= OP_FLOAT64 ? this.#numeric(op, wide) : this.#value(field, wide)
    }
    if (this.#p < this.#e) fail(TRUNCATED, this.#p)
  }

  /** Steps over the field at the cursor. */
  #skip(wide: boolean): void {
    if (wide) this.#p += 1 + this.#valueSize(this.#p + 1)
    else this.#nhead()
  }

  /** What an absent field holds (§12). */
  #zero(field: Field): unknown {
    return field.op === OP_STRUCT ? this.#zeroStruct(field.sub) : zeroOf(field.op)
  }

  /** The keyed field at the cursor. */
  #value(field: Field, wide: boolean): unknown {
    const op = field.op
    if (op === OP_STRUCT || op === OP_POINTER_STRUCT) return this.#struct(field.sub, wide)
    if (op === OP_STRUCTS) return this.#structs(field.sub, wide)
    if (op === OP_MAP) return this.#map(field, wide)
    if (op === OP_ANY || op === OP_ANYS) {
      // The key is a field's; behind it is a key-less value. (Wide only: a
      // section puts no dynamic value under a four-bit key.)
      this.#p++
      return this.#dynamicValue()
    }
    const scalar = op === OP_POINTER ? field.elemOp : op
    return scalar <= OP_FLOAT64 ? this.#numeric(scalar, wide) : this.#scalar(scalar, wide)
  }

  /** A keyed struct of plan `sub`. */
  #struct(sub: number, wide: boolean): Record<string, unknown> {
    const at = this.#p
    if (sub < 0) fail(BAD_SECTION, at)
    if (wide) {
      const runWide = this.#structHead()
      return this.#record(sub, this.#ls, this.#p, runWide)
    }
    this.#nhead()
    if ((this.#nib & ~1) !== NIB_LENGTH) fail(BAD_DESCRIPTOR, at)
    return this.#record(sub, this.#fs, this.#p, this.#nib === NIB_LENGTH + 1)
  }

  /** A keyed list of structs of plan `sub`: a list of records, or a table. */
  #structs(sub: number, wide: boolean): unknown[] {
    const at = this.#p
    if (sub < 0) fail(BAD_SECTION, at)
    const subWide = this.#plan(sub).wide
    let flag: number
    let start: number
    if (wide) {
      // A wide table is a MAP with its table bit; its grouped bit is 4.
      const desc = this.#b[at + 1]
      const table = desc >= 0x80 && ((desc >> 4) & 7) === CLASS_MAP && (desc & 8) !== 0
      this.#wideBody(table ? CLASS_MAP : CLASS_LIST)
      flag = table ? 1 | ((desc & 4) >> 1) : 0
      start = this.#ls
    } else {
      // 00 counted, 10 one element, 01 a table, 11 a grouped table.
      flag = this.#composite(0b1111)
      start = this.#fs
    }
    const end = this.#p
    if (flag & 1) return this.#table(sub, start, end, flag === 3, wide || subWide)
    if (wide) return this.#items(start, end, ITEM_STRUCT, sub)
    let out: unknown[]
    this.#enter()
    if (flag === 2) {
      out = [this.#record(sub, start, end, subWide)]
    } else {
      const count = this.#count(start, end)
      if (count > end - this.#p) fail(TRUNCATED, this.#p)
      out = new Array(count)
      for (let i = 0; i < count; i++) {
        const size = this.#count(this.#p, end)
        const from = this.#p
        if (size > end - from) fail(TRUNCATED, from)
        out[i] = this.#record(sub, from, from + size, subWide)
      }
    }
    this.#depth--
    this.#p = end
    return out
  }

  // ---- scalars --------------------------------------------------------------

  /**
   * Reads the narrow field header at the cursor: sets `nib` and `fs` (payload
   * start), moves the cursor past the payload and returns the payload size.
   */
  #nhead(): number {
    const b = this.#b
    const p = this.#p
    const e = this.#e
    if (p >= e) fail(TRUNCATED, p)
    const nib = b[p] & 15
    let size = 0
    let start = p + 1
    if (nib > NIB_INLINE_LAST && nib < NIB_LENGTH) size = nib - 3
    else if (nib >= NIB_LENGTH) {
      if (p + 1 >= e) fail(TRUNCATED, p)
      const first = b[p + 1]
      if (first < 0xf0) {
        size = first
        start = p + 2
      } else if (first <= 0xfd) {
        if (p + 2 >= e) fail(TRUNCATED, p)
        size = 240 + (((first - 0xf0) << 8) | b[p + 2])
        start = p + 3
      } else {
        const wide = first === 0xff
        if (p + (wide ? 5 : 3) >= e) fail(TRUNCATED, p)
        this.#le(p + 2, wide ? 4 : 2)
        size = this.#lo
        start = p + (wide ? 6 : 4)
      }
    }
    if (size > e - start) fail(TRUNCATED, p)
    this.#nib = nib
    this.#fs = start
    this.#p = start + size
    return size
  }

  /** A keyed scalar of op `op` at the cursor. */
  #scalar(op: number, wide: boolean): unknown {
    if (op <= OP_FLOAT64) return this.#numeric(op, wide)
    const at = this.#p
    const b = this.#b
    if (op === OP_STRING || op === OP_BYTES) {
      // -1 plain, 0 packed opening lower case, 1 upper.
      let packed = -1
      let size: number
      if (wide) {
        size = this.#wideBody(CLASS_BLOB)
        const desc = b[at + 1]
        if (op === OP_STRING && (desc & 0x0c) === 4) packed = (desc & 2) >> 1
      } else {
        size = this.#nhead()
        const nib = this.#nib
        if (nib <= NIB_INLINE_LAST || (nib >= NIB_LENGTH && (nib & 3) === 3)) fail(BAD_DESCRIPTOR, at)
        if (nib >= NIB_LENGTH) packed = (nib & 3) - 1
      }
      const start = wide ? this.#ls : this.#fs
      if (op === OP_BYTES) {
        if (packed >= 0) fail(BAD_DESCRIPTOR, at)
        return b.slice(start, start + size)
      }
      return packed < 0 ? utf8(b, start, start + size) : packed5(b, start, start + size, packed === 1)
    }
    if (op === OP_STRINGS) return wide ? this.#wideStrings(at + 1) : this.#narrowStrings(true)
    if (op < OP_INT8S || op > OP_UINT64S) fail(UNWALKABLE, at)
    // An integer array: its elements are signed for []int8..[]int64.
    const signed = op <= OP_INT64S
    let length: number
    let width: number
    let positive = !signed
    if (wide) {
      length = this.#wideBody(CLASS_VEC)
      const desc = b[at + 1]
      width = 1 << ((desc >> 2) & 3)
      positive = (desc & 2) !== 0
    } else {
      length = this.#nhead()
      if (this.#nib < NIB_LENGTH) fail(BAD_DESCRIPTOR, at)
      width = 1 << (this.#nib & 3)
      if (width > 1 << (op - (signed ? OP_INT8S : OP_INT64S))) fail(TOO_WIDE, at)
    }
    if (length % width !== 0) fail(TRUNCATED, at)
    const start = wide ? this.#ls : this.#fs
    const count = length / width
    const out: unknown[] = new Array(count)
    for (let i = 0; i < count; i++) {
      this.#le(start + i * width, width)
      let lo = this.#lo
      let hi = this.#hi
      // Two's complement narrower than eight bytes: sign-extend.
      if (!positive && width < 8 && (lo >>> (width * 8 - 1)) & 1) {
        if (width < 4) lo = (lo | (-1 << (width * 8))) >>> 0
        hi = 0xffffffff
      }
      out[i] = this.#laneValue(lo, hi, signed)
    }
    return out
  }

  /** A keyed bool or number of op `op` at the cursor: apart from #scalar so
   * that the commonest field stays small enough to inline. */
  #numeric(op: number, wide: boolean): unknown {
    const at = this.#p
    const signed = op >= OP_INT8 && op <= OP_INT64
    if (wide) {
      if (signed) this.#elementSigned(at + 1, true)
      else this.#elementUnsigned(at + 1)
    } else if (signed) this.#narrowSigned()
    else this.#narrowUnsigned()
    return this.#number(op, at)
  }

  /** The lanes as op `op`'s value, refusing one wider than the op. */
  #number(op: number, at: number): unknown {
    const lo = this.#lo
    const hi = this.#hi
    switch (op) {
      case OP_BOOL:
        return hi === 0 && lo === 1
      case OP_FLOAT32:
        if (hi !== 0) fail(TOO_WIDE, at)
        return this.#reversed32()
      case OP_FLOAT64:
        return this.#reversed64()
      case OP_INT64:
      case OP_UINT64:
        return this.#laneValue(lo, hi, op === OP_INT64)
    }
    // An unsigned value is the low lane; a signed one the low lane as an
    // int32, the high lane its sign extension.
    const limit = LIMIT[op]
    if (op > OP_INT32) {
      if (hi !== 0 || lo >= limit) fail(TOO_WIDE, at)
      return lo
    }
    const value = lo | 0
    if (hi !== (value < 0 ? 0xffffffff : 0) || value >= limit || value < -limit) fail(TOO_WIDE, at)
    return value
  }

  /** A narrow unsigned value into `lo`/`hi`. */
  #narrowUnsigned(): void {
    const size = this.#nhead()
    const nib = this.#nib
    if (nib <= NIB_INLINE_LAST) {
      this.#lo = nib + 1
      this.#hi = 0
    } else if (nib < NIB_LENGTH || (nib === NIB_LENGTH && size === 0)) {
      this.#le(this.#fs, size)
    } else fail(BAD_DESCRIPTOR, this.#fs - 1)
  }

  /** A narrow signed value into two's complement `lo`/`hi`: inline 1..3 or −1,
   * a positive magnitude, or (length form) a negative one. */
  #narrowSigned(): void {
    const size = this.#nhead()
    const nib = this.#nib
    if (nib <= NIB_INLINE_LAST) {
      this.#lo = nib === NIB_INLINE_LAST ? 0xffffffff : nib + 1
      this.#hi = nib === NIB_INLINE_LAST ? 0xffffffff : 0
      return
    }
    if (nib > NIB_LENGTH || size > 8) fail(nib > NIB_LENGTH ? BAD_DESCRIPTOR : TOO_WIDE, this.#p)
    this.#le(this.#fs, size)
    if (nib === NIB_LENGTH) this.#negate()
    else if (this.#hi >= 0x80000000) fail(TOO_WIDE, this.#p)
  }

  /** Turns the magnitude in `lo`/`hi` into its negative, refusing one past −2^63. */
  #negate(): void {
    const lo = this.#lo
    const hi = this.#hi
    if (hi > 0x80000000 || (hi === 0x80000000 && lo !== 0)) fail(TOO_WIDE, this.#p)
    this.#lo = -lo >>> 0
    this.#hi = (~hi + (lo === 0 ? 1 : 0)) >>> 0
  }

  /** `size` (0..8) little-endian bytes from `at` into `lo`/`hi`. */
  #le(at: number, size: number): void {
    const b = this.#b
    let lo = 0
    let hi = 0
    switch (size) {
      case 8: hi |= b[at + 7] << 24 // falls through
      case 7: hi |= b[at + 6] << 16 // falls through
      case 6: hi |= b[at + 5] << 8 // falls through
      case 5: hi |= b[at + 4] // falls through
      case 4: lo |= b[at + 3] << 24 // falls through
      case 3: lo |= b[at + 2] << 16 // falls through
      case 2: lo |= b[at + 1] << 8 // falls through
      case 1: lo |= b[at]
    }
    this.#lo = lo >>> 0
    this.#hi = hi >>> 0
  }

  /** Two lanes as a signed or unsigned 64-bit value: a bigint past 2^53. */
  #laneValue(lo: number, hi: number, signed: boolean): number | bigint {
    const h = signed ? hi | 0 : hi
    if ((h < SAFE_HI && h >= -SAFE_HI) || !this.#bigint) return h * TWO32 + lo
    const big = (BigInt(hi) << 32n) | BigInt(lo)
    return signed ? BigInt.asIntN(64, big) : big
  }

  /** A float whose bits travel byte-reversed in the lanes (§5.1). */
  #reversed64(): number {
    scratch.setUint32(0, this.#lo, true)
    scratch.setUint32(4, this.#hi, true)
    return scratch.getFloat64(0, false)
  }

  #reversed32(): number {
    scratch.setUint32(0, this.#lo, true)
    return shortest32(scratch.getFloat32(0, false))
  }

  // ---- strings --------------------------------------------------------------

  /** A narrow string array: counted elements, or with `single` the one-element
   * form (`1110`), which a table's column never is. */
  #narrowStrings(single: boolean): string[] {
    const at = this.#p
    const size = this.#nhead()
    const nib = this.#nib
    const start = this.#fs
    const end = start + size
    if (single && nib === NIB_LENGTH + 2) return [utf8(this.#b, start, end)]
    if (nib !== NIB_LENGTH) fail(BAD_DESCRIPTOR, at)
    return this.#elements(start, end, -1)
  }

  /** A wide string list (a homogeneous LIST) whose descriptor is at `at`. */
  #wideStrings(at: number): string[] {
    const end = this.#span(at, CLASS_LIST)
    const count = this.#count(this.#ls, end)
    if (count > end - this.#p) fail(TRUNCATED, at)
    this.run(this.#p, end, count, TRUNCATED)
    this.#p = end
    return this.strs
  }

  /** `[size][bytes]` elements to `end` (or exactly `count` of them, then
   * nothing after), each size one byte up to 254 or 0xFF and a u32. */
  #elements(start: number, end: number, count: number): string[] {
    const p = this.run(start, end, count, count >= 0 ? BAD_TABLE : TRUNCATED)
    if (count >= 0 && p !== end) fail(BAD_TABLE, p)
    return this.strs
  }

  /** The strings decoded by the last run(). */
  strs: string[] = []

  /** Decodes `[size][bytes]` strings from `start` into `this.strs`: exactly
   * `count` of them, or to `end` when count is negative. Returns where the last
   * one ends. Running out of bytes before `count` fails with `short`.
   *
   * A run of several strings is decoded with one TextDecoder call (wholeRun)
   * and each string cut from it with substring: a decoder call costs far more
   * than a substring. When every byte maps to one unit (most columns, ASCII)
   * a string sits at its byte offset; otherwise at the units counted so far,
   * which holds while every string is well-formed (wellFormedUnits). The first
   * malformed one drops the rest of the run to one decode per string. */
  run(start: number, end: number, count: number, short: string): number {
    const b = this.#b
    const out: string[] = count >= 0 ? new Array(count) : []
    this.strs = out
    const text = end - start >= 32 && count !== 0 && count !== 1 ? wholeRun(b, start, end) : null
    // 0: byte offsets in text; 1: counted units in text; 2: no text.
    let mode = text === null ? 2 : text.length === end - start ? 0 : 1
    let u = 0
    let p = start
    let n = 0
    while (count >= 0 ? n < count : p < end) {
      if (p >= end) fail(short, p)
      let size = b[p]
      let from = p + 1
      if (size === 0xff) {
        if (p + 5 > end) fail(TRUNCATED, p)
        size = (b[p + 1] | (b[p + 2] << 8) | (b[p + 3] << 16) | (b[p + 4] << 24)) >>> 0
        from = p + 5
      }
      if (size > end - from) fail(TRUNCATED, p)
      const to = from + size
      if (mode === 0) out[n++] = (text as string).substring(from - start, to - start)
      else {
        let k = -1
        if (mode === 1) {
          u += from - p === 1 ? 1 : 1 + lengthUnits(b, p + 1)
          k = wellFormedUnits(b, from, to)
        }
        if (k >= 0) {
          out[n++] = (text as string).substring(u, u + k)
          u += k
        } else {
          mode = 2
          out[n++] = utf8(b, from, to)
        }
      }
      p = to
    }
    return p
  }

  // ---- composites -----------------------------------------------------------

  /** A narrow composite: its flags must be `accept`'s; returns the flags and
   * leaves the body at `[fs, p)`. */
  #composite(accept: number): number {
    const at = this.#p
    this.#nhead()
    const nib = this.#nib
    if (nib < NIB_LENGTH || ((1 << (nib & 3)) & accept) === 0) fail(BAD_DESCRIPTOR, at)
    return nib & 3
  }

  /** A count at `at` (one byte up to 254, or 0xFF and a u32) that must lie
   * before `end`; the cursor moves past it. */
  #count(at: number, end: number): number {
    const b = this.#b
    if (at >= end) fail(TRUNCATED, at)
    const first = b[at]
    if (first !== 0xff) {
      this.#p = at + 1
      return first
    }
    if (at + 5 > end) fail(TRUNCATED, at)
    this.#p = at + 5
    return (b[at + 1] | (b[at + 2] << 8) | (b[at + 3] << 16) | (b[at + 4] << 24)) >>> 0
  }

  /** A keyed map: entries of typed keys and values, or (wide, a map of any)
   * compact ones. */
  #map(field: Field, wide: boolean): Record<string, unknown> {
    const keyKind = field.keyKind
    const valueKind = field.valueKind
    let count = -1
    if (wide) this.#wideBody(CLASS_MAP)
    else if (this.#composite(0b0101) === 2) count = 1 // 00 counted, 10 one entry
    const start = wide ? this.#ls : this.#fs
    const end = this.#p
    if (keyKind > MAP_UINT) fail(UNWALKABLE, start)
    if (valueKind === MAP_ANY) return this.#entries(start, end)
    const saved = this.#e
    this.#e = end
    this.#p = start
    if (count < 0) {
      count = this.#count(start, end)
      if (count > (end - this.#p) / 2) fail(TRUNCATED, this.#p)
    }
    this.#enter()
    const keys: string[] = new Array(count)
    const values: unknown[] = new Array(count)
    let h = count
    for (let i = 0; i < count; i++) {
      if (keyKind === MAP_STRING) {
        keys[i] = this.#elementString(wide, true)
        h = Math.imul(h ^ lastKey.slot, 0x9e3779b1)
      } else {
        this.#element(keyKind === MAP_INT, wide)
        keys[i] = String(this.#laneValue(this.#lo, this.#hi, keyKind === MAP_INT))
      }
      values[i] = this.#mapValue(valueKind, field.sub, field.listKind, wide)
    }
    this.#depth--
    this.#e = saved
    this.#p = end
    return object(keys, values, h)
  }

  /** A map value of kind `kind` at the cursor. */
  #mapValue(kind: number, sub: number, listKind: number, wide: boolean): unknown {
    switch (kind) {
      case MAP_STRING:
        return this.#elementString(wide, false)
      case MAP_INT:
      case MAP_UINT:
        this.#element(kind === MAP_INT, wide)
        return this.#laneValue(this.#lo, this.#hi, kind === MAP_INT)
      case MAP_FLOAT32:
      case MAP_FLOAT64:
      case MAP_BOOL:
        this.#element(false, wide)
        if (kind === MAP_BOOL) return this.#hi === 0 && this.#lo === 1
        return kind === MAP_FLOAT32 ? this.#reversed32() : this.#reversed64()
      case MAP_STRUCT: {
        if (sub < 0) fail(BAD_SECTION, this.#p)
        if (wide) return this.#elementStruct(sub)
        const size = this.#count(this.#p, this.#e)
        const from = this.#p
        if (size > this.#e - from) fail(TRUNCATED, from)
        return this.#record(sub, from, from + size, this.#plan(sub).wide)
      }
      // A map of lists is wide only.
      case MAP_STRUCTS:
        if (sub < 0) fail(BAD_SECTION, this.#p)
        this.#plan(sub)
        return this.#typedRows(sub)
      case MAP_LIST: {
        if (this.#elementKind() !== KIND_LIST) fail(BAD_DESCRIPTOR, this.#p)
        const end = this.#span(this.#p, CLASS_LIST)
        return this.#items(this.#ls, end, ITEM_MAP_VALUE, listKind)
      }
      default:
        fail(UNWALKABLE, this.#p)
    }
  }

  /** A key-less integer element at the cursor into the lanes: narrow, a code
   * (`[pos:1][n:3]` when signed, else 0..7 the value and 8..15 that many bytes
   * less seven); wide, the descriptor forms. */
  #element(signed: boolean, wide: boolean): void {
    if (wide) {
      if (signed) this.#elementSigned(this.#p, false)
      else this.#elementUnsigned(this.#p)
      return
    }
    const p = this.#p
    if (p >= this.#e) fail(TRUNCATED, p)
    const code = this.#b[p]
    const width = signed ? MAGNITUDE_WIDTH[code & 7] : (code & 15) <= 7 ? 0 : (code & 15) - 7
    if (p + 1 + width > this.#e) fail(TRUNCATED, p)
    this.#le(p + 1, width)
    if (width === 0) this.#lo = signed ? 1 : code & 15
    this.#p = p + 1 + width
    if (signed && (code & 8) === 0) {
      // wrapping_neg, as the Rust reader does: no range refusal here.
      const lo = this.#lo
      this.#lo = -lo >>> 0
      this.#hi = (~this.#hi + (lo === 0 ? 1 : 0)) >>> 0
    }
  }

  /** A key-less string element at the cursor; with `asKey`, through the key
   * cache. */
  #elementString(wide: boolean, asKey: boolean): string {
    const p = this.#p
    let start: number
    let end: number
    if (wide) {
      end = this.#span(p, CLASS_BLOB)
      start = this.#ls
    } else {
      if (p >= this.#e) fail(TRUNCATED, p)
      const width = LENGTH_WIDTH[this.#b[p] & 3]
      if (p + 1 + width > this.#e) fail(TRUNCATED, p)
      this.#le(p + 1, width)
      start = p + 1 + width
      if (this.#hi !== 0 || this.#lo > this.#e - start) fail(TRUNCATED, p)
      end = start + this.#lo
    }
    this.#p = end
    return asKey ? key(this.#b, start, end) : utf8(this.#b, start, end)
  }

  // ---- wide forms -----------------------------------------------------------

  /** The size of the value whose descriptor is at `at`, descriptor included
   * (`value_size`). Not recursive: a hostile message cannot deepen the stack. */
  #valueSize(at: number): number {
    const b = this.#b
    const e = this.#e
    let extra = 0
    for (;;) {
      if (at >= e) fail(TRUNCATED, at)
      const desc = b[at]
      if (desc < 0x80) return extra + 1
      const cls = (desc >> 4) & 7
      if (cls === CLASS_INT) {
        const width = MAGNITUDE_WIDTH[desc & 7]
        if (e - (at + 1) < width) fail(TRUNCATED, at)
        return extra + 1 + width
      }
      if (cls === CLASS_SPECIAL) {
        if (desc & SPECIAL_VARINT) return extra + this.#varintSize(at)
        const detail = desc & 15
        if (detail === 3 || detail === 4 || detail === 5) {
          extra += 1
          at += 1
          continue
        }
        if (detail === 7) {
          this.#count(at + 1, e)
          extra += this.#p - at
          at = this.#p
          continue
        }
        if (detail === 6) fail(BAD_DESCRIPTOR, at)
        return extra + 1
      }
      return extra + this.#span(at, cls) - at
    }
  }

  /** The bytes a varint whose descriptor is at `at` occupies, refusing more
   * than 64 bits. Leaves the value in the lanes. */
  #varintSize(at: number): number {
    const b = this.#b
    const e = this.#e
    let lo = b[at] & 7
    let hi = 0
    let shift = 3
    let p = at + 1
    for (;;) {
      if (p >= e) fail(TRUNCATED, at)
      const byte = b[p++]
      const v = byte & 0x7f
      if (shift < 32) {
        lo |= v << shift
        if (shift > 25) hi |= v >>> (32 - shift)
      } else if (shift < 64) {
        hi |= v << (shift - 32)
        if (shift > 57 && v >>> (64 - shift) !== 0) fail(TOO_WIDE, at)
      } else if (v !== 0 || byte >= 0x80) fail(TOO_WIDE, at)
      if (byte < 0x80) break
      shift += 7
      if (shift > 66) fail(TOO_WIDE, at)
    }
    this.#lo = lo >>> 0
    this.#hi = hi >>> 0
    return p - at
  }

  /**
   * The length a length-carrying class declares at descriptor `at`
   * (`declared_length`); `ls` is set to where its payload starts.
   */
  #declared(at: number, want: number): number {
    const b = this.#b
    if (at >= this.#e) fail(TRUNCATED, at)
    const desc = b[at]
    if (desc < 0x80 || ((desc >> 4) & 7) !== want) fail(BAD_DESCRIPTOR, at)
    let width = LENGTH_WIDTH[desc & 3]
    if (want === CLASS_VEC) width = (desc & 1) === 0 ? 1 : 4
    else if (want === CLASS_BLOB) {
      if (desc & 8) {
        this.#ls = at + 1
        return (desc & 7) + 1
      }
      if (desc & 4) width = LENGTH_WIDTH[desc & 1]
    }
    if (this.#e - (at + 1) < width) fail(TRUNCATED, at)
    this.#le(at + 1, width)
    if (this.#hi !== 0) fail(TRUNCATED, at)
    this.#ls = at + 1 + width
    return this.#lo
  }

  /** The value of class `want` whose descriptor is at `at`: its payload starts
   * at `ls`, and its end, which must lie within `e`, is returned. */
  #span(at: number, want: number): number {
    const length = this.#declared(at, want)
    if (length > this.#e - this.#ls) fail(TRUNCATED, at)
    return this.#ls + length
  }

  /** A keyed composite of class `want`: its body is `[ls, p)` after, and its
   * length is returned. */
  #wideBody(want: number): number {
    this.#p = this.#span(this.#p + 1, want)
    return this.#p - this.#ls
  }

  /** A keyed STRUCT: body at `[ls, p)`; returns whether its run is wide. */
  #structHead(): boolean {
    const wide = (this.#b[this.#p + 1] & 8) !== 0
    this.#wideBody(CLASS_STRUCT)
    return wide
  }

  /** An unsigned value whose descriptor is at `at`: inline, INT or varint.
   * The INT form's sign goes to `neg`; an unsigned read ignores it, as Go's
   * and Rust's readers do. */
  #elementUnsigned(at: number): void {
    const b = this.#b
    if (at >= this.#e) fail(TRUNCATED, at)
    const desc = b[at]
    this.#neg = false
    if (desc < 0x80) {
      this.#lo = desc
      this.#hi = 0
      this.#p = at + 1
      return
    }
    const cls = (desc >> 4) & 7
    if (cls === CLASS_INT) {
      const width = MAGNITUDE_WIDTH[desc & 7]
      if (this.#e - (at + 1) < width) fail(TRUNCATED, at)
      this.#le(at + 1, width)
      if (width === 0) this.#lo = 1
      this.#neg = (desc & 8) === 0
      this.#p = at + 1 + width
      return
    }
    if (cls === CLASS_SPECIAL && desc & SPECIAL_VARINT) {
      this.#p = at + this.#varintSize(at)
      return
    }
    fail(BAD_DESCRIPTOR, at)
  }

  /** A signed value whose descriptor is at `at`, two's complement into the
   * lanes: INT with its sign, inline, or a varint, zigzagged when `zigzag` (a
   * keyed field's is, a key-less element's is not). A magnitude past int64 is
   * refused. */
  #elementSigned(at: number, zigzag: boolean): void {
    const desc = this.#b[at]
    this.#elementUnsigned(at)
    if (zigzag && desc >= 0x80 && ((desc >> 4) & 7) === CLASS_SPECIAL) {
      const lo = this.#lo
      const hi = this.#hi
      const m = -(lo & 1)
      this.#lo = (((lo >>> 1) | (hi << 31)) ^ m) >>> 0
      this.#hi = ((hi >>> 1) ^ m) >>> 0
    } else if (this.#neg) this.#negate()
    else if (this.#hi >= 0x80000000) fail(TOO_WIDE, at)
  }

  /** A list body at `[start, end)`: a count, then that many elements read as
   * `what` says (`arg` the plan of a struct, the kind of a map value). */
  #items(start: number, end: number, what: number, arg: number): unknown[] {
    const saved = this.#e
    this.#e = end
    const count = this.#count(start, end)
    if (count > end - this.#p) fail(TRUNCATED, this.#p)
    this.#enter()
    const out: unknown[] = new Array(count)
    for (let i = 0; i < count; i++) {
      out[i] = what === ITEM_COMPACT ? this.#compactValue()
        : what === ITEM_STRUCT ? this.#elementStruct(arg)
          : this.#mapValue(arg, -1, 0, true)
    }
    this.#depth--
    this.#e = saved
    this.#p = end
    return out
  }

  /** A map body at `[start, end)` of compact entries. */
  #entries(start: number, end: number): Record<string, unknown> {
    const saved = this.#e
    this.#e = end
    const count = this.#count(start, end)
    if (count > (end - this.#p) / 2) fail(TRUNCATED, this.#p)
    const out = this.#compactEntries(count)
    this.#e = saved
    this.#p = end
    return out
  }

  /** A key-less STRUCT element at the cursor, as the record of plan `at`. */
  #elementStruct(at: number): Record<string, unknown> {
    const d = this.#p
    const end = this.#span(d, CLASS_STRUCT)
    return this.#record(at, this.#ls, end, (this.#b[d] & 8) !== 0)
  }

  /** A LIST or TABLE element at the cursor, as rows of plan `at`. */
  #typedRows(at: number): unknown[] {
    const kind = this.#elementKind()
    const d = this.#p
    // A dynamic value's table is never grouped.
    if ((kind !== KIND_LIST && kind !== KIND_TABLE) || (kind === KIND_TABLE && this.#b[d] & 4)) fail(BAD_DESCRIPTOR, d)
    const end = this.#span(d, kind === KIND_LIST ? CLASS_LIST : CLASS_MAP)
    if (kind === KIND_LIST) return this.#items(this.#ls, end, ITEM_STRUCT, at)
    return this.#table(at, this.#ls, end, false, true)
  }

  // ---- dynamic values (§9) --------------------------------------------------

  #elementKind(): number {
    if (this.#p >= this.#e) return KIND_INVALID
    const desc = this.#b[this.#p]
    if (desc < 0x80) return KIND_INT
    const cls = (desc >> 4) & 7
    if (cls === CLASS_SPECIAL) return desc & SPECIAL_VARINT ? KIND_INT : SPECIAL_KIND[desc & 7]
    return cls === CLASS_MAP && desc & 8 ? KIND_TABLE : CLASS_KIND[cls]
  }

  /** A key-less value that says what it is; the cursor is on its descriptor. */
  #dynamicValue(): unknown {
    const at = this.#p
    const b = this.#b
    const kind = this.#elementKind()
    switch (kind) {
      case KIND_NULL:
      case KIND_BOOL:
        this.#p++
        return kind === KIND_NULL ? null : (b[at] & 15) === 1
      case KIND_INT:
        this.#elementUnsigned(at)
        if (this.#neg) this.#negate()
        // Signed where it fits, unsigned only past 2^63.
        return this.#laneValue(this.#lo, this.#hi, this.#neg || this.#hi < 0x80000000)
      case KIND_FLOAT64:
      case KIND_FLOAT32:
        this.#elementUnsigned(at + 1)
        return kind === KIND_FLOAT64 ? this.#reversed64() : this.#reversed32()
      case KIND_STRING:
      case KIND_BYTES: {
        const end = this.#span(kind === KIND_STRING ? at : at + 1, CLASS_BLOB)
        const start = this.#ls
        this.#p = end
        return kind === KIND_STRING ? utf8(b, start, end) : b.slice(start, end)
      }
      case KIND_LIST:
      case KIND_MAP: {
        const end = this.#span(at, kind === KIND_LIST ? CLASS_LIST : CLASS_MAP)
        return kind === KIND_LIST ? this.#items(this.#ls, end, ITEM_COMPACT, 0) : this.#entries(this.#ls, end)
      }
      case KIND_TYPED:
        return this.#typedValue(this.#count(at + 1, this.#e))
      default:
        // A STRUCT or TABLE needs a type tag naming it.
        fail(this.#p >= this.#e ? TRUNCATED : BAD_DESCRIPTOR, at)
    }
  }

  /** The record or rows behind a type tag naming plan `at`. */
  #typedValue(at: number): unknown {
    if (this.#plans[at] === undefined) fail(BAD_SECTION, this.#p)
    return this.#elementKind() === KIND_STRUCT ? this.#elementStruct(at) : this.#typedRows(at)
  }

  // ---- compact values (§9.1) ------------------------------------------------

  #compactValue(): unknown {
    const b = this.#b
    const p = this.#p
    if (p >= this.#e) fail(TRUNCATED, p)
    const code = b[p]
    if (code < 0x80) {
      this.#p = p + 1
      return code
    }
    if (code < 0x90) return this.#compactMapBody(code & 15, p + 1)
    if (code < 0xa0) return this.#compactListBody(code & 15, p + 1)
    if (code < 0xc0) return this.#compactBytes(p + 1, code & 31, false)
    if (code <= 0xc2) {
      this.#p = p + 1
      return code === 0xc0 ? null : code === 0xc2
    }
    if (code <= 0xc8) {
      // A string (c3..c5) or bytes (c6..c8) of a 1, 2 or 4 byte length.
      const bytes = code >= 0xc6
      const width = 1 << (code - (bytes ? 0xc6 : 0xc3))
      if (width > this.#e - (p + 1)) fail(TRUNCATED, p)
      this.#le(p + 1, width)
      return this.#compactBytes(p + 1 + width, this.#lo, bytes)
    }
    if (code <= 0xcb) {
      const count = this.#count(p + 1, this.#e)
      if (code === 0xc9) return this.#compactMapBody(count, this.#p)
      return code === 0xca ? this.#compactListBody(count, this.#p) : this.#typedValue(count)
    }
    if (code === 0xcc) {
      this.#p = p + 1
      return Number(this.#compactInteger())
    }
    if (code >= 0xd0 && code <= 0xdf) return this.#compactInteger()
    if (code >= 0xe0 && code <= 0xed) {
      // A float64 (e0..e8) or float32 (e9..ed) of that many bytes.
      const double = code <= 0xe8
      const width = code - (double ? 0xe0 : 0xe9)
      if (width > this.#e - (p + 1)) fail(TRUNCATED, p)
      this.#le(p + 1, width)
      this.#p = p + 1 + width
      return double ? this.#reversed64() : this.#reversed32()
    }
    if (code >= 0xf0) {
      this.#p = p + 1
      return code - 0xf0
    }
    fail(BAD_DESCRIPTOR, p)
  }

  /** A compact integer at the cursor: signed where it fits, unsigned past 2^63. */
  #compactInteger(): number | bigint {
    const p = this.#p
    if (p >= this.#e) fail(TRUNCATED, p)
    const code = this.#b[p]
    if (code < 0x80) {
      this.#p = p + 1
      return code
    }
    const width = (code & 7) + 1
    if (code < 0xd0 || code > 0xdf || width > this.#e - (p + 1)) fail(code < 0xd0 || code > 0xdf ? BAD_DESCRIPTOR : TRUNCATED, p)
    this.#le(p + 1, width)
    this.#p = p + 1 + width
    if (code >= 0xd8) {
      const lo = this.#lo
      const hi = this.#hi
      if (hi > 0x80000000 || (hi === 0x80000000 && lo !== 0)) fail(BAD_DESCRIPTOR, p)
      return this.#laneValue(-lo >>> 0, (~hi + (lo === 0 ? 1 : 0)) >>> 0, true)
    }
    return this.#laneValue(this.#lo, this.#hi, this.#hi < 0x80000000)
  }

  #compactBytes(start: number, size: number, bytes: boolean): unknown {
    if (size > this.#e - start) fail(TRUNCATED, start)
    this.#p = start + size
    return bytes ? this.#b.slice(start, start + size) : utf8(this.#b, start, start + size)
  }

  #compactListBody(count: number, at: number): unknown[] {
    this.#p = at
    if (count > this.#e - at) fail(TRUNCATED, at)
    this.#enter()
    const out: unknown[] = new Array(count)
    for (let i = 0; i < count; i++) out[i] = this.#compactValue()
    this.#depth--
    return out
  }

  #compactMapBody(count: number, at: number): Record<string, unknown> {
    this.#p = at
    if (count > (this.#e - at) / 2) fail(TRUNCATED, at)
    return this.#compactEntries(count)
  }

  #compactEntries(count: number): Record<string, unknown> {
    this.#enter()
    const keys: string[] = new Array(count)
    const values: unknown[] = new Array(count)
    let h = count
    for (let i = 0; i < count; i++) {
      keys[i] = this.#compactKey()
      h = Math.imul(h ^ lastKey.slot, 0x9e3779b1)
      values[i] = this.#compactValue()
    }
    this.#depth--
    return object(keys, values, h)
  }

  #compactKey(): string {
    const p = this.#p
    if (p >= this.#e) fail(TRUNCATED, p)
    const code = this.#b[p]
    let start = p + 1
    let size = code & 31
    if (code >= 0xc3 && code <= 0xc5) {
      const width = 1 << (code - 0xc3)
      if (width > this.#e - start) fail(TRUNCATED, p)
      this.#le(start, width)
      start += width
      size = this.#lo
    } else if (!(code >= 0xa0 && code < 0xc0)) {
      if (code < 0x80 || (code >= 0xd0 && code <= 0xdf)) {
        lastKey.slot = -1
        return String(this.#compactInteger())
      }
      fail(BAD_DESCRIPTOR, p)
    }
    if (size > this.#e - start) fail(TRUNCATED, start)
    this.#p = start + size
    return key(this.#b, start, start + size)
  }

  /** A dynamic root: a map's entries to the end, or one value. */
  dynamicRoot(start: number, list: boolean): unknown {
    this.#p = start
    if (list) {
      const value = this.#compactValue()
      if (this.#p < this.#e) fail(BAD_DESCRIPTOR, this.#p)
      return value
    }
    this.#enter()
    const keys: string[] = []
    const values: unknown[] = []
    let h = 0
    while (this.#p < this.#e) {
      keys.push(this.#compactKey())
      h = Math.imul(h ^ lastKey.slot, 0x9e3779b1)
      values.push(this.#compactValue())
    }
    this.#depth--
    return object(keys, values, Math.imul(h ^ keys.length, 0x9e3779b1))
  }

  // ---- tables (§6.3) --------------------------------------------------------

  /** A table body at `[start, end)` of plan `at`, keyed wide when `wide`; the
   * cursor ends at `end`. */
  #table(at: number, start: number, end: number, grouped: boolean, wide: boolean): unknown[] {
    const plan = this.#plan(at)
    const saved = this.#e
    this.#e = end
    const rows = this.#count(start, end)
    let groupRows = rows
    if (grouped) {
      groupRows = this.#count(this.#p, end)
      if (groupRows === 0) fail(BAD_TABLE, this.#p)
    }
    this.#takeRows(rows, plan.fields.length)
    this.#enter()
    const columnar = this.columnar
    // columns(): the groups' columns are kept, and no row is built.
    const groups: Col[][] = []
    const sizes: number[] = []
    this.columnar = false
    const out: unknown[] = columnar ? [] : new Array(rows)
    const b = this.#b
    let p = this.#p
    let done = 0
    // An ungrouped table is one group with no size in front.
    for (let first = !grouped; first || done < rows; first = false) {
      let to = end
      if (grouped) {
        if (end - p < 4) fail(BAD_TABLE, p)
        const size = (b[p] | (b[p + 1] << 8) | (b[p + 2] << 16) | (b[p + 3] << 24)) >>> 0
        if (size > end - p - 4) fail(BAD_TABLE, p)
        p += 4
        to = p + size
      }
      const n = Math.min(groupRows, rows - done)
      const cols = this.#gather(at, p, to, n, wide, 0)
      if (columnar) {
        groups.push(cols)
        sizes.push(n)
      } else this.#buildRows(at, cols, n, out, done)
      p = to
      done += n
    }
    if (p !== end) fail(BAD_TABLE, p)
    this.columnar = columnar
    this.#depth--
    this.#e = saved
    this.#p = end
    if (columnar) {
      const columns: Column[] = []
      return (this.#flatColumns(at, groups, sizes, rows, columns) ? new Columns(rows, columns) : null) as unknown as unknown[]
    }
    return out
  }

  /** columns(): set, a table at the root is kept as columns (a Columns). */
  columnar = false

  /** The message's one table as columns, or null when it is not one: the root
   * is an envelope of a table, or a struct whose one field is a table. */
  rootColumns(start: number, end: number, wide: boolean): ColumnTable | null {
    const plan = this.#plan(0)
    const fields = plan.fields
    if (fields.length !== 1 || fields[0].op !== OP_STRUCTS) return null
    this.columnar = true
    const value = this.root(start, end, wide)
    const table = plan.envelope ? value : (value as Record<string, unknown>)[fields[0].name]
    if (!(table instanceof Columns)) return null
    const out: ColumnTable = { rowCount: table.rowCount, columns: table.columns }
    if (!plan.envelope) out.field = fields[0].name
    return out
  }

  /** A plan's columns over every group, its pages' in line; false when a
   * field has no flat form (a list of structs, a struct, a map). */
  #flatColumns(at: number, groups: (Col[] | null)[], sizes: number[], rows: number, out: Column[]): boolean {
    const fields = this.#plans[at].fields
    for (let k = 0; k < fields.length; k++) {
      const field = fields[k]
      const cols = groups.map((g) => (g === null || (field.pageLink && !g[k].present) ? null : g[k]))
      if (field.pageLink) {
        if (!this.#flatColumns(field.sub, cols.map((c) => (c === null ? null : c.page)), sizes, rows, out)) return false
        continue
      }
      const pointer = field.op === OP_POINTER
      const op = pointer ? field.elemOp : field.op
      if (!(op <= OP_FLOAT64 || op === OP_STRING)) return false
      out.push(columnOf(field.name, op, pointer, cols, sizes, rows))
    }
    return true
  }

  /** One column run of `rows` rows of plan `at`, by field index. */
  #gather(at: number, start: number, end: number, rows: number, wide: boolean, listDepth: number): Col[] {
    const plan = this.#plan(at)
    const fields = plan.fields
    const cols: Col[] = new Array(fields.length)
    for (let k = 0; k < fields.length; k++) cols[k] = { present: false, kind: K_CONST, values: null, dict: null, page: null, valid: null }
    const saved = this.#e
    this.#e = end
    this.#p = start
    const b = this.#b
    while (wide ? this.#p + 1 < end : this.#p < end) {
      const index = plan.byKey[wide ? b[this.#p] : b[this.#p] >> 4]
      if (index < 0) {
        this.#skip(wide)
        continue
      }
      const field = fields[index]
      const col = cols[index]
      col.present = true
      if (field.pageLink) {
        // A page is a wide table of the same rows, never grouped.
        const desc = b[this.#p + 1]
        this.#wideBody(CLASS_MAP)
        const pageEnd = this.#p
        if ((desc & 0xfc) !== 0xe8 || this.#count(this.#ls, pageEnd) !== rows) fail(BAD_TABLE, this.#ls)
        col.page = this.#gather(field.sub, this.#p, pageEnd, rows, true, listDepth)
        this.#p = pageEnd
      } else if (field.op === OP_STRUCTS) {
        this.#takeList(field, col, rows, wide, listDepth)
      } else {
        this.#takeColumn(field, col, rows, wide)
      }
    }
    if (this.#p < end) fail(TRUNCATED, this.#p)
    this.#e = saved
    return cols
  }

  #takeList(field: Field, col: Col, rows: number, wide: boolean, listDepth: number): void {
    if (field.sub < 0) fail(BAD_SECTION, this.#p)
    const sub = this.#plan(field.sub)
    const at = this.#p
    if (wide && (this.#b[at + 1] & 0xf8) !== 0xb8) fail(BAD_DESCRIPTOR, at)
    const start = this.#columnBody(wide, CLASS_COL, NIB_LENGTH)
    const end = this.#p
    const children = this.#count(start, end)
    const lengths = decodeInts(this.#b, this.#p, end, rows, false)
    const counts = lengths.num
    if (counts === null || lengths.decimal) fail(BAD_TABLE, at)
    let left = children
    for (let i = 0; i < rows; i++) {
      const n = counts[i]
      if (n < 0 || n > left) fail(BAD_TABLE, at)
      left -= n
    }
    if (left !== 0) fail(BAD_TABLE, at)
    if (listDepth >= MAX_LIST_DEPTH) fail(TOO_DEEP, at)
    this.#takeRows(children, sub.fields.length)
    this.#enter()
    const elementCols = this.#gather(field.sub, lengths.next, end, children, wide || sub.wide, listDepth + 1)
    const elements: unknown[] = new Array(children)
    this.#buildRows(field.sub, elementCols, children, elements, 0)
    this.#depth--
    const values: unknown[] = new Array(rows)
    let cursor = 0
    for (let i = 0; i < rows; i++) {
      const n = counts[i]
      // A row with none is an empty slice, which a record leaves out: null.
      values[i] = n === 0 ? null : elements.slice(cursor, cursor + n)
      cursor += n
    }
    col.kind = K_VAL
    col.values = values
    this.#p = end
  }

  #takeColumn(field: Field, col: Col, rows: number, wide: boolean): void {
    const at = this.#p
    const pointer = field.op === OP_POINTER
    const op = pointer ? field.elemOp : field.op
    if (!(op <= OP_FLOAT64 || op === OP_STRING)) fail(UNWALKABLE, at)
    const b = this.#b
    const desc = wide ? (at + 1 < this.#e ? b[at + 1] : -1) : (b[at] & 15)
    const cls = (desc >> 4) & 7
    if (op === OP_STRING && this.#dicts !== null && (wide ? desc >> 4 === 0xb : (desc & 13) === NIB_DICT)) {
      this.#dictColumn(col, rows, wide, wide ? (desc & 4) !== 0 : desc === NIB_NULLABLE_DICT)
      return
    }
    const nullable = pointer && (wide
      ? desc >= 0x80 && (cls === CLASS_COL || cls === CLASS_LIST) && (desc & 4) !== 0
      : desc === NIB_NULLABLE)
    col.kind = op === OP_BOOL ? K_BOOL : K_VAL
    if (!nullable && op === OP_STRING) {
      const strings = wide ? this.#wideStrings(at + 1) : this.#narrowStrings(false)
      // A column shorter than the table renders zeros; longer is ignored.
      for (let i = strings.length; i < rows; i++) strings.push('')
      col.values = strings
      return
    }
    const start = this.#columnBody(wide, op === OP_STRING ? CLASS_LIST : CLASS_COL, nullable ? NIB_NULLABLE : NIB_LENGTH)
    const end = this.#p
    let n = rows
    let p = start
    if (nullable) {
      col.valid = this.#presence(start, end, rows)
      n = this.#present
      p += col.valid.length
    }
    if (op === OP_STRING) col.values = this.#elements(p, end, n)
    else col.values = this.#columnValues(op, decodeInts(b, p, end, n, op === OP_FLOAT32 || op === OP_FLOAT64), n, at)
  }

  /** A column field's frame: wide, class `cls` with any detail; narrow, the
   * length form with nibble `nib`. Returns the body start; cursor at its end. */
  #columnBody(wide: boolean, cls: number, nib: number): number {
    const at = this.#p
    if (wide) {
      this.#wideBody(cls)
      return this.#ls
    }
    this.#nhead()
    if (this.#nib !== nib) fail(BAD_DESCRIPTOR, at)
    return this.#fs
  }

  /** The population of the last presence(). */
  #present = 0

  /** A presence bitmap of `rows` rows at `start`; its population goes to
   * `present`. */
  #presence(start: number, end: number, rows: number): Uint8Array {
    const size = (rows + 7) >>> 3
    if (size > end - start) fail(TRUNCATED, start)
    const bits = this.#b.subarray(start, start + size)
    if ((rows & 7) !== 0 && bits[size - 1] >> (rows & 7) !== 0) fail(BAD_TABLE, start)
    let present = 0
    for (let i = 0; i < size; i++) {
      let v = bits[i]
      v = v - ((v >> 1) & 0x55)
      v = (v & 0x33) + ((v >> 2) & 0x33)
      present += (v + (v >> 4)) & 0x0f
    }
    if (present === 0) fail(BAD_TABLE, start)
    this.#present = present
    return bits
  }

  #dictColumn(col: Col, rows: number, wide: boolean, nullable: boolean): void {
    const at = this.#p
    const start = this.#columnBody(wide, CLASS_COL, nullable ? NIB_NULLABLE_DICT : NIB_DICT)
    const end = this.#p
    const dict = (this.#dicts as string[][])[this.#count(start, end)]
    if (dict === undefined) fail(BAD_DICTIONARY, at)
    let p = this.#p
    let n = rows
    if (nullable) {
      col.valid = this.#presence(p, end, rows)
      n = this.#present
      p += col.valid.length
    }
    const ints = decodeInts(this.#b, p, end, n, false)
    const ids = ints.num
    if (ids === null || ints.decimal) fail(BAD_DICTIONARY, at)
    for (let i = 0; i < n; i++) {
      const id = ids[i]
      if (id < 0 || id >= dict.length) fail(BAD_DICTIONARY, at)
    }
    this.#p = end
    col.kind = K_DICT
    col.values = ids
    col.dict = dict
  }

  /** A decoded integer column as its op's values: a Float64Array of numbers
   * (bools as 0/1), or a BigInt array for a 64-bit column past 2^53. */
  #columnValues(op: number, ints: Ints, n: number, at: number): Float64Array | BigInt64Array | BigUint64Array {
    let num = ints.num
    let pairs = ints.pairs as Uint32Array
    if (op === OP_FLOAT64) return ints.decimal && num !== null ? num : pairsToFloat64(pairs)
    if (op === OP_FLOAT32) {
      const out = new Float64Array(n)
      for (let i = 0; i < n; i++) {
        const lo = pairs[2 * i]
        const hi = pairs[2 * i + 1]
        // The bits as a uint32, or as an int32 sign-extended.
        if (hi !== 0 && !(hi === 0xffffffff && lo >= 0x80000000)) fail(TOO_WIDE, at)
        scratch.setUint32(0, lo)
        out[i] = shortest32(scratch.getFloat32(0))
      }
      return out
    }
    if (ints.decimal && num !== null) {
      // A decimal column read as integers is its floats' bit patterns.
      pairs = new Uint32Array(new Float64Array(num).buffer)
      num = null
    }
    if (op === OP_INT64 || op === OP_UINT64) {
      const signed = op === OP_INT64
      if (num !== null) {
        if (signed) return num
        // Decoded signed: an unsigned value past 2^63 came out negative.
        let negative = false
        for (let i = 0; i < n; i++) if (num[i] < 0) negative = true
        if (!negative) return num
        pairs = new Uint32Array(2 * n)
        for (let i = 0; i < n; i++) {
          const v = num[i]
          const hi = Math.floor(v / TWO32)
          pairs[2 * i] = v - hi * TWO32
          pairs[2 * i + 1] = hi
        }
      }
      if (!this.#bigint || pairsSafe(pairs, signed)) return pairsToNum(pairs, signed)
      return pairsToBig(pairs, signed)
    }
    // A narrow integer or a bool: every value within the op's signed width,
    // and an unsigned one stored sign-extended.
    if (num === null) fail(TOO_WIDE, at)
    const limit = op === OP_BOOL ? 0x80 : LIMIT[op <= OP_INT32 ? op : op - 4]
    for (let i = 0; i < n; i++) {
      const v = num[i]
      if (v < -limit || v >= limit) fail(TOO_WIDE, at)
      if (v < 0 && op > OP_INT64) num[i] += 2 * limit
    }
    return num
  }

  #buildRows(at: number, cols: Col[], rows: number, out: unknown[], offset: number): void {
    const cache = this.#cacheOf(at)
    const columns: RowColumn[] = []
    const kinds: number[] = []
    this.#flatten(at, cols, rows, columns, kinds)
    const signature = kinds.join('')
    let build = cache.rows.get(signature) as ReturnType<typeof rowBuilder> | undefined
    if (build === undefined) {
      build = rowBuilder(cache.names, kinds)
      cache.rows.set(signature, build)
    }
    build(out, offset, rows, columns)
  }

  /** A plan's columns in its object's field order, its pages' merged in. */
  #flatten(at: number, cols: Col[] | null, rows: number, columns: RowColumn[], kinds: number[]): void {
    const fields = this.#plans[at].fields
    for (let k = 0; k < fields.length; k++) {
      const field = fields[k]
      const col = cols === null || !cols[k].present ? null : cols[k]
      if (field.pageLink) {
        this.#flatten(field.sub, col === null ? null : col.page, rows, columns, kinds)
        continue
      }
      let column: RowColumn
      if (col === null) {
        // An absent column is its field's zero in every row.
        if (field.op === OP_STRUCT) {
          const values: unknown[] = new Array(rows)
          for (let i = 0; i < rows; i++) values[i] = this.#zeroStruct(field.sub)
          column = { kind: K_VAL, values, dict: null }
        } else column = { kind: K_CONST, values: zeroOf(field.op), dict: null }
      } else if (col.valid !== null) {
        // Present rows only: spread, null elsewhere, and K_VAL reads them.
        const values = expand(col, rows)
        column = { kind: K_VAL, values, dict: null }
      } else column = { kind: col.kind, values: col.values, dict: col.dict }
      columns.push(column)
      kinds.push(column.kind)
    }
  }
}

/** A nullable column's values spread over its rows, null where a row has none. */
function expand(col: Col, rows: number): unknown[] {
  const out: unknown[] = new Array(rows)
  const values = col.values as ArrayLike<unknown>
  const presence = col.valid as Uint8Array
  const dict = col.kind === K_DICT ? (col.dict as string[]) : null
  const bool = col.kind === K_BOOL
  let next = 0
  for (let i = 0; i < rows; i++) {
    if ((presence[i >> 3] >> (i & 7)) & 1) {
      const v = values[next++]
      out[i] = dict !== null ? dict[v as number] : bool ? v === 1 : v
    } else out[i] = null
  }
  return out
}

/** A column's stored value as its row reads it. */
function read(col: Col, v: unknown): unknown {
  return col.kind === K_DICT ? (col.dict as string[])[v as number] : col.kind === K_BOOL ? v === 1 : v
}

// ---- columns() --------------------------------------------------------------

/** A column as columns() hands it back: the shape colbin's wasm `columns()`
 * returns (materialize.ts), so a caller can take either. */
export type Column = (
  | { name: string; kind: 'bool'; length: number; values: (boolean | null)[] }
  | { name: string; kind: 'int'; length: number; values: Float64Array }
  | { name: string; kind: 'bigint'; length: number; values: BigInt64Array | BigUint64Array }
  | { name: string; kind: 'float'; length: number; values: Float64Array }
  | { name: string; kind: 'string'; length: number; values: (string | null)[] }
) & {
  /** A nullable column's validity bitmap: bit `i` (`valid[i >> 3] >> (i & 7) & 1`)
   * set means row `i` holds a value; a typed array holds 0 at a null row. */
  valid?: Uint8Array
}

export type ColumnTable = {
  rowCount: number
  columns: Column[]
  /** The root field the table is, when the root is a struct. */
  field?: string
}

/** A table kept as columns, while the walk carries it to the root. */
class Columns {
  rowCount: number
  columns: Column[]
  constructor(rowCount: number, columns: Column[]) {
    this.rowCount = rowCount
    this.columns = columns
  }
}

/** One field's column over every group: `cols[g]` is the group's column (null
 * or not present: the field's zero, or null rows for a pointer). */
function columnOf(name: string, op: number, pointer: boolean, cols: (Col | null)[], sizes: number[], rows: number): Column {
  const typed = op !== OP_STRING && op !== OP_BOOL
  const big = cols.some((c) => c !== null && c.present && (c.values instanceof BigInt64Array || c.values instanceof BigUint64Array))
  const kind = !typed ? (op === OP_BOOL ? 'bool' : 'string') : op === OP_FLOAT32 || op === OP_FLOAT64 ? 'float' : big ? 'bigint' : 'int'
  // A pointer field's column carries `valid` whether or not a row is null, as
  // the wasm columns() does: the type says nullable, not this message.
  const valid = pointer ? new Uint8Array((rows + 7) >>> 3) : null
  // Absent rows hold the zero, or null for a pointer; a typed array's stay 0.
  const absent = pointer ? null : zeroOf(op)
  let values: unknown[] | Float64Array | BigInt64Array | BigUint64Array
  const only = cols.length === 1 ? cols[0] : null
  if (only !== null && only.present && only.valid === null && only.kind === K_VAL) {
    // One group with every row in its run: the decoded array is the column
    // (cut to the table when a string column runs longer).
    values = only.values as unknown[] | Float64Array
    if (values.length !== rows) values = (values as unknown[]).slice(0, rows)
    if (valid !== null) setBits(valid, 0, rows)
  } else {
    values = !typed ? new Array(rows) : big ? (op === OP_UINT64 ? new BigUint64Array(rows) : new BigInt64Array(rows)) : new Float64Array(rows)
    let row = 0
    for (let g = 0; g < cols.length; g++) {
      const n = sizes[g]
      const c = cols[g]
      if (c === null || !c.present) {
        if (!typed) for (let i = 0; i < n; i++) values[row + i] = absent
      } else if (typed && !big) {
        const src = c.values as Float64Array
        const out = values as Float64Array
        const bits = c.valid
        if (bits === null) {
          out.set(src.length === n ? src : src.subarray(0, n), row)
          if (valid !== null) setBits(valid, row, n)
        } else {
          // The present values spread a presence byte at a time: a full byte
          // copies eight, a partial one walks its set bits.
          let next = 0
          for (let i = 0; i < n; i += 8) {
            let m = bits[i >> 3]
            const at = row + i
            if (m === 0xff) {
              for (let k = 0; k < 8; k++) out[at + k] = src[next + k]
              next += 8
            } else {
              for (; m !== 0; m &= m - 1) out[at + 31 - Math.clz32(m & -m)] = src[next++]
            }
          }
          const v = valid as Uint8Array
          if ((row & 7) === 0) v.set(bits, row >> 3)
          else for (let i = 0; i < n; i++) if ((bits[i >> 3] >> (i & 7)) & 1) v[(row + i) >> 3] |= 1 << ((row + i) & 7)
        }
      } else {
        const src = c.values as ArrayLike<unknown>
        const bits = c.valid
        let next = 0
        for (let i = 0; i < n; i++) {
          const r = row + i
          if (bits !== null && ((bits[i >> 3] >> (i & 7)) & 1) === 0) {
            if (!typed) values[r] = null
            continue
          }
          const v = read(c, src[next++])
          values[r] = big && typeof v === 'number' ? BigInt(v) : v
          if (valid !== null) valid[r >> 3] |= 1 << (r & 7)
        }
      }
      row += n
    }
  }
  const out = { name, kind, length: rows, values } as Column
  if (valid !== null) out.valid = valid
  return out
}

/** Sets bits `[from, from + n)`. */
function setBits(bits: Uint8Array, from: number, n: number): void {
  let i = from
  const end = from + n
  for (; i < end && (i & 7) !== 0; i++) bits[i >> 3] |= 1 << (i & 7)
  if (i < end) bits.fill(0xff, i >> 3, (end >> 3))
  for (i = Math.max(i, end & ~7); i < end; i++) bits[i >> 3] |= 1 << (i & 7)
}

// ---- per-plan caches --------------------------------------------------------

type Cache = {
  /** The object's keys, pages' included, in order; slot k holds names[k]. */
  names: string[]
  /** What each slot holds when the message leaves its field out (§12); null
   * for a by-value struct, built when needed. */
  zeros: unknown[]
  /** The generated literal, compiled on the type's second object: a one-off
   * document pays no compile for types it holds once. */
  make: ((values: unknown[]) => Record<string, unknown>) | null
  /** Objects built before `make` was compiled. */
  built: number
  /** Per plan (this one and its pages): field index to slot. */
  slots: Map<number, Int32Array>
  rows: Map<string, unknown>
  /** The by-value struct fields as (slot, plan index) pairs. */
  structs: number[]
}

/** A struct's object from its slot values. */
function makeObject(cache: Cache, values: unknown[]): Record<string, unknown> {
  if (cache.make !== null) return cache.make(values)
  if (cache.built++ > 0) {
    cache.make = arrayFactory(cache.names)
    return cache.make(values)
  }
  const out: Record<string, unknown> = {}
  const names = cache.names
  for (let k = 0; k < names.length; k++) put(out, names[k], values[k])
  return out
}

function buildCache(plans: Plan[], at: number): Cache {
  const names: string[] = []
  const zeros: unknown[] = []
  const structs: number[] = []
  const slots = new Map<number, Int32Array>()
  const walk = (index: number, level: number): void => {
    if (level > 16) fail(BAD_SECTION)
    const fields = plans[index].fields
    const own = new Int32Array(fields.length)
    slots.set(index, own)
    for (let k = 0; k < fields.length; k++) {
      const field = fields[k]
      if (field.pageLink) {
        walk(field.sub, level + 1)
        continue
      }
      // Two pages may name a field alike: one slot, the last decoded wins.
      const seen = names.indexOf(field.name)
      own[k] = seen >= 0 ? seen : names.length
      if (seen >= 0) continue
      if (field.op === OP_STRUCT) structs.push(names.length, field.sub)
      names.push(field.name)
      zeros.push(zeroOf(field.op))
    }
  }
  walk(at, 0)
  return { names, zeros, make: null, built: 0, slots, rows: new Map(), structs }
}
