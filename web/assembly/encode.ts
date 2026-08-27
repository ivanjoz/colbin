// The JSON-mode message: a schema section, then the columnar body.
//
//   message   := [0x04] [schemaLen:uvarint] schema body
//   schema    := [flags:1] [structCount:uvarint] structDef{structCount} rootDesc
//   structDef := [fieldCount:1] ( [id:1] packed5(name) desc )*
//   desc      := [descFlags:1] extra
//   body      := [recordCount:uvarint] subTable
//   subTable  := [colCount:1] ( [id:1] [flags:1] payload )*
//
// Every layout decision here is colbin's, and the level-2 vectors are what say
// so: the same JSON through Go's MarshalJSON has to give the same bytes.

import { Writer } from './bytes'
import { Diag, D_UNSUPPORTED } from './diag'
import { Doc, K_FLOAT, K_UINT } from './json'
import { SHAPE_SINGLE, Schema } from './infer'
import {
  FT_ARRAY,
  FT_FLOAT,
  FT_INT,
  FT_STRING,
  FT_STRUCT,
  Field,
  SK_BOOL,
  Type,
} from './schema'
import { append as packed5Append } from './packed5'
import { appendArray } from './varint'

const JSON_FORMAT_VERSION: u8 = 0x04
const SCH_RECORDS: u8 = 1 << 0
const SCH_SINGLE_STRUCT: u8 = 1 << 1
const DESC_NULLABLE: u8 = 1 << 3

function writeUvarint(w: Writer, v: u64): void {
  while (v >= 0x80) {
    w.writeByte(<u8>(v | 0x80))
    v >>= 7
  }
  w.writeByte(<u8>v)
}

/** Bits a scalar kind occupies, which is what the varint column is told. */
@inline function bitWidthOf(t: Type): u8 {
  return t.kind == SK_BOOL ? 8 : 64
}

/**
 * Builds the struct table.
 *
 * Go hoists struct types into an indexed table and reaches them through
 * reflect.StructOf, which returns the *same* type for two structurally
 * identical field sets — so `{"a":{"x":1},"b":{"x":2}}` describes one struct
 * twice, not two structs. Interning structurally here is what reproduces that.
 * The index is reserved before the fields are walked, so the order matches Go's
 * depth-first assignment.
 */
class SchemaBuilder {
  types: Array<Type> = []
  defs: Array<Writer | null> = []

  structIndex(t: Type): i32 {
    for (let i = 0; i < this.types.length; i++) {
      if (typeEqual(unchecked(this.types[i]), t)) return i
    }
    const idx = this.types.length
    this.types.push(t)
    this.defs.push(null) // reserved before the walk, as Go reserves it

    const def = new Writer(64)
    def.writeByte(<u8>t.fields.length)
    for (let i = 0; i < t.fields.length; i++) {
      const f = unchecked(t.fields[i])
      def.writeByte(f.id)
      packed5Append(def, f.name)
      this.appendDesc(def, f.type)
    }
    this.defs[idx] = def
    return idx
  }

  /**
   * One descriptor. A nullable field contributes only the nullable bit; the
   * class and the extras come from the type underneath it, which is why a
   * nullable array still describes its element here.
   */
  appendDesc(w: Writer, t: Type): void {
    const nullBit: u8 = t.nullable ? DESC_NULLABLE : 0
    w.writeByte(t.ft | nullBit)
    if (t.ft == FT_INT || t.ft == FT_FLOAT) {
      w.writeByte(t.kind)
      return
    }
    if (t.ft == FT_ARRAY) {
      this.appendDesc(w, t.elem!)
      return
    }
    if (t.ft == FT_STRUCT) {
      writeUvarint(w, <u64>this.structIndex(t))
    }
    // FT_STRING carries no extras.
  }
}

function typeEqual(a: Type, b: Type): bool {
  if (a.ft != b.ft || a.kind != b.kind || a.nullable != b.nullable) return false
  if (a.ft == FT_ARRAY) return typeEqual(a.elem!, b.elem!)
  if (a.ft != FT_STRUCT) return true
  if (a.fields.length != b.fields.length) return false
  for (let i = 0; i < a.fields.length; i++) {
    const fa = unchecked(a.fields[i])
    const fb = unchecked(b.fields[i])
    if (fa.id != fb.id || fa.name.length != fb.name.length) return false
    if (memory.compare(fa.name.dataStart, fb.name.dataStart, <usize>fa.name.length) != 0) return false
    if (!typeEqual(fa.type, fb.type)) return false
  }
  return true
}

export class Encoder {
  doc: Doc
  diag: Diag

  constructor(doc: Doc, diag: Diag) {
    this.doc = doc
    this.diag = diag
  }

  encode(schema: Schema): Uint8Array | null {
    const builder = new SchemaBuilder()
    const rootIndex = builder.structIndex(schema.root)

    const section = new Writer(128)
    let flags = SCH_RECORDS
    if (schema.shape == SHAPE_SINGLE) flags |= SCH_SINGLE_STRUCT
    section.writeByte(flags)
    writeUvarint(section, <u64>builder.defs.length)
    for (let i = 0; i < builder.defs.length; i++) {
      const def = unchecked(builder.defs[i])!
      section.writeBytes(def.buf, 0, def.len)
    }
    // The root struct is never nullable or elided, so its descriptor is the
    // bare class plus its table index.
    section.writeByte(FT_STRUCT)
    writeUvarint(section, <u64>rootIndex)

    const out = new Writer(256)
    out.writeByte(JSON_FORMAT_VERSION)
    writeUvarint(out, <u64>section.len)
    out.writeBytes(section.buf, 0, section.len)

    writeUvarint(out, <u64>schema.records.length)
    const nodes = new Int32Array(schema.records.length)
    for (let i = 0; i < schema.records.length; i++) {
      unchecked((nodes[i] = unchecked(schema.records[i])))
    }
    this.subTable(out, schema.root, nodes)
    if (!this.diag.ok) return null
    return out.take()
  }

  subTable(w: Writer, t: Type, nodes: Int32Array): void {
    w.writeByte(<u8>t.fields.length)
    for (let i = 0; i < t.fields.length; i++) {
      const f = unchecked(t.fields[i])
      w.writeByte(f.id)
      this.column(w, f.type, this.valuesFor(f, i, t, nodes))
      if (!this.diag.ok) return
    }
  }

  /**
   * The child node of field i for every record, or -1 where the key is absent.
   *
   * The scan starts where the last match landed: records usually carry their
   * keys in the order the fields were first seen, so the common case finds it
   * immediately instead of walking the whole object per field.
   */
  valuesFor(f: Field, index: i32, parent: Type, nodes: Int32Array): Int32Array {
    const doc = this.doc
    const out = new Int32Array(nodes.length)
    for (let r = 0; r < nodes.length; r++) {
      const obj = unchecked(nodes[r])
      unchecked((out[r] = -1))
      if (obj < 0) continue
      const keyCount = doc.count(obj)
      for (let k = 0; k < keyCount; k++) {
        const probe = (index + k) % keyCount
        const key = doc.keyOf(obj, probe)
        if (key.length == f.name.length &&
            memory.compare(key.dataStart, f.name.dataStart, <usize>key.length) == 0) {
          unchecked((out[r] = doc.childAt(obj, probe)))
          break
        }
      }
    }
    return out
  }

  /** One column: the nullable wrapper if there is one, then the values. */
  column(w: Writer, t: Type, nodes: Int32Array): void {
    if (!t.nullable) {
      this.elemColumn(w, t, nodes)
      return
    }

    const doc = this.doc
    let hasNulls = false
    for (let i = 0; i < nodes.length; i++) {
      const n = unchecked(nodes[i])
      if (n < 0 || doc.kindOf(n) == 0 /* K_NULL */) {
        hasNulls = true
        break
      }
    }

    if (!hasNulls) {
      w.writeByte(0) // nullFlags: no nulls, so no bitmap
      this.elemColumn(w, t, nodes)
      return
    }

    w.writeByte(1) // nullFlags: a presence bitmap follows
    const bitmapLen = (nodes.length + 7) >> 3
    const bitmap = new Uint8Array(bitmapLen)
    let present = 0
    for (let i = 0; i < nodes.length; i++) {
      const n = unchecked(nodes[i])
      if (n >= 0 && doc.kindOf(n) != 0) {
        const bit: u8 = <u8>1 << <u8>(i & 7) // 1 == present
        unchecked((bitmap[i >> 3] |= bit))
        present++
      }
    }
    w.writeBytes(bitmap, 0, bitmapLen)

    // Only the non-null values are stored, densely.
    const dense = new Int32Array(present)
    let j = 0
    for (let i = 0; i < nodes.length; i++) {
      const n = unchecked(nodes[i])
      if (n >= 0 && doc.kindOf(n) != 0) unchecked((dense[j++] = n))
    }
    this.elemColumn(w, t, dense)
  }

  /** A node's value as f64, whatever numeric kind the scanner gave it. */
  floatValue(node: i32): f64 {
    const doc = this.doc
    const kind = doc.kindOf(node)
    const raw = unchecked(doc.num[node])
    if (kind == K_FLOAT) return reinterpret<f64>(raw)
    if (kind == K_UINT) return <f64>(<u64>raw)
    return <f64>raw // K_INT, and K_BOOL's 0/1
  }

  /** A column's flags byte and payload, with nullability already handled. */
  elemColumn(w: Writer, t: Type, nodes: Int32Array): void {
    const doc = this.doc

    if (t.ft == FT_INT) {
      w.writeByte(FT_INT)
      const vals = new Int64Array(nodes.length)
      for (let i = 0; i < nodes.length; i++) {
        const n = unchecked(nodes[i])
        unchecked((vals[i] = n < 0 ? 0 : unchecked(doc.num[n])))
      }
      appendArray(w, vals, bitWidthOf(t))
      return
    }

    if (t.ft == FT_FLOAT) {
      let empty = true
      const vals = new Float64Array(nodes.length)
      for (let i = 0; i < nodes.length; i++) {
        const n = unchecked(nodes[i])
        // One float promotes the column, so the integers that shared it arrive
        // here as integer nodes and have to be converted rather than
        // reinterpreted - their payload holds a value, not float bits.
        const v = n < 0 ? 0.0 : this.floatValue(n)
        unchecked((vals[i] = v))
        if (v != 0) empty = false
      }
      // Precision bit 4 is set for float64; bit 7 says the column is all zero
      // and carries no payload at all.
      w.writeByte(FT_FLOAT | (1 << 4) | (empty ? <u8>(1 << 7) : 0))
      if (empty) return
      for (let i = 0; i < nodes.length; i++) {
        const bits = reinterpret<u64>(unchecked(vals[i]))
        for (let b = 0; b < 8; b++) w.writeByte(<u8>(bits >> (8 * b)))
      }
      return
    }

    if (t.ft == FT_STRING) {
      w.writeByte(FT_STRING)
      for (let i = 0; i < nodes.length; i++) {
        const n = unchecked(nodes[i])
        packed5Append(w, n < 0 ? EMPTY : doc.strOf(n))
      }
      return
    }

    if (t.ft == FT_STRUCT) {
      w.writeByte(FT_STRUCT)
      this.subTable(w, t, nodes)
      return
    }

    if (t.ft == FT_ARRAY) {
      w.writeByte(FT_ARRAY)
      // A length column, then one flattened column of every element of every
      // record, which is what lets the element codec see them all at once.
      const lengths = new Int64Array(nodes.length)
      let total = 0
      for (let i = 0; i < nodes.length; i++) {
        const n = unchecked(nodes[i])
        const count = n < 0 ? 0 : doc.count(n)
        unchecked((lengths[i] = <i64>count))
        total += count
      }
      w.writeByte(FT_INT)
      appendArray(w, lengths, 64)

      const flat = new Int32Array(total)
      let j = 0
      for (let i = 0; i < nodes.length; i++) {
        const n = unchecked(nodes[i])
        if (n < 0) continue
        const count = doc.count(n)
        for (let k = 0; k < count; k++) unchecked((flat[j++] = doc.childAt(n, k)))
      }
      this.elemColumn(w, t.elem!, flat)
      return
    }

    this.diag.fail(D_UNSUPPORTED, -1, '', 'column type ' + t.ft.toString() + ' is not implemented yet')
  }
}

const EMPTY = new Uint8Array(0)

export function encodeMessage(doc: Doc, schema: Schema, diag: Diag): Uint8Array | null {
  return new Encoder(doc, diag).encode(schema)
}
