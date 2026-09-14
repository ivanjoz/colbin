// The default entry: the module travels inside the JavaScript.
//
// PACKAGE_PLAN.md §3 — inline base64 is the default because it needs no
// bundler configuration and has no runtime 404 mode. A consumer who would
// rather ship the `.wasm` as an asset imports `colbin/asset`, and one running
// on Node resolves the package's `node` condition instead, which reads the
// file beside itself rather than carrying a third of a megabyte of base64
// through the parser.
//
// The base64 is behind a dynamic `import()`, which is what keeps it out of a
// bundle that never reaches the default source — a page calling
// `Codec.open({ wasm: fetch('/colbin.wasm') })` should not pay for a copy of
// the module it is not using.

import { codecFor, convenience, Codec as CodecClass, type WasmSource } from './core.ts'

export * from './shared.ts'

async function inline(): Promise<WasmSource> {
  const { wasmBase64 } = await import('./generated/wasm-inline.ts')
  return fromBase64(wasmBase64)
}

/**
 * The handle.
 *
 * ```ts
 * import { Codec } from 'colbin'
 * const codec = await Codec.open()
 * const rows = codec.unmarshal(bytes)
 * ```
 */
export const Codec = codecFor(inline)

/** The same name as a type, for `let codec: Codec`. */
export type Codec = CodecClass

export const { unmarshal, columns, toJSONText, marshal, inspect } = convenience(Codec)

function fromBase64(base64: string): Uint8Array {
  // `Uint8Array.fromBase64` is one native pass where the engine has it; `atob`
  // and a copy is the fallback.
  const native = (Uint8Array as unknown as { fromBase64?: (s: string) => Uint8Array }).fromBase64
  if (native) return native(base64)
  const binary = atob(base64)
  const bytes = new Uint8Array(binary.length)
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i)
  return bytes
}
