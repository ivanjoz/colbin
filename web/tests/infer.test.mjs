// Inference (PLAN.md 3) and enforcement (4).
//
// The field-id cases are the ones that matter most: the ids come from a real
// colbin schema section, not from a second copy of the hash, so this is the
// port against the library rather than against itself.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { load, vectors } from './harness.mjs'

const wasm = await load()
const fieldIds = await vectors('fieldids.json')

const FT = { INT: 0, FLOAT: 1, STRING: 2, BYTES: 3, ARRAY: 4, STRUCT: 5, MAP: 6, ANY: 7 }
const SK = { BOOL: 0, INT64: 4, UINT64: 9, FLOAT64: 12 }
const SHAPE = { RECORDS: 0, SINGLE: 1, VALUE: 2 }

function infer(text) {
  const src = Buffer.from(text, 'utf8')
  wasm.u8.set(src, wasm.exports.inPtr())
  return wasm.exports.inferJSON(src.length)
}

function out(len) {
  return Buffer.from(wasm.u8.subarray(wasm.exports.outPtr(), wasm.exports.outPtr() + len)).toString()
}

const message = () => out(wasm.exports.jsonErrMessage())
const diagPath = () => out(wasm.exports.diagPath())
const warnings = () =>
  Array.from({ length: wasm.exports.diagWarningCount() }, (_, i) => out(wasm.exports.diagWarning(i)))

// A path is up to 8 field indices packed one per byte.
const P = (...steps) => [steps.reduce((acc, s, i) => acc | (BigInt(s) << BigInt(i * 8)), 0n), steps.length]
const fieldName = (path, depth, i) => out(wasm.exports.schemaFieldName(path, depth, i))

// ---- field ids, against the library ----------------------------------------

for (const c of fieldIds) {
  test(`field ids: ${c.name}`, () => {
    const buf = []
    for (const n of c.names) {
      const b = Buffer.from(n, 'utf8')
      assert.ok(b.length < 256, 'test encoding uses a one-byte length')
      buf.push(b.length, ...b)
    }
    wasm.u8.set(Uint8Array.from(buf), wasm.exports.inPtr())
    const n = wasm.exports.fieldIdsFor(c.names.length)
    assert.equal(n, c.names.length, 'id assignment reported a duplicate')
    const got = Array.from(wasm.u8.subarray(wasm.exports.outPtr(), wasm.exports.outPtr() + n))
    assert.deepEqual(got, c.ids)
  })
}

// ---- shape ------------------------------------------------------------------

test('an array of objects is records mode', () => {
  assert.equal(infer('[{"a":1},{"a":2}]'), SHAPE.RECORDS)
  assert.equal(wasm.exports.inferRecordCount(), 2)
})

test('a lone object is one record, rendered back as an object', () => {
  assert.equal(infer('{"a":1}'), SHAPE.SINGLE)
  assert.equal(wasm.exports.inferRecordCount(), 1)
})

test('an empty array has no shape', () => {
  assert.ok(infer('[]') < 0)
  assert.match(message(), /empty array/)
})

test('null has no shape', () => {
  assert.ok(infer('null') < 0)
})

// ---- types ------------------------------------------------------------------

test('scalar columns', () => {
  assert.equal(infer('[{"i":1,"s":"x","b":true,"f":1.5}]'), SHAPE.RECORDS)
  const [p, d] = P()
  assert.equal(wasm.exports.schemaFieldCount(p, d), 4)
  const kinds = [0, 1, 2, 3].map((i) => {
    const [pp, dd] = P(i)
    return [fieldName(p, d, i), wasm.exports.schemaFt(pp, dd), wasm.exports.schemaKind(pp, dd)]
  })
  assert.deepEqual(kinds, [
    ['i', FT.INT, SK.INT64],
    ['s', FT.STRING, SK.INT64],
    ['b', FT.INT, SK.BOOL],
    ['f', FT.FLOAT, SK.FLOAT64],
  ])
})

test('one float promotes the whole column', () => {
  assert.equal(infer('[{"v":1},{"v":2.5},{"v":3}]'), SHAPE.RECORDS)
  const [p, d] = P(0)
  assert.equal(wasm.exports.schemaFt(p, d), FT.FLOAT)
  assert.equal(wasm.exports.schemaKind(p, d), SK.FLOAT64)
})

test('integers past int64 make the column uint64', () => {
  assert.equal(infer('[{"v":1},{"v":18446744073709551615}]'), SHAPE.RECORDS)
  const [p, d] = P(0)
  assert.equal(wasm.exports.schemaKind(p, d), SK.UINT64)
})

test('a column spanning both signed and unsigned extremes is refused', () => {
  assert.ok(infer('[{"v":-1},{"v":18446744073709551615}]') < 0)
  assert.match(message(), /no single integer column/)
})

test('integers are never narrowed', () => {
  assert.equal(infer('[{"v":1},{"v":2}]'), SHAPE.RECORDS)
  const [p, d] = P(0)
  assert.equal(wasm.exports.schemaKind(p, d), SK.INT64)
})

test('nested objects become struct columns', () => {
  assert.equal(infer('[{"c":{"ruc":"20","name":"ACME"}}]'), SHAPE.RECORDS)
  const [p, d] = P(0)
  assert.equal(wasm.exports.schemaFt(p, d), FT.STRUCT)
  assert.equal(wasm.exports.schemaFieldCount(p, d), 2)
  assert.equal(fieldName(p, d, 0), 'ruc')
})

test('arrays of objects become array-of-struct columns', () => {
  assert.equal(infer('[{"lines":[{"sku":"A"},{"sku":"B"}]}]'), SHAPE.RECORDS)
  const [p, d] = P(0)
  assert.equal(wasm.exports.schemaFt(p, d), FT.ARRAY)
  // The array step consumes a level; the element type is the struct.
  const [pe, de] = P(0, 0)
  assert.equal(wasm.exports.schemaFt(pe, de), FT.STRUCT)
  assert.equal(fieldName(pe, de, 0), 'sku')
})

test('the element type unifies across every array of every record', () => {
  assert.equal(infer('[{"v":[1,2]},{"v":[3.5]}]'), SHAPE.RECORDS)
  const [pe, de] = P(0, 0)
  assert.equal(wasm.exports.schemaFt(pe, de), FT.FLOAT)
})

// ---- nullability -------------------------------------------------------------

test('an explicit null makes the column nullable', () => {
  assert.equal(infer('[{"a":1},{"a":null}]'), SHAPE.RECORDS)
  const [p, d] = P(0)
  assert.equal(wasm.exports.schemaNullable(p, d), 1)
  assert.equal(wasm.exports.schemaFt(p, d), FT.INT)
})

test('a key missing from one record makes the column nullable', () => {
  assert.equal(infer('[{"a":1,"b":2},{"a":3}]'), SHAPE.RECORDS)
  const [pb, db] = P(1)
  assert.equal(wasm.exports.schemaNullable(pb, db), 1)
  const [pa, da] = P(0)
  assert.equal(wasm.exports.schemaNullable(pa, da), 0)
})

test('a column that is only ever null falls back to a nullable string, with a warning', () => {
  assert.equal(infer('[{"a":null},{"a":null}]'), SHAPE.RECORDS)
  const [p, d] = P(0)
  assert.equal(wasm.exports.schemaFt(p, d), FT.STRING)
  assert.equal(wasm.exports.schemaNullable(p, d), 1)
  assert.match(warnings().join(' '), /only ever null/)
})

test('an always-empty array warns about its element type', () => {
  assert.equal(infer('[{"a":[]},{"a":[]}]'), SHAPE.RECORDS)
  assert.match(warnings().join(' '), /always an empty array/)
})

// ---- conflicts ---------------------------------------------------------------

test('a field that changes type is refused, naming both sides', () => {
  assert.ok(infer('[{"qty":1},{"qty":2},{"qty":"x"}]') < 0)
  assert.match(message(), /type conflict: string/)
  assert.match(message(), /records 0-1 had number/)
  assert.equal(diagPath(), '[2].qty')
})

test('a conflict deeper in the shape reports its path', () => {
  assert.ok(infer('[{"c":{"v":1}},{"c":{"v":[2]}}]') < 0)
  assert.equal(diagPath(), '[1].c.v')
})

test('a conflict inside an array reports its index', () => {
  assert.ok(infer('[{"v":[1,"x"]}]') < 0)
  assert.match(message(), /type conflict/)
})

test('object versus scalar is a conflict', () => {
  assert.ok(infer('[{"v":{"a":1}},{"v":3}]') < 0)
  assert.match(message(), /type conflict: number/)
})

// ---- limits ------------------------------------------------------------------

test('254 fields is allowed and 255 is not', () => {
  const ok = {}
  for (let i = 0; i < 254; i++) ok['f' + i] = i
  assert.equal(infer(JSON.stringify([ok])), SHAPE.RECORDS)
  const [p, d] = P()
  assert.equal(wasm.exports.schemaFieldCount(p, d), 254)

  const tooMany = {}
  for (let i = 0; i < 255; i++) tooMany['f' + i] = i
  assert.ok(infer(JSON.stringify([tooMany])) < 0)
  assert.match(message(), /more than 254 fields/)
})

test('field order is first-seen across records, not per record', () => {
  assert.equal(infer('[{"b":1},{"a":2,"b":3}]'), SHAPE.RECORDS)
  const [p, d] = P()
  assert.equal(fieldName(p, d, 0), 'b')
  assert.equal(fieldName(p, d, 1), 'a')
})
