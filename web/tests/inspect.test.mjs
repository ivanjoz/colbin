// The column inspector. Its spans are also a check on the decoder: if they do
// not tile the message exactly, something is being read twice or skipped.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { load, vectors } from './harness.mjs'

const wasm = await load()
const cases = await vectors('messages.json')

function inspect(hexText) {
  const msg = Uint8Array.from(Buffer.from(hexText, 'hex'))
  wasm.u8.set(msg, wasm.exports.inPtr())
  const len = wasm.exports.inspectMsg(msg.length)
  assert.ok(len >= 0, 'inspect failed')
  return JSON.parse(
    Buffer.from(wasm.u8.subarray(wasm.exports.outPtr(), wasm.exports.outPtr() + len)).toString()
  )
}

/** Depth-first list of every column in the tree. */
function flatten(columns) {
  return columns.flatMap((c) => [c, ...flatten(c.children)])
}

for (const c of cases) {
  test(`inspect ${c.tier}/${c.name}`, () => {
    const report = inspect(c.message)
    assert.equal(report.totalBytes, c.message.length / 2)
    assert.ok(report.schemaBytes > 0 && report.schemaBytes < report.totalBytes)
    assert.ok(report.columns.length > 0)

    for (const col of flatten(report.columns)) {
      assert.ok(col.end > col.start, `${col.name} has an empty span`)
      assert.ok(col.end <= report.totalBytes, `${col.name} runs past the message`)
      assert.equal(col.bytes, col.end - col.start)
      for (const child of col.children) {
        assert.ok(child.start >= col.start && child.end <= col.end,
          `${child.name} is not inside ${col.name}`)
      }
    }
  })

  test(`inspect spans tile the body: ${c.tier}/${c.name}`, () => {
    const report = inspect(c.message)
    // Top-level columns must cover the body end to end, with no gap and no
    // overlap: the body is exactly the record count plus the columns.
    let at = report.columns[0].start
    for (const col of report.columns) {
      assert.equal(col.start, at, `gap or overlap before ${col.name}`)
      at = col.end
    }
    assert.equal(at, report.totalBytes, 'columns do not reach the end of the message')
  })
}

test('the inspector names types the way the schema does', () => {
  const c = cases.find((x) => x.name === 'three-records')
  const report = inspect(c.message)
  const byName = Object.fromEntries(report.columns.map((col) => [col.name, col.type]))
  assert.deepEqual(byName, { id: 'int64', name: 'string', price: 'int64', active: 'bool' })
})

test('nested columns report their children', () => {
  const c = cases.find((x) => x.name === 'array-of-objects')
  const report = inspect(c.message)
  const lines = report.columns.find((col) => col.name === 'lines')
  assert.equal(lines.type, '[]struct')
  assert.deepEqual(lines.children.map((x) => x.name).sort(), ['qty', 'sku'])
})

test('a nullable column says so', () => {
  const c = cases.find((x) => x.name === 'explicit-null')
  const report = inspect(c.message)
  assert.equal(report.columns[0].nullable, true)
})
