// `colbin/asset` — the module as a file the bundler emits, not as base64.
//
// `new URL('./colbin.wasm', import.meta.url)` is the one spelling Vite,
// webpack 5, Rollup and Parcel all understand natively: each of them rewrites
// it to the hashed asset it emitted, and none of them needs a plugin or a
// loader rule to do it. Unbundled in a browser it is simply a relative URL, so
// the same file works from a plain `<script type="module">`.
//
// The cost against the default entry is a second request; the saving is that
// the module is not parsed as JavaScript and is cached by the browser as the
// `application/wasm` it is, which is what lets `compileStreaming` run.

import { codecFor, convenience, Codec as CodecClass, type WasmSource } from './core.ts'

export * from './shared.ts'

/** Where the module is, as the bundler resolved it. Exported so a caller can
 * prefetch it, or hand it to a service worker to cache. */
export const wasmURL = new URL('./colbin.wasm', import.meta.url)

function fetched(): WasmSource {
  return fetch(wasmURL)
}

/** The handle. See `colbin`'s README for the shape. */
export const Codec = codecFor(fetched)

/** The same name as a type, for `let codec: Codec`. */
export type Codec = CodecClass

export const { unmarshal, columns, toJSONText, marshal, inspect } = convenience(Codec)
