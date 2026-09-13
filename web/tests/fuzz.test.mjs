// Phase 3: decode is a trust boundary, and this is the property it has to hold.
//
// **No trap, no out-of-bounds read, always a diagnostic.** Not correctness —
// PLAN.md 4.3 measured that half of all single-byte corruptions of a colbin
// message decode to well-formed, wrong JSON, and no amount of bounds checking
// changes that. Detecting corruption needs a checksum the format does not have.
// What is promised is narrower and is entirely this module's to keep: whatever
// bytes arrive, the caller gets an answer rather than a RuntimeError.
//
// The harness makes `abort` throw, so a trap fails the test it happens in rather
// than being swallowed. That is the whole mechanism: every case below is a
// success if it returns at all.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { load, vectors, unhex } from './harness.mjs'

const wasm = await load()
const cases = await vectors('types')
const decoder = new TextDecoder()

function feed(bytes) {
  wasm.u8.set(bytes, wasm.exports.inPtr())
  return bytes.length
}

function outText(length) {
  const at = wasm.exports.outPtr()
  return decoder.decode(wasm.u8.slice(at, at + length))
}

/** Decodes and insists the answer is an answer: JSON, or a diagnostic that says
 * something. Returns true when it decoded. */
function survives(bytes, what) {
  wasm.exports.setSection(0)
  const got = wasm.exports.messageJSON(feed(bytes))
  if (got < 0) {
    const why = outText(wasm.exports.lastMessage())
    assert.ok(why.length > 0, `${what}: a refusal must carry a message`)
    return false
  }
  const text = outText(got)
  // "It decoded" is not enough: it has to be JSON, or the module has written
  // something no caller can use and called it success.
  JSON.parse(text)
  return true
}

// A deterministic generator, so a failure is reproducible from the seed alone.
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
  // Pure noise is nearly always rejected at byte 0, which tests one comparison.
  // Forcing a root the module accepts drives the section parser and the walk.
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
  // The sweep PLAN.md 4.3 ran against Go, run against this decoder: each byte
  // of each message, with the three masks that turn a bit, a high bit and the
  // whole byte.
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
  // Both outcomes must actually occur, or the sweep is not exercising anything:
  // all-refused would mean the first byte check is doing all the work, and
  // all-decoded would mean nothing is being validated.
  assert.ok(refused > 0, 'no corruption was refused')
  assert.ok(decoded > 0, 'no corruption decoded, so nothing walked')
})

// The comparison PLAN.md 7 asks for, against a number rather than an impression.
//
// The corpus records how many corruptions of each message Go's own decoder
// refuses. This module must refuse exactly the same ones: fewer means it is
// walking into something Go catches, and *more* means it is rejecting a message
// Go accepts, which on a decoder is the worse of the two.
//
// 16% across the corpus, not the 38.6% PLAN.md 4.3 quotes — that figure was
// measured on the old columnar format, which carried more redundancy to
// contradict. A leaner wire detects less, and neither decoder can do anything
// about it: a flipped bit inside a magnitude is simply a different number.
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
        assert.equal(wasm.exports.setSection(feed(section)), 0)
        const got = wasm.exports.messageJSON(feed(bytes))
        if (got < 0) refused++
        else JSON.parse(outText(got))
      }
    }
    assert.equal(refused, c.refused, `${c.name}: refusals differ from Go`)
    mine += refused
    theirs += c.refused
  }
  assert.equal(mine, theirs)
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
  // The section is a second untrusted input and a different parser, so it gets
  // its own sweep: a struct table it claims, a name length it claims, an op
  // number this version does not assign, an index pointing outside the table.
  const next = rng(0xc2b2ae35)
  for (const c of cases) {
    const section = unhex(c.section)
    for (let round = 0; round < 40; round++) {
      const bytes = section.slice()
      const hits = 1 + (next() % 3)
      for (let i = 0; i < hits; i++) bytes[next() % bytes.length] = next() & 0xff
      wasm.exports.setSection(0)
      const ok = wasm.exports.setSection(feed(bytes))
      if (ok === 0) {
        // A section that parsed must then survive being pointed at a message.
        const got = wasm.exports.messageJSON(feed(unhex(c.message)))
        if (got >= 0) JSON.parse(outText(got))
      } else {
        assert.ok(outText(wasm.exports.lastMessage()).length > 0)
      }
    }
  }
})

test('a table claiming an impossible row count is refused, not allocated', () => {
  // A narrow table under key 0: [key|tableFlag][len][rows escape][4-byte rows].
  // The row count is the message's own claim and the only thing standing between
  // it and a multi-gigabyte allocation is the bound in walk.ts.
  const c = cases.find((one) => one.name === 'table')
  const section = unhex(c.section)
  assert.equal(wasm.exports.setSection(feed(section)), 0)

  const rows = 0x7fffffff
  const body = Uint8Array.from([
    0x00,
    0x0a, // integer field 0 = 1, so the message is not only the table
    0x18 | 0x00, // key 1, table flag, one-byte length
    0x05,
    0xff,
    rows & 0xff,
    (rows >> 8) & 0xff,
    (rows >> 16) & 0xff,
    (rows >> 24) & 0xff,
  ])
  const message = Uint8Array.from([0xd0, ...body])
  const got = wasm.exports.messageJSON(feed(message))
  assert.equal(got, -1)
  assert.match(outText(wasm.exports.lastMessage()), /rows|field id|truncated|ends inside/)
})
