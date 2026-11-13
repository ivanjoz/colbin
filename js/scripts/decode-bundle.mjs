// colbin/decode as an application's bundler sees it (bun scripts/decode-bundle.mjs).
//
// An app that imports only the plain-JavaScript decoder must not carry the
// WebAssembly module, its base64, or anything that needs Node or a bundler's
// asset handling. This bundles dist/decode/index.js, minified, the way a web
// app's build would, and checks what came out:
//
//   - no WebAssembly, no import.meta, no node: import, no long base64 run;
//   - at most LIMIT bytes gzipped, so growth is a decision and not a drift.
//
// Run after `bun run dist` (the test script does).

import { mkdtemp, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

/** The gzipped ceiling, a little over what the decoder measures today. */
const LIMIT = 14.5 * 1024

const entry = join(dirname(fileURLToPath(import.meta.url)), '..', 'dist', 'decode', 'index.js')
const where = await mkdtemp(join(tmpdir(), 'colbin-decode-bundle-'))
try {
  const app = join(where, 'app.js')
  await writeFile(app, `import { Decoder } from ${JSON.stringify(entry)}\nglobalThis.decoder = new Decoder()\n`)
  const result = await Bun.build({ entrypoints: [app], minify: true, target: 'browser' })
  if (!result.success) throw new Error(result.logs.join('\n'))
  const code = await result.outputs[0].text()
  const problems = []
  if (code.includes('WebAssembly')) problems.push('it references WebAssembly')
  if (code.includes('import.meta')) problems.push('it uses import.meta')
  if (/["']node:/.test(code)) problems.push('it imports a node: module')
  if (/[A-Za-z0-9+/]{1000,}/.test(code)) problems.push('it carries a long base64 run')
  const gzipped = Bun.gzipSync(new TextEncoder().encode(code), { level: 9 }).length
  if (gzipped > LIMIT) problems.push(`${gzipped} B gzipped, over the ${LIMIT} B ceiling`)
  console.log(`colbin/decode bundled: ${code.length} B minified, ${gzipped} B gzipped`)
  if (problems.length > 0) {
    console.error(problems.map((p) => `colbin/decode bundle: ${p}`).join('\n'))
    process.exitCode = 1
  }
} finally {
  await rm(where, { recursive: true, force: true })
}
