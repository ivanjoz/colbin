// The tarball, as a consumer receives it — PACKAGE_PLAN.md phase 5.
//
// `npm pack`, install the tarball into a directory outside the repository, and
// import the entries from a file that has never seen this checkout. What that
// catches, and nothing else in the suite does: a missing `files` entry, an
// `exports` map that resolves to nothing, a relative import that worked from
// `src/` and not from `dist/`, a `.wasm` that was never copied in.
//
// The consumer is deliberately plain Node with no bundler and no TypeScript:
// anything that needs one of those is a bundler's problem, and `web/` is the
// test for that (§9).

import { execFileSync } from 'node:child_process'
import { mkdtemp, mkdir, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

/**
 * What the consumer runs. Written out rather than imported, because the point
 * is that it resolves `colbin` by name from its own `node_modules`.
 *
 * Twelve rows rather than three: the encoder transposes a list of records into
 * columns at eight, and it is the columnar shape the materializer covers — so
 * a three-row fixture would quietly test the JSON fallback instead.
 */
const CONSUMER = `import { Codec, ColbinError, unmarshal } from 'colbin'
import * as asset from 'colbin/asset'
import { readFile } from 'node:fs/promises'
import assert from 'node:assert/strict'
import { createRequire } from 'node:module'

const cities = ['Lima', 'Cusco', 'Arequipa', 'Trujillo', 'Piura']
const rows = Array.from({ length: 12 }, (_, i) => ({
  id: i + 1,
  sku: 'SKU-' + (1000 + i),
  city: cities[i % cities.length],
  price: 4.25 + i,
  active: i % 3 !== 0,
}))

// The main entry, under Node: the \`node\` condition, reading the .wasm beside
// itself.
const codec = await Codec.open()
const { message, section, standalone } = codec.marshal(rows)
assert.deepEqual(codec.unmarshal(message, { section }), rows)
assert.equal(codec.lastPath, 'materialize')
assert.deepEqual(codec.unmarshal(standalone), rows)
assert.deepEqual(JSON.parse(codec.toJSONText(standalone)), rows)
assert.equal(codec.columns(standalone).columns.length, 5)
assert.ok(message.length < JSON.stringify(rows).length)

// The free function, with no handle held.
assert.deepEqual(await unmarshal(standalone), rows)

// The browser entry, which Node will never resolve through the exports map
// because the \`node\` condition always wins there — so it is imported as the
// file a bundler would have picked. It carries the module inline rather than
// reading one, which is the thing worth proving here.
const browser = await import(new URL('./node_modules/colbin/dist/index.js', import.meta.url))
const inlineCodec = await browser.Codec.open()
assert.deepEqual(inlineCodec.unmarshal(standalone), rows)

// The asset entry: Node cannot fetch a file: URL, so the URL is checked and
// the bytes are handed over. See js/tests/package.test.mjs for why.
const assetCodec = await asset.Codec.open({ wasm: await readFile(asset.wasmURL) })
assert.deepEqual(assetCodec.unmarshal(standalone), rows)

// And the entry that carries the other module — the one with the span walk.
// The default module does not have it, and says so rather than failing as
// \`is not a function\`.
const spans = await import('colbin/inspect')
const spanCodec = await spans.Codec.open({ wasm: await readFile(spans.wasmURL) })
assert.equal(spanCodec.canInspect, true)
assert.equal(spanCodec.inspect(message, { section }).totalBytes, message.length)
assert.equal(codec.canInspect, false)
assert.throws(() => codec.inspect(message, { section }), (e) => e.message.includes('colbin/inspect'))

// And the failure type, by identity across the entries.
assert.equal(asset.ColbinError, ColbinError)
assert.throws(() => codec.unmarshal(Uint8Array.from([1, 2, 3])), ColbinError)

// The subpath that lets a build tool reach the module file itself.
const require = createRequire(import.meta.url)
assert.ok(require.resolve('colbin/colbin.wasm').endsWith('colbin.wasm'))

const json = Buffer.byteLength(JSON.stringify(rows))
console.log(
  'every entry unmarshals: ' + rows.length + ' rows, ' +
  message.length + ' B of message + ' + section.length + ' B of section against ' + json + ' B of JSON',
)
`

/** Everything the tarball has to carry for the above to work. */
const REQUIRED = [
  'package.json',
  'dist/index.js',
  'dist/index.d.ts',
  'dist/node.js',
  'dist/node.d.ts',
  'dist/asset.js',
  'dist/asset.d.ts',
  'dist/inspect.js',
  'dist/inspect.d.ts',
  'dist/core.js',
  'dist/colbin.wasm',
  'dist/colbin.inspect.wasm',
  'dist/generated/wasm-inline.js',
  'README.md',
  'LICENSE',
]

const packageRoot = join(dirname(fileURLToPath(import.meta.url)), '..')

const run = (command, args, cwd) =>
  execFileSync(command, args, { cwd, encoding: 'utf8', stdio: ['ignore', 'pipe', 'inherit'] })

const where = await mkdtemp(join(tmpdir(), 'colbin-tarball-'))
let failed = false
try {
  // `--ignore-scripts` on the pack, and `bun run build` already ran (the
  // `tarball` script runs it first). Not to skip the build, but because npm
  // gives a lifecycle script this process's stdout, and `prepack`'s output
  // would land in the middle of the JSON being parsed here.
  const packed = JSON.parse(
    run('npm', ['pack', '--json', '--ignore-scripts', '--pack-destination', where], packageRoot),
  )
  const tarball = join(where, packed[0].filename)
  console.log(`packed ${packed[0].filename} — ${(packed[0].size / 1024).toFixed(0)} KB`)

  // Every file the tarball carries, so a missing one is named here rather than
  // surfacing as a resolution failure three steps later.
  const shipped = new Set(packed[0].files.map((f) => f.path))
  for (const required of REQUIRED) {
    if (!shipped.has(required)) throw new Error(`the tarball is missing ${required}`)
  }

  const consumer = join(where, 'consumer')
  await mkdir(consumer, { recursive: true })
  await writeFile(
    join(consumer, 'package.json'),
    JSON.stringify({ name: 'colbin-consumer', private: true, type: 'module' }, null, 2),
  )

  // `--ignore-scripts` is the posture CI and pnpm default to, and §1's second
  // reason is that the package must install under it. If anything here needed
  // a Rust toolchain on install, this is where it would fail.
  run('npm', ['install', '--ignore-scripts', '--no-audit', '--no-fund', tarball], consumer)

  await writeFile(join(consumer, 'smoke.mjs'), CONSUMER)
  process.stdout.write(run('node', ['smoke.mjs'], consumer))
} catch (err) {
  failed = true
  console.error(err.message)
} finally {
  await rm(where, { recursive: true, force: true })
}

process.exit(failed ? 1 : 0)
