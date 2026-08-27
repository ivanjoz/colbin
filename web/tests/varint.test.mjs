// Level-1 vectors: every frame the Go codec produced must come out of the
// AssemblyScript port byte for byte, and decode back to the same values.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { load, vectors, hex, unhex } from './harness.mjs'

const wasm = await load()
const cases = await vectors('varint.json')

test('varint vectors are present', () => {
  assert.ok(cases.length >= 30, `only ${cases.length} vectors`)
})

for (const c of cases) {
  const values = c.values.map(BigInt)

  test(`encode ${c.name} (width ${c.width}, n=${values.length})`, () => {
    const inPtr = wasm.exports.inPtr()
    const view = wasm.view
    values.forEach((v, i) => view.setBigInt64(inPtr + i * 8, v, true))

    const len = wasm.exports.varintEncode(values.length, c.width)
    assert.ok(len >= 0, 'encode failed')
    const got = wasm.u8.subarray(wasm.exports.outPtr(), wasm.exports.outPtr() + len)
    assert.equal(hex(got), c.frame, `frame mismatch for ${c.name}`)
  })

  test(`decode ${c.name}`, () => {
    const frame = unhex(c.frame)
    const inPtr = wasm.exports.inPtr()
    wasm.u8.set(frame, inPtr)

    const consumed = wasm.exports.varintDecode(frame.length, values.length, c.width)
    assert.ok(consumed >= 0, `decode failed with err ${-consumed - 1}`)
    assert.equal(consumed, frame.length, 'consumed span must be the whole frame')

    const view = wasm.view
    const outPtr = wasm.exports.outPtr()
    const got = values.map((_, i) => view.getBigInt64(outPtr + i * 8, true))
    assert.deepEqual(got, values)
  })
}
