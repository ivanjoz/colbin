# colbin

A columnar binary format for the JSON a backend actually sends: lists of
records, all the same shape. The field names travel once, as a schema, instead
of once per record; the values travel as columns, bit-packed.

This package is the browser and Node client — a WebAssembly module compiled
from the Rust implementation, and a small wrapper that turns its output into
JavaScript objects **without JSON text in the middle**.

```
npm install colbin
```

## Quickstart

```js
import { Codec } from 'colbin'

const codec = await Codec.open()           // compiles the module, once

const response = await fetch('/api/products')
const rows = codec.unmarshal(new Uint8Array(await response.arrayBuffer()))
```

`Codec.open()` is async because compiling WebAssembly is. Everything after it
is synchronous: `unmarshal`, `marshal`, `columns` and the rest are straight
runs of wasm calls with no `await` in them.

## The schema travels out of band

The delivery the format is built for sends the schema **once per connection**
and every message after it carries only its body:

```js
const codec = await Codec.open()
codec.setSchema(section)                    // sent once, by whatever channel

const rows = codec.unmarshal(message)       // every message after it
```

A message can also carry its own schema, for a file or a one-off response —
that is what `marshal`'s `standalone` is, and `unmarshal` reads it with no
`setSchema` call at all.

## Writing

```js
const { message, section, standalone, warnings } = codec.marshal(rows)
```

`marshal` takes a value or JSON text. A string is handed to the encoder as it
stands, which is the only way to carry an integer past 2^53 in: by the time it
is a JavaScript `Number`, it has already been rounded.

`standalone` is the same document with its section in front of it. It is
composed on first read, not encoded a second time.

Pass `{ verify: true }` to decode the message back and walk it against the
input before it is returned. It costs about 65% on top of the encode and what
it catches is a codec bug rather than a caller's mistake — worth having in a
test suite, and in the first deployment of anything whose shapes are new.

## The API

| | |
|---|---|
| `Codec.open(opts?)` | compile + instantiate. `opts.module` takes a pre-compiled `WebAssembly.Module`, `opts.wasm` takes bytes, a `Response`, or a URL |
| `Codec.fromModule(module)` | a second handle over a module compiled already — an instance, no compile |
| `Codec.preload()` | start the compile without waiting for it |
| `codec.unmarshal(bytes, opts?)` | **the headline** — objects, no JSON text when the shape allows it |
| `codec.columns(bytes, opts?)` | the columns themselves, for a grid or a chart that never wants row objects. `null` when the message is not that shape |
| `codec.toJSONText(bytes, opts?)` | the text path, for every shape |
| `codec.marshal(value \| jsonText, opts?)` | `{ message, section, standalone, warnings }` |
| `codec.setSchema(section \| null)` | hold a section for the messages that follow; `null` clears it |
| `codec.inspect(bytes, opts?)` | the field tree, with a byte span on every node. Needs `colbin/inspect` — see below |
| `codec.lastPath` | `'materialize'` or `'json'` — which path the last `unmarshal` took |
| `codec.canInspect` | whether this handle's module has the span walk in it |
| `unmarshal` / `columns` / `toJSONText` / `marshal` / `inspect` | the same, without holding a handle, each `await`ed |
| `ColbinError` | `code`, `offset`, `line`, `path`, `message`, `warnings` |

Every call that can fail throws a `ColbinError` carrying the module's whole
diagnostic — the byte offset, the line for a failure in JSON text, and the path
through the document. A *successful* `marshal` can still have something to say
(an always-null column, a null where an object belongs), so warnings ride on
the result rather than being thrown.

## Two paths, and which one you got

When the message is a table — a list of records, which is the shape this format
is for — `unmarshal` never writes JSON text. The module hands over columns in a
flat typed buffer and the objects are built in JavaScript from a generated,
monomorphic row builder cached per schema.

Anything else (a nested document, a heterogeneous list, a bare object) falls
back to the JSON-text path automatically. `codec.lastPath` says which ran, so a
caller can find out they are on the slower one without guessing.

The fast path is also the one that is **exact past 2^53**: a column whose values
do not fit a `Number` comes back as `bigint`, per column, using range
information the column header already carries. The JSON fallback goes through
`JSON.parse` and rounds like any other JSON reader would.

Under a Content-Security-Policy without `unsafe-eval`, the generated row builder
cannot be compiled. The package detects that once and falls back to a generic
builder, which costs about ten times as much as the generated one and is still
two orders of magnitude below the decode.

## Numbers

1000 product records, seven fields. Node 24 / V8, best of twelve runs, no
network. `bun run bench` in this directory reproduces it.

| | wire | gzipped |
|---|---:|---:|
| minified JSON | 106 670 B | 12 124 B |
| colbin — 30 411 B of message, 64 B of section | **30 475 B** | **3 250 B** |

The section is the 64 bytes, and it is sent once per connection rather than
once per message.

| | |
|---|---:|
| `codec.unmarshal` — objects in hand, no JSON text | **0.14 ms** |
| `JSON.parse` of the same document, as a reference point | 0.26 ms |
| `codec.toJSONText` | 0.20 ms |
| `JSON.stringify` of the same document | 0.15 ms |
| `codec.marshal` — parse, infer, build | 1.15 ms |
| `codec.marshal` with `{ verify: true }` | 1.88 ms |

The reference rows are there to calibrate the others, not as a bar the design
had to clear. What the project optimises for is backend CPU, backend RAM and
transfer size; client CPU is budget to spend in service of those.

## Entry points

| | |
|---|---|
| `colbin` | the module inline as base64. No bundler configuration, no runtime 404 mode |
| `colbin` on Node | the `node` condition: reads `colbin.wasm` from beside the JavaScript |
| `colbin/asset` | `new URL('./colbin.wasm', import.meta.url)` — Vite, webpack 5, Rollup and Parcel all emit it as an asset, and it works unbundled in a browser too |
| `colbin/inspect` | the same, over the **other module**: the one built with `inspect`. See below |
| `colbin/colbin.wasm` | the module file itself, for a build step that wants to place it |

Measured, on the modules this version ships:

| | raw | gzipped |
|---|---:|---:|
| `colbin.wasm` — what `colbin/asset` serves | 209 109 B | **84 195 B** |
| the same module as inline base64 | 279 124 B | **114 271 B** |
| `colbin.inspect.wasm` — what `colbin/inspect` serves | 224 199 B | 90 127 B |
| the wrapper's own JavaScript, all entries | 36 085 B | 10 651 B |

Inline costs **36% more over the wire** than the asset, gzipped. Base64 breaks
the byte alignment gzip's matcher works on, so gzip recovers much less of the
33% expansion than the raw ratio suggests. Inline is still the default because
it needs no configuration and cannot 404; a page that cares about the transfer
should import `colbin/asset`, or hand `Codec.open` its own source:

```js
const codec = await Codec.open({ wasm: fetch('/colbin.wasm') })
```

### `inspect` is a separate module

`codec.inspect()` — the field tree with a byte span on every node — is the one
thing here that only a tool wants: something that *draws* the bytes rather than
reads them. It is 15 KB of module, and it used to be in the module everybody
downloaded.

So it is not. `colbin` and `colbin/asset` carry the build without it, and
`colbin/inspect` carries the build with it:

```js
import { Codec } from 'colbin/inspect'

const codec = await Codec.open()
const report = codec.inspect(message, { section })   // fields, with byte spans
```

`codec.canInspect` says which build a handle is over, and calling `inspect()`
on one that cannot throws an error naming the entry that can, rather than
failing as `inspect_message is not a function` several frames away.

Both modules go through `wasm-opt -O3`, which is worth 12% of the raw bytes and
4% gzipped over what `cargo` emits, and costs nothing measurable in throughput.

## Version pinning

> colbin 0.x makes no wire-compatibility promise across minor versions. A Go
> service on 0.3 and a browser on 0.2 will not interoperate, and the failure
> will look like corrupt data rather than a version error. Pin both.

One tag drives all three implementations: `v0.1.0` publishes `colbin@0.1.0`, and
is the Go module tag and the Rust crate version. Compatibility while on `0.x` is
by matching **minor** version.

## What this does not do

No streaming — the ABI is bytes in, bytes out, and allocates the whole input.
No worker wrapper: decoding off the main thread is your choice, and shipping one
would mean owning a message protocol. No types generated from the schema. The
column fast path covers tables; deeply nested or heterogeneous documents go
through the JSON-text path, which is correct but slower.

MIT. Source, format notes and the Go and Rust implementations:
<https://github.com/ivanjoz/colbin>.
