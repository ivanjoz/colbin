# Performance

colbin 0.9.0 against MessagePack and vtprotobuf on AWS Lambda x86_64, on the
four official datasets, and against eleven formats on one dataset on a
laptop. This is a summary: the full per-case tables and the raw output are in
the `colbin-benchmarks` repository (`lambda/RESULTS.md`, `lambda/results/`). The
numbers compare formats with each other; they are not the paper's matrix.

A ratio is the other format's time (or size) over colbin's, so above 1× colbin
is faster (or smaller) and below it loses. Only the `asm` build counts:
colbinbench built normally, no `GOEXPERIMENT`, no `purego`.

## The run

| | |
|---|---|
| Date | 2026-10-27 |
| Where | AWS Lambda x86_64, us-east-1: `provided.al2023`, 4096 MB, Intel Xeon @ 2.50GHz, one function per dataset, one CPU for the whole run |
| Method | `colbinbench -worker`, one process per case, `GOMAXPROCS=1`, 5 samples of at least 100 ms each, median |
| colbin | 0.9.0 (`bedc044`), through `go.work` |
| Go | go1.27.0 |

## Summary, per dataset

Geometric mean over the cases each pair has in common.

| dataset | cases | encode vs msgpack / vtprotobuf | decode-fresh vs msgpack / vtprotobuf | decode-reuse vs msgpack / vtprotobuf | size vs msgpack / vtprotobuf |
|---|--:|--:|--:|--:|--:|
| TPC-H SF 0.1 | 10 | 1.15× / 1.80× | 1.73× / 1.99× | 1.60× / 1.98× | 1.94× / 1.16× |
| TPC-DS SF 0.1 | 24 | 1.01× / 1.57× | 1.56× / 1.69× | 1.05× / 1.76× | 4.22× / 1.56× |
| Public BI | 46 | 1.00× / 1.26× | 1.70× / 1.48× | 1.53× / 1.79× | 3.91× / 1.39× |
| SchemaStore | 54 | 0.79× / 0.88× | 0.91× / 1.07× | 1.00× / 1.31× | 1.62× / 1.10× |

- **Tables (TPC-H, TPC-DS, Public BI):** colbin is the fastest decode against
  both formats and the smallest. Encode is level with msgpack (1.00–1.15×) and
  faster than vtprotobuf (1.26–1.80×).
- **Small documents (SchemaStore):** colbin loses encode to both formats and
  decode-fresh to msgpack. colbin has no result for 6 of the 54 cases
  (`geojson`, `gruntcontribclean`, `nightwatch`, `openweatherroadrisk`,
  `packagejsonlintrc`, `tslintmulti`), and vtprotobuf has none for the
  `dynamic` cases; they are left out of the means.
- **Size:** colbin is the smallest on every dataset, 1.6–4.2× under msgpack
  and 1.1–1.6× under vtprotobuf.

## TPC-H SF 0.1, per case

`nested` is each order with its lines inside it; `normalized` is all eight
tables in one message; the other eight are one table each.

| case | op | colbin | vs msgpack | vs vtprotobuf |
|---|---|--:|--:|--:|
| nested | encode | 80.1 ms | 1.18× | 1.73× |
| | decode-fresh | 105.6 ms | 2.44× | 2.32× |
| | decode-reuse | 110.1 ms | 2.13× | 2.23× |
| normalized | encode | 70.7 ms | 1.38× | 2.11× |
| | decode-fresh | 125.9 ms | 2.17× | 2.16× |
| | decode-reuse | 134.1 ms | 1.91× | 1.91× |
| orders | encode | 9.6 ms | 1.32× | 1.95× |
| | decode-fresh | 18.7 ms | 2.04× | 2.35× |
| | decode-reuse | 19.0 ms | 1.88× | 2.16× |
| lineitem | encode | 55.0 ms | 1.38× | 2.17× |
| | decode-fresh | 102.9 ms | 2.05× | 1.85× |
| | decode-reuse | 103.1 ms | 1.93× | 1.84× |
| customer | encode | 894.6 µs | 1.35× | 1.98× |
| | decode-fresh | 3.8 ms | 1.55× | 1.74× |
| | decode-reuse | 3.3 ms | 1.82× | 1.95× |
| part | encode | 1.3 ms | 1.38× | 2.05× |
| | decode-fresh | 4.8 ms | 1.96× | 2.35× |
| | decode-reuse | 4.8 ms | 1.33× | 2.18× |
| partsupp | encode | 2.7 ms | 1.56× | 2.32× |
| | decode-fresh | 8.5 ms | 1.34× | 1.78× |
| | decode-reuse | 7.0 ms | 1.55× | 2.11× |
| supplier | encode | 46.6 µs | 1.34× | 2.15× |
| | decode-fresh | 205.8 µs | 1.85× | 2.16× |
| | decode-reuse | 218.5 µs | 1.62× | 2.02× |
| nation | encode | 1.1 µs | **0.68×** | 1.27× |
| | decode-fresh | 3.6 µs | 1.42× | 1.89× |
| | decode-reuse | 3.8 µs | 1.23× | 1.77× |
| region | encode | 302 ns | **0.54×** | **0.90×** |
| | decode-fresh | 977 ns | 1.03× | 1.50× |
| | decode-reuse | 801 ns | 1.02× | 1.73× |

| case | colbin | msgpack | vtprotobuf |
|---|--:|--:|--:|
| nested | 50,287,763 B | 162,291,941 B | 65,356,713 B |
| normalized | 65,539,427 B | 192,500,486 B | 84,468,527 B |
| orders | 12,628,985 B | 27,987,447 B | 14,678,282 B |
| lineitem | 38,068,699 B | 142,797,489 B | 53,517,301 B |
| customer | 2,227,935 B | 3,301,879 B | 2,426,154 B |
| part | 2,091,567 B | 3,539,513 B | 2,392,716 B |
| partsupp | 10,391,904 B | 14,682,502 B | 11,311,721 B |
| supplier | 127,818 B | 188,112 B | 139,677 B |
| nation | 2,125 B | 3,017 B | 2,274 B |
| region | 394 B | 509 B | 402 B |

The tables lose only at the bottom: `nation` (25 rows) and `region` (5 rows),
where a message is a few hundred bytes and a fixed cost per message is most of
the time.

## Against eleven formats, TPC-DS on a laptop

All 24 TPC-DS tables, 1,176,240 records in total. Time is per record: the
time of all 24 tables over their records, so the big tables weigh in
proportion to their rows. Size is the sum of the 24 encoded tables. Run on an
Intel Core i7-1355U, pinned to one performance core, one case at a time, 5
samples. The laptop was not idle (load average about 3.6), so these are for
comparing formats with each other, not for reporting.

| format | encode | decode-fresh | decode-reuse | size |
|---|--:|--:|--:|--:|
| colbin | 63 ns | 97 ns | 89 ns | 43,502,096 B |
| json-v2 | 1.06 µs | 1.79 µs | 1.72 µs | 389,945,271 B |
| cbor | 518 ns | 1.34 µs | 1.00 µs | 305,227,705 B |
| msgpack (generated) | 76 ns | 249 ns | 152 ns | 307,847,786 B |
| msgpack-reflect | 688 ns | 1.36 µs | 1.01 µs | 414,028,468 B |
| protobuf | 228 ns | 299 ns | 285 ns | 85,319,465 B |
| vtprotobuf | 66 ns | 226 ns | 217 ns | 85,319,465 B |
| flatbuffers | 36.98 µs ¹ | 298 ns | 266 ns | 196,044,728 B |
| arrow | 185 ns | 145 ns | 119 ns | 171,573,504 B |
| avro | 152 ns | 868 ns | 578 ns | 70,506,393 B |
| capnproto | 371 ns | 494 ns | 471 ns | 198,099,032 B |
| gob ² (17 tables) | 224 ns | 316 ns | 216 ns | 25,753,384 B |
| colbin, same 17 tables | 32 ns | 93 ns | 81 ns | 15,226,864 B |

¹ One table, `store_sales`, takes 42.8 s to encode in flatbuffers (colbin takes
  27 ms), and `catalog_sales` takes 0.42 s; together they are almost all of the
  36.98 µs. Without `store_sales` it is 748 ns per record, and without both,
  322 ns. I did not look into why the Go FlatBuffers builder is that slow on
  them.

² `gob` does not round-trip 7 tables (it drops pointers to a zero value), so it
  was timed on the other 17, which leave out the largest ones. It is not
  comparable with the rows above, only with the last row.

- Avro is the closest in size to colbin, and msgpack and vtprotobuf, the two
  with generated code, are the closest in encode time.
- JSON is encoding/json/v2, Avro is hamba/avro with the schema derived from the
  Go type, Cap'n Proto is go-capnp with code generated from the same types.

## Read this before quoting these numbers

- One function instance per dataset and 5 samples: a difference under about 5%
  is inside the noise. Lambda does not promise which x86_64 CPU a function
  lands on; the CPU of every run is in its raw output.
- The Lambda run measures colbin, msgpack and vtprotobuf only. The other nine
  formats have been run on TPC-DS on the laptop and nowhere else yet.
- arm64 is not in this run.
- colbin-benchmarks was at `1a2c78f` plus uncommitted work: TPC-H's eight tables
  as cases, TPC-DS, the new formats and the regenerated code.
- The October 2026 optimisation log that this file used to be, with what was
  tried, kept and reverted, is in this file's git history.
