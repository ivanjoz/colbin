// Decode is a trust boundary, and this is the property it has to hold.
//
// **No trap, no out-of-bounds read, always a diagnostic.** Not correctness —
// five sixths of single-byte corruptions of a colbin message decode to
// well-formed, wrong JSON, and no amount of bounds checking changes that.
// What is promised is narrower: whatever bytes arrive, the caller gets an
// answer rather than a RuntimeError.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { load, put, takeText, failure, vectors, unhex } from './harness.mjs'

const wasm = await load()
const cases = await vectors('types')

function survives(bytes, what) {
  wasm.exports.set_schema(0)
  const got = wasm.exports.decode(put(wasm, bytes))
  if (got < 0) {
    const why = failure(wasm)
    assert.ok(why.length > 0, `${what}: a refusal must carry a message`)
    return false
  }
  JSON.parse(takeText(wasm, got))
  return true
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

test('random bytes never trap', () => {
  const next = rng(0x9e3779b9)
  for (let round = 0; round < 2000; round++) {
    const length = next() % 64
    const bytes = new Uint8Array(length)
    for (let i = 0; i < length; i++) bytes[i] = next() & 0xff
    survives(bytes, `random round ${round}`)
  }
})

test('random bytes behind a valid root byte never trap', () => {
  const next = rng(0x85ebca6b)
  for (const root of [0xd0, 0xd4, 0xd8, 0xdc]) {
    for (let round = 0; round < 1000; round++) {
      const length = 1 + (next() % 80)
      const bytes = new Uint8Array(length)
      bytes[0] = root
      for (let i = 1; i < length; i++) bytes[i] = next() & 0xff
      survives(bytes, `root ${root.toString(16)} round ${round}`)
    }
  }
})

test('every single-byte corruption of every message never traps', () => {
  let decoded = 0
  let refused = 0
  for (const c of cases) {
    const original = unhex(c.selfDescribing)
    for (let at = 0; at < original.length; at++) {
      for (const mask of [0x01, 0x80, 0xff]) {
        const bytes = original.slice()
        bytes[at] ^= mask
        if (survives(bytes, `${c.name} byte ${at} mask ${mask}`)) decoded++
        else refused++
      }
    }
  }
  assert.ok(refused > 0, 'no corruption was refused')
  assert.ok(decoded > 0, 'no corruption decoded, so nothing walked')
})

test('the module refuses exactly the corruptions Go refuses', () => {
  let mine = 0
  let theirs = 0
  for (const c of cases) {
    const original = unhex(c.message)
    const section = unhex(c.section)
    let refused = 0
    for (let at = 0; at < original.length; at++) {
      for (const mask of [0x01, 0x80, 0xff]) {
        const bytes = original.slice()
        bytes[at] ^= mask
        assert.equal(wasm.exports.set_schema(put(wasm, section)), 0)
        const got = wasm.exports.decode(put(wasm, bytes))
        if (got < 0) refused++
        else JSON.parse(takeText(wasm, got))
      }
    }
    // Rust may refuse more than Go — invalid UTF-8, unassigned root details —
    // but must not accept a corruption Go catches.
    assert.ok(
      refused >= c.refused,
      `${c.name}: accepted ${c.refused - refused} corruptions Go refuses`,
    )
    mine += refused
    theirs += c.refused
  }
  assert.ok(mine >= theirs)
  assert.ok(theirs > 0, 'the corpus records no refusals at all')
})

test('truncation at every length never traps', () => {
  for (const c of cases) {
    const full = unhex(c.selfDescribing)
    for (let cut = 0; cut <= full.length; cut++) {
      survives(full.subarray(0, cut), `${c.name} cut to ${cut}`)
    }
  }
})

test('a mutated schema section never traps', () => {
  const next = rng(0xc2b2ae35)
  for (const c of cases) {
    const section = unhex(c.section)
    for (let round = 0; round < 40; round++) {
      const bytes = section.slice()
      const hits = 1 + (next() % 3)
      for (let i = 0; i < hits; i++) bytes[next() % bytes.length] = next() & 0xff
      wasm.exports.set_schema(0)
      const ok = wasm.exports.set_schema(put(wasm, bytes))
      if (ok === 0) {
        const got = wasm.exports.decode(put(wasm, unhex(c.message)))
        if (got >= 0) JSON.parse(takeText(wasm, got))
      } else {
        assert.ok(failure(wasm).length > 0)
      }
    }
  }
})

test('a table claiming an impossible row count is refused, not allocated', () => {
  const c = cases.find((one) => one.name === 'table')
  const section = unhex(c.section)
  assert.equal(wasm.exports.set_schema(put(wasm, section)), 0)

  const rows = 0x7fffffff
  const body = Uint8Array.from([
    0x00,
    0x0a,
    0x18 | 0x00,
    0x05,
    0xff,
    rows & 0xff,
    (rows >> 8) & 0xff,
    (rows >> 16) & 0xff,
    (rows >> 24) & 0xff,
  ])
  const message = Uint8Array.from([0xd0, ...body])
  const got = wasm.exports.decode(put(wasm, message))
  assert.equal(got, -1)
  assert.match(failure(wasm), /rows|field id|truncated|ends inside/)
})
