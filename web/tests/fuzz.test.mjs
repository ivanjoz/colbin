// PLAN.md 4.3: a corrupt message must produce a diagnostic. Never a trap,
// never an out-of-bounds read, never an allocation the tab does not come back
// from. Go gets that from an unconditional bounds check plus recover;
// AssemblyScript has neither, so the decoder checks every count and offset
// itself - and this is what says it does.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { load, vectors } from './harness.mjs'

const wasm = await load()
const cases = await vectors('messages.json')

function attempt(bytes) {
  wasm.u8.set(bytes, wasm.exports.inPtr())
  // A trap surfaces as a throw: either the abort import or a RuntimeError.
  // Either one is a failure of the guarantee, so nothing is caught here.
  return wasm.exports.decodeMsg(bytes.length)
}

// Deterministic, so a failure is reproducible from the seed alone.
function rng(seed) {
  let s = seed >>> 0
  return () => {
    s = (s * 1664525 + 1013904223) >>> 0
    return s
  }
}

test('every single-byte corruption of every vector is survivable', () => {
  let errors = 0
  let decoded = 0
  for (const c of cases) {
    const original = Uint8Array.from(Buffer.from(c.message, 'hex'))
    for (let i = 0; i < original.length; i++) {
      for (const mask of [0x01, 0x80, 0xff]) {
        const mutated = Uint8Array.from(original)
        mutated[i] ^= mask
        const r = attempt(mutated)
        if (r < 0) errors++
        else decoded++
      }
    }
  }
  // Both outcomes are legitimate; what matters is that neither threw.
  assert.ok(errors + decoded > 3000, `only ${errors + decoded} mutations ran`)
})

test('truncation at every length is survivable', () => {
  for (const c of cases) {
    const original = Uint8Array.from(Buffer.from(c.message, 'hex'))
    for (let len = 0; len < original.length; len++) {
      attempt(original.subarray(0, len))
    }
  }
})

test('random buffers are survivable', () => {
  const next = rng(20260826)
  for (let round = 0; round < 2000; round++) {
    const len = next() % 64
    const buf = new Uint8Array(len)
    for (let i = 0; i < len; i++) buf[i] = next() & 0xff
    // A JSON-mode message starts 0x04, so bias towards buffers that get past
    // the version check and reach the parts that actually index the buffer.
    if (len > 0 && round % 2 === 0) buf[0] = 0x04
    attempt(buf)
  }
})

test('multi-byte corruption is survivable', () => {
  const next = rng(7)
  for (const c of cases) {
    const original = Uint8Array.from(Buffer.from(c.message, 'hex'))
    for (let round = 0; round < 40; round++) {
      const mutated = Uint8Array.from(original)
      const hits = 1 + (next() % 6)
      for (let h = 0; h < hits; h++) mutated[next() % mutated.length] = next() & 0xff
      attempt(mutated)
    }
  }
})

test('a record count inflated to 2^40 is refused, not allocated', () => {
  // The case 4.3 works through: Go answers it with makeslice panicking into
  // recover. There is no recover here, so the count is checked instead.
  const c = cases.find((x) => x.name === 'three-records')
  const original = Array.from(Buffer.from(c.message, 'hex'))
  const schemaLen = original[1]
  const countAt = 2 + schemaLen
  const inflated = Uint8Array.from([
    ...original.slice(0, countAt),
    0xff, 0xff, 0xff, 0xff, 0xff, 0x3f,
    ...original.slice(countAt + 1),
  ])
  const r = attempt(inflated)
  assert.ok(r < 0, 'an impossible record count must be refused')
  const len = wasm.exports.jsonErrMessage()
  const msg = Buffer.from(wasm.u8.subarray(wasm.exports.outPtr(), wasm.exports.outPtr() + len)).toString()
  assert.match(msg, /exceeds|larger than/)
})

test('a binary-mode message is refused with an explanation, not a crash', () => {
  const r = attempt(Uint8Array.from([0x02, 0x01, 0x01]))
  assert.ok(r < 0)
  const len = wasm.exports.jsonErrMessage()
  const msg = Buffer.from(wasm.u8.subarray(wasm.exports.outPtr(), wasm.exports.outPtr() + len)).toString()
  assert.match(msg, /binary-mode/)
})

test('a compact-mode message says so rather than misreading itself', () => {
  const r = attempt(Uint8Array.from([0x03, 0x01, 0x01]))
  assert.ok(r < 0)
  const len = wasm.exports.jsonErrMessage()
  const msg = Buffer.from(wasm.u8.subarray(wasm.exports.outPtr(), wasm.exports.outPtr() + len)).toString()
  assert.match(msg, /compact-mode/)
})
