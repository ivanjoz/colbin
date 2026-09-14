// Reads the flat buffer `colbin::materialize::to_buffer` writes and turns it
// into row objects without going through JSON text — PACKAGE_PLAN.md §5.
//
// The buffer has no offset table: every run's byte length is derivable from
// `rowCount` and the field's own kind, so this walks it once, in field order,
// the same way the Rust writer laid it out. See `rust/src/materialize.rs`'s
// module doc for the exact layout.

const KIND_BOOL = 0
const KIND_INT = 1
const KIND_FLOAT = 2
const KIND_STRING = 3

const FLAG_SIGNED = 1 << 0
const FLAG_EXCEEDS_SAFE_INT = 1 << 1
const FLAG_ASCII = 1 << 2

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

type Field = { name: string; col: ColumnKind }

/** A uniform per-row accessor, for the CSP fallback path only — the generated
 * builder never calls this, since a function call per cell is exactly the
 * cost §2.1 measured a monomorphic object literal beating by 10x. */
type Generic = { get(row: number): unknown }

/**
 * One field's data, as views over the buffer's bytes. Every reader below —
 * the generated row builder's arguments, the fallback's accessors, and the
 * public column table — is derived from these, so the cursor arithmetic that
 * walks the runs is written once.
 */
type Run =
  | { col: 'bool'; bitmap: Uint8Array }
  | { col: 'intSafe'; hi: Int32Array; lo: Uint32Array }
  | { col: 'intBigSigned'; view: BigInt64Array }
  | { col: 'intBigUnsigned'; view: BigUint64Array }
  | { col: 'float'; view: Float64Array }
  | { col: 'stringAscii'; text: string; offsets: Uint32Array }
  | { col: 'stringUtf8'; blob: Uint8Array; offsets: Uint32Array }

function pad8(at: number): number {
  return Math.ceil(at / 8) * 8
}

/** Reads every field's run at the current cursor, in header order. */
function readRuns(buffer: ArrayBuffer, start: number, rowCount: number, fields: Field[]): Run[] {
  const runs: Run[] = []
  let at = start

  for (const field of fields) {
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
        // exceeds flag guarantees for this column kind. §2.3.
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
        // code unit offsets exactly when the blob is ASCII. §5.3.
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
    at = pad8(at)
  }
  return runs
}

/** The flattened argument list the generated builder wants: one or two typed
 * arrays per field, in field order, matching `getFactory`'s parameter list. */
function argsOf(runs: Run[]): unknown[] {
  const args: unknown[] = []
  for (const run of runs) {
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
  })
}

type Factory = (rowCount: number, ...args: unknown[]) => unknown[]

const factoryCache = new Map<string, Factory>()

function signatureOf(fields: Field[]): string {
  return fields.map((f) => `${f.col}:${f.name}`).join('|')
}

/**
 * A generated, monomorphic row builder for this exact field signature,
 * compiled once and cached — §2.1 measures this at 10x a generic `for key in
 * schema` loop, because V8 gives every row the same hidden class.
 */
function getFactory(fields: Field[]): Factory {
  const key = signatureOf(fields)
  const cached = factoryCache.get(key)
  if (cached) return cached

  const params: string[] = []
  const entries: string[] = []
  fields.forEach((field, k) => {
    const prop = JSON.stringify(field.name)
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

/** The buffer's header: how many rows, what the fields are, and where the
 * first run starts. */
function readHeader(buffer: ArrayBuffer): { rowCount: number; fields: Field[]; at: number } {
  const view = new DataView(buffer)
  const version = view.getUint32(0, true)
  if (version !== 1) {
    throw new Error(`materialize: unknown buffer version ${version}`)
  }
  const rowCount = view.getUint32(4, true)
  const fieldCount = view.getUint32(8, true)

  const nameDecoder = new TextDecoder()
  let at = 12
  const fields: Field[] = []
  for (let i = 0; i < fieldCount; i++) {
    const kind = view.getUint8(at)
    const flags = view.getUint8(at + 1)
    const nameLen = view.getUint16(at + 2, true)
    const name = nameDecoder.decode(new Uint8Array(buffer, at + 4, nameLen))
    at += 4 + nameLen
    fields.push({ name, col: classify(kind, flags) })
  }
  return { rowCount, fields, at: pad8(at) }
}

/**
 * The materialize buffer to row objects.
 *
 * `buffer` should be a copy the caller owns outright (not a live view into
 * wasm memory) — this constructs several typed-array views over it at
 * differing offsets, and a later `alloc()` growing the module's memory would
 * detach every one of them.
 */
export function readRows(buffer: ArrayBuffer, options: ReadRowsOptions = {}): unknown[] {
  const { rowCount, fields, at } = readHeader(buffer)
  const runs = readRuns(buffer, at, rowCount, fields)

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

/** What a column's values arrive as. */
export type Column =
  /** One value per row, unpacked from the bitmap the buffer carries. */
  | { name: string; kind: 'bool'; length: number; values: boolean[] }
  /** Every value fits a `Number` exactly — the column's own range says so. */
  | { name: string; kind: 'int'; length: number; values: Float64Array }
  /** Some value is past 2^53, so the column is carried whole rather than
   * rounded. A view over the buffer's own bytes. */
  | { name: string; kind: 'bigint'; length: number; values: BigInt64Array | BigUint64Array }
  /** A view over the buffer's own bytes. */
  | { name: string; kind: 'float'; length: number; values: Float64Array }
  | { name: string; kind: 'string'; length: number; values: string[] }

export type ColumnTable = { rowCount: number; columns: Column[] }

/**
 * The materialize buffer as columns rather than rows — PACKAGE_PLAN.md §6, for
 * a caller feeding a grid or a chart that never wants row objects.
 *
 * `float` and `bigint` columns are views straight over `buffer`; the rest are
 * built, because a bitmap, a UTF-8 blob and a pair of i32 lanes are not things
 * a chart can read. Building them is the cheap end of §2.3 — thousandths of a
 * millisecond per thousand rows — and it is still less work than the row build
 * this exists to skip.
 */
export function readColumns(buffer: ArrayBuffer): ColumnTable {
  const { rowCount, fields, at } = readHeader(buffer)
  const runs = readRuns(buffer, at, rowCount, fields)

  const columns = runs.map((run, k): Column => {
    const name = fields[k].name
    switch (run.col) {
      case 'bool': {
        const values = new Array<boolean>(rowCount)
        for (let i = 0; i < rowCount; i++) values[i] = ((run.bitmap[i >> 3] >> (i & 7)) & 1) === 1
        return { name, kind: 'bool', length: rowCount, values }
      }
      case 'intSafe': {
        const values = new Float64Array(rowCount)
        for (let i = 0; i < rowCount; i++) values[i] = run.hi[2 * i + 1] * 4294967296 + run.lo[2 * i]
        return { name, kind: 'int', length: rowCount, values }
      }
      case 'intBigSigned':
      case 'intBigUnsigned':
        return { name, kind: 'bigint', length: rowCount, values: run.view }
      case 'float':
        return { name, kind: 'float', length: rowCount, values: run.view }
      case 'stringAscii':
      case 'stringUtf8': {
        const read = genericOf([run])[0]
        const values = new Array<string>(rowCount)
        for (let i = 0; i < rowCount; i++) values[i] = read.get(i) as string
        return { name, kind: 'string', length: rowCount, values }
      }
    }
  })

  return { rowCount, columns }
}
