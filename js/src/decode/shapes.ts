// Dynamic maps (a document without a schema, a map of any) as objects. Adding
// the keys one at a time is a keyed store each, megamorphic in a decoder, and
// past a dozen keys it turns the object into a dictionary: slower to build and
// to read than what JSON.parse returns. Documents repeat their key sequences,
// so a sequence seen before is built by a literal generated for it, one hidden
// class per sequence, as fast as a struct's.

import { arrayFactory, canGenerate, put } from './build.ts'

/** A key sequence and, once it has come back, the literal that builds it. */
class Shape {
  keys: string[]
  make: ((v: unknown[]) => Record<string, unknown>) | null = null
  constructor(keys: string[]) {
    this.keys = keys
  }
}

/** Shapes by the hash of their key sequence; one per hash, a collision builds
 * the slow way. Bounded: a stream of ever new sequences clears it. */
const shapes = new Map<number, Shape>()
const MAX_SHAPES = 2048
/** Keys past this many build the slow way: a literal that long is rare and
 * costly to compile. */
const MAX_KEYS = 256

/**
 * The object of `keys[k]` → `values[k]`, the last of equal keys winning.
 * `hash` identifies the sequence (the caller hashes as it reads the keys); it
 * only picks the shape, the keys themselves are compared.
 */
export function object(keys: string[], values: unknown[], hash: number): Record<string, unknown> {
  const n = keys.length
  if (n > 0 && n <= MAX_KEYS && canGenerate()) {
    const shape = shapes.get(hash)
    if (shape !== undefined) {
      const known = shape.keys
      let same = known.length === n
      for (let k = 0; same && k < n; k++) same = known[k] === keys[k]
      if (same) {
        // The second sighting compiles: a one-off document pays nothing.
        if (shape.make === null) shape.make = arrayFactory(keys)
        return shape.make(values)
      }
    } else {
      if (shapes.size >= MAX_SHAPES) shapes.clear()
      shapes.set(hash, new Shape(keys))
    }
  }
  const out: Record<string, unknown> = {}
  for (let k = 0; k < n; k++) put(out, keys[k], values[k])
  return out
}
