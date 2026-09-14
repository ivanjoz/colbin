// Writes what the module encodes, for the Go decoder to read.
//
//   node tests/emit.mjs            # regenerate
//   go test ./js/vectors          # fail if Go cannot read it

import { writeFile } from 'node:fs/promises'
import { join } from 'node:path'
import { load, put, take, takeText, root } from './harness.mjs'
import { documents, textOf } from './documents.mjs'

const wasm = await load()
const encoder = new TextEncoder()
const VERIFY = 2
const PACK_STRINGS = 4

function feed(text) {
  return put(wasm, encoder.encode(text))
}

const hex = (bytes) => Buffer.from(bytes).toString('hex')

const runs = [
  { suffix: '', flags: VERIFY },
  { suffix: ' (packed)', flags: VERIFY | PACK_STRINGS },
]

const cases = []
for (const document of documents) {
  for (const run of runs) {
    const text = textOf(document)
    const length = wasm.exports.encode(feed(text), run.flags)
    if (length < 0) {
      throw new Error(`${document.name}: ${takeText(wasm, wasm.exports.last_error())}`)
    }
    const message = take(wasm, length)
    const section = take(wasm, wasm.exports.section())

    wasm.exports.set_schema(put(wasm, section))
    const decoded = wasm.exports.decode(put(wasm, message))
    if (decoded < 0) throw new Error(`${document.name}: the module cannot read its own message`)

    cases.push({
      name: document.name + run.suffix,
      input: text,
      section: hex(section),
      message: hex(message),
      json: takeText(wasm, decoded),
    })
  }
}

const path = join(root, 'vectors/web_encoded.json')
await writeFile(path, `${JSON.stringify({ cases }, null, 2)}\n`)
console.log(`wrote ${path}`)
