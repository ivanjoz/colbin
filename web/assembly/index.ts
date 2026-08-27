// The public ABI (PLAN.md §2.1).
//
// UTF-8 bytes in, UTF-8 bytes out, and no other shape crosses the boundary: the
// host never hands over a JavaScript object graph, because JSON.parse would have
// already destroyed any integer past 2^53 before the codec saw it.
//
// Nothing here traps. An AssemblyScript abort reaches the host as a RuntimeError
// carrying no path and no offset, which is exactly what a caller cannot act on,
// so every failure returns a negative length and leaves a JSON diagnostic for
// lastError() to hand back.

import { Diag, lineOf } from './diag'
import { decodeMessage, decodeValues, inspectMessage } from './decode'
import { encodeMessage } from './encode'
import { inferSchema } from './infer'
import { parseJSON } from './json'
import { Verifier } from './verify'
import { Writer } from './bytes'

/** Held in a global so the collector cannot reclaim it between calls. */
let input: Uint8Array = new Uint8Array(0)
let result: Uint8Array = new Uint8Array(0)
const diag = new Diag()
let source: Uint8Array = new Uint8Array(0)

/** Reserves n bytes for the caller to write the input into. */
export function alloc(n: i32): usize {
  input = new Uint8Array(n)
  return input.dataStart
}

/** Where the last successful call left its output. */
export function resultPtr(): usize {
  return result.dataStart
}

/**
 * JSON to a colbin JSON-mode message.
 * Returns the message length, or -1 with a diagnostic in lastError().
 *
 * verify != 0 makes the encoder read its own output back and compare it against
 * the input before returning, which is what turns "a decodable message or an
 * error, never anything else" from an argument into a check (PLAN.md §4.5). It
 * costs about one decode. Pass 0 only after measuring and deciding.
 */
export function encode(len: i32, verify: i32): i32 {
  diag.reset()
  const src = input.subarray(0, len)
  source = src

  const doc = parseJSON(src, diag)
  if (doc == null) return -1
  const schema = inferSchema(doc, diag)
  if (schema == null) return -1
  const message = encodeMessage(doc, schema, diag)
  if (message == null) return -1

  if (verify != 0) {
    const decoded = decodeValues(message!, diag)
    if (decoded == null) return -1
    if (!new Verifier(doc!, diag).check(schema!.records, decoded!)) return -1
  }

  result = message
  return message.length
}

/**
 * A colbin JSON-mode message back to JSON text.
 * Returns its length, or -1 with a diagnostic in lastError().
 */
export function decode(len: i32): i32 {
  diag.reset()
  source = new Uint8Array(0) // offsets refer to the message, not to any source
  const text = decodeMessage(input.subarray(0, len), diag)
  if (text == null) return -1
  result = text
  return text.length
}

/**
 * The column tree of a message as JSON, with each column's byte span.
 * Returns its length, or -1 with a diagnostic in lastError().
 */
export function inspect(len: i32): i32 {
  diag.reset()
  source = new Uint8Array(0)
  const text = inspectMessage(input.subarray(0, len), diag)
  if (text == null) return -1
  result = text
  return text.length
}

/**
 * The last failure as JSON: code, path, offset, line, message, warnings.
 * Returns its length, with the bytes at resultPtr().
 */
export function lastError(): i32 {
  const w = new Writer(256)
  writeString(w, '{"code":')
  writeString(w, diag.code.toString())
  writeString(w, ',"offset":')
  writeString(w, diag.offset.toString())
  writeString(w, ',"line":')
  writeString(w, (diag.offset < 0 ? -1 : lineOf(source, diag.offset)).toString())
  writeString(w, ',"path":')
  writeJSONString(w, diag.path)
  writeString(w, ',"message":')
  writeJSONString(w, diag.message)
  writeString(w, ',"warnings":[')
  for (let i = 0; i < diag.warnings.length; i++) {
    if (i > 0) w.writeByte(0x2c)
    writeJSONString(w, unchecked(diag.warnings[i]))
  }
  writeString(w, ']}')

  result = w.take()
  return result.length
}

function writeString(w: Writer, s: string): void {
  const bytes = Uint8Array.wrap(String.UTF8.encode(s, false))
  w.writeBytes(bytes, 0, bytes.length)
}

/** A JSON string literal. Diagnostics are the only strings this writes. */
function writeJSONString(w: Writer, s: string): void {
  const bytes = Uint8Array.wrap(String.UTF8.encode(s, false))
  w.writeByte(0x22)
  for (let i = 0; i < bytes.length; i++) {
    const c = unchecked(bytes[i])
    if (c == 0x22 || c == 0x5c) {
      w.writeByte(0x5c)
      w.writeByte(c)
    } else if (c == 0x0a) {
      w.writeByte(0x5c)
      w.writeByte(0x6e)
    } else if (c < 0x20) {
      writeString(w, '\\u00')
      w.writeByte(hexDigit(c >> 4))
      w.writeByte(hexDigit(c & 0x0f))
    } else {
      w.writeByte(c)
    }
  }
  w.writeByte(0x22)
}

@inline function hexDigit(v: u8): u8 {
  return v < 10 ? 0x30 + v : 0x61 + (v - 10)
}
