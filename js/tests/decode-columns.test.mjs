// columns() against decode(): for every message whose root is one table, each
// column's row i is what decode()'s row i holds under that key, a null row is a
// clear bit in `valid` (and null in a bool or string column), and a message
// that is not one table gives null.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import { join } from 'node:path'
import { root, vectors, unhex } from './harness.mjs'
import { Decoder, columns } from '../src/decode/index.ts'
import { Walker } from '../src/decode/walk.ts'
import { parseSchema } from '../src/decode/schema.ts'

const cases = [...(await vectors('types')), ...(await vectors('packed'))]
const rust = JSON.parse(await readFile(join(root, '..', 'rust', 'vectors', 'vectors.json'), 'utf8'))
const all = [
  ...cases.map((c) => ({ name: c.name, section: c.section, message: c.message })),
  ...rust.walks.filter((c) => c.section).map((c) => ({ name: `walk ${c.name}`, section: c.section, message: c.message })),
]

function rowsOf(value, table) {
  if (Array.isArray(value)) return value
  return table.field !== undefined ? value[table.field] : null
}

test('columns() matches decode() row by row', () => {
  let tables = 0
  for (const c of all) {
    const decoder = new Decoder()
    decoder.setSchema(unhex(c.section))
    const bytes = unhex(c.message)
    const table = decoder.columns(bytes)
    if (table === null) continue
    tables++
    const rows = rowsOf(decoder.decode(bytes), table)
    assert.ok(Array.isArray(rows), `${c.name}: decode() holds the table`)
    assert.equal(table.rowCount, rows.length, `${c.name}: rowCount`)
    for (const column of table.columns) {
      assert.equal(column.length, rows.length, `${c.name}.${column.name}: length`)
      assert.equal(column.values.length, rows.length, `${c.name}.${column.name}: values`)
      for (let i = 0; i < rows.length; i++) {
        const want = rows[i][column.name]
        const valid = column.valid === undefined || ((column.valid[i >> 3] >> (i & 7)) & 1) === 1
        const got = column.values[i]
        const what = `${c.name}.${column.name}[${i}]`
        if (want === null) {
          assert.ok(column.valid !== undefined && !valid, `${what}: a null row is a clear bit`)
          if (column.kind === 'bool' || column.kind === 'string') assert.equal(got, null, what)
          continue
        }
        assert.ok(valid, `${what}: a value row is a set bit`)
        if (column.kind === 'bigint') assert.equal(BigInt.asIntN(64, BigInt(want)) === BigInt.asIntN(64, got), true, what)
        else assert.ok(Object.is(got, want) || got === want, `${what}: ${got} != ${want}`)
      }
    }
  }
  assert.ok(tables > 0, 'some vector is a table')
})

test('a message that is not one table gives null, never a throw', () => {
  for (const c of all) {
    const decoder = new Decoder()
    decoder.setSchema(unhex(c.section))
    const table = decoder.columns(unhex(c.message))
    if (table === null) continue
    assert.ok(Array.isArray(table.columns))
  }
  // A dynamic document holds no table.
  assert.equal(columns(new Uint8Array([0xd1, 0xa1, 0x61, 0x01])), null)
})

// The vectors put a table beside other fields, which columns() does not take;
// walking with the column mode on keeps every root-level table as columns
// wherever it sits, which covers grouped, nullable, wide and paged tables.

function columnsTree(c) {
  const schema = parseSchema(unhex(c.section))
  const bytes = unhex(c.message)
  const walker = new Walker(bytes, schema, undefined)
  walker.columnar = true
  if (bytes[0] >= 0xd0 && bytes[0] <= 0xdf) {
    if (bytes[0] !== 0xd0 && bytes[0] !== 0xd8) return null
    return walker.root(1, bytes.length, bytes[0] === 0xd8)
  }
  return walker.root(0, bytes.length, false)
}

function compareColumns(name, table, rows) {
  assert.equal(table.rowCount, rows.length, `${name}: rowCount`)
  for (const column of table.columns) {
    for (let i = 0; i < rows.length; i++) {
      const want = rows[i][column.name]
      const valid = column.valid === undefined || ((column.valid[i >> 3] >> (i & 7)) & 1) === 1
      const got = column.values[i]
      const what = `${name}.${column.name}[${i}]`
      if (want === null) {
        assert.ok(column.valid !== undefined && !valid, `${what}: a null row is a clear bit`)
        continue
      }
      assert.ok(valid, `${what}: a value row is a set bit`)
      if (column.kind === 'bigint') assert.equal(BigInt(want), got, what)
      else assert.ok(Object.is(got, want), `${what}: ${got} != ${want}`)
    }
  }
}

test('every root-level table as columns matches its rows', () => {
  let found = 0
  for (const c of all) {
    let tree
    try {
      tree = columnsTree(c)
    } catch {
      continue
    }
    if (tree === null || typeof tree !== 'object') continue
    const decoder = new Decoder()
    decoder.setSchema(unhex(c.section))
    const value = decoder.decode(unhex(c.message))
    for (const key of Object.keys(tree)) {
      const table = tree[key]
      if (table === null || typeof table !== 'object' || !Array.isArray(table.columns)) continue
      found++
      compareColumns(`${c.name}.${key}`, table, value[key])
    }
  }
  assert.ok(found >= 8, `found ${found} tables`)
})
