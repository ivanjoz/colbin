// Inference (PLAN.md §3) and enforcement (§4).
//
// Two passes, and the split is load-bearing. The first observes every record
// without deciding anything; the second resolves each observation into a type
// and refuses the ones that contradict themselves. A schema decided from record
// 0 that record 500 contradicts is precisely how an encoder emits a body its own
// schema does not describe (§4.5, rule 1), so nothing here commits to a type
// until every record has been seen.

import { Diag, D_CONFLICT, D_LIMIT, D_UNSUPPORTED } from './diag'
import { Doc, K_ARRAY, K_BOOL, K_FLOAT, K_INT, K_NULL, K_OBJECT, K_STRING, K_UINT } from './json'
import {
  FT_ARRAY,
  FT_FLOAT,
  FT_INT,
  FT_STRING,
  FT_STRUCT,
  Field,
  MAX_FIELDS,
  SK_BOOL,
  SK_FLOAT64,
  SK_INT64,
  SK_UINT64,
  Type,
  assignFieldIDs,
} from './schema'

// Value categories. Two different categories in one column is a conflict; the
// numeric one absorbs int, uint and float internally because those unify.
const C_BOOL: i32 = 0
const C_NUM: i32 = 1
const C_STR: i32 = 2
const C_ARR: i32 = 3
const C_OBJ: i32 = 4
const C_COUNT: i32 = 5

const CATEGORY_NAMES: StaticArray<string> = ['bool', 'number', 'string', 'array', 'object']

/** What one position in the shape was observed to hold, across every record. */
class Obs {
  sawNull: bool = false
  sawInt: bool = false
  sawNeg: bool = false
  sawUint: bool = false
  sawFloat: bool = false

  /** First record and source offset each category appeared at, or -1. */
  catRec: StaticArray<i32> = new StaticArray<i32>(C_COUNT)
  catOff: StaticArray<i32> = new StaticArray<i32>(C_COUNT)

  elem: Obs | null = null // array element
  fields: Array<FieldObs> = [] // struct fields, first-seen order
  /** Objects merged into this position; a field short of it is nullable. */
  objects: i32 = 0

  constructor() {
    for (let i = 0; i < C_COUNT; i++) {
      unchecked((this.catRec[i] = -1))
      unchecked((this.catOff[i] = -1))
    }
  }

  @inline note(cat: i32, record: i32, offset: i32): void {
    if (unchecked(this.catRec[cat]) < 0) {
      unchecked((this.catRec[cat] = record))
      unchecked((this.catOff[cat] = offset))
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

export const SHAPE_RECORDS: i32 = 0 // array of objects
export const SHAPE_SINGLE: i32 = 1 // one object, rendered back as an object
export const SHAPE_VALUE: i32 = 2 // value mode

export class Schema {
  shape: i32 = SHAPE_RECORDS
  /** The record type for records mode; the value's type for value mode. */
  root: Type = new Type(FT_STRUCT, SK_INT64)
  /** Node indices of the records, in order. */
  records: Array<i32> = []
}

export class Inferrer {
  doc: Doc
  diag: Diag
  /** Path segments of the position being walked, for diagnostics. */
  path: Array<string> = []
  record: i32 = 0

  constructor(doc: Doc, diag: Diag) {
    this.doc = doc
    this.diag = diag
  }

  pathString(): string {
    let out = ''
    for (let i = 0; i < this.path.length; i++) out += unchecked(this.path[i])
    return out
  }

  infer(): Schema | null {
    const doc = this.doc
    const root = doc.root
    const schema = new Schema()
    const kind = doc.kindOf(root)

    if (kind == K_ARRAY) {
      const n = doc.count(root)
      if (n == 0) {
        this.diag.fail(D_CONFLICT, unchecked(doc.off[root]), '', 'an empty array has no shape to infer')
        return null
      }
      let allObjects = true
      for (let i = 0; i < n; i++) {
        if (doc.kindOf(doc.childAt(root, i)) != K_OBJECT) {
          allObjects = false
          break
        }
      }
      if (!allObjects) {
        this.diag.fail(D_UNSUPPORTED, unchecked(doc.off[root]),
          '', 'value mode (an array that is not an array of objects) is not implemented yet')
        return null
      }
      schema.shape = SHAPE_RECORDS
      for (let i = 0; i < n; i++) schema.records.push(doc.childAt(root, i))
    } else if (kind == K_OBJECT) {
      schema.shape = SHAPE_SINGLE
      schema.records.push(root)
    } else if (kind == K_NULL) {
      this.diag.fail(D_CONFLICT, unchecked(doc.off[root]), '', 'null has no shape to infer')
      return null
    } else {
      this.diag.fail(D_UNSUPPORTED, unchecked(doc.off[root]),
        '', 'value mode (a top-level value that is not an object or an array of objects) is not implemented yet')
      return null
    }

    // Pass one: observe every record without deciding anything.
    const obs = new Obs()
    for (let i = 0; i < schema.records.length; i++) {
      this.record = i
      this.path = []
      this.path.push('[' + i.toString() + ']')
      this.observe(obs, unchecked(schema.records[i]))
      if (!this.diag.ok) return null
    }

    // Pass two: resolve, which is where a contradiction becomes an error.
    this.path = []
    const t = this.resolve(obs)
    if (t == null) return null
    schema.root = t
    return schema
  }

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
      obs.sawInt = true
      if (unchecked(doc.num[node]) < 0) obs.sawNeg = true
      return
    }
    if (kind == K_UINT) {
      obs.note(C_NUM, this.record, offset)
      obs.sawInt = true
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
      const n = doc.count(node)
      // Created only when there is an element to observe: a field that is always
      // an empty array must stay distinguishable from one that is always null,
      // because the two get different warnings and different element types.
      if (n > 0 && obs.elem == null) obs.elem = new Obs()
      const elem = obs.elem
      for (let i = 0; i < n; i++) {
        this.path.push('[' + i.toString() + ']')
        this.observe(elem!, doc.childAt(node, i))
        this.path.pop()
        if (!this.diag.ok) return
      }
      return
    }

    // K_OBJECT
    obs.note(C_OBJ, this.record, offset)
    obs.objects++
    const n = doc.count(node)
    for (let i = 0; i < n; i++) {
      const key = doc.keyOf(node, i)
      let slot = this.findField(obs, key)
      if (slot < 0) {
        if (obs.fields.length >= MAX_FIELDS) {
          this.diag.fail(D_LIMIT, offset, this.pathString(),
            'an object with more than ' + MAX_FIELDS.toString() +
              ' fields cannot be encoded: field id 255 is the record terminator')
          return
        }
        obs.fields.push(new FieldObs(copyBytes(key)))
        slot = obs.fields.length - 1
      }
      const f = unchecked(obs.fields[slot])
      // A duplicate key overwrites, as JSON.parse does, so the count still
      // reflects records rather than occurrences.
      if (f.present < obs.objects) f.present = obs.objects
      this.path.push('.' + bytesToString(key))
      this.observe(f.obs, doc.childAt(node, i))
      this.path.pop()
      if (!this.diag.ok) return
    }
  }

  findField(obs: Obs, key: Uint8Array): i32 {
    for (let i = 0; i < obs.fields.length; i++) {
      if (bytesEqual(unchecked(obs.fields[i]).name, key)) return i
    }
    return -1
  }

  /** Turns one observation into a type, or fails with the contradiction. */
  resolve(obs: Obs): Type | null {
    let seen = 0
    for (let c = 0; c < C_COUNT; c++) {
      if (unchecked(obs.catRec[c]) >= 0) seen++
    }

    if (seen > 1) {
      this.reportConflict(obs)
      return null
    }

    if (seen == 0) {
      // Only ever null, or an array that was always empty.
      this.diag.warn(this.pathString() + ' was only ever null; encoded as a nullable string')
      const t = new Type(FT_STRING, SK_INT64)
      t.nullable = true
      return t
    }

    // Pick the incumbent category: the one with the lowest first record.
    let cat = -1
    for (let c = 0; c < C_COUNT; c++) {
      if (unchecked(obs.catRec[c]) < 0) continue
      if (cat < 0 || unchecked(obs.catRec[c]) < unchecked(obs.catRec[cat])) cat = c
    }

    let t: Type
    if (cat == C_BOOL) {
      t = new Type(FT_INT, SK_BOOL)
    } else if (cat == C_NUM) {
      if (obs.sawFloat) {
        if (obs.sawUint) {
          this.diag.warn(
            this.pathString() +
              ' mixes integers above 2^63 with floats; the column is float64 and those integers lose precision'
          )
        }
        t = new Type(FT_FLOAT, SK_FLOAT64)
      } else if (obs.sawUint) {
        if (obs.sawNeg) {
          this.diag.fail(D_CONFLICT, unchecked(obs.catOff[C_NUM]),
            '[' + unchecked(obs.catRec[C_NUM]).toString() + ']' + this.pathString(),
            'values span both below zero and above the int64 maximum, so no single integer column holds them exactly')
          return null
        }
        t = new Type(FT_INT, SK_UINT64)
      } else {
        t = new Type(FT_INT, SK_INT64)
      }
    } else if (cat == C_STR) {
      t = new Type(FT_STRING, SK_INT64)
    } else if (cat == C_ARR) {
      const elemObs = obs.elem
      let elem: Type | null
      if (elemObs == null) {
        this.diag.warn(this.pathString() + ' was always an empty array; the element type is taken as string')
        elem = new Type(FT_STRING, SK_INT64)
      } else {
        elem = this.resolve(elemObs)
        if (elem == null) return null
      }
      t = new Type(FT_ARRAY, SK_INT64)
      t.elem = elem
    } else {
      t = new Type(FT_STRUCT, SK_INT64)
      for (let i = 0; i < obs.fields.length; i++) {
        const fo = unchecked(obs.fields[i])
        this.path.push('.' + bytesToString(fo.name))
        const ft = this.resolve(fo.obs)
        this.path.pop()
        if (ft == null) return null
        // A key absent from some record is indistinguishable from an explicit
        // null on the wire (PLAN.md §6), and both make the column nullable.
        if (fo.present < obs.objects) ft.nullable = true
        const f = new Field(fo.name, ft)
        f.present = fo.present
        t.fields.push(f)
      }
      if (!assignFieldIDs(t.fields)) {
        this.diag.fail(D_CONFLICT, -1, this.pathString(), 'two fields resolved to the same wire id')
        return null
      }
    }

    if (obs.sawNull) t.nullable = true
    return t
  }

  reportConflict(obs: Obs): void {
    // The incumbent is whatever appeared first; the offender is the newcomer.
    let first = -1
    let last = -1
    for (let c = 0; c < C_COUNT; c++) {
      if (unchecked(obs.catRec[c]) < 0) continue
      if (first < 0 || unchecked(obs.catRec[c]) < unchecked(obs.catRec[first])) first = c
      if (last < 0 || unchecked(obs.catRec[c]) > unchecked(obs.catRec[last])) last = c
    }
    const firstRec = unchecked(obs.catRec[first])
    const lastRec = unchecked(obs.catRec[last])
    let where = 'record ' + firstRec.toString()
    if (lastRec - firstRec > 1) where = 'records ' + firstRec.toString() + '-' + (lastRec - 1).toString()

    // pathString() is the pass-two path, which starts at the record's fields;
    // the record index comes from the observation that caused the conflict.
    this.diag.fail(D_CONFLICT, unchecked(obs.catOff[last]),
      '[' + lastRec.toString() + ']' + this.pathString(),
      'type conflict: ' + unchecked(CATEGORY_NAMES[last]) + ', but ' + where + ' had ' +
        unchecked(CATEGORY_NAMES[first]))
  }
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

function bytesToString(b: Uint8Array): string {
  return String.UTF8.decodeUnsafe(b.dataStart, <usize>b.length, false)
}

export function inferSchema(doc: Doc, diag: Diag): Schema | null {
  return new Inferrer(doc, diag).infer()
}
