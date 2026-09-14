// The field tree, and the one invariant that makes it trustworthy.
//
// **The spans tile the body exactly.** A key run has no padding and an omitted
// field writes nothing, so the fields a message *did* write are contiguous from
// the first byte of the body to the last. If this walk consumed a field
// differently from the decoder, a gap or an overlap appears here — which is why
// inspect being a second walk is safe rather than a liability.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { load, put, take, takeText, failure, must, vectors, unhex } from './harness.mjs'
import { documents, textOf } from './documents.mjs'

const wasm = await load({ inspect: true })
const goCases = await vectors('types')
const encoder = new TextEncoder()
const VERIFY = 2

function inspect(section, message) {
  if (section === null) {
    must(wasm, wasm.exports.set_schema(0), 'clear schema')
  } else {
    must(wasm, wasm.exports.set_schema(put(wasm, section)), 'set_schema')
  }
  const length = must(wasm, wasm.exports.inspect_message(put(wasm, message)), 'inspect')
  return JSON.parse(takeText(wasm, length))
}

function checkTiling(report, what) {
  const bodyStart = report.rootBytes + report.schemaBytes
  const bodyEnd = report.totalBytes
  walkSpans(report.fields, bodyStart, bodyEnd, what, true)
}

function walkSpans(nodes, from, to, what, exhaustive) {
  let cursor = from
  for (const node of nodes) {
    assert.ok(node.end >= node.start, `${what}: ${node.name} ends before it starts`)
    assert.ok(node.start >= from && node.end <= to, `${what}: ${node.name} escapes its parent`)
    assert.ok(
      node.start >= cursor,
      `${what}: ${node.name} at ${node.start} overlaps what came before, at ${cursor}`,
    )
    assert.equal(node.bytes, node.end - node.start, `${what}: ${node.name} bytes disagree`)
    cursor = node.end
    if (node.children.length > 0) {
      walkSpans(node.children, node.start, node.end, `${what} > ${node.name}`, false)
    }
  }
  if (exhaustive) {
    assert.equal(cursor, to, `${what}: the fields cover ${cursor - from} of ${to - from} bytes`)
  }
}

test('the Go corpus is covered', () => {
  assert.ok(goCases.length >= 20, `only ${goCases.length} cases`)
})

for (const c of goCases) {
  test(`${c.name}: the spans tile the body (Go's bytes)`, () => {
    const report = inspect(unhex(c.section), unhex(c.message))
    assert.equal(report.schemaBytes, 0, 'an out-of-band message carries no section')
    checkTiling(report, c.name)
  })

  test(`${c.name}: the spans tile the body (self-describing)`, () => {
    const report = inspect(null, unhex(c.selfDescribing))
    assert.ok(report.schemaBytes > 0, 'a self-describing message carries a section')
    checkTiling(report, c.name)
  })
}

for (const document of documents) {
  test(`${document.name}: the spans tile what the module wrote`, () => {
    const text = textOf(document)
    const length = must(
      wasm,
      wasm.exports.encode(put(wasm, encoder.encode(text)), VERIFY),
      'encode',
    )
    const message = take(wasm, length)
    const section = take(wasm, wasm.exports.section())
    checkTiling(inspect(section, message), document.name)
  })
}

test('a table reports its columns, not its rows', () => {
  const c = goCases.find((one) => one.name === 'table-wide')
  const report = inspect(unhex(c.section), unhex(c.message))
  const table = report.fields.find((f) => f.type === '[]struct')
  assert.ok(table, 'the table field should be in the tree')
  assert.equal(table.children.length, 3)
  for (const column of table.children) assert.match(column.type, / column$/)
  assert.equal(report.rows, 300)
})

test('a list reports its elements, and a long one collapses its tail', () => {
  const c = goCases.find((one) => one.name === 'list')
  const report = inspect(unhex(c.section), unhex(c.message))
  const list = report.fields.find((f) => f.type === '[]struct')
  assert.ok(list, 'the list field should be in the tree')
  assert.equal(list.children.length, 3, 'three elements, each with a span')
  for (const element of list.children) {
    assert.ok(element.children.length > 0, 'an element shows its own fields')
  }
})

test('the tree names the wire shape of every field', () => {
  const c = goCases.find((one) => one.name === 'scalars')
  const report = inspect(unhex(c.section), unhex(c.message))
  const named = Object.fromEntries(report.fields.map((f) => [f.name, f.type]))
  assert.equal(named.Flag, 'bool')
  assert.equal(named.Large, 'int64')
  assert.equal(named.Giant, 'uint64')
  assert.equal(named.Single, 'float32')
  assert.equal(named.Text, 'string')
  assert.equal(named.Blob, 'bytes')
})

test('a pointer field is marked optional', () => {
  const c = goCases.find((one) => one.name === 'optionals-zero')
  const report = inspect(unhex(c.section), unhex(c.message))
  assert.ok(report.fields.length > 0)
  for (const field of report.fields) assert.equal(field.optional, true)
})

test('an envelope says so, so the page can show the array it was given', () => {
  const length = must(wasm, wasm.exports.encode(put(wasm, encoder.encode('[1,2,3]')), VERIFY), 'encode')
  const message = take(wasm, length)
  const section = take(wasm, wasm.exports.section())
  const report = inspect(section, message)
  assert.equal(report.envelope, true)
  assert.equal(report.fields.length, 1)
  assert.equal(report.fields[0].name, 'rows')
})

test('inspect refuses what decode refuses, rather than trapping', () => {
  wasm.exports.set_schema(0)
  for (const bytes of [
    Uint8Array.from([]),
    Uint8Array.from([0x00]),
    Uint8Array.from([0xd1, 0x00]),
    Uint8Array.from([0xd4, 0xff]),
  ]) {
    assert.equal(wasm.exports.inspect_message(put(wasm, bytes)), -1)
    assert.ok(failure(wasm).length > 0)
  }
})

test('every truncation of a message is a diagnostic or a tiling tree', () => {
  for (const name of ['corpus-sale-table', 'corpus-product', 'list', 'table']) {
    const c = goCases.find((one) => one.name === name)
    const full = unhex(c.selfDescribing)
    for (let cut = 1; cut < full.length; cut++) {
      wasm.exports.set_schema(0)
      const got = wasm.exports.inspect_message(put(wasm, full.subarray(0, cut)))
      if (got < 0) continue
      const report = JSON.parse(takeText(wasm, got))
      walkSpans(report.fields, report.rootBytes + report.schemaBytes, cut, `${name}@${cut}`, false)
    }
  }
})
