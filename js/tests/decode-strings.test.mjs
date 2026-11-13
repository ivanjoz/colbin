// A run of length-prefixed strings is decoded with one TextDecoder call and cut
// with substring (Walker.run). Each string must come out exactly as decoding it
// alone would: ASCII, multi-byte, four-byte, malformed, and long (u32) lengths
// whose four bytes are anything.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Walker } from '../src/decode/walk.ts'
import { Schema } from '../src/decode/schema.ts'
import { utf8, decodeHere } from '../src/decode/strings.ts'

const alone = new TextDecoder('utf-8', { ignoreBOM: true })

function rng(seed) {
  let state = seed >>> 0
  return () => {
    state ^= state << 13
    state ^= state >>> 17
    state ^= state << 5
    return state >>> 0
  }
}

const pieces = [
  [0x61], [0x7a], [0x20], [0x00],
  [0xc3, 0xa9], [0xc2, 0x80], [0xdf, 0xbf],
  [0xe2, 0x82, 0xac], [0xe0, 0xa0, 0x80], [0xed, 0x9f, 0xbf], [0xef, 0xbb, 0xbf],
  [0xf0, 0x9f, 0x98, 0x80], [0xf4, 0x8f, 0xbf, 0xbf],
]
const broken = [
  [0x80], [0xbf], [0xc0, 0x80], [0xc1], [0xc3], [0xe0, 0x80, 0x80], [0xed, 0xa0, 0x80],
  [0xe2, 0x82], [0xf0, 0x80, 0x80, 0x80], [0xf4, 0x90, 0x80, 0x80], [0xf5], [0xff], [0xf0, 0x9f],
]

function string(next, malformed) {
  const out = []
  const length = next() % 4 === 0 ? 200 + (next() % 200) : next() % 24
  while (out.length < length) {
    if (malformed && next() % 40 === 0) out.push(...broken[next() % broken.length])
    else if (next() % 3 === 0) out.push(...pieces[next() % pieces.length])
    else out.push(0x20 + (next() % 95))
  }
  return out
}

function encode(strings) {
  const bytes = []
  for (const s of strings) {
    if (s.length < 0xff) bytes.push(s.length)
    else bytes.push(0xff, s.length & 0xff, (s.length >> 8) & 0xff, 0, 0)
    bytes.push(...s)
  }
  return new Uint8Array(bytes)
}

test('a run of strings decodes each one as it decodes alone', () => {
  const next = rng(0x1b873593)
  for (let round = 0; round < 3000; round++) {
    const malformed = round % 3 === 0
    const ascii = round % 5 === 0
    const count = 2 + (next() % 30)
    const strings = []
    for (let i = 0; i < count; i++) {
      strings.push(ascii ? Array.from({ length: next() % 40 }, () => 0x20 + (next() % 95)) : string(next, malformed))
    }
    const bytes = encode(strings)
    const walker = new Walker(bytes, new Schema([], 0), undefined)
    const end = walker.run(0, bytes.length, -1, 'short')
    assert.equal(end, bytes.length)
    assert.deepEqual(walker.strs, strings.map((s) => alone.decode(new Uint8Array(s))), `round ${round}`)
  }
})

test('a long length whose four bytes form a multi-byte sequence', () => {
  // 0xC3 0x80 inside the u32 (size 0x80C3) would merge into one unit.
  const size = 0x80c3
  const long = new Array(size).fill(0x61)
  long[0] = 0xc3
  long[1] = 0xa9
  const strings = [[0xc3, 0xa9], long, [0x62, 0xe2, 0x82, 0xac]]
  const bytes = encode(strings)
  assert.deepEqual([...bytes.subarray(3, 8)], [0xff, 0xc3, 0x80, 0, 0])
  const walker = new Walker(bytes, new Schema([], 0), undefined)
  walker.run(0, bytes.length, -1, 'short')
  assert.deepEqual(walker.strs, strings.map((s) => alone.decode(new Uint8Array(s))))
})

// The decoder's own UTF-8 (short strings that are not ASCII, and every string
// where there is no TextDecoder) must replace malformed bytes exactly as
// TextDecoder does.
test('the UTF-8 decoded here matches TextDecoder, malformed bytes included', () => {
  const next = rng(0x85ebca6b)
  for (let round = 0; round < 5000; round++) {
    const bytes = new Uint8Array(string(next, round % 2 === 0))
    // Random bytes too, which are mostly malformed.
    if (round % 7 === 0) for (let i = 0; i < bytes.length; i++) bytes[i] = next() & 0xff
    const want = alone.decode(bytes)
    assert.equal(decodeHere(bytes, 0, bytes.length), want, `round ${round}: decodeHere`)
    assert.equal(utf8(bytes, 0, bytes.length), want, `round ${round}: utf8`)
  }
  for (const piece of [...pieces, ...broken]) {
    const bytes = new Uint8Array(piece)
    assert.equal(decodeHere(bytes, 0, bytes.length), alone.decode(bytes), JSON.stringify(piece))
  }
})
