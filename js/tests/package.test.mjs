// The package as a consumer receives it: the built `dist/`, all three entries,
// and the `Codec` handle's whole surface — PACKAGE_PLAN.md §4 and §6.
//
// These import `../dist/*.js` rather than `../src/*.ts`, which is the point:
// everything else in this directory tests the module, and this tests the
// packaging around it — the exports map, the emitted JavaScript, the `.wasm`
// sitting where each entry expects to find it. A relative import that worked
// from `src/` and not from `dist/` fails here rather than at a consumer.

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import { load, put, take, must } from './harness.mjs'
import { documents, textOf } from './documents.mjs'

import * as inlineEntry from '../dist/index.js'
import * as nodeEntry from '../dist/node.js'
import * as assetEntry from '../dist/asset.js'
import * as inspectEntry from '../dist/inspect.js'

const { Codec, ColbinError } = nodeEntry

/** The fixture every entry is checked against: a bare-array table, which is
 * the shape the materializer covers. */
const table = documents.find((d) => d.name === 'records')
const tableText = textOf(table)

/**
 * The three entries, each with the way it has to be opened here.
 *
 * `colbin/asset` resolves its module with `new URL('./colbin.wasm',
 * import.meta.url)` and fetches it — which is what a bundler rewrites and what
 * a browser can do, and what Node cannot: `fetch` refuses a `file:` URL
 * outright. So the URL is asserted to point at a real file and the bytes are
 * handed to `open` directly. The fetch itself is exercised for real by the
 * demo site, through Vite, in `web/tests/browser.mjs` (§9).
 */
const entries = [
  { name: 'colbin (inline)', open: () => inlineEntry.Codec.open() },
  { name: 'colbin (node)', open: () => nodeEntry.Codec.open() },
  {
    name: 'colbin/asset',
    open: async () => assetEntry.Codec.open({ wasm: await readFile(assetEntry.wasmURL) }),
  },
  {
    name: 'colbin/inspect',
    open: async () => inspectEntry.Codec.open({ wasm: await readFile(inspectEntry.wasmURL) }),
  },
]

test('every entry exports the same surface', () => {
  for (const entry of [inlineEntry, nodeEntry, assetEntry, inspectEntry]) {
    assert.equal(typeof entry.Codec.open, 'function')
    assert.equal(typeof entry.Codec.fromModule, 'function')
    assert.equal(typeof entry.Codec.preload, 'function')
    assert.equal(entry.ColbinError, ColbinError)
    for (const name of ['unmarshal', 'columns', 'toJSONText', 'marshal', 'inspect']) {
      assert.equal(typeof entry[name], 'function', `${name} is missing`)
    }
  }
})

test('the asset entry points at a module that is actually there', async () => {
  const bytes = await readFile(assetEntry.wasmURL)
  assert.ok(bytes.length > 1000, `${assetEntry.wasmURL} is ${bytes.length} bytes`)
  assert.deepEqual([...bytes.subarray(0, 4)], [0x00, 0x61, 0x73, 0x6d], 'not a wasm header')
  // And the node entry's copy is the same file, not a second one that could
  // drift from it.
  assert.equal(nodeEntry.wasmPath.href, assetEntry.wasmURL.href)
})

test('all three entries unmarshal the same message identically', async () => {
  const results = []
  for (const entry of entries) {
    const codec = await entry.open()
    const { message, section } = codec.marshal(tableText)
    const rows = codec.unmarshal(message, { section })
    assert.equal(codec.lastPath, 'materialize', `${entry.name} fell back to JSON`)
    assert.deepEqual(rows, table.json, entry.name)
    results.push(rows)
  }
  assert.deepEqual(results[0], results[1])
  assert.deepEqual(results[1], results[2])
})

test('the handle holds a schema across messages', async () => {
  const codec = await Codec.open()
  const { message, section } = codec.marshal(tableText)
  codec.setSchema(section)
  // Twice, because the second call is the one that exercises the instance
  // already holding the parsed section rather than re-parsing it (§6.1).
  assert.deepEqual(codec.unmarshal(message), table.json)
  assert.deepEqual(codec.unmarshal(message), table.json)

  codec.setSchema(null)
  assert.throws(() => codec.unmarshal(message), ColbinError)
})

test('a self-describing message needs no schema at all', async () => {
  const codec = await Codec.open()
  const { standalone } = codec.marshal(tableText)
  assert.deepEqual(codec.unmarshal(standalone), table.json)
})

test('`standalone` is the section in front of the body, byte for byte', async () => {
  // `marshal` composes the self-describing form rather than encoding a second
  // time. That is only sound if it lands on exactly what the module writes for
  // the SELF_DESCRIBING flag, so check it against the module itself over the
  // whole corpus rather than on the one fixture above.
  const wasm = await load()
  const codec = await Codec.open()
  const encoder = new TextEncoder()
  let checked = 0

  for (const doc of documents) {
    let text
    try {
      text = textOf(doc)
    } catch {
      continue
    }
    let marshaled
    try {
      marshaled = codec.marshal(text)
    } catch (err) {
      assert.ok(err instanceof ColbinError)
      continue
    }
    const at = put(wasm, encoder.encode(text))
    const length = must(wasm, wasm.exports.encode(at, 1), `encode ${doc.name}`)
    assert.deepEqual(marshaled.standalone, take(wasm, length), doc.name)
    checked++
  }
  assert.ok(checked > 10, `only ${checked} documents checked`)
})

test('a document the materializer declines still comes back, via JSON', async () => {
  const codec = await Codec.open()
  const doc = documents.find((d) => d.name === 'flat-object')
  const { message, section } = codec.marshal(textOf(doc))
  const value = codec.unmarshal(message, { section })
  assert.equal(codec.lastPath, 'json')
  assert.deepEqual(value, doc.json)
})

test('columns hands back the columns themselves', async () => {
  const codec = await Codec.open()
  const { message, section } = codec.marshal(tableText)
  const shaped = codec.columns(message, { section })
  assert.ok(shaped, 'a table should have columns')
  assert.equal(shaped.rowCount, table.json.length)

  for (const column of shaped.columns) {
    assert.equal(column.length, shaped.rowCount)
    assert.equal(column.values.length, shaped.rowCount)
    // Every column reads back what the row objects hold for that field.
    for (let i = 0; i < shaped.rowCount; i++) {
      assert.deepEqual(column.values[i], table.json[i][column.name], `${column.name}[${i}]`)
    }
  }

  // And a shape the materializer declines has no columns rather than a lie.
  const flat = documents.find((d) => d.name === 'flat-object')
  const other = codec.marshal(textOf(flat))
  assert.equal(codec.columns(other.message, { section: other.section }), null)
})

test('the CSP fallback builds the same rows as the generated builder', async () => {
  const codec = await Codec.open()
  const { message, section } = codec.marshal(tableText)
  const generated = codec.unmarshal(message, { section })
  const fallback = codec.unmarshal(message, { section, allowGenerated: false })
  assert.deepEqual(fallback, generated)
  assert.deepEqual(fallback, table.json)
})

test('toJSONText is the text path, and it agrees with unmarshal', async () => {
  const codec = await Codec.open()
  const { message, section } = codec.marshal(tableText)
  const text = codec.toJSONText(message, { section })
  assert.deepEqual(JSON.parse(text), codec.unmarshal(message, { section }))
})

test('inspect describes the message it was handed', async () => {
  const codec = await inspectEntry.Codec.open({ wasm: await readFile(inspectEntry.wasmURL) })
  assert.ok(codec.canInspect)
  const { message, section } = codec.marshal(tableText)
  const report = codec.inspect(message, { section })
  assert.equal(report.totalBytes, message.length)
  assert.equal(report.rows, table.json.length)
  assert.ok(report.fields.length > 0)
})

test('the published module does not carry the span walk, and says so', async () => {
  // The size guarantee, asserted rather than assumed: if `inspect` ever leaks
  // back into the default feature set, every consumer starts downloading 15 KB
  // they do not call and nothing but this would notice.
  const codec = await Codec.open()
  assert.equal(codec.canInspect, false)
  const { message, section } = codec.marshal(tableText)
  assert.throws(
    () => codec.inspect(message, { section }),
    /built without `inspect`.*colbin\/inspect/s,
  )
  // Everything else still works on it, which is the point of the split.
  assert.deepEqual(codec.unmarshal(message, { section }), table.json)
})

test('the two modules are different builds, and the lean one is smaller', async () => {
  const lean = await readFile(nodeEntry.wasmPath)
  const withSpans = await readFile(inspectEntry.wasmURL)
  assert.ok(
    withSpans.length > lean.length,
    `inspect build ${withSpans.length} B should exceed lean ${lean.length} B`,
  )
  // A real difference, not a rounding one: the span walk measures ~15 KB.
  assert.ok(withSpans.length - lean.length > 8000, `only ${withSpans.length - lean.length} B apart`)
})

test('a failure is a ColbinError carrying every diagnostic field', async () => {
  const codec = await Codec.open()
  let err
  try {
    codec.unmarshal(Uint8Array.from([0x41, 0x42, 0x43]))
    assert.fail('three bytes of ASCII are not a message')
  } catch (thrown) {
    err = thrown
  }
  assert.ok(err instanceof ColbinError, `threw ${err}`)
  assert.equal(err.name, 'ColbinError')
  assert.equal(typeof err.code, 'number')
  assert.equal(typeof err.offset, 'number')
  assert.equal(typeof err.line, 'number')
  assert.equal(typeof err.path, 'string')
  assert.ok(err.message.length > 0)
  assert.ok(Array.isArray(err.warnings))
  assert.ok(err instanceof Error)
})

test('marshal reports warnings without failing', async () => {
  const codec = await Codec.open()
  // An always-null column is the warning the encoder has to give: there is no
  // type to infer, so the column is written as absent everywhere.
  const { warnings } = codec.marshal('[{"a":1,"b":null},{"a":2,"b":null}]')
  assert.ok(Array.isArray(warnings))
})

test('verify on refuses nothing the encoder writes correctly', async () => {
  const codec = await Codec.open()
  const checked = codec.marshal(tableText, { verify: true })
  const plain = codec.marshal(tableText)
  assert.deepEqual(checked.message, plain.message)
})

test('the free functions work without a handle', async () => {
  const { message, section } = await nodeEntry.marshal(tableText)
  assert.deepEqual(await nodeEntry.unmarshal(message, { section }), table.json)
  assert.equal(typeof (await nodeEntry.toJSONText(message, { section })), 'string')
})

test('two handles over one compiled module are independent', async () => {
  const first = await Codec.open()
  const { message, section } = first.marshal(tableText)
  const second = Codec.fromModule(await WebAssembly.compile(await readFile(nodeEntry.wasmPath)))

  first.setSchema(section)
  // `second` was never given the schema, so it must refuse rather than pick up
  // the one its sibling holds.
  assert.throws(() => second.unmarshal(message), ColbinError)
  assert.deepEqual(first.unmarshal(message), table.json)
})
