// PLAN.md 4.5: encode reads its own output back and compares it against the
// input before returning. The message vectors say the bytes match Go; this
// says the encoder catches itself when they would not.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { load, vectors, hex } from './harness.mjs'

const wasm = await load()
const cases = await vectors('messages.json')

function out(len) {
  return Buffer.from(wasm.u8.subarray(wasm.exports.outPtr(), wasm.exports.outPtr() + len)).toString()
}

function encodeVerified(text) {
  const src = Buffer.from(text, 'utf8')
  wasm.u8.set(src, wasm.exports.inPtr())
  const len = wasm.exports.encodeVerified(src.length)
  return { len, message: len >= 0 ? hex(wasm.u8.subarray(wasm.exports.outPtr(), wasm.exports.outPtr() + len)) : null }
}

for (const c of cases) {
  test(`self-check passes: ${c.tier}/${c.name}`, () => {
    const r = encodeVerified(c.json)
    assert.ok(r.len >= 0, `self-check rejected a good message: ${out(wasm.exports.jsonErrMessage())}`)
    // The check must not change what is written.
    assert.equal(r.message, c.message)
  })
}

// The three round-trip differences that are the format working, not the
// encoder failing (PLAN.md 6). The self-check has to accept all three, or it
// would refuse perfectly good input.
const expectedDifferences = [
  ['a key missing from one record', '[{"a":1,"b":2},{"a":3}]'],
  ['an explicit null', '[{"a":1},{"a":null}]'],
  ['an empty array', '[{"v":[1]},{"v":[]}]'],
  ['an array that is always empty', '[{"v":[]},{"v":[]}]'],
  ['an integer in a float column', '[{"v":1},{"v":2.5}]'],
  ['a duplicate key, last one winning', '[{"a":1,"a":2}]'],
  ['every value at its zero', '[{"i":0,"s":"","b":false}]'],
]

for (const [what, json] of expectedDifferences) {
  test(`self-check accepts ${what}`, () => {
    const r = encodeVerified(json)
    assert.ok(r.len >= 0, `wrongly refused: ${out(wasm.exports.jsonErrMessage())}`)
  })
}

test('the self-check and the plain path agree on the bytes', () => {
  for (const c of cases) {
    const src = Buffer.from(c.json, 'utf8')
    wasm.u8.set(src, wasm.exports.inPtr())
    const plain = wasm.exports.encodeJSON(src.length)
    const plainBytes = hex(wasm.u8.subarray(wasm.exports.outPtr(), wasm.exports.outPtr() + plain))
    assert.equal(encodeVerified(c.json).message, plainBytes)
  }
})

test('a value past 2^53 survives the self-check, which JSON.parse would not', () => {
  const r = encodeVerified('[{"id":7295013456321098765}]')
  assert.ok(r.len >= 0)
  // Decoding gives the digits back exactly; reading them with JSON.parse is
  // what loses them, which is why the check compares values in wasm.
  const msg = Uint8Array.from(Buffer.from(r.message, 'hex'))
  wasm.u8.set(msg, wasm.exports.inPtr())
  const len = wasm.exports.decodeMsg(msg.length)
  assert.match(out(len), /7295013456321098765/)
})
