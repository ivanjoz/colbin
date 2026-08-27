// Test-only entry point. Exposes the internal codecs over two static scratch
// buffers so the Node harness can drive them without the AssemblyScript loader.
// Not part of the public ABI and not built into the site bundle.

import { Reader, Writer } from './bytes'
import { appendArray, decodeArray } from './varint'
import { append as packed5Append, decode as packed5Decode, size as packed5Size } from './packed5'
import { Diag, lineOf } from './diag'
import { Doc, K_FLOAT, K_STRING, parseJSON } from './json'
import { Schema, inferSchema } from './infer'
import { encodeMessage } from './encode'
import { decodeMessage, decodeValues, inspectMessage } from './decode'
import { Verifier } from './verify'
import { Field, Type, assignFieldIDs } from './schema'

const IN = new Uint8Array(1 << 20)
const OUT = new Uint8Array(1 << 20)

export function inPtr(): usize {
  return IN.dataStart
}

export function outPtr(): usize {
  return OUT.dataStart
}

/** n little-endian i64 values at inPtr -> a varint frame at outPtr. */
export function varintEncode(n: i32, width: i32): i32 {
  const vals = new Int64Array(n)
  for (let i = 0; i < n; i++) {
    unchecked((vals[i] = load<i64>(IN.dataStart + <usize>(i * 8))))
  }
  const w = new Writer(64)
  appendArray(w, vals, <u8>width)
  memory.copy(OUT.dataStart, w.buf.dataStart, <usize>w.len)
  return w.len
}

/**
 * A varint frame at inPtr -> n little-endian i64 values at outPtr.
 * Returns the bytes consumed, or -(err + 1) on failure.
 */
export function varintDecode(frameLen: i32, n: i32, width: i32): i32 {
  const frame = new Uint8Array(frameLen)
  memory.copy(frame.dataStart, IN.dataStart, <usize>frameLen)
  const r = new Reader(frame)
  const out = new Int64Array(n)
  if (!decodeArray(r, n, out, <u8>width)) return -(r.err + 1)
  for (let i = 0; i < n; i++) {
    store<i64>(OUT.dataStart + <usize>(i * 8), unchecked(out[i]))
  }
  return r.pos
}

// Debug: the per-candidate totals for the values currently at inPtr, so a
// disagreement with Go can be localised to a transform rather than guessed at.
import { debugTotals } from './varint'

export function varintTotals(n: i32, width: i32, which: i32): i32 {
  const vals = new Int64Array(n)
  for (let i = 0; i < n; i++) {
    unchecked((vals[i] = load<i64>(IN.dataStart + <usize>(i * 8))))
  }
  return debugTotals(vals, <u8>width, which)
}

/** Raw bytes at inPtr -> a packed5 frame at outPtr. Returns the frame length. */
export function packed5Encode(len: i32): i32 {
  const s = new Uint8Array(len)
  memory.copy(s.dataStart, IN.dataStart, <usize>len)
  const w = new Writer(64)
  packed5Append(w, s)
  memory.copy(OUT.dataStart, w.buf.dataStart, <usize>w.len)
  return w.len
}

/** What packed5Encode would produce for the bytes at inPtr, without encoding. */
export function packed5SizeOf(len: i32): i32 {
  const s = new Uint8Array(len)
  memory.copy(s.dataStart, IN.dataStart, <usize>len)
  return packed5Size(s)
}

/**
 * A packed5 frame at inPtr -> the decoded bytes at outPtr.
 * Returns the decoded byte count, or -(err + 1). The frame length consumed is
 * left in outConsumed.
 */
let outConsumed: i32 = 0

export function packed5ConsumedBytes(): i32 {
  return outConsumed
}

export function packed5DecodeFrame(frameLen: i32): i32 {
  const frame = new Uint8Array(frameLen)
  memory.copy(frame.dataStart, IN.dataStart, <usize>frameLen)
  const r = new Reader(frame)
  const out = new Writer(64)
  if (!packed5Decode(r, out)) return -(r.err + 1)
  outConsumed = r.pos
  memory.copy(OUT.dataStart, out.buf.dataStart, <usize>out.len)
  return out.len
}

// ---- JSON scanner ----------------------------------------------------------

let lastDoc: Doc | null = null
// Kept separately from the document: a parse that fails has no document, and a
// diagnostic without its line number is exactly the case that needs one.
let lastSrc: Uint8Array = new Uint8Array(0)
const lastDiag = new Diag()

/** Parses the JSON at inPtr. Returns the root kind, or -(diag code). */
export function jsonParse(len: i32): i32 {
  const src = new Uint8Array(len)
  memory.copy(src.dataStart, IN.dataStart, <usize>len)
  lastDiag.reset()
  lastSrc = src
  lastDoc = parseJSON(src, lastDiag)
  const doc = lastDoc
  if (doc == null) return -lastDiag.code
  return <i32>doc.kindOf(doc.root)
}

export function jsonErrOffset(): i32 {
  return lastDiag.offset
}

export function jsonErrLine(): i32 {
  return lastDiag.offset < 0 ? -1 : lineOf(lastSrc, lastDiag.offset)
}

/** Copies the diagnostic message to outPtr and returns its byte length. */
export function jsonErrMessage(): i32 {
  const bytes = String.UTF8.encode(lastDiag.message, false)
  const view = Uint8Array.wrap(bytes)
  memory.copy(OUT.dataStart, view.dataStart, <usize>view.length)
  return view.length
}

/** The root's 64-bit payload: an int, a uint's bits, a bool, or f64 bits. */
export function jsonRootNum(): i64 {
  const doc = lastDoc
  if (doc == null) return 0
  return unchecked(doc.num[doc.root])
}

/** Copies the root string's bytes to outPtr and returns the length. */
export function jsonRootString(): i32 {
  const doc = lastDoc
  if (doc == null) return -1
  if (doc.kindOf(doc.root) != K_STRING) return -1
  const s = doc.strOf(doc.root)
  memory.copy(OUT.dataStart, s.dataStart, <usize>s.length)
  return s.length
}

export function jsonRootCount(): i32 {
  const doc = lastDoc
  return doc == null ? -1 : doc.count(doc.root)
}

/** Kind of the i-th child of the root. */
export function jsonChildKind(i: i32): i32 {
  const doc = lastDoc
  if (doc == null) return -1
  return <i32>doc.kindOf(doc.childAt(doc.root, i))
}

/** Copies the i-th key of the root object to outPtr; returns its length. */
export function jsonChildKey(i: i32): i32 {
  const doc = lastDoc
  if (doc == null) return -1
  const k = doc.keyOf(doc.root, i)
  memory.copy(OUT.dataStart, k.dataStart, <usize>k.length)
  return k.length
}

export function jsonIsFloatRoot(): bool {
  const doc = lastDoc
  return doc != null && doc.kindOf(doc.root) == K_FLOAT
}

// ---- inference -------------------------------------------------------------

let lastSchema: Schema | null = null

/** Parses and infers. Returns the shape, or -(diag code). */
export function inferJSON(len: i32): i32 {
  const src = new Uint8Array(len)
  memory.copy(src.dataStart, IN.dataStart, <usize>len)
  lastDiag.reset()
  lastSrc = src
  lastSchema = null
  const doc = parseJSON(src, lastDiag)
  if (doc == null) return -lastDiag.code
  lastDoc = doc
  const schema = inferSchema(doc, lastDiag)
  if (schema == null) return -lastDiag.code
  lastSchema = schema
  return schema.shape
}

export function inferRecordCount(): i32 {
  const s = lastSchema
  return s == null ? -1 : s.records.length
}

/** Walks to a nested type by a path of field indices packed into an i64. */
function typeAt(path: i64, depth: i32): Type | null {
  const s = lastSchema
  if (s == null) return null
  let t: Type | null = s.root
  for (let i = 0; i < depth; i++) {
    const cur = t
    if (cur == null) return null
    const step = <i32>((path >> (<i64>(i * 8))) & 0xff)
    if (cur.ft == 4) {
      t = cur.elem // array: the step is ignored, there is one element type
    } else {
      if (step >= cur.fields.length) return null
      t = unchecked(cur.fields[step]).type
    }
  }
  return t
}

export function schemaFieldCount(path: i64, depth: i32): i32 {
  const t = typeAt(path, depth)
  return t == null ? -1 : t.fields.length
}

export function schemaFt(path: i64, depth: i32): i32 {
  const t = typeAt(path, depth)
  return t == null ? -1 : <i32>t.ft
}

export function schemaKind(path: i64, depth: i32): i32 {
  const t = typeAt(path, depth)
  return t == null ? -1 : <i32>t.kind
}

export function schemaNullable(path: i64, depth: i32): i32 {
  const t = typeAt(path, depth)
  return t == null ? -1 : (t.nullable ? 1 : 0)
}

export function schemaFieldId(path: i64, depth: i32, i: i32): i32 {
  const t = typeAt(path, depth)
  if (t == null || i >= t.fields.length) return -1
  return <i32>unchecked(t.fields[i]).id
}

/** Copies the i-th field name at that path to outPtr; returns its length. */
export function schemaFieldName(path: i64, depth: i32, i: i32): i32 {
  const t = typeAt(path, depth)
  if (t == null || i >= t.fields.length) return -1
  const n = unchecked(t.fields[i]).name
  memory.copy(OUT.dataStart, n.dataStart, <usize>n.length)
  return n.length
}

export function diagPath(): i32 {
  const bytes = String.UTF8.encode(lastDiag.path, false)
  const view = Uint8Array.wrap(bytes)
  memory.copy(OUT.dataStart, view.dataStart, <usize>view.length)
  return view.length
}

export function diagWarningCount(): i32 {
  return lastDiag.warnings.length
}

export function diagWarning(i: i32): i32 {
  if (i >= lastDiag.warnings.length) return -1
  const bytes = String.UTF8.encode(unchecked(lastDiag.warnings[i]), false)
  const view = Uint8Array.wrap(bytes)
  memory.copy(OUT.dataStart, view.dataStart, <usize>view.length)
  return view.length
}

/**
 * Field ids for a list of names supplied at inPtr as length-prefixed UTF-8
 * (one leading byte per name), written back to outPtr as one id per name.
 */
export function fieldIdsFor(count: i32): i32 {
  const fields: Array<Field> = []
  let pos = 0
  for (let i = 0; i < count; i++) {
    const len = <i32>load<u8>(IN.dataStart + <usize>pos)
    pos++
    const name = new Uint8Array(len)
    memory.copy(name.dataStart, IN.dataStart + <usize>pos, <usize>len)
    pos += len
    fields.push(new Field(name, new Type(0, 4)))
  }
  if (!assignFieldIDs(fields)) return -1
  for (let i = 0; i < count; i++) {
    store<u8>(OUT.dataStart + <usize>i, unchecked(fields[i]).id)
  }
  return count
}

/** Parses, infers and encodes the JSON at inPtr. Returns the message length at
 *  outPtr, or -(diag code). */
export function encodeJSON(len: i32): i32 {
  const src = new Uint8Array(len)
  memory.copy(src.dataStart, IN.dataStart, <usize>len)
  lastDiag.reset()
  lastSrc = src
  const doc = parseJSON(src, lastDiag)
  if (doc == null) return -lastDiag.code
  lastDoc = doc
  const schema = inferSchema(doc, lastDiag)
  if (schema == null) return -lastDiag.code
  lastSchema = schema
  const msg = encodeMessage(doc, schema, lastDiag)
  if (msg == null) return -lastDiag.code
  memory.copy(OUT.dataStart, msg.dataStart, <usize>msg.length)
  return msg.length
}

/** A colbin message at inPtr -> JSON text at outPtr. Returns -(code) on failure. */
export function decodeMsg(len: i32): i32 {
  const buf = new Uint8Array(len)
  memory.copy(buf.dataStart, IN.dataStart, <usize>len)
  lastDiag.reset()
  lastSrc = new Uint8Array(0)
  const text = decodeMessage(buf, lastDiag)
  if (text == null) return -lastDiag.code
  memory.copy(OUT.dataStart, text.dataStart, <usize>text.length)
  return text.length
}

/** Encode with the self-check on, so the tests exercise the path the ABI uses. */
export function encodeVerified(len: i32): i32 {
  const src = new Uint8Array(len)
  memory.copy(src.dataStart, IN.dataStart, <usize>len)
  lastDiag.reset()
  lastSrc = src
  const doc = parseJSON(src, lastDiag)
  if (doc == null) return -lastDiag.code
  lastDoc = doc
  const schema = inferSchema(doc, lastDiag)
  if (schema == null) return -lastDiag.code
  const msg = encodeMessage(doc, schema, lastDiag)
  if (msg == null) return -lastDiag.code
  const decoded = decodeValues(msg, lastDiag)
  if (decoded == null) return -lastDiag.code
  if (!new Verifier(doc, lastDiag).check(schema.records, decoded)) return -lastDiag.code
  memory.copy(OUT.dataStart, msg.dataStart, <usize>msg.length)
  return msg.length
}

/** A colbin message at inPtr -> the inspector's JSON at outPtr. */
export function inspectMsg(len: i32): i32 {
  const buf = new Uint8Array(len)
  memory.copy(buf.dataStart, IN.dataStart, <usize>len)
  lastDiag.reset()
  const text = inspectMessage(buf, lastDiag)
  if (text == null) return -lastDiag.code
  memory.copy(OUT.dataStart, text.dataStart, <usize>text.length)
  return text.length
}
