/**
 * The one failure type. PACKAGE_PLAN.md §6: a thrown error rather than an
 * `Outcome` union, because a library consumer's normal path is success and a
 * page that wants a union can wrap this in three lines.
 *
 * Every field the module's diagnostic carries survives onto the error, so a
 * caller can point at the byte that failed rather than re-deriving it from a
 * message string.
 */
export class ColbinError extends Error {
  /** The module's diagnostic code. `2` is the wire-level refusal every
   * decode-only build can still produce; the encoder's parser uses its own. */
  readonly code: number
  /** Byte offset into the input the failure was found at, or -1. */
  readonly offset: number
  /** 1-based line, for a failure in JSON text, or -1. */
  readonly line: number
  /** The path through the document — `records[3].price` — or `''`. */
  readonly path: string
  /** Warnings the module gathered before it gave up. Usually empty. */
  readonly warnings: string[]

  constructor(diagnostic: Diagnostic) {
    super(diagnostic.message)
    this.name = 'ColbinError'
    this.code = diagnostic.code
    this.offset = diagnostic.offset
    this.line = diagnostic.line
    this.path = diagnostic.path
    this.warnings = diagnostic.warnings ?? []
  }
}

/** The JSON envelope `colbin::diag::Diag::encode` writes. */
export type Diagnostic = {
  code: number
  offset: number
  line: number
  path: string
  message: string
  warnings: string[]
}
