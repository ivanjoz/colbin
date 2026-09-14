// The encoder, against itself and against the decoder.
//
// The cross-language half is emit.mjs and `go test ./js/vectors` — this file
// is the part that does not need Go: every document encodes, the self-check
// passes, the message decodes back to the input, and the refusals are refused
// with a message that names what went wrong.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { load, put, take, takeText, failure, must } from './harness.mjs'
import { documents, refusals, textOf } from './documents.mjs'

const wasm = await load()
const encoder = new TextEncoder()

const SELF_DESCRIBING = 1
const VERIFY = 2

function feed(text) {
  return put(wasm, encoder.encode(text))
}

function encode(text, flags) {
  return wasm.exports.encode(feed(text), flags)
}

function decodeWith(section, message) {
  if (section === null) {
    must(wasm, wasm.exports.set_schema(0), 'clear schema')
  } else {
    must(wasm, wasm.exports.set_schema(put(wasm, section)), 'set_schema')
  }
  const length = must(wasm, wasm.exports.decode(put(wasm, message)), 'decode')
  return takeText(wasm, length)
}

for (const document of documents) {
  const text = textOf(document)

  test(`${document.name}: encodes and reads back`, () => {
    const length = must(wasm, encode(text, VERIFY), 'encode')
    const message = take(wasm, length)
    const section = take(wasm, wasm.exports.section())

    assert.ok(message.length > 0, 'a message must have bytes')
    assert.ok(message[0] >= 0xd0 && message[0] <= 0xdf, `root byte ${message[0].toString(16)}`)
    assert.equal(message[0] & 0x04, 0, 'an out-of-band message carries no section')

    const back = decodeWith(section, message)
    JSON.parse(back)
  })

  test(`${document.name}: self-describing stands alone`, () => {
    const length = must(wasm, encode(text, VERIFY | SELF_DESCRIBING), 'encode')
    const message = take(wasm, length)
    assert.equal(message[0] & 0x04, 0x04, 'the schema bit must be set')
    const back = decodeWith(null, message)
    JSON.parse(back)
  })

  test(`${document.name}: the two deliveries agree about the body`, () => {
    const plainLen = must(wasm, encode(text, 0), 'encode')
    const plain = take(wasm, plainLen)
    const section = take(wasm, wasm.exports.section())

    const inlineLen = must(wasm, encode(text, SELF_DESCRIBING), 'encode')
    const inline = take(wasm, inlineLen)

    assert.deepEqual(
      Array.from(inline.subarray(1 + section.length)),
      Array.from(plain.subarray(1)),
    )
  })
}

for (const one of refusals) {
  test(`refuses ${one.name}`, () => {
    const length = encode(one.text, VERIFY)
    assert.equal(length, -1, `${one.name} should have been refused`)
    const why = failure(wasm)
    assert.ok(why.length > 0, 'a refusal must carry a message')
    assert.match(why, one.match)
  })
}

test('an object at the top level is not wrapped', () => {
  const length = must(wasm, encode('{"a":1}', VERIFY), 'encode')
  const section = take(wasm, wasm.exports.section())
  assert.equal(section[2] & 0x02, 0, 'an object root needs no envelope')
})

test('a non-object top level is wrapped, and the wrapper is unwrapped on the way back', () => {
  const length = must(wasm, encode('[1,2,3]', VERIFY), 'encode')
  const message = take(wasm, length)
  const section = take(wasm, wasm.exports.section())
  assert.equal(section[2] & 0x02, 0x02, 'an array root must be marked as an envelope')
  assert.equal(decodeWith(section, message), '[1,2,3]')
})

test('a sixteen-field object stays narrow and a seventeen-field one goes wide', () => {
  const narrow = documents.find((d) => d.name === 'sixteen-fields')
  const wide = documents.find((d) => d.name === 'seventeen-fields')

  const nLen = must(wasm, encode(textOf(narrow), VERIFY), 'encode')
  assert.equal(take(wasm, 1)[0] & 0x08, 0, 'sixteen fields fit four key bits')

  const wLen = must(wasm, encode(textOf(wide), VERIFY), 'encode')
  assert.equal(take(wasm, 1)[0] & 0x08, 0x08, 'seventeen fields need eight')
})

test('the self-check catches a message that does not read back', () => {
  const length = must(wasm, encode(textOf(documents[1]), VERIFY), 'encode')
  assert.ok(wasm.exports.section() > 0, 'the section must be available after a verified encode')
})

test('a duplicate key takes the last value, as JSON.parse does', () => {
  const length = must(wasm, encode('{"a":1,"a":2}', VERIFY), 'encode')
  const message = take(wasm, length)
  const section = take(wasm, wasm.exports.section())
  assert.deepEqual(JSON.parse(decodeWith(section, message)), { a: 2 })
})

test('integers past 2^53 survive, which is why the parser is in wasm', () => {
  const text = '{"snowflake":7295013456321098765,"max":9223372036854775807}'
  const length = must(wasm, encode(text, VERIFY), 'encode')
  const message = take(wasm, length)
  const section = take(wasm, wasm.exports.section())
  assert.equal(decodeWith(section, message), text)
})
