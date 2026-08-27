// Structured failure, for every layer.
//
// PLAN.md §4.4: one shape for parse errors, type conflicts, limits and corrupt
// messages alike. Nothing here throws or aborts — an AssemblyScript trap reaches
// the host as an RuntimeError carrying no path and no offset, which is exactly
// what a caller cannot act on.

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
