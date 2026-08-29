# colbin (Rust)

A decoder for the [colbin](https://github.com/ivanjoz/colbin) binary format. The
Go codecs in the parent directory are the specification; this crate is a port of
them, pinned by a corpus they generate.

```toml
colbin = { git = "https://github.com/ivanjoz/colbin", rev = "<sha>" }
```

Pinned by `rev` rather than `tag`: the repository's tags are the Go module's
version ladder, so a Cargo tag requirement would either entangle the two release
cadences or resolve to a tag that predates this crate.

## Reading a message

Neither binary mode is self-describing, so the caller supplies the field layout.
`Schema::from_go` takes the Go struct's field names **in declaration order** —
that order is what decides the ids, because colbin hashes each name and linear
probes past the slots already taken.

```rust
use colbin::{Kind, Schema};

let schema = Schema::from_go(&[
    ("CompanyID", Kind::Int32),
    ("ID", Kind::Int32),
    ("Created", Kind::Int32),
    ("Hash", Kind::Uint64),
    ("User", Kind::String),
])?;

let record = colbin::decode_one(message, &schema)?;
let user = record.str(schema.fields()[4].id);
```

`Schema::from_fields` is the same thing for a struct that pins ids with `cb:"N"`,
and `Schema::from_ids` skips the names entirely when the ids are already known.

A record is sparse: compact mode omits a field holding its zero value, since the
key run is the presence information. `Record::get` reports that as `None`; the
typed readers (`i64`, `u64`, `f64`, `bool`, `str`) return the zero value instead,
which is what the Go decoder writes into the destination struct.

## Scope

Reads: both wire modes, hashed and explicit field ids, and the value kinds
compact mode itself carries — bool, every integer width, `f32`/`f64`, strings,
byte blobs, and slices of primitives.

Does not read: nested structs, maps, `interface{}` columns, the self-describing
messages `MarshalJSON` writes, or standard mode's *value* layout (which carries
no marker distinguishing it from the records layout). Each is refused with an
error naming why, rather than half-decoded.

There is no encoder. Nothing in Rust writes colbin, and an unused encoder would
be a second copy of the specification to keep honest for free.

Strings must be valid UTF-8. `packed5` is byte exact and the Go codec permits a
string that is not; a Rust `String` cannot be, so such a frame is an error.

## Tests

```sh
cargo test                 # against the committed corpus
go run ./rust/vectors      # regenerate it from the Go codecs (repo root)
```

Nothing in the corpus is hand-written: the messages come from `colbin.Marshal`,
the field ids out of a schema section colbin wrote, and the expected values back
out of the Go decoder. CI regenerates it and fails on a diff.
