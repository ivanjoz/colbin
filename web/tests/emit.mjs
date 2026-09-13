// Writes what the module encodes, for the Go decoder to read.
//
// This is the direction byte-for-byte vectors cannot test. The corpus in
// web/vectors/vectors.json goes Go -> module: Go writes the bytes and the module
// must reproduce and read them. This goes module -> Go, which is the property a
// caller actually has: a JavaScript client encodes, a Go service decodes, and
// nothing in between shares an implementation.
//
//   node tests/emit.mjs            # regenerate
//   go test ./web/vectors          # fail if Go cannot read it
//
// It is the same discipline `cargo run --example emit_vectors` keeps for the
// Rust port, and it lands in the same place: a committed file, regenerated in
// CI, with a diff check.

import { writeFile } from 'node:fs/promises'
import { join } from 'node:path'
import { load, root } from './harness.mjs'
import { documents, textOf } from './documents.mjs'

const wasm = await load()
const encoder = new TextEncoder()
const decoder = new TextDecoder()
const VERIFY = 2

function feed(text) {
  const bytes = encoder.encode(text)
  wasm.u8.set(bytes, wasm.exports.inPtr())
  return bytes.length
}

function out(length) {
  const at = wasm.exports.outPtr()
  return wasm.u8.slice(at, at + length)
}

const hex = (bytes) => Buffer.from(bytes).toString('hex')

const PACK_STRINGS = 4

// Every document twice: raw strings, which is what an ordinary encode writes,
// and packed, which is what a size-bound caller asks for. Go has to read both,
// and the packed half is the one no byte-for-byte corpus covers — there the
// module is the encoder rather than the copy.
const runs = [
  { suffix: '', flags: VERIFY },
  { suffix: ' (packed)', flags: VERIFY | PACK_STRINGS },
]

const cases = []
for (const document of documents) {
 for (const run of runs) {
  const text = textOf(document)
  const length = wasm.exports.encodeJSON(feed(text), run.flags)
  if (length < 0) {
    const why = decoder.decode(out(wasm.exports.lastMessage()))
    throw new Error(`${document.name}: ${why}`)
  }
  const message = out(length)
  const section = out(wasm.exports.lastSection())

  // What the module itself renders the message back as. Go has to agree with
  // this up to the one difference the envelope makes, which the Go side knows
  // about and checks for by name.
  wasm.u8.set(section, wasm.exports.inPtr())
  wasm.exports.setSection(section.length)
  wasm.u8.set(message, wasm.exports.inPtr())
  const decoded = wasm.exports.messageJSON(message.length)
  if (decoded < 0) throw new Error(`${document.name}: the module cannot read its own message`)

  cases.push({
    name: document.name + run.suffix,
    input: text,
    section: hex(section),
    message: hex(message),
    json: decoder.decode(out(decoded)),
  })
 }
}

const path = join(root, 'vectors/web_encoded.json')
await writeFile(path, `${JSON.stringify({ cases }, null, 2)}\n`)
console.log(`wrote ${path}`)
