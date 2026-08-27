// Level-1 vectors: every frame the Go codec produced must come out of the
// AssemblyScript port byte for byte, and decode back to the original bytes —
// including the inputs that are not valid UTF-8, which is the whole reason the
// escape opcode carries bytes rather than runes.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { load, vectors, hex, unhex } from './harness.mjs'

const wasm = await load()
const cases = await vectors('packed5.json')

test('packed5 vectors are present', () => {
  assert.ok(cases.length >= 25, `only ${cases.length} vectors`)
})

for (const c of cases) {
  const input = Uint8Array.from(Buffer.from(c.input, 'base64'))

  test(`encode ${c.name}`, () => {
    wasm.u8.set(input, wasm.exports.inPtr())
    const len = wasm.exports.packed5Encode(input.length)
    const got = wasm.u8.subarray(wasm.exports.outPtr(), wasm.exports.outPtr() + len)
    assert.equal(hex(got), c.frame, `frame mismatch for ${c.name}`)
  })

  test(`size ${c.name}`, () => {
    wasm.u8.set(input, wasm.exports.inPtr())
    assert.equal(wasm.exports.packed5SizeOf(input.length), c.size)
  })

  test(`decode ${c.name}`, () => {
    const frame = unhex(c.frame)
    wasm.u8.set(frame, wasm.exports.inPtr())
    const len = wasm.exports.packed5DecodeFrame(frame.length)
    assert.ok(len >= 0, `decode failed with err ${-len - 1}`)
    assert.equal(wasm.exports.packed5ConsumedBytes(), frame.length, 'frame must be self-delimiting')
    const got = wasm.u8.subarray(wasm.exports.outPtr(), wasm.exports.outPtr() + len)
    assert.equal(hex(got), hex(input), `round-trip mismatch for ${c.name}`)
  })
}
