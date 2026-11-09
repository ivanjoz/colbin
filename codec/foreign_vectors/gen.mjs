// Writes the messages codec/foreign_test.go decodes: what the browser's encoder
// (codec.marshal, the Rust infer.rs behind the wasm module) produces, which is
// the only thing a foreign section ever is in practice.
//
//   node codec/foreign_vectors/gen.mjs
//
// The files are committed, so the Go tests need neither node nor a built wasm.
import { writeFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { Codec } from '../../js/dist/index.js'

const here = dirname(fileURLToPath(import.meta.url))
const codec = await Codec.open()

function product(i) {
  const row = {
    id: i + 1,
    qty: i % 200,
    price: i * 0.25 + 0.1,          // fractional: float64
    weight: (i % 40) * 3,           // all whole numbers: inferred int64, Go holds a float32
    tags: [i, i + 1, -(i % 7)],     // []int64, Go holds []int32
    name: `product ${i}`,
    seller: { name: `seller ${i % 13}`, rating: i % 5 },
    note: i % 3 === 0 ? null : `note ${i}`, // null in some rows: a pointer to a string
    _updatedFields: ['price'],      // UI-only: the Go type has no such field
  }
  return row
}

const write = (name, value) => {
  const out = codec.marshal(value)
  writeFileSync(join(here, `${name}.standalone.bin`), out.standalone)
  writeFileSync(join(here, `${name}.message.bin`), out.message)
  console.log(name, 'standalone', out.standalone.length, 'message', out.message.length, 'section', out.section.length)
}

write('products500', Array.from({ length: 500 }, (_, i) => product(i)))
write('products3', Array.from({ length: 3 }, (_, i) => product(i)))

const overflow = Array.from({ length: 500 }, (_, i) => product(i))
overflow[7].qty = 70000 // past int16
write('overflow500', overflow)

// A struct at the root, with a list and a nested struct inside it.
write('order', {
  number: 1234,
  customer: { name: 'ada', tier: 2 },
  lines: Array.from({ length: 4 }, (_, i) => ({ sku: `sku-${i}`, qty: i + 1, price: i + 0.5 })),
  total: 99.5,
})
write('order600', {
  number: 1234,
  customer: { name: 'ada', tier: 2 },
  lines: Array.from({ length: 600 }, (_, i) => ({ sku: `sku-${i}`, qty: i + 1, price: i + 0.5 })),
  total: 99.5,
})

// The same products without the nested struct and the array, so the rows are all
// columns and the message is a table: what the table readers are measured on.
write('flat500', Array.from({ length: 500 }, (_, i) => {
  const { tags, seller, _updatedFields, ...flat } = product(i)
  flat.note = `note ${i}` // never null: a nullable column is not one the encoder transposes
  return flat
}))
