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
import { readRows, readColumns } from '../src/materialize.ts'
import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { root } from './harness.mjs'

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

test('a root object whose one field is a table materializes to that object', () => {
  const doc = findDoc('records-past-the-table-threshold')
  const { message, section } = buildMessage(textOf(doc))
  const length = materializeLength(message, section)
  assert.ok(length > 0, `expected a table buffer, got length ${length}`)
  const buffer = take(wasm, length).buffer
  assert.deepEqual(readRows(buffer), doc.json)
  assert.deepEqual(readRows(buffer, { allowGenerated: false }), doc.json)
  const columns = readColumns(buffer)
  assert.equal(columns.field, 'rows')
  assert.equal(columns.rowCount, doc.json.rows.length)
})

for (const name of ['flat-object', 'records-under-the-table-threshold']) {
  test(`${name}: not this fast path's shape, materialize signals 0`, () => {
    const doc = findDoc(name)
    const { message, section } = buildMessage(textOf(doc))
    assert.equal(materializeLength(message, section), 0)
  })
}

test('a non-ASCII table exercises the per-string decode fallback', () => {
  // An all-ASCII corpus would never reach the byte-offset-vs-UTF-16-offset
  // trap, so this is its own fixture rather than reused text.
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

// Go's corpus: every document Go wrote whose root the materializer covers must
// read back as Go's own JSON — nullable, dictionary and grouped tables among
// them, which only Go's writer produces.
const unhex = (text) => Uint8Array.from(text.match(/../g) ?? [], (b) => parseInt(b, 16))
const corpus = JSON.parse(readFileSync(join(root, '..', 'rust', 'vectors', 'vectors.json'), 'utf8'))
const walks = corpus.walks.filter((w) => w.name.startsWith('doc.'))

/** Numbers past 2^53 come back as bigint here and rounded from JSON.parse. */
const rounded = (value) =>
  JSON.parse(JSON.stringify(value, (_, v) => (typeof v === 'bigint' ? Number(v) : v)))

test("Go's table documents materialize to Go's JSON, generated and fallback builders", () => {
  const covered = []
  for (const walk of walks) {
    const length = materializeLength(unhex(walk.message), unhex(walk.section))
    assert.ok(length >= 0, `${walk.name}: materialize failed`)
    if (length === 0) continue
    covered.push(walk.name)
    const buffer = take(wasm, length).buffer
    const want = JSON.parse(walk.json)
    assert.deepEqual(rounded(readRows(buffer)), want, `${walk.name}: generated builder`)
    // Same keys in the same order as the JSON path, which deepEqual does not check.
    assert.equal(JSON.stringify(rounded(readRows(buffer))), JSON.stringify(want), `${walk.name}: key order`)
    assert.deepEqual(
      rounded(readRows(buffer, { allowGenerated: false })),
      want,
      `${walk.name}: fallback builder`,
    )
  }
  for (const name of [
    'doc.nullable-columns',
    'doc.dictionary-columns',
    'doc.grouped-records',
    'doc.paged-table',
  ]) {
    assert.ok(covered.includes(name), `${name} no longer materializes: ${covered}`)
  }
})

test('a nullable column reads as null in rows and in columns', () => {
  const walk = walks.find((w) => w.name === 'doc.nullable-columns')
  const length = materializeLength(unhex(walk.message), unhex(walk.section))
  const buffer = take(wasm, length).buffer
  const want = JSON.parse(walk.json)
  const table = readColumns(buffer)
  assert.ok(table, 'one table, so a column table')
  const wantRows = Array.isArray(want) ? want : Object.values(want)[0]
  let nulls = 0
  for (const column of table.columns) {
    for (let i = 0; i < table.rowCount; i++) {
      const expected = wantRows[i][column.name]
      const isNull = column.valid ? ((column.valid[i >> 3] >> (i & 7)) & 1) === 0 : false
      assert.equal(isNull, expected === null, `${column.name}[${i}] validity`)
      if (isNull) {
        nulls++
        if (column.kind === 'bool' || column.kind === 'string') assert.equal(column.values[i], null)
      } else {
        assert.equal(Number(column.values[i]), Number(expected), `${column.name}[${i}]`)
      }
    }
  }
  assert.ok(nulls > 0, 'the fixture has nulls')
})
