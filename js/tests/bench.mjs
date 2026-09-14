// What the module costs, end to end.
//
//   node tests/bench.mjs
//
// The corpus is a thousand product records, seven fields. Both encode and
// decode are timed on the release wasm the page ships.

import { readFile } from 'node:fs/promises'
import { wasmPath } from './harness.mjs'
import { readRows } from '../src/materialize.ts'

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

const module = await WebAssembly.compile(await readFile(wasmPath))

function fresh() {
  return new WebAssembly.Instance(module, {}).exports
}

function best(call) {
  const times = []
  for (let round = 0; round < 12; round++) {
    const e = fresh()
    const offset = e.alloc(input.length)
    new Uint8Array(e.memory.buffer).set(input, offset)
    const started = performance.now()
    const got = call(e)
    times.push(performance.now() - started)
    if (got < 0) throw new Error('the module refused the corpus')
  }
  return times.sort((a, b) => a - b)[0]
}

const held = fresh()
const ptr = held.alloc(input.length)
new Uint8Array(held.memory.buffer).set(input, ptr)
const messageLen = held.encode(input.length, 0)
const message = new Uint8Array(held.memory.buffer).slice(
  held.result_ptr(),
  held.result_ptr() + messageLen,
)
const sectionLen = held.section()
const section = new Uint8Array(held.memory.buffer).slice(
  held.result_ptr(),
  held.result_ptr() + sectionLen,
)

function decodeOnce() {
  const e = fresh()
  let at = e.alloc(section.length)
  new Uint8Array(e.memory.buffer).set(section, at)
  e.set_schema(section.length)
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

/** The fast path end to end: wasm's column gather plus the JS row builder —
 * PACKAGE_PLAN.md §5.1's calibration, kept live rather than measured once. */
function materializeOnce() {
  const e = fresh()
  let at = e.alloc(section.length)
  new Uint8Array(e.memory.buffer).set(section, at)
  e.set_schema(section.length)
  at = e.alloc(message.length)
  new Uint8Array(e.memory.buffer).set(message, at)
  const started = performance.now()
  const got = e.materialize(message.length)
  if (got <= 0) throw new Error('the module did not materialize its own table')
  const buffer = e.memory.buffer.slice(e.result_ptr(), e.result_ptr() + got)
  const rows = readRows(buffer)
  const took = performance.now() - started
  if (rows.length !== 1000) throw new Error(`expected 1000 rows, got ${rows.length}`)
  return took
}

const materializes = []
for (let round = 0; round < 12; round++) materializes.push(materializeOnce())
materializes.sort((a, b) => a - b)

const encode = best((e) => e.encode(input.length, 0))
const verified = best((e) => e.encode(input.length, 2))

// V8 doing the same two jobs, in the same process on the same data. It is a
// reference point for reading the numbers above, not a bar the module has to
// clear — `JSON.parse` is also the thing that rounds an integer past 2^53, which
// is why the module scans the text itself.
const value = JSON.parse(text)
const minifiedText = JSON.stringify(value)
const minified = minifiedText.length

function bestOf(call) {
  const times = []
  for (let round = 0; round < 12; round++) {
    const started = performance.now()
    call()
    times.push(performance.now() - started)
  }
  return times.sort((a, b) => a - b)[0]
}

const stringify = bestOf(() => JSON.stringify(value))
const parse = bestOf(() => JSON.parse(minifiedText))

const pad = (n, w = 6) => n.toFixed(2).padStart(w)
const against = (ours, theirs) => `${(ours / theirs).toFixed(2)}x`

console.log(`corpus            1000 product records, ${minified} B minified`)
console.log(`message           ${message.length} B, section ${section.length} B, ${(minified / message.length).toFixed(2)}x`)
console.log('')
console.log(`encode            ${pad(encode)} ms   parse + infer + build`)
console.log(`encode + verify   ${pad(verified)} ms   +${((verified / encode - 1) * 100).toFixed(0)}% for the self-check`)
console.log(`decode            ${pad(decodes[0])} ms`)
console.log(`materialize       ${pad(materializes[0])} ms   wasm gather + JS row builder, no JSON text`)
console.log('')
console.log(`JSON.parse        ${pad(parse)} ms   ${against(encode, parse)} for colbin encode`)
console.log(`JSON.stringify    ${pad(stringify)} ms   ${against(decodes[0], stringify)} for colbin decode`)
console.log(`JSON.parse        ${pad(parse)} ms   ${against(materializes[0], parse)} for colbin materialize`)

// PACKAGE_PLAN.md §10: the regression gate, for CI.
//
// The threshold is a *ratio* against `JSON.parse` measured in the same process
// on the same data, never a millisecond figure — a shared runner's absolute
// speed varies by more than any regression worth catching, and the ratio moves
// only when the module does. It sits far above where the numbers actually land
// (0.5-0.6x on this corpus) because the job here is to catch the JSON-text
// intermediate creeping back into `unmarshal`, which would cost 3-4x, and not
// to litigate a few percent.
if (process.argv.includes('--check')) {
  const ceiling = 1.5
  const ratio = materializes[0] / parse
  console.log('')
  if (ratio > ceiling) {
    console.error(
      `materialize is ${ratio.toFixed(2)}x JSON.parse, over the ${ceiling}x ceiling — ` +
        'PACKAGE_PLAN.md §2 is the whole justification for this design, so this fails the build',
    )
    process.exit(1)
  }
  console.log(`gate: materialize at ${ratio.toFixed(2)}x JSON.parse, under the ${ceiling}x ceiling`)
}
