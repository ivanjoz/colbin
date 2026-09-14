// The page's wrapper around the published package — PACKAGE_PLAN.md phase 6.
//
// Everything that used to be here (the instance pool, the ABI, the
// materializer's buffer reader) is `colbin` now, and this file is what is left
// once that is taken out: one held handle, and the `Outcome` union the page's
// error panel is written against. The package throws a `ColbinError`, which is
// the right shape for a library and the wrong one for a UI that renders the
// diagnostic as a value.
//
// The import is `colbin/inspect`, which is the module *with* the span walk in
// it — this page is the reason that walk exists, and it is the only caller of
// `codec.inspect()` there is. Everything the package publishes by default
// leaves it out, because 15 KB of byte-drawing is not something a client
// reading a service's answers should download (`rust/wasm/Cargo.toml`, feature
// `inspect`). The page pays 90 KB gzipped where an ordinary consumer pays 84.
//
// It is an *asset* entry rather than the inline default for the same reason
// `colbin/asset` is: the module is served as a `.wasm` file instead of a third
// of a megabyte of base64 in the bundle. And it is the entry whose
// `new URL('./colbin.inspect.wasm', import.meta.url)` has to survive a real
// bundler: this site is that test (§9), so importing it any other way here
// would leave nothing checking it.

import {
  Codec,
  ColbinError,
  type Diagnostic,
  type Field,
  type Path,
  type Report,
} from 'colbin/inspect'

export type { Diagnostic, Field, Path, Report }

export type Outcome<T> =
  | { ok: true; value: T; warnings: string[] }
  | { ok: false; error: Diagnostic }

/** A message and the schema section that describes it. */
export type Encoded = {
  message: Uint8Array
  section: Uint8Array
  /** The same document with its section in front of it, which is what a file
   * that has to stand alone needs. */
  standalone: Uint8Array
}

/**
 * One handle for the page, opened once.
 *
 * The schema is not held on it: the page encodes and decodes different
 * documents on every keystroke, so each call passes its own section. A client
 * talking to one endpoint would `setSchema` instead and send the section once
 * per connection, which is the delivery the format is built for.
 */
let opened: Promise<Codec> | undefined
const handle = () => (opened ??= Codec.open())

/** Warms the compile, so the first keystroke is not what waits for it. */
export function preload(): void {
  void handle().catch(() => {})
}

/** Turns the package's thrown error back into the value the page renders. */
function failed(err: unknown): { ok: false; error: Diagnostic } {
  if (err instanceof ColbinError) {
    return {
      ok: false,
      error: {
        code: err.code,
        offset: err.offset,
        line: err.line,
        path: err.path,
        message: err.message,
        warnings: err.warnings,
      },
    }
  }
  return {
    ok: false,
    error: {
      code: -1,
      offset: -1,
      line: -1,
      path: '',
      message: err instanceof Error ? err.message : String(err),
      warnings: [],
    },
  }
}

/**
 * JSON text to a colbin message.
 *
 * Both deliveries come back from one call — `standalone` is composed from the
 * message and its section rather than encoded again, so the page can show both
 * numbers without the timing being a lie about either.
 */
export async function encode(json: string, verify = true): Promise<Outcome<Encoded>> {
  try {
    const codec = await handle()
    const { message, section, standalone, warnings } = codec.marshal(json, { verify })
    return { ok: true, value: { message, section, standalone }, warnings }
  } catch (err) {
    return failed(err)
  }
}

/**
 * A colbin message back to JSON text.
 *
 * `section` is the schema, for the out-of-band delivery. Pass none for a
 * message that carries its own — which is what a dropped file will be.
 */
export async function decode(message: Uint8Array, section?: Uint8Array): Promise<Outcome<string>> {
  try {
    const codec = await handle()
    return { ok: true, value: codec.toJSONText(message, { section }), warnings: [] }
  } catch (err) {
    return failed(err)
  }
}

export type Unmarshaled = {
  rows: unknown
  /** Which path produced `rows` — `materialize` skips JSON text entirely
   * (PACKAGE_PLAN.md §5); `json` is the fallback for anything that is not a
   * one-field envelope around a table. */
  path: Path
}

/** A colbin message to real objects, and which of the two paths ran. */
export async function unmarshal(
  message: Uint8Array,
  section?: Uint8Array,
): Promise<Outcome<Unmarshaled>> {
  try {
    const codec = await handle()
    const rows = codec.unmarshal(message, { section })
    return { ok: true, value: { rows, path: codec.lastPath }, warnings: [] }
  } catch (err) {
    return failed(err)
  }
}

/** The field tree of a message, with every node's byte span. */
export async function inspect(
  message: Uint8Array,
  section?: Uint8Array,
): Promise<Outcome<Report>> {
  try {
    const codec = await handle()
    return { ok: true, value: codec.inspect(message, { section }), warnings: [] }
  } catch (err) {
    return failed(err)
  }
}
