## Minimal mode is a third mode, not a variant of compact

**Context** — Both existing modes spend 60-90 ns per record on framing: a plan
walk, an `ALL_POSITIVE` pre-scan, and a transform search across each array column.
That is the right trade when a record is one of hundreds and pure loss when it is
one record of eight scalars on a hot path. Measured against the hand-written fixed
layouts of a wire protocol, `Codec[T].Append` was 36x the encoder it would replace
and 20x the parser; a single-element `[]int32` field alone cost 815 ns, because the
integer array codec searches for a delta transform across a column of one. The
alternative on offer was to hand-write the frames, which is fast and puts every
byte offset in the programmer's head.

**Decision** — A third mode: byte-aligned `[key:4][what follows]`, one record, at
most sixteen explicitly numbered primitive fields, a zero-valued field omitted
entirely. `minimal/` owns the wire format with no reflection, like `compact/`;
`codec/minimal.go` holds the per-type plan and the `MarshalMinimal` façade;
`rust/src/minimal.rs` is the port, pinned to the Go side by a shared vector.

A ten-field record encodes in 5.8 ns through the writer straight-line, 17.6 ns
through `MinimalCodec[T]`, and 62 ns through `MarshalMinimal`, against compact
mode's 92 — for 11 bytes against 10. The layers are the point: the façade is what
a caller reaches for, the handle is what a hot path holds, and the writer is what
a generated codec emits.

**Rationale** — Three properties carry that number, and each costs something that
had to be accepted rather than engineered around.

*Byte alignment.* Nothing is packed across a byte boundary, so an encode is a
header byte and a few stores. Compact mode's bitstream is denser on a record full
of small values; minimal mode gives up those bits to keep the codec a switch.

*Width-typed entry points.* `Uint` takes a `uint64` and must therefore carry all
six size branches, which puts it past Go's inline budget. `U16` can only be one
byte or two, so it is two appends with no call underneath and it inlines — and
calling the writer that matches each field's static Go type took a ten-field
encode from 16.9 ns to 5.8 ns. This is why the plan resolves an op per field
rather than routing everything through the general path.

*No key check.* `if key >= 16` once per field measured 8 ns of a 25 ns encode.
It is gone, under a principle the package states out loud: the reader defends
against the network, the writer trusts its own program. A key is a constant of the
record definition, never data, so the cost of being wrong is a record nothing can
read back — and `minimal/README.md` prescribes a compile-time assertion where the
key constants live. The reader keeps every bound, and has two the writer does not
need: a size continuation run longer than nine bytes, or one describing more than
an `int` holds, is refused rather than wrapped into a small size whose bounds
check would then pass.

**Sizes are continued, not capped.** A header carries a size's low bits and a flag;
LEB128 bytes carry the rest. The common size costs nothing extra — 2047 bytes of
string, 255 array elements — and nothing above it is refused. That removed the last
way a write could fail, so `Writer` has no error at all, which is worth more than
the ceiling it replaced: a caller cannot forget to check what cannot happen.

**What the mode gives up**, and why each was the right thing to give up rather
than fix: it has no version byte, so `Unmarshal` cannot recognise a minimal
message and `UnmarshalMinimal` is the only way back — a version byte on an 11-byte
message is a 9% tax on the mode's entire reason to exist. An unknown key cannot be
skipped, because the header sizes a field without saying which of the four layouts
it is; size code 7 stays unassigned as the door to a self-describing variant if one
is ever wanted. And no composite of any kind: a nested struct would need a length
or a terminator, and the format has neither.

**Floats needed no shape of their own.** They ride the integer field carrying their
IEEE-754 bit pattern, because the reader already knows a field is a float — it
knows its type from the key. A pattern with zero high bytes simply costs fewer of
them, and positive zero is omitted like every other zero.

## Unmarshal panicked on a message it should have refused

**Context** — A minimal message's first byte is a field header, and for a record
whose first field is key 0 it can be `0x08` — which standard mode reads as the
JSON-with-omit-empty version byte. `header()` then stepped the cursor over a
schema section by a length it had not checked, and `recordCount` sliced from a
position past the end: a panic, not an error. Nothing about this was specific to
minimal mode; any truncated or foreign input with a plausible first byte did it,
and a decoder reads from files and sockets.

**Decision** — `header()` refuses an empty message, and checks the schema
section's declared length against what is actually left before moving the cursor;
`recordCount` refuses a cursor already past the end. A test walks a list of
garbage and truncated messages, including every prefix of a real one, and requires
an error and no panic from each.

**Rationale** — A length in a message is an instruction from somewhere else, and
acting on one before checking it is how a decoder reads memory it was never given.
The same rule is already what the minimal reader is built around; this is the
standard decoder catching up with it. The fix is two bounds checks on a path that
runs once per message, so it costs nothing measurable.

## `#[derive(Colbin)]`, and the dependency it is worth

**Context** — After the `Codec` handle, Rust encode was ahead of Go and Rust decode was still ~46 ns
behind on a seven-field record. The remaining cost was not the codec: the dynamic path builds a
`Record`, a `Vec<(u8, Value)>` with a 32-byte `Value` per field, so a decode allocates a vector and
moves forty bytes per field before the caller has read anything. Go writes into a struct and does
none of that. A `Value` tree is the right shape when the layout is only known at runtime, and pure
overhead when it is known at compile time.

**Decision** — A `Colbin` trait and a `TypedCodec<T>`, with `#[derive(Colbin)]` generating the two
dispatches: a `match` on the field's declaration position whose arms assign to the field, and a write
that tests each field for zero. No `Value` exists on that path.

Measured, same subject: **decode 190 → 103 ns/op** (1.85x, and 1.45x faster than Go's ~150),
**encode 65 → 46 ns/op** (2.6x faster than Go's ~120).

Four calls inside it worth recording:

- **`syn`/`quote`, behind an optional `derive` feature.** The crate's manifest says it has no
  dependencies on purpose. A procedural macro has to be its own crate, and hand-rolling the token
  parsing to avoid `syn` would be a fragile parser for no gain. Off by default, so the promise still
  holds for anyone who does not opt in — and the typed path itself (`Colbin`, `TypedCodec`, the field
  reader and writer) is *not* gated, so a hand-written impl needs no proc-macro toolchain at all.
- **The macro does not compute field ids.** It emits the field *names* and lets
  `Schema::from_fields` assign ids at runtime, exactly as every other caller does, and the generated
  `match` dispatches on declaration index. Hashing the names in the macro so the match could be on
  the id itself would have put a third copy of the id algorithm in the tree, after Go's and the Rust
  runtime's, to save one array lookup per field.
- **The value readers and writers are shared, not regenerated.** Both paths were refactored to call
  the same `Reader::bools`/`Writer::strings`/... methods, which is what keeps the typed path from
  drifting: `writes_what_the_dynamic_path_writes` compares the two encoders on nine corpus values and
  they cannot diverge in the value codecs because there is one copy of each.
- **The flat forms only.** A nested struct, a slice of structs or a map needs the schema of the type
  one level down at the point of each read; the flat forms do not, and the dynamic `Codec` carries
  composites already. Extending the derive means a precomputed tree of id-to-index tables, which is
  a design worth reviewing rather than one to invent unsupervised.

**Rationale** — The typed path is only an optimisation, so what the tests pin first is that it *is*
only an optimisation: for every corpus value it must write what the dynamic path writes, byte for
byte, and against Go's own bytes wherever a string did not pack smaller than raw.

That caught a real bug. An unsigned slice is *reinterpreted* at its own width rather than widened —
Go hands the same bytes to the signed codec (`PutInts` over `(*int32)(data)`), so `4_000_000_000` as
a `u32` goes out as the `i32` with that bit pattern. The typed writer widened it, which changed the
value the array codec saw and the message with it. `i64::from(v as i32)` is the fix, and the only
reason it was found is that the test compares against messages Go actually wrote rather than against
the typed path's own output.

A skipped field (`#[cb(skip)]`, Go's `cb:"-"`) is *reset* by a decode rather than left alone, because
that is what Go does: `decodeCompact` calls `SetZero` on the destination struct, which does not spare
the fields it will not write.

## The Rust Codec handle, and three optimisations that were not the problem

**Context** — The Rust encoder and decoder were written for correctness against Go, and both redid on
every call work that is a property of the schema: `Schema::narrow_keys` walks the whole schema tree,
`composite_reaches_signed` walks it again, and every message grew a fresh `Vec` from zero. On a
seven-field record, measured: 134 ns to encode, of which **48 ns was `Vec` growth** — four
reallocations for a 17-byte message — and **12 ns was `narrow_keys`**. Decode was 254 ns, spending
23 ns on each record's field vector and another allocation on the `Vec<Record>` that `decode_one`
built only to take one record out of.

**Decision** — A `Codec` handle, which is Go's `Codec[T]` in Rust's terms: the schema's derived facts
resolved once, `append_one`/`append` writing into a buffer the caller owns, `decode_one_into` and
`decode_into` reusing a destination. It is immutable and `Send + Sync`; the only shared mutable state
is an `AtomicU32` size hint, which is a hint. `BitWriter` borrows `&mut Vec<u8>` rather than owning
one, packed5 frames and integer arrays are streamed straight through the bitstream instead of built
in scratch buffers, and `BitReader` gained an accumulator that refills once per eight bytes rather
than loading a window on every read.

Result: **encode 124 → 67 ns/op** (now faster than Go's `Codec.Append` at ~120 ns, which allocates
nothing — Rust's advantage is that streaming the frames removed two scratch buffers Go still pays
for), **decode 222 → 196 ns/op**.

**Rationale** — The wins came from allocation and from the reader's inner loop, not from where the
work looked like it was. Three things measured as dead ends, and they are recorded because each is
the kind of thing a later reader would "fix" back:

- **Porting Go's unaligned-load fast path made the reader slower.** `bitReader.chunk` in Go tests
  `index+8 <= len(buf)` and does one unaligned 8-byte load, falling back to a copy near the end. The
  same shape in Rust cost 40 ns on a fixed cost that was 19 ns, consistently across alternated runs.
  A compact message is small enough that the fast path rarely applies, and the fallback's
  `rest.len()` bound is not provable to the optimiser where `min(8)` is. Reverted; the comment in
  `chunk` says so, so nobody ports it again.
- **Collapsing four `match kind` into one exhaustive match changed nothing.** `read_value` walked a
  composite check, then `scalar_int`, then `slice_int`, then a final match — and a `bool` field, one
  bit on the wire, measured the same 34 ns as an `i32` while an `f64` measured less, which looked
  exactly like dispatch cost. LLVM was already merging them. The single match stayed, because it
  reads better, but it bought nothing and the 34 ns turned out to be `BitReader::get`.
- **Reusing the `aligned_tail` shift buffer removed seven allocations per record and did not move the
  benchmark.** Kept, since it mirrors Go and matters under allocator pressure this microbenchmark
  does not create, but it is not why strings are fast or slow.

What is left on decode is the API, not the codec: it builds an owned `Vec<(u8, Value)>` with a
32-byte `Value` per field, where Go writes into a typed struct. That is the remaining 46 ns against
Go, and closing it means a typed destination — a derive macro — which is a different change.

The handle deliberately does not own a scratch buffer, which would have forced `&mut self` and made
it unshareable. Streaming the frames removed the need for one, and the output buffer belongs to the
caller: `append_one` is the zero-allocation path precisely because the crate does not try to own the
memory. A pool would have been the other answer and is what Go uses, but Go has `sync.Pool` in its
standard library and this crate has no dependencies by choice.

## A Rust compact-mode encoder, exact everywhere but strings

**Context** — The entry below argued against a Rust encoder: nothing in Rust wrote colbin, so it would
be "a second copy of the specification to keep honest for free, and the first thing to rot". There is
now a caller — `fareward` and the Go backend exchanging messages over mirrored structs — so the
premise is gone, but the failure mode it named is not. Meanwhile the Rust `Schema`/`Kind`/`Value` were
flat and carried no composite in either wire mode, so the compact composites added above were
unreadable there.

Two size optimisers stood between a port and byte-exactness: `packed5/encode.go` (384 lines: a greedy
scan over case runs, digit runs, a symbol table and escapes, plus the exact cost of all four
header-flag settings computed per string) and `varint/array.go` (366 lines: four transforms scored
against 32 (k, d) pairs). Both have trivially valid alternatives, since Go decodes any well-formed
message.

**Decision** — `Kind`/`Value`/`Schema` became trees, the compact reader recursed, and a compact-mode
encoder went in: `encode`/`encode_one` over the same `Schema` and `Record`/`Value` the decoder
returns. The **integer array codec is ported in full** and is byte-exact. **Strings are not**: they go
out as raw `packed5` frames. Standard-mode composites stay unread.

Two decisions fell out of building it and are not obvious from the spec. `Kind::Array` of a bare
scalar is refused when a `Schema` is built, because no Go type produces it — a slice of scalars is its
own kind and rides a bulk codec — and an encoder that honoured it would write a message Go's decoder
misreads. And `all_positive` deliberately reproduces Go's *conservative* answer, giving up the pre-scan
whenever a signed integer sits inside a composite rather than recursing to find it.

**Rationale** — Raw strings are cheap in lines and the loss is small and bounded, because Go's encoder
already picks raw whenever raw is cheaper: `""`, `"a"`, `"uno"`, `"ok"` all go out raw from Go too, and
packing only wins from about six letters. Measured over the whole corpus that is 685 B against 654 B,
+4.7%, worst case +6 B on one record — and it is a CPU *win*, since the greedy scan and the four-way
flag plan are what is skipped. The array codec is the opposite case: it is what makes colbin small on
the payloads that are actually large, and porting it costs one function rather than a second
specification.

Being conservative about `ALL_POSITIVE` looks like leaving a bit on the floor, and being *more*
accurate would have been worse: it would produce a smaller message than Go's for the same value, which
is precisely the divergence a port exists not to have. The same reasoning is why the omit-zero rule
lives in the encoder rather than the caller — a record built by hand and one that came back from
`decode` have to encode to the same bytes.

Rotting is the real risk and it is what the tests are shaped around, because a decoder reading its own
output proves nothing. Three layers: Go writes and Rust reads (the corpus, as before); Rust writes and
Go reads (`rust/vectors/verify_test.go`, 30 messages, 16 of them in the string-divergent set nothing
else hands to Go); and byte equality against messages Go actually wrote, for the cases where the two
agree exactly. That last one needed the generator to say *which* cases those are, so it now records
`rustByteExact` per case — computed by asking `packed5` whether any string in the value packs, rather
than guessed at from the Rust side.

The composite corpus is a second file rather than more cases in `vectors.json` because the schema
representation differs: a flat schema names a kind with a string, a composite one needs a tree. Keeping
them apart left the original 26 messages and their parser untouched, which is worth more than one
uniform file — those messages pin the format as it stood before composites existed.

## Composites in compact mode, and two rules for choosing the mode

**Context** — Compact mode refused nested structs, arrays of structs, maps and nested slices. The
refusal read as structural but was not: the reader takes every value's type from the schema, and a
nested struct's fields, a struct element's fields and a map's key and value types are all static and
already resolved by `describeKind`. Every value form in the format is self-delimiting — an array
carries its count, a `packed5` frame carries its own end, a key run closes on the terminator — so
recursion needs no byte lengths and no type tags. What is genuinely impossible is an `interface`,
whose concrete type is a property of the *value*; a pointer to the zero value without omit-empty,
because presence *is* being named by a key; and a self-referential type, because the plan is a tree
of sub-plans. Only the record count past three is a header limit (two shape bits).

But `compactUsable`'s old comment was right about *size*: a struct holding a hundred sub-structs has
a hundred records' worth of columnar data, so per-record framing loses. Lifting the refusal without
addressing that would have made `Marshal` silently produce larger messages for exactly those types.

**Decision** — Compact mode carries nested structs (a nested key run), arrays of non-scalar elements
and maps (a count then the values), to any depth. The mode choice split in two: a **flat** type still
takes compact outright at one record, and everything else — two or three records, or any composite
type — has both forms built and the smaller kept, which is the rule two-and-three records already
used. `MarshalForceCompact` overrides it and errors rather than falling back, naming the field in the
way. `Codec[T].Append` stays columnar for a composite type and routes it through the same build-both
path, so a handle writes byte for byte what `Marshal` writes.

Three secondary calls: `compactOp` gained a `uint16` sub-index inside the padding it already had, so
it is still sixteen bytes and the scalar hot loop is untouched. The `ALL_POSITIVE` pre-scan reports
`false` outright when a signed integer sits inside a composite, instead of recursing to find it. And
`decodeCompact` now reuses its destination's capacity, which it never did.

**Rationale** — Two mode rules rather than one because one rule has to pick a side: always-compact
makes some messages bigger with no warning, always-compare costs a second encode on the flat
single-record path that is the whole reason compact mode exists. Splitting on `hasComposite` — a
property of the type, resolved once — keeps today's fast path byte-identical and pays the extra
encode only where the answer is genuinely unknown. `vectors.json` regenerating unchanged is the
evidence: nothing already on the wire moved.

`MarshalForceCompact` errors instead of falling back because the failures are all type errors a
caller wants to hear about once, at development time — an `interface{}` field, an untagged pointer, a
fourth record — and a silent columnar message turns each into a size regression nobody looks for.

The conservative `ALL_POSITIVE` is the one real concession. Finding a signed integer inside a nested
struct, a struct element or a map value means a second full traversal of the value, duplicating the
write path for one payload bit per integer. Reporting `false` costs exactly that bit, only for
composite types, which are new to compact mode and therefore regress nothing — and the build-both
rule above means such a type cannot lose on size for it. Full recursion is a later change with a
measurement behind it, not a guess.

Both directions keep the record path's scalar cases inlined rather than shared with the positional
element path, which is the duplication in this change. It was measured, not assumed. On the write
side the record loop's speed *is* that it reads each field exactly once, fusing the zero test into
the load, and a shared value writer would load every field twice. On the read side there is no zero
test to fuse and sharing looked free — it measured 4% slower on `BenchmarkRecordUnmarshalCodec`,
because the shared switch is far too large to inline and a flat single-record decode is nothing but
those calls. Inlining the fifteen scalar cases and delegating only the composites recovered it
exactly. Composites pay the call, which is noise against the reflect work they already do.

Map entries go out in Go's iteration order, so compact output is not byte-stable for a map-bearing
type. That matches the columnar path, which has never sorted either; diverging would have meant two
orders for one value, and sorting both modes would have moved bytes that are already stored.

The Rust decoder is deliberately left behind. Its `Schema`, `Kind` and `Value` are flat and support
no composite in *either* mode, so this is not a regression — a nested-struct message was unreadable
there before. Keeping up means `Kind::Struct(Schema)`, `Value::Record` and a recursive `read_value`
across `compact.rs` and `standard.rs`, which is a comparable amount of work to this change and worth
doing when a Rust caller needs it. No composite vectors were added, since they would fail there by
construction.

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
keep honest for free, and the first thing to rot. *(Superseded: a caller appeared. See the entry
above, which keeps this reasoning and answers it with a Rust→Go direction in CI.)* The compact-mode surface rather than the full
standard-mode one, because compact mode is what a single-record message *is*, and it is the shape
every current Rust reader wants; the cost is that a Rust reader of an ORM blob with nested structs
or maps would need the crate widened first, which is a larger crate and a decision worth taking
when there is a caller for it rather than in advance. *(Partly superseded: compact-mode composites
are read and written now. Standard-mode composites still are not, and the cost named here is still
what they would take.)*

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
