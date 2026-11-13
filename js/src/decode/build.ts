// The objects a decode hands back, built by functions generated once per shape.
//
// An object literal with the same keys in the same order is one hidden class
// in every engine, and a function that returns one is the cheapest way to make
// many of them: the engine clones a boilerplate rather than adding properties
// one at a time. So a record starts as a generated literal of its zeros, and a
// table's rows come from a generated loop that reads each row's values straight
// out of the columns.
//
// Under a Content-Security-Policy without 'unsafe-eval', `new Function` throws;
// that is detected once and every builder falls back to a generic one, which
// assigns the same keys in the same order.
//
// The only thing taken from the data into generated source is a property name,
// and it goes in through JSON.stringify. A name `__proto__` is written as a
// computed key, which makes it an own property as JSON.parse does, rather than
// setting the prototype as a literal `"__proto__":` would.

let evalAllowed: boolean | null = null

/** Whether `new Function` works here; asked once. */
export function canGenerate(): boolean {
  if (evalAllowed === null) {
    try {
      evalAllowed = new Function('return 1')() === 1
    } catch {
      evalAllowed = false
    }
  }
  return evalAllowed
}

/** For tests: pretend the policy forbids generated code (or stop pretending). */
export function forceGeneric(on: boolean): void {
  evalAllowed = on ? false : null
}

function key(name: string): string {
  return name === '__proto__' ? `["__proto__"]` : JSON.stringify(name)
}

/** Sets a property as an own property whatever its name. */
export function put(target: Record<string, unknown>, name: string, value: unknown): void {
  if (name === '__proto__') {
    Object.defineProperty(target, name, { value, writable: true, enumerable: true, configurable: true })
  } else target[name] = value
}

/**
 * A function building an object of `names` from an array of their values,
 * slot k for names[k]: one literal per struct type, so every object of the type
 * shares one hidden class and the stores are monomorphic, where assigning each
 * decoded field in turn would go megamorphic once a page holds several types.
 */
export function arrayFactory(names: string[]): (v: unknown[]) => Record<string, unknown> {
  if (canGenerate()) {
    const parts = names.map((name, k) => `${key(name)}:v[${k}]`)
    return new Function(`return function(v){return{${parts.join(',')}}}`)() as (v: unknown[]) => Record<string, unknown>
  }
  return (v) => {
    const out: Record<string, unknown> = {}
    for (let k = 0; k < names.length; k++) put(out, names[k], v[k])
    return out
  }
}

// How a row reads one field's value from its column.
export const K_CONST = 0 // the same value every row: an absent column's zero
export const K_VAL = 1 // column[i]
export const K_BOOL = 2 // column[i] === 1
export const K_DICT = 3 // dict[ids[i]]

export type RowColumn = { kind: number; values: unknown; dict: unknown }

export type RowBuilder = (out: unknown[], offset: number, n: number, columns: RowColumn[]) => void

/** A row builder for fields `names` read the ways `kinds` says. */
export function rowBuilder(names: string[], kinds: number[]): RowBuilder {
  if (!canGenerate()) {
    return (out, off, n, c) => {
      for (let i = 0; i < n; i++) {
        const row: Record<string, unknown> = {}
        for (let k = 0; k < kinds.length; k++) {
          const values = c[k].values as ArrayLike<unknown>
          const kind = kinds[k]
          put(row, names[k], kind === K_CONST ? values : kind === K_VAL ? values[i] : kind === K_BOOL ? values[i] === 1 : (c[k].dict as unknown[])[values[i] as number])
        }
        out[off + i] = row
      }
    }
  }
  const unpack: string[] = []
  const entries: string[] = []
  kinds.forEach((kind, k) => {
    const v = `v${k}`
    unpack.push(`const ${v}=c[${k}].values,d${k}=c[${k}].dict`)
    const expr = kind === K_CONST ? v : kind === K_VAL ? `${v}[i]` : kind === K_BOOL ? `${v}[i]===1` : `d${k}[${v}[i]]`
    entries.push(`${key(names[k])}:${expr}`)
  })
  return new Function(
    `return function(out,off,n,c){${unpack.join(';')};for(let i=0;i<n;i++)out[off+i]={${entries.join(',')}}}`,
  )() as RowBuilder
}
