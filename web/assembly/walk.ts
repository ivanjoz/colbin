// Walking a message with a schema where the Go type would be.
//
// Port of codec/json.go. The Go decoders write through pointers into a layout
// they were given; there is no layout here — the plan came off the wire — so
// this is a second set of walkers rather than a flag on the first.
//
// # What the walk has to get right
//
//   - **An absent key is a zero value, not a missing field.** colbin writes
//     nothing for a zero, so the JSON has to put it back or the output is not
//     the record. Every field of the schema appears; the ones the message
//     omitted come out as 0, "", false or null.
//   - **A slice of structs has two shapes**, and the writer picks per value. The
//     table is the harder one: it is keyed per column and JSON is per row, so
//     this decoder does a transpose the Go decoder gets for free by scattering.
//   - **Floats are byte-reversed bit patterns** in a scalar field and *plain*
//     ones in a column. Reading one as the other is silent nonsense, so the two
//     paths are separate and each has a vector.
//   - **K4 cannot skip.** A narrow message holding a key the schema does not
//     list ends the walk, as it does for a Go type. That is K4's standing trade
//     and the error says so.
//
// # Depth is data here, not type
//
// `[]Node` inside `Node` nests as deep as the *message* says, so a crafted one
// could run the stack out. The bound is the same 128 Go uses — far past anything
// a real record nests and far short of anything that hurts.

import { JSONSink } from './jsontext'
import {
  MAP_BOOL,
  MAP_FLOAT32,
  MAP_FLOAT64,
  MAP_INT,
  MAP_STRING,
  MAP_UINT,
  OP_BOOL,
  OP_BYTES,
  OP_FLOAT32,
  OP_FLOAT64,
  OP_INT16,
  OP_INT16S,
  OP_INT32,
  OP_INT32S,
  OP_INT64,
  OP_INT64S,
  OP_INT8,
  OP_INT8S,
  OP_MAP,
  OP_POINTER,
  OP_STRING,
  OP_STRINGS,
  OP_STRUCT,
  OP_STRUCTS,
  OP_UINT16,
  OP_UINT16S,
  OP_UINT32,
  OP_UINT32S,
  OP_UINT64,
  OP_UINT64S,
  OP_UINT8,
  Plan,
  PlanField,
  widthOfOp,
} from './plan'
import { floatFromReversed32, floatFromReversed64, wireErrorText } from './wire/desc'
import { NarrowReader, Vec } from './wire/narrow'
import { WideReader } from './wire/wide'

const MAX_DEPTH: i32 = 128

/**
 * The most rows a table may claim.
 *
 * This is the one place a message's own number decides an allocation, and it is
 * the one divergence from Go on this path. Go allocates what the message asks
 * for and lets `makeslice: len out of range` become a recovered panic; here an
 * allocation that cannot be satisfied calls `abort`, which reaches the host as a
 * RuntimeError carrying nothing a caller can act on. So the number is refused in
 * front of the allocation instead.
 *
 * It cannot be derived from the bytes left, and that is worth stating rather
 * than hiding: a constant column is nine bytes for any length, so a legitimate
 * table of a million identical rows really does fit in a handful of bytes. The
 * bound is therefore a budget, not a proof — four million rows is 32 MB per
 * integer column, which is already past what a browser tab should be handed, and
 * far past anything the encoder on the other side of this module will produce.
 */
const MAX_ROWS: i32 = 1 << 22

export class Walker {
  to: JSONSink
  error: string = ''
  private depth: i32 = 0

  constructor(to: JSONSink) {
    this.to = to
  }

  @inline get ok(): bool {
    return this.error.length == 0 && this.to.ok
  }

  fail(message: string): void {
    if (this.error.length == 0) this.error = message
  }

  private failWire(code: i32): void {
    this.fail('colbin: ' + wireErrorText(code))
  }

  private enter(): bool {
    if (this.depth >= MAX_DEPTH) {
      this.fail('colbin: the message nests deeper than this decoder walks')
      return false
    }
    this.depth++
    return true
  }

  @inline private leave(): void {
    this.depth--
  }

  /** A whole record, at whichever key width the run declared. */
  message(plan: Plan | null, body: Uint8Array, wide: bool): void {
    if (plan == null) {
      this.fail('colbin: the schema describes a composite field without saying what is inside it')
      return
    }
    if (wide) this.wideRun(plan, body)
    else this.narrowRun(plan, body)
  }

  /**
   * The root run, which is the only place an envelope can be.
   *
   * A document whose top level was not an object was wrapped in a one-field
   * struct, because the root of a colbin message is a struct and nothing else.
   * The section says so (REFACTOR_PLAN.md §5.1), so unwrapping it is reading a
   * fact rather than guessing at a field name — and a reader that does not know
   * the flag, Go included, renders `{"rows":[…]}` and is not wrong, only more
   * literal.
   */
  root(plan: Plan | null, body: Uint8Array, wide: bool): void {
    if (plan == null || !plan.isEnvelope || plan.fields.length != 1) {
      this.message(plan, body, wide)
      return
    }
    this.envelope(plan, body, wide)
  }

  private envelope(plan: Plan, body: Uint8Array, wide: bool): void {
    if (!this.enter()) return
    const field = unchecked(plan.fields[0])
    if (wide) {
      const reader = new WideReader(body)
      if (reader.more && reader.key == field.key) {
        this.wideValue(reader, field)
      } else {
        this.zero(field)
      }
      if (!reader.ok) this.failWire(reader.err)
    } else {
      const reader = new NarrowReader(body)
      if (reader.more && reader.key == field.key) {
        this.narrowValue(reader, field)
      } else {
        this.zero(field)
      }
      if (!reader.ok) this.failWire(reader.err)
    }
    this.leave()
  }

  // ---- runs -----------------------------------------------------------------

  private narrowRun(plan: Plan, body: Uint8Array): void {
    if (!this.enter()) return
    const reader = new NarrowReader(body)
    const seen = new StaticArray<bool>(plan.fields.length)
    this.to.beginObject()
    while (reader.more && this.ok) {
      const key = reader.key
      const index = plan.positionOf(key)
      if (index < 0) {
        // Four descriptor bits have no room for a class, so nothing can size a
        // field it cannot classify.
        this.fail(
          'colbin: message holds field id ' +
            key.toString() +
            ', which the schema does not declare, and a narrow key cannot be skipped',
        )
        this.leave()
        return
      }
      unchecked((seen[index] = true))
      this.to.key(unchecked(plan.names[index]))
      this.narrowValue(reader, unchecked(plan.fields[index]))
    }
    if (!reader.ok) this.failWire(reader.err)
    if (!this.ok) {
      this.leave()
      return
    }
    this.absent(plan, seen)
    this.to.endObject()
    this.leave()
  }

  private wideRun(plan: Plan, body: Uint8Array): void {
    if (!this.enter()) return
    const reader = new WideReader(body)
    const seen = new StaticArray<bool>(plan.fields.length)
    this.to.beginObject()
    while (reader.more && this.ok) {
      const index = plan.positionOf(reader.key)
      if (index < 0) {
        // A key the schema does not list is stepped over, which is what the wide
        // width is for.
        if (!reader.skip()) break
        continue
      }
      unchecked((seen[index] = true))
      this.to.key(unchecked(plan.names[index]))
      this.wideValue(reader, unchecked(plan.fields[index]))
    }
    if (!reader.ok) this.failWire(reader.err)
    if (!this.ok) {
      this.leave()
      return
    }
    this.absent(plan, seen)
    this.to.endObject()
    this.leave()
  }

  /**
   * The fields the message left out.
   *
   * Not an afterthought: omission *is* the encoding of a zero value, so a record
   * that writes three of its nine fields still has nine, and a reader that
   * printed three would be printing a different record.
   */
  private absent(plan: Plan, seen: StaticArray<bool>): void {
    for (let index = 0; index < plan.fields.length; index++) {
      if (unchecked(seen[index])) continue
      this.to.key(unchecked(plan.names[index]))
      this.zero(unchecked(plan.fields[index]))
    }
  }

  /** What an absent key means, per op — which is what Unmarshal would have left
   * in the struct, because it zeroes the record first. */
  zero(field: PlanField): void {
    const op = field.op
    if (op == OP_BOOL) {
      this.to.boolean(false)
    } else if (op >= OP_INT8 && op <= OP_INT64) {
      this.to.signed(0)
    } else if (op >= OP_UINT8 && op <= OP_UINT64) {
      this.to.unsigned(0)
    } else if (op == OP_FLOAT32) {
      this.to.float(0, 32)
    } else if (op == OP_FLOAT64) {
      this.to.float(0, 64)
    } else if (op == OP_STRING) {
      this.to.textBytes(new Uint8Array(0))
    } else if (op == OP_STRUCT) {
      // A nested struct is always written, empty body or not, so this is
      // unreachable from anything colbin produces. A section from somewhere else
      // may still say it, and an object of zeros is what the field would hold.
      this.zeroStruct(field.sub)
    } else {
      // A blob, an array, a slice of structs, a map and a pointer are all nil
      // when the key is absent, and a nil one of those is null.
      this.to.null()
    }
  }

  private zeroStruct(plan: Plan | null): void {
    if (plan == null || !this.enter()) {
      this.to.null()
      return
    }
    const none = new StaticArray<bool>(plan.fields.length)
    this.to.beginObject()
    this.absent(plan, none)
    this.to.endObject()
    this.leave()
  }

  // ---- one field, each width ------------------------------------------------
  //
  // The composites are here and the values are in *Scalar, because a pointer
  // field's payload *is* a value — the op it points at — and the two switches
  // would otherwise be one switch written twice.

  narrowValue(reader: NarrowReader, field: PlanField): void {
    const op = field.op
    if (op == OP_STRUCT) {
      const body = reader.structBody()
      if (!body.ok) {
        this.failWire(reader.err)
        return
      }
      this.message(field.sub, body.bytes, body.wide)
    } else if (op == OP_STRUCTS) {
      if (reader.isTable()) this.narrowTable(reader, field)
      else this.narrowList(reader, field)
    } else if (op == OP_MAP) {
      this.narrowMap(reader, field)
    } else if (op == OP_POINTER) {
      this.narrowScalar(reader, field.elemOp)
    } else {
      this.narrowScalar(reader, op)
    }
  }

  private narrowScalar(reader: NarrowReader, op: u8): void {
    if (op == OP_BOOL) {
      this.to.boolean(reader.bool())
    } else if (op >= OP_INT8 && op <= OP_INT64) {
      this.to.signed(reader.int())
    } else if (op >= OP_UINT8 && op <= OP_UINT64) {
      this.to.unsigned(reader.uint())
    } else if (op == OP_FLOAT32) {
      this.to.float(<f64>reader.f32(), 32)
    } else if (op == OP_FLOAT64) {
      this.to.float(reader.f64(), 64)
    } else if (op == OP_STRING) {
      this.to.textBytes(reader.bytes())
    } else if (op == OP_BYTES) {
      this.to.blob(reader.bytes())
    } else if (op == OP_STRINGS) {
      this.textArray(reader.strings())
    } else if (isArrayOp(op)) {
      this.intArray(reader.vec(), op)
    } else {
      this.unwalkable(op)
    }
    if (!reader.ok) this.failWire(reader.err)
  }

  wideValue(reader: WideReader, field: PlanField): void {
    const op = field.op
    if (op == OP_STRUCT) {
      const body = reader.structBody()
      if (!body.ok) {
        this.failWire(reader.err)
        return
      }
      this.message(field.sub, body.bytes, body.wide)
    } else if (op == OP_STRUCTS) {
      if (reader.isTable()) this.wideTable(reader, field)
      else this.wideList(reader, field)
    } else if (op == OP_MAP) {
      this.wideMap(reader, field)
    } else if (op == OP_POINTER) {
      this.wideScalar(reader, field.elemOp)
    } else {
      this.wideScalar(reader, op)
    }
  }

  private wideScalar(reader: WideReader, op: u8): void {
    if (op == OP_BOOL) {
      this.to.boolean(reader.bool())
    } else if (op >= OP_INT8 && op <= OP_INT64) {
      this.to.signed(reader.int())
    } else if (op >= OP_UINT8 && op <= OP_UINT64) {
      this.to.unsigned(reader.uint())
    } else if (op == OP_FLOAT32) {
      this.to.float(<f64>reader.f32(), 32)
    } else if (op == OP_FLOAT64) {
      this.to.float(reader.f64(), 64)
    } else if (op == OP_STRING) {
      // packed5 is on its way out (REFACTOR_PLAN.md §4), so a packed blob is
      // named rather than decoded: a wrong string is worse than a clear refusal,
      // and Packed5 is off by default in Go so nothing ordinary reaches this.
      if (reader.blobEncoding() != 0) {
        this.fail('colbin: this build does not read packed strings; encode with Packed5 off')
        return
      }
      this.to.textBytes(reader.bytes())
    } else if (op == OP_BYTES) {
      this.to.blob(reader.bytes())
    } else if (op == OP_STRINGS) {
      this.textArray(reader.strings())
    } else if (isArrayOp(op)) {
      this.intArray(reader.vec(), op)
    } else {
      this.unwalkable(op)
    }
    if (!reader.ok) this.failWire(reader.err)
  }

  private unwalkable(op: u8): void {
    this.fail('colbin: the schema puts field type ' + op.toString() + ' where a value belongs')
  }

  // ---- arrays ---------------------------------------------------------------

  private intArray(vec: Vec, op: u8): void {
    if (!vec.ok) return
    const signed = op >= OP_INT8S && op <= OP_INT64S
    this.to.beginArray()
    const count = vec.count
    for (let index = 0; index < count; index++) {
      const value = vec.at(index)
      if (signed) this.to.signed(value)
      else this.to.unsigned(<u64>value)
    }
    this.to.endArray()
  }

  private textArray(values: Array<Uint8Array>): void {
    this.to.beginArray()
    for (let index = 0; index < values.length; index++) {
      this.to.textBytes(unchecked(values[index]))
    }
    this.to.endArray()
  }

  // ---- lists of structs, the row-wise shape ---------------------------------

  /**
   * A narrow list, whose element is a length and a key run with no descriptor
   * between them. That missing descriptor is a byte saved per element and the
   * reason a structDef states its key width: this is the one run on the wire
   * whose width the wire does not say.
   */
  private narrowList(reader: NarrowReader, field: PlanField): void {
    const body = reader.compositeBody()
    if (!reader.ok) {
      this.failWire(reader.err)
      return
    }
    const sub = field.sub
    if (sub == null) {
      this.fail('colbin: the schema describes a composite field without saying what is inside it')
      return
    }
    const elements = new NarrowReader(body)
    const count = elements.countPrefix()
    if (!elements.ok) {
      this.failWire(elements.err)
      return
    }
    if (!this.enter()) return
    this.to.beginArray()
    for (let index = 0; index < count; index++) {
      const element = elements.element()
      if (!elements.ok) {
        this.failWire(elements.err)
        this.leave()
        return
      }
      this.message(sub, element, sub.isWide)
      if (!this.ok) {
        this.leave()
        return
      }
    }
    this.to.endArray()
    this.leave()
  }

  /** A wide list, whose elements carry a descriptor of their own — which is what
   * lets a wide reader step over one it does not understand. */
  private wideList(reader: WideReader, field: PlanField): void {
    const list = reader.list()
    if (!list.ok) {
      this.failWire(reader.err)
      return
    }
    const sub = field.sub
    if (sub == null) {
      this.fail('colbin: the schema describes a composite field without saying what is inside it')
      return
    }
    if (!this.enter()) return
    this.to.beginArray()
    for (let index = 0; index < list.count; index++) {
      const body = list.reader.elementStructBody()
      if (!body.ok) {
        this.failWire(list.reader.err)
        this.leave()
        return
      }
      this.message(sub, body.bytes, body.wide)
      if (!this.ok) {
        this.leave()
        return
      }
    }
    this.to.endArray()
    this.leave()
  }

  // ---- tables, the columnar shape -------------------------------------------
  //
  // This is the transpose, and the one place this decoder does work the Go
  // decoder does not. A table is one key per *column*; JSON is one object per
  // *row*. The Go decoder scatters each column across a slice as it reads it and
  // never holds two at once. Here every column has to be in memory before the
  // first row can be written, because the first row needs the first value of
  // every one of them.
  //
  // So a table costs its whole self in memory for the length of the walk, and
  // the alternative — one pass over the message per row — is worse by the row
  // count. The row count is the message's claim, exactly as it is for the typed
  // decoder, and bounding that belongs in the wire layer where both would get it.

  private narrowTable(reader: NarrowReader, field: PlanField): void {
    const body = reader.compositeBody()
    if (!reader.ok) {
      this.failWire(reader.err)
      return
    }
    const sub = field.sub
    if (sub == null) {
      this.fail('colbin: the schema describes a composite field without saying what is inside it')
      return
    }
    const counter = new NarrowReader(body)
    const rows = counter.countPrefix()
    if (!counter.ok) {
      this.failWire(counter.err)
      return
    }
    if (!this.rowsArePlausible(rows)) return
    // A table's columns are keyed at the width of the run that holds it: the
    // narrow writer never sets the descriptor's k8 bit and the wide one never
    // has to, so the enclosing width is the whole of the dispatch.
    const columns = body.subarray(counter.at)
    const gathered = new TableColumns(sub.fields.length, rows)
    this.gatherNarrow(new NarrowReader(columns), sub, gathered, rows)
    if (!this.ok) return
    this.tableRows(sub, gathered, rows)
  }

  private wideTable(reader: WideReader, field: PlanField): void {
    const table = reader.table()
    if (!table.ok) {
      this.failWire(reader.err)
      return
    }
    const sub = field.sub
    if (sub == null) {
      this.fail('colbin: the schema describes a composite field without saying what is inside it')
      return
    }
    if (!this.rowsArePlausible(table.count)) return
    const gathered = new TableColumns(sub.fields.length, table.count)
    this.gatherWide(table.reader, sub, gathered, table.count)
    if (!this.ok) return
    this.tableRows(sub, gathered, table.count)
  }

  /** Refuses a row count before it becomes an allocation. See MAX_ROWS. */
  private rowsArePlausible(rows: i32): bool {
    if (rows >= 0 && rows <= MAX_ROWS) return true
    this.fail(
      'colbin: a table claims ' + rows.toString() + ' rows, past what this decoder will allocate',
    )
    return false
  }

  private gatherNarrow(
    columns: NarrowReader,
    sub: Plan,
    gathered: TableColumns,
    rows: i32,
  ): void {
    while (columns.more) {
      const key = columns.key
      const index = sub.positionOf(key)
      if (index < 0) {
        this.fail(
          'colbin: a table holds column ' +
            key.toString() +
            ', which the schema does not declare, and a narrow key cannot be skipped',
        )
        return
      }
      const field = unchecked(sub.fields[index])
      if (field.op == OP_STRING) {
        gathered.strings[index] = columns.strings()
      } else {
        const values = new Int64Array(rows)
        columns.column(rows, values, widthOfColumn(field.op))
        gathered.ints[index] = values
      }
      unchecked((gathered.present[index] = true))
      if (!columns.ok) {
        this.failWire(columns.err)
        return
      }
    }
  }

  private gatherWide(columns: WideReader, sub: Plan, gathered: TableColumns, rows: i32): void {
    while (columns.more) {
      const index = sub.positionOf(columns.key)
      if (index < 0) {
        if (!columns.skip()) {
          this.failWire(columns.err)
          return
        }
        continue
      }
      const field = unchecked(sub.fields[index])
      if (field.op == OP_STRING) {
        gathered.strings[index] = columns.strings()
      } else {
        const values = new Int64Array(rows)
        columns.column(rows, values, widthOfColumn(field.op))
        gathered.ints[index] = values
      }
      unchecked((gathered.present[index] = true))
      if (!columns.ok) {
        this.failWire(columns.err)
        return
      }
    }
  }

  /** The transpose itself. */
  private tableRows(sub: Plan, gathered: TableColumns, rows: i32): void {
    if (!this.enter()) return
    this.to.beginArray()
    for (let row = 0; row < rows; row++) {
      this.to.beginObject()
      for (let index = 0; index < sub.fields.length; index++) {
        const field = unchecked(sub.fields[index])
        this.to.key(unchecked(sub.names[index]))
        if (!unchecked(gathered.present[index])) {
          // A column whose every value was zero is not written at all, and its
          // absence is the whole of what says so.
          this.zero(field)
        } else if (field.op == OP_STRING) {
          const column = gathered.strings[index]
          this.to.textBytes(
            column != null && row < column.length ? unchecked(column[row]) : new Uint8Array(0),
          )
        } else {
          const column = gathered.ints[index]
          // A column shorter than the table's row count is a message that
          // disagrees with itself; the row still has to be written, and a zero
          // is what an absent column means everywhere else here.
          const raw = column != null && row < column.length ? unchecked(column[row]) : 0
          this.columnValue(field.op, raw)
        }
      }
      this.to.endObject()
    }
    this.to.endArray()
    this.leave()
  }

  /**
   * One gathered column value, back to its type.
   *
   * A column carries a float as its **plain** bit pattern where a scalar field
   * carries the byte-reversed one: the reversal exists to move a float's zero
   * bytes where the integer trim can reach them, and a column has no such trim
   * to feed. Reading either as the other yields a number rather than an error,
   * which is why they do not share a line of code.
   */
  private columnValue(op: u8, raw: i64): void {
    if (op == OP_BOOL) {
      this.to.boolean(raw == 1)
    } else if (op >= OP_INT8 && op <= OP_INT64) {
      this.to.signed(raw)
    } else if (op >= OP_UINT8 && op <= OP_UINT64) {
      this.to.unsigned(<u64>raw)
    } else if (op == OP_FLOAT32) {
      this.to.float(<f64>reinterpret<f32>(<u32>raw), 32)
    } else if (op == OP_FLOAT64) {
      this.to.float(reinterpret<f64>(<u64>raw), 64)
    } else {
      // Nothing else is columnable, so nothing else can be here.
      this.unwalkable(op)
    }
  }

  // ---- maps -----------------------------------------------------------------
  //
  // JSON objects are unordered and a Go map has no iteration order, so a
  // schema-described message holding a map does not render to identical JSON
  // bytes twice. That rules a map out of any golden vector and is worth writing
  // down rather than discovering.
  //
  // An integer key becomes a quoted decimal, because a JSON object key is a
  // string. Reading one back the other way is out of scope.

  private narrowMap(reader: NarrowReader, field: PlanField): void {
    const body = reader.compositeBody()
    if (!reader.ok) {
      this.failWire(reader.err)
      return
    }
    if (!keyIsRenderable(field.keyKind)) {
      this.badMapKey(field.keyKind)
      return
    }
    const entries = new NarrowReader(body)
    const count = entries.countPrefix()
    if (!entries.ok) {
      this.failWire(entries.err)
      return
    }
    if (!this.enter()) return
    this.to.beginObject()
    for (let index = 0; index < count; index++) {
      if (field.keyKind == MAP_STRING) this.to.keyBytes(entries.elementBytes())
      else if (field.keyKind == MAP_INT) this.to.key(entries.elementInt().toString())
      else this.to.key(entries.elementUint().toString())

      const kind = field.valueKind
      if (kind == MAP_STRING) this.to.textBytes(entries.elementBytes())
      else if (kind == MAP_INT) this.to.signed(entries.elementInt())
      else if (kind == MAP_UINT) this.to.unsigned(entries.elementUint())
      else if (kind == MAP_FLOAT32) this.to.float(<f64>floatFromReversed32(<u32>entries.elementUint()), 32)
      else if (kind == MAP_FLOAT64) this.to.float(floatFromReversed64(entries.elementUint()), 64)
      else if (kind == MAP_BOOL) this.to.boolean(entries.elementUint() == 1)

      if (!entries.ok) {
        this.failWire(entries.err)
        this.leave()
        return
      }
    }
    this.to.endObject()
    this.leave()
  }

  private wideMap(reader: WideReader, field: PlanField): void {
    const entries = reader.map()
    if (!entries.ok) {
      this.failWire(reader.err)
      return
    }
    if (!keyIsRenderable(field.keyKind)) {
      this.badMapKey(field.keyKind)
      return
    }
    if (!this.enter()) return
    const inner = entries.reader
    this.to.beginObject()
    for (let index = 0; index < entries.count; index++) {
      if (field.keyKind == MAP_STRING) this.to.keyBytes(inner.elementBytes())
      else if (field.keyKind == MAP_INT) this.to.key(inner.elementInt().toString())
      else this.to.key(inner.elementUint().toString())

      const kind = field.valueKind
      if (kind == MAP_STRING) this.to.textBytes(inner.elementBytes())
      else if (kind == MAP_INT) this.to.signed(inner.elementInt())
      else if (kind == MAP_UINT) this.to.unsigned(inner.elementUint())
      else if (kind == MAP_FLOAT32) this.to.float(<f64>floatFromReversed32(<u32>inner.elementUint()), 32)
      else if (kind == MAP_FLOAT64) this.to.float(floatFromReversed64(inner.elementUint()), 64)
      else if (kind == MAP_BOOL) this.to.boolean(inner.elementUint() == 1)

      if (!inner.ok) {
        this.failWire(inner.err)
        this.leave()
        return
      }
    }
    this.to.endObject()
    this.leave()
  }

  private badMapKey(kind: u8): void {
    this.fail(
      'colbin: the schema gives a map a key of kind ' + kind.toString() + ', which is not a key',
    )
  }
}

/** A decoded table, by field position rather than by key. */
class TableColumns {
  ints: Array<Int64Array | null>
  /** A string column is held as views onto the message rather than as strings,
   * for the reason textBytes exists: the walk only ever appends them. */
  strings: Array<Array<Uint8Array> | null>
  present: StaticArray<bool>

  constructor(fields: i32, _rows: i32) {
    this.ints = new Array<Int64Array | null>(fields)
    this.strings = new Array<Array<Uint8Array> | null>(fields)
    this.present = new StaticArray<bool>(fields)
    for (let index = 0; index < fields; index++) {
      this.ints[index] = null
      this.strings[index] = null
    }
  }
}

/**
 * A map key kind the format accepts. A float and a bool are refused at plan
 * time, so only a section from somewhere else can name one — and a key that did
 * not consume its bytes would misread every entry after it, so this is checked
 * once before the loop rather than defaulted inside it.
 */
@inline
function keyIsRenderable(kind: u8): bool {
  return kind == MAP_STRING || kind == MAP_INT || kind == MAP_UINT
}

@inline
function isArrayOp(op: u8): bool {
  return (op >= OP_INT8S && op <= OP_INT64S) || (op >= OP_UINT16S && op <= OP_UINT64S)
}

/** The element width a column of this op was encoded at. */
@inline
function widthOfColumn(op: u8): i32 {
  return widthOfOp(op)
}
