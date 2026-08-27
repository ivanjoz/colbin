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

/**
 * Byte positions to corrupt in a message.
 *
 * Exhaustive up to a budget, sampled above it. A 25 KB vector has 75 000
 * single-byte mutations on its own, which turned this file from half a second
 * into minutes; the guarantee being tested is "corrupt input yields a
 * diagnostic", and spreading the same number of probes over the whole corpus
 * tests it better than exhausting one member of it.
 *
 * The sample is deliberately front- and back-loaded: the version byte, the
 * schema section and the first column headers are where a mutation reaches the
 * counts and offsets that PLAN.md 4.3 is about, and the tail is where
 * truncation-shaped failures live.
 */
const PROBE_BUDGET = 192

function probePositions(length) {
  if (length <= PROBE_BUDGET) return Array.from({ length }, (_, i) => i)
  const hits = new Set()
  for (let i = 0; i < 64 && i < length; i++) hits.add(i)
  for (let i = Math.max(0, length - 16); i < length; i++) hits.add(i)
  const stride = Math.max(1, Math.floor(length / (PROBE_BUDGET - hits.size)))
  for (let i = 0; i < length; i += stride) hits.add(i)
  return [...hits].sort((a, b) => a - b)
}

// Deterministic, so a failure is reproducible from the seed alone.
function rng(seed) {
  let s = seed >>> 0
  return () => {
    s = (s * 1664525 + 1013904223) >>> 0
    return s
  }
}

test('single-byte corruption is survivable', () => {
  let errors = 0
  let decoded = 0
  let exhaustive = 0
  for (const c of cases) {
    const original = Uint8Array.from(Buffer.from(c.message, 'hex'))
    const positions = probePositions(original.length)
    if (positions.length === original.length) exhaustive++
    for (const i of positions) {
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
  assert.ok(errors + decoded > 5000, `only ${errors + decoded} mutations ran`)
  assert.ok(exhaustive >= cases.length - 4, `${exhaustive}/${cases.length} vectors covered exhaustively`)
})

test('truncation is survivable', () => {
  for (const c of cases) {
    const original = Uint8Array.from(Buffer.from(c.message, 'hex'))
    for (const len of probePositions(original.length)) {
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
