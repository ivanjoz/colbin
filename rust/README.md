# colbin (Rust)

The [colbin](https://github.com/ivanjoz/colbin) binary format in Rust: a
byte-aligned `[key][descriptor][payload]` wire, a blocked column codec, and the
packed5 string codec. The Go packages in the parent directory are the
specification; this crate is a port of them, pinned to them by a corpus they
generate.

```toml
colbin = { git = "https://github.com/ivanjoz/colbin", rev = "<sha>", features = ["derive"] }
```

Pinned by `rev` rather than `tag`: the repository's tags are the Go module's
version ladder, so a Cargo tag requirement would either entangle the two release
cadences or resolve to a tag that predates this crate.

## Reading and writing a record

```rust
use colbin::Colbin;

#[derive(Colbin, Debug, PartialEq)]
struct Charge {
    #[cb(0)] company_id: u32,
    #[cb(1)] user_id: u32,
    #[cb(2)] note: String,
}

let charge = Charge { company_id: 7, user_id: 42, note: "ok".into() };
let message = charge.encode();          // 0xD0, then the fields
let back = Charge::decode(&message)?;
assert_eq!(back, charge);
```

`#[derive(Colbin)]` generates the straight-line encode and decode a hand-written
codec would be: a writer call per field with the key as a literal, and a `match`
on a literal key per decode arm. There is no reflective path to fall back to and
no `Value` tree in the middle.

### Field ids

The ids are what the two sides have to agree on, so they are assigned by exactly
the rule Go's `codec` uses:

```rust
#[cb(5)]                    // explicit wire id, as Go's `cb:"5"`
#[cb(name = "CompanyID")]   // the name colbin hashes, when Rust's differs
#[cb(name = "qty", 5)]      // both, as Go's `cb:"qty,5"`
#[cb(skip)]                 // not encoded, as Go's `cb:"-"`
```

A field without a number takes `fnv8` of its name and then the next free id
upward, with explicit ids reserved first so a hash cannot squat on a number
somebody asked for. The hash is over the *Go* name, so a `snake_case` Rust field
mirroring a Go one needs `#[cb(name = "...")]` — or an explicit id, which
sidesteps the question.

### Key width

Four-bit keys are the default and the fast path. A type goes wide when it has to:

- an id above fifteen, which four key bits cannot carry; or
- any id derived from a name, which lands anywhere in 0..=255.

That is the same rule `codec/wide.go` applies, so the two sides agree without
being told. `#[cb(wide)]` on the struct asks for it anyway, which is what a wire
that has to evolve wants: a wide reader **steps over a field it has never heard
of**, and a narrow one cannot.

```rust
#[derive(Colbin)]
#[cb(wide)]      // eight-bit keys even where four would fit
struct Evolving { /* ... */ }

#[derive(Colbin)]
#[cb(packed5)]   // pack string fields (implies wide)
struct Labels { /* ... */ }
```

## What it carries

| | |
|---|---|
| scalars | `bool`, `i8`–`i64`, `u8`–`u64`, `f32`, `f64`, `String` |
| blobs | `Vec<u8>` |
| slices | `Vec<i8..i64>`, `Vec<u16..u64>`, `Vec<String>` |
| optional | `Option<T>` for any scalar or `String` |
| nested | a field whose type also derives `Colbin` |
| lists and tables | `Vec<T>` where `T: Colbin`, row-wise or transposed |
| maps | `HashMap` and `BTreeMap` with scalar or `String` keys and values |

`isize` and `usize` are refused: their width is platform dependent and never
reaches the wire, so a message written on one host would decode differently on
another.

A `Vec<T: Colbin>` is written as a list of key runs below
`colbin::TABLE_THRESHOLD` rows and transposed into keyed columns at or above it,
which is the decision the Go encoder makes at the same threshold. Both are
self-describing — a `LIST` and a `TABLE` are different descriptor classes — so
the reader dispatches on what it finds.

## Driving the wire directly

A caller that would rather not derive can drive the writers and readers, which is
what the generated code does:

```rust
use colbin::wire::{Reader, Writer};

let mut buf = Vec::new();
let mut w = Writer::new(&mut buf);
w.u32(0, company_id);
w.string(1, &name);

let mut r = Reader::new(&buf);
while r.more() {
    match r.key() {
        0 => company_id = r.u32(),
        1 => name = r.string(),
        _ => r.skip(),   // refuses: a narrow key cannot be stepped over
    }
}
r.err()?;
```

Three framings of a key run are available, as separate types rather than a flag
— a key width the compiler cannot see is a width it cannot fold:

| | type | key | skip | fields |
|---|---|---|---|---|
| narrow | `Writer` / `Reader` | 4 bits, shares the descriptor byte | no | 16 |
| wide | `Writer8` / `Reader8` | 8 bits, own byte | **yes** | 256 |
| bitmap | `BitmapWriter` / `BitmapReader` | a presence bitmap, no key at all | yes | 64 |

`colbin::column` is the blocked column codec on its own, and `colbin::packed5`
the string codec: both are usable without the rest.

## Errors

A write cannot fail — neither writer has an error, because every size escalates
to a width that holds it and nothing a caller can hold in memory is too large to
describe. A read can, and its errors are sticky: the first failure parks the
cursor at the end of the message, every later read answers zero, and one check of
`err()` covers the whole record.

Strings must be valid UTF-8. The Go codec is byte exact and permits a string that
is not; a Rust `String` cannot be, so such a field is `Error::NotUtf8` rather
than a lossy replacement.

## Tests

```sh
cargo test -p colbin --features derive     # the port
go run ./rust/vectors                      # regenerate the corpus, from the repo root
go test ./rust/vectors                     # fail if the committed corpus is stale
```

`rust/vectors/vectors.json` is written by the Go codecs — every message by
`colbin.Marshal` on a real Go value, every field id out of `colbin.FieldIDs`,
every column by `column.AppendArray`. `rust/tests/vectors.rs` holds the same
values and asserts **both directions**: that this port decodes those bytes to
those values, and that it encodes those values to those bytes. So neither port
can move without the other failing.

The corpus covers every scalar at its extremes, every slice kind, the blob and
string-array size escapes, optionals against their explicit zeros, nested
structs, a row-wise list and a transposed table, maps, both key widths, a wide
reader skipping a field it does not know, derived ids, the packed5 encoding, and
fourteen column shapes across all four element widths.

Beyond it: `tests/wire.rs` drives all three framings directly, `tests/derive.rs`
covers the generated code, and both walk every truncation of a valid message and
a large space of arbitrary bytes, requiring an error rather than a panic. The
crate is `#![forbid(unsafe_code)]`.

## Status

Complete against the Go implementation for everything in the table above, and
pinned to it by the corpus. What is not here is what is not there either:
interfaces have no form on the wire yet.
