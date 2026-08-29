## A Rust decoder in this repository, consumed over git

**Context** — `genix`'s `server_utils/src/bridge/token.rs` hand-wrote a colbin decoder in Rust:
577 lines mirroring `codec/format.go`, `compact/bitstream.go`, `codec/typeinfo.go` and the string
codec, to decode exactly one struct. It was pinned by vectors generated from Go and it was already
wrong — it targets `formatVersion 0x01`, whose integer column was a frame-of-reference base plus
fixed-width deltas and whose strings were raw UTF-8. Since then the integer column became the
`varint` array codec, strings became `packed5` frames, and every single-record message routes
through compact mode, so the token it was written to read now arrives as 27 bytes whose first byte
has bit 0 set. Rewriting it in place would leave the same arrangement standing: the format's rules
in one repository and a partial transcription of them in another, with nothing but a code review
between them.

**Decision** — A `rust/` crate in this repository, next to the codecs it ports and the vectors that
pin it, added as a Cargo workspace member under a root `Cargo.toml`. A decoder only. It carries the
value kinds compact mode itself admits — scalars, strings, byte blobs, slices of primitives, hashed
and `cb:"N"` ids — and refuses a nested struct, a map, an `interface{}` column or a `MarshalJSON`
message with an error naming why. `rust/vectors/main.go` generates the corpus with the real package
and `rust/tests/vectors.rs` asserts against it; CI regenerates and diffs. Consumers pin it by `rev`.

**Rationale** — Cargo does not need the crate at the repository root: it clones, reads the root
manifest and searches the workspace members for the package named, so a Go module and a Cargo
workspace coexist in one tree without either noticing the other. That is what lets the port live
beside its specification instead of downstream of it.

No encoder, because nothing in Rust writes colbin — an encoder would be a second specification to
keep honest for free, and the first thing to rot. The compact-mode surface rather than the full
standard-mode one, because compact mode is what a single-record message *is*, and it is the shape
every current Rust reader wants; the cost is that a Rust reader of an ORM blob with nested structs
or maps would need the crate widened first, which is a larger crate and a decision worth taking
when there is a caller for it rather than in advance.

`rev` rather than `tag`, because the tags here are the Go module's version ladder (`v0.1.0` is
`83d9655`, which predates this crate): a tag requirement would either entangle the two release
cadences or silently resolve to a tag with no crate in it.

The corpus is what makes this a port rather than a second guess. Every message comes from
`colbin.Marshal` on a real Go value, every field id is read out of a schema section colbin wrote,
and every expected value is read back by the Go decoder itself — the compact ones through a
`compact.Reader` driven by the same field kinds the Rust schema declares, the columnar ones through
`Unmarshal`, which also round-trips against the input. The generator refuses to write a corpus
missing a mode, a shape, a key width, a version byte or a value kind, and the two string-alignment
cases are *measured* by replaying the record through the real `compact.Writer` rather than asserted
from the bit arithmetic. What this costs is a Go toolchain in CI to regenerate, which was already
there.

One deliberate difference from the Go decoder: a string that is not valid UTF-8 is an error rather
than a value. `packed5` carries bytes and the Go codec is byte exact for input that is not valid
UTF-8; a Rust `String` cannot be, and `from_utf8_lossy` would silently substitute characters into
data a caller may be authenticating.
