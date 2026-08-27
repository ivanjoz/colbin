// Level-2 vectors (PLAN.md 7): the same JSON through Go's MarshalJSON and
// through this module must give the same bytes. Tiers are turned on as the
// encoder grows; a tier that is off is skipped loudly rather than passing.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { load, vectors, hex } from './harness.mjs'

const wasm = await load()
const cases = await vectors('messages.json')

const IMPLEMENTED = new Set(['scalar', 'float', 'nested', 'array', 'nullable', 'value'])

function out(len) {
  return Buffer.from(wasm.u8.subarray(wasm.exports.outPtr(), wasm.exports.outPtr() + len)).toString()
}

for (const c of cases) {
  test(`${c.tier}: ${c.name}`, { skip: !IMPLEMENTED.has(c.tier) }, () => {
    const src = Buffer.from(c.json, 'utf8')
    wasm.u8.set(src, wasm.exports.inPtr())
    const len = wasm.exports.encodeJSON(src.length)
    assert.ok(len >= 0, `encode failed: ${out(wasm.exports.jsonErrMessage())}`)
    const got = wasm.u8.subarray(wasm.exports.outPtr(), wasm.exports.outPtr() + len)
    assert.equal(hex(got), c.message, 'message must be byte-identical to colbin.MarshalJSON')
  })
}
