// Phase 7's other half: writing the opt-in string encoding.
//
// Byte parity with Go, not a round trip. The scan is greedy rather than
// size-optimal, so a tie broken the other way is still a legal frame that
// decodes to the same string — a round-trip test would pass on an encoder that
// disagreed with Go about every string it touched, and two encoders that
// disagree produce two different messages for one record.
//
// Both key widths, because they frame the same payload differently: an escape
// code in the blob header under four key bits, the descriptor's `enc` under
// eight.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { load, vectors, hex, unhex } from './harness.mjs'

const wasm = await load()
const cases = await vectors('packed5')

test('the packed encoder tier is present, and both outcomes occur', () => {
  assert.ok(cases.length >= 20, `only ${cases.length} frames`)
  assert.ok(cases.some((c) => c.packed), 'some strings must pack')
  assert.ok(
    cases.some((c) => !c.packed),
    'and some must not, or the never-inflate rule is untested',
  )
})

function frame(call, key, input) {
  wasm.u8.set(input, wasm.exports.inPtr())
  const length = call(key, input.length)
  const at = wasm.exports.outPtr()
  return wasm.u8.subarray(at, at + length)
}

for (const c of cases) {
  test(`packed ${c.name}: the same bytes as Go, at both widths`, () => {
    const input = unhex(c.input)
    assert.equal(hex(frame(wasm.exports.packedNarrow, 3, input)), c.narrow, 'narrow')
    assert.equal(hex(frame(wasm.exports.packedWide, 3, input)), c.wide, 'wide')
  })
}

// The property that makes the encoding free to turn on: it is chosen per string
// and the raw form wins ties, so no string can come out larger for it.
test('no string is made larger by offering it to the encoder', () => {
  for (const c of cases) {
    const input = unhex(c.input)
    const narrow = frame(wasm.exports.packedNarrow, 3, input)
    const wide = frame(wasm.exports.packedWide, 3, input)
    // A raw narrow blob is two header bytes; a raw wide one is three.
    assert.ok(narrow.length <= input.length + 2, `${c.name} grew under four-bit keys`)
    assert.ok(wide.length <= input.length + 3, `${c.name} grew under eight-bit keys`)
  }
})

// And what the decoder makes of what the encoder just wrote, which is the one
// thing byte parity cannot check on its own: that both halves agree.
test('everything the encoder writes, the decoder reads back', async () => {
  const decoder = new TextDecoder()
  for (const c of cases) {
    const input = unhex(c.input)
    for (const [name, write] of [
      ['narrow', wasm.exports.packedNarrow],
      ['wide', wasm.exports.packedWide],
    ]) {
      const field = Uint8Array.from(frame(write, 3, input))
      // Wrap it as a one-field message so the ordinary decode path reads it.
      const message = Uint8Array.from([name === 'narrow' ? 0xd0 : 0xd8, ...field])
      // A section for a single string field at key 3, hand-built: length, one
      // struct, flags, one field, key 3, name "s", op 11 (string).
      const body = [1, 0, 1, 3, 1, 0x73, 11]
      if (name === 'wide') body[1] = 1
      const section = Uint8Array.from([body.length, ...body])
      wasm.u8.set(section, wasm.exports.inPtr())
      assert.equal(wasm.exports.setSection(section.length), 0)
      wasm.u8.set(message, wasm.exports.inPtr())
      const got = wasm.exports.messageJSON(message.length)
      assert.ok(got >= 0, `${c.name}/${name}: ${decoder.decode(wasm.u8.slice(wasm.exports.outPtr(), wasm.exports.outPtr() + wasm.exports.lastMessage()))}`)
      const text = decoder.decode(wasm.u8.slice(wasm.exports.outPtr(), wasm.exports.outPtr() + got))
      const back = JSON.parse(text).s
      // Compared through what encoding/json would have written for the input,
      // because invalid UTF-8 is replaced on the way out by both sides.
      assert.equal(back, JSON.parse(jsonOf(input)), `${c.name}/${name}`)
    }
  }
})

/** The input as a JSON string literal, with invalid UTF-8 replaced the way the
 * decoder's escaper does it. */
function jsonOf(bytes) {
  return JSON.stringify(new TextDecoder('utf-8', { fatal: false }).decode(bytes))
}
