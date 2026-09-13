// The schema section: a type, in bytes, for a reader that has not got it.
//
// Port of codec/schema_plan.go's read half. The write half belongs to the
// encoder and arrives with it (REFACTOR_PLAN.md phase 4).
//
//	section   := [byteLength] [structCount] structDef{structCount}
//	structDef := [flags:1] [fieldCount] field{fieldCount}
//	field     := [key:1] [nameLen] name [desc]
//	desc      := [op:1] extra
//
// `extra` by op:
//
//	scalars, string, bytes, arrays   —   (the op names the element type)
//	opStruct, opStructs              [structIndex]
//	opMap                            [keyKind:1] [valueKind:1]
//	opPointer                        [elemOp:1]
//
// Every length is the format's own — one byte escaping to four behind 0xFF —
// rather than a varint, for the reason nothing else here is a varint. byteLength
// covers everything after itself, so a reader that wants the body and not the
// schema adds it to the cursor and is done.
//
// # Struct hoisting
//
// Structs are an indexed table rather than inlined, so a self-referential type
// (`type Node struct{ Kids []Node }`) describes itself in finite space. The
// index is reserved before the fields are walked, so a field that reaches back
// resolves to an index already assigned. JSON cannot express such a type, so
// this matters only on the read side — which is exactly where a crafted section
// could otherwise recurse forever, and why every plan exists before any is
// filled below.
//
// # Everything here is untrusted
//
// A section arrives from a peer, so every length is checked against what is
// actually there and every code against what this version assigns. Nothing may
// allocate on a number the section merely claims: a struct table is bounded by
// the bytes left to hold it, which is the cheapest honest bound there is.

import { Writer } from './bytes'
import {
  MAP_KIND_COUNT,
  OP_COUNT,
  OP_MAP,
  OP_POINTER,
  OP_STRUCT,
  OP_STRUCTS,
  Plan,
  PlanField,
} from './plan'

/** The smallest a definition can be, which is what bounds a declared count
 * against the bytes actually left: a structDef is a flags byte and a field
 * count, and a field is a key, a name length and an op. */
const MIN_STRUCT_DEF_BYTES: i32 = 2
const MIN_FIELD_BYTES: i32 = 3

/** The structDef flag saying the run it describes uses eight-bit keys. */
const SCHEMA_WIDE_KEYS: u8 = 0x01

/**
 * The structDef flag saying this struct is a one-field envelope round a document
 * whose top level was not an object (REFACTOR_PLAN.md §5.1).
 *
 * It lives here rather than in the root byte for a reason worth stating. The
 * root's unallocated detail bits are reserved by the *format*, and `rootOf`
 * refuses every one it does not assign — so a message marked there would be
 * refused outright by Go, which is a worse outcome than a cosmetic difference.
 * A structDef's flags byte has seven spare bits and `parseStructDef` reads only
 * bit 0, ignoring the rest, which is exactly the room the section was designed
 * to have. So Go renders an enveloped document as `{"rows":[…]}` — the cost
 * §5.1 documented — and this module unwraps it, and neither has to be told
 * anything the other does not already carry.
 *
 * It is a claim on a shared format made from one side. It belongs in the Go
 * schema.go comment too, and until it is there it is written down here.
 */
const SCHEMA_ENVELOPE: u8 = 0x02

/** What eight key bits buy, and therefore the most fields a section may name. */
const MAX_WIDE_FIELDS: i32 = 256

const LENGTH_ESCAPE: u8 = 0xff

/** A parsed section: the root plan, and how many bytes the section occupied. */
export class Section {
  plan: Plan | null = null
  /** The section's whole length, so a caller can step over it to the body. */
  size: i32 = 0
  error: string = ''

  @inline get ok(): bool {
    return this.plan != null
  }
}

/** A length and its width, which AssemblyScript has no tuple to return. */
class Length {
  value: i32 = 0
  width: i32 = 0
  ok: bool = false
}

/**
 * A length written the way this format writes every count and every element
 * length: one byte, escaping to four behind 0xFF. Reading one back is a compare
 * and a load rather than a loop whose trip count is data.
 */
function readLength(buf: Uint8Array, at: i32): Length {
  const out = new Length()
  if (at >= buf.length) return out
  const first = unchecked(buf[at])
  if (first != LENGTH_ESCAPE) {
    out.value = <i32>first
    out.width = 1
    out.ok = true
    return out
  }
  if (at + 5 > buf.length) return out
  let value: u64 = 0
  for (let index = 0; index < 4; index++) {
    value |= (<u64>unchecked(buf[at + 1 + index])) << <u64>(index << 3)
  }
  if (value > 0x7fffffff) return out
  out.value = <i32>value
  out.width = 5
  out.ok = true
  return out
}

/** The cursor a parse carries, so the failure has one place to live. */
class Cursor {
  buf: Uint8Array
  at: i32 = 0
  error: string = ''

  constructor(buf: Uint8Array) {
    this.buf = buf
  }

  @inline get ok(): bool {
    return this.error.length == 0
  }

  fail(message: string): void {
    if (this.ok) this.error = message
  }

  /** The one error every length check ends at. A section that ends early is
   * corrupt or truncated, and which byte ran out first says nothing a caller
   * can act on. */
  short(): void {
    this.fail('a schema section ends inside a definition')
  }
}

/**
 * Reads a section written by Schema.Bytes, or the one carried in front of a
 * self-describing message.
 */
export function parseSection(data: Uint8Array): Section {
  const out = new Section()

  const outer = readLength(data, 0)
  if (!outer.ok || outer.value > data.length - outer.width) {
    out.error = 'a schema section declares a length the bytes cannot hold'
    return out
  }
  out.size = outer.width + outer.value
  const body = data.subarray(outer.width, out.size)

  const cursor = new Cursor(body)
  const table = readLength(body, 0)
  if (!table.ok) {
    out.error = 'a schema section ends inside a definition'
    return out
  }
  cursor.at = table.width
  const count = table.value
  if (count == 0) {
    out.error = 'a schema section describes no struct'
    return out
  }
  if (count > (body.length - cursor.at) / MIN_STRUCT_DEF_BYTES) {
    out.error = 'a schema section declares more structs than its bytes can hold'
    return out
  }

  // Every plan exists before any is filled, so a field may point forward as well
  // as back — a struct table is an index, not an order — and a self-referential
  // type resolves rather than recursing.
  const plans = new Array<Plan>(count)
  for (let index = 0; index < count; index++) plans[index] = new Plan()
  for (let index = 0; index < count; index++) {
    parseStructDef(cursor, unchecked(plans[index]), plans)
    if (!cursor.ok) {
      out.error = cursor.error
      return out
    }
  }
  for (let index = 0; index < count; index++) unchecked(plans[index]).finishParsed()

  // Bytes past the last definition are left alone rather than refused: the
  // byteLength is what a reader steps over, and room behind the definitions is
  // where a later version would put something this one does not know about.
  out.plan = unchecked(plans[0])
  return out
}

function parseStructDef(cursor: Cursor, plan: Plan, plans: Array<Plan>): void {
  if (cursor.at >= cursor.buf.length) {
    cursor.short()
    return
  }
  const flags = unchecked(cursor.buf[cursor.at])
  plan.isWide = (flags & SCHEMA_WIDE_KEYS) != 0
  plan.isEnvelope = (flags & SCHEMA_ENVELOPE) != 0
  cursor.at++

  const fieldCount = readLength(cursor.buf, cursor.at)
  if (!fieldCount.ok) {
    cursor.short()
    return
  }
  cursor.at += fieldCount.width
  const count = fieldCount.value
  if (count > (cursor.buf.length - cursor.at) / MIN_FIELD_BYTES) {
    cursor.short()
    return
  }
  if (count > MAX_WIDE_FIELDS) {
    cursor.fail('a schema section declares more fields than a one-byte key holds')
    return
  }

  for (let index = 0; index < count; index++) {
    if (cursor.at + 2 > cursor.buf.length) {
      cursor.short()
      return
    }
    const field = new PlanField()
    field.key = unchecked(cursor.buf[cursor.at])
    const nameLength = readLength(cursor.buf, cursor.at + 1)
    if (!nameLength.ok || nameLength.value > cursor.buf.length - cursor.at - 1 - nameLength.width) {
      cursor.short()
      return
    }
    const from = cursor.at + 1 + nameLength.width
    plan.names.push(String.UTF8.decodeUnsafe(
      cursor.buf.dataStart + <usize>from,
      nameLength.value,
      false,
    ))
    cursor.at = from + nameLength.value

    parseDesc(cursor, field, plans)
    if (!cursor.ok) return
    plan.fields.push(field)
  }
}

function parseDesc(cursor: Cursor, field: PlanField, plans: Array<Plan>): void {
  if (cursor.at >= cursor.buf.length) {
    cursor.short()
    return
  }
  const op = unchecked(cursor.buf[cursor.at])
  if (op >= OP_COUNT) {
    cursor.fail('a schema section names a field type this version does not assign')
    return
  }
  field.op = op
  cursor.at++

  if (op == OP_STRUCT || op == OP_STRUCTS) {
    const index = readLength(cursor.buf, cursor.at)
    if (!index.ok) {
      cursor.short()
      return
    }
    if (index.value >= plans.length) {
      cursor.fail('a schema section points at a struct that is not in its table')
      return
    }
    field.sub = unchecked(plans[index.value])
    cursor.at += index.width
    return
  }
  if (op == OP_MAP) {
    if (cursor.at + 2 > cursor.buf.length) {
      cursor.short()
      return
    }
    const keyKind = unchecked(cursor.buf[cursor.at])
    const valueKind = unchecked(cursor.buf[cursor.at + 1])
    if (keyKind >= MAP_KIND_COUNT || valueKind >= MAP_KIND_COUNT) {
      cursor.fail('a schema section names a map kind this version does not assign')
      return
    }
    field.keyKind = keyKind
    field.valueKind = valueKind
    cursor.at += 2
    return
  }
  if (op == OP_POINTER) {
    if (cursor.at >= cursor.buf.length) {
      cursor.short()
      return
    }
    const elemOp = unchecked(cursor.buf[cursor.at])
    if (elemOp >= OP_COUNT) {
      cursor.fail('a schema section names a field type this version does not assign')
      return
    }
    field.elemOp = elemOp
    cursor.at++
    return
  }
  // Everything else is named by its op alone: an array's element type is in the
  // op, and a string and a blob are different ops.
}

// ---- the write half ---------------------------------------------------------

/**
 * Serialises a plan and everything it reaches.
 *
 * Structs are hoisted into an indexed table, and the index is reserved before
 * the fields are walked, so a field that reaches back resolves to an index
 * already assigned rather than recursing. JSON cannot express a self-referential
 * type, so that cannot happen from this encoder — it is kept because the table
 * is the format's shape and half a rule is worse than a whole one.
 */
export function buildSection(root: Plan): Uint8Array {
  const builder = new SectionBuilder()
  builder.structIndex(root) // the root is index 0, by being asked for first

  const body = new Writer(256)
  appendLength(body, builder.defs.length)
  for (let index = 0; index < builder.defs.length; index++) {
    const def = unchecked(builder.defs[index])
    body.writeBytes(def, 0, def.length)
  }
  const bytes = body.take()

  const section = new Writer(bytes.length + 5)
  appendLength(section, bytes.length)
  section.writeBytes(bytes, 0, bytes.length)
  return section.take()
}

class SectionBuilder {
  plans: Array<Plan> = new Array<Plan>()
  defs: Array<Uint8Array> = new Array<Uint8Array>()

  /** A plan's slot in the table, describing it on first use. */
  structIndex(plan: Plan): i32 {
    for (let index = 0; index < this.plans.length; index++) {
      if (unchecked(this.plans[index]) === plan) return index
    }
    const at = this.plans.length
    this.plans.push(plan)
    this.defs.push(new Uint8Array(0)) // reserved, filled in below

    const def = new Writer(64)
    let flags: u8 = 0
    if (plan.isWide) flags |= SCHEMA_WIDE_KEYS
    if (plan.isEnvelope) flags |= SCHEMA_ENVELOPE
    def.writeByte(flags)
    appendLength(def, plan.fields.length)
    for (let index = 0; index < plan.fields.length; index++) {
      const field = unchecked(plan.fields[index])
      def.writeByte(field.key)
      const name = Uint8Array.wrap(String.UTF8.encode(unchecked(plan.names[index]), false))
      appendLength(def, name.length)
      def.writeBytes(name, 0, name.length)
      this.appendDesc(def, field)
    }
    this.defs[at] = def.take()
    return at
  }

  /** One field's type: the op, and whatever the op does not say by itself. */
  private appendDesc(def: Writer, field: PlanField): void {
    def.writeByte(field.op)
    if (field.op == OP_STRUCT || field.op == OP_STRUCTS) {
      const sub = field.sub
      appendLength(def, sub == null ? 0 : this.structIndex(sub))
      return
    }
    if (field.op == OP_MAP) {
      def.writeByte(field.keyKind)
      def.writeByte(field.valueKind)
      return
    }
    if (field.op == OP_POINTER) {
      def.writeByte(field.elemOp)
    }
    // Everything else is named by its op alone.
  }
}

/** A length, the way this format writes every count: one byte, escaping to four
 * behind 0xFF. Not a varint, for the reason nothing here is a varint. */
function appendLength(out: Writer, value: i32): void {
  if (value < <i32>LENGTH_ESCAPE) {
    out.writeByte(<u8>value)
    return
  }
  out.writeByte(LENGTH_ESCAPE)
  out.writeLE(<u64>value, 4)
}
