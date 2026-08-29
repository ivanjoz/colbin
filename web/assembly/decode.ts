// A colbin JSON-mode message back to JSON text.
//
// This is where PLAN.md §4.3 is spent. The encoder controls its own input and
// can trust its counts; the decoder is handed bytes by a stranger, and the
// AssemblyScript runtime offers neither of the two things Go leans on here — an
// unconditional bounds check, and a recover to turn the resulting panic into an
// error. So every count and every offset is tested against what is left of the
// buffer before anything is read or allocated.
//
// It also accepts columns this module never writes. A Go service that had a real
// struct will send []byte, map and interface{} columns, and refusing to read
// them would make the decoder useless for the case it exists for.

import { ERR_CORRUPT, ERR_TRUNCATED, Reader, Writer } from './bytes'
import { D_CORRUPT, D_UNSUPPORTED, Diag } from './diag'
import { decode as packed5Decode } from './packed5'
import { decodeArray } from './varint'
import {
  FT_ANY,
  FT_ARRAY,
  FT_BYTES,
  FT_FLOAT,
  FT_INT,
  FT_MAP,
  FT_STRING,
  FT_STRUCT,
  SK_BOOL,
} from './schema'

const JSON_FORMAT_VERSION: u8 = 0x04
const BINARY_FORMAT_VERSION: u8 = 0x02
// The same two formats written with omit-empty on: a column of nothing but empty
// values is its type byte alone, with EMPTY_COLUMN set and no payload. The
// version says so outright so a reader that does not know the bit refuses the
// message instead of walking into the next column.
const JSON_FORMAT_VERSION_OMIT_EMPTY: u8 = 0x08
const BINARY_FORMAT_VERSION_OMIT_EMPTY: u8 = 0x06
const EMPTY_COLUMN: u8 = 1 << 7
const SCH_RECORDS: u8 = 1 << 0
const SCH_SINGLE_STRUCT: u8 = 1 << 1
const DESC_NULLABLE: u8 = 1 << 3
const DESC_CYCLIC: u8 = 1 << 4

// Scalar kinds, for rendering: the wire says only "integer column", so the
// schema's kind is what decides signedness and how wide the value really was.
const SK_INT8: u8 = 1
const SK_INT16: u8 = 2
const SK_INT32: u8 = 3
const SK_INT64: u8 = 4
const SK_INT: u8 = 5
const SK_UINT8: u8 = 6
const SK_UINT16: u8 = 7
const SK_UINT32: u8 = 8
const SK_UINT64: u8 = 9
const SK_UINT: u8 = 10
const SK_FLOAT32: u8 = 11

@inline function bitWidthOfKind(kind: u8): u8 {
  if (kind == SK_BOOL || kind == SK_INT8 || kind == SK_UINT8) return 8
  if (kind == SK_INT16 || kind == SK_UINT16) return 16
  if (kind == SK_INT32 || kind == SK_UINT32 || kind == SK_FLOAT32) return 32
  return 64
}

@inline function isUnsigned(kind: u8): bool {
  return kind >= SK_UINT8 && kind <= SK_UINT
}

class Desc {
  ft: u8 = 0
  kind: u8 = 0
  nullable: bool = false
  cyclic: bool = false
  elem: Desc | null = null
  key: Desc | null = null
  val: Desc | null = null
  structIdx: i32 = -1
}

class StructDef {
  ids: Array<u8> = []
  names: Array<Uint8Array> = []
  descs: Array<Desc> = []
}

class Section {
  flags: u8 = 0
  structs: Array<StructDef> = []
  root: Desc = new Desc()
}

// Decoded values. A tagged union, because a column of one type can still hold a
// nested shape and JSON needs the whole record before it can render a row.
export const V_NULL: u8 = 0
export const V_BOOL: u8 = 1
export const V_INT: u8 = 2
export const V_UINT: u8 = 3
export const V_FLOAT: u8 = 4
export const V_STRING: u8 = 5
export const V_BYTES: u8 = 6
export const V_ARRAY: u8 = 7
export const V_OBJECT: u8 = 8

export class Val {
  tag: u8 = V_NULL
  num: i64 = 0
  bytes: Uint8Array | null = null
  items: Array<Val> | null = null
  keys: Array<Uint8Array> | null = null
}

const NULL_VAL = new Val()

function scalarVal(tag: u8, num: i64): Val {
  const v = new Val()
  v.tag = tag
  v.num = num
  return v
}

/** Where one column's bytes are, and what they hold. */
export class Span {
  name: string = ''
  id: i32 = -1
  type: string = ''
  nullable: bool = false
  start: i32 = 0
  end: i32 = 0
  children: Array<Span> = []
}

export class Decoder {
  r: Reader
  diag: Diag
  section: Section = new Section()

  /** Non-null while inspecting: the span list the current subTable fills. */
  spans: Array<Span> | null = null

  /** Value mode has no named columns, so its one span is kept here. */
  valueSpan: Span | null = null

  constructor(buf: Uint8Array, diag: Diag) {
    this.r = new Reader(buf)
    this.diag = diag
  }

  fail(message: string): void {
    this.diag.fail(D_CORRUPT, this.r.pos, '', message)
  }

  @inline checkReader(): bool {
    if (this.r.ok) return true
    this.fail(this.r.err == ERR_TRUNCATED ? 'message ends in the middle of a value' : 'malformed message')
    return false
  }

  uvarint(): i64 {
    let v: u64 = 0
    let shift: u32 = 0
    for (let i = 0; i < 10; i++) {
      const b = this.r.readByte()
      if (!this.checkReader()) return -1
      v |= (<u64>(b & 0x7f)) << shift
      if (b < 0x80) {
        // Anything past i32 range cannot be a count in this buffer, and
        // returning it would hand a bad allocation to the caller.
        if (v > <u64>0x7fffffff) {
          this.fail('a length or count in the message is larger than the message could hold')
          return -1
        }
        return <i64>v
      }
      shift += 7
    }
    this.fail('malformed varint')
    return -1
  }

  /**
   * Rejects a count that cannot fit in what is left of the buffer.
   *
   * The cheapest possible value is one byte, so a count above the remaining
   * length is a corrupt message however the columns are laid out. This is the
   * guard that turns "ask for 2^40 elements" into a diagnostic instead of an
   * allocation the tab does not come back from.
   */
  plausible(count: i64, name: string): bool {
    if (count < 0) return false
    if (count > <i64>this.r.remaining) {
      this.fail(name + ' of ' + count.toString() + ' exceeds the ' + this.r.remaining.toString() +
        ' bytes left in the message')
      return false
    }
    return true
  }

  // ---- schema section ------------------------------------------------------

  parseSection(): bool {
    const version = this.r.readByte()
    if (!this.checkReader()) return false
    if ((version & 1) == 1) {
      this.diag.fail(D_UNSUPPORTED, 0, '', 'this is a compact-mode message, which is not implemented yet')
      return false
    }
    if (version == BINARY_FORMAT_VERSION || version == BINARY_FORMAT_VERSION_OMIT_EMPTY) {
      this.diag.fail(D_UNSUPPORTED, 0, '',
        'this is a binary-mode message: it carries no schema, so it can only be read with the type that wrote it')
      return false
    }
    if (version != JSON_FORMAT_VERSION && version != JSON_FORMAT_VERSION_OMIT_EMPTY) {
      this.fail('unknown format version ' + version.toString())
      return false
    }

    const schemaLen = this.uvarint()
    if (schemaLen < 0) return false
    if (!this.plausible(schemaLen, 'schema length')) return false
    const schemaEnd = this.r.pos + <i32>schemaLen

    const section = this.section
    section.flags = this.r.readByte()
    if (!this.checkReader()) return false

    const structCount = this.uvarint()
    if (structCount < 0 || !this.plausible(structCount, 'struct count')) return false

    // Reserved before the fields are read, so a descriptor that refers back to a
    // struct still being read resolves to the slot already assigned to it.
    for (let i: i64 = 0; i < structCount; i++) section.structs.push(new StructDef())

    for (let i: i64 = 0; i < structCount; i++) {
      const def = unchecked(section.structs[<i32>i])
      const fieldCount = <i32>this.r.readByte()
      if (!this.checkReader()) return false
      for (let f = 0; f < fieldCount; f++) {
        const id = this.r.readByte()
        if (!this.checkReader()) return false
        const name = new Writer(32)
        if (!packed5Decode(this.r, name)) {
          this.fail('malformed field name in the schema')
          return false
        }
        const d = this.desc()
        if (d == null) return false
        def.ids.push(id)
        def.names.push(name.take())
        def.descs.push(d)
      }
    }

    const root = this.desc()
    if (root == null) return false
    section.root = root

    if (this.r.pos != schemaEnd) {
      this.fail('the schema section does not end where its length says it does')
      return false
    }
    return true
  }

  desc(): Desc | null {
    const flags = this.r.readByte()
    if (!this.checkReader()) return null
    const d = new Desc()
    d.ft = flags & 0x07
    d.nullable = (flags & DESC_NULLABLE) != 0
    d.cyclic = (flags & DESC_CYCLIC) != 0

    if (d.ft == FT_INT || d.ft == FT_FLOAT) {
      d.kind = this.r.readByte()
      if (!this.checkReader()) return null
      return d
    }
    if (d.ft == FT_ARRAY) {
      d.elem = this.desc()
      return d.elem == null ? null : d
    }
    if (d.ft == FT_STRUCT) {
      const idx = this.uvarint()
      if (idx < 0) return null
      if (idx >= <i64>this.section.structs.length) {
        this.fail('the schema refers to struct ' + idx.toString() + ', which it does not define')
        return null
      }
      d.structIdx = <i32>idx
      return d
    }
    if (d.ft == FT_MAP) {
      d.key = this.desc()
      if (d.key == null) return null
      d.val = this.desc()
      return d.val == null ? null : d
    }
    return d // string, bytes, any carry no extras
  }

  // ---- body ----------------------------------------------------------------

  decodeBody(): Array<Val> | null {
    const section = this.section
    if ((section.flags & SCH_RECORDS) == 0) {
      // Value mode: one element column holding one value, and no record count
      // in front of it because there is no record to count.
      let span: Span | null = null
      if (this.spans != null) {
        span = new Span()
        span.name = '(value)'
        span.type = typeName(section.root)
        span.nullable = section.root.nullable
        span.start = this.r.pos
        this.spans = span.children
      }
      const one = this.column(section.root, 1)
      if (span != null) {
        const children = this.spans!
        this.spans = null
        span.end = this.r.pos
        span.children = children
        this.valueSpan = span
      }
      return one
    }
    const count = this.uvarint()
    if (count < 0 || !this.plausible(count, 'record count')) return null
    if (section.root.ft != FT_STRUCT) {
      this.fail('a records-mode message must have a struct at its root')
      return null
    }
    // Records mode writes the subTable straight after the count: the root
    // struct has no column header of its own.
    return this.subTable(section.root.structIdx, <i32>count)
  }

  subTable(structIdx: i32, n: i32): Array<Val> | null {
    const section = this.section
    if (structIdx < 0 || structIdx >= section.structs.length) {
      this.fail('column refers to a struct the schema does not define')
      return null
    }
    const def = unchecked(section.structs[structIdx])
    const colCount = <i32>this.r.readByte()
    if (!this.checkReader()) return null

    const rows = new Array<Val>(n)
    for (let i = 0; i < n; i++) {
      const v = new Val()
      v.tag = V_OBJECT
      v.items = new Array<Val>()
      v.keys = new Array<Uint8Array>()
      unchecked((rows[i] = v))
    }

    for (let c = 0; c < colCount; c++) {
      const id = this.r.readByte()
      if (!this.checkReader()) return null
      let slot = -1
      for (let f = 0; f < def.ids.length; f++) {
        if (unchecked(def.ids[f]) == id) {
          slot = f
          break
        }
      }
      if (slot < 0) {
        this.fail('field id ' + id.toString() + ' is not in the schema')
        return null
      }
      const desc = unchecked(def.descs[slot])
      const name = unchecked(def.names[slot])

      // The span is opened before the column is read and closed after, so a
      // nested column's bytes land inside its parent's rather than beside them.
      let span: Span | null = null
      let parentSpans: Array<Span> | null = null
      if (this.spans != null) {
        span = new Span()
        span.name = String.UTF8.decodeUnsafe(name.dataStart, <usize>name.length, false)
        span.id = <i32>id
        span.type = typeName(desc)
        span.nullable = desc.nullable
        span.start = this.r.pos - 1 // the field id byte belongs to the column
        parentSpans = this.spans
        this.spans = span.children
      }

      const values = this.column(desc, n)

      if (span != null) {
        this.spans = parentSpans
        span.end = this.r.pos
        parentSpans!.push(span)
      }
      if (values == null) return null
      for (let i = 0; i < n; i++) {
        unchecked(rows[i]).keys!.push(name)
        unchecked(rows[i]).items!.push(unchecked(values[i]))
      }
    }
    return rows
  }

  column(d: Desc, n: i32): Array<Val> | null {
    if (d.nullable) return this.nullableColumn(d, n)

    if (d.ft == FT_INT) {
      const raw = this.intColumn(n, bitWidthOfKind(d.kind))
      if (raw == null) return null
      const out = new Array<Val>(n)
      const unsigned = isUnsigned(d.kind)
      for (let i = 0; i < n; i++) {
        const v = unchecked(raw[i])
        if (d.kind == SK_BOOL) unchecked((out[i] = scalarVal(V_BOOL, v != 0 ? 1 : 0)))
        else unchecked((out[i] = scalarVal(unsigned ? V_UINT : V_INT, v)))
      }
      return out
    }

    if (d.ft == FT_FLOAT) return this.floatColumn(d, n)

    const flags = this.r.readByte() // every other column writes its class here
    if (!this.checkReader()) return null
    if ((flags & 0x07) != d.ft) {
      this.fail('column class ' + (flags & 0x07).toString() + ' does not match the schema')
      return null
    }

    if (d.ft == FT_STRING) {
      const out = new Array<Val>(n)
      if ((flags & EMPTY_COLUMN) != 0) {
        // Nothing but "", and no frames follow.
        for (let i = 0; i < n; i++) {
          const v = new Val()
          v.tag = V_STRING
          v.bytes = new Writer(0).take()
          unchecked((out[i] = v))
        }
        return out
      }
      for (let i = 0; i < n; i++) {
        const w = new Writer(32)
        if (!packed5Decode(this.r, w)) {
          this.fail('malformed string frame')
          return null
        }
        const v = new Val()
        v.tag = V_STRING
        v.bytes = w.take()
        unchecked((out[i] = v))
      }
      return out
    }

    if (d.ft == FT_BYTES) {
      const lengths = this.intColumn(n, 64)
      if (lengths == null) return null
      const out = new Array<Val>(n)
      for (let i = 0; i < n; i++) {
        const l = unchecked(lengths[i])
        if (l < 0 || !this.plausible(l, 'byte-column length')) return null
        const v = new Val()
        v.tag = V_BYTES
        const b = new Uint8Array(<i32>l)
        memory.copy(b.dataStart, this.r.buf.dataStart + <usize>this.r.pos, <usize>l)
        this.r.pos += <i32>l
        v.bytes = b
        unchecked((out[i] = v))
      }
      return out
    }

    if (d.ft == FT_STRUCT) return this.subTable(d.structIdx, n)
    if (d.ft == FT_ARRAY) return this.arrayColumn(d, n)
    if (d.ft == FT_MAP) return this.mapColumn(d, n)
    if (d.ft == FT_ANY) {
      const out = new Array<Val>(n)
      for (let i = 0; i < n; i++) {
        const v = this.anyValue(0)
        if (v == null) return null
        unchecked((out[i] = v))
      }
      return out
    }

    this.fail('unknown column class ' + d.ft.toString())
    return null
  }

  intColumn(n: i32, bitWidth: u8): Int64Array | null {
    const flags = this.r.readByte()
    if (!this.checkReader()) return null
    if ((flags & 0x07) != FT_INT) {
      this.fail('expected an integer column')
      return null
    }
    const out = new Int64Array(n)
    // Nothing but zeros, and no frame follows. Every column framed by a length
    // sub-column -- bytes, arrays, maps -- collapses through this one.
    if ((flags & EMPTY_COLUMN) != 0) return out
    if (!decodeArray(this.r, n, out, bitWidth)) {
      this.checkReader()
      this.fail('malformed integer column')
      return null
    }
    return out
  }

  floatColumn(d: Desc, n: i32): Array<Val> | null {
    const flags = this.r.readByte()
    if (!this.checkReader()) return null
    if ((flags & 0x07) != FT_FLOAT) {
      this.fail('expected a float column')
      return null
    }
    const width: i32 = ((flags >> 4) & 7) == 1 ? 64 : 32
    const out = new Array<Val>(n)
    if (((flags >> 7) & 1) == 1) {
      for (let i = 0; i < n; i++) unchecked((out[i] = scalarVal(V_FLOAT, 0)))
      return out
    }
    if (!this.r.has(n * (width >> 3))) {
      this.fail('float column runs past the end of the message')
      return null
    }
    for (let i = 0; i < n; i++) {
      let bits: u64 = 0
      const byteCount = width >> 3
      for (let b = 0; b < byteCount; b++) bits |= (<u64>this.r.readByte()) << (8 * b)
      const value: f64 = width == 64 ? reinterpret<f64>(bits) : <f64>reinterpret<f32>(<u32>bits)
      unchecked((out[i] = scalarVal(V_FLOAT, reinterpret<i64>(value))))
    }
    return out
  }

  arrayColumn(d: Desc, n: i32): Array<Val> | null {
    const lengths = this.intColumn(n, 64)
    if (lengths == null) return null
    let total: i64 = 0
    for (let i = 0; i < n; i++) {
      const l = unchecked(lengths[i])
      if (l < 0) {
        this.fail('negative array length')
        return null
      }
      total += l
    }
    if (!this.plausible(total, 'total element count')) return null

    let flat: Array<Val> | null = null
    // The encoder elides an empty column only for a self-referential element
    // type, and both sides decide that from the schema rather than a marker.
    if (!(total == 0 && d.elem!.cyclic)) {
      flat = this.column(d.elem!, <i32>total)
      if (flat == null) return null
    }

    const out = new Array<Val>(n)
    let at = 0
    for (let i = 0; i < n; i++) {
      const l = <i32>unchecked(lengths[i])
      if (l == 0) {
        // An empty slice and nil are the same on the wire, and both render null.
        unchecked((out[i] = NULL_VAL))
        continue
      }
      const v = new Val()
      v.tag = V_ARRAY
      const items = new Array<Val>()
      for (let k = 0; k < l; k++) items.push(unchecked(flat![at + k]))
      v.items = items
      at += l
      unchecked((out[i] = v))
    }
    return out
  }

  mapColumn(d: Desc, n: i32): Array<Val> | null {
    const lengths = this.intColumn(n, 64)
    if (lengths == null) return null
    let total: i64 = 0
    for (let i = 0; i < n; i++) {
      const l = unchecked(lengths[i])
      if (l < 0) {
        this.fail('negative map length')
        return null
      }
      total += l
    }
    if (!this.plausible(total, 'total entry count')) return null

    let keys: Array<Val> | null = null
    let vals: Array<Val> | null = null
    if (!(total == 0 && d.key!.cyclic)) {
      keys = this.column(d.key!, <i32>total)
      if (keys == null) return null
    }
    if (!(total == 0 && d.val!.cyclic)) {
      vals = this.column(d.val!, <i32>total)
      if (vals == null) return null
    }

    const out = new Array<Val>(n)
    let at = 0
    for (let i = 0; i < n; i++) {
      const l = <i32>unchecked(lengths[i])
      if (l == 0) {
        unchecked((out[i] = NULL_VAL))
        continue
      }
      const v = new Val()
      v.tag = V_OBJECT
      const ks = new Array<Uint8Array>()
      const items = new Array<Val>()
      for (let k = 0; k < l; k++) {
        // JSON object keys are strings, so a non-string key is rendered as one,
        // which is what encoding/json does too.
        ks.push(keyBytes(unchecked(keys![at + k])))
        items.push(unchecked(vals![at + k]))
        at++
      }
      v.keys = ks
      v.items = items
      unchecked((out[i] = v))
    }
    return out
  }

  nullableColumn(d: Desc, n: i32): Array<Val> | null {
    const nullFlags = this.r.readByte()
    if (!this.checkReader()) return null
    const present = new Uint8Array(n)
    let numPresent = 0

    if ((nullFlags & 1) == 1) {
      const bitmapLen = (n + 7) >> 3
      if (!this.r.has(bitmapLen)) {
        this.fail('presence bitmap runs past the end of the message')
        return null
      }
      const at = this.r.pos
      this.r.pos += bitmapLen
      for (let i = 0; i < n; i++) {
        const bit = (unchecked(this.r.buf[at + (i >> 3)]) >> <u8>(i & 7)) & 1
        if (bit == 1) {
          unchecked((present[i] = 1))
          numPresent++
        }
      }
    } else {
      for (let i = 0; i < n; i++) unchecked((present[i] = 1))
      numPresent = n
    }

    const out = new Array<Val>(n)
    if (numPresent == 0 && d.cyclic) {
      for (let i = 0; i < n; i++) unchecked((out[i] = NULL_VAL))
      return out
    }

    const inner = new Desc()
    inner.ft = d.ft
    inner.kind = d.kind
    inner.cyclic = d.cyclic
    inner.elem = d.elem
    inner.key = d.key
    inner.val = d.val
    inner.structIdx = d.structIdx
    const dense = this.column(inner, numPresent)
    if (dense == null) return null

    let k = 0
    for (let i = 0; i < n; i++) {
      unchecked((out[i] = unchecked(present[i]) == 1 ? unchecked(dense[k++]) : NULL_VAL))
    }
    return out
  }

  /** One self-describing value from an interface{} column. */
  anyValue(depth: i32): Val | null {
    if (depth > 64) {
      this.fail('any-value nesting is deeper than 64 levels')
      return null
    }
    const tag = this.r.readByte()
    if (!this.checkReader()) return null

    if (tag == 0) return NULL_VAL
    if (tag == 1) return scalarVal(V_BOOL, 0)
    if (tag == 2) return scalarVal(V_BOOL, 1)
    if (tag == 3 || tag == 4) {
      const one = this.intColumn(1, 64)
      if (one == null) return null
      return scalarVal(tag == 3 ? V_INT : V_UINT, unchecked(one[0]))
    }
    if (tag == 5) {
      if (!this.r.has(8)) {
        this.fail('float value runs past the end of the message')
        return null
      }
      let bits: u64 = 0
      for (let b = 0; b < 8; b++) bits |= (<u64>this.r.readByte()) << (8 * b)
      return scalarVal(V_FLOAT, <i64>bits)
    }
    if (tag == 6) {
      const w = new Writer(32)
      if (!packed5Decode(this.r, w)) {
        this.fail('malformed string in an any value')
        return null
      }
      const v = new Val()
      v.tag = V_STRING
      v.bytes = w.take()
      return v
    }
    if (tag == 7) {
      const len = this.uvarint()
      if (len < 0 || !this.plausible(len, 'byte length')) return null
      const b = new Uint8Array(<i32>len)
      memory.copy(b.dataStart, this.r.buf.dataStart + <usize>this.r.pos, <usize>len)
      this.r.pos += <i32>len
      const v = new Val()
      v.tag = V_BYTES
      v.bytes = b
      return v
    }
    if (tag == 8 || tag == 9) {
      const count = this.uvarint()
      if (count < 0 || !this.plausible(count, 'any-value element count')) return null
      const v = new Val()
      v.tag = tag == 8 ? V_ARRAY : V_OBJECT
      const items = new Array<Val>()
      const keys = new Array<Uint8Array>()
      for (let i: i64 = 0; i < count; i++) {
        if (tag == 9) {
          const w = new Writer(32)
          if (!packed5Decode(this.r, w)) {
            this.fail('malformed key in an any map')
            return null
          }
          keys.push(w.take())
        }
        const item = this.anyValue(depth + 1)
        if (item == null) return null
        items.push(item)
      }
      v.items = items
      if (tag == 9) v.keys = keys
      return v
    }

    this.fail('unknown any-value tag ' + tag.toString())
    return null
  }
}

/** A map key rendered as a JSON object key, as encoding/json does. */
function keyBytes(v: Val): Uint8Array {
  if (v.tag == V_STRING || v.tag == V_BYTES) return v.bytes!
  const w = new Writer(24)
  writeScalar(w, v)
  return w.take()
}

// ---- rendering --------------------------------------------------------------

function writeScalar(w: Writer, v: Val): void {
  if (v.tag == V_BOOL) {
    writeAscii(w, v.num != 0 ? 'true' : 'false')
  } else if (v.tag == V_INT) {
    writeAscii(w, v.num.toString())
  } else if (v.tag == V_UINT) {
    writeAscii(w, (<u64>v.num).toString())
  } else if (v.tag == V_FLOAT) {
    writeFloat(w, reinterpret<f64>(v.num))
  } else {
    writeAscii(w, 'null')
  }
}

/**
 * A float the way encoding/json writes one, so a Go service and this module
 * render the same value the same way.
 *
 * AssemblyScript's toString already agrees with Go on everything that matters:
 * shortest round-trip digits, and the same switch to exponent form at 1e21 and
 * 1e-6. It differs in one detail — it writes a whole number as "1.0" where Go
 * writes "1" — so that suffix comes off here.
 */
function writeFloat(w: Writer, f: f64): void {
  // JSON has no infinity and no NaN. encoding/json writes null; so does this.
  if (!isFinite(f)) {
    writeAscii(w, 'null')
    return
  }
  const s = f.toString()
  const n = s.length
  if (n > 2 && s.charCodeAt(n - 2) == 0x2e && s.charCodeAt(n - 1) == 0x30) {
    writeAscii(w, s.substring(0, n - 2))
    return
  }
  writeAscii(w, s)
}

function writeAscii(w: Writer, s: string): void {
  const bytes = Uint8Array.wrap(String.UTF8.encode(s, false))
  w.writeBytes(bytes, 0, bytes.length)
}

const BASE64: StaticArray<u8> = [
  0x41, 0x42, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48, 0x49, 0x4a, 0x4b, 0x4c, 0x4d, 0x4e, 0x4f, 0x50,
  0x51, 0x52, 0x53, 0x54, 0x55, 0x56, 0x57, 0x58, 0x59, 0x5a, 0x61, 0x62, 0x63, 0x64, 0x65, 0x66,
  0x67, 0x68, 0x69, 0x6a, 0x6b, 0x6c, 0x6d, 0x6e, 0x6f, 0x70, 0x71, 0x72, 0x73, 0x74, 0x75, 0x76,
  0x77, 0x78, 0x79, 0x7a, 0x30, 0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x37, 0x38, 0x39, 0x2b, 0x2f,
]

/** Standard base64 with padding, which is how encoding/json renders []byte. */
function writeBase64(w: Writer, b: Uint8Array): void {
  let i = 0
  while (i + 2 < b.length) {
    const n = (<u32>unchecked(b[i]) << 16) | (<u32>unchecked(b[i + 1]) << 8) | <u32>unchecked(b[i + 2])
    w.writeByte(unchecked(BASE64[(n >> 18) & 63]))
    w.writeByte(unchecked(BASE64[(n >> 12) & 63]))
    w.writeByte(unchecked(BASE64[(n >> 6) & 63]))
    w.writeByte(unchecked(BASE64[n & 63]))
    i += 3
  }
  const left = b.length - i
  if (left == 1) {
    const n = <u32>unchecked(b[i]) << 16
    w.writeByte(unchecked(BASE64[(n >> 18) & 63]))
    w.writeByte(unchecked(BASE64[(n >> 12) & 63]))
    w.writeByte(0x3d)
    w.writeByte(0x3d)
  } else if (left == 2) {
    const n = (<u32>unchecked(b[i]) << 16) | (<u32>unchecked(b[i + 1]) << 8)
    w.writeByte(unchecked(BASE64[(n >> 18) & 63]))
    w.writeByte(unchecked(BASE64[(n >> 12) & 63]))
    w.writeByte(unchecked(BASE64[(n >> 6) & 63]))
    w.writeByte(0x3d)
  }
}

/** A JSON string literal from raw UTF-8 bytes. */
function writeJSONString(w: Writer, b: Uint8Array): void {
  w.writeByte(0x22)
  for (let i = 0; i < b.length; i++) {
    const c = unchecked(b[i])
    if (c == 0x22 || c == 0x5c) {
      w.writeByte(0x5c)
      w.writeByte(c)
    } else if (c == 0x0a) {
      w.writeByte(0x5c)
      w.writeByte(0x6e)
    } else if (c == 0x0d) {
      w.writeByte(0x5c)
      w.writeByte(0x72)
    } else if (c == 0x09) {
      w.writeByte(0x5c)
      w.writeByte(0x74)
    } else if (c < 0x20) {
      writeAscii(w, '\\u00')
      w.writeByte(c >> 4 < 10 ? 0x30 + (c >> 4) : 0x61 + (c >> 4) - 10)
      const lo = c & 0x0f
      w.writeByte(lo < 10 ? 0x30 + lo : 0x61 + lo - 10)
    } else {
      w.writeByte(c)
    }
  }
  w.writeByte(0x22)
}

function writeValue(w: Writer, v: Val): void {
  if (v.tag == V_STRING) {
    writeJSONString(w, v.bytes!)
    return
  }
  if (v.tag == V_BYTES) {
    const b64 = new Writer(v.bytes!.length * 2 + 4)
    writeBase64(b64, v.bytes!)
    writeJSONString(w, b64.take())
    return
  }
  if (v.tag == V_ARRAY) {
    w.writeByte(0x5b)
    const items = v.items!
    for (let i = 0; i < items.length; i++) {
      if (i > 0) w.writeByte(0x2c)
      writeValue(w, unchecked(items[i]))
    }
    w.writeByte(0x5d)
    return
  }
  if (v.tag == V_OBJECT) {
    w.writeByte(0x7b)
    const items = v.items!
    const keys = v.keys!
    for (let i = 0; i < items.length; i++) {
      if (i > 0) w.writeByte(0x2c)
      writeJSONString(w, unchecked(keys[i]))
      w.writeByte(0x3a)
      writeValue(w, unchecked(items[i]))
    }
    w.writeByte(0x7d)
    return
  }
  writeScalar(w, v)
}

/** A decoded message: the records, and whether it renders as a lone object. */
export class Decoded {
  rows: Array<Val> = []
  single: bool = false
  /** The message holds one value rather than a table of records. */
  valueMode: bool = false
}

/** Reads a message into its values, without rendering anything. */
export function decodeValues(buf: Uint8Array, diag: Diag): Decoded | null {
  const dec = new Decoder(buf, diag)
  if (!dec.parseSection()) return null
  const rows = dec.decodeBody()
  if (rows == null) return null

  const out = new Decoded()
  out.rows = rows
  out.valueMode = (dec.section.flags & SCH_RECORDS) == 0
  out.single = (dec.section.flags & SCH_SINGLE_STRUCT) != 0
  if (out.single && !out.valueMode && out.rows.length != 1) {
    dec.fail('a lone-struct message must carry exactly one record')
    return null
  }
  return out
}

/**
 * A message to JSON text.
 *
 * Keys come out in wire order, which is the order the encoder recorded them in.
 * Go's DecodeJSON builds a map per record and encoding/json sorts the keys, so
 * the two differ textually on purpose (PLAN.md §6) — here a round trip returns
 * the caller's own key order.
 */
export function decodeMessage(buf: Uint8Array, diag: Diag): Uint8Array | null {
  const decoded = decodeValues(buf, diag)
  if (decoded == null) return null

  const w = new Writer(256)
  const rows = decoded.rows
  if (decoded.valueMode) {
    writeValue(w, unchecked(rows[0]))
  } else if (decoded.single) {
    writeValue(w, unchecked(rows[0]))
  } else {
    w.writeByte(0x5b)
    for (let i = 0; i < rows.length; i++) {
      if (i > 0) w.writeByte(0x2c)
      writeValue(w, unchecked(rows[i]))
    }
    w.writeByte(0x5d)
  }
  return w.take()
}

/** A column's type, as the inspector shows it. */
function typeName(d: Desc): string {
  if (d.ft == FT_INT) {
    if (d.kind == SK_BOOL) return 'bool'
    return (isUnsigned(d.kind) ? 'uint' : 'int') + bitWidthOfKind(d.kind).toString()
  }
  if (d.ft == FT_FLOAT) return 'float' + bitWidthOfKind(d.kind).toString()
  if (d.ft == FT_STRING) return 'string'
  if (d.ft == FT_BYTES) return 'bytes'
  if (d.ft == FT_ARRAY) return '[]' + typeName(d.elem!)
  if (d.ft == FT_STRUCT) return 'struct'
  if (d.ft == FT_MAP) return 'map'
  return 'any'
}

function writeSpans(w: Writer, spans: Array<Span>): void {
  w.writeByte(0x5b)
  for (let i = 0; i < spans.length; i++) {
    if (i > 0) w.writeByte(0x2c)
    const s = unchecked(spans[i])
    writeAscii(w, '{"name":')
    writeJSONString(w, Uint8Array.wrap(String.UTF8.encode(s.name, false)))
    writeAscii(w, ',"id":' + s.id.toString())
    writeAscii(w, ',"type":"' + s.type + '"')
    writeAscii(w, ',"nullable":' + (s.nullable ? 'true' : 'false'))
    writeAscii(w, ',"start":' + s.start.toString())
    writeAscii(w, ',"end":' + s.end.toString())
    writeAscii(w, ',"bytes":' + (s.end - s.start).toString())
    writeAscii(w, ',"children":')
    writeSpans(w, s.children)
    w.writeByte(0x7d)
  }
  w.writeByte(0x5d)
}

/**
 * The column tree of a message, with the byte span of every column.
 *
 * Driven by the decoder rather than by a second walk, so it cannot drift from
 * what decoding actually reads — and so the spans are a check on the decoder
 * too: if they do not tile the buffer, something is being read twice or skipped.
 */
export function inspectMessage(buf: Uint8Array, diag: Diag): Uint8Array | null {
  const dec = new Decoder(buf, diag)
  if (!dec.parseSection()) return null
  const schemaEnd = dec.r.pos

  const top = new Array<Span>()
  dec.spans = top
  const rows = dec.decodeBody()
  if (rows == null) return null
  if (dec.valueSpan != null) top.push(dec.valueSpan!)

  const w = new Writer(512)
  writeAscii(w, '{"totalBytes":' + buf.length.toString())
  writeAscii(w, ',"schemaBytes":' + schemaEnd.toString())
  writeAscii(w, ',"recordCount":' + rows.length.toString())
  const shape = (dec.section.flags & SCH_RECORDS) == 0
    ? 'value'
    : ((dec.section.flags & SCH_SINGLE_STRUCT) != 0 ? 'object' : 'array')
  writeAscii(w, ',"shape":"' + shape + '"')
  writeAscii(w, ',"columns":')
  writeSpans(w, top)
  w.writeByte(0x7d)
  return w.take()
}
