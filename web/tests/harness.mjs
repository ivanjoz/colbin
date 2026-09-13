import { readFile } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'

const here = dirname(fileURLToPath(import.meta.url))
export const root = join(here, '..')

/** Instantiates the test module. abort throws so a trap is visible as a failure. */
export async function load() {
  const bytes = await readFile(join(root, 'build/test.wasm'))
  const { instance } = await WebAssembly.instantiate(bytes, {
    env: {
      abort(_msg, _file, line, col) {
        throw new Error(`AssemblyScript abort at ${line}:${col}`)
      },
    },
  })
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
 * One tier of the corpus, by the name the generator gave it.
 *
 * The file is committed rather than generated into a gitignored directory, and
 * regenerated in CI with a diff check -- the discipline rust/vectors keeps. So
 * a Go-side format change shows up as a reviewable diff of the bytes, and a
 * stale corpus fails `go test ./web/vectors` on the Go side's own push rather
 * than quietly passing here.
 */
export async function vectors(tier) {
  const held = JSON.parse(await readFile(join(root, 'vectors/vectors.json'), 'utf8'))
  if (!(tier in held)) {
    throw new Error(`no tier ${tier}: run \`go run ./web/vectors\``)
  }
  return held[tier]
}

export const hex = (bytes) => Buffer.from(bytes).toString('hex')
export const unhex = (s) => Uint8Array.from(Buffer.from(s, 'hex'))
