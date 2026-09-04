/**
 * The examples (PLAN.md §9).
 *
 * Chosen to cover the inference rules and the enforcement rules, not to flatter
 * the format. Five of them are cases colbin does badly on or refuses outright,
 * because a demo that ships only its best cases is one nobody believes twice.
 */

export type Example = {
  key: string
  title: string
  note: string
  json: string
}

function products(n: number): string {
  const names = ['Tin Light', 'Steel Lamp', 'Copper Wire', 'Brass Hinge', 'Zinc Plate']
  const cities = ['Lima', 'Arequipa', 'Trujillo', 'Cusco', 'Piura']
  const rows = Array.from({ length: n }, (_, i) => ({
    id: 100000 + i * 7,
    sku: `SKU-${String(i).padStart(5, '0')}`,
    name: names[i % 5],
    city: cities[i % 5],
    price: 599 + i * 13,
    stock: i % 97,
    active: i % 3 !== 0,
  }))
  return JSON.stringify(rows, null, 2)
}

function metrics(n: number): string {
  const rows = Array.from({ length: n }, (_, i) => ({
    t: 1756200000 + i * 15,
    host: `node-${String(i % 12).padStart(2, '0')}`,
    cpu: Number((20 + (i % 1000) / 8).toFixed(3)),
    up: i % 7 !== 0,
  }))
  return JSON.stringify(rows, null, 2)
}


/**
 * A client table: 1000 records, nine fields.
 *
 * This is the shape the format is built for, and each column wins differently.
 * `id` and `updated` climb steadily, so the varint codec stores the step rather
 * than the value. `categoryID` and `age` are small integers with a narrow
 * spread. `name`, `city` and `email` repeat their vocabulary, which is what
 * packed5 is for, and `telephone` is digits, which it packs ten to a token.
 *
 * Generated from a fixed seed so the example is the same on every load, and so
 * the same document can be checked byte for byte against Go.
 */
function clients(n: number): string {
  // A small LCG rather than Math.random: an example that changes on every
  // reload cannot be compared against anything.
  let seed = 20260826
  const next = () => {
    seed = (Math.imul(seed, 1664525) + 1013904223) >>> 0
    return seed
  }
  const pick = <T>(list: T[]): T => list[next() % list.length]

  const first = ['María', 'José', 'Ana', 'Luis', 'Carmen', 'Jorge', 'Rosa', 'Miguel', 'Elena', 'Carlos']
  const last = ['García', 'Rodríguez', 'Fernández', 'Quispe', 'Mamani', 'Torres', 'Ramos', 'Flores', 'Díaz', 'Vargas']
  const cities = ['Lima', 'Arequipa', 'Trujillo', 'Cusco', 'Piura', 'Chiclayo', 'Iquitos', 'Tacna']

  // Records were updated over a fortnight, in roughly the order they were made.
  let updated = 1756200000

  const rows: string[] = []
  for (let i = 0; i < n; i++) {
    const nombre = `${pick(first)} ${pick(last)}`
    const slug = nombre
      .toLowerCase()
      .normalize('NFD')
      .replace(/[\u0300-\u036f]/g, '')
      .replace(' ', '.')
    updated += next() % 1200
    rows.push(
      JSON.stringify({
        id: 100000 + i,
        categoryID: 1 + (next() % 8),
        name: nombre,
        telephone: `+51 9${String(10000000 + (next() % 89999999))}`,
        email: `${slug}${i}@example.pe`,
        city: pick(cities),
        age: 18 + (next() % 62),
        active: next() % 5 !== 0,
        updated,
      })
    )
  }
  return `[\n  ${rows.join(',\n  ')}\n]`
}

export const examples: Example[] = [
  {
    key: 'clients',
    title: 'Clients, 1000 records',
    note: 'Nine fields over a thousand rows, and every column wins for a different reason. id and updated climb steadily, so the varint codec stores the step instead of the value; categoryID and age are small integers; name, city and email repeat their vocabulary, which is what packed5 is for. Hover or tap a column to see where its bytes went.',
    json: clients(1000),
  },
  {
    key: 'products',
    title: 'Products, 200 records',
    note: 'The ordinary case: an array of homogeneous records, which is what the format is for.',
    json: products(200),
  },
  {
    key: 'metrics',
    title: 'Metric points, 200',
    note: 'Timestamps that barely change between rows. The varint codec finds the delta and the column nearly disappears.',
    json: metrics(200),
  },
  {
    key: 'invoices',
    title: 'Invoices',
    note: 'Nesting and arrays of objects, where a row-oriented format repeats every key on every line.',
    json: JSON.stringify(
      [
        {
          serie: 'F001',
          numero: 120,
          cliente: { ruc: '20512345678', nombre: 'ACME S.A.C.' },
          lineas: [
            { sku: 'A-1', descripcion: 'Cable 2m', cantidad: 2, precio: 1550 },
            { sku: 'B-7', descripcion: 'Adaptador USB-C', cantidad: 1, precio: 4990 },
          ],
          total: 8090,
        },
        {
          serie: 'F001',
          numero: 121,
          cliente: { ruc: '20598765432', nombre: 'Globex E.I.R.L.' },
          lineas: [{ sku: 'C-3', descripcion: 'Teclado', cantidad: 7, precio: 12000 }],
          total: 84000,
        },
      ],
      null,
      2
    ),
  },
  {
    key: 'people',
    title: 'People',
    note: 'String-heavy, so packed5 is doing the work. The accented names go through its escape, byte for byte.',
    json: JSON.stringify(
      [
        { nombre: 'María Ñandú', ciudad: 'Lima', email: 'maria@example.pe', edad: 34 },
        { nombre: 'José Gómez', ciudad: 'Arequipa', email: 'jose@example.pe', edad: 41 },
        { nombre: 'Ana Rodríguez', ciudad: 'Cusco', email: 'ana@example.pe', edad: 29 },
        { nombre: 'Luis Fernández', ciudad: 'Trujillo', email: 'luis@example.pe', edad: 52 },
      ],
      null,
      2
    ),
  },
  {
    key: 'bigints',
    title: 'Integers past 2^53',
    note: 'JSON.parse would read this id as 7295013456321098800. The parser runs inside the wasm module precisely so it does not.',
    // Written as text, not built with JSON.stringify: passing these through a
    // JavaScript number would round them to ...800 before the page ever loaded,
    // which is the exact failure this example exists to show.
    json: `[
  { "id": 7295013456321098765, "kind": "message" },
  { "id": 7295013456321098766, "kind": "reply" },
  { "id": 18446744073709551615, "kind": "the uint64 ceiling" }
]`,
  },
  {
    key: 'single',
    title: 'One object',
    note: 'colbin LOSES here: a self-describing message carries a schema, and at this size the message is mostly schema. Binary mode, which needs the type at the far end, is about 3x smaller than the JSON.',
    json: JSON.stringify({ id: 42, name: 'Solo', price: 100 }, null, 2),
  },
  {
    key: 'scalars',
    title: 'A bare array of numbers',
    note: 'Also a loss. There is framing and nothing to amortise it over. Not every payload is a batch of records.',
    json: '[1, 2, 3, 4, 5]',
  },
  {
    key: 'nulls',
    title: 'Nulls and missing keys',
    note: 'Watch the decoded output: the third record gets "note": null even though the key was absent. Columns are dense, so the two are the same on the wire.',
    json: JSON.stringify(
      [{ id: 1, note: 'hi' }, { id: 2, note: null }, { id: 3 }],
      null,
      2
    ),
  },
  {
    key: 'mixed',
    title: 'Mixed numbers',
    note: 'One fractional value promotes the whole column to float64. The integers that shared it are stored as floats.',
    json: JSON.stringify([{ v: 1 }, { v: 2.5 }, { v: 3 }], null, 2),
  },
  {
    key: 'broken',
    title: 'A field that changes type',
    note: 'Refused, with the record and the field named. Encoding it anyway would mean an "any" column, which quietly throws away everything the columnar layout was for.',
    json: JSON.stringify(
      [{ sku: 'A-1', qty: 2 }, { sku: 'B-7', qty: 1 }, { sku: 'C-3', qty: 'seven' }],
      null,
      2
    ),
  },
]

export const defaultExample = examples[0]
