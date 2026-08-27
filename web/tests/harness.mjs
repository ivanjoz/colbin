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

export async function vectors(name) {
  return JSON.parse(await readFile(join(root, 'tests/vectors', name), 'utf8'))
}

export const hex = (bytes) => Buffer.from(bytes).toString('hex')
export const unhex = (s) => Uint8Array.from(Buffer.from(s, 'hex'))
