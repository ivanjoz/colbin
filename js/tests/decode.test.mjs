// The pure-JS decoder (colbin/decode) against every message Go wrote into the
// two corpora: js/vectors (types and packed, each under both deliveries) and
// rust/vectors (the walks, dynamic documents among them).
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import { join } from 'node:path'
import { root, vectors, unhex } from './harness.mjs'
import { Decoder, decode } from '../src/decode/index.ts'
import { canonical, canonicalJSON } from './decode-compare.mjs'
import { forceGeneric } from '../src/decode/build.ts'

const types = await vectors('types')
const packed = await vectors('packed')
const rust = JSON.parse(await readFile(join(root, '..', 'rust', 'vectors', 'vectors.json'), 'utf8'))

function check(name, value, json) {
  assert.equal(canonical(value), canonicalJSON(json), name)
}

for (const c of [...types, ...packed]) {
  test(`${c.name}: section held`, () => {
    const decoder = new Decoder()
    decoder.setSchema(unhex(c.section))
    check(c.name, decoder.decode(unhex(c.message)), c.json)
  })
  test(`${c.name}: self-describing`, () => {
    check(c.name, decode(unhex(c.selfDescribing)), c.json)
  })
}

for (const c of rust.walks) {
  test(`walk ${c.name}: section held`, () => {
    const decoder = new Decoder()
    if (c.section) decoder.setSchema(unhex(c.section))
    check(c.name, decoder.decode(unhex(c.message)), c.json)
  })
  if (c.selfDescribing) {
    test(`walk ${c.name}: self-describing`, () => {
      check(c.name, decode(unhex(c.selfDescribing)), c.json)
    })
  }
}

// Records and dynamic maps are built by generated literals once their shape
// repeats (build.ts, shapes.ts), and by plain stores under a policy that
// forbids generated code. Every path must give the same value, in the same key
// order: decode each message several times, then again without generation.
function keyOrder(value) {
  if (value === null || typeof value !== 'object' || ArrayBuffer.isView(value)) return []
  const own = Object.keys(value)
  return [own, ...own.map((k) => keyOrder(value[k]))]
}

test('repeated decodes and the no-generation path agree, key order included', () => {
  const all = [
    ...[...types, ...packed].map((c) => ({ name: c.name, section: c.section, message: c.message, json: c.json })),
    ...rust.walks.map((c) => ({ name: c.name, section: c.section, message: c.message, json: c.json })),
  ]
  const first = new Map()
  for (const generic of [false, true]) {
    forceGeneric(generic)
    try {
      for (const c of all) {
        const decoder = new Decoder()
        if (c.section) decoder.setSchema(unhex(c.section))
        for (let round = 0; round < 4; round++) {
          const value = decoder.decode(unhex(c.message))
          check(`${c.name} round ${round} generic ${generic}`, value, c.json)
          const order = JSON.stringify(keyOrder(value))
          if (!first.has(c.name)) first.set(c.name, order)
          else assert.equal(order, first.get(c.name), `${c.name}: key order, round ${round}, generic ${generic}`)
        }
      }
    } finally {
      forceGeneric(false)
    }
  }
})

test('a key named __proto__ stays an own key on every path', () => {
  // {"__proto__": 1, "a": 2} as a dynamic root (0xD1), compact form.
  const bytes = new Uint8Array([0xd1, 0xa9, ...Buffer.from('__proto__'), 0x01, 0xa1, 0x61, 0x02])
  for (const generic of [false, true]) {
    forceGeneric(generic)
    try {
      for (let round = 0; round < 4; round++) {
        const value = decode(bytes)
        assert.equal(Object.getPrototypeOf(value), Object.prototype)
        assert.deepEqual(Object.keys(value), ['__proto__', 'a'])
        assert.equal(Object.getOwnPropertyDescriptor(value, '__proto__').value, 1)
      }
    } finally {
      forceGeneric(false)
    }
  }
})
