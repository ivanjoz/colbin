// Where the bytes went: the field tree, with a byte span on every node.
//
// This is the page's whole point. Anyone can be told "three times smaller";
// watching `name` eat 38% of the payload is what makes the format legible. It is
// also the decoder's own test — if the spans do not tile the buffer exactly,
// something in the walk is reading a field differently from the way it was
// written, and that shows up here as a gap rather than as a wrong value.
//
// # Why this is its own walk
//
// walk.ts answers "what does this message say"; this answers "where is it". They
// visit different things. A rendering walk descends into every row of a table
// and every element of a list, because JSON needs each value. A span walk stops
// at the column: a table of four thousand rows is six nodes, not twenty-four
// thousand. Threading a position hook through the rendering walk would have made
// one function serve two shapes and neither well.
//
// The duplication is held honest by the strongest invariant there is: **the
// spans tile the body exactly**. A key run has no padding, an omitted field
// writes nothing, so the fields that *are* written are contiguous from the first
// byte of the body to the last. If this walk consumed a field differently from
// the decoder, the total would not match and the test says so.
//
// # Every node has a real span
//
// Including a list's, which is the one shape where that took a decision. A list
// is row-wise, so there is no per-field span inside it — the bytes of `sku` are
// scattered through every element. So a list's children are its *elements*,
// each with a real span, and an element's children are its fields. That keeps
// the tiling exact where per-field totals would have made `start` a fiction and
// the hex highlight a lie.
//
// A list past `ELEMENT_LIMIT` elements collapses the tail into one node, which
// keeps the tree bounded without putting a hole in it. It rarely happens: a list
// of columnable structs becomes a table at eight rows, so a long list means an
// element the column codec cannot carry.

import { Writer } from './bytes'
import { JSONSink } from './jsontext'
import {
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
import { parseSection } from './section'
import { wireErrorText } from './wire/desc'
import { NarrowReader } from './wire/narrow'
import { WideReader } from './wire/wide'

/** How many of a list's elements get a node of their own. */
const ELEMENT_LIMIT: i32 = 64

/** How deep the tree goes, matching the decoder's bound. */
const MAX_DEPTH: i32 = 128

const ROOT_FIRST: u8 = 0xd0
const ROOT_LAST: u8 = 0xdf
const ROOT_WIDE: u8 = 0x08
const ROOT_SCHEMA: u8 = 0x04

export class Inspected {
  json: Uint8Array = new Uint8Array(0)
  error: string = ''

  @inline get ok(): bool {
    return this.error.length == 0
  }
}

/** One node of the tree, built before it is written so a parent can hold its
 * children's total. */
class Node {
  name: string = ''
  key: i32 = 0
  type: string = ''
  /** Whether the field is a pointer, which is the format's "absent, not zero". */
  optional: bool = false
  /** Absolute offsets into the whole message, so the hex view can highlight
   * without knowing where the body starts. */
  start: i32 = 0
  end: i32 = 0
  children: Array<Node> = new Array<Node>()

  @inline get bytes(): i32 {
    return this.end - this.start
  }
}

export function inspect(data: Uint8Array, schema: Plan | null): Inspected {
  const out = new Inspected()
  if (data.length == 0) {
    out.error = 'colbin: empty message'
    return out
  }
  const root = unchecked(data[0])
  if (root < ROOT_FIRST || root > ROOT_LAST) {
    out.error = 'colbin: byte 0 is not a colbin root'
    return out
  }

  let plan = schema
  let at = 1
  let sectionBytes = 0
  if ((root & ROOT_SCHEMA) != 0) {
    const section = parseSection(data.subarray(1))
    if (!section.ok) {
      out.error = 'colbin: ' + section.error
      return out
    }
    sectionBytes = section.size
    at = 1 + sectionBytes
    if (plan == null) plan = section.plan
  } else if (plan == null) {
    out.error = 'colbin: this message carries no schema section; pass the schema'
    return out
  }

  const walker = new SpanWalker(data, at)
  const fields = walker.run(plan!, data.subarray(at), (root & ROOT_WIDE) != 0)
  if (!walker.ok) {
    out.error = walker.error
    return out
  }

  const sink = new JSONSink()
  sink.beginObject()
  sink.key('totalBytes')
  sink.signed(data.length)
  sink.key('schemaBytes')
  sink.signed(sectionBytes)
  sink.key('bodyBytes')
  sink.signed(data.length - at)
  sink.key('rootBytes')
  sink.signed(1)
  sink.key('wide')
  sink.boolean((root & ROOT_WIDE) != 0)
  sink.key('rows')
  sink.signed(walker.rows)
  sink.key('envelope')
  sink.boolean(plan!.isEnvelope)
  sink.key('fields')
  writeNodes(sink, fields)
  sink.endObject()
  out.json = sink.take()
  return out
}

function writeNodes(sink: JSONSink, nodes: Array<Node>): void {
  sink.beginArray()
  for (let index = 0; index < nodes.length; index++) {
    const node = unchecked(nodes[index])
    sink.beginObject()
    sink.key('name')
    sink.text(node.name)
    sink.key('key')
    sink.signed(node.key)
    sink.key('type')
    sink.text(node.type)
    sink.key('optional')
    sink.boolean(node.optional)
    sink.key('start')
    sink.signed(node.start)
    sink.key('end')
    sink.signed(node.end)
    sink.key('bytes')
    sink.signed(node.bytes)
    sink.key('children')
    writeNodes(sink, node.children)
    sink.endObject()
  }
  sink.endArray()
}

class SpanWalker {
  /** The whole message, so every span is an offset a caller can use directly. */
  data: Uint8Array
  base: i32
  error: string = ''
  /**
   * How many records the document holds: the element count of the root's list
   * or table, or one for a lone record.
   *
   * Taken at depth one only. A nested list deeper in is a field of a record, not
   * a count of them, and letting it win would have made an invoice with three
   * lines report three records.
   */
  rows: i32 = 1
  private depth: i32 = 0

  constructor(data: Uint8Array, base: i32) {
    this.data = data
    this.base = base
  }

  @inline get ok(): bool {
    return this.error.length == 0
  }

  private fail(message: string): void {
    if (this.ok) this.error = message
  }

  private failWire(code: i32): void {
    this.fail('colbin: ' + wireErrorText(code))
  }

  private enter(): bool {
    if (this.depth >= MAX_DEPTH) {
      this.fail('colbin: the message nests deeper than this inspector walks')
      return false
    }
    this.depth++
    return true
  }

  @inline private leave(): void {
    this.depth--
  }

  /** One key run, as a list of nodes. `offset` is where `body` starts in the
   * whole message, so a child's span is absolute. */
  run(plan: Plan, body: Uint8Array, wide: bool): Array<Node> {
    return wide ? this.wideRun(plan, body, this.base) : this.narrowRun(plan, body, this.base)
  }

  private narrowRun(plan: Plan, body: Uint8Array, offset: i32): Array<Node> {
    const out = new Array<Node>()
    if (!this.enter()) return out
    const reader = new NarrowReader(body)
    while (reader.more && this.ok) {
      const start = reader.at
      const index = plan.positionOf(reader.key)
      if (index < 0) {
        this.fail('colbin: the message holds a field id the schema does not declare')
        break
      }
      const field = unchecked(plan.fields[index])
      const node = new Node()
      node.name = unchecked(plan.names[index])
      node.key = field.key
      node.type = typeName(field)
      node.optional = field.op == OP_POINTER
      this.narrowValue(reader, field, node, offset)
      node.start = offset + start
      node.end = offset + reader.at
      out.push(node)
    }
    if (!reader.ok) this.failWire(reader.err)
    this.leave()
    return out
  }

  private narrowValue(reader: NarrowReader, field: PlanField, node: Node, offset: i32): void {
    const op = field.op
    if (op == OP_STRUCT) {
      const body = reader.structBody()
      if (!body.ok) return
      const inner = offset + reader.at - body.bytes.length
      node.children = body.wide
        ? this.wideRun(field.sub!, body.bytes, inner)
        : this.narrowRun(field.sub!, body.bytes, inner)
      return
    }
    if (op == OP_STRUCTS) {
      if (reader.isTable()) this.narrowTable(reader, field, node, offset)
      else this.narrowList(reader, field, node, offset)
      return
    }
    if (op == OP_MAP) {
      reader.compositeBody()
      return
    }
    this.narrowScalar(reader, op == OP_POINTER ? field.elemOp : op)
  }

  /** Consumes one scalar without rendering it. The dispatch mirrors walk.ts's;
   * the tiling test is what keeps the two from drifting. */
  private narrowScalar(reader: NarrowReader, op: u8): void {
    if (op == OP_BOOL || (op >= OP_UINT8 && op <= OP_UINT64)) {
      reader.uint()
    } else if (op >= OP_INT8 && op <= OP_INT64) {
      reader.int()
    } else if (op == OP_FLOAT32 || op == OP_FLOAT64) {
      reader.uint()
    } else if (op == OP_STRING || op == OP_BYTES) {
      reader.bytes()
    } else if (op == OP_STRINGS) {
      reader.strings()
    } else if (isArrayOp(op)) {
      reader.vec()
    } else {
      this.fail('colbin: the schema puts a composite where a value belongs')
    }
  }

  private narrowList(reader: NarrowReader, field: PlanField, node: Node, offset: i32): void {
    const before = reader.at
    const body = reader.compositeBody()
    if (!reader.ok) return
    const bodyAt = offset + reader.at - body.length
    const sub = field.sub
    if (sub == null) return
    const elements = new NarrowReader(body)
    const count = elements.countPrefix()
    if (!elements.ok) {
      this.failWire(elements.err)
      return
    }
    if (this.depth == 1) this.rows = count
    if (!this.enter()) return
    for (let index = 0; index < count && this.ok; index++) {
      const start = elements.at
      const element = elements.element()
      if (!elements.ok) {
        this.failWire(elements.err)
        break
      }
      if (index < ELEMENT_LIMIT) {
        const child = new Node()
        child.name = '[' + index.toString() + ']'
        child.key = index
        child.type = 'struct'
        child.start = bodyAt + start
        child.end = bodyAt + elements.at
        const inner = bodyAt + elements.at - element.length
        child.children = sub.isWide
          ? this.wideRun(sub, element, inner)
          : this.narrowRun(sub, element, inner)
        node.children.push(child)
      } else if (index == ELEMENT_LIMIT) {
        // One node for everything past the limit, so the tree stays bounded and
        // the tiling stays exact.
        const tail = new Node()
        tail.name = '… ' + (count - ELEMENT_LIMIT).toString() + ' more'
        tail.key = index
        tail.type = 'struct'
        tail.start = bodyAt + start
        tail.end = bodyAt + body.length
        node.children.push(tail)
      }
    }
    this.leave()
    // Past the limit the loop still walks every element, because the list's own
    // end is where the last one ends.
    if (count > ELEMENT_LIMIT && node.children.length > 0) {
      unchecked(node.children[node.children.length - 1]).end = bodyAt + body.length
    }
    if (before == reader.at) this.fail('colbin: a list consumed no bytes')
  }

  private narrowTable(reader: NarrowReader, field: PlanField, node: Node, offset: i32): void {
    const body = reader.compositeBody()
    if (!reader.ok) return
    const bodyAt = offset + reader.at - body.length
    const sub = field.sub
    if (sub == null) return
    const counter = new NarrowReader(body)
    const rows = counter.countPrefix()
    if (!counter.ok) {
      this.failWire(counter.err)
      return
    }
    if (this.depth == 1) this.rows = rows
    const columns = new NarrowReader(body, counter.at)
    if (!this.enter()) return
    while (columns.more && this.ok) {
      const start = columns.at
      const index = sub.positionOf(columns.key)
      if (index < 0) {
        this.fail('colbin: a table holds a column the schema does not declare')
        break
      }
      const column = unchecked(sub.fields[index])
      if (column.op == OP_STRING) columns.strings()
      else columns.compositeBody()
      if (!columns.ok) {
        this.failWire(columns.err)
        break
      }
      const child = new Node()
      child.name = unchecked(sub.names[index])
      child.key = column.key
      child.type = typeName(column) + ' column'
      child.start = bodyAt + start
      child.end = bodyAt + columns.at
      node.children.push(child)
    }
    this.leave()
  }

  // ---- the wide path --------------------------------------------------------

  private wideRun(plan: Plan, body: Uint8Array, offset: i32): Array<Node> {
    const out = new Array<Node>()
    if (!this.enter()) return out
    const reader = new WideReader(body)
    while (reader.more && this.ok) {
      const start = reader.at
      const index = plan.positionOf(reader.key)
      if (index < 0) {
        if (!reader.skip()) {
          this.failWire(reader.err)
          break
        }
        const unknown = new Node()
        unknown.name = 'field ' + reader.key.toString()
        unknown.type = 'unknown'
        unknown.start = offset + start
        unknown.end = offset + reader.at
        out.push(unknown)
        continue
      }
      const field = unchecked(plan.fields[index])
      const node = new Node()
      node.name = unchecked(plan.names[index])
      node.key = field.key
      node.type = typeName(field)
      node.optional = field.op == OP_POINTER
      this.wideValue(reader, field, node, offset)
      node.start = offset + start
      node.end = offset + reader.at
      out.push(node)
    }
    if (!reader.ok) this.failWire(reader.err)
    this.leave()
    return out
  }

  private wideValue(reader: WideReader, field: PlanField, node: Node, offset: i32): void {
    const op = field.op
    if (op == OP_STRUCT) {
      const body = reader.structBody()
      if (!body.ok) return
      const inner = offset + reader.at - body.bytes.length
      node.children = body.wide
        ? this.wideRun(field.sub!, body.bytes, inner)
        : this.narrowRun(field.sub!, body.bytes, inner)
      return
    }
    if (op == OP_STRUCTS) {
      if (reader.isTable()) this.wideTable(reader, field, node, offset)
      else this.wideList(reader, field, node, offset)
      return
    }
    // Everything else the wide descriptor can size on its own, which is what the
    // eight-bit key buys and what makes this path three lines rather than a
    // switch.
    reader.skip()
  }

  private wideList(reader: WideReader, field: PlanField, node: Node, offset: i32): void {
    const before = reader.at
    const list = reader.list()
    if (!list.ok) {
      this.failWire(reader.err)
      return
    }
    const sub = field.sub
    if (sub == null) return
    if (this.depth == 1) this.rows = list.count
    const inner = list.reader
    const bodyAt = offset + reader.at - inner.buf.length
    if (!this.enter()) return
    for (let index = 0; index < list.count && this.ok; index++) {
      const start = inner.at
      const element = inner.elementStructBody()
      if (!element.ok) {
        this.failWire(inner.err)
        break
      }
      if (index < ELEMENT_LIMIT) {
        const child = new Node()
        child.name = '[' + index.toString() + ']'
        child.key = index
        child.type = 'struct'
        child.start = bodyAt + start
        child.end = bodyAt + inner.at
        const at = bodyAt + inner.at - element.bytes.length
        child.children = element.wide
          ? this.wideRun(sub, element.bytes, at)
          : this.narrowRun(sub, element.bytes, at)
        node.children.push(child)
      } else if (index == ELEMENT_LIMIT) {
        const tail = new Node()
        tail.name = '… ' + (list.count - ELEMENT_LIMIT).toString() + ' more'
        tail.key = index
        tail.type = 'struct'
        tail.start = bodyAt + start
        tail.end = bodyAt + inner.buf.length
        node.children.push(tail)
      }
    }
    this.leave()
    if (list.count > ELEMENT_LIMIT && node.children.length > 0) {
      unchecked(node.children[node.children.length - 1]).end = bodyAt + inner.buf.length
    }
    if (before == reader.at) this.fail('colbin: a list consumed no bytes')
  }

  private wideTable(reader: WideReader, field: PlanField, node: Node, offset: i32): void {
    const table = reader.table()
    if (!table.ok) {
      this.failWire(reader.err)
      return
    }
    const sub = field.sub
    if (sub == null) return
    if (this.depth == 1) this.rows = table.count
    const columns = table.reader
    const bodyAt = offset + reader.at - columns.buf.length
    if (!this.enter()) return
    while (columns.more && this.ok) {
      const start = columns.at
      const index = sub.positionOf(columns.key)
      if (index < 0) {
        if (!columns.skip()) break
        continue
      }
      const column = unchecked(sub.fields[index])
      columns.skip()
      if (!columns.ok) {
        this.failWire(columns.err)
        break
      }
      const child = new Node()
      child.name = unchecked(sub.names[index])
      child.key = column.key
      child.type = typeName(column) + ' column'
      child.start = bodyAt + start
      child.end = bodyAt + columns.at
      node.children.push(child)
    }
    this.leave()
  }
}

@inline
function isArrayOp(op: u8): bool {
  return (op >= OP_INT8S && op <= OP_INT64S) || (op >= OP_UINT16S && op <= OP_UINT64S)
}

/** What a field is called in the tree. Names the *wire* shape rather than the
 * Go type, because that is what the spans beside it are showing. */
function typeName(field: PlanField): string {
  const op = field.op == OP_POINTER ? field.elemOp : field.op
  if (op == OP_BOOL) return 'bool'
  if (op == OP_INT8) return 'int8'
  if (op == OP_INT16) return 'int16'
  if (op == OP_INT32) return 'int32'
  if (op == OP_INT64) return 'int64'
  if (op == OP_UINT8) return 'uint8'
  if (op == OP_UINT16) return 'uint16'
  if (op == OP_UINT32) return 'uint32'
  if (op == OP_UINT64) return 'uint64'
  if (op == OP_FLOAT32) return 'float32'
  if (op == OP_FLOAT64) return 'float64'
  if (op == OP_STRING) return 'string'
  if (op == OP_BYTES) return 'bytes'
  if (op == OP_INT8S) return '[]int8'
  if (op == OP_INT16S) return '[]int16'
  if (op == OP_INT32S) return '[]int32'
  if (op == OP_INT64S) return '[]int64'
  if (op == OP_UINT16S) return '[]uint16'
  if (op == OP_UINT32S) return '[]uint32'
  if (op == OP_UINT64S) return '[]uint64'
  if (op == OP_STRINGS) return '[]string'
  if (op == OP_STRUCT) return 'struct'
  if (op == OP_STRUCTS) return '[]struct'
  if (op == OP_MAP) return 'map'
  return 'unknown'
}
