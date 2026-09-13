// Phase 7: the opt-in string encoding, embedded in a message.
//
// The module only ever meets the embedded form — a BLOB whose descriptor says
// the encoding and whose size the descriptor already carries, so the standalone
// frame's header byte is not there. Which is the whole of what the codec's
// re-landing bought: one byte off every packed string field.
//
// Which encoding a field used comes off the wire, so nothing here configures
// anything. The tier is generated twice over: once under four-bit keys, where a
// packed string names itself through the blob header's escape codes, and once
// under eight, where it uses the descriptor's `enc`. packed5 no longer forces
// the wide key, which is what makes the first of those reachable at all.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { load, vectors, unhex } from './harness.mjs'

const wasm = await load()
const cases = await vectors('packed')
const decoder = new TextDecoder()

function feed(bytes) {
  wasm.u8.set(bytes, wasm.exports.inPtr())
  return bytes.length
}

function outText(length) {
  const at = wasm.exports.outPtr()
  return decoder.decode(wasm.u8.slice(at, at + length))
}

const failure = () => outText(wasm.exports.lastMessage())

test('the packed tier is present, at both key widths', () => {
  assert.ok(cases.length >= 6, `only ${cases.length} cases`)
  const roots = new Set(cases.map((c) => unhex(c.message)[0] & 0x08))
  assert.equal(roots.size, 2, 'the tier must cover both key widths')
})

for (const c of cases) {
  test(`${c.name}: renders the same bytes as Go`, () => {
    assert.equal(wasm.exports.setSection(feed(unhex(c.section))), 0, failure())
    const length = wasm.exports.messageJSON(feed(unhex(c.message)))
    assert.ok(length >= 0, `decode: ${failure()}`)
    assert.equal(outText(length), c.json)
  })

  test(`${c.name}: self-describing`, () => {
    wasm.exports.setSection(0)
    const length = wasm.exports.messageJSON(feed(unhex(c.selfDescribing)))
    assert.ok(length >= 0, `decode: ${failure()}`)
    assert.equal(outText(length), c.json)
  })

  test(`${c.name}: the spans still tile`, () => {
    assert.equal(wasm.exports.setSection(feed(unhex(c.section))), 0, failure())
    const length = wasm.exports.inspectJSON(feed(unhex(c.message)))
    assert.ok(length >= 0, `inspect: ${failure()}`)
    const report = JSON.parse(outText(length))
    let cursor = report.rootBytes + report.schemaBytes
    for (const field of report.fields) {
      assert.ok(field.start >= cursor, `${field.name} overlaps`)
      cursor = field.end
    }
    assert.equal(cursor, report.totalBytes, 'the fields must cover the body')
  })
}

// A packed payload is a unit stream: the characters only exist once it is
// expanded, so there is nothing in the message to hand back a view of. Getting
// the scratch wrong would show up as one field's text leaking into the next.
test('a record of packed strings keeps its fields apart', () => {
  const c = cases.find((one) => one.name === 'packed-narrow')
  assert.equal(wasm.exports.setSection(feed(unhex(c.section))), 0)
  const length = wasm.exports.messageJSON(feed(unhex(c.message)))
  assert.ok(length >= 0, failure())
  const back = JSON.parse(outText(length))
  assert.equal(back.Name, 'Tin Light')
  assert.equal(back.City, 'Arequipa')
  assert.equal(back.SKU, 'SKU-00042')
  // The accents come out of the extended table, byte for byte.
  assert.equal(back.Note, 'el niño comió jamón')
  // And the raw escape carries bytes rather than runes, which is what makes the
  // codec exact for input that is not valid UTF-8. encoding/json renders those
  // as U+FFFD, so both sides agree on the replacement rather than on the bytes.
  assert.ok(back.Raw.length > 0)
})

// Every corruption of a packed payload must be a diagnostic, never a trap: the
// unit stream's counts are the message's claim like any other.
test('a corrupt packed string is refused, not trapped', () => {
  for (const c of cases) {
    const original = unhex(c.message)
    const section = unhex(c.section)
    for (let at = 1; at < original.length; at++) {
      for (const mask of [0x01, 0xff]) {
        const bytes = original.slice()
        bytes[at] ^= mask
        wasm.exports.setSection(feed(section))
        const got = wasm.exports.messageJSON(feed(bytes))
        if (got >= 0) JSON.parse(outText(got))
        else assert.ok(failure().length > 0)
      }
    }
  }
})

test('every truncation of a packed message is a diagnostic or valid JSON', () => {
  for (const c of cases) {
    const full = unhex(c.selfDescribing)
    for (let cut = 1; cut < full.length; cut++) {
      wasm.exports.setSection(0)
      const got = wasm.exports.messageJSON(feed(full.subarray(0, cut)))
      if (got >= 0) JSON.parse(outText(got))
    }
  }
})
