// What the module costs, end to end.
//
//   node --experimental-strip-types tests/bench.mjs
//
// The corpus is the one PLAN.md's appendix measures: a thousand product
// records, seven fields, 107 KB of minified JSON. Keeping it means the numbers
// there can be compared against the numbers here rather than re-argued.
//
// Both modules are timed, because they answer different questions. The release
// build is what ships and what the page runs. The debug build carries the test
// exports, which is the only way to time the scanner on its own — and the split
// is what says whether a slow encode is the codec or the parse in front of it.
//
// Timing inside a browser-shaped runtime is noisy, so each figure is the best of
// twelve runs on a fresh instance. Treat them as indicative: this laptop
// throttles, and the point of the split is the ratios rather than the absolutes.

import { readFile } from 'node:fs/promises'
import { join } from 'node:path'
import { root } from './harness.mjs'

const encoder = new TextEncoder()

const names = ['Tin Light', 'Steel Lamp', 'Copper Wire', 'Brass Hinge', 'Zinc Plate']
const cities = ['Lima', 'Arequipa', 'Trujillo', 'Cusco', 'Piura']
const text = JSON.stringify(
  Array.from({ length: 1000 }, (_, i) => ({
    id: 100000 + i * 7,
    sku: `SKU-${String(i).padStart(5, '0')}`,
    name: names[i % 5],
    city: cities[i % 5],
    price: 599 + i * 13,
    stock: i % 97,
    active: i % 3 !== 0,
  })),
  null,
  2,
)
const input = encoder.encode(text)

async function compile(file) {
  return WebAssembly.compile(await readFile(join(root, 'build', file)))
}

function fresh(module) {
  const instance = new WebAssembly.Instance(module, {
    env: {
      abort(_msg, _file, line, column) {
        throw new Error(`colbin.wasm aborted at ${line}:${column}`)
      },
    },
  })
  return instance.exports
}

/** The best of twelve, each on an instance that has never run before. */
function best(module, call, at = (e) => e.alloc(input.length)) {
  const times = []
  for (let round = 0; round < 12; round++) {
    const e = fresh(module)
    // The offset first, then the view: allocating may grow memory, which
    // detaches every view taken before it. The same hazard src/lib/codec.ts
    // documents, and it bites here for the same reason.
    const offset = at(e)
    new Uint8Array(e.memory.buffer).set(input, offset)
    const started = performance.now()
    const got = call(e)
    times.push(performance.now() - started)
    if (got < 0) throw new Error('the module refused the corpus')
  }
  return times.sort((a, b) => a - b)[0]
}

const release = await compile('colbin.wasm')
const debug = await compile('test.wasm')

// One encode, to get something to decode.
const held = fresh(release)
const ptr = held.alloc(input.length)
new Uint8Array(held.memory.buffer).set(input, ptr)
const messageLen = held.encode(input.length, 0)
const message = new Uint8Array(held.memory.buffer).slice(
  held.resultPtr(),
  held.resultPtr() + messageLen,
)
const sectionLen = held.section()
const section = new Uint8Array(held.memory.buffer).slice(
  held.resultPtr(),
  held.resultPtr() + sectionLen,
)

function decodeOnce() {
  const e = fresh(release)
  let at = e.alloc(section.length)
  new Uint8Array(e.memory.buffer).set(section, at)
  e.setSchema(section.length)
  at = e.alloc(message.length)
  new Uint8Array(e.memory.buffer).set(message, at)
  const started = performance.now()
  const got = e.decode(message.length)
  const took = performance.now() - started
  if (got < 0) throw new Error('the module cannot read its own message')
  return took
}

const decodes = []
for (let round = 0; round < 12; round++) decodes.push(decodeOnce())
decodes.sort((a, b) => a - b)

const scanner = best(debug, (e) => e.jsonParse(input.length), (e) => e.inPtr())
const encode = best(release, (e) => e.encode(input.length, 0))
const verified = best(release, (e) => e.encode(input.length, 2))

const minified = JSON.stringify(JSON.parse(text)).length
const pad = (n, w = 6) => n.toFixed(2).padStart(w)

console.log(`corpus            1000 product records, ${minified} B minified`)
console.log(`message           ${message.length} B, section ${section.length} B, ${(minified / message.length).toFixed(2)}x`)
console.log('')
console.log(`scanner alone     ${pad(scanner)} ms   (debug build; the release one has no such export)`)
console.log(`encode            ${pad(encode)} ms   parse + infer + build`)
console.log(`encode + verify   ${pad(verified)} ms   +${((verified / encode - 1) * 100).toFixed(0)}% for the self-check`)
console.log(`decode            ${pad(decodes[0])} ms`)
