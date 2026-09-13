// The Rust decoder, driven the way a browser would drive it.
//
//   node rust/wasm/smoke.mjs
//
// Against the same corpus the AssemblyScript module is tested on: every case
// carries a section, a message, and the JSON the module rendered, which
// `go test ./web/vectors` has already agreed with. Byte equality here makes
// three implementations that agree.

import { readFile } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'

const here = dirname(fileURLToPath(import.meta.url))
const root = join(here, '..', '..')

// This crate is its own workspace — a `[profile]` is only honoured at a
// workspace root — so its build lands in `rust/wasm/target`, not the repository
// one. Pointing at the repository one loads whatever was there last, which is a
// pass against a module that is not the one just built.
const wasm = await readFile(
  process.argv[2] ?? join(here, 'target/wasm32-unknown-unknown/release/colbin_wasm.wasm'),
)
const corpus = JSON.parse(await readFile(join(root, 'web/vectors/web_encoded.json'), 'utf8'))
// The dynamic corpus is Go's alone — no third implementation in the loop — which
// is what makes it the pin for `map[string]any`, whose bytes are new.
const dynamic = JSON.parse(await readFile(join(root, 'rust/vectors/vectors.json'), 'utf8'))

const mod = await WebAssembly.compile(wasm)
const dec = new TextDecoder()

function fresh() {
  return new WebAssembly.Instance(mod, {}).exports
}

const unhex = (text) =>
  Uint8Array.from({ length: text.length / 2 }, (_, i) => parseInt(text.slice(i * 2, i * 2 + 2), 16))

/** Writes bytes into the module and returns their length, the safe way round:
 *  alloc first, then take the view, because allocating may grow memory and
 *  detach every view taken before it. */
function put(e, bytes) {
  const at = e.alloc(bytes.length)
  new Uint8Array(e.memory.buffer).set(bytes, at)
  return bytes.length
}

function out(e, length) {
  const at = e.result_ptr()
  return dec.decode(new Uint8Array(e.memory.buffer, at, length))
}

let pass = 0
const failures = []

for (const item of corpus.cases) {
  const section = unhex(item.section)
  const message = unhex(item.message)

  // Both deliveries, because they take different paths through decode():
  // out-of-band holds the section across calls, self-describing reads it out of
  // the message. The corpus messages are the out-of-band shape.
  const e = fresh()
  if (e.set_schema(put(e, section)) < 0) {
    failures.push(`${item.name}: set_schema refused — ${out(e, e.last_error())}`)
    continue
  }
  const length = e.decode(put(e, message))
  if (length < 0) {
    failures.push(`${item.name}: decode refused — ${out(e, e.last_error())}`)
    continue
  }
  const got = out(e, length)
  if (got !== item.json) {
    failures.push(`${item.name}:\n    want ${item.json}\n    got  ${got}`)
    continue
  }
  pass++
}

// The dynamic shapes, through both doors. A `map[string]any` encodes differently
// with a section held than it does standing alone — a record inside it is a type
// tag in one and a map of field names in the other — so both have to arrive at
// the same document, and `decode()` reaches them by different code.
let dynamicPass = 0
for (const item of dynamic.walks) {
  const want = JSON.parse(item.json)

  for (const [delivery, run] of [
    [
      'out of band',
      (e) => {
        if (e.set_schema(put(e, unhex(item.section))) < 0) return -1
        return e.decode(put(e, unhex(item.message)))
      },
    ],
    ['self-describing', (e) => e.decode(put(e, unhex(item.selfDescribing)))],
  ]) {
    const e = fresh()
    const length = run(e)
    if (length < 0) {
      failures.push(`${item.name} (${delivery}): refused — ${out(e, e.last_error())}`)
      continue
    }
    const got = out(e, length)
    // Compared as values: the fields a message omitted are written last, and the
    // two deliveries omit different ones.
    if (JSON.stringify(JSON.parse(got)) !== JSON.stringify(want)) {
      failures.push(`${item.name} (${delivery}):\n    want ${item.json}\n    got  ${got}`)
      continue
    }
    dynamicPass++
  }
}

// A message with no schema set must be refused with something a caller can read,
// rather than trapping.
{
  const e = fresh()
  const message = unhex(corpus.cases[0].message)
  const length = e.decode(put(e, message))
  if (length >= 0) failures.push('a message with no schema held was accepted')
  else {
    const diagnostic = JSON.parse(out(e, e.last_error()))
    if (typeof diagnostic.message !== 'string' || diagnostic.message.length === 0) {
      failures.push('the no-schema refusal carried no message')
    }
  }
}

// Garbage must be refused, not trapped.
{
  const e = fresh()
  for (const bytes of [[0x00], [0xff, 0xff, 0xff], [0xd0], []]) {
    const length = e.decode(put(e, Uint8Array.from(bytes)))
    if (length >= 0) failures.push(`garbage accepted: ${bytes}`)
  }
}

console.log(`${pass}/${corpus.cases.length} cases byte-identical to the AssemblyScript module`)
console.log(`${dynamicPass}/${dynamic.walks.length * 2} dynamic deliveries agree with Go`)
if (failures.length > 0) {
  console.log(`\n${failures.length} failure(s):`)
  for (const line of failures.slice(0, 10)) console.log('  ' + line)
  process.exit(1)
}
console.log('the Rust decoder agrees with the module and refuses what it should')
