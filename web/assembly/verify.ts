// The encode self-check (PLAN.md §4.5).
//
// The structural rules in infer.ts and build.ts are an argument that a message
// says what its input said. This is the proof: the encoder decodes what it just
// wrote and walks it against the parsed input, so the caller hears about a
// disagreement as an error rather than as data.
//
// It is on by default and costs roughly one decode. §4.3 established why that is
// worth paying, and the corruption sweep in tests/fuzz.test.mjs puts a number on
// it: five sixths of all single-byte corruptions of a colbin message decode to
// well-formed, wrong JSON. A decoder cannot tell. An encoder can, because it
// still has the input.
//
// # It compares values, not bytes
//
// "It decoded without error" is far too weak a check, for exactly that reason.
// And comparing the two *texts* would be too strong: the decoder writes every
// field of the schema, in the order the message carried them, where the input
// wrote only what it had in the order the author typed it.
//
// Three differences are expected rather than failures, and every one of them is
// a documented property of a dense columnar layout (PLAN.md §6):
//
//   - a key absent from a record comes back as its zero;
//   - an empty array comes back as null;
//   - an integer in a column one float promoted comes back as that float.
//
// Anything else is a bug in the encoder, and the point of this file is that it
// is caught here rather than by whoever reads the message next.

import { D_CORRUPT, Diag } from './diag'
import {
  Doc,
  K_ARRAY,
  K_BOOL,
  K_FLOAT,
  K_INT,
  K_NULL,
  K_OBJECT,
  K_STRING,
  K_UINT,
  parseJSON,
} from './json'

/**
 * Compares the JSON the decoder produced against the document the encoder was
 * given. Returns true when they agree.
 */
export function verify(input: Doc, inputRoot: i32, decoded: Uint8Array, diag: Diag): bool {
  const quiet = new Diag()
  const back = parseJSON(decoded, quiet)
  if (back == null) {
    diag.fail(
      D_CORRUPT,
      -1,
      '',
      'the encoder wrote a message whose decoding is not valid JSON: ' + quiet.message,
    )
    return false
  }
  const checker = new Checker(input, back, diag)
  checker.compare(inputRoot, back.root)
  return diag.ok
}

class Checker {
  input: Doc
  back: Doc
  diag: Diag
  path: Array<string> = []

  constructor(input: Doc, back: Doc, diag: Diag) {
    this.input = input
    this.back = back
    this.diag = diag
  }

  private pathString(): string {
    let out = ''
    for (let index = 0; index < this.path.length; index++) out += unchecked(this.path[index])
    return out.length == 0 ? '(root)' : out
  }

  private differs(what: string): void {
    this.diag.fail(
      D_CORRUPT,
      -1,
      this.pathString(),
      'the encoder wrote a message that does not read back as its input: ' + what,
    )
  }

  /**
   * One value against another. `mine` may be -1, which is a key the input did
   * not carry — the decoder will still have written it, and its zero is what it
   * must have written.
   */
  compare(mine: i32, theirs: i32): void {
    if (!this.diag.ok) return
    const back = this.back
    if (mine < 0) {
      // An absent key comes back as its zero. That is the encoding of a zero
      // value, not a lost field.
      if (!this.isZeroish(theirs)) {
        this.differs('a key the input did not carry came back as something other than a zero')
      }
      return
    }

    const input = this.input
    const kind = input.kindOf(mine)

    if (kind == K_NULL) {
      // A null is omitted, so it comes back as null where the format has a
      // pointer for it and as the zero where it does not.
      if (!this.isZeroish(theirs)) this.differs('a null came back as a value')
      return
    }
    if (kind == K_BOOL) {
      if (back.kindOf(theirs) != K_BOOL ||
          (unchecked(back.num[theirs]) != 0) != (unchecked(input.num[mine]) != 0)) {
        this.differs('a boolean came back differently')
      }
      return
    }
    if (kind == K_INT || kind == K_UINT || kind == K_FLOAT) {
      this.compareNumber(mine, theirs)
      return
    }
    if (kind == K_STRING) {
      if (back.kindOf(theirs) != K_STRING || !sameBytes(input.strOf(mine), back.strOf(theirs))) {
        this.differs('a string came back differently')
      }
      return
    }
    if (kind == K_ARRAY) {
      this.compareArray(mine, theirs)
      return
    }
    this.compareObject(mine, theirs)
  }

  private compareNumber(mine: i32, theirs: i32): void {
    const input = this.input
    const back = this.back
    const theirKind = back.kindOf(theirs)
    if (theirKind != K_INT && theirKind != K_UINT && theirKind != K_FLOAT) {
      this.differs('a number came back as something else')
      return
    }
    const myKind = input.kindOf(mine)
    if (myKind == K_FLOAT || theirKind == K_FLOAT) {
      // One float anywhere in a column promotes the whole column, so an integer
      // legitimately comes back as that float. Comparing as f64 is what makes
      // that a tolerance rather than a hole: a value that does not survive the
      // promotion still differs.
      if (numberAsFloat(input, mine) != numberAsFloat(back, theirs)) {
        this.differs('a number came back with a different value')
      }
      return
    }
    // Both integers: compare the bits *and* the signedness, so an int64 and a
    // uint64 holding the same bit pattern are not mistaken for each other.
    if (unchecked(input.num[mine]) != unchecked(back.num[theirs]) || myKind != theirKind) {
      this.differs('an integer came back with a different value')
    }
  }

  private compareArray(mine: i32, theirs: i32): void {
    const input = this.input
    const back = this.back
    const count = input.count(mine)
    if (back.kindOf(theirs) == K_NULL) {
      // An empty array and a nil one are the same on the wire.
      if (count != 0) this.differs('an array came back as null')
      return
    }
    if (back.kindOf(theirs) != K_ARRAY) {
      this.differs('an array came back as something else')
      return
    }
    if (back.count(theirs) != count) {
      this.differs(
        'an array of ' + count.toString() + ' came back with ' + back.count(theirs).toString(),
      )
      return
    }
    for (let index = 0; index < count && this.diag.ok; index++) {
      this.path.push('[' + index.toString() + ']')
      this.compare(input.childAt(mine, index), back.childAt(theirs, index))
      this.path.pop()
    }
  }

  private compareObject(mine: i32, theirs: i32): void {
    const input = this.input
    const back = this.back
    if (back.kindOf(theirs) != K_OBJECT) {
      this.differs('an object came back as something else')
      return
    }
    // Walk the *decoder's* keys, because it writes every field of the schema
    // and the input may have written only some. A key the input did not carry
    // is compared against -1, which is the absent case above.
    const count = back.count(theirs)
    for (let index = 0; index < count && this.diag.ok; index++) {
      const key = back.keyOf(theirs, index)
      this.path.push('.' + String.UTF8.decodeUnsafe(key.dataStart, <usize>key.length, false))
      this.compare(findKey(input, mine, key), back.childAt(theirs, index))
      this.path.pop()
    }
    // And the other way: a key the input carried that the decoder did not write
    // is a field the schema lost, which is the failure this whole file exists
    // to catch.
    const written = input.count(mine)
    for (let index = 0; index < written && this.diag.ok; index++) {
      const key = input.keyOf(mine, index)
      if (findKey(back, theirs, key) < 0) {
        this.path.push('.' + String.UTF8.decodeUnsafe(key.dataStart, <usize>key.length, false))
        this.differs('the input carried a key the message does not')
        this.path.pop()
      }
    }
  }

  /** Whether a decoded value is the zero an omitted field comes back as. */
  private isZeroish(node: i32): bool {
    const back = this.back
    const kind = back.kindOf(node)
    if (kind == K_NULL) return true
    if (kind == K_BOOL) return unchecked(back.num[node]) == 0
    if (kind == K_INT || kind == K_UINT) return unchecked(back.num[node]) == 0
    if (kind == K_FLOAT) return back.floatOf(node) == 0
    if (kind == K_STRING) return back.strOf(node).length == 0
    if (kind == K_ARRAY) return back.count(node) == 0
    if (kind == K_OBJECT) {
      // A nested struct is always written, so an omitted one comes back as an
      // object whose every field is itself a zero.
      const count = back.count(node)
      for (let index = 0; index < count; index++) {
        if (!this.isZeroish(back.childAt(node, index))) return false
      }
      return true
    }
    return false
  }
}

/** The last child under `key`, or -1. Last rather than first, because a
 * duplicate key overwrites — which is what JSON.parse does and what build.ts
 * writes. */
function findKey(doc: Doc, node: i32, key: Uint8Array): i32 {
  if (node < 0 || doc.kindOf(node) != K_OBJECT) return -1
  const count = doc.count(node)
  let found = -1
  for (let index = 0; index < count; index++) {
    if (sameBytes(doc.keyOf(node, index), key)) found = doc.childAt(node, index)
  }
  return found
}

function numberAsFloat(doc: Doc, node: i32): f64 {
  const kind = doc.kindOf(node)
  if (kind == K_FLOAT) return doc.floatOf(node)
  if (kind == K_UINT) return <f64><u64>unchecked(doc.num[node])
  return <f64>unchecked(doc.num[node])
}

function sameBytes(a: Uint8Array, b: Uint8Array): bool {
  if (a.length != b.length) return false
  if (a.length == 0) return true
  return memory.compare(a.dataStart, b.dataStart, <usize>a.length) == 0
}
