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

Round-trip tests for both formats and the full per-model payload report:

```sh
go test ./comparison -v
```

Only the payload report:

```sh
go test ./comparison -run TestPayloadSizeComparison -v -count=1
```

Paired speed and allocation benchmarks:

```sh
go test ./comparison -run '^$' -bench . -benchmem -count=5
```

Fixture generation happens before benchmark timers start. Payload metrics are
reported as `B/payload`; each operation processes 64 values of every domain type
(`1344 models/op`). The generated Go bindings and Protobuf schema are checked in,
so none of the commands above require `protoc`.

## Development snapshot

Medians of a local `-count=5` run on an i7-1355U with Go 1.27:

| operation | format | payload | time/op | bytes allocated/op | allocations/op |
|---|---|---:|---:|---:|---:|
| encode | Colbin | 226,559 B | 4.08 ms | 1,670,068 B | 4,769 |
| encode | Protobuf | 324,822 B | 1.32 ms | 454,658 B | 8,929 |
| encode | JSON v2 | 740,373 B | 2.63 ms | 777,472 B | 2,233 |
| encode | CBOR | 563,866 B | 1.38 ms | 566,688 B | 3 |
| decode | Colbin | 226,559 B | 2.50 ms | 2,207,309 B | 27,354 |
| decode | Protobuf | 324,822 B | 2.08 ms | 1,501,765 B | 39,288 |
| decode | JSON v2 | 740,373 B | 4.85 ms | 1,407,758 B | 30,000 |
| decode | CBOR | 563,866 B | 3.82 ms | 1,432,629 B | 34,147 |

For this fixture, Colbin produced the smallest payload, Protobuf and CBOR
encoded fastest (their sample ranges overlap), and Protobuf decoded fastest.
Treat this as a development snapshot, not a universal result; use the commands
above on the target machine and data.

## Regenerate the bindings

Install the standalone `protoc` compiler, then install the pinned Go plugin:

```sh
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
go generate ./comparison
```

The checked-in `examples.pb.go` was generated with `protoc` 34.1 and
`protoc-gen-go` 1.36.12. Commit `examples.proto` and the regenerated
`examples.pb.go` together.
