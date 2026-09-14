import { readFile } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'

const here = dirname(fileURLToPath(import.meta.url))
export const root = join(here, '..')

/**
 * The module the package ships, not the one cargo last happened to write.
 *
 * `bun run wasm` copies the release artifact here, and the package loads this
 * exact file. Reading the cargo target directory instead would pass whatever
 * was built there last — a `--no-default-features` decode-only module, say,
 * whose missing `encode` export surfaces as `is not a function` several
 * frames from the cause.
 */
export const wasmPath = join(root, 'build', 'colbin.wasm')

/**
 * The other module: the same build with the field-tree walk in it.
 *
 * `inspect` is off by default (`rust/wasm/Cargo.toml`), because the span walk
 * is 15 KB that only a tool drawing the bytes calls — so the module the
 * package publishes has no `inspect_message` export at all, and the tests that
 * assert the spans tile have to ask for the build that does. Everything else
 * keeps running against the published bytes, which is the point of loading
 * from `build/` rather than from cargo's target directory.
 */
export const wasmInspectPath = join(root, 'build', 'colbin.inspect.wasm')

/**
 * Instantiates the Rust module the way the page does: no imports.
 *
 * `load({ inspect: true })` takes the build with the span walk; the default is
 * the one that ships.
 */
export async function load({ inspect = false } = {}) {
  const path = inspect ? wasmInspectPath : wasmPath
  const bytes = await readFile(path).catch((err) => {
    throw new Error(`no module at ${path}: run \`bun run wasm\` (${err.code})`)
  })
  const { instance } = await WebAssembly.instantiate(bytes, {})
  const e = instance.exports
  return {
    exports: e,
    memory: e.memory,
    get view() {
      return new DataView(e.memory.buffer)
    },
    get u8() {
      return new Uint8Array(e.memory.buffer)
    },
  }
}

/**
 * Writes bytes into the module the safe way round: alloc first, then take the
 * view, because allocating may grow memory and detach every view taken before it.
 */
export function put(wasm, bytes) {
  const at = wasm.exports.alloc(bytes.length)
  new Uint8Array(wasm.exports.memory.buffer).set(bytes, at)
  return bytes.length
}

/** A copy of the module's last output. */
export function take(wasm, length) {
  const at = wasm.exports.result_ptr()
  return new Uint8Array(wasm.exports.memory.buffer).slice(at, at + length)
}

export function takeText(wasm, length) {
  return new TextDecoder().decode(take(wasm, length))
}

/** The last diagnostic. Overwrites the last successful output. */
export function failure(wasm) {
  return takeText(wasm, wasm.exports.last_error())
}

/**
 * Throws with the diagnostic if `length` is negative. Template-string
 * `assert.ok(n >= 0, failure())` is unsafe: the message is evaluated on
 * success too, and `last_error` overwrites the output we were about to copy.
 */
export function must(wasm, length, label = 'call') {
  if (length < 0) throw new Error(`${label}: ${failure(wasm)}`)
  return length
}

/**
 * One tier of the corpus, by the name the generator gave it.
 *
 * The file is committed rather than generated into a gitignored directory, and
 * regenerated in CI with a diff check -- the discipline rust/vectors keeps. So
 * a Go-side format change shows up as a reviewable diff of the bytes, and a
 * stale corpus fails `go test ./js/vectors` on the Go side's own push rather
 * than quietly passing here.
 */
export async function vectors(tier) {
  const held = JSON.parse(await readFile(join(root, 'vectors/vectors.json'), 'utf8'))
  if (!(tier in held)) {
    throw new Error(`no tier ${tier}: run \`go run ./js/vectors\``)
  }
  return held[tier]
}

export const hex = (bytes) => Buffer.from(bytes).toString('hex')
export const unhex = (s) => Uint8Array.from(Buffer.from(s, 'hex'))
