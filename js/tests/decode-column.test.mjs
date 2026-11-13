// The pure-JS column codec against the columns Go wrote into rust/vectors,
// read both ways: in doubles where it may, and in exact lanes always.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import { join } from 'node:path'
import { root, unhex } from './harness.mjs'
import { decodeInts } from '../src/decode/column.ts'

// The values are int64 literals; the reviver reads each from its source text so
// an extreme one is not rounded on the way in.
const text = await readFile(join(root, '..', 'rust', 'vectors', 'vectors.json'), 'utf8')
const corpus = JSON.parse(text, (key, value, context) =>
  typeof value === 'number' && context?.source !== undefined ? BigInt(context.source) : value,
)

function lanes(pairs) {
  const out = []
  for (let i = 0; i < pairs.length; i += 2) {
    out.push(BigInt.asIntN(64, (BigInt(pairs[i + 1]) << 32n) | BigInt(pairs[i])))
  }
  return out
}

for (const c of corpus.columns) {
  const bytes = unhex(c.encoded)
  const n = c.values.length
  const want = c.values.map((v) => BigInt(v))

  test(`column ${c.name}: exact lanes`, () => {
    const got = decodeInts(bytes, 0, bytes.length, n, true)
    assert.ok(got.pairs !== null)
    assert.deepEqual(lanes(got.pairs), want)
    assert.equal(got.next, bytes.length)
  })

  test(`column ${c.name}: numbers where safe`, () => {
    const got = decodeInts(bytes, 0, bytes.length, n, false)
    const values = got.num !== null ? Array.from(got.num, (v) => BigInt(v)) : lanes(got.pairs)
    assert.deepEqual(values, want)
    if (got.num !== null) {
      for (const v of want) assert.ok(v <= BigInt(Number.MAX_SAFE_INTEGER) && v >= -BigInt(Number.MAX_SAFE_INTEGER))
    }
  })

  test(`column ${c.name}: every truncation refused`, () => {
    for (let cut = 0; cut < bytes.length; cut++) {
      assert.throws(() => decodeInts(bytes.subarray(0, cut), 0, cut, n, false), { name: 'ColbinError' })
    }
  })
}
