// The encode fast path: live JavaScript objects to the arrays of a parsed
// document, and the schema section inferred for them — in one walk, with no JSON
// text built or scanned.
//
// # Why it exists
//
// `marshal` used to be `JSON.stringify`, a UTF-8 encode, a copy into the module,
// a scan of the text and an inference pass over what the scan found. Measured
// natively on 500 products the stages are: scan 315 µs, inference 386 µs and the
// body — the part that actually compresses — 102 µs. The JavaScript side did
// the same work a second time, in the form of `JSON.stringify`. So the fastest
// encode is the one that reads the objects once, infers while it does, and hands
// the module the arrays it would have built and the section it would have
// inferred. The module then writes the body from them (`encode_doc`, which is the
// text path's own builder) and nothing else.
//
// # The text path is the oracle
//
// `rust/ENCODER.md` §5 is why: a message that decodes to something other than
// what went in is the failure that does not announce itself. So this file does
// **not** decide anything it is unsure of. Every shape the text path would warn
// about, refuse or round differently makes `buildBlob` return `null`, and the
// caller encodes `JSON.stringify(value)` instead — which produces the canonical
// diagnostic, warning or number. What is left for the fast path is the ordinary
// document, and for that the two must write the same bytes:
// `js/tests/fast.test.mjs` holds them to it over the corpus and a generated
// sweep.
//
// What falls back, and why:
//
//   - an integer past 2^53 — `JSON.stringify` prints its shortest digits, not
//     its value, and the text path reads those; this would have to guess them;
//   - anything `JSON.stringify` would call code for: a `toJSON`, a getter-less
//     class instance, a Map, a Date, a typed array, a bigint;
//   - a type conflict, a field only ever null, an always-empty array, a null where
//     an object belongs, an array of floats, of bools or of arrays, an empty
//     object, a scalar root: the text path's warnings and errors;
//   - more than 256 fields in an object, nesting past 60, a document past 64 MiB.
//
// # The rules are infer.rs's
//
// Ids sequential in first-seen order, integers never narrowed, one fractional
// value makes the column float64, null or a key absent from some record makes a
// scalar column a pointer. `rust/src/infer.rs` is the statement of them and this
// is a port of its two passes: `walk` is the observation, `resolve*` the
// resolution. The one place it does not copy is the count of records carrying a
// key, which `infer.rs` once got wrong (it followed the latest record) and no
// longer does.

/** `Doc` node kinds, `rust/src/json/parse.rs`. */
const K_NULL = 0
const K_BOOL = 1
const K_INT = 2
const K_FLOAT = 4
const K_STRING = 5
const K_ARRAY = 6
const K_OBJECT = 7

/** Field ops, `rust/src/plan.rs`: the ones an inferred schema reaches. */
const OP_BOOL = 0
const OP_INT64 = 4
const OP_FLOAT64 = 10
const OP_STRING = 11
const OP_INT64S = 16
const OP_STRINGS = 20
const OP_STRUCT = 21
const OP_STRUCTS = 22
const OP_POINTER = 24

/** Value categories, as a bit set: more than one bit is a type conflict. */
const C_BOOL = 1
const C_NUM = 2
const C_STR = 4
const C_ARR = 8
const C_OBJ = 16

const MAX_FIELDS = 256
/** What four key bits hold: past it a struct's run is eight-bit. */
const NARROW_FIELDS = 16
/** Below `build.rs`'s own bound of 64, so what this accepts it can write. */
const MAX_DEPTH = 60
const MAX_BLOB = 64 << 20

const SCHEMA_WIDE_KEYS = 1
const SCHEMA_ENVELOPE = 2
const LENGTH_ESCAPE = 0xff

/** The arrays are written through two views of one buffer, which is only the
 * document the module expects on a little-endian host. Every host a browser or
 * Node runs on is; this makes the one that is not fall back rather than guess. */
const LITTLE_ENDIAN = new Uint8Array(new Uint32Array([1]).buffer)[0] === 1

const encoder = new TextEncoder()

/** What one position in the document was observed to hold, across every record. */
class Obs {
  cats = 0
  sawNull = false
  sawFloat = false
  /** The observation of this array's elements, once there has been one. */
  elem: Obs | null = null
  /** Fields in first-seen order. */
  fields: FieldObs[] = []
  by: Map<string, FieldObs> | null = null
  /** Objects merged into this position. A field in fewer of them is nullable. */
  objects = 0
}

class FieldObs {
  name: string
  obs: Obs = new Obs()
  present = 0
  /** The ordinal of the last object that carried it. */
  last = 0
  /** Where the name's bytes are in the text arena. */
  keyAt: number
  keyLen: number
  constructor(name: string, keyAt: number, keyLen: number) {
    this.name = name
    this.keyAt = keyAt
    this.keyLen = keyLen
  }
}

type PlanField = { op: number; sub: number; elemOp: number }
type Plan = { fields: PlanField[]; names: FieldObs[] | null; wide: boolean; envelope: boolean }

// ---- the arena, reused across calls --------------------------------------
//
// Growable and never shrunk: a handle that encodes one document after another
// pays for its arrays once. A call is synchronous and a getter could re-enter
// it, which `busy` turns into a fallback rather than two walks writing one set
// of arrays.

let nodeCap = 4096
let kind = new Uint8Array(nodeCap)
let numBuf = new ArrayBuffer(nodeCap * 8)
let numF64 = new Float64Array(numBuf)
let numU32 = new Uint32Array(numBuf)
let nodeA = new Uint32Array(nodeCap)
let nodeB = new Uint32Array(nodeCap)

let childCap = 4096
let child = new Uint32Array(childCap)
let keyA = new Uint32Array(childCap)
let keyB = new Uint32Array(childCap)

let textCap = 1 << 16
let text = new Uint8Array(textCap)

/** The children of the containers being walked, three words each — node, key
 * start, key length — so a container's children land contiguous in `child`
 * however deep its own children nest. */
let stackCap = 3 * 1024
let stack = new Int32Array(stackCap)

let nodes = 0
let children = 0
let textLen = 0
let sp = 0
let bail = false
let busy = false

function growNodes(): void {
  const cap = nodeCap * 2
  const grow8 = (old: Uint8Array) => {
    const next = new Uint8Array(cap)
    next.set(old)
    return next
  }
  const grow32 = (old: Uint32Array) => {
    const next = new Uint32Array(cap)
    next.set(old)
    return next
  }
  kind = grow8(kind)
  const nextNum = new ArrayBuffer(cap * 8)
  new Uint8Array(nextNum).set(new Uint8Array(numBuf))
  numBuf = nextNum
  numF64 = new Float64Array(numBuf)
  numU32 = new Uint32Array(numBuf)
  nodeA = grow32(nodeA)
  nodeB = grow32(nodeB)
  nodeCap = cap
}

function growChildren(need: number): void {
  let cap = childCap
  while (cap < children + need) cap *= 2
  const grow = (old: Uint32Array) => {
    const next = new Uint32Array(cap)
    next.set(old)
    return next
  }
  child = grow(child)
  keyA = grow(keyA)
  keyB = grow(keyB)
  childCap = cap
}

function growText(need: number): void {
  let cap = textCap
  while (cap < textLen + need) cap *= 2
  const next = new Uint8Array(cap)
  next.set(text.subarray(0, textLen))
  text = next
  textCap = cap
}

function growStack(): void {
  const next = new Int32Array(stackCap * 2)
  next.set(stack)
  stack = next
  stackCap *= 2
}

function newNode(k: number): number {
  const at = nodes
  if (at >= nodeCap) growNodes()
  nodes = at + 1
  kind[at] = k
  return at
}

/** Writes a string's UTF-8 into the arena, and returns its length. A lone
 * surrogate becomes U+FFFD, which is what the text path's scan makes of the
 * `\uD800` `JSON.stringify` writes for it. */
function putText(s: string): number {
  const length = s.length
  if (textLen + length * 3 > textCap) growText(length * 3)
  const start = textLen
  let at = start
  let i = 0
  for (; i < length; i++) {
    const c = s.charCodeAt(i)
    if (c >= 0x80) break
    text[at++] = c
  }
  if (i < length) {
    at = start + encoder.encodeInto(s, text.subarray(start)).written
  }
  textLen = at
  return at - start
}

const NOT_JSON = new Set(['undefined', 'function', 'symbol'])

/**
 * One value into the arrays, observing it into `obs` on the way. Returns the
 * node. A shape the fast path will not decide sets `bail` and the caller unwinds.
 */
function walk(value: unknown, obs: Obs, depth: number): number {
  switch (typeof value) {
    case 'number': {
      const at = newNode(K_INT)
      if ((value | 0) === value) {
        // An int32 — the common case, with no division.
        obs.cats |= C_NUM
        numU32[2 * at] = value
        numU32[2 * at + 1] = value < 0 ? -1 : 0
      } else if (value !== value || value === Infinity || value === -Infinity) {
        // `JSON.stringify` writes null for these.
        kind[at] = K_NULL
        obs.sawNull = true
      } else if (Number.isSafeInteger(value)) {
        obs.cats |= C_NUM
        numU32[2 * at] = value
        numU32[2 * at + 1] = Math.floor(value / 4294967296)
      } else if (Number.isInteger(value)) {
        // Its text is the shortest digits that round-trip, not its value.
        bail = true
      } else {
        obs.cats |= C_NUM
        kind[at] = K_FLOAT
        numF64[at] = value
        obs.sawFloat = true
      }
      return at
    }
    case 'string': {
      const at = newNode(K_STRING)
      obs.cats |= C_STR
      nodeA[at] = textLen
      nodeB[at] = putText(value)
      return at
    }
    case 'boolean': {
      const at = newNode(K_BOOL)
      obs.cats |= C_BOOL
      numU32[2 * at] = value ? 1 : 0
      numU32[2 * at + 1] = 0
      return at
    }
    case 'object':
      if (value === null) {
        const at = newNode(K_NULL)
        obs.sawNull = true
        return at
      }
      if (Array.isArray(value)) return walkArray(value, obs, depth)
      return walkObject(value, obs, depth)
    default:
      // undefined, a function or a symbol reach here only inside an array, where
      // `JSON.stringify` writes null; a bigint it throws on, and the text path
      // is the one that says so.
      if (NOT_JSON.has(typeof value)) {
        const at = newNode(K_NULL)
        obs.sawNull = true
        return at
      }
      bail = true
      return 0
  }
}

function walkArray(value: unknown[], obs: Obs, depth: number): number {
  if (depth >= MAX_DEPTH || Object.getPrototypeOf(value) !== Array.prototype) {
    bail = true
    return 0
  }
  const at = newNode(K_ARRAY)
  obs.cats |= C_ARR
  const count = value.length
  let elem = obs.elem
  if (count > 0 && elem === null) elem = obs.elem = new Obs()
  const base = sp
  for (let i = 0; i < count; i++) {
    if (sp + 3 > stackCap) growStack()
    stack[sp] = walk(value[i], elem as Obs, depth + 1)
    sp += 3
    if (bail) return at
  }
  if (children + count > childCap) growChildren(count)
  const first = children
  for (let i = 0; i < count; i++) child[children++] = stack[base + 3 * i]
  sp = base
  nodeA[at] = first
  nodeB[at] = count
  return at
}

function walkObject(value: object, obs: Obs, depth: number): number {
  if (depth >= MAX_DEPTH) {
    bail = true
    return 0
  }
  const proto = Object.getPrototypeOf(value)
  if (proto !== Object.prototype && proto !== null) {
    // A class instance, a Date, a Map: `JSON.stringify` may call code, or render
    // something this does not.
    bail = true
    return 0
  }
  const source = value as Record<string, unknown>
  // `JSON.stringify` calls it and renders what it returns.
  if (source.toJSON !== undefined) {
    bail = true
    return 0
  }
  const at = newNode(K_OBJECT)
  obs.cats |= C_OBJ
  const objects = ++obs.objects
  const fields = obs.fields
  const base = sp
  let count = 0
  for (const key in source) {
    const v = source[key]
    const t = typeof v
    // `JSON.stringify` leaves these keys out, so they are not fields.
    if (t === 'undefined' || t === 'function' || t === 'symbol') continue

    // The key the previous object had at this position, which is almost always
    // this one: a pointer compare for interned names rather than a hash.
    let field: FieldObs | undefined = count < fields.length ? fields[count] : undefined
    if (field === undefined || field.name !== key) {
      field = obs.by?.get(key)
      if (field === undefined) {
        if (fields.length >= MAX_FIELDS) {
          bail = true
          return at
        }
        const keyAt = textLen
        const keyLen = putText(key)
        field = new FieldObs(key, keyAt, keyLen)
        fields.push(field)
        ;(obs.by ??= new Map()).set(key, field)
      }
    }
    if (field.last !== objects) {
      field.last = objects
      field.present++
    }
    if (sp + 3 > stackCap) growStack()
    stack[sp] = walk(v, field.obs, depth + 1)
    stack[sp + 1] = field.keyAt
    stack[sp + 2] = field.keyLen
    sp += 3
    if (bail) return at
    count++
  }
  if (children + count > childCap) growChildren(count)
  const first = children
  for (let i = 0; i < count; i++) {
    const from = base + 3 * i
    child[children] = stack[from]
    keyA[children] = stack[from + 1]
    keyB[children] = stack[from + 2]
    children++
  }
  sp = base
  nodeA[at] = first
  nodeB[at] = count
  return at
}

// ---- resolution ------------------------------------------------------------

function single(cats: number): boolean {
  return cats === C_BOOL || cats === C_NUM || cats === C_STR || cats === C_ARR || cats === C_OBJ
}

/** `columnable_op`: the ops a pointer can point at. */
function pointable(op: number): boolean {
  return op <= OP_FLOAT64 || op === OP_STRING
}

class Resolver {
  plans: Plan[] = []
  failed = false

  fail(): number {
    this.failed = true
    return -1
  }

  /** One observation into an op, filling `field` with what the op does not say. */
  op(obs: Obs, field: PlanField): number {
    if (!single(obs.cats)) return this.fail() // a conflict, or only ever null
    if (obs.cats === C_OBJ) {
      const sub = this.struct(obs)
      if (sub < 0) return this.fail()
      field.sub = sub
      // A null where an object belongs is a warning the text path words.
      if (obs.sawNull) return this.fail()
      return OP_STRUCT
    }
    if (obs.cats === C_ARR) return this.array(obs, field)
    const op = obs.cats === C_BOOL ? OP_BOOL : obs.cats === C_NUM ? (obs.sawFloat ? OP_FLOAT64 : OP_INT64) : OP_STRING
    if (obs.sawNull) {
      field.elemOp = op
      return OP_POINTER
    }
    return op
  }

  array(obs: Obs, field: PlanField): number {
    const elem = obs.elem
    // Always empty: the text path warns that the element type is a guess.
    if (elem === null || !single(elem.cats)) return this.fail()
    switch (elem.cats) {
      case C_STR:
        return OP_STRINGS
      case C_OBJ: {
        const sub = this.struct(elem)
        if (sub < 0) return this.fail()
        field.sub = sub
        return OP_STRUCTS
      }
      case C_NUM:
        // Floats have no array op.
        return elem.sawFloat ? this.fail() : OP_INT64S
      default:
        return this.fail()
    }
  }

  /** A struct into the table, in the pre-order a reader's own hoisting assigns. */
  struct(obs: Obs): number {
    const me = this.plans.length
    this.plans.push({ fields: [], names: null, wide: false, envelope: false })
    const observed = obs.fields
    if (observed.length === 0) return this.fail()
    const fields: PlanField[] = []
    for (let i = 0; i < observed.length; i++) {
      const o = observed[i]
      const field: PlanField = { op: 0, sub: 0, elemOp: 0 }
      const op = this.op(o.obs, field)
      if (op < 0) return -1
      field.op = op
      // A key absent from some record reads back as null, so it is a pointer too.
      if (o.present < obs.objects && op !== OP_POINTER && pointable(op)) {
        field.elemOp = op
        field.op = OP_POINTER
      }
      fields.push(field)
    }
    this.plans[me] = { fields, names: observed, wide: fields.length > NARROW_FIELDS, envelope: false }
    return me
  }
}

// ---- the section ---------------------------------------------------------------

function appendLength(out: number[], value: number): void {
  if (value < LENGTH_ESCAPE) {
    out.push(value)
    return
  }
  out.push(LENGTH_ESCAPE, value & 0xff, (value >>> 8) & 0xff, (value >>> 16) & 0xff, (value >>> 24) & 0xff)
}

const ENVELOPE_NAME = encoder.encode('rows')

/** `section::build`: the same bytes, for the same plans. */
function buildSection(plans: Plan[]): Uint8Array {
  const body: number[] = []
  appendLength(body, plans.length)
  for (const plan of plans) {
    body.push((plan.wide ? SCHEMA_WIDE_KEYS : 0) | (plan.envelope ? SCHEMA_ENVELOPE : 0))
    appendLength(body, plan.fields.length)
    for (let key = 0; key < plan.fields.length; key++) {
      const field = plan.fields[key]
      body.push(key)
      if (plan.names === null) {
        appendLength(body, ENVELOPE_NAME.length)
        for (const byte of ENVELOPE_NAME) body.push(byte)
      } else {
        const name = plan.names[key]
        appendLength(body, name.keyLen)
        for (let i = 0; i < name.keyLen; i++) body.push(text[name.keyAt + i])
      }
      body.push(field.op)
      if (field.op === OP_STRUCT || field.op === OP_STRUCTS) appendLength(body, field.sub)
      else if (field.op === OP_POINTER) body.push(field.elemOp)
    }
  }
  const out: number[] = []
  appendLength(out, body.length)
  return Uint8Array.from(out.concat(body))
}

// ---- the blob ----------------------------------------------------------------------

/** What `colbin::build::encode_blob` reads. */
export type Blob = {
  /** The bytes the module is handed. */
  readonly length: number
  /** Writes them at `at`, in the module's memory. */
  copyTo(memory: Uint8Array, at: number): void
}

const HEADER = 32

function pack(section: Uint8Array): Blob {
  const n = nodes
  const m = children
  const length = HEADER + n * (1 + 8 + 4 + 4) + m * 12 + textLen + section.length
  const header = new Uint32Array([n, m, textLen, section.length, 0, 0, 0, 0])
  return {
    length,
    copyTo(memory, at) {
      const put = (bytes: Uint8Array) => {
        memory.set(bytes, at)
        at += bytes.length
      }
      put(new Uint8Array(header.buffer))
      put(kind.subarray(0, n))
      put(new Uint8Array(numBuf, 0, n * 8))
      put(new Uint8Array(nodeA.buffer, 0, n * 4))
      put(new Uint8Array(nodeB.buffer, 0, n * 4))
      put(new Uint8Array(child.buffer, 0, m * 4))
      put(new Uint8Array(keyA.buffer, 0, m * 4))
      put(new Uint8Array(keyB.buffer, 0, m * 4))
      put(text.subarray(0, textLen))
      put(section)
    },
  }
}

/**
 * Walks `value` once and returns the document and its section as a blob for the
 * module, or `null` when the text path should encode it instead. The blob is
 * valid until the next call, and `copyTo` must be called before then.
 */
export function buildBlob(value: unknown): Blob | null {
  if (busy || !LITTLE_ENDIAN || typeof value !== 'object' || value === null) return null
  busy = true
  nodes = children = textLen = sp = 0
  bail = false
  try {
    const root = new Obs()
    walk(value, root, 0)
    if (bail || nodes === 0) return null

    const resolver = new Resolver()
    if (root.cats === C_OBJ) {
      if (resolver.struct(root) !== 0) return null
    } else if (root.cats === C_ARR) {
      // The root of a message is a struct, so a top-level array goes in the
      // one-field envelope the text path builds: index 0, reserved first.
      resolver.plans.push({ fields: [], names: null, wide: false, envelope: true })
      const field: PlanField = { op: 0, sub: 0, elemOp: 0 }
      const op = resolver.op(root, field)
      if (op < 0) return null
      field.op = op
      resolver.plans[0] = { fields: [field], names: null, wide: false, envelope: true }
    } else {
      return null
    }
    if (resolver.failed) return null

    const section = buildSection(resolver.plans)
    const blob = pack(section)
    return blob.length > MAX_BLOB ? null : blob
  } catch {
    // A getter that throws, a Proxy: the text path is the one that reports it.
    return null
  } finally {
    busy = false
  }
}
