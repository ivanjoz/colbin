// Tier 2: the blocked column codec, against frames the Go codec produced.
//
// Both directions, because they fail differently. A frame that differs by a byte
// is an encoder that broke a tie the other way or scored a transform wrong; a
// frame that decodes to the wrong values is the packing. The tier records which
// transform Go picked, so a size difference says which of the four rather than
// only that the bytes differ.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { load, vectors, hex, unhex } from './harness.mjs'

const wasm = await load()
const cases = await vectors('columns')

test('the column tier is present', () => {
  assert.ok(cases.length >= 20, `only ${cases.length} columns`)
})

// The corpus records column values as decimal strings, because they are int64
// and JSON.parse turns 9223372036854775807 into 9223372036854775808 — which is
// the corruption this whole module exists to prevent, and would have made the
// expectation wrong in exactly the cases that matter most.
function writeValues(values) {
  const view = new DataView(wasm.memory.buffer)
  const at = wasm.exports.inPtr()
  for (let i = 0; i < values.length; i++) {
    view.setBigInt64(at + i * 8, BigInt(values[i]), true)
  }
}

function out(length) {
  const at = wasm.exports.outPtr()
  return wasm.u8.subarray(at, at + length)
}

for (const c of cases) {
  const values = c.values ?? []

  test(`column ${c.name} encodes (${c.transform || 'empty'})`, () => {
    writeValues(values)
    const length = wasm.exports.columnEncode(values.length, c.width)
    assert.equal(hex(out(length)), c.encoded)
  })

  test(`column ${c.name} decodes`, () => {
    const frame = unhex(c.encoded)
    wasm.u8.set(frame, wasm.exports.inPtr())
    const count = wasm.exports.columnDecode(frame.length, values.length, c.width)
    assert.equal(count, values.length)
    const view = new DataView(wasm.memory.buffer)
    const at = wasm.exports.outPtr()
    for (let i = 0; i < values.length; i++) {
      assert.equal(view.getBigInt64(at + i * 8, true), BigInt(values[i]), `value ${i}`)
    }
  })
}

// A corrupt frame must be a diagnostic, never a trap and never a read past the
// end. This is the property PLAN.md 4.3 exists for, applied to the one codec
// whose counts are entirely the message's claim.
test('a truncated column frame is refused, not trapped', () => {
  const full = unhex(cases.find((c) => c.name === 'delta-1000').encoded)
  for (const cut of [1, 2, 5, full.length >> 1, full.length - 1]) {
    wasm.u8.set(full.subarray(0, cut), wasm.exports.inPtr())
    const got = wasm.exports.columnDecode(cut, 1000, 8)
    assert.equal(got, -1, `a ${cut}-byte prefix should not decode 1000 values`)
  }
})

test('a block width above 64 is refused', () => {
  // transform raw, then a width byte of 65.
  const frame = Uint8Array.from([0x00, 65])
  wasm.u8.set(frame, wasm.exports.inPtr())
  assert.equal(wasm.exports.columnDecode(frame.length, 4, 8), -1)
})
