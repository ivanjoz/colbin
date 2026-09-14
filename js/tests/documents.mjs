// The documents the encoder is driven over.
//
// Shared between encode.test.mjs, which checks the module against itself, and
// emit.mjs, which hands the same messages to Go. One list, so a case that only
// this module can read cannot hide in the gap between the two.
//
// Chosen to cover `rust/ENCODER.md` §1-2's inference rules and §4's
// enforcement, not to flatter the format: five of them are shapes colbin does
// badly on or refuses.

export const documents = [
  {
    name: 'flat-object',
    json: { id: 1, name: 'Tin Light', price: 599, active: true },
  },
  {
    name: 'records',
    json: Array.from({ length: 12 }, (_, i) => ({
      id: 100000 + i * 7,
      sku: `SKU-${String(i).padStart(5, '0')}`,
      name: ['Tin Light', 'Steel Lamp', 'Copper Wire'][i % 3],
      price: 599 + i * 13,
      stock: i % 97,
      active: i % 3 !== 0,
    })),
  },
  {
    name: 'records-past-the-table-threshold',
    // Every field an integer, so the element is columnable and a list of them
    // crosses into the transposed layout at eight.
    json: { rows: Array.from({ length: 40 }, (_, i) => ({ id: i, qty: i % 7, cents: 499 + i * 13 })) },
  },
  {
    name: 'records-under-the-table-threshold',
    json: { rows: Array.from({ length: 7 }, (_, i) => ({ id: i, qty: i % 7, cents: 499 + i * 13 })) },
  },
  {
    name: 'nested-objects',
    json: { id: 9, inner: { sku: 'ABC-1', qty: 2, cents: 1999 }, note: 'pickup' },
  },
  {
    name: 'four-deep',
    json: { a: { b: { c: { d: 1, e: 'deep' } } } },
  },
  {
    name: 'scalar-arrays',
    json: { ints: [1, 2, 3, -4], strings: ['a', '', 'ccc'], big: [9007199254740993, 1] },
  },
  {
    name: 'nulls-and-missing-keys',
    // ENCODER.md §6's dense-column property, made visible: the third record comes
    // back with note null, because a column carries a value for every row.
    json: [{ id: 1, note: 'hi' }, { id: 2, note: null }, { id: 3 }],
  },
  {
    name: 'mixed-numbers',
    // One 2.5 promotes the whole column to float64.
    json: [{ v: 1 }, { v: 2.5 }, { v: 3 }],
  },
  {
    name: 'big-integers',
    // Past 2^53, which JSON.parse would have rounded before the codec saw it.
    json: { small: 1, snowflake: 7295013456321098765n, max: 9223372036854775807n },
  },
  {
    name: 'unsigned-past-int64',
    json: { v: 18446744073709551615n },
  },
  {
    name: 'floats',
    json: { one: 1.0, tenth: 0.1, neg: -2.25, tiny: 5e-324, huge: 1.7976931348623157e308, zero: 0.0 },
  },
  {
    name: 'strings',
    json: { plain: 'hello', accented: 'el niño comió jamón', cjk: '日本語', emoji: 'party 🎉', empty: '' },
  },
  {
    name: 'escapes',
    json: { quote: 'say "hi"', slash: 'a\\b', control: '', tab: 'a\tb' },
  },
  {
    name: 'long-strings',
    // A narrow list element past 255 bytes, which is the shape that found a bug
    // in the Go writer while this encoder was being written.
    json: { lines: Array.from({ length: 3 }, (_, i) => ({ note: 'x'.repeat(300 + i), qty: i })) },
  },
  {
    name: 'seventeen-fields',
    // One field past what four key bits hold, which is the only thing that puts
    // a message on the wide path now.
    json: Object.fromEntries(Array.from({ length: 17 }, (_, i) => [`f${i}`, i + 1])),
  },
  {
    name: 'sixteen-fields',
    json: Object.fromEntries(Array.from({ length: 16 }, (_, i) => [`f${i}`, i + 1])),
  },
  { name: 'bare-scalar', json: 42 },
  { name: 'bare-string', json: 'hello' },
  { name: 'scalar-array', json: [1, 2, 3, 4, 5] },
  { name: 'string-array', json: ['a', 'b', 'c'] },
  {
    name: 'empty-strings-everywhere',
    json: [{ a: '', b: 0 }, { a: '', b: 0 }],
  },
  {
    name: 'duplicate-keys',
    // The last occurrence wins, as JSON.parse does. Written as text because an
    // object literal cannot hold one.
    text: '{"a":1,"a":2}',
  },
]

/** The refusals: shapes the encoder must reject rather than encode badly. */
export const refusals = [
  { name: 'type-conflict', text: '[{"qty":1},{"qty":"two"}]', match: /type conflict/ },
  { name: 'empty-array', text: '[]', match: /no shape to infer/ },
  { name: 'null-root', text: 'null', match: /no shape to infer/ },
  { name: 'float-array', text: '{"v":[1.5,2.5]}', match: /array of floats/ },
  { name: 'bool-array', text: '{"v":[true,false]}', match: /array of booleans/ },
  { name: 'nested-array', text: '{"v":[[1,2]]}', match: /array of arrays/ },
  {
    name: 'int-spanning-both-ends',
    text: '[{"v":-1},{"v":18446744073709551615}]',
    match: /below zero and above the int64 maximum/,
  },
  { name: 'malformed', text: '{"a":', match: /./ },
]

/** JSON text for a document, with BigInt written as the digits it is. */
export function textOf(document) {
  if (document.text !== undefined) return document.text
  return JSON.stringify(document.json, (_, value) =>
    typeof value === 'bigint' ? new RawNumber(value.toString()) : value,
  ).replace(/"__raw:(-?\d+)"/g, '$1')
}

class RawNumber {
  constructor(digits) {
    this.digits = digits
  }
  toJSON() {
    return `__raw:${this.digits}`
  }
}
