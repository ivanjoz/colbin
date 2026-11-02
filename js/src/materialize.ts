// Reads the flat buffer `colbin::materialize::to_buffer` writes and turns it
// into row objects without going through JSON text — INTERNALS.md §16.3.
//
// The buffer holds the tables at a message's root: one, for a root that was a
// bare array (the reader returns the array), or one per field of a root struct
// whose every field is a table (the reader returns an object of arrays). A
// column may be nullable, in which case a validity bitmap precedes its values.
//
// The buffer has no offset table: every run's byte length is derivable from
// `rowCount` and the field's own kind and flags, so this walks it once, in field
// order, the same way the Rust writer laid it out. See
// `rust/src/materialize.rs`'s module doc for the exact layout.

const VERSION = 2
const SHAPE_ARRAY = 0
const SHAPE_OBJECT = 1

const KIND_BOOL = 0
const KIND_INT = 1
const KIND_FLOAT = 2
const KIND_STRING = 3

const FLAG_SIGNED = 1 << 0
const FLAG_EXCEEDS_SAFE_INT = 1 << 1
const FLAG_ASCII = 1 << 2
const FLAG_NULLABLE = 1 << 3

/**
 * What a column's values are read as. Derived purely from a field's `kind`
 * and `flags` — never from the data — which is what lets the generated row
 * builder below be cached by field signature and reused across messages from
 * the same endpoint.
 */
type ColumnKind =
  | 'bool'
  | 'intSafe'
  | 'intBigSigned'
  | 'intBigUnsigned'
  | 'float'
  | 'stringAscii'
  | 'stringUtf8'

function classify(kind: number, flags: number): ColumnKind {
  if (kind === KIND_BOOL) return 'bool'
  if (kind === KIND_INT) {
    if ((flags & FLAG_EXCEEDS_SAFE_INT) === 0) return 'intSafe'
    return (flags & FLAG_SIGNED) !== 0 ? 'intBigSigned' : 'intBigUnsigned'
  }
  if (kind === KIND_FLOAT) return 'float'
  return (flags & FLAG_ASCII) !== 0 ? 'stringAscii' : 'stringUtf8'
}

type Field = { name: string; col: ColumnKind; nullable: boolean }

/** A uniform per-row accessor, for the CSP fallback path only — the generated
 * builder never calls this, since a function call per cell is exactly the
 * cost a monomorphic object literal was measured beating by 10x. */
type Generic = { get(row: number): unknown }

/**
 * One field's data, as views over the buffer's bytes. Every reader below —
 * the generated row builder's arguments, the fallback's accessors, and the
 * public column table — is derived from these, so the cursor arithmetic that
 * walks the runs is written once.
 */
type Run = (
  | { col: 'bool'; bitmap: Uint8Array }
  | { col: 'intSafe'; hi: Int32Array; lo: Uint32Array }
  | { col: 'intBigSigned'; view: BigInt64Array }
  | { col: 'intBigUnsigned'; view: BigUint64Array }
  | { col: 'float'; view: Float64Array }
  | { col: 'stringAscii'; text: string; offsets: Uint32Array }
  | { col: 'stringUtf8'; blob: Uint8Array; offsets: Uint32Array }
) & {
  /** A nullable column's validity bitmap: bit `i` set means row `i` holds a
   * value, clear means it is null. Absent for a column that cannot be null. */
  valid?: Uint8Array
}

function isValid(valid: Uint8Array, i: number): boolean {
  return ((valid[i >> 3] >> (i & 7)) & 1) === 1
}

function pad8(at: number): number {
  return Math.ceil(at / 8) * 8
}

/** Reads every field's run at the current cursor, in header order. */
function readRuns(
  buffer: ArrayBuffer,
  start: number,
  rowCount: number,
  fields: Field[],
): { runs: Run[]; at: number } {
  const runs: Run[] = []
  let at = start

  for (const field of fields) {
    let valid: Uint8Array | undefined
    if (field.nullable) {
      const bytes = Math.ceil(rowCount / 8)
      valid = new Uint8Array(buffer, at, bytes)
      at = pad8(at + bytes)
    }
    const before = runs.length
    switch (field.col) {
      case 'bool': {
        const bytes = Math.ceil(rowCount / 8)
        runs.push({ col: 'bool', bitmap: new Uint8Array(buffer, at, bytes) })
        at += bytes
        break
      }
      case 'intSafe': {
        // Low/high Int32 pairs over the same bytes an i64 run holds. `hi` is
        // read signed and `lo` unsigned: that reconstructs any 64-bit two's
        // complement pattern exactly, whichever the field's own signedness,
        // as long as the true value's magnitude is under 2^53 — which the
        // exceeds flag guarantees for this column kind (INTERNALS.md §16.3).
        runs.push({
          col: 'intSafe',
          hi: new Int32Array(buffer, at, rowCount * 2),
          lo: new Uint32Array(buffer, at, rowCount * 2),
        })
        at += rowCount * 8
        break
      }
      case 'intBigSigned': {
        runs.push({ col: 'intBigSigned', view: new BigInt64Array(buffer, at, rowCount) })
        at += rowCount * 8
        break
      }
      case 'intBigUnsigned': {
        runs.push({ col: 'intBigUnsigned', view: new BigUint64Array(buffer, at, rowCount) })
        at += rowCount * 8
        break
      }
      case 'float': {
        runs.push({ col: 'float', view: new Float64Array(buffer, at, rowCount) })
        at += rowCount * 8
        break
      }
      case 'stringAscii': {
        const offsets = new Uint32Array(buffer, at, rowCount + 1)
        const blobStart = at + (rowCount + 1) * 4
        const blobLen = offsets[rowCount]
        // One bulk decode over the whole blob, then `substring` per row: 10-12x
        // faster than decoding each string, because byte offsets equal UTF-16
        // code unit offsets exactly when the blob is ASCII.
        const text = new TextDecoder().decode(new Uint8Array(buffer, blobStart, blobLen))
        runs.push({ col: 'stringAscii', text, offsets })
        at = blobStart + blobLen
        break
      }
      case 'stringUtf8': {
        const offsets = new Uint32Array(buffer, at, rowCount + 1)
        const blobStart = at + (rowCount + 1) * 4
        const blobLen = offsets[rowCount]
        runs.push({
          col: 'stringUtf8',
          blob: new Uint8Array(buffer, blobStart, blobLen),
          offsets,
        })
        at = blobStart + blobLen
        break
      }
    }
    if (valid) runs[before].valid = valid
    at = pad8(at)
  }
  return { runs, at }
}

/** The flattened argument list the generated builder wants: one or two typed
 * arrays per field, in field order, matching `getFactory`'s parameter list. */
function argsOf(runs: Run[]): unknown[] {
  const args: unknown[] = []
  for (const run of runs) {
    if (run.valid) args.push(run.valid)
    switch (run.col) {
      case 'bool':
        args.push(run.bitmap)
        break
      case 'intSafe':
        args.push(run.hi, run.lo)
        break
      case 'intBigSigned':
      case 'intBigUnsigned':
      case 'float':
        args.push(run.view)
        break
      case 'stringAscii':
        args.push(run.text, run.offsets)
        break
      case 'stringUtf8':
        args.push(run.blob, run.offsets)
        break
    }
  }
  return args
}

/** A uniform reader per field, for the CSP fallback. */
function genericOf(runs: Run[]): Generic[] {
  return runs.map((run): Generic => {
    const read = valueOf(run)
    const valid = run.valid
    return valid ? { get: (i) => (isValid(valid, i) ? read.get(i) : null) } : read
  })
}

/** A run's values, nulls not considered. */
function valueOf(run: Run): Generic {
  switch (run.col) {
    case 'bool':
      return { get: (i) => ((run.bitmap[i >> 3] >> (i & 7)) & 1) === 1 }
    case 'intSafe':
      return { get: (i) => run.hi[2 * i + 1] * 4294967296 + run.lo[2 * i] }
    case 'intBigSigned':
    case 'intBigUnsigned':
    case 'float':
      return { get: (i) => run.view[i] }
    case 'stringAscii':
      return { get: (i) => run.text.substring(run.offsets[i], run.offsets[i + 1]) }
    case 'stringUtf8': {
      const decoder = new TextDecoder()
      return { get: (i) => decoder.decode(run.blob.subarray(run.offsets[i], run.offsets[i + 1])) }
    }
  }
}

type Factory = (rowCount: number, ...args: unknown[]) => unknown[]

const factoryCache = new Map<string, Factory>()

function signatureOf(fields: Field[]): string {
  return fields.map((f) => `${f.col}${f.nullable ? '?' : ''}:${f.name}`).join('|')
}

/**
 * A generated, monomorphic row builder for this exact field signature,
 * compiled once and cached — measured at 10x a generic `for key in schema`
 * loop, because V8 gives every row the same hidden class.
 */
function getFactory(fields: Field[]): Factory {
  const key = signatureOf(fields)
  const cached = factoryCache.get(key)
  if (cached) return cached

  const params: string[] = []
  const entries: string[] = []
  fields.forEach((field, k) => {
    const prop = JSON.stringify(field.name)
    const valid = `n${k}`
    if (field.nullable) params.push(valid)
    const before = entries.length
    switch (field.col) {
      case 'bool': {
        const p = `b${k}`
        params.push(p)
        entries.push(`${prop}:((${p}[i>>3]>>(i&7))&1)===1`)
        break
      }
      case 'intSafe': {
        const hi = `h${k}`
        const lo = `l${k}`
        params.push(hi, lo)
        entries.push(`${prop}:${hi}[2*i+1]*4294967296+${lo}[2*i]`)
        break
      }
      case 'intBigSigned':
      case 'intBigUnsigned': {
        const p = `v${k}`
        params.push(p)
        entries.push(`${prop}:${p}[i]`)
        break
      }
      case 'float': {
        const p = `f${k}`
        params.push(p)
        entries.push(`${prop}:${p}[i]`)
        break
      }
      case 'stringAscii': {
        const s = `s${k}`
        const o = `o${k}`
        params.push(s, o)
        entries.push(`${prop}:${s}.substring(${o}[i],${o}[i+1])`)
        break
      }
      case 'stringUtf8': {
        const s = `s${k}`
        const o = `o${k}`
        params.push(s, o)
        entries.push(`${prop}:__dec.decode(${s}.subarray(${o}[i],${o}[i+1]))`)
        break
      }
    }
    if (field.nullable) {
      // `prop:expr` becomes `prop:(valid bit set ? expr : null)`.
      const entry = entries[before]
      const expr = entry.slice(prop.length + 1)
      entries[before] = `${prop}:((${valid}[i>>3]>>(i&7))&1)===1?${expr}:null`
    }
  })

  const body =
    `const __dec=new TextDecoder();` +
    `return function(n,${params.join(',')}){` +
    `const out=new Array(n);` +
    `for(let i=0;i<n;i++)out[i]={${entries.join(',')}};` +
    `return out};`
  // A generated function, not a string eval'd in scope: the only thing built
  // from field data is a property name, quoted through `JSON.stringify`, so
  // there is no injection surface here even though the source is assembled by
  // hand.
  // eslint-disable-next-line no-new-func, @typescript-eslint/no-implied-eval
  const factory = new Function(body)() as Factory
  factoryCache.set(key, factory)
  return factory
}

function buildGeneric(fields: Field[], generic: Generic[], rowCount: number): unknown[] {
  const out = new Array(rowCount)
  for (let i = 0; i < rowCount; i++) {
    const row: Record<string, unknown> = {}
    for (let k = 0; k < fields.length; k++) {
      row[fields[k].name] = generic[k].get(i)
    }
    out[i] = row
  }
  return out
}

/** Undefined until the first real probe; `allowGenerated` bypasses the cache
 * entirely, which is what lets a test force the CSP fallback without one. */
let generatedAllowed: boolean | undefined

function canUseGenerated(force?: boolean): boolean {
  if (force !== undefined) return force
  if (generatedAllowed === undefined) {
    try {
      // eslint-disable-next-line no-new-func, @typescript-eslint/no-implied-eval
      new Function('return 1')()
      generatedAllowed = true
    } catch {
      generatedAllowed = false
    }
  }
  return generatedAllowed
}

export type ReadRowsOptions = {
  /** Forces the CSP fallback path (or forces it off) regardless of what this
   * page's own CSP allows — for testing the fallback without one. */
  allowGenerated?: boolean
}

/** One root table: its name (empty for an envelope root), rows and runs. */
type TableRuns = { name: string; rowCount: number; fields: Field[]; runs: Run[] }

/** The whole buffer: its shape and every table in it, in buffer order. */
function readBuffer(buffer: ArrayBuffer): { shape: number; tables: TableRuns[] } {
  const view = new DataView(buffer)
  const version = view.getUint32(0, true)
  if (version !== VERSION) {
    throw new Error(`materialize: unknown buffer version ${version}`)
  }
  const shape = view.getUint32(4, true)
  if (shape !== SHAPE_ARRAY && shape !== SHAPE_OBJECT) {
    throw new Error(`materialize: unknown buffer shape ${shape}`)
  }
  const tableCount = view.getUint32(8, true)
  if (shape === SHAPE_ARRAY && tableCount !== 1) {
    throw new Error(`materialize: an array root holds one table, not ${tableCount}`)
  }

  const nameDecoder = new TextDecoder()
  const tables: TableRuns[] = []
  let at = 12
  for (let t = 0; t < tableCount; t++) {
    const tableNameLen = view.getUint16(at, true)
    const name = nameDecoder.decode(new Uint8Array(buffer, at + 2, tableNameLen))
    at = pad8(at + 2 + tableNameLen)

    const rowCount = view.getUint32(at, true)
    const fieldCount = view.getUint32(at + 4, true)
    at += 8
    const fields: Field[] = []
    for (let i = 0; i < fieldCount; i++) {
      const kind = view.getUint8(at)
      const flags = view.getUint8(at + 1)
      const nameLen = view.getUint16(at + 2, true)
      const fieldName = nameDecoder.decode(new Uint8Array(buffer, at + 4, nameLen))
      at += 4 + nameLen
      fields.push({
        name: fieldName,
        col: classify(kind, flags),
        nullable: (flags & FLAG_NULLABLE) !== 0,
      })
    }
    const read = readRuns(buffer, pad8(at), rowCount, fields)
    at = read.at
    tables.push({ name, rowCount, fields, runs: read.runs })
  }
  return { shape, tables }
}

function buildRows(table: TableRuns, options: ReadRowsOptions): unknown[] {
  const { rowCount, fields, runs } = table
  if (canUseGenerated(options.allowGenerated)) {
    try {
      return getFactory(fields)(rowCount, ...argsOf(runs))
    } catch {
      // A signature this engine refused to compile mid-session (a CSP that
      // only some navigations enforce, say) — fall through to the generic path.
    }
  }
  return buildGeneric(fields, genericOf(runs), rowCount)
}

/**
 * The materialize buffer to row objects: the array for a root that was a bare
 * array, or an object with one array per table field, in the order the message
 * wrote them — the value the JSON path would have produced.
 *
 * `buffer` should be a copy the caller owns outright (not a live view into
 * wasm memory) — this constructs several typed-array views over it at
 * differing offsets, and a later `alloc()` growing the module's memory would
 * detach every one of them.
 */
export function readRows(
  buffer: ArrayBuffer,
  options: ReadRowsOptions = {},
): unknown[] | Record<string, unknown[]> {
  const { shape, tables } = readBuffer(buffer)
  if (shape === SHAPE_ARRAY) return buildRows(tables[0], options)
  const out: Record<string, unknown[]> = {}
  for (const table of tables) out[table.name] = buildRows(table, options)
  return out
}

/** What a column's values arrive as. */
export type Column = (
  /** One value per row, unpacked from the bitmap the buffer carries; `null`
   * at a null row. */
  | { name: string; kind: 'bool'; length: number; values: (boolean | null)[] }
  /** Every value fits a `Number` exactly — the column's own range says so. */
  | { name: string; kind: 'int'; length: number; values: Float64Array }
  /** Some value is past 2^53, so the column is carried whole rather than
   * rounded. A view over the buffer's own bytes. */
  | { name: string; kind: 'bigint'; length: number; values: BigInt64Array | BigUint64Array }
  /** A view over the buffer's own bytes. */
  | { name: string; kind: 'float'; length: number; values: Float64Array }
  /** `null` at a null row. */
  | { name: string; kind: 'string'; length: number; values: (string | null)[] }
) & {
  /** Present only for a nullable column: bit `i` (`valid[i >> 3] >> (i & 7) & 1`)
   * set means row `i` holds a value. A typed array cannot hold `null`, so an
   * `int`, `bigint` or `float` column has a zero at a null row and this says
   * which zeros are nulls; `bool` and `string` columns also carry `null`. */
  valid?: Uint8Array
}

export type ColumnTable = {
  rowCount: number
  columns: Column[]
  /** The root field the table is, for a message whose root is a struct with a
   * table field; absent for a root that was a bare array. */
  field?: string
}

/**
 * The materialize buffer as columns rather than rows — INTERNALS.md §16.2, for
 * a caller feeding a grid or a chart that never wants row objects.
 *
 * `float` and `bigint` columns are views straight over `buffer`; the rest are
 * built, because a bitmap, a UTF-8 blob and a pair of i32 lanes are not things
 * a chart can read. Building them is cheap — thousandths of a
 * millisecond per thousand rows — and it is still less work than the row build
 * this exists to skip.
 *
 * `null` for a root struct with more than one table: there is no one column
 * table to hand back, and {@link readRows} has all of them.
 */
export function readColumns(buffer: ArrayBuffer): ColumnTable | null {
  const { shape, tables } = readBuffer(buffer)
  if (tables.length !== 1) return null
  const { rowCount, fields, runs } = tables[0]

  const columns = runs.map((run, k): Column => {
    const name = fields[k].name
    const valid = run.valid
    const extra = valid ? { valid } : {}
    switch (run.col) {
      case 'bool': {
        const values = new Array<boolean | null>(rowCount)
        for (let i = 0; i < rowCount; i++) {
          values[i] =
            valid && !isValid(valid, i) ? null : ((run.bitmap[i >> 3] >> (i & 7)) & 1) === 1
        }
        return { name, kind: 'bool', length: rowCount, values, ...extra }
      }
      case 'intSafe': {
        const values = new Float64Array(rowCount)
        for (let i = 0; i < rowCount; i++) values[i] = run.hi[2 * i + 1] * 4294967296 + run.lo[2 * i]
        return { name, kind: 'int', length: rowCount, values, ...extra }
      }
      case 'intBigSigned':
      case 'intBigUnsigned':
        return { name, kind: 'bigint', length: rowCount, values: run.view, ...extra }
      case 'float':
        return { name, kind: 'float', length: rowCount, values: run.view, ...extra }
      case 'stringAscii':
      case 'stringUtf8': {
        const read = genericOf([run])[0]
        const values = new Array<string | null>(rowCount)
        for (let i = 0; i < rowCount; i++) values[i] = read.get(i) as string | null
        return { name, kind: 'string', length: rowCount, values, ...extra }
      }
    }
  })

  return shape === SHAPE_OBJECT ? { rowCount, columns, field: tables[0].name } : { rowCount, columns }
}
