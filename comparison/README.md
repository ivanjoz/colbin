# Colbin vs Protobuf, JSON v2, and CBOR

This package provides a reproducible comparison corpus with 21 domain models:
people and addresses, companies, products and orders, invoices, sensor and
weather data, routes, posts and comments, game data, metrics and logs, and
investment portfolios.

The schema deliberately mixes:

- signed and unsigned integers, booleans, 32/64-bit floats, strings, and bytes;
- repeated scalar and string fields;
- repeated and nullable nested messages;
- maps with string, integer, and string values;
- shallow records and deeper object graphs.

`GenerateCorpus` uses a locally seeded
[`gofakeit/v7`](https://github.com/brianvoe/gofakeit) instance for realistic
names, companies, addresses, emails, product names, and prose. Numeric values,
IDs, and timestamps use a second local seeded generator. No global random state
or network service is involved, so all serializers always receive identical
input.

The comparison uses the native Go 1.27 `encoding/json/v2` package and
`github.com/fxamacker/cbor/v2` with its default encoding options. Protobuf uses
`google.golang.org/protobuf/proto`. No compression is layered over any format.

## Run the comparison

Round-trip tests for every format and the full per-model payload report:

```sh
go test ./comparison -v
```

Only the payload report:

```sh
go test ./comparison -run TestPayloadSizeComparison -v -count=1
```

Paired speed and allocation benchmarks, including Colbin's JSON mode:

```sh
go test ./comparison -run '^$' -bench . -benchmem -count=5
```

The JSON-mode sub-benchmarks are `ColbinJSONText` (`MarshalJSON` / `DecodeJSON`)
and `ColbinJSONValues` (`MarshalJSON` / `DecodeAny`). Their encode side is the
same call measured twice, on purpose — see [why every row runs in its own
process](#why-every-row-runs-in-its-own-process). For figures worth comparing,
run one row per process:

```sh
go test ./comparison -run '^$' -bench '^BenchmarkEncode$/^Colbin$' -benchmem -count=6
```

Fixture generation happens before benchmark timers start. Payload metrics are
reported as `B/payload`; each operation processes 64 values of every domain type
(`1344 models/op`). The generated Go bindings and Protobuf schema are checked in,
so none of the commands above require `protoc`.

## Development snapshot

Medians of `-count=6` on an i7-1355U with Go 1.27, **each row measured in its own
process from a cold start** (package under 62 °C). `Colbin/JSON` is the
self-describing mode: `MarshalJSON` on the encode side, and on the decode side
either `DecodeAny` (Go values) or `DecodeJSON` (JSON text), neither of which needs
the corpus type.

| operation | format | payload | time/op | bytes allocated/op | allocations/op |
|---|---|---:|---:|---:|---:|
| encode | Colbin | 226,559 B | 2.09 ms | 611,144 B | 265 |
| encode | Colbin/JSON | 227,998 B | 2.13 ms | 612,377 B | 266 |
| encode | Protobuf | 324,822 B | 1.34 ms | 454,659 B | 8,929 |
| encode | JSON v2 | 740,373 B | 2.53 ms | 777,515 B | 2,233 |
| encode | CBOR | 563,866 B | 1.17 ms | 566,864 B | 3 |
| decode | Colbin | 226,559 B | 2.23 ms | 2,403,516 B | 12,298 |
| decode | Colbin/JSON → Go values | 227,998 B | 3.30 ms | 4,567,638 B | 51,680 |
| decode | Colbin/JSON → JSON text | 227,998 B | 8.43 ms | 8,390,764 B | 68,542 |
| decode | Protobuf | 324,822 B | 1.98 ms | 1,501,768 B | 39,288 |
| decode | JSON v2 | 740,373 B | 4.49 ms | 1,407,868 B | 30,000 |
| decode | CBOR | 563,866 B | 3.53 ms | 1,432,676 B | 34,147 |

For this fixture, Colbin produced the smallest payload and allocated by far the
least often on both sides; CBOR encoded fastest and Protobuf decoded fastest.
Treat this as a development snapshot, not a universal result; use the commands
above on the target machine and data.

### Why every row runs in its own process

One `go test -bench .` over the whole table distorts it twice over, in opposite
directions, which makes small differences unreadable rather than merely noisy:

- **Position.** An allocation-heavy benchmark that runs early pays to grow the
  heap that every later row then inherits. Measured inside the shared process,
  `encode/Colbin` — the first row — came out at 2.30 ms against 2.09 ms alone: a
  9% penalty from nothing but context.
- **Heat.** This laptop throttles under sustained load, so later rows run slower.
  A sequential run charges them 6–11% for the heat the earlier rows produced.

`BenchmarkEncode` measures `MarshalJSON` twice, once under each JSON-mode row.
That duplication is deliberate: the two are the *same call*, so the gap between
them is this benchmark's noise floor, measured on the spot. Here they landed
2.071 ms and 2.167 ms — **4.6% apart** — which brackets plain Colbin's 2.09 ms.
Any encode difference smaller than that is not a result.

### What the JSON mode costs

Embedding the schema costs **1,439 B on this corpus — 0.6%** — and it describes
the *type*, so it is the same 1,439 B whether the message holds 64 records per
model or 64,000. That still leaves it the smallest payload in the table after
plain Colbin.

**Encoding is indistinguishable from plain Colbin**, and has to be:
`MarshalJSON` is `Marshal` plus a cached schema lookup, one allocation, and a
1,439 B copy into a 226,559 B buffer — 0.6% more `memcpy`, well under the noise
floor above. It cannot be *faster*: the two share `appendMessage` and the JSON
path only ever adds work. If a run shows it winning, that is the measurement, not
the code.

The decoders are where the mode is genuinely paid for, and both are slower than
the typed `Unmarshal` for a structural reason: with no Go type to fill, every
record becomes a `map[string]any` of boxed values instead of a struct.

- **→ Go values** (`DecodeAny`): 1.5x the time and 4.2x the allocations of the
  typed decode. Still faster than decoding this corpus from JSON v2 or CBOR.
- **→ JSON text** (`DecodeJSON`): 3.8x the typed decode. It does the work above
  and then renders it, so it is the slowest path in the table. Reach for it when
  the alternative is *storing* JSON, not when a Go type is available.

The JSON it emits is 744,538 B against JSON v2's 740,373 B for the same corpus:
slightly larger because a columnar layout cannot honour `omitempty` — every record
carries a value for every field, so zero values are written out rather than
omitted. Per-model schema overhead is reported by
`go test ./comparison -run TestJSONModeSchemaOverhead -v -count=1`.

## Regenerate the bindings

Install the standalone `protoc` compiler, then install the pinned Go plugin:

```sh
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
go generate ./comparison
```

The checked-in `examples.pb.go` was generated with `protoc` 34.1 and
`protoc-gen-go` 1.36.12. Commit `examples.proto` and the regenerated
`examples.pb.go` together.
