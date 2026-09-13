// The scanner against Go: exact integers, correctly-rounded floats, and the
// escape rules encoding/json defines - including lone surrogates becoming
// U+FFFD, so that what reaches the encoder is always valid UTF-8.
//
// json.ts and decimal.ts are the two files the format change does not touch, so
// this is the one suite that survives the re-port intact (REFACTOR_PLAN.md 2).
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { load, vectors, hex } from './harness.mjs'

const wasm = await load()
const numbers = await vectors('numbers')
const strings = await vectors('texts')

const K = { NULL: 0, BOOL: 1, INT: 2, UINT: 3, FLOAT: 4, STRING: 5, ARRAY: 6, OBJECT: 7 }

function parse(text) {
  const src = Buffer.from(text, 'utf8')
  wasm.u8.set(src, wasm.exports.inPtr())
  return wasm.exports.jsonParse(src.length)
}

function errorMessage() {
  const len = wasm.exports.jsonErrMessage()
  return Buffer.from(wasm.u8.subarray(wasm.exports.outPtr(), wasm.exports.outPtr() + len)).toString()
}

for (const c of numbers) {
  test(`number ${c.name} = ${c.literal}`, () => {
    const kind = parse(c.literal)

    if (c.kind === 'error') {
      assert.ok(kind < 0, `expected a diagnostic, got kind ${kind}`)
      assert.ok(errorMessage().length > 0, 'a diagnostic must carry a message')
      return
    }
    assert.ok(kind >= 0, `parse failed: ${errorMessage()}`)

    if (c.kind === 'int') {
      assert.equal(kind, K.INT)
      assert.equal(wasm.exports.jsonRootNum(), BigInt(c.value))
    } else if (c.kind === 'uint') {
      assert.equal(kind, K.UINT)
      assert.equal(BigInt.asUintN(64, wasm.exports.jsonRootNum()), BigInt(c.value))
    } else {
      assert.equal(kind, K.FLOAT)
      const bits = BigInt.asUintN(64, wasm.exports.jsonRootNum())
      assert.equal(bits.toString(16).padStart(16, '0'), c.bits, 'float64 bits must match Go exactly')
    }
  })
}

for (const c of strings) {
  test(`string ${c.name}`, () => {
    const kind = parse(c.literal)
    assert.ok(kind >= 0, `parse failed: ${errorMessage()}`)
    assert.equal(kind, K.STRING)
    const len = wasm.exports.jsonRootString()
    const got = wasm.u8.subarray(wasm.exports.outPtr(), wasm.exports.outPtr() + len)
    assert.equal(hex(got), hex(Buffer.from(c.decoded, 'base64')))
  })
}

test('structure: objects keep first-seen key order', () => {
  assert.equal(parse('{"z":1,"a":2,"m":3}'), K.OBJECT)
  assert.equal(wasm.exports.jsonRootCount(), 3)
  const keys = [0, 1, 2].map((i) => {
    const len = wasm.exports.jsonChildKey(i)
    return Buffer.from(wasm.u8.subarray(wasm.exports.outPtr(), wasm.exports.outPtr() + len)).toString()
  })
  assert.deepEqual(keys, ['z', 'a', 'm'])
})

test('structure: nested arrays and objects', () => {
  assert.equal(parse('[{"a":[1,2]},null,true]'), K.ARRAY)
  assert.equal(wasm.exports.jsonRootCount(), 3)
  assert.equal(wasm.exports.jsonChildKind(0), K.OBJECT)
  assert.equal(wasm.exports.jsonChildKind(1), K.NULL)
  assert.equal(wasm.exports.jsonChildKind(2), K.BOOL)
})

// A raw control character inside a string is illegal JSON; built here rather
// than written literally so this file stays printable ASCII.
const CTRL = '"a' + String.fromCharCode(1) + 'b"'

const malformed = [
  ['unterminated string', '"abc'],
  ['trailing comma', '[1,2,]'],
  ['missing colon', '{"a" 1}'],
  ['bare word', 'nul'],
  ['trailing content', '{} {}'],
  ['control char in string', CTRL],
  ['leading zero', '01'],
  ['bare minus', '-'],
  ['exponent with no digits', '1e'],
  ['point with no digits', '1.'],
  ['empty input', ''],
]

for (const [name, text] of malformed) {
  test(`rejects ${name}`, () => {
    assert.ok(parse(text) < 0, `${name} should have been rejected`)
    assert.ok(wasm.exports.jsonErrOffset() >= 0, 'a diagnostic must carry an offset')
  })
}

test('rejects nesting past the depth limit', () => {
  assert.ok(parse('['.repeat(200) + ']'.repeat(200)) < 0)
  assert.match(errorMessage(), /nesting deeper/)
})

test('diagnostics carry a line number', () => {
  assert.ok(parse('{\n  "a": 1,\n  "b": @\n}') < 0)
  assert.equal(wasm.exports.jsonErrLine(), 3)
})
