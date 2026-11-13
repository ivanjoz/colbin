// colbin/decode: a colbin decoder in plain JavaScript.
//
// No WebAssembly, no Node built-ins, no import.meta, nothing at import time:
// this entry and what it imports are the whole of it, so a bundle that imports
// it carries no module and no base64, and it runs wherever a Uint8Array does.
//
//   import { Decoder } from 'colbin/decode'
//
//   const decoder = new Decoder()
//   decoder.setSchema(section)          // once per connection
//   const rows = decoder.decode(bytes)  // every message after it
//
// A message that carries its own section needs no setSchema at all.

import { ColbinError } from '../errors.ts'
import { fail, BAD_DESCRIPTOR } from './fail.ts'
import { Schema, parseSchema } from './schema.ts'
import { Walker, type Options, type Column, type ColumnTable } from './walk.ts'

export { ColbinError }
export type { Options, Column, ColumnTable }

const ROOT_SCHEMA = 0x04
const NO_SCHEMA = 'the message carries no schema section and none was set: call setSchema with the section, or send the message self-describing'

export type Input = Uint8Array | ArrayBuffer | ArrayBufferView

function bytesOf(input: Input): Uint8Array {
  if (input instanceof Uint8Array) return input
  if (input instanceof ArrayBuffer) return new Uint8Array(input)
  if (ArrayBuffer.isView(input)) return new Uint8Array(input.buffer, input.byteOffset, input.byteLength)
  throw new TypeError('colbin: decode takes a Uint8Array, an ArrayBuffer or a view of one')
}

/**
 * A decoder holding the schema section its connection sent, and the last
 * section it read in front of a message, so neither is parsed twice.
 */
export class Decoder {
  #held: Schema | null = null
  #heldBytes: Uint8Array | null = null
  #seenBytes: Uint8Array | null = null
  #seen: Schema | null = null
  #options: Options | undefined

  constructor(options?: Options) {
    this.#options = options
  }

  /** Holds a section for the messages that follow; `null` clears it. */
  setSchema(section: Input | null): void {
    const bytes = section === null ? null : bytesOf(section)
    if (bytes !== null && this.#heldBytes !== null && same(this.#heldBytes, bytes, 0)) return
    this.#held = bytes === null ? null : parseSchema(bytes)
    this.#heldBytes = bytes === null ? null : bytes.slice()
  }

  /** A message → its value: objects, arrays and scalars, no JSON text. */
  decode(input: Input): unknown {
    return this.#run(input, false)
  }

  /**
   * A message's table as columns, for a grid or a chart that never wants row
   * objects: typed arrays for numbers, arrays for strings and bools, and a
   * `valid` bitmap on a nullable column. The shape colbin's wasm `columns()`
   * returns. `null` when the message is not one table: the root must be an
   * envelope of a table, or a struct whose one field is a table (`field`
   * names it); a list-of-structs column has no flat form either.
   */
  columns(input: Input): ColumnTable | null {
    return this.#run(input, true) as ColumnTable | null
  }

  #run(input: Input, columnar: boolean): unknown {
    const bytes = bytesOf(input)
    const n = bytes.length
    // A typed narrow message has no root byte: the message is its key run.
    const root = n > 0 && bytes[0] >> 4 === 0xd ? bytes[0] : 0xd0
    const kind = root & ~ROOT_SCHEMA
    let schema = this.#held
    let at = root === bytes[0] ? 1 : 0
    if (root & ROOT_SCHEMA) {
      // A dictionary message carries no section of its own.
      if (kind === 0xd2 || kind === 0xda) fail(BAD_DESCRIPTOR, 0)
      schema = this.#section(bytes)
      at += schema.size
    }
    if (kind === 0xd1 || kind === 0xd3) {
      // A document without a schema holds no table.
      if (columnar) return null
      return new Walker(bytes, schema ?? new Schema([], 0), this.#options).dynamicRoot(at, kind === 0xd3)
    }
    if ((kind & 0xf5) !== 0xd0) fail(BAD_DESCRIPTOR, 0)
    if (schema === null) fail(NO_SCHEMA, 0)
    const walker = new Walker(bytes, schema, this.#options)
    if (kind & 2) at = walker.dictionaries(at)
    const wide = (kind & 8) !== 0
    return columnar ? walker.rootColumns(at, n, wide) : walker.root(at, n, wide)
  }

  /** The section in front of a self-describing message, parsed once for a run
   * of messages that all carry the same one. */
  #section(bytes: Uint8Array): Schema {
    const seen = this.#seenBytes
    if (seen !== null && bytes.length > seen.length && same(seen, bytes, 1)) return this.#seen as Schema
    const schema = parseSchema(bytes, 1)
    this.#seen = schema
    this.#seenBytes = bytes.slice(1, 1 + schema.size)
    return schema
  }
}

/** Whether `b[at..at + a.length)` is `a`, and `a` is all of `b` past `at`
 * when `at` is 0. */
function same(a: Uint8Array, b: Uint8Array, at: number): boolean {
  const n = a.length
  if ((at === 0 && b.length !== n) || b.length < at + n) return false
  for (let i = 0; i < n; i++) if (a[i] !== b[at + i]) return false
  return true
}

/** A message's table as columns (Decoder.columns), or null. */
export function columns(input: Input, options?: Options & { schema?: Input | null }): ColumnTable | null {
  return decoderFor(options).columns(input)
}

/**
 * Decodes one message. `schema` is the section for a message that does not
 * carry its own; a decoder per connection (`new Decoder()` and `setSchema`)
 * saves parsing it again for every message.
 */
export function decode(input: Input, options?: Options & { schema?: Input | null }): unknown {
  return decoderFor(options).decode(input)
}

let shared: Decoder | null = null

/** A decoder for one call: a shared one when the options ask for nothing. */
function decoderFor(options?: Options & { schema?: Input | null }): Decoder {
  if (options?.schema == null && options?.int64 === undefined) {
    shared ??= new Decoder()
    shared.setSchema(null)
    return shared
  }
  const decoder = new Decoder(options)
  if (options?.schema != null) decoder.setSchema(options.schema)
  return decoder
}
