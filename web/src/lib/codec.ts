import { base } from '$app/paths'

// The WebAssembly module, compiled once and instantiated per operation.
//
// PLAN.md §2.2: the module is built with the bump-allocator runtime, which
// never frees, so a page where someone encodes fifty times would grow without
// bound on a shared instance. Each instance gets its own memory instead, a
// ~30 KB module instantiates in well under a millisecond, and an input large
// enough to exhaust memory takes its own instance down rather than the page.

export type Diagnostic = {
  code: number
  offset: number
  line: number
  path: string
  message: string
  warnings: string[]
}

export type Column = {
  name: string
  id: number
  type: string
  nullable: boolean
  start: number
  end: number
  bytes: number
  children: Column[]
}

export type Report = {
  totalBytes: number
  schemaBytes: number
  recordCount: number
  shape: 'array' | 'object'
  columns: Column[]
}

type Exports = {
  memory: WebAssembly.Memory
  alloc(n: number): number
  resultPtr(): number
  encode(len: number, verify: number): number
  decode(len: number): number
  inspect(len: number): number
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
  const ptr = e.alloc(input.length)
  new Uint8Array(e.memory.buffer).set(input, ptr)
  return input.length
}

export type Outcome<T> = { ok: true; value: T; warnings: string[] } | { ok: false; error: Diagnostic }

async function run<T>(input: Uint8Array, call: (e: Exports, len: number) => number,
                      read: (e: Exports, out: number) => T): Promise<Outcome<T>> {
  const e = instantiate(await compile())
  const len = write(e, input)
  const out = call(e, len)
  if (out < 0) return { ok: false, error: readError(e) }
  const value = read(e, out)
  // Warnings live in the same envelope as errors, and a successful encode can
  // still have something to say (an all-null column, an always-empty array).
  const warnings = readError(e).warnings
  return { ok: true, value, warnings }
}

const encoder = new TextEncoder()
const decoder = new TextDecoder()

/** JSON text to a colbin JSON-mode message. */
export function encode(json: string, verify = true): Promise<Outcome<Uint8Array>> {
  return run(encoder.encode(json), (e, len) => e.encode(len, verify ? 1 : 0), bytesOf)
}

/** A colbin message back to JSON text. */
export function decode(message: Uint8Array): Promise<Outcome<string>> {
  return run(message, (e, len) => e.decode(len), (e, out) => decoder.decode(bytesOf(e, out)))
}

/** The column tree of a message, with each column's byte span. */
export function inspect(message: Uint8Array): Promise<Outcome<Report>> {
  return run(message, (e, len) => e.inspect(len),
    (e, out) => JSON.parse(decoder.decode(bytesOf(e, out))) as Report)
}
