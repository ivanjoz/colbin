// Inference (PLAN.md §3) and enforcement (§4), producing a plan.
//
// Two passes, and the split is load-bearing. The first observes every record
// without deciding anything; the second resolves each observation into an op and
// refuses the ones that contradict themselves. A schema decided from record 0
// that record 500 contradicts is precisely how an encoder emits a body its own
// schema does not describe (§4.5, rule 1), so nothing here commits to a type
// until every record has been seen.
//
// The rules are the ones the first port implemented. What changed is what they
// produce: a `Plan` of `fieldOp`s rather than the old format's type classes, and
// with it three decisions the old format did not have.
//
// # Field ids are sequential, in first-seen order
//
// REFACTOR_PLAN.md §5.2. The old module hashed a field name with fnv8 and probed
// past collisions, matching what Go does for an untagged struct — and a derived
// id lands anywhere in 0..255, so every message it wrote used eight-bit keys.
// Numbering them 0, 1, 2 instead puts any object of sixteen fields or fewer on
// the four-bit fast path, which is a byte per field smaller. The section carries
// the ids, so nothing that reads the section can be confused by it.
//
// First-seen order stays exactly as load-bearing as it was, for a different
// reason: it is no longer the input to a hash, it *is* the id.
//
// # The root is a struct, and JSON's top level often is not
//
// REFACTOR_PLAN.md §5.1. `Marshal` encodes a struct and nothing else, so an
// array or a bare scalar at the top level is wrapped in a one-field envelope.
// The envelope is marked in the section rather than guessed at on the way back —
// see section.ts.
//
// # What the format does not carry
//
// An array of floats, an array of bools and an array of arrays have no op. That
// is the format's limit rather than this module's: `sliceOp` in codec/codec.go
// resolves integers, strings and structs and refuses the rest. They are refused
// here with a message that says so, because the alternative is a message that
// does not decode.

import { D_CONFLICT, D_LIMIT, D_UNSUPPORTED, Diag } from './diag'
import { Doc, K_ARRAY, K_BOOL, K_FLOAT, K_INT, K_NULL, K_OBJECT, K_STRING, K_UINT } from './json'
import {
  OP_BOOL,
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
  columnableOp,
} from './plan'

/** What eight key bits buy, and therefore the most fields one object may hold. */
const MAX_FIELDS: i32 = 256

/** The name the envelope's single field takes. It is only ever built when the
 * top level is *not* an object, so it can never collide with a caller's key. */
export const ENVELOPE_FIELD: string = 'rows'

// Value categories. Two different categories in one position is a conflict; the
// numeric one absorbs int, uint and float internally because those unify.
const C_BOOL: i32 = 0
const C_NUM: i32 = 1
const C_STR: i32 = 2
const C_ARR: i32 = 3
const C_OBJ: i32 = 4
const C_COUNT: i32 = 5

// @ts-ignore: decorator
@lazy
const CATEGORY_NAMES: StaticArray<string> = ['bool', 'number', 'string', 'array', 'object']

/** What one position in the shape was observed to hold, across every record. */
class Obs {
  sawNull: bool = false
  sawNeg: bool = false
  sawUint: bool = false
  sawFloat: bool = false

  /** First record and source offset each category appeared at, or -1. */
  catRec: StaticArray<i32> = new StaticArray<i32>(C_COUNT)
  catOff: StaticArray<i32> = new StaticArray<i32>(C_COUNT)

  elem: Obs | null = null
  fields: Array<FieldObs> = []
  /** Objects merged into this position; a field short of it is nullable. */
  objects: i32 = 0

  constructor() {
    for (let index = 0; index < C_COUNT; index++) {
      unchecked((this.catRec[index] = -1))
      unchecked((this.catOff[index] = -1))
    }
  }

  @inline note(category: i32, record: i32, offset: i32): void {
    if (unchecked(this.catRec[category]) < 0) {
      unchecked((this.catRec[category] = record))
      unchecked((this.catOff[category] = offset))
    }
  }
}

class FieldObs {
  name: Uint8Array
  obs: Obs = new Obs()
  present: i32 = 0

  constructor(name: Uint8Array) {
    this.name = name
  }
}

/** An inferred schema: the root plan, and the nodes the body is written from. */
export class Inferred {
  plan: Plan = new Plan()
  /** The document node the root plan describes. For an envelope this is the
   * top-level value; otherwise it is the object itself. */
  root: i32 = -1
}

export class Inferrer {
  doc: Doc
  diag: Diag
  path: Array<string> = []
  record: i32 = 0

  constructor(doc: Doc, diag: Diag) {
    this.doc = doc
    this.diag = diag
  }

  pathString(): string {
    let out = ''
    for (let index = 0; index < this.path.length; index++) out += unchecked(this.path[index])
    return out
  }

  infer(): Inferred | null {
    const doc = this.doc
    const root = doc.root
    const kind = doc.kindOf(root)

    if (kind == K_NULL) {
      this.diag.fail(D_CONFLICT, unchecked(doc.off[root]), '', 'null has no shape to infer')
      return null
    }
    if (kind == K_ARRAY && doc.count(root) == 0) {
      this.diag.fail(
        D_CONFLICT,
        unchecked(doc.off[root]),
        '',
        'an empty array has no shape to infer',
      )
      return null
    }

    const out = new Inferred()
    out.root = root

    if (kind == K_OBJECT) {
      // The ordinary case: the top level is the record, and the root plan is its
      // struct. No envelope, and nothing to unwrap on the way back.
      const obs = new Obs()
      this.record = 0
      this.path = []
      this.observe(obs, root)
      if (!this.diag.ok) return null
      this.path = []
      const plan = this.resolveStruct(obs)
      if (plan == null) return null
      out.plan = plan
      return out
    }

    // Everything else goes in an envelope, because the root of a colbin message
    // is a struct and nothing else.
    const obs = new Obs()
    if (kind == K_ARRAY) {
      // Each element is a record, so a conflict names the record it appeared in
      // rather than "inside the array".
      const count = doc.count(root)
      for (let index = 0; index < count; index++) {
        this.record = index
        this.path = []
        this.path.push('[' + index.toString() + ']')
        this.observe(obs.elem == null ? this.makeElem(obs) : obs.elem!, doc.childAt(root, index))
        if (!this.diag.ok) return null
      }
      obs.note(C_ARR, 0, unchecked(doc.off[root]))
    } else {
      this.record = 0
      this.path = []
      this.observe(obs, root)
      if (!this.diag.ok) return null
    }

    this.path = []
    const field = new PlanField()
    field.key = 0
    const op = this.resolveOp(obs, field)
    if (op < 0) return null
    field.op = <u8>op

    const plan = new Plan()
    plan.isEnvelope = true
    plan.fields.push(field)
    plan.names.push(ENVELOPE_FIELD)
    plan.finish()
    out.plan = plan
    return out
  }

  private makeElem(obs: Obs): Obs {
    const elem = new Obs()
    obs.elem = elem
    return elem
  }

  // ---- pass one: observe ----------------------------------------------------

  observe(obs: Obs, node: i32): void {
    if (!this.diag.ok) return
    const doc = this.doc
    const kind = doc.kindOf(node)
    const offset = unchecked(doc.off[node])

    if (kind == K_NULL) {
      obs.sawNull = true
      return
    }
    if (kind == K_BOOL) {
      obs.note(C_BOOL, this.record, offset)
      return
    }
    if (kind == K_INT) {
      obs.note(C_NUM, this.record, offset)
      if (unchecked(doc.num[node]) < 0) obs.sawNeg = true
      return
    }
    if (kind == K_UINT) {
      obs.note(C_NUM, this.record, offset)
      obs.sawUint = true
      return
    }
    if (kind == K_FLOAT) {
      obs.note(C_NUM, this.record, offset)
      obs.sawFloat = true
      return
    }
    if (kind == K_STRING) {
      obs.note(C_STR, this.record, offset)
      return
    }
    if (kind == K_ARRAY) {
      obs.note(C_ARR, this.record, offset)
      const count = doc.count(node)
      // Created only when there is an element to observe: a field that is always
      // an empty array must stay distinguishable from one that is always null.
      if (count > 0 && obs.elem == null) this.makeElem(obs)
      const elem = obs.elem
      if (elem == null) return
      for (let index = 0; index < count; index++) {
        this.path.push('[' + index.toString() + ']')
        this.observe(elem, doc.childAt(node, index))
        this.path.pop()
        if (!this.diag.ok) return
      }
      return
    }

    // An object.
    obs.note(C_OBJ, this.record, offset)
    obs.objects++
    const count = doc.count(node)
    for (let index = 0; index < count; index++) {
      const key = doc.keyOf(node, index)
      let slot = this.findField(obs, key)
      if (slot < 0) {
        if (obs.fields.length >= MAX_FIELDS) {
          this.diag.fail(
            D_LIMIT,
            offset,
            this.pathString(),
            'an object with more than ' +
              MAX_FIELDS.toString() +
              ' fields cannot be encoded: a key is one byte',
          )
          return
        }
        obs.fields.push(new FieldObs(copyBytes(key)))
        slot = obs.fields.length - 1
      }
      const field = unchecked(obs.fields[slot])
      // A duplicate key overwrites, as JSON.parse does, so the count still
      // reflects records rather than occurrences.
      if (field.present < obs.objects) field.present = obs.objects
      this.path.push('.' + bytesToString(key))
      this.observe(field.obs, doc.childAt(node, index))
      this.path.pop()
      if (!this.diag.ok) return
    }
  }

  private findField(obs: Obs, key: Uint8Array): i32 {
    for (let index = 0; index < obs.fields.length; index++) {
      if (bytesEqual(unchecked(obs.fields[index]).name, key)) return index
    }
    return -1
  }

  // ---- pass two: resolve ----------------------------------------------------

  /** The incumbent category, or -1 when the position contradicts itself. */
  private categoryOf(obs: Obs): i32 {
    let seen = 0
    for (let category = 0; category < C_COUNT; category++) {
      if (unchecked(obs.catRec[category]) >= 0) seen++
    }
    if (seen > 1) {
      this.reportConflict(obs)
      return -1
    }
    if (seen == 0) return -2 // only ever null
    let winner = -1
    for (let category = 0; category < C_COUNT; category++) {
      if (unchecked(obs.catRec[category]) < 0) continue
      if (winner < 0 || unchecked(obs.catRec[category]) < unchecked(obs.catRec[winner])) {
        winner = category
      }
    }
    return winner
  }

  /**
   * One observation into an op, filling `field` with whatever the op does not
   * say by itself. Returns -1 on a refusal.
   */
  private resolveOp(obs: Obs, field: PlanField): i32 {
    const category = this.categoryOf(obs)
    if (category == -1) return -1

    if (category == -2) {
      this.diag.warn(this.pathString() + ' was only ever null; encoded as a nullable string')
      field.elemOp = OP_STRING
      return OP_POINTER
    }

    if (category == C_OBJ) {
      const sub = this.resolveStruct(obs)
      if (sub == null) return -1
      field.sub = sub
      // A null where an object belongs cannot be a pointer — the format refuses
      // a pointer to a composite — so it decodes back as an object of zeros.
      if (obs.sawNull) {
        this.diag.warn(
          this.pathString() + ' is sometimes null; a null object decodes as an object of zeros',
        )
      }
      return OP_STRUCT
    }

    if (category == C_ARR) {
      return this.resolveArray(obs, field)
    }

    let op = OP_STRING
    if (category == C_BOOL) {
      op = OP_BOOL
    } else if (category == C_NUM) {
      const numeric = this.resolveNumber(obs)
      if (numeric < 0) return -1
      op = <u8>numeric
    }

    // A null, or a key absent from some record, makes the column a pointer:
    // one that is nil is omitted and costs nothing, and one pointing at a zero
    // writes an explicit zero so the two stay distinguishable.
    if (obs.sawNull) {
      field.elemOp = op
      return OP_POINTER
    }
    return op
  }

  private resolveNumber(obs: Obs): i32 {
    if (obs.sawFloat) {
      if (obs.sawUint) {
        this.diag.warn(
          this.pathString() +
            ' mixes integers above 2^63 with floats; the column is float64 and those integers lose precision',
        )
      }
      return OP_FLOAT64
    }
    if (obs.sawUint) {
      if (obs.sawNeg) {
        this.diag.fail(
          D_CONFLICT,
          unchecked(obs.catOff[C_NUM]),
          '[' + unchecked(obs.catRec[C_NUM]).toString() + ']' + this.pathString(),
          'values span both below zero and above the int64 maximum, so no single integer column holds them exactly',
        )
        return -1
      }
      return OP_UINT64
    }
    return OP_INT64
  }

  private resolveArray(obs: Obs, field: PlanField): i32 {
    const elem = obs.elem
    if (elem == null) {
      this.diag.warn(
        this.pathString() + ' was always an empty array; the element type is taken as string',
      )
      return OP_STRINGS
    }
    const category = this.categoryOf(elem)
    if (category == -1) return -1
    if (category == -2) {
      this.diag.warn(this.pathString() + ' holds only nulls; the element type is taken as string')
      return OP_STRINGS
    }
    if (category == C_STR) return OP_STRINGS
    if (category == C_OBJ) {
      const sub = this.resolveStruct(elem)
      if (sub == null) return -1
      field.sub = sub
      return OP_STRUCTS
    }
    if (category == C_NUM) {
      const numeric = this.resolveNumber(elem)
      if (numeric < 0) return -1
      if (numeric == OP_FLOAT64) {
        return this.unsupported('an array of floats')
      }
      return numeric == OP_UINT64 ? OP_UINT64S : OP_INT64S
    }
    if (category == C_BOOL) return this.unsupported('an array of booleans')
    return this.unsupported('an array of arrays')
  }

  /**
   * Every op the format carries as an array element is an integer, a string or
   * a struct. The rest are refused here rather than encoded into something that
   * does not decode.
   */
  private unsupported(what: string): i32 {
    this.diag.fail(
      D_UNSUPPORTED,
      -1,
      this.pathString(),
      what + ' has no form on the wire: the format carries arrays of integers, of strings and of objects',
    )
    return -1
  }

  private resolveStruct(obs: Obs): Plan | null {
    const plan = new Plan()
    for (let index = 0; index < obs.fields.length; index++) {
      const observed = unchecked(obs.fields[index])
      this.path.push('.' + bytesToString(observed.name))
      const field = new PlanField()
      // Sequential, in first-seen order. See the header.
      field.key = <u8>index
      const op = this.resolveOp(observed.obs, field)
      this.path.pop()
      if (op < 0) return null
      field.op = <u8>op

      // A key absent from some record is indistinguishable from an explicit null
      // on the wire (PLAN.md §6), and both make the column a pointer — where the
      // format allows one.
      if (observed.present < obs.objects && field.op != OP_POINTER && pointable(field.op)) {
        field.elemOp = field.op
        field.op = OP_POINTER
      }
      plan.fields.push(field)
      plan.names.push(bytesToString(observed.name))
    }
    if (plan.fields.length == 0) {
      this.diag.fail(
        D_CONFLICT,
        -1,
        this.pathString(),
        'an object with no fields has nothing to encode',
      )
      return null
    }
    plan.finish()
    return plan
  }

  private reportConflict(obs: Obs): void {
    // The incumbent is whatever appeared first; the offender is the newcomer.
    let first = -1
    let last = -1
    for (let category = 0; category < C_COUNT; category++) {
      if (unchecked(obs.catRec[category]) < 0) continue
      if (first < 0 || unchecked(obs.catRec[category]) < unchecked(obs.catRec[first])) {
        first = category
      }
      if (last < 0 || unchecked(obs.catRec[category]) > unchecked(obs.catRec[last])) {
        last = category
      }
    }
    const firstRecord = unchecked(obs.catRec[first])
    const lastRecord = unchecked(obs.catRec[last])
    let where = 'record ' + firstRecord.toString()
    if (lastRecord - firstRecord > 1) {
      where = 'records ' + firstRecord.toString() + '-' + (lastRecord - 1).toString()
    }
    this.diag.fail(
      D_CONFLICT,
      unchecked(obs.catOff[last]),
      '[' + lastRecord.toString() + ']' + this.pathString(),
      'type conflict: ' +
        unchecked(CATEGORY_NAMES[last]) +
        ', but ' +
        where +
        ' had ' +
        unchecked(CATEGORY_NAMES[first]),
    )
  }
}

/** Whether an op can sit behind a pointer. The format refuses a pointer to a
 * composite: those carry a length already, and what a nil one should mean is
 * not settled. */
@inline
function pointable(op: u8): bool {
  return columnableOp(op)
}

function copyBytes(src: Uint8Array): Uint8Array {
  const out = new Uint8Array(src.length)
  memory.copy(out.dataStart, src.dataStart, <usize>src.length)
  return out
}

function bytesEqual(a: Uint8Array, b: Uint8Array): bool {
  if (a.length != b.length) return false
  return memory.compare(a.dataStart, b.dataStart, <usize>a.length) == 0
}

function bytesToString(bytes: Uint8Array): string {
  return String.UTF8.decodeUnsafe(bytes.dataStart, <usize>bytes.length, false)
}

export function inferPlan(doc: Doc, diag: Diag): Inferred | null {
  return new Inferrer(doc, diag).infer()
}
