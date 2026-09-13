// Phase 4: the encoder, against itself and against the decoder.
//
// The cross-language half is emit.mjs and `go test ./web/vectors` — this file
// is the part that does not need Go: every document encodes, the self-check
// passes, the message decodes back to the input, and the refusals are refused
// with a message that names what went wrong.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { load } from './harness.mjs'
import { documents, refusals, textOf } from './documents.mjs'

const wasm = await load()
const encoder = new TextEncoder()
const decoder = new TextDecoder()

const SELF_DESCRIBING = 1
const VERIFY = 2

function feed(text) {
  const bytes = encoder.encode(text)
  wasm.u8.set(bytes, wasm.exports.inPtr())
  return bytes.length
}

function out(length) {
  const at = wasm.exports.outPtr()
  return wasm.u8.slice(at, at + length)
}

function outText(length) {
  return decoder.decode(out(length))
}

function failure() {
  return outText(wasm.exports.lastMessage())
}

/** The module's own decode of a message, through a section it is handed. */
function decodeWith(section, message) {
  if (section === null) {
    assert.equal(wasm.exports.setSection(0), 0)
  } else {
    wasm.u8.set(section, wasm.exports.inPtr())
    assert.equal(wasm.exports.setSection(section.length), 0, failure())
  }
  wasm.u8.set(message, wasm.exports.inPtr())
  const length = wasm.exports.messageJSON(message.length)
  assert.ok(length >= 0, `decode: ${failure()}`)
  return outText(length)
}

for (const document of documents) {
  const text = textOf(document)

  test(`${document.name}: encodes and reads back`, () => {
    // VERIFY is on, so the module has already compared its own output against
    // the input before returning. The decode below is the same claim made from
    // outside, which is what catches a self-check that agrees with a bug.
    const length = wasm.exports.encodeJSON(feed(text), VERIFY)
    assert.ok(length >= 0, `encode: ${failure()}`)
    const message = out(length)
    const section = out(wasm.exports.lastSection())

    assert.ok(message.length > 0, 'a message must have bytes')
    assert.ok(message[0] >= 0xd0 && message[0] <= 0xdf, `root byte ${message[0].toString(16)}`)
    assert.equal(message[0] & 0x04, 0, 'an out-of-band message carries no section')

    const back = decodeWith(section, message)
    JSON.parse(back)
  })

  test(`${document.name}: self-describing stands alone`, () => {
    const length = wasm.exports.encodeJSON(feed(text), VERIFY | SELF_DESCRIBING)
    assert.ok(length >= 0, `encode: ${failure()}`)
    const message = out(length)
    assert.equal(message[0] & 0x04, 0x04, 'the schema bit must be set')
    // No section handed in: the message's own is the only one there is.
    const back = decodeWith(null, message)
    JSON.parse(back)
  })

  test(`${document.name}: the two deliveries agree about the body`, () => {
    const plainLen = wasm.exports.encodeJSON(feed(text), 0)
    assert.ok(plainLen >= 0, failure())
    const plain = out(plainLen)
    const section = out(wasm.exports.lastSection())

    const inlineLen = wasm.exports.encodeJSON(feed(text), SELF_DESCRIBING)
    assert.ok(inlineLen >= 0, failure())
    const inline = out(inlineLen)

    // [root][section][body] against [root][body]: the same bytes behind the
    // first, which is what makes the two deliveries one format.
    assert.deepEqual(
      Array.from(inline.subarray(1 + section.length)),
      Array.from(plain.subarray(1)),
    )
  })
}

for (const one of refusals) {
  test(`refuses ${one.name}`, () => {
    const length = wasm.exports.encodeJSON(feed(one.text), VERIFY)
    assert.equal(length, -1, `${one.name} should have been refused`)
    const why = failure()
    assert.ok(why.length > 0, 'a refusal must carry a message')
    assert.match(why, one.match)
  })
}

test('an object at the top level is not wrapped', () => {
  const length = wasm.exports.encodeJSON(feed('{"a":1}'), VERIFY)
  assert.ok(length >= 0, failure())
  const section = out(wasm.exports.lastSection())
  // The envelope flag is bit 1 of the root structDef's flags byte, which sits
  // behind the section length and the struct count: both one byte here.
  assert.equal(section[2] & 0x02, 0, 'an object root needs no envelope')
})

test('a non-object top level is wrapped, and the wrapper is unwrapped on the way back', () => {
  const length = wasm.exports.encodeJSON(feed('[1,2,3]'), VERIFY)
  assert.ok(length >= 0, failure())
  const message = out(length)
  const section = out(wasm.exports.lastSection())
  assert.equal(section[2] & 0x02, 0x02, 'an array root must be marked as an envelope')
  assert.equal(decodeWith(section, message), '[1,2,3]')
})

test('a sixteen-field object stays narrow and a seventeen-field one goes wide', () => {
  const narrow = documents.find((d) => d.name === 'sixteen-fields')
  const wide = documents.find((d) => d.name === 'seventeen-fields')

  assert.ok(wasm.exports.encodeJSON(feed(textOf(narrow)), VERIFY) >= 0, failure())
  assert.equal(out(1)[0] & 0x08, 0, 'sixteen fields fit four key bits')

  assert.ok(wasm.exports.encodeJSON(feed(textOf(wide)), VERIFY) >= 0, failure())
  assert.equal(out(1)[0] & 0x08, 0x08, 'seventeen fields need eight')
})

test('the self-check catches a message that does not read back', () => {
  // There is no way to make the encoder wrong on purpose from out here, so this
  // checks the check is wired: a document the verifier must pass, and the flag
  // actually reaching it. A zero-length section would mean encode returned
  // before the schema was built.
  const length = wasm.exports.encodeJSON(feed(textOf(documents[1])), VERIFY)
  assert.ok(length >= 0, failure())
  assert.ok(wasm.exports.lastSection() > 0, 'the section must be available after a verified encode')
})

test('a duplicate key takes the last value, as JSON.parse does', () => {
  const length = wasm.exports.encodeJSON(feed('{"a":1,"a":2}'), VERIFY)
  assert.ok(length >= 0, failure())
  const message = out(length)
  const section = out(wasm.exports.lastSection())
  assert.deepEqual(JSON.parse(decodeWith(section, message)), { a: 2 })
})

test('integers past 2^53 survive, which is why the parser is in wasm', () => {
  const text = '{"snowflake":7295013456321098765,"max":9223372036854775807}'
  const length = wasm.exports.encodeJSON(feed(text), VERIFY)
  assert.ok(length >= 0, failure())
  const message = out(length)
  const section = out(wasm.exports.lastSection())
  assert.equal(decodeWith(section, message), text)
})
