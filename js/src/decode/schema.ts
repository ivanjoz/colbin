// The schema section (INTERNALS.md §10): bytes → the struct table a walk reads
// a message against. Mirrors `codec/schema_plan.go` `ParseSchema`, refusals
// included, since a section is untrusted input like any message.

import { fail, BAD_SECTION } from './fail.ts'
import { utf8 } from './strings.ts'

// Field ops (`codec/codec.go`), pinned by the format.
export const OP_BOOL = 0
export const OP_INT8 = 1
export const OP_INT16 = 2
export const OP_INT32 = 3
export const OP_INT64 = 4
export const OP_UINT8 = 5
export const OP_UINT16 = 6
export const OP_UINT32 = 7
export const OP_UINT64 = 8
export const OP_FLOAT32 = 9
export const OP_FLOAT64 = 10
export const OP_STRING = 11
export const OP_BYTES = 12
export const OP_INT8S = 13
export const OP_INT16S = 14
export const OP_INT32S = 15
export const OP_INT64S = 16
export const OP_UINT16S = 17
export const OP_UINT32S = 18
export const OP_UINT64S = 19
export const OP_STRINGS = 20
export const OP_STRUCT = 21
export const OP_STRUCTS = 22
export const OP_MAP = 23
export const OP_POINTER = 24
export const OP_ANY = 25
export const OP_ANYS = 26
export const OP_POINTER_STRUCT = 27
// Arrays of floats, of bools and of blobs (wire/arrays.go).
export const OP_FLOAT64S = 28
export const OP_FLOAT32S = 29
export const OP_BOOLS = 30
export const OP_BYTES_LIST = 31
const OP_COUNT = 32

// Map kinds (§6.4).
export const MAP_STRING = 0
export const MAP_INT = 1
export const MAP_UINT = 2
export const MAP_FLOAT64 = 3
export const MAP_BOOL = 4
export const MAP_FLOAT32 = 5
export const MAP_ANY = 6
export const MAP_STRUCT = 7
export const MAP_STRUCTS = 8
export const MAP_LIST = 9
/** A blob value: a string element's bytes that are not text. */
export const MAP_BYTES = 10
const MAP_KIND_COUNT = 11

const FLAG_WIDE = 0x01
const FLAG_ENVELOPE = 0x02
const FLAG_PAGE = 0x04
const PAGE_LINK = 255
const MAX_WIDE_FIELDS = 256
const MAX_NARROW_FIELDS = 16
const MIN_STRUCT_DEF_BYTES = 2
const MIN_FIELD_BYTES = 3
const MAX_ZERO_FIELDS = 1 << 20
export const MAX_DEPTH = 128

export class Field {
  key: number
  op: number
  name: string
  /** The struct a struct, a list of structs or a map of structs holds; -1. */
  sub: number
  /** What a pointer points at. */
  elemOp: number
  keyKind: number
  valueKind: number
  listKind: number
  /** Whether this field is the link to the next page of a paged type. */
  pageLink: boolean

  constructor(key: number, op: number, name: string) {
    this.key = key
    this.op = op
    this.name = name
    this.sub = -1
    this.elemOp = 0
    this.keyKind = 0
    this.valueKind = 0
    this.listKind = 0
    this.pageLink = false
  }
}

export class Plan {
  fields: Field[]
  /** Field index by key; -1 for a key the plan does not list. */
  byKey: Int16Array
  wide: boolean
  envelope: boolean
  page: boolean
  /** What the walk builds on first use (walk.ts `Cache`), so it lives as long
   * as the schema does. */
  cache: unknown

  constructor() {
    this.fields = []
    this.byKey = new Int16Array(MAX_WIDE_FIELDS).fill(-1)
    this.wide = false
    this.envelope = false
    this.page = false
    this.cache = null
  }
}

export class Schema {
  plans: Plan[]
  /** The section's whole length, so a caller steps over it to the body. */
  size: number

  constructor(plans: Plan[], size: number) {
    this.plans = plans
    this.size = size
  }
}

/** Refuses the section unless `ok`: every refusal is the one message. */
function check(ok: boolean): void {
  if (!ok) fail(BAD_SECTION)
}

/** One byte up to 254, or 0xFF and a u32: every length a section holds. Sets
 * `lengthWidth` to the bytes it took. */
function readLength(b: Uint8Array, at: number, end: number): number {
  check(at < end)
  lengthWidth = b[at] === 0xff ? 5 : 1
  check(at + lengthWidth <= end)
  return lengthWidth === 1 ? b[at] : (b[at + 1] | (b[at + 2] << 8) | (b[at + 3] << 16) | (b[at + 4] << 24)) >>> 0
}
let lengthWidth = 1

/** Parses a section at the front of `data`, from `start`. */
export function parseSchema(data: Uint8Array, start = 0, limit = data.length): Schema {
  const outer = readLength(data, start, limit)
  let at = start + lengthWidth
  check(outer <= limit - at)
  const end = at + outer
  const count = readLength(data, at, end)
  at += lengthWidth
  check(count > 0 && count <= (end - at) / MIN_STRUCT_DEF_BYTES)
  const plans: Plan[] = []
  for (let i = 0; i < count; i++) plans.push(new Plan())
  for (const plan of plans) at = parseStructDef(data, at, end, plan, count)

  checkValueNesting(plans)
  // A page is reachable only as the struct under key 255 of a wide struct.
  check(!plans[0].page)
  for (const plan of plans) {
    plan.fields.forEach((field, i) => {
      plan.byKey[field.key] = i
      const page = field.sub >= 0 && plans[field.sub].page
      check(!page || (field.op === OP_STRUCT && field.key === PAGE_LINK && plan.wide))
      field.pageLink = page
    })
  }
  // Bytes past the last definition are where a later version adds something.
  return new Schema(plans, end - start)
}

function parseStructDef(b: Uint8Array, at: number, end: number, plan: Plan, tableLen: number): number {
  check(at < end)
  const flags = b[at++]
  check((flags & ~(FLAG_WIDE | FLAG_ENVELOPE | FLAG_PAGE)) === 0)
  const wide = (plan.wide = (flags & FLAG_WIDE) !== 0)
  plan.envelope = (flags & FLAG_ENVELOPE) !== 0
  plan.page = (flags & FLAG_PAGE) !== 0

  const count = readLength(b, at, end)
  at += lengthWidth
  check(count <= (end - at) / MIN_FIELD_BYTES && count <= MAX_WIDE_FIELDS)
  const seen = new Uint8Array(MAX_WIDE_FIELDS)
  for (let i = 0; i < count; i++) {
    check(end - at >= 2)
    const key = b[at]
    check((wide || key < MAX_NARROW_FIELDS) && !seen[key])
    seen[key] = 1
    const nameLength = readLength(b, at + 1, end)
    const nameAt = at + 1 + lengthWidth
    check(nameLength <= end - nameAt)
    const field = new Field(key, 0, utf8(b, nameAt, nameAt + nameLength))
    at = parseDesc(b, nameAt + nameLength, end, field, tableLen)
    const op = field.op
    const value = op === OP_MAP ? field.valueKind : -1
    // A four-bit key holds no dynamic value and no map of lists.
    check(wide || !(op === OP_ANY || op === OP_ANYS || value === MAP_ANY || value === MAP_STRUCTS || value === MAP_LIST))
    plan.fields.push(field)
  }
  return at
}

function parseDesc(b: Uint8Array, at: number, end: number, field: Field, tableLen: number): number {
  check(at < end)
  const op = (field.op = b[at++])
  check(op < OP_COUNT)
  if (op === OP_STRUCT || op === OP_STRUCTS || op === OP_POINTER_STRUCT) return structIndex(b, at, end, field, tableLen)
  if (op === OP_POINTER) {
    check(at < end && b[at] < OP_COUNT)
    field.elemOp = b[at]
    return at + 1
  }
  if (op !== OP_MAP) return at
  check(end - at >= 2 && b[at] < MAP_KIND_COUNT && b[at + 1] < MAP_KIND_COUNT)
  field.keyKind = b[at]
  const value = (field.valueKind = b[at + 1])
  at += 2
  if (value === MAP_STRUCT || value === MAP_STRUCTS) return structIndex(b, at, end, field, tableLen)
  if (value !== MAP_LIST) return at
  // A map of lists holds lists of a scalar kind.
  check(at < end && b[at] <= MAP_FLOAT32)
  field.listKind = b[at]
  return at + 1
}

function structIndex(b: Uint8Array, at: number, end: number, field: Field, tableLen: number): number {
  field.sub = readLength(b, at, end)
  check(field.sub < tableLen)
  return at + lengthWidth
}

/** Refuses a struct held inside itself by value, or by-value nesting deeper or
 * wider than a walk goes (`checkValueNesting`). */
function checkValueNesting(plans: Plan[]): void {
  const zero = new Int32Array(plans.length)
  const depth = new Int32Array(plans.length)
  const measure = (at: number, level: number): void => {
    // A chain longer than a walk goes fails below anyway; failing on the level
    // keeps the recursion off the engine's stack limit.
    check(zero[at] !== -1 && level <= MAX_DEPTH)
    if (zero[at] !== 0) return
    zero[at] = -1
    let z = 1
    let d = 1
    for (const field of plans[at].fields) {
      if (field.op !== OP_STRUCT) {
        z++
        continue
      }
      measure(field.sub, level + 1)
      z += zero[field.sub]
      d = Math.max(d, depth[field.sub] + 1)
      check(z <= MAX_ZERO_FIELDS && d <= MAX_DEPTH)
    }
    zero[at] = z
    depth[at] = d
  }
  for (let i = 0; i < plans.length; i++) measure(i, 0)
}
