// Structured failure, for every layer.
//
// PLAN.md §4.4: one shape for parse errors, type conflicts, limits and corrupt
// messages alike. Nothing here throws or aborts — an AssemblyScript trap reaches
// the host as an RuntimeError carrying no path and no offset, which is exactly
// what a caller cannot act on.

import { Writer } from './bytes'

export const D_NONE: i32 = 0
export const D_SYNTAX: i32 = 1
export const D_NUMBER: i32 = 2
export const D_LIMIT: i32 = 3
export const D_CONFLICT: i32 = 4
export const D_CORRUPT: i32 = 5
export const D_UNSUPPORTED: i32 = 6

export class Diag {
  code: i32 = D_NONE
  offset: i32 = -1
  line: i32 = -1
  path: string = ''
  message: string = ''
  warnings: Array<string> = []

  @inline get ok(): bool {
    return this.code == D_NONE
  }

  /** First failure wins: later ones are usually consequences of the first. */
  fail(code: i32, offset: i32, path: string, message: string): void {
    if (this.code != D_NONE) return
    this.code = code
    this.offset = offset
    this.path = path
    this.message = message
  }

  warn(message: string): void {
    this.warnings.push(message)
  }

  reset(): void {
    this.code = D_NONE
    this.offset = -1
    this.line = -1
    this.path = ''
    this.message = ''
    this.warnings = []
  }

  /** The diagnostic as JSON: code, offset, line, path, message, warnings. */
  encode(): Uint8Array {
    const out = new Writer(256)
    writeText(out, '{"code":')
    writeText(out, this.code.toString())
    writeText(out, ',"offset":')
    writeText(out, this.offset.toString())
    writeText(out, ',"line":')
    writeText(out, this.line.toString())
    writeText(out, ',"path":')
    writeJSONText(out, this.path)
    writeText(out, ',"message":')
    writeJSONText(out, this.message)
    writeText(out, ',"warnings":[')
    for (let index = 0; index < this.warnings.length; index++) {
      if (index > 0) out.writeByte(0x2c)
      writeJSONText(out, unchecked(this.warnings[index]))
    }
    writeText(out, ']}')
    return out.take()
  }
}

function writeText(out: Writer, text: string): void {
  const bytes = Uint8Array.wrap(String.UTF8.encode(text, false))
  out.writeBytes(bytes, 0, bytes.length)
}

/** A JSON string literal. Diagnostics are the only strings this writes, so the
 * escape set is the minimum that keeps the envelope parseable rather than
 * encoding/json's — jsontext.ts is where that one lives. */
function writeJSONText(out: Writer, text: string): void {
  const bytes = Uint8Array.wrap(String.UTF8.encode(text, false))
  out.writeByte(0x22)
  for (let index = 0; index < bytes.length; index++) {
    const character = unchecked(bytes[index])
    if (character == 0x22 || character == 0x5c) {
      out.writeByte(0x5c)
      out.writeByte(character)
    } else if (character == 0x0a) {
      out.writeByte(0x5c)
      out.writeByte(0x6e)
    } else if (character < 0x20) {
      writeText(out, '\\u00')
      out.writeByte(hexDigit(character >> 4))
      out.writeByte(hexDigit(character & 0x0f))
    } else {
      out.writeByte(character)
    }
  }
  out.writeByte(0x22)
}

@inline
function hexDigit(value: u8): u8 {
  return value < 10 ? 0x30 + value : 0x61 + (value - 10)
}

/**
 * 1-based line of a byte offset. Computed only when reporting, so the parser
 * does not carry a line counter through its hot loop.
 */
export function lineOf(src: Uint8Array, offset: i32): i32 {
  let line = 1
  const end = offset < src.length ? offset : src.length
  for (let i = 0; i < end; i++) {
    if (unchecked(src[i]) == 0x0a) line++
  }
  return line
}
