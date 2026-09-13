import { base } from '$app/paths'

// The WebAssembly module, compiled once and instantiated per operation.
//
// PLAN.md §2.2: a shared instance whose allocator never frees would grow without
// bound on a page where someone encodes fifty times. Each instance gets its own
// memory instead, a module this size instantiates in well under a millisecond,
// and an input large enough to exhaust memory takes its own instance down rather
// than the page.
//
// # The schema travels out of band
//
// Which is the delivery the format advises and the one this page should show: a
// message carries no schema, and `section()` hands back the one the encode
// resolved. `encodeSelfDescribing` is the other shape, for the file the download
// button writes — a document with nowhere to put a section has to carry its own.

export type Diagnostic = {
  code: number
  offset: number
  line: number
  path: string
  message: string
  warnings: string[]
}

/** One node of the field tree: a field, a table's column, or a list's element. */
export type Field = {
  name: string
  key: number
  /** The wire shape — `int64`, `string`, `[]struct`, `float64 column`. */
  type: string
  /** A pointer field: the format's "absent, rather than zero". */
  optional: boolean
  /** Absolute offsets into the message, so the hex view needs no arithmetic. */
  start: number
  end: number
  bytes: number
  children: Field[]
}

export type Report = {
  totalBytes: number
  /** The schema section, when the message carries one. Zero out of band. */
  schemaBytes: number
  bodyBytes: number
  rootBytes: number
  /** Whether the run uses eight-bit keys, which costs a byte per present field. */
  wide: boolean
  /** The row count of the root's list or table, or 1 for a lone record. */
  rows: number
  /** Whether the document was wrapped, because its top level was not an object. */
  envelope: boolean
  fields: Field[]
}

/** Encode flags, mirroring assembly/index.ts. */
const SELF_DESCRIBING = 1
const VERIFY = 2

type Exports = {
  memory: WebAssembly.Memory
  alloc(n: number): number
  resultPtr(): number
  encode(len: number, flags: number): number
  section(): number
  setSchema(len: number): number
  decode(len: number): number
  inspectMessage(len: number): number
  lastError(): number
}

let compiled: Promise<WebAssembly.Module> | undefined

function compile(): Promise<WebAssembly.Module> {
  compiled ??= (async () => {
    const response = await fetch(`${base}/colbin.wasm`)
    if (!response.ok) throw new Error(`colbin.wasm: ${response.status}`)
    // compileStreaming needs the right content type; some static hosts do not
    // set it, so fall back rather than fail on a header.
    if (response.headers.get('content-type')?.includes('application/wasm')) {
      return WebAssembly.compileStreaming(response.clone())
    }
    return WebAssembly.compile(await response.arrayBuffer())
  })()
  return compiled
}

/** Warms the module so the first keystroke is not the one that waits for it. */
export function preload(): void {
  void compile().catch(() => {})
}

function instantiate(module: WebAssembly.Module): Exports {
  const instance = new WebAssembly.Instance(module, {
    env: {
      // The module is written not to abort; if one ever fires, it is a bug here
      // rather than bad input, and it should be loud.
      abort(_msg: number, _file: number, line: number, column: number) {
        throw new Error(`colbin.wasm aborted at ${line}:${column}`)
      },
    },
  })
  return instance.exports as unknown as Exports
}

/**
 * A copy of the module's output.
 *
 * The view is built here rather than kept anywhere, and that is load-bearing: a
 * call that grows the module's memory detaches every existing ArrayBuffer view,
 * so a cached one throws on next use. It only bites above the initial memory
 * size, which is why an example with a thousand records is the one that would
 * find it.
 */
function bytesOf(e: Exports, length: number): Uint8Array {
  const at = e.resultPtr()
  return new Uint8Array(e.memory.buffer).slice(at, at + length)
}

function readError(e: Exports): Diagnostic {
  const length = e.lastError()
  const text = new TextDecoder().decode(bytesOf(e, length))
  return JSON.parse(text) as Diagnostic
}

function write(e: Exports, input: Uint8Array): number {
  // alloc first, then take the view: allocating may grow memory, which detaches
  // any view taken before it.
  const ptr = e.alloc(input.length)
  new Uint8Array(e.memory.buffer).set(input, ptr)
  return input.length
}

export type Outcome<T> =
  | { ok: true; value: T; warnings: string[] }
  | { ok: false; error: Diagnostic }

const encoder = new TextEncoder()
const decoder = new TextDecoder()

/** A message and the schema section that describes it. */
export type Encoded = {
  message: Uint8Array
  section: Uint8Array
  /** The same document with its section in front of it, which is what a file
   * that has to stand alone needs. */
  standalone: Uint8Array
}

/**
 * JSON text to a colbin message.
 *
 * Both deliveries come back from one call, because the page shows both numbers
 * and encoding twice to get them would make the timing a lie.
 */
export async function encode(json: string, verify = true): Promise<Outcome<Encoded>> {
  const e = instantiate(await compile())
  const input = encoder.encode(json)

  const flags = verify ? VERIFY : 0
  const messageLen = e.encode(write(e, input), flags)
  if (messageLen < 0) return { ok: false, error: readError(e) }
  const message = bytesOf(e, messageLen)
  const section = bytesOf(e, e.section())

  const standaloneLen = e.encode(write(e, input), flags | SELF_DESCRIBING)
  if (standaloneLen < 0) return { ok: false, error: readError(e) }
  const standalone = bytesOf(e, standaloneLen)

  // Warnings live in the same envelope as errors, and a successful encode can
  // still have something to say (an always-null column, a null where an object
  // belongs).
  const warnings = readError(e).warnings
  return { ok: true, value: { message, section, standalone }, warnings }
}

/**
 * A colbin message back to JSON text.
 *
 * `section` is the schema, for the out-of-band delivery. Pass none for a
 * message that carries its own — which is what a dropped file will be.
 */
export async function decode(
  message: Uint8Array,
  section?: Uint8Array,
): Promise<Outcome<string>> {
  const e = instantiate(await compile())
  if (section && section.length > 0) {
    if (e.setSchema(write(e, section)) < 0) return { ok: false, error: readError(e) }
  }
  const length = e.decode(write(e, message))
  if (length < 0) return { ok: false, error: readError(e) }
  return { ok: true, value: decoder.decode(bytesOf(e, length)), warnings: [] }
}

/** The field tree of a message, with every node's byte span. */
export async function inspect(
  message: Uint8Array,
  section?: Uint8Array,
): Promise<Outcome<Report>> {
  const e = instantiate(await compile())
  if (section && section.length > 0) {
    if (e.setSchema(write(e, section)) < 0) return { ok: false, error: readError(e) }
  }
  const length = e.inspectMessage(write(e, message))
  if (length < 0) return { ok: false, error: readError(e) }
  return {
    ok: true,
    value: JSON.parse(decoder.decode(bytesOf(e, length))) as Report,
    warnings: [],
  }
}
