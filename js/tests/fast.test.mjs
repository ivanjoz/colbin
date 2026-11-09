// The browser fast path (`marshal(value)` walking live objects) against the
// text path (`marshal(JSON.stringify(value))`), which is the oracle: the two
// must produce the same message, section and warnings, byte for byte, and
// whatever the walk cannot reproduce must fall back rather than differ.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Codec } from '../dist/node.js'
import { documents } from './documents.mjs'

const codec = await Codec.open()

function both(value, options) {
  const fast = codec.marshal(value, options)
  const path = codec.lastEncodePath
  const text = codec.marshal(JSON.stringify(value), options)
  assert.equal(codec.lastEncodePath, 'json')
  assert.deepEqual(fast.message, text.message, 'message')
  assert.deepEqual(fast.section, text.section, 'section')
  assert.deepEqual(fast.warnings, text.warnings, 'warnings')
  return path
}

function same(value) {
  let a, b
  try { a = codec.marshal(value) } catch (e) { a = String(e.message) }
  try { b = codec.marshal(JSON.stringify(value)) } catch (e) { b = String(e.message) }
  if (typeof a === 'string' || typeof b === 'string') return assert.equal(a, b)
  assert.deepEqual(a.message, b.message)
  assert.deepEqual(a.section, b.section)
}

test('the corpus encodes identically, on the fast path', () => {
  let fast = 0
  for (const d of documents) {
    if (d.json === undefined || typeof d.json === 'string') continue
    let text
    try { text = JSON.stringify(d.json) } catch { continue } // BigInt: text-only
    let path
    try { path = both(d.json) } catch (e) {
      // A refusal must be the same refusal.
      assert.throws(() => codec.marshal(text), { message: e.message }, d.name)
      continue
    }
    if (path === 'doc') fast++
  }
  assert.ok(fast >= 10, `only ${fast} documents took the fast path`)
})

test('ordinary documents take the fast path', () => {
  const rows = Array.from({ length: 50 }, (_, i) => ({
    id: i, price: i * 0.25 + 0.1, name: `p${i}`, tags: [i, i + 1], note: i % 3 ? 'x' : null,
    seller: { name: `s${i % 4}`, rating: i % 5 },
  }))
  assert.equal(both(rows), 'doc')
  assert.equal(both({ a: 1, b: 'x', c: true }), 'doc')
  assert.equal(both(rows, { verify: true }), 'doc')
})

test('a generated sweep agrees with the text path', () => {
  let seed = 12345
  const rnd = (n) => { seed = (seed * 1103515245 + 12345) & 0x7fffffff; return seed % n }
  const scalar = (kind) => [
    () => rnd(1000) - 500, () => rnd(100) / 4, () => 's' + rnd(50) + (rnd(5) ? '' : 'é☃😀'),
    () => rnd(2) === 0, () => null,
  ][kind]()
  for (let round = 0; round < 300; round++) {
    const keys = Array.from({ length: 1 + rnd(20) }, (_, i) => 'k' + i)
    const kinds = keys.map(() => rnd(5))
    const n = 1 + rnd(30)
    const value = Array.from({ length: n }, () => {
      const o = {}
      for (let i = 0; i < keys.length; i++) {
        if (rnd(8) === 0) continue
        o[keys[rnd(10) === 0 ? rnd(keys.length) : i]] = scalar(kinds[i])
      }
      return o
    })
    same(round % 3 === 0 ? { rows: value } : value)
  }
})

test('shapes the walk must not reproduce fall back or agree', () => {
  const cases = [
    [{ a: 1 }, { a: 'x' }], [{ a: null }], [], {}, null, 42, [1, 2, 3], ['a', 'b'],
    { big: 2 ** 60 }, { big: 9007199254740993 }, { n: NaN, i: Infinity }, [{ n: NaN }, { n: 1.5 }],
    { d: new Date(0) }, { m: new Map([[1, 2]]) }, { t: { toJSON() { return 7 } } },
    { u: undefined, f() {}, s: Symbol('x'), a: 1 }, [undefined, () => 1, 3],
    { b: [] }, { b: [null, null] }, { x: { y: { z: [{ q: 1 }] } } },
    { 'a b': 1, 'é': 2, '': 3 }, { s: '\ud800' }, { neg: -0, z: 0 },
    Object.fromEntries(Array.from({ length: 300 }, (_, i) => ['f' + i, i])),
    Object.fromEntries(Array.from({ length: 17 }, (_, i) => ['f' + i, i])),
    { __proto__: { inherited: 1 }, own: 2 },
  ]
  let deep = { v: 1 }
  for (let i = 0; i < 200; i++) deep = { n: deep }
  cases.push(deep)
  for (const c of cases) same(c)
  // A cycle must throw, as JSON.stringify does, not hang or crash.
  const cyc = { a: 1 }; cyc.self = cyc
  assert.throws(() => codec.marshal(cyc))
})

test('bigint falls back to the same error', () => {
  assert.throws(() => codec.marshal({ b: 1n }), TypeError)
})

test('a corrupted blob is refused or encoded, never a trap', async () => {
  const { load } = await import('./harness.mjs')
  const { buildBlob } = await import('../dist/fast.js')
  const wasm = await load()
  const e = wasm.exports
  const rows = Array.from({ length: 12 }, (_, i) => ({ id: i, s: 'v' + i, o: { q: i }, t: [i, 1] }))
  const blob = buildBlob(rows)
  const good = new Uint8Array(blob.length)
  const at0 = e.alloc(blob.length)
  blob.copyTo(new Uint8Array(e.memory.buffer), at0)
  good.set(new Uint8Array(e.memory.buffer, at0, blob.length))
  assert.ok(e.encode_doc(good.length, 0) > 0)

  let seed = 7
  const rnd = (n) => { seed = (seed * 1103515245 + 12345) & 0x7fffffff; return seed % n }
  const run = (bytes) => {
    const at = e.alloc(bytes.length)
    new Uint8Array(e.memory.buffer).set(bytes, at)
    e.encode_doc(bytes.length, rnd(4)) // a trap would throw RuntimeError here
  }
  for (let i = 0; i < 3000; i++) {
    const bytes = good.slice()
    for (let k = 1 + rnd(4); k > 0; k--) bytes[rnd(i % 2 ? 64 : bytes.length)] = rnd(256)
    run(bytes)
  }
  for (let cut = 0; cut < good.length; cut += 7) run(good.subarray(0, cut))
})

test('number edge cases agree with the text path', () => {
  const nums = [0, -0, 1, -1, 2 ** 31, -(2 ** 31), 2 ** 31 - 1, 2 ** 32, -(2 ** 32) - 1, 3e9, -3e9,
    Number.MAX_SAFE_INTEGER, -Number.MAX_SAFE_INTEGER, 2 ** 53, 1e21, 1e-7, 5e-324, 1.7976931348623157e308,
    0.1, -0.5, 123456.789, 4294967295, 4294967296.5]
  for (const n of nums) {
    same({ v: n })
    same([{ v: n }, { v: 1 }])
    same([{ v: n }, { v: 1.5 }, { v: -7 }])
  }
  same(Array.from({ length: 20 }, (_, i) => ({ v: nums[i % nums.length] })))
})

test('exotic containers agree with the text path', () => {
  same({ n: new Number(3), s: new String('x'), b: new Boolean(true) })
  same({ 2: 'a', 1: 'b', z: 'c', 10: 'd' })
  same([{ 2: 1, a: 2 }, { a: 3, 2: 4 }])
  same({ a: [1, , 3] })
  const arr = [1, 2]; arr.extra = 'x'
  same({ a: arr })
  same(Object.create(null, { k: { value: 1, enumerable: true } }))
  same({ get g() { return 5 }, h: 1 })
  same({ a: Object.defineProperty({}, 'hidden', { value: 1, enumerable: false }) })
  same(new (class P { constructor() { this.x = 1 } })())
  same([new Uint8Array([1, 2])])
})
