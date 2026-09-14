// The materializer: the wasm `materialize` export, and the JS reader in
// `src/materialize.ts` that turns its buffer into rows.
//
// Node 24 strips `.ts` types natively, so this imports the real module the
// page ships rather than a reimplementation of it — a bug in the generated
// row builder's codegen is a bug this test can actually catch.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { load, put, take, failure, must } from './harness.mjs'
import { documents, textOf } from './documents.mjs'
import { readRows } from '../src/materialize.ts'

const wasm = await load()
const encoder = new TextEncoder()
const VERIFY = 2

function findDoc(name) {
  const doc = documents.find((d) => d.name === name)
  if (!doc) throw new Error(`no fixture named ${name}`)
  return doc
}

/** Encodes JSON text out-of-band and returns the message and its section. */
function buildMessage(text) {
  const at = put(wasm, encoder.encode(text))
  const length = must(wasm, wasm.exports.encode(at, VERIFY), 'encode')
  const message = take(wasm, length)
  const section = take(wasm, wasm.exports.section())
  return { message, section }
}

/** `materialize()`'s return: the buffer length, 0 (not this shape), or -1. */
function materializeLength(message, section) {
  must(wasm, wasm.exports.set_schema(put(wasm, section)), 'set_schema')
  return wasm.exports.materialize(put(wasm, message))
}

test('a bare-array table materializes to the same rows as its JSON', () => {
  const doc = findDoc('records')
  const { message, section } = buildMessage(textOf(doc))
  const length = materializeLength(message, section)
  assert.ok(length > 0, `expected a table buffer, got length ${length}`)
  const rows = readRows(take(wasm, length).buffer)
  assert.deepEqual(rows, doc.json)
})

for (const name of [
  'flat-object',
  'records-past-the-table-threshold',
  'records-under-the-table-threshold',
]) {
  test(`${name}: not this fast path's shape, materialize signals 0`, () => {
    const doc = findDoc(name)
    const { message, section } = buildMessage(textOf(doc))
    assert.equal(materializeLength(message, section), 0)
  })
}

test('a non-ASCII table exercises the per-string decode fallback', () => {
  // An all-ASCII corpus would never reach the byte-offset-vs-UTF-16-offset
  // trap §5.3 calls out, so this is its own fixture rather than reused text.
  const json = JSON.stringify(
    Array.from({ length: 9 }, (_, i) => ({
      id: i,
      label: ['el niño comió', '日本語', 'party 🎉', 'plain'][i % 4],
    })),
  )
  const { message, section } = buildMessage(json)
  const length = materializeLength(message, section)
  assert.ok(length > 0, `expected a table buffer, got length ${length}`)
  const rows = readRows(take(wasm, length).buffer)
  assert.deepEqual(rows, JSON.parse(json))
})

test('a table column past 2^53 comes back as an exact bigint', () => {
  const snowflake = 9007199254740993n
  const json = JSON.stringify(
    Array.from({ length: 9 }, (_, i) => ({ id: i, big: 0 })),
  ).replaceAll('"big":0', `"big":${snowflake}`)
  const { message, section } = buildMessage(json)
  const length = materializeLength(message, section)
  assert.ok(length > 0, `expected a table buffer, got length ${length}`)
  const rows = readRows(take(wasm, length).buffer)
  assert.equal(rows.length, 9)
  for (const row of rows) {
    assert.equal(typeof row.big, 'bigint')
    assert.equal(row.big, snowflake)
    assert.equal(typeof row.id, 'number')
  }
})

test('the CSP fallback produces the same rows as the generated builder', () => {
  const doc = findDoc('records')
  const { message, section } = buildMessage(textOf(doc))
  const length = materializeLength(message, section)
  const buffer = take(wasm, length).buffer
  const generated = readRows(buffer, { allowGenerated: true })
  const fallback = readRows(buffer, { allowGenerated: false })
  assert.deepEqual(fallback, generated)
  assert.deepEqual(fallback, doc.json)
})

test('every prefix of a table message is a diagnostic, never a trap', () => {
  const doc = findDoc('records')
  const { message, section } = buildMessage(textOf(doc))
  must(wasm, wasm.exports.set_schema(put(wasm, section)), 'set_schema')
  for (let cut = 1; cut < message.length; cut++) {
    const got = wasm.exports.materialize(put(wasm, message.subarray(0, cut)))
    if (got > 0) {
      readRows(take(wasm, got).buffer) // must not throw
    } else if (got < 0) {
      failure(wasm) // must have a diagnostic, not throw reading it
    }
  }
})
