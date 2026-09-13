// Tier 1: blob framing at both key widths.
//
// This is the tier REFACTOR_PLAN.md 4.2 is going to change — the K4 size
// ceiling drops from 2047 to 1022 and two header bits are re-spent on the
// string encoding — so it is deliberately the one checked hardest. When the Go
// side lands that change, this file is what goes red and it names desc.ts.
//
// The corpus records the header alone and reconstructs the payload from a
// repeating fill, so a 64 KB case costs two fields rather than 128 KB of hex.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { load, vectors, hex, unhex } from './harness.mjs'

const wasm = await load()
const cases = await vectors('strings')

const MAX_NARROW_KEY = 15

test('the string tier is present', () => {
  assert.ok(cases.length >= 20, `only ${cases.length} frames`)
})

function payloadOf(c) {
  const fill = unhex(c.fill)
  if (c.size === 0 || fill.length === 0) return new Uint8Array(0)
  const out = new Uint8Array(c.size)
  for (let i = 0; i < c.size; i++) out[i] = fill[i % fill.length]
  return out
}

function frame(call, key, payload) {
  wasm.u8.set(payload, wasm.exports.inPtr())
  const length = call(key, payload.length)
  const at = wasm.exports.outPtr()
  return wasm.u8.subarray(at, at + length)
}

for (const c of cases) {
  test(`blob ${c.name} frames at both widths`, () => {
    const payload = payloadOf(c)

    if (c.key <= MAX_NARROW_KEY) {
      const narrow = frame(wasm.exports.blobNarrow, c.key, payload)
      assert.equal(hex(narrow.subarray(0, narrow.length - c.size)), c.narrow, 'narrow header')
      assert.equal(hex(narrow.subarray(narrow.length - c.size)), hex(payload), 'narrow payload')
    } else {
      assert.equal(c.narrow, '', 'a key past fifteen has no narrow form')
    }

    const wide = frame(wasm.exports.blobWide, c.key, payload)
    assert.equal(hex(wide.subarray(0, wide.length - c.size)), c.wide, 'wide header')
    assert.equal(hex(wide.subarray(wide.length - c.size)), hex(payload), 'wide payload')
  })
}

test('an empty blob is not written at all', () => {
  const empty = cases.find((c) => c.size === 0)
  assert.ok(empty, 'the corpus should hold an empty case')
  assert.equal(empty.narrow, '')
  assert.equal(empty.wide, '')
  assert.equal(frame(wasm.exports.blobNarrow, empty.key, new Uint8Array(0)).length, 0)
  assert.equal(frame(wasm.exports.blobWide, empty.key, new Uint8Array(0)).length, 0)
})
