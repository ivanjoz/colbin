// The opt-in string encoding, embedded in a message.
//
// Which encoding a field used comes off the wire, so nothing here configures
// anything. The tier is generated twice over: once under four-bit keys, where a
// packed string names itself through the blob header's escape codes, and once
// under eight, where it uses the descriptor's `enc`.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { load, put, takeText, failure, must, vectors, unhex } from './harness.mjs'

const wasm = await load({ inspect: true })
const cases = await vectors('packed')

test('the packed tier is present, at both key widths', () => {
  assert.ok(cases.length >= 6, `only ${cases.length} cases`)
  const roots = new Set(cases.map((c) => unhex(c.message)[0] & 0x08))
  assert.equal(roots.size, 2, 'the tier must cover both key widths')
})

for (const c of cases) {
  // packed-narrow's Raw field is not valid UTF-8. Go renders it as U+FFFD; a
  // Rust String cannot hold it, so the walk refuses. The spans still tile —
  // skip_string does not decode.
  const invalidUtf8 = c.name === 'packed-narrow'

  test(`${c.name}: renders the same bytes as Go`, () => {
    must(wasm, wasm.exports.set_schema(put(wasm, unhex(c.section))), 'set_schema')
    const length = wasm.exports.decode(put(wasm, unhex(c.message)))
    if (invalidUtf8) {
      assert.equal(length, -1)
      assert.match(failure(wasm), /not valid UTF-8/)
      return
    }
    must(wasm, length, 'decode')
    assert.equal(takeText(wasm, length), c.json)
  })

  test(`${c.name}: self-describing`, () => {
    wasm.exports.set_schema(0)
    const length = wasm.exports.decode(put(wasm, unhex(c.selfDescribing)))
    if (invalidUtf8) {
      assert.equal(length, -1)
      assert.match(failure(wasm), /not valid UTF-8/)
      return
    }
    must(wasm, length, 'decode')
    assert.equal(takeText(wasm, length), c.json)
  })

  test(`${c.name}: the spans still tile`, () => {
    must(wasm, wasm.exports.set_schema(put(wasm, unhex(c.section))), 'set_schema')
    const length = must(wasm, wasm.exports.inspect_message(put(wasm, unhex(c.message))), 'inspect')
    const report = JSON.parse(takeText(wasm, length))
    let cursor = report.rootBytes + report.schemaBytes
    for (const field of report.fields) {
      assert.ok(field.start >= cursor, `${field.name} overlaps`)
      cursor = field.end
    }
    assert.equal(cursor, report.totalBytes, 'the fields must cover the body')
  })
}

test('a record of packed strings keeps its fields apart', () => {
  // Decode refuses packed-narrow (Raw is not UTF-8). The span walk still names
  // every field, which is what proves they did not leak into each other.
  const c = cases.find((one) => one.name === 'packed-narrow')
  must(wasm, wasm.exports.set_schema(put(wasm, unhex(c.section))), 'set_schema')
  const length = must(wasm, wasm.exports.inspect_message(put(wasm, unhex(c.message))), 'inspect')
  const report = JSON.parse(takeText(wasm, length))
  const named = Object.fromEntries(report.fields.map((f) => [f.name, f.type]))
  assert.equal(named.Name, 'string')
  assert.equal(named.City, 'string')
  assert.equal(named.SKU, 'string')
  assert.equal(named.Note, 'string')
  assert.equal(named.Raw, 'string')
})

test('a corrupt packed string is refused, not trapped', () => {
  for (const c of cases) {
    const original = unhex(c.message)
    const section = unhex(c.section)
    for (let at = 1; at < original.length; at++) {
      for (const mask of [0x01, 0xff]) {
        const bytes = original.slice()
        bytes[at] ^= mask
        wasm.exports.set_schema(put(wasm, section))
        const got = wasm.exports.decode(put(wasm, bytes))
        if (got >= 0) JSON.parse(takeText(wasm, got))
        else assert.ok(failure(wasm).length > 0)
      }
    }
  }
})

test('every truncation of a packed message is a diagnostic or valid JSON', () => {
  for (const c of cases) {
    const full = unhex(c.selfDescribing)
    for (let cut = 1; cut < full.length; cut++) {
      wasm.exports.set_schema(0)
      const got = wasm.exports.decode(put(wasm, full.subarray(0, cut)))
      if (got >= 0) JSON.parse(takeText(wasm, got))
    }
  }
})
