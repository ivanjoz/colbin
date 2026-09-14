// The `Codec` handle — PACKAGE_PLAN.md §6.
//
// Everything the module can do hangs off one instance, and the instance is
// what holds the schema. That is the shape the format's recommended delivery
// already implies: a section is sent once per connection, so the thing that
// parses it should outlive the message that follows it (§6.1).
//
// # Why the operations are synchronous
//
// `Codec.open()` is async because compiling WebAssembly is, and there is no
// synchronous escape — `new WebAssembly.Module` is blocked on the main thread
// above 4 KB and this module is 244 KB. Everything after that is not:
// `new WebAssembly.Instance` over an already-compiled module is synchronous,
// so `open` instantiates once and every call afterwards is a straight run of
// wasm calls with no `await` in it.
//
// That is what lets one instance back the whole handle rather than the pool
// `web/src/lib/codec.ts` needed. The pool existed because that file compiled
// lazily inside each call, so two calls fired with `Promise.all` could both be
// waiting on `compile()` and then interleave their writes into one instance's
// `INPUT`. With the compile hoisted into `open`, a call cannot be preempted
// between writing its input and reading its output, because there is nothing
// in it to preempt.

import { ColbinError, type Diagnostic } from './errors.ts'
import { readRows, readColumns, type ColumnTable, type ReadRowsOptions } from './materialize.ts'

/** Encode flags, mirroring `colbin::build`. */
const SELF_DESCRIBING = 1
const VERIFY = 2

/** Byte 0 of a message, with bit 2 set, says the section is in front of the
 * body. See `rust/wasm/src/lib.rs` and ENCODER.md. */
const ROOT_SCHEMA_BIT = 0x04

/** The module's exported ABI. `rust/wasm/src/lib.rs` is the definition. */
type Exports = {
  memory: WebAssembly.Memory
  alloc(n: number): number
  result_ptr(): number
  encode(len: number, flags: number): number
  section(): number
  set_schema(len: number): number
  decode(len: number): number
  materialize(len: number): number
  inspect_message(len: number): number
  last_error(): number
}

/**
 * Anything `Codec.open` can turn into a compiled module.
 *
 * A `Response` (or a promise of one, which is what `fetch(...)` is before it is
 * awaited) takes the streaming path; bytes are compiled directly; a `URL` or a
 * URL string is fetched first.
 */
export type WasmSource =
  | WebAssembly.Module
  | BufferSource
  | Response
  | URL
  | string
  | Promise<WebAssembly.Module | BufferSource | Response>

export type OpenOptions = {
  /** A module compiled already — by a service worker, a bundler plugin, or a
   * previous `Codec.open()`. Skips every fetch and every compile. */
  module?: WebAssembly.Module
  /** Where to get the module from, when it is not the entry's own default. */
  wasm?: WasmSource
}

/** One node of the field tree: a field, a table's column, or a list's element. */
export type Field = {
  name: string
  key: number
  /** The wire shape — `int64`, `string`, `[]struct`, `float64 column`. */
  type: string
  /** A pointer field: the format's "absent, rather than zero". */
  optional: boolean
  /** Absolute offsets into the message, so a hex view needs no arithmetic. */
  start: number
  end: number
  bytes: number
  children: Field[]
}

/** What `codec.inspect` returns: the message, described. */
export type Report = {
  totalBytes: number
  /** The schema section, when the message carries one. Zero out of band. */
  schemaBytes: number
  bodyBytes: number
  rootBytes: number
  /** Whether the run uses eight-bit keys, which costs a byte per present field. */
  wide: boolean
  /** The row count of the root's list or table, or 1 for a lone record. */
  rows: number
  /** Whether the document was wrapped, because its top level was not an object. */
  envelope: boolean
  fields: Field[]
}

/** A message and the schema section that describes it. */
export type Marshaled = {
  /** The body. Carries no schema: send `section` once per connection. */
  message: Uint8Array
  /** The schema for `message`, for the out-of-band delivery. */
  section: Uint8Array
  /** The same document with its section in front of it — what a file with
   * nowhere else to put a schema needs. Composed on first read rather than
   * encoded a second time. */
  readonly standalone: Uint8Array
  /** A successful encode can still have something to say: an always-null
   * column, a null where an object belongs. */
  warnings: string[]
}

export type MarshalOptions = {
  /**
   * Decode the message that was just built and walk it against the input
   * before handing it back, failing rather than returning bytes that do not
   * read back. Off by default: it costs about 70% on top of the encode
   * (PACKAGE_PLAN.md §2.2), and what it catches is a codec bug rather than a
   * caller's mistake. Worth turning on in a test suite, and in the first
   * deployment of anything whose shapes are new.
   */
  verify?: boolean
}

export type UnmarshalOptions = ReadRowsOptions & {
  /** The schema for this message alone, in place of the held one. Does not
   * replace what {@link Codec.setSchema} holds. */
  section?: Uint8Array
}

export type DecodeOptions = {
  /** The schema for this message alone, in place of the held one. */
  section?: Uint8Array
}

/** Which path a call took. `materialize` never writes JSON text; `json` is the
 * fallback for anything that is not the table shape §5 covers. */
export type Path = 'materialize' | 'json'

const encoder = new TextEncoder()
const decoder = new TextDecoder()

/**
 * The handle. One WebAssembly instance, the schema it holds, and the module's
 * whole surface.
 *
 * Construct it with `Codec.open()` from whichever entry point suits — `colbin`
 * carries the module inline, `colbin/asset` resolves it as a URL beside
 * itself.
 */
export class Codec {
  readonly #exports: Exports
  /** The section last written into this instance, by identity. Holding it is
   * what keeps a handle from re-parsing the same section once per message. */
  #applied: Uint8Array | null = null
  #held: Uint8Array | null = null
  #lastPath: Path = 'json'

  private constructor(exports: Exports) {
    this.#exports = exports
  }

  /**
   * Compiles the module and instantiates it. Async because compiling
   * WebAssembly is; nothing after it is.
   */
  static async open(options: OpenOptions = {}): Promise<Codec> {
    const module = options.module ?? (await compile(options.wasm))
    return Codec.fromModule(module)
  }

  /** A second handle over an already-compiled module — its own instance, its
   * own schema, no compile. */
  static fromModule(module: WebAssembly.Module): Codec {
    // No imports: the module does not abort, and a missing import would be a
    // link error rather than a trap on first use.
    const instance = new WebAssembly.Instance(module, {})
    return new Codec(instance.exports as unknown as Exports)
  }

  /**
   * Holds a schema section for the messages that follow — the delivery the
   * format recommends, where the section is sent once per connection and every
   * message after it carries only its body. `null` clears it.
   *
   * A self-describing message ignores this and uses its own.
   */
  setSchema(section: Uint8Array | null): void {
    this.#held = section && section.length > 0 ? section : null
  }

  /** Which path the last {@link unmarshal} took. §5.4: a caller that wants to
   * know it landed on the slow one should not have to guess. */
  get lastPath(): Path {
    return this.#lastPath
  }

  /**
   * A message to real objects, without JSON text in the middle when the shape
   * allows it.
   *
   * The fast path is the one that is exact past 2^53: a column whose values do
   * not fit a `Number` comes back as `bigint`, where the JSON fallback goes
   * through `JSON.parse` and rounds like any other JSON reader would.
   * {@link lastPath} says which ran.
   */
  unmarshal(message: Uint8Array, options: UnmarshalOptions = {}): unknown {
    const e = this.#exports
    this.#schema(options.section)

    // A module built without `materialize` has no fast path to try, and that
    // is not an error — the text path answers every shape. Treated exactly
    // like the materializer declining the shape.
    const length = this.canMaterialize ? this.#call(e.materialize(this.#write(message))) : 0
    if (length > 0) {
      this.#lastPath = 'materialize'
      // A copy this call owns outright, not a live view: `readRows` builds
      // several typed-array views over it, and any later `alloc` that grows
      // the module's memory would detach every one of them.
      const at = e.result_ptr()
      return readRows(e.memory.buffer.slice(at, at + length), options)
    }

    // Not the table shape the materializer covers — the text path.
    this.#lastPath = 'json'
    return JSON.parse(this.toJSONText(message, options))
  }

  /**
   * The columns themselves, for a caller feeding a grid or a chart that never
   * wants row objects — it skips even the 7 µs the row build costs.
   *
   * Returns `null` when the message is not the table shape §5 covers, since
   * there is no honest column view of a nested document; such a caller should
   * fall back to {@link unmarshal}.
   */
  columns(message: Uint8Array, options: DecodeOptions = {}): ColumnTable | null {
    const e = this.#exports
    this.#needs('materialize', 'materialize', 'rebuild the module with --features materialize')
    this.#schema(options.section)
    const length = this.#call(e.materialize(this.#write(message)))
    if (length === 0) return null
    const at = e.result_ptr()
    return readColumns(e.memory.buffer.slice(at, at + length))
  }

  /** A message back to JSON text. Every shape, and the only path for the ones
   * {@link unmarshal} reports as `json`. */
  toJSONText(message: Uint8Array, options: DecodeOptions = {}): string {
    const e = this.#exports
    this.#schema(options.section)
    const length = this.#call(e.decode(this.#write(message)))
    return decoder.decode(this.#result(length))
  }

  /**
   * A value or JSON text to a colbin message.
   *
   * A string is taken as JSON text and handed to the module as it stands;
   * anything else goes through `JSON.stringify` first — which means an integer
   * past 2^53 must arrive as text, since by the time it is a JavaScript
   * `Number` it has already been rounded.
   */
  marshal(value: unknown, options: MarshalOptions = {}): Marshaled {
    const e = this.#exports
    this.#needs('encode', 'encode', 'rebuild the module with --features encode')
    const text = typeof value === 'string' ? value : JSON.stringify(value)
    const flags = options.verify ? VERIFY : 0

    const length = this.#call(e.encode(this.#write(encoder.encode(text)), flags))
    const message = this.#result(length)
    const section = this.#result(this.#call(e.section()))
    // Warnings ride in the same envelope as errors, and reading them
    // overwrites the output — so both copies are already taken above.
    const warnings = this.#diagnostic().warnings

    let standalone: Uint8Array | undefined
    return {
      message,
      section,
      warnings,
      get standalone() {
        // The self-describing form is the same body behind the same section,
        // with bit 2 of the root byte set — so it is composed here rather than
        // encoded a second time. `js/tests/package.test.mjs` asserts this
        // against what the module writes for `SELF_DESCRIBING` over the whole
        // corpus, which is what makes the shortcut checkable rather than
        // merely plausible.
        if (standalone) return standalone
        const out = new Uint8Array(section.length + message.length)
        out[0] = message[0] | ROOT_SCHEMA_BIT
        out.set(section, 1)
        out.set(message.subarray(1), 1 + section.length)
        standalone = out
        return out
      },
    }
  }

  /**
   * The field tree of a message, with every node's byte span — absolute
   * offsets into the bytes handed in, so a hex view needs no arithmetic.
   *
   * Needs a module built with the `inspect` feature, which is **not** the one
   * `colbin` and `colbin/asset` carry: the span walk is 15 KB that nothing but
   * a tool drawing the bytes ever calls, so it has a module of its own behind
   * `colbin/inspect`.
   */
  inspect(message: Uint8Array, options: DecodeOptions = {}): Report {
    const e = this.#exports
    this.#needs('inspect_message', 'inspect', "import { Codec } from 'colbin/inspect'")
    this.#schema(options.section)
    const length = this.#call(e.inspect_message(this.#write(message)))
    return JSON.parse(decoder.decode(this.#result(length))) as Report
  }

  /** Whether this handle's module can {@link inspect} — i.e. whether it came
   * from `colbin/inspect` or another build with the feature on. */
  get canInspect(): boolean {
    return typeof this.#exports.inspect_message === 'function'
  }

  /** Whether this handle's module carries §5's fast path. True of every build
   * this package ships; false only for a module a caller supplied. */
  get canMaterialize(): boolean {
    return typeof this.#exports.materialize === 'function'
  }

  /** The flag the encoder needs to write a document that carries its own
   * schema, for a caller composing `encode` flags directly. */
  static get SELF_DESCRIBING(): number {
    return SELF_DESCRIBING
  }

  // --- the boundary ------------------------------------------------------

  /**
   * Writes `input` into the module and returns its length, which is what every
   * export takes. Allocate first and take the view second: allocating may grow
   * memory, and growing detaches every view taken before it.
   */
  #write(input: Uint8Array): number {
    const e = this.#exports
    const at = e.alloc(input.length)
    new Uint8Array(e.memory.buffer).set(input, at)
    return input.length
  }

  /** A copy of the module's last output. */
  #result(length: number): Uint8Array {
    const e = this.#exports
    const at = e.result_ptr()
    return new Uint8Array(e.memory.buffer).slice(at, at + length)
  }

  /**
   * Refuses a call the module was not built to answer, by name, before it
   * becomes a `TypeError: e.inspect_message is not a function` several frames
   * from anything a caller can act on.
   *
   * Which exports exist is a property of the bytes, not of this wrapper: the
   * module is built per feature set (`rust/wasm/Cargo.toml`), and the entries
   * differ in which build they carry.
   */
  #needs(exported: keyof Exports, feature: string, fix: string): void {
    if (typeof this.#exports[exported] === 'function') return
    throw new Error(
      `colbin: this module was built without \`${feature}\`, so it has no ` +
        `\`${exported}\` export. ${fix}`,
    )
  }

  /** Turns the ABI's negative length into the thrown error §6 promises. */
  #call(length: number): number {
    if (length < 0) throw new ColbinError(this.#diagnostic())
    return length
  }

  #diagnostic(): Diagnostic {
    const e = this.#exports
    return JSON.parse(decoder.decode(this.#result(e.last_error()))) as Diagnostic
  }

  /**
   * Makes this instance hold `section` — the per-call one when there is one,
   * the handle's own otherwise.
   *
   * Compared by identity, not by content: re-parsing a section the instance
   * already holds is exactly the cost §6.1 says a handle exists to avoid, and
   * a caller who mutates a section's bytes in place and passes the same object
   * again is not a case worth a memcmp per message.
   */
  #schema(section?: Uint8Array): void {
    const want = section && section.length > 0 ? section : this.#held
    if (want === this.#applied) return
    const e = this.#exports
    this.#call(want === null ? e.set_schema(0) : e.set_schema(this.#write(want)))
    this.#applied = want
  }
}

/** Turns whatever `open` was given into a compiled module. */
async function compile(source?: WasmSource): Promise<WebAssembly.Module> {
  if (source === undefined) {
    throw new TypeError(
      'colbin: no module to compile — pass `wasm` or `module` to Codec.open(), ' +
        "or import an entry that carries one ('colbin' or 'colbin/asset')",
    )
  }
  const resolved = await source
  if (resolved instanceof WebAssembly.Module) return resolved
  if (typeof resolved === 'string' || resolved instanceof URL) {
    return compileResponse(await fetch(resolved))
  }
  if (typeof Response !== 'undefined' && resolved instanceof Response) {
    return compileResponse(resolved)
  }
  return WebAssembly.compile(resolved as BufferSource)
}

async function compileResponse(response: Response): Promise<WebAssembly.Module> {
  if (!response.ok) {
    throw new Error(`colbin: ${response.url || 'the module'} answered ${response.status}`)
  }
  // `compileStreaming` needs the right content type and some static hosts do
  // not set it, so fall back on the header rather than fail on it.
  if (
    typeof WebAssembly.compileStreaming === 'function' &&
    response.headers.get('content-type')?.includes('application/wasm')
  ) {
    return WebAssembly.compileStreaming(response)
  }
  return WebAssembly.compile(await response.arrayBuffer())
}

/**
 * The `Codec` an entry point exports: `open()` with the entry's own way of
 * finding the module already wired in, overridable per call.
 *
 * Compiled once per entry and cached, so a page that opens a second handle
 * pays for an instance and not for a compile.
 */
export function codecFor(source: () => WasmSource): CodecEntry {
  let compiled: Promise<WebAssembly.Module> | undefined
  const module = () => (compiled ??= compile(source()))
  return {
    async open(options: OpenOptions = {}) {
      if (options.module) return Codec.fromModule(options.module)
      if (options.wasm !== undefined) return Codec.open(options)
      return Codec.fromModule(await module())
    },
    fromModule: Codec.fromModule,
    preload() {
      void module().catch(() => {})
    },
  }
}

export type CodecEntry = {
  /** Compile (once per entry) and instantiate. */
  open(options?: OpenOptions): Promise<Codec>
  /** A handle over a module compiled already — no fetch, no compile. */
  fromModule(module: WebAssembly.Module): Codec
  /** Start the compile without waiting for it, so the first real call is not
   * the one that pays for it. Failures are deliberately swallowed — `open`
   * will surface the same one. */
  preload(): void
}

/**
 * The free functions each entry re-exports — `unmarshal(bytes)` without
 * holding a handle, for the caller who has one message to read and no
 * connection to hold a schema for.
 *
 * They share one lazily-opened codec rather than opening a fresh one per call:
 * a fresh instance per message is what §6.1 argued against, and none of these
 * hold a schema across calls, so there is nothing for the sharing to leak.
 */
export function convenience(entry: CodecEntry): Convenience {
  let held: Promise<Codec> | undefined
  const codec = () => (held ??= entry.open())
  return {
    async unmarshal(message, options) {
      return (await codec()).unmarshal(message, options)
    },
    async columns(message, options) {
      return (await codec()).columns(message, options)
    },
    async toJSONText(message, options) {
      return (await codec()).toJSONText(message, options)
    },
    async marshal(value, options) {
      return (await codec()).marshal(value, options)
    },
    async inspect(message, options) {
      return (await codec()).inspect(message, options)
    },
  }
}

export type Convenience = {
  unmarshal(message: Uint8Array, options?: UnmarshalOptions): Promise<unknown>
  columns(message: Uint8Array, options?: DecodeOptions): Promise<ColumnTable | null>
  toJSONText(message: Uint8Array, options?: DecodeOptions): Promise<string>
  marshal(value: unknown, options?: MarshalOptions): Promise<Marshaled>
  inspect(message: Uint8Array, options?: DecodeOptions): Promise<Report>
}
