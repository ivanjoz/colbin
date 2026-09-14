// `colbin/inspect` — the module built with the field-tree walk in it.
//
// A second `.wasm`, not a second copy of the wrapper: `codec.inspect()` is the
// only thing that needs it, and it is 15 KB of code (6.6% of the module) that
// nothing else calls. Every other entry ships the build without it, so a
// consumer who only reads messages does not download a span walk to never run
// — and a consumer who *is* writing a tool that draws the bytes gets it from
// npm rather than having to build Rust.
//
// | | raw | gzipped |
// |---|---:|---:|
// | `colbin` / `colbin/asset` | 209 109 B | 84 190 B |
// | `colbin/inspect` | 224 199 B | 90 114 B |
//
// Resolved as an asset, like `colbin/asset` and for the same reasons — a tool
// that wants the field tree is a tool, and 90 KB inline in its bundle is not
// what it wants.

import { codecFor, convenience, Codec as CodecClass, type WasmSource } from './core.ts'

export * from './shared.ts'

/** Where the inspect-capable module is, as the bundler resolved it. */
export const wasmURL = new URL('./colbin.inspect.wasm', import.meta.url)

function fetched(): WasmSource {
  return fetch(wasmURL)
}

/** The handle, over a module that has `inspect`. Otherwise identical to the
 * one every other entry exports. */
export const Codec = codecFor(fetched)

/** The same name as a type, for `let codec: Codec`. */
export type Codec = CodecClass

export const { unmarshal, columns, toJSONText, marshal, inspect } = convenience(Codec)
