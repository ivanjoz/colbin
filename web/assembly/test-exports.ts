// Test-only entry point. Exposes the internal codecs over two static scratch
// buffers so the Node harness can drive them without the AssemblyScript loader.
// Not part of the public ABI and not built into the site bundle.

import { Reader, Writer } from './bytes'
import { Builder } from './build'
import { appendArray, decodeArray } from './column'
import { inferPlan } from './infer'
import { inspect } from './inspect'
import { toJSON } from './message'
import { Plan } from './plan'
import { buildSection, parseSection } from './section'
import { verify } from './verify'
import { writeBlob as writeBlobNarrow } from './wire/narrow'
import { NarrowWriter } from './wire/narrowwrite'
import { WideWriter } from './wire/widewrite'
import { writeBlob as writeBlobWide } from './wire/wide'

const IN = new Uint8Array(1 << 22)
const OUT = new Uint8Array(1 << 22)

export function inPtr(): usize {
  return IN.dataStart
}

export function outPtr(): usize {
  return OUT.dataStart
}

function emit(bytes: Uint8Array): i32 {
  const length = bytes.length <= OUT.length ? bytes.length : OUT.length
  memory.copy(OUT.dataStart, bytes.dataStart, <usize>length)
  return length
}

// ---- the column codec -------------------------------------------------------

/** n little-endian i64 values at inPtr -> a column frame at outPtr. */
export function columnEncode(n: i32, width: i32): i32 {
  const values = new Int64Array(n)
  for (let index = 0; index < n; index++) {
    unchecked((values[index] = load<i64>(IN.dataStart + <usize>(index << 3))))
  }
  const out = new Writer(1024)
  appendArray(out, values, n, width)
  return emit(out.take())
}

/**
 * A column frame at inPtr -> n little-endian i64 values at outPtr. Returns the
 * number of values, or -1 when the frame does not read.
 */
export function columnDecode(frameLen: i32, n: i32, width: i32): i32 {
  const reader = new Reader(IN.subarray(0, frameLen))
  const values = new Int64Array(n)
  if (!decodeArray(reader, n, values, width)) return -1
  for (let index = 0; index < n; index++) {
    store<i64>(OUT.dataStart + <usize>(index << 3), unchecked(values[index]))
  }
  return n
}

// ---- blob framing -----------------------------------------------------------

/**
 * A blob of `size` bytes at inPtr, under `key` -> the whole frame at outPtr.
 *
 * The vectors record the header alone, so the test compares the first
 * (returned length - size) bytes. Writing the payload too is what proves the
 * header and the payload compose, which is the property the tier leans on.
 */
export function blobNarrow(key: i32, size: i32): i32 {
  const out = new Writer(size + 16)
  writeBlobNarrow(out, <u8>key, IN.subarray(0, size))
  return emit(out.take())
}

/** A string of `size` bytes at inPtr -> its packed field at outPtr, at either
 * key width. This is where the encoder's every decision shows up as bytes. */
export function packedNarrow(key: i32, size: i32): i32 {
  const out = new Writer(size + 32)
  new NarrowWriter(out).packedString(<u8>key, IN.subarray(0, size))
  return emit(out.take())
}

export function packedWide(key: i32, size: i32): i32 {
  const out = new Writer(size + 32)
  new WideWriter(out).packedString(<u8>key, IN.subarray(0, size))
  return emit(out.take())
}

export function blobWide(key: i32, size: i32): i32 {
  const out = new Writer(size + 16)
  writeBlobWide(out, <u8>key, IN.subarray(0, size))
  return emit(out.take())
}

// ---- messages ---------------------------------------------------------------

/** The schema held between setSection and messageJSON. */
let section: Plan | null = null

/**
 * Parses a schema section at inPtr, or clears the held one at length zero.
 *
 * Clearing has to be a case of its own. Without it a test that meant "decode
 * this with nothing but the message's own section" silently reused whatever the
 * test before it left behind — which is exactly how a self-describing case can
 * pass without the self-describing path ever running.
 */
export function setSection(len: i32): i32 {
  if (len == 0) {
    section = null
    failure = ''
    return 0
  }
  const parsed = parseSection(IN.subarray(0, len))
  if (!parsed.ok) {
    failure = parsed.error
    return -1
  }
  section = parsed.plan
  failure = ''
  return 0
}

let failure: string = ''

/**
 * A message at inPtr -> JSON text at outPtr, through the held section or the
 * message's own. Returns the length, or -1 with lastMessage set.
 */
export function messageJSON(len: i32): i32 {
  const decoded = toJSON(IN.subarray(0, len), section)
  if (!decoded.ok) {
    failure = decoded.error
    return -1
  }
  failure = ''
  return emit(decoded.json)
}

/** A message at inPtr -> its field tree as JSON at outPtr. */
export function inspectJSON(len: i32): i32 {
  const tree = inspect(IN.subarray(0, len), section)
  if (!tree.ok) {
    failure = tree.error
    return -1
  }
  failure = ''
  return emit(tree.json)
}

/** The last failure, as UTF-8 at outPtr. */
export function lastMessage(): i32 {
  return emit(Uint8Array.wrap(String.UTF8.encode(failure, false)))
}

// ---- encode -----------------------------------------------------------------

let encodedSection: Uint8Array = new Uint8Array(0)

/**
 * JSON at inPtr -> a message at outPtr, with the section available from
 * lastSection(). `flags` is the public ABI's: 1 self-describing, 2 verify.
 */
export function encodeJSON(len: i32, flags: i32): i32 {
  const scratch = new Diag()
  encodedSection = new Uint8Array(0)
  const doc = parseJSON(IN.subarray(0, len), scratch)
  if (doc == null) {
    failure = scratch.message
    return -1
  }
  const inferred = inferPlan(doc, scratch)
  if (inferred == null) {
    failure = scratch.message
    return -1
  }
  const plan = inferred.plan
  const built = buildSection(plan)
  const builder = new Builder(doc, scratch)
  builder.packStrings = (flags & 4) != 0
  const selfDescribing = (flags & 1) != 0
  if (selfDescribing) {
    builder.out.writeByte(plan.isWide ? 0xdc : 0xd4)
    builder.out.writeBytes(built, 0, built.length)
    builder.run(plan, inferred.root)
  } else {
    builder.build(plan, inferred.root, false)
  }
  if (!scratch.ok) {
    failure = scratch.message
    return -1
  }
  const message = builder.out.take()

  if ((flags & 2) != 0) {
    const decoded = toJSON(message, selfDescribing ? null : plan)
    if (!decoded.ok) {
      failure = 'cannot read back: ' + decoded.error
      return -1
    }
    if (!verify(doc, inferred.root, decoded.json, scratch)) {
      failure = scratch.message
      return -1
    }
  }

  encodedSection = built
  failure = ''
  // The section is held for the caller rather than emitted, so one call gives
  // the message and the next gives the schema without re-encoding.
  return emit(message)
}

/** The section for the last encodeJSON. */
export function lastSection(): i32 {
  return emit(encodedSection)
}

/** Warnings the last encode raised, newline separated. */
export function lastWarnings(): i32 {
  return emit(Uint8Array.wrap(String.UTF8.encode(warnings, false)))
}

let warnings: string = ''

// ---- the JSON scanner -------------------------------------------------------
//
// json.ts and decimal.ts are what the format change does not touch, so these
// entry points are carried across unchanged from the first port.

import { Diag, lineOf } from './diag'
import { Doc, K_FLOAT, K_STRING, parseJSON } from './json'

let doc: Doc | null = null
const scannerDiag = new Diag()
let scannerSource: Uint8Array = new Uint8Array(0)

/** Parses the JSON at inPtr. Returns the root kind, or -(diag code). */
export function jsonParse(len: i32): i32 {
  scannerDiag.reset()
  scannerSource = IN.subarray(0, len)
  doc = parseJSON(scannerSource, scannerDiag)
  const held = doc
  if (held == null) return -scannerDiag.code
  return <i32>held.kindOf(held.root)
}

export function jsonErrOffset(): i32 {
  return scannerDiag.offset
}

export function jsonErrLine(): i32 {
  return scannerDiag.offset < 0 ? -1 : lineOf(scannerSource, scannerDiag.offset)
}

/** Copies the diagnostic message to outPtr and returns its byte length. */
export function jsonErrMessage(): i32 {
  return emit(Uint8Array.wrap(String.UTF8.encode(scannerDiag.message, false)))
}

/** The root's 64-bit payload: an int, a uint's bits, a bool, or f64 bits. */
export function jsonRootNum(): i64 {
  const held = doc
  return held == null ? 0 : unchecked(held.num[held.root])
}

/** Copies the root string's bytes to outPtr and returns the length. */
export function jsonRootString(): i32 {
  const held = doc
  if (held == null) return -1
  if (held.kindOf(held.root) != K_STRING) return -1
  return emit(held.strOf(held.root))
}

export function jsonRootCount(): i32 {
  const held = doc
  return held == null ? -1 : held.count(held.root)
}

/** Kind of the i-th child of the root. */
export function jsonChildKind(index: i32): i32 {
  const held = doc
  if (held == null) return -1
  return <i32>held.kindOf(held.childAt(held.root, index))
}

/** Copies the i-th key of the root object to outPtr; returns its length. */
export function jsonChildKey(index: i32): i32 {
  const held = doc
  if (held == null) return -1
  return emit(held.keyOf(held.root, index))
}

export function jsonIsFloatRoot(): bool {
  const held = doc
  return held != null && held.kindOf(held.root) == K_FLOAT
}
