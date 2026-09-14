// `wasm-opt` over `build/colbin.wasm`, in place.
//
// What it is worth, measured on the module this package ships: 253 438 ->
// 223 558 B raw (-11.8%) and 93 407 -> 89 710 B gzipped (-4.0%). The raw
// figure is the one that matters most — it is code the engine has to compile
// before the page can do anything — but neither is the order of magnitude the
// profile comment in `rust/wasm/Cargo.toml` was written about, back when the
// module was 34 KB. It is worth having and it is not worth a hard dependency.
//
// Hence: a tool, not a package. `binaryen` on npm is 104 MB (binaryen compiled
// to JavaScript) and the `wasm-opt` package is 61 MB of every platform's
// binary fetched by a postinstall — which would fail under the
// `--ignore-scripts` install `scripts/tarball.mjs` exists to prove works. So
// this uses `wasm-opt` from the PATH when there is one, says so plainly when
// there is not, and CI passes `--required` so the published module cannot
// quietly ship unoptimised.
//
// No feature flags are passed. `rustc` writes a `target_features` custom
// section naming exactly what it emitted, `wasm-opt` reads it, and
// `rust/wasm/Cargo.toml` keeps that section alive with `strip = "debuginfo"`
// rather than `strip = true` precisely so this works. A hand-kept
// `--enable-bulk-memory --enable-sign-ext …` list measured the same to within
// 200 bytes and would go stale the first time a toolchain turns on something
// new.

import { execFileSync } from 'node:child_process'
import { gzipSync } from 'node:zlib'
import { readFile, rename, stat, unlink } from 'node:fs/promises'
import { basename, dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = join(dirname(fileURLToPath(import.meta.url)), '..')
const args = process.argv.slice(2).filter((a) => !a.startsWith('--'))
const target = join(root, 'build', args[0] ?? 'colbin.wasm')
const scratch = `${target}.opt`
// `--required` on the command line, or COLBIN_REQUIRE_WASM_OPT in the
// environment — CI sets the variable once for the job rather than threading a
// flag through every script that ends up here.
const required =
  process.argv.includes('--required') || process.env.COLBIN_REQUIRE_WASM_OPT === '1'
const tool = process.env.WASM_OPT ?? 'wasm-opt'

const before = await readFile(target).catch((err) => {
  throw new Error(`no module at ${target}: run \`bun run wasm\` first (${err.code})`)
})

function available() {
  try {
    execFileSync(tool, ['--version'], { stdio: 'ignore' })
    return true
  } catch {
    return false
  }
}

if (!available()) {
  const message =
    `wasm-opt not found (tried \`${tool}\`). The module is ${kb(before.length)} ` +
    'unoptimised, about 12% larger than it needs to be.\n' +
    '  Fedora: dnf install binaryen   Debian/Ubuntu: apt install binaryen   macOS: brew install binaryen\n' +
    '  or point WASM_OPT at a binary from https://github.com/WebAssembly/binaryen/releases'
  if (required) {
    console.error(`error: ${message}`)
    process.exit(1)
  }
  console.warn(`warning: ${message}`)
  process.exit(0)
}

execFileSync(
  tool,
  [
    // -O3 rather than -Oz: it wins on gzipped size (89 710 B against -Oz's
    // 89 790) while also being the level that does not trade speed away, and
    // speed is the whole subject of PACKAGE_PLAN.md §2. -Oz wins on raw bytes
    // by 1.5 KB, which gzip then gives back.
    '-O3',
    // The sections that exist only to get us here.
    '--strip-debug',
    '--strip-producers',
    '--strip-target-features',
    target,
    '-o',
    scratch,
  ],
  { stdio: ['ignore', 'inherit', 'inherit'] },
)

// Written beside the input and moved over it, so a failed run leaves the
// module that was there rather than a half-written one.
const after = await stat(scratch)
if (after.size === 0) {
  await unlink(scratch)
  throw new Error('wasm-opt produced an empty module')
}
await rename(scratch, target)

const gzBefore = gzipSync(before, { level: 9 }).length
const gzAfter = gzipSync(await readFile(target), { level: 9 }).length
const pct = (from, to) => `${(((to - from) / from) * 100).toFixed(1)}%`
// The input still carries the `name` section cargo was told to keep, so the
// raw percentage here flatters wasm-opt a little; against the module as it
// used to ship (`strip = true`, no wasm-opt) it is -17% raw and -10% gzipped.
console.log(
  `wasm-opt -O3 ${basename(target)}: ${kb(before.length)} -> ${kb(after.size)} ` +
    `(${pct(before.length, after.size)}), gzipped ${kb(gzBefore)} -> ${kb(gzAfter)} ` +
    `(${pct(gzBefore, gzAfter)})`,
)

function kb(n) {
  return `${(n / 1024).toFixed(1)} KB`
}
