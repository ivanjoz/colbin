// The encode self-check (PLAN.md §4.5).
//
// The structural rules in infer.ts and encode.ts are an argument that a message
// says what its input said. This is the proof: the encoder decodes what it just
// wrote and walks it against the parsed input. Cost is roughly one decode, which
// is why it can be turned off — and why it is on by default, since §4.3
// established that a well-formed-but-wrong message is the failure that does not
// announce itself.
//
// It compares values, not bytes. "It decoded without error" is far too weak a
// check: over half of the corrupt messages in §4.3 decoded without error.
//
// Three differences are expected rather than failures, and every one of them is
// a documented property of a dense columnar layout (§6):
//
//   - a key absent from a record comes back as null
//   - an empty array comes back as null
//   - an integer in a column one float promoted comes back as that float
//
// Anything else is a bug in the encoder, and the point of this file is that the
// caller hears about it as an error rather than as data.

import { Diag, D_CORRUPT } from './diag'
import { Decoded, V_ARRAY, V_BOOL, V_FLOAT, V_INT, V_NULL, V_OBJECT, V_STRING, V_UINT, Val } from './decode'
import { Doc, K_ARRAY, K_BOOL, K_FLOAT, K_INT, K_NULL, K_OBJECT, K_STRING, K_UINT } from './json'

export class Verifier {
  doc: Doc
  diag: Diag

  constructor(doc: Doc, diag: Diag) {
    this.doc = doc
    this.diag = diag
  }

  fail(path: string, message: string): bool {
    this.diag.fail(D_CORRUPT, -1, path,
      'the encoder produced a message that does not read back as its input (' + message +
        '). This is a bug in colbin, not in your JSON.')
    return false
  }

  /** Every record, against the values the message gave back. */
  check(records: Array<i32>, decoded: Decoded): bool {
    if (decoded.rows.length != records.length) {
      return this.fail('', 'record count ' + decoded.rows.length.toString() + ' instead of ' +
        records.length.toString())
    }
    for (let i = 0; i < records.length; i++) {
      if (!this.value(unchecked(records[i]), unchecked(decoded.rows[i]), '[' + i.toString() + ']')) {
        return false
      }
    }
    return true
  }

  value(node: i32, val: Val, path: string): bool {
    const doc = this.doc
    const kind = doc.kindOf(node)

    if (kind == K_NULL) {
      return val.tag == V_NULL ? true : this.fail(path, 'null came back as something else')
    }

    if (kind == K_BOOL) {
      if (val.tag != V_BOOL) return this.fail(path, 'a bool came back as another type')
      const want = unchecked(doc.num[node])
      return val.num == want ? true : this.fail(path, 'a bool changed value')
    }

    if (kind == K_INT || kind == K_UINT) {
      const raw = unchecked(doc.num[node])
      if (val.tag == V_INT || val.tag == V_UINT) {
        // Both carry the 64-bit pattern, so signedness does not enter the test.
        return val.num == raw ? true : this.fail(path, 'an integer changed value')
      }
      if (val.tag == V_FLOAT) {
        // One float promoted the column, so this integer is stored as a float.
        const want: f64 = kind == K_UINT ? <f64>(<u64>raw) : <f64>raw
        const got = reinterpret<f64>(val.num)
        return got == want
          ? true
          : this.fail(path, 'an integer in a float column changed value')
      }
      return this.fail(path, 'an integer came back as another type')
    }

    if (kind == K_FLOAT) {
      if (val.tag != V_FLOAT) return this.fail(path, 'a float came back as another type')
      return val.num == unchecked(doc.num[node])
        ? true
        : this.fail(path, 'a float changed value')
    }

    if (kind == K_STRING) {
      if (val.tag != V_STRING) return this.fail(path, 'a string came back as another type')
      const want = doc.strOf(node)
      const got = val.bytes!
      if (want.length != got.length) return this.fail(path, 'a string changed length')
      if (want.length == 0) return true
      return memory.compare(want.dataStart, got.dataStart, <usize>want.length) == 0
        ? true
        : this.fail(path, 'a string changed content')
    }

    if (kind == K_ARRAY) {
      const count = doc.count(node)
      // An empty array and null are the same on the wire, so an empty array
      // coming back as null is the format working, not the encoder failing.
      if (count == 0) {
        return val.tag == V_NULL || (val.tag == V_ARRAY && val.items!.length == 0)
          ? true
          : this.fail(path, 'an empty array came back as something else')
      }
      if (val.tag != V_ARRAY) return this.fail(path, 'an array came back as another type')
      const items = val.items!
      if (items.length != count) return this.fail(path, 'an array changed length')
      for (let i = 0; i < count; i++) {
        if (!this.value(doc.childAt(node, i), unchecked(items[i]), path + '[' + i.toString() + ']')) {
          return false
        }
      }
      return true
    }

    // K_OBJECT
    if (val.tag != V_OBJECT) return this.fail(path, 'an object came back as another type')
    const keys = val.keys!
    const items = val.items!
    for (let i = 0; i < keys.length; i++) {
      const key = unchecked(keys[i])
      const child = this.lookup(node, key)
      const sub = path + '.' + String.UTF8.decodeUnsafe(key.dataStart, <usize>key.length, false)
      if (child < 0) {
        // The key was absent from this record, which is indistinguishable from
        // an explicit null once the column is dense.
        if (unchecked(items[i]).tag != V_NULL) {
          return this.fail(sub, 'a key this record did not have came back with a value')
        }
        continue
      }
      if (!this.value(child, unchecked(items[i]), sub)) return false
    }

    // Every key the input had must be one the message carries back.
    const inputKeys = doc.count(node)
    for (let i = 0; i < inputKeys; i++) {
      const key = doc.keyOf(node, i)
      let found = false
      for (let k = 0; k < keys.length; k++) {
        const other = unchecked(keys[k])
        if (other.length == key.length &&
            memory.compare(other.dataStart, key.dataStart, <usize>key.length) == 0) {
          found = true
          break
        }
      }
      if (!found) {
        return this.fail(path + '.' + String.UTF8.decodeUnsafe(key.dataStart, <usize>key.length, false),
          'a key of the input is missing from the message')
      }
    }
    return true
  }

  /** The value of a key in an object node, or -1. The scanner has already
   *  resolved duplicates, so at most one entry can match. */
  lookup(node: i32, key: Uint8Array): i32 {
    const doc = this.doc
    const count = doc.count(node)
    for (let i = 0; i < count; i++) {
      const candidate = doc.keyOf(node, i)
      if (candidate.length == key.length &&
          memory.compare(candidate.dataStart, key.dataStart, <usize>key.length) == 0) {
        return doc.childAt(node, i)
      }
    }
    return -1
  }
}
