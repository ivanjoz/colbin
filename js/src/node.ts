// The `node` condition of the package's main entry.
//
// Node resolves `import 'colbin'` here rather than to `index.ts`, and the only
// difference is where the module comes from: the `.wasm` ships in the tarball
// beside this file, so reading it costs one `readFile` and no base64 at all.
// An inlined third of a megabyte would be parsed by V8 on every process start
// for nothing.
//
// `node:fs` is imported dynamically so that a bundler which resolves the
// `node` condition for a browser build — some do, by misconfiguration — fails
// at the point it tries to open a file rather than at import time, and so the
// import never appears in a bundle that does not call `open()`.

import { codecFor, convenience, Codec as CodecClass, type WasmSource } from './core.ts'

export * from './shared.ts'

/** The `.wasm` beside this file: `dist/colbin.wasm` in the tarball. */
export const wasmPath = new URL('./colbin.wasm', import.meta.url)

async function fromDisk(): Promise<WasmSource> {
  const { readFile } = await import('node:fs/promises')
  return readFile(wasmPath)
}

/** The handle. See `colbin`'s README for the shape. */
export const Codec = codecFor(fromDisk)

/** The same name as a type, for `let codec: Codec`. */
export type Codec = CodecClass

export const { unmarshal, columns, toJSONText, marshal, inspect } = convenience(Codec)
