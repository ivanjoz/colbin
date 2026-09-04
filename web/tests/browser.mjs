// The phase-3 gate (PLAN.md 10): headless Chrome drives every example end to
// end, and any console error fails the run.
//
// Driven over the DevTools protocol with Node's own WebSocket, so the check
// costs no dependency and runs the same in CI as it does here.

import { spawn } from 'node:child_process'
import { createServer } from 'node:http'
import { existsSync } from 'node:fs'
import { readFile } from 'node:fs/promises'
import { dirname, extname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = join(dirname(fileURLToPath(import.meta.url)), '..', 'build-site')
const TYPES = {
  '.html': 'text/html',
  '.js': 'text/javascript',
  '.css': 'text/css',
  '.wasm': 'application/wasm',
  '.json': 'application/json',
}

const server = createServer(async (req, res) => {
  const path = (req.url ?? '/').split('?')[0]
  const file = path === '/' ? '/index.html' : path
  try {
    const body = await readFile(join(root, file))
    res.writeHead(200, { 'content-type': TYPES[extname(file)] ?? 'application/octet-stream' })
    res.end(body)
  } catch {
    res.writeHead(404).end('not found')
  }
})
await new Promise((resolve) => server.listen(0, resolve))
const origin = 'http://127.0.0.1:' + server.address().port

// This machine and the CI runner do not agree on what Chrome is called, and a
// missing binary otherwise surfaces twelve seconds later as "did not come up".
function chromeBinary() {
  if (process.env.CHROME_PATH) return process.env.CHROME_PATH
  const names = ['google-chrome', 'google-chrome-stable', 'chromium', 'chromium-browser']
  const dirs = (process.env.PATH ?? '').split(':').filter(Boolean)
  for (const name of names) {
    for (const dir of dirs) if (existsSync(join(dir, name))) return join(dir, name)
  }
  throw new Error('no Chrome found on PATH; set CHROME_PATH')
}

const chrome = spawn(
  chromeBinary(),
  ['--headless', '--no-sandbox', '--disable-gpu', '--remote-debugging-port=9333', 'about:blank'],
  { stdio: 'ignore' }
)
chrome.on('error', (err) => {
  console.error('could not start Chrome: ' + err.message)
  process.exit(1)
})

async function target() {
  for (let i = 0; i < 60; i++) {
    try {
      const list = await (await fetch('http://127.0.0.1:9333/json/list')).json()
      const page = list.find((t) => t.type === 'page')
      if (page) return page
    } catch {}
    await new Promise((r) => setTimeout(r, 200))
  }
  throw new Error('Chrome did not come up')
}

const page = await target()
const ws = new WebSocket(page.webSocketDebuggerUrl)
await new Promise((resolve) => (ws.onopen = resolve))

let nextId = 1
const pending = new Map()
const consoleErrors = []

ws.onmessage = (event) => {
  const msg = JSON.parse(event.data)
  if (msg.id && pending.has(msg.id)) {
    pending.get(msg.id)(msg)
    pending.delete(msg.id)
    return
  }
  if (msg.method === 'Runtime.consoleAPICalled' && msg.params.type === 'error') {
    consoleErrors.push(msg.params.args.map((a) => a.value ?? a.description).join(' '))
  }
  if (msg.method === 'Runtime.exceptionThrown') {
    const d = msg.params.exceptionDetails
    consoleErrors.push(d.text + ' ' + (d.exception?.description ?? ''))
  }
  if (msg.method === 'Log.entryAdded' && msg.params.entry.level === 'error') {
    consoleErrors.push(msg.params.entry.text)
  }
}

function send(method, params = {}) {
  const id = nextId++
  ws.send(JSON.stringify({ id, method, params }))
  return new Promise((resolve) => pending.set(id, resolve))
}

async function evaluate(expression) {
  const res = await send('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true })
  const details = res.result?.exceptionDetails
  if (details) throw new Error(details.exception?.description ?? 'evaluate threw')
  return res.result.result.value
}

const wait = (ms) => new Promise((r) => setTimeout(r, ms))
const failures = []

function check(name, condition, detail = '') {
  if (condition) console.log('  ok   ' + name)
  else {
    console.log('  FAIL ' + name + ' ' + detail)
    failures.push(name)
  }
}

const click = (text) =>
  evaluate(
    "Array.from(document.querySelectorAll('aside button'))" +
      ".find(b => b.textContent.includes(" + JSON.stringify(text) + ")).click()"
  )

try {
  await send('Runtime.enable')
  await send('Log.enable')
  await send('Page.enable')
  // A fixed viewport, so the layout checks below mean the same thing on every
  // machine that runs them.
  await send('Emulation.setDeviceMetricsOverride', {
    width: 1280,
    height: 800,
    deviceScaleFactor: 1,
    mobile: false,
  })
  await send('Page.navigate', { url: origin })
  await wait(2500)

  const names = await evaluate(
    "Array.from(document.querySelectorAll('aside button')).map(b => b.textContent.trim())"
  )
  console.log('Driving ' + names.length + ' examples')
  check('every example is listed', names.length === 11, 'got ' + names.length)

  for (let i = 0; i < names.length; i++) {
    await evaluate("document.querySelectorAll('aside button')[" + i + '].click()')
    await wait(700)

    const state = await evaluate(
      '(() => ({' +
        "refused: !!document.querySelector('.box.bad')," +
        "message: document.querySelector('.box.bad p')?.textContent ?? ''," +
        "ratio: document.querySelector('.headline strong')?.textContent ?? ''," +
        "columns: document.querySelectorAll('.tree .row').length," +
        "hexCells: document.querySelectorAll('.hex .cell').length" +
        '}))()'
    )

    const name = names[i]
    if (name.includes('changes type')) {
      // The deliberately broken one: refused, and it must say where.
      check(name + ': refused', state.refused)
      check(name + ': names the conflict', /type conflict/.test(state.message), state.message)
    } else {
      check(name + ': encoded', !state.refused, state.message)
      check(name + ': has a ratio', /x$/.test(state.ratio), state.ratio)
      check(name + ': columns listed', state.columns > 0)
      check(name + ': hex rendered', state.hexCells > 0)
    }
  }

  // The honesty cases have to show what the format actually does.
  await click('One object')
  await wait(600)
  check(
    'a lone object is shown as a loss',
    (await evaluate("document.querySelector('.headline').classList.contains('is-loss')")) === true
  )

  await click('Metric points')
  await wait(700)
  await evaluate(
    "document.querySelector('.tree .row')" +
      ".dispatchEvent(new PointerEvent('pointerenter', {bubbles: true, pointerType: 'mouse'}))"
  )
  await wait(200) // Svelte applies the update on a microtask, not synchronously
  const hover = await evaluate("document.querySelectorAll('.hex .cell.hit').length")
  check('hovering a column highlights its bytes', hover > 0, 'highlighted ' + hover)

  await click('Clients')
  await wait(1200)

  // Side by side, the hover has to reach a column whose bytes are far past the
  // first kilobyte — otherwise the dump lights up nothing and reads as broken.
  await evaluate(
    "Array.from(document.querySelectorAll('.tree .row')).find(r => /email/.test(r.textContent))" +
      ".dispatchEvent(new PointerEvent('pointerenter', {bubbles: true, pointerType: 'mouse'}))"
  )
  await wait(200)
  const far = await evaluate("document.querySelectorAll('.hex .cell.hit').length")
  check('hovering a column past the window moves it there', far > 0, 'highlighted ' + far)

  // The gzip toggle swaps what the two bars measure; it must not stack a
  // second pair, which is what made the panel tall enough to scroll.
  const barState =
    "(() => ({" +
    "bars: document.querySelectorAll('.bar-row').length," +
    "ratio: document.querySelector('.headline strong').textContent," +
    "labels: Array.from(document.querySelectorAll('.bar-row .label')).map(l => l.textContent)" +
    "}))()"
  const raw = await evaluate(barState)
  await evaluate("document.querySelector('.gzip-toggle input').click()")
  await wait(300)
  const gz = await evaluate(barState)
  check('raw shows two bars', raw.bars === 2, 'got ' + raw.bars)
  check('gzip replaces them rather than adding', gz.bars === 2, 'got ' + gz.bars)
  check('gzip relabels the bars', gz.labels.every((l) => /gz$/.test(l)), gz.labels.join(','))
  check('gzip restates the ratio', gz.ratio !== raw.ratio, raw.ratio + ' -> ' + gz.ratio)

  // The one control the message pane has left.
  check(
    'the download button is on its own row above the panels',
    (await evaluate("!!document.querySelector('.download-row .download')")) === true
  )

  // The shell fills the viewport exactly: no scrollbar on the page itself, and
  // nothing clipped behind one that is not there.
  const overflow = await evaluate(
    '(() => ({' +
      'w: document.documentElement.scrollWidth, h: document.body.scrollHeight,' +
      'winW: window.innerWidth, winH: window.innerHeight' +
      '}))()'
  )
  check('the page does not scroll sideways', overflow.w <= overflow.winW, JSON.stringify(overflow))
  check('the page does not scroll down', overflow.h <= overflow.winH, JSON.stringify(overflow))

  // A phone. The same page, one column, and above all not one pixel wider
  // than the screen — a sideways scroll is what "not responsive" looks like.
  await send('Emulation.setDeviceMetricsOverride', {
    width: 390,
    height: 844,
    deviceScaleFactor: 2,
    mobile: true,
  })
  await send('Page.navigate', { url: origin })
  await wait(2500)

  const phone = await evaluate(
    '(() => {' +
      'const tracks = (el) => getComputedStyle(el).gridTemplateColumns.split(" ").length;' +
      // Whatever sticks out, named, so a failure says which element to fix.
      // The hex dump and the example strip scroll sideways on purpose, so
      // their contents being wider than the screen is the design, not a bug.
      'const wide = Array.from(document.querySelectorAll("body *"))' +
      '  .filter(el => !el.closest(".hex, aside ul"))' +
      '  .filter(el => el.getBoundingClientRect().right > window.innerWidth + 1)' +
      '  .map(el => el.tagName.toLowerCase() + "." + (el.className.baseVal ?? el.className ?? ""));' +
      'return {' +
      'docW: document.documentElement.scrollWidth, winW: window.innerWidth,' +
      'shell: tracks(document.querySelector(".shell")),' +
      'main: tracks(document.querySelector("main")),' +
      'panels: tracks(document.querySelector(".panels")),' +
      'examples: document.querySelectorAll("aside button").length,' +
      'editorH: document.querySelector("textarea").getBoundingClientRect().height,' +
      'wide: wide.slice(0, 4)' +
      '};})()'
  )
  check('the phone page does not scroll sideways', phone.docW <= phone.winW, JSON.stringify(phone))
  check('nothing overflows the screen', phone.wide.length === 0, phone.wide.join(', '))
  check('the shell is one column', phone.shell === 1, 'got ' + phone.shell)
  check('the editor and the message stack', phone.main === 1, 'got ' + phone.main)
  check('the columns and the bytes stack', phone.panels === 1, 'got ' + phone.panels)
  check('every example is still reachable', phone.examples === 11, 'got ' + phone.examples)
  check('the editor keeps a usable height', phone.editorH >= 200, 'got ' + phone.editorH)

  // No hover on a touch screen, so the link between the two panels is a tap.
  await evaluate("document.querySelector('.tree .row').click()")
  await wait(200)
  const tapped = await evaluate("document.querySelectorAll('.hex .cell.hit').length")
  check('tapping a column highlights its bytes', tapped > 0, 'highlighted ' + tapped)

  check('no console errors', consoleErrors.length === 0, consoleErrors.join(' | '))
} finally {
  ws.close()
  chrome.kill()
  server.close()
}

if (failures.length > 0) {
  console.log('\n' + failures.length + ' failed')
  process.exit(1)
}
console.log('\nall browser checks passed')
