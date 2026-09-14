/**
 * The examples.
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
    note: 'Nine fields over a thousand rows. Past eight rows a slice of all-scalar records is transposed into a table — one key per column rather than one per field per row — and each column then goes through the block codec, which stores id and updated as the step between rows instead of the value. 2.8x against the JSON, with the schema sent once.',
    json: clients(1000),
  },
  {
    key: 'products',
    title: 'Products, 200 records',
    note: 'The ordinary case: an array of homogeneous records, which is what the columnar layout is for. Two hundred rows, transposed, 3.5x.',
    json: products(200),
  },
  {
    key: 'metrics',
    title: 'Metric points, 200',
    note: 'Timestamps that barely change between rows. The column codec finds the delta and the column nearly disappears — the best ratio here, at 3.8x.',
    json: metrics(200),
  },
  {
    key: 'invoices',
    title: 'Invoices',
    note: 'Nesting and arrays of objects. Each invoice has three lines, which is under the row count where a table pays for itself, so the lines stay row-wise. The writer picks per field rather than per message.',
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
    note: 'String-heavy, which is where the format has least to offer: a string is its bytes and a length either way. 1.8x, against 3.8x on the metrics. The packed5 encoding that used to close some of that gap is being replaced and is off.',
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
    note: 'JSON.parse would read this id as 7295013456321098800. The parser runs inside the wasm module precisely so it does not — the digits go straight to an int64 and come back out the same.',
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
    note: 'One flat object, and the case that shows why the schema travels out of band. As a stream it is 3.2x smaller than the JSON, because the section is sent once and this message is eleven bytes. As one standalone file it carries its own schema and is exactly the size of the JSON — a tie, on a document that is mostly schema.',
    json: JSON.stringify({ id: 42, name: 'Solo', price: 100 }, null, 2),
  },
  {
    key: 'scalars',
    title: 'A bare array of numbers',
    note: 'A LOSS, and the plainest one: 0.58x as a file. There is framing and nothing to amortise it over, and the schema describing it is longer than the data. Not every payload is a batch of records.',
    json: '[1, 2, 3, 4, 5]',
  },
  {
    key: 'nulls',
    title: 'Nulls and missing keys',
    note: 'Watch the decoded output: the third record gets \"note\": null even though the key was absent. A field holding its zero is not written at all, so an absent key and an explicit null are the same bytes — and nothing on the way back can tell them apart.',
    json: JSON.stringify(
      [{ id: 1, note: 'hi' }, { id: 2, note: null }, { id: 3 }],
      null,
      2
    ),
  },
  {
    key: 'mixed',
    title: 'Mixed numbers',
    note: 'One fractional value promotes the whole column to float64, and the integers that shared it are stored as floats. Another loss as a file, at 0.79x.',
    json: JSON.stringify([{ v: 1 }, { v: 2.5 }, { v: 3 }], null, 2),
  },
  {
    key: 'broken',
    title: 'A field that changes type',
    note: 'Refused, with the record and the field named. Encoding it anyway would mean giving up the type — and a column whose type is \"whatever this row had\" is not a column.',
    json: JSON.stringify(
      [{ sku: 'A-1', qty: 2 }, { sku: 'B-7', qty: 1 }, { sku: 'C-3', qty: 'seven' }],
      null,
      2
    ),
  },
]

export const defaultExample = examples[0]
