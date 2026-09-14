// A message and its section, rendered as JSON, against colbin.ToJSON.
//
// The comparison is on **bytes**, not on parsed values: the claim is that the
// module writes what encoding/json would have written for the same record, down
// to the escaping and the spelling of the numbers, and a test that parsed both
// sides would not notice a float printed one digit differently.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { load, put, takeText, failure, must, vectors, unhex } from './harness.mjs'

const wasm = await load()
const cases = await vectors('types')

test('the type tier is present', () => {
  assert.ok(cases.length >= 20, `only ${cases.length} types`)
})

for (const c of cases) {
  // A narrow map's entries take their type from the schema and need a second
  // element codec. The Rust walk refuses rather than guessing — RUST_WASM_PLAN.md
  // §7. The page never produces one: JSON objects infer to structs.
  if (c.name === 'maps') {
    test(`${c.name}: refused as unsupported, not guessed at`, () => {
      must(wasm, wasm.exports.set_schema(put(wasm, unhex(c.section))), 'set_schema')
      assert.equal(wasm.exports.decode(put(wasm, unhex(c.message))), -1)
      assert.match(failure(wasm), /does not render maps/)
    })
    continue
  }

  test(`${c.name}: out-of-band schema`, () => {
    must(wasm, wasm.exports.set_schema(put(wasm, unhex(c.section))), 'set_schema')
    const length = must(wasm, wasm.exports.decode(put(wasm, unhex(c.message))), 'decode')
    assert.equal(takeText(wasm, length), c.json)
  })

  test(`${c.name}: self-describing`, () => {
    wasm.exports.set_schema(0)
    const length = must(wasm, wasm.exports.decode(put(wasm, unhex(c.selfDescribing))), 'decode')
    assert.equal(takeText(wasm, length), c.json)
  })
}

test('a message with no section and no schema is refused by name', () => {
  wasm.exports.set_schema(0)
  const plain = unhex(cases.find((c) => c.name === 'corpus-user').message)
  assert.equal(wasm.exports.decode(put(wasm, plain)), -1)
  assert.match(failure(wasm), /carries no schema section/)
})

test('a first byte outside the reserved range is not colbin', () => {
  wasm.exports.set_schema(0)
  for (const byte of [0x00, 0x1f, 0x7b, 0xcf, 0xe0, 0xff]) {
    assert.equal(wasm.exports.decode(put(wasm, Uint8Array.from([byte, 0]))), -1)
    assert.match(failure(wasm), /not a root descriptor/)
  }
})

test('an unassigned root detail is refused rather than guessed at', () => {
  wasm.exports.set_schema(0)
  for (const byte of [0xd1, 0xd2, 0xd3, 0xdb]) {
    assert.equal(wasm.exports.decode(put(wasm, Uint8Array.from([byte, 0]))), -1)
    assert.match(failure(wasm), /not a root descriptor/)
  }
})

test('every prefix of a valid message is a diagnostic, never a trap', () => {
  for (const name of ['corpus-sale-table', 'corpus-product', 'table', 'maps', 'arrays']) {
    const c = cases.find((one) => one.name === name)
    const full = unhex(c.selfDescribing)
    for (let cut = 1; cut < full.length; cut++) {
      wasm.exports.set_schema(0)
      const got = wasm.exports.decode(put(wasm, full.subarray(0, cut)))
      if (got >= 0) {
        JSON.parse(takeText(wasm, got))
      }
    }
  }
})
