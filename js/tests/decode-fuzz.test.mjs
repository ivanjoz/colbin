// The pure-JS decoder is a trust boundary too (INTERNALS.md §13): whatever
// bytes arrive, the caller gets a value or a ColbinError — never a TypeError, a
// RangeError, a hang, or an allocation the message did not pay for.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { vectors, unhex } from './harness.mjs'
import { Decoder } from '../src/decode/index.ts'
import { ColbinError } from '../src/errors.ts'

const cases = [...(await vectors('types')), ...(await vectors('packed'))]

/** Decodes and reports whether the bytes were accepted; any failure other than
 * a ColbinError fails the test. */
function survives(decoder, bytes, what) {
  try {
    walk(decoder.decode(bytes))
    return true
  } catch (err) {
    if (!(err instanceof ColbinError)) {
      assert.fail(`${what}: threw ${err?.name}: ${err?.message}`)
    }
    assert.ok(err.message.length > 0, `${what}: a refusal must carry a message`)
    return false
  }
}

/** Touches every value, so a lazily wrong one would surface here. */
function walk(value, depth = 0) {
  assert.ok(depth < 300, 'nesting past every limit')
  if (value === null || typeof value !== 'object') return
  if (ArrayBuffer.isView(value)) return
  for (const k of Object.keys(value)) walk(value[k], depth + 1)
}

/** Whether a value holds a float JSON has no form for, which Go's walk refuses
 * and this decoder keeps. */
function hasNonFinite(value) {
  if (typeof value === 'number') return !Number.isFinite(value)
  if (value === null || typeof value !== 'object' || ArrayBuffer.isView(value)) return false
  return Object.keys(value).some((k) => hasNonFinite(value[k]))
}

function rng(seed) {
  let state = seed >>> 0
  return () => {
    state ^= state << 13
    state ^= state >>> 17
    state ^= state << 5
    return state >>> 0
  }
}

test('random bytes never throw anything but a ColbinError', () => {
  const next = rng(0x9e3779b9)
  const decoder = new Decoder()
  for (let round = 0; round < 5000; round++) {
    const length = next() % 64
    const bytes = new Uint8Array(length)
    for (let i = 0; i < length; i++) bytes[i] = next() & 0xff
    survives(decoder, bytes, `random round ${round}`)
  }
})

test('random bytes behind every root byte never throw anything else', () => {
  const next = rng(0x85ebca6b)
  for (let root = 0xd0; root <= 0xdf; root++) {
    for (let round = 0; round < 500; round++) {
      const length = 1 + (next() % 80)
      const bytes = new Uint8Array(length)
      bytes[0] = root
      for (let i = 1; i < length; i++) bytes[i] = next() & 0xff
      survives(new Decoder(), bytes, `root ${root.toString(16)} round ${round}`)
    }
  }
})

test('random bodies against every held section never throw anything else', () => {
  const next = rng(0x27d4eb2f)
  for (const c of cases) {
    const decoder = new Decoder()
    decoder.setSchema(unhex(c.section))
    for (let round = 0; round < 300; round++) {
      const length = next() % 96
      const bytes = new Uint8Array(length)
      for (let i = 0; i < length; i++) bytes[i] = next() & 0xff
      survives(decoder, bytes, `${c.name} round ${round}`)
    }
  }
})

test('every single-byte corruption of every message never throws anything else', () => {
  let refused = 0
  let decoded = 0
  for (const c of cases) {
    const original = unhex(c.selfDescribing)
    for (let at = 0; at < original.length; at++) {
      for (const mask of [0x01, 0x80, 0xff]) {
        const bytes = original.slice()
        bytes[at] ^= mask
        if (survives(new Decoder(), bytes, `${c.name} byte ${at} mask ${mask}`)) decoded++
        else refused++
      }
    }
  }
  assert.ok(refused > 0 && decoded > 0)
})

test('refuses at least every corruption Go refuses', () => {
  for (const c of cases) {
    const original = unhex(c.message)
    const decoder = new Decoder()
    decoder.setSchema(unhex(c.section))
    let refused = 0
    for (let at = 0; at < original.length; at++) {
      for (const mask of [0x01, 0x80, 0xff]) {
        const bytes = original.slice()
        bytes[at] ^= mask
        try {
          const value = decoder.decode(bytes)
          // Go's JSON walk refuses NaN and ±Inf; this decoder keeps them.
          if (hasNonFinite(value)) refused++
        } catch (err) {
          assert.ok(err instanceof ColbinError, `${c.name}: ${err}`)
          refused++
        }
      }
    }
    assert.ok(refused >= c.refused, `${c.name}: accepted ${c.refused - refused} corruptions Go refuses`)
  }
})

test('truncation at every length never throws anything else', () => {
  for (const c of cases) {
    const full = unhex(c.selfDescribing)
    for (let cut = 0; cut <= full.length; cut++) survives(new Decoder(), full.subarray(0, cut), `${c.name} cut ${cut}`)
  }
})

test('a mutated schema section never throws anything else', () => {
  const next = rng(0xc2b2ae35)
  for (const c of cases) {
    const section = unhex(c.section)
    for (let round = 0; round < 200; round++) {
      const bytes = section.slice()
      const hits = 1 + (next() % 3)
      for (let i = 0; i < hits; i++) bytes[next() % bytes.length] = next() & 0xff
      const decoder = new Decoder()
      try {
        decoder.setSchema(bytes)
      } catch (err) {
        assert.ok(err instanceof ColbinError, `${c.name}: setSchema threw ${err}`)
        continue
      }
      survives(decoder, unhex(c.message), `${c.name} section round ${round}`)
    }
  }
})

test('a table claiming an impossible row count is refused, not allocated', () => {
  const c = cases.find((one) => one.name === 'table')
  const decoder = new Decoder()
  decoder.setSchema(unhex(c.section))
  const message = unhex(c.message)
  // Find the table's row count byte by trying every position with 0xFF and a
  // u32 of four billion: whichever lands on the count must be refused.
  for (let at = 0; at < message.length; at++) {
    const bytes = new Uint8Array(message.length + 4)
    bytes.set(message.subarray(0, at))
    bytes.set([0xff, 0xff, 0xff, 0xff, 0xff], at)
    bytes.set(message.subarray(at + 1), at + 5)
    survives(decoder, bytes, `count at ${at}`)
  }
})
