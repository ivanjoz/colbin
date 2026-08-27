// Decode: a message back to JSON.
//
// Compared against Go's DecodeJSON semantically, not textually, because the two
// order keys differently on purpose (PLAN.md 6) - and compared with integers
// kept as BigInt, so a value past 2^53 cannot pass by both sides being wrong
// in the same way.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { load, vectors } from './harness.mjs'

const wasm = await load()
const cases = await vectors('messages.json')

function out(len) {
  return Buffer.from(wasm.u8.subarray(wasm.exports.outPtr(), wasm.exports.outPtr() + len)).toString()
}

/** Parses JSON keeping integers exact and sorting object keys, for comparison. */
function canonical(text) {
  let i = 0
  const ws = () => {
    while (i < text.length && ' \t\n\r'.includes(text[i])) i++
  }
  function value() {
    ws()
    const c = text[i]
    if (c === '{') {
      i++
      const entries = []
      ws()
      if (text[i] === '}') { i++; return { obj: entries } }
      for (;;) {
        ws()
        const k = value()
        ws()
        i++ // :
        entries.push([k, value()])
        ws()
        if (text[i] === ',') { i++; continue }
        i++ // }
        break
      }
      entries.sort((a, b) => (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0))
      return { obj: entries }
    }
    if (c === '[') {
      i++
      const items = []
      ws()
      if (text[i] === ']') { i++; return { arr: items } }
      for (;;) {
        items.push(value())
        ws()
        if (text[i] === ',') { i++; continue }
        i++ // ]
        break
      }
      return { arr: items }
    }
    if (c === '"') {
      const start = i
      i++
      while (text[i] !== '"') i += text[i] === '\\' ? 2 : 1
      i++
      return JSON.parse(text.slice(start, i))
    }
    const start = i
    while (i < text.length && !',]} \t\n\r'.includes(text[i])) i++
    const lit = text.slice(start, i)
    if (lit === 'true') return true
    if (lit === 'false') return false
    if (lit === 'null') return null
    // An integer literal stays exact; a real number compares as a float64.
    return /[.eE]/.test(lit) ? { f: Number(lit) } : { i: BigInt(lit).toString() }
  }
  return value()
}

function decodeMessage(hexText) {
  const msg = Uint8Array.from(Buffer.from(hexText, 'hex'))
  wasm.u8.set(msg, wasm.exports.inPtr())
  const len = wasm.exports.decodeMsg(msg.length)
  return { len, text: len >= 0 ? out(len) : null }
}

for (const c of cases) {
  test(`decode ${c.tier}/${c.name}`, () => {
    const r = decodeMessage(c.message)
    assert.ok(r.len >= 0, `decode failed: ${out(wasm.exports.jsonErrMessage())}`)
    assert.deepEqual(canonical(r.text), canonical(c.decoded))
  })

  test(`round trip ${c.tier}/${c.name}`, () => {
    const src = Buffer.from(c.json, 'utf8')
    wasm.u8.set(src, wasm.exports.inPtr())
    const encoded = wasm.exports.encodeJSON(src.length)
    assert.ok(encoded >= 0)
    const msg = wasm.u8.slice(wasm.exports.outPtr(), wasm.exports.outPtr() + encoded)
    wasm.u8.set(msg, wasm.exports.inPtr())
    const len = wasm.exports.decodeMsg(msg.length)
    assert.ok(len >= 0, `decode failed: ${out(wasm.exports.jsonErrMessage())}`)
    // Against Go's reading of the same bytes, so this is interoperation and
    // not the module agreeing with itself.
    assert.deepEqual(canonical(out(len)), canonical(c.decoded))
  })
}

test('key order is the wire order, not alphabetical', () => {
  const src = Buffer.from('[{"z":1,"a":2}]', 'utf8')
  wasm.u8.set(src, wasm.exports.inPtr())
  const encoded = wasm.exports.encodeJSON(src.length)
  const msg = wasm.u8.slice(wasm.exports.outPtr(), wasm.exports.outPtr() + encoded)
  wasm.u8.set(msg, wasm.exports.inPtr())
  const len = wasm.exports.decodeMsg(msg.length)
  assert.equal(out(len), '[{"z":1,"a":2}]')
})
