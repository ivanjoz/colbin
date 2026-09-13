// Tier 3: a message and its section, rendered as JSON, against colbin.ToJSON.
//
// This is phase 2's whole gate. The comparison is on **bytes**, not on parsed
// values: the claim is that the module writes what encoding/json would have
// written for the same record, down to the escaping and the spelling of the
// numbers, and a test that parsed both sides would not notice a float printed
// one digit differently.
//
// Every case goes through twice, because the two deliveries are different code
// paths to the same answer: the section passed out of band, which is what a
// stream should do, and the section carried in front of the body, which is what
// a document that has to stand alone does.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { load, vectors, unhex } from './harness.mjs'

const wasm = await load()
const cases = await vectors('types')

const decoder = new TextDecoder()

test('the type tier is present', () => {
  assert.ok(cases.length >= 20, `only ${cases.length} types`)
})

function feed(bytes) {
  wasm.u8.set(bytes, wasm.exports.inPtr())
  return bytes.length
}

function outText(length) {
  const at = wasm.exports.outPtr()
  return decoder.decode(wasm.u8.slice(at, at + length))
}

function failure() {
  return outText(wasm.exports.lastMessage())
}

for (const c of cases) {
  test(`${c.name}: out-of-band schema`, () => {
    assert.equal(wasm.exports.setSection(feed(unhex(c.section))), 0, failure())
    const length = wasm.exports.messageJSON(feed(unhex(c.message)))
    assert.ok(length >= 0, `decode: ${failure()}`)
    assert.equal(outText(length), c.json)
  })

  test(`${c.name}: self-describing`, () => {
    // No section held, so the message's own is the only one there is.
    wasm.exports.setSection(0)
    const length = wasm.exports.messageJSON(feed(unhex(c.selfDescribing)))
    assert.ok(length >= 0, `decode: ${failure()}`)
    assert.equal(outText(length), c.json)
  })
}

test('a message with no section and no schema is refused by name', () => {
  wasm.exports.setSection(0)
  const plain = unhex(cases.find((c) => c.name === 'corpus-user').message)
  assert.equal(wasm.exports.messageJSON(feed(plain)), -1)
  assert.match(failure(), /carries no schema section/)
})

test('a first byte outside the reserved range is not colbin', () => {
  wasm.exports.setSection(0)
  for (const byte of [0x00, 0x1f, 0x7b, 0xcf, 0xe0, 0xff]) {
    assert.equal(wasm.exports.messageJSON(feed(Uint8Array.from([byte, 0]))), -1)
    assert.match(failure(), /not a colbin root/)
  }
})

test('an unassigned root detail is refused rather than guessed at', () => {
  wasm.exports.setSection(0)
  for (const byte of [0xd1, 0xd2, 0xd3, 0xdb]) {
    assert.equal(wasm.exports.messageJSON(feed(Uint8Array.from([byte, 0]))), -1)
    assert.match(failure(), /not assigned by this version/)
  }
})

// Decode is a trust boundary and a wider one than encode: the encoder controls
// its own input and can trust its counts, while this is handed bytes by a
// stranger. AssemblyScript has neither of the two halves Go leans on here, so
// every prefix of a valid message must be a diagnostic rather than a trap.
test('every prefix of a valid message is a diagnostic, never a trap', () => {
  for (const name of ['corpus-sale-table', 'corpus-product', 'table', 'maps', 'arrays']) {
    const c = cases.find((one) => one.name === name)
    const full = unhex(c.selfDescribing)
    for (let cut = 1; cut < full.length; cut++) {
      wasm.exports.setSection(0)
      const got = wasm.exports.messageJSON(feed(full.subarray(0, cut)))
      if (got >= 0) {
        // A prefix that happens to decode is not a failure — a truncated
        // message can still be a shorter valid one — but it must be JSON.
        JSON.parse(outText(got))
      }
    }
  }
})
