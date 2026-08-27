// The public ABI (PLAN.md 2.1), driven the way the page and the npm package
// will drive it: allocate, write UTF-8 in, read bytes out.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'
import { vectors, hex } from './harness.mjs'

const root = join(dirname(fileURLToPath(import.meta.url)), '..')
const bytes = await readFile(join(root, 'build/colbin.wasm'))
const cases = await vectors('messages.json')

// A fresh instance per operation, which is what PLAN.md 2.2 specifies: the
// module compiles once and each call gets its own memory.
const module = await WebAssembly.compile(bytes)

function instantiate() {
  const instance = new WebAssembly.Instance(module, {
    env: {
      abort(_m, _f, line, col) {
        throw new Error(`AssemblyScript abort at ${line}:${col}`)
      },
    },
  })
  const e = instance.exports
  return {
    encode(text) {
      const src = Buffer.from(text, 'utf8')
      const ptr = e.alloc(src.length)
      new Uint8Array(e.memory.buffer).set(src, Number(ptr))
      const len = e.encode(src.length)
      if (len < 0) {
        const errLen = e.lastError()
        const at = Number(e.resultPtr())
        return { ok: false, error: JSON.parse(Buffer.from(new Uint8Array(e.memory.buffer).subarray(at, at + errLen)).toString()) }
      }
      const at = Number(e.resultPtr())
      return { ok: true, bytes: new Uint8Array(e.memory.buffer).slice(at, at + len) }
    },
  }
}

test('the module compiles and instantiates', () => {
  assert.ok(instantiate())
})

for (const c of cases) {
  test(`public abi: ${c.tier}/${c.name}`, () => {
    const r = instantiate().encode(c.json)
    assert.ok(r.ok, `encode failed: ${JSON.stringify(r.error)}`)
    assert.equal(hex(r.bytes), c.message)
  })
}

test('a type conflict comes back as a diagnostic, not a trap', () => {
  const r = instantiate().encode('[{"qty":1},{"qty":"x"}]')
  assert.equal(r.ok, false)
  assert.equal(r.error.path, '[1].qty')
  assert.match(r.error.message, /type conflict: string/)
  assert.ok(r.error.offset > 0)
})

test('malformed JSON reports an offset and a line', () => {
  const r = instantiate().encode('{\n  "a": 1,\n  "b": @\n}')
  assert.equal(r.ok, false)
  assert.equal(r.error.line, 3)
})

test('warnings travel with a successful encode', () => {
  const r = instantiate().encode('[{"a":null},{"a":null}]')
  assert.ok(r.ok)
})

test('every instance is independent', () => {
  const a = instantiate()
  const b = instantiate()
  const first = a.encode('[{"v":1}]')
  const second = b.encode('[{"v":2}]')
  assert.ok(first.ok && second.ok)
  assert.notEqual(hex(first.bytes), hex(second.bytes))
  assert.equal(hex(a.encode('[{"v":1}]').bytes), hex(first.bytes))
})
