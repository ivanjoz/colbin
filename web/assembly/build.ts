// The body: a plan and a parsed document in, a message out.
//
// The body is generated from the **plan**, never from the raw JSON values
// (PLAN.md §4.5, rule 2). One source of truth means a value that does not fit
// its column cannot be written — it is a conflict at fill time instead of a byte
// that decodes as something else. So every function here takes the field it is
// writing and looks the value up, rather than taking a value and deciding what
// it is.
//
// # Two widths, written twice
//
// Exactly as the readers are, and for the format's reason rather than the
// compiler's: the two writers produce different bytes for the same field and
// sharing them behind an interface would cost a virtual call per field to save a
// switch that is already resolved per run.
//
// # A slice of structs picks its own layout
//
// Past `TABLE_THRESHOLD` rows a `[]struct` is transposed into a table — one key
// per column rather than one per field per row — provided every field of the
// element is a column the codec carries. The threshold is Go's, and it has to
// be: the two encoders must agree on shape for the same data or the vectors
// diverge on a record that is otherwise identical.

import { Writer } from './bytes'
import { Diag, D_LIMIT, D_UNSUPPORTED } from './diag'
import { Doc, K_ARRAY, K_BOOL, K_FLOAT, K_INT, K_NULL, K_OBJECT, K_STRING, K_UINT } from './json'
import {
  OP_BOOL,
  OP_FLOAT32,
  OP_FLOAT64,
  OP_INT64,
  OP_INT64S,
  OP_POINTER,
  OP_STRING,
  OP_STRINGS,
  OP_STRUCT,
  OP_STRUCTS,
  OP_UINT64,
  OP_UINT64S,
  Plan,
  PlanField,
  widthOfOp,
} from './plan'
import { NarrowWriter } from './wire/narrowwrite'
import { WideWriter } from './wire/widewrite'

/** Where a table overtakes a list of structs. Measured in Go, and shared rather
 * than re-derived: the two encoders must pick the same shape for the same data. */
export const TABLE_THRESHOLD: i32 = 8

/** Root descriptors. The class is STRUCT, which is what puts every colbin
 * message in 0xD0..0xDF. */
const ROOT_NARROW: u8 = 0xd0
const ROOT_WIDE: u8 = 0xd8
const ROOT_SCHEMA: u8 = 0x04

/** How deep the encoder will recurse. At or below the decoder's bound, so this
 * module can always read back what it just wrote (PLAN.md §4.5, rule 4). */
const MAX_DEPTH: i32 = 64

export class Builder {
  doc: Doc
  diag: Diag
  out: Writer = new Writer(1024)
  /**
   * Whether to offer each string to the packed encoding.
   *
   * A writer setting, exactly as Go's `SetPacked5` is, and off for the same
   * reason: a raw blob is a sub-slice of the message on the way out and a memcpy
   * on the way in, where a packed one is a pass over every character on both
   * sides. The trade is a third of a short token against that pass, which is
   * worth taking on a wire that is size-bound and not on one that is not.
   *
   * It is never a correctness question. The encoding is recorded in each
   * string's own descriptor, so a decoder reads either form without being told —
   * and the encoder tries the packed form and keeps it only when it is smaller,
   * so turning this on cannot make a message larger.
   */
  packStrings: bool = false
  private depth: i32 = 0
  /** Reused across every column of every table, because a column is written out
   * before the next is gathered. */
  private scratch: Int64Array = new Int64Array(0)

  constructor(doc: Doc, diag: Diag) {
    this.doc = doc
    this.diag = diag
  }

  @inline private get ok(): bool {
    return this.diag.ok
  }

  private enter(): bool {
    if (this.depth >= MAX_DEPTH) {
      this.diag.fail(D_LIMIT, -1, '', 'the document nests deeper than the encoder writes')
      return false
    }
    this.depth++
    return true
  }

  @inline private leave(): void {
    this.depth--
  }

  private reserve(rows: i32): Int64Array {
    if (this.scratch.length < rows) this.scratch = new Int64Array(rows)
    return this.scratch
  }

  /** The root byte and then the run. */
  build(plan: Plan, node: i32, withSchema: bool): void {
    let root = plan.isWide ? ROOT_WIDE : ROOT_NARROW
    if (withSchema) root |= ROOT_SCHEMA
    this.out.writeByte(root)
    this.run(plan, node)
  }

  /** A key run at whichever width the plan declared. */
  run(plan: Plan, node: i32): void {
    if (plan.isWide) this.wideRun(plan, node)
    else this.narrowRun(plan, node)
  }

  // ---- the narrow path ------------------------------------------------------

  private narrowRun(plan: Plan, node: i32): void {
    if (!this.enter()) return
    const writer = new NarrowWriter(this.out)
    for (let index = 0; index < plan.fields.length && this.ok; index++) {
      const field = unchecked(plan.fields[index])
      // An envelope's single field *is* the document, so there is no object to
      // look a name up in.
      const value = plan.isEnvelope ? node : this.valueOf(plan, index, node)
      this.narrowField(writer, field, value)
    }
    this.leave()
  }

  private narrowField(writer: NarrowWriter, field: PlanField, node: i32): void {
    const doc = this.doc
    if (node < 0 || doc.kindOf(node) == K_NULL) {
      // An absent key and an explicit null are the same thing on the wire: the
      // field is simply not written, and a pointer field decodes back as null.
      return
    }
    const op = field.op
    if (op == OP_POINTER) {
      // A non-nil pointer to a zero writes an explicit zero — two bytes —
      // because otherwise it would be indistinguishable from nil. That is the
      // one value in the format written solely to say it is there.
      this.narrowScalar(writer, field.key, field.elemOp, node, true)
      return
    }
    if (op == OP_STRUCT) {
      const sub = field.sub
      if (sub == null) return
      const mark = writer.openStruct(field.key, sub.isWide)
      this.run(sub, node)
      writer.close(mark)
      return
    }
    if (op == OP_STRUCTS) {
      this.narrowStructs(writer, field, node)
      return
    }
    if (op == OP_STRINGS) {
      writer.strings(field.key, this.gatherStrings(node))
      return
    }
    if (op == OP_INT64S || op == OP_UINT64S) {
      const count = doc.count(node)
      const values = this.gatherInts(node, count)
      writer.ints(field.key, values, count, true)
      return
    }
    this.narrowScalar(writer, field.key, op, node, false)
  }

  private narrowScalar(
    writer: NarrowWriter,
    key: u8,
    op: u8,
    node: i32,
    explicitZero: bool,
  ): void {
    const doc = this.doc
    if (op == OP_BOOL) {
      const value = unchecked(doc.num[node]) != 0
      if (!value && explicitZero) writer.zeroUnsigned(key)
      else writer.bool(key, value)
      return
    }
    if (op == OP_UINT64) {
      const value = <u64>unchecked(doc.num[node])
      if (value == 0 && explicitZero) writer.zeroUnsigned(key)
      else writer.uint(key, value)
      return
    }
    if (op == OP_INT64) {
      const value = this.asInt(node)
      if (value == 0 && explicitZero) writer.zeroSigned(key)
      else writer.int(key, value)
      return
    }
    if (op == OP_FLOAT64 || op == OP_FLOAT32) {
      const value = this.asFloat(node)
      // A float rides the unsigned shape, so its explicit zero is the unsigned
      // one: reading it back goes through uint() either way.
      if (value == 0 && explicitZero) writer.zeroUnsigned(key)
      else writer.f64(key, value)
      return
    }
    if (op == OP_STRING) {
      const bytes = doc.kindOf(node) == K_STRING ? doc.strOf(node) : new Uint8Array(0)
      if (bytes.length == 0 && explicitZero) writer.emptyString(key)
      else if (this.packStrings) writer.packedString(key, bytes)
      else writer.blob(key, bytes)
      return
    }
    this.unwritable(op)
  }

  private narrowStructs(writer: NarrowWriter, field: PlanField, node: i32): void {
    const sub = field.sub
    if (sub == null) return
    const doc = this.doc
    const rows = doc.count(node)
    if (rows == 0) return
    if (sub.canTable && rows >= TABLE_THRESHOLD) {
      const mark = writer.openTable(field.key, rows)
      this.narrowColumns(writer, sub, node, rows)
      writer.close(mark)
      return
    }
    const list = writer.openList(field.key, rows)
    for (let index = 0; index < rows && this.ok; index++) {
      const element = writer.openElement()
      this.run(sub, doc.childAt(node, index))
      writer.closeElement(element)
    }
    writer.close(list)
  }

  private narrowColumns(writer: NarrowWriter, sub: Plan, node: i32, rows: i32): void {
    const doc = this.doc
    for (let index = 0; index < sub.fields.length && this.ok; index++) {
      const field = unchecked(sub.fields[index])
      if (field.op == OP_STRING) {
        writer.stringColumn(field.key, this.gatherStringColumn(sub, index, node, rows))
        continue
      }
      const values = this.gatherColumn(sub, index, node, rows)
      writer.column(field.key, values, rows, widthOfOp(field.op))
    }
  }

  // ---- the wide path --------------------------------------------------------

  private wideRun(plan: Plan, node: i32): void {
    if (!this.enter()) return
    const writer = new WideWriter(this.out)
    for (let index = 0; index < plan.fields.length && this.ok; index++) {
      const field = unchecked(plan.fields[index])
      const value = plan.isEnvelope ? node : this.valueOf(plan, index, node)
      this.wideField(writer, field, value)
    }
    this.leave()
  }

  private wideField(writer: WideWriter, field: PlanField, node: i32): void {
    const doc = this.doc
    if (node < 0 || doc.kindOf(node) == K_NULL) return
    const op = field.op
    if (op == OP_POINTER) {
      this.wideScalar(writer, field.key, field.elemOp, node, true)
      return
    }
    if (op == OP_STRUCT) {
      const sub = field.sub
      if (sub == null) return
      const mark = writer.openStruct(field.key, sub.isWide)
      this.run(sub, node)
      writer.close(mark)
      return
    }
    if (op == OP_STRUCTS) {
      this.wideStructs(writer, field, node)
      return
    }
    if (op == OP_STRINGS) {
      writer.strings(field.key, this.gatherStrings(node))
      return
    }
    if (op == OP_INT64S || op == OP_UINT64S) {
      const count = doc.count(node)
      const values = this.gatherInts(node, count)
      writer.ints(field.key, values, count, true)
      return
    }
    this.wideScalar(writer, field.key, op, node, false)
  }

  private wideScalar(writer: WideWriter, key: u8, op: u8, node: i32, explicitZero: bool): void {
    const doc = this.doc
    if (op == OP_BOOL) {
      const value = unchecked(doc.num[node]) != 0
      if (!value && explicitZero) writer.zeroUnsigned(key)
      else writer.bool(key, value)
      return
    }
    if (op == OP_UINT64) {
      const value = <u64>unchecked(doc.num[node])
      if (value == 0 && explicitZero) writer.zeroUnsigned(key)
      else writer.uint(key, value)
      return
    }
    if (op == OP_INT64) {
      const value = this.asInt(node)
      if (value == 0 && explicitZero) writer.zeroSigned(key)
      else writer.int(key, value)
      return
    }
    if (op == OP_FLOAT64 || op == OP_FLOAT32) {
      const value = this.asFloat(node)
      if (value == 0 && explicitZero) writer.zeroUnsigned(key)
      else writer.f64(key, value)
      return
    }
    if (op == OP_STRING) {
      const bytes = doc.kindOf(node) == K_STRING ? doc.strOf(node) : new Uint8Array(0)
      if (bytes.length == 0 && explicitZero) writer.emptyString(key)
      else if (this.packStrings) writer.packedString(key, bytes)
      else writer.blob(key, bytes)
      return
    }
    this.unwritable(op)
  }

  private wideStructs(writer: WideWriter, field: PlanField, node: i32): void {
    const sub = field.sub
    if (sub == null) return
    const doc = this.doc
    const rows = doc.count(node)
    if (rows == 0) return
    if (sub.canTable && rows >= TABLE_THRESHOLD) {
      const mark = writer.openTable(field.key, rows)
      this.wideColumns(writer, sub, node, rows)
      writer.close(mark)
      return
    }
    const list = writer.openList(field.key, rows)
    for (let index = 0; index < rows && this.ok; index++) {
      const element = writer.openElementStruct(sub.isWide)
      this.run(sub, doc.childAt(node, index))
      writer.close(element)
    }
    writer.close(list)
  }

  private wideColumns(writer: WideWriter, sub: Plan, node: i32, rows: i32): void {
    for (let index = 0; index < sub.fields.length && this.ok; index++) {
      const field = unchecked(sub.fields[index])
      if (field.op == OP_STRING) {
        writer.stringColumn(field.key, this.gatherStringColumn(sub, index, node, rows))
        continue
      }
      const values = this.gatherColumn(sub, index, node, rows)
      writer.column(field.key, values, rows, widthOfOp(field.op))
    }
  }

  // ---- gathering ------------------------------------------------------------

  /** The document node for one field of one record, or -1 when the record does
   * not carry it. */
  private valueOf(plan: Plan, index: i32, node: i32): i32 {
    const doc = this.doc
    if (node < 0 || doc.kindOf(node) != K_OBJECT) return -1
    const name = unchecked(plan.nameBytes[index])
    const count = doc.count(node)
    // The last occurrence wins, as JSON.parse does. Taking the first was a real
    // bug the self-check caught the first time it ran, on a document with a
    // duplicate key.
    let found = -1
    for (let at = 0; at < count; at++) {
      if (keyEquals(doc, node, at, name)) found = doc.childAt(node, at)
    }
    return found
  }

  private gatherStrings(node: i32): Array<Uint8Array> {
    const doc = this.doc
    const out = new Array<Uint8Array>()
    const count = doc.count(node)
    for (let index = 0; index < count; index++) {
      const child = doc.childAt(node, index)
      out.push(doc.kindOf(child) == K_STRING ? doc.strOf(child) : new Uint8Array(0))
    }
    return out
  }

  private gatherInts(node: i32, count: i32): Int64Array {
    const doc = this.doc
    const out = new Int64Array(count)
    for (let index = 0; index < count; index++) {
      unchecked((out[index] = this.asInt(doc.childAt(node, index))))
    }
    return out
  }

  /**
   * One column of a table, transposed out of the rows.
   *
   * A float column carries its **plain** bit pattern, where a scalar field
   * carries the byte-reversed one: the reversal exists to move a float's zero
   * bytes where the integer trim can reach them, and a column has no such trim
   * to feed.
   */
  private gatherColumn(sub: Plan, index: i32, node: i32, rows: i32): Int64Array {
    const doc = this.doc
    const field = unchecked(sub.fields[index])
    const values = this.reserve(rows)
    for (let row = 0; row < rows; row++) {
      const cell = this.valueOf(sub, index, doc.childAt(node, row))
      let raw: i64 = 0
      if (cell >= 0 && doc.kindOf(cell) != K_NULL) {
        if (field.op == OP_FLOAT64) {
          raw = reinterpret<i64>(this.asFloat(cell))
        } else if (field.op == OP_FLOAT32) {
          raw = <i64>reinterpret<u32>(<f32>this.asFloat(cell))
        } else {
          raw = this.asInt(cell)
        }
      }
      unchecked((values[row] = raw))
    }
    return values
  }

  private gatherStringColumn(sub: Plan, index: i32, node: i32, rows: i32): Array<Uint8Array> {
    const doc = this.doc
    const out = new Array<Uint8Array>()
    for (let row = 0; row < rows; row++) {
      const cell = this.valueOf(sub, index, doc.childAt(node, row))
      out.push(
        cell >= 0 && doc.kindOf(cell) == K_STRING ? doc.strOf(cell) : new Uint8Array(0),
      )
    }
    return out
  }

  /** A node as an i64, whatever number kind it parsed as. */
  private asInt(node: i32): i64 {
    const doc = this.doc
    const kind = doc.kindOf(node)
    if (kind == K_INT || kind == K_UINT) return unchecked(doc.num[node])
    if (kind == K_BOOL) return unchecked(doc.num[node])
    if (kind == K_FLOAT) return <i64>doc.floatOf(node)
    return 0
  }

  private asFloat(node: i32): f64 {
    const doc = this.doc
    const kind = doc.kindOf(node)
    if (kind == K_FLOAT) return doc.floatOf(node)
    if (kind == K_UINT) return <f64><u64>unchecked(doc.num[node])
    if (kind == K_INT) return <f64>unchecked(doc.num[node])
    return 0
  }

  private unwritable(op: u8): void {
    this.diag.fail(
      D_UNSUPPORTED,
      -1,
      '',
      'field type ' + op.toString() + ' is one this encoder does not write',
    )
  }
}

/**
 * Whether the record's key at `slot` is the field's name, compared where the key
 * already is.
 *
 * `doc.keyOf` would hand back a view, and a view is an allocation. This runs
 * once per key per field per record — on a thousand seven-field records that is
 * forty-nine thousand of them, for a comparison that touches a handful of bytes.
 * The same shape cost the self-check most of its time until verify.ts stopped
 * doing it.
 */
@inline
function keyEquals(doc: Doc, node: i32, slot: i32, name: Uint8Array): bool {
  const at = unchecked(doc.a[node]) + slot
  const length = unchecked(doc.keyB[at])
  if (length != name.length) return false
  if (length == 0) return true
  return (
    memory.compare(
      doc.text.buf.dataStart + <usize>unchecked(doc.keyA[at]),
      name.dataStart,
      <usize>length,
    ) == 0
  )
}
