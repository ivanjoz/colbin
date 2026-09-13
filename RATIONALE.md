## A slice or map at the root is wrapped, not given a root shape of its own

**Context** — The byte-aligned format encodes a struct and nothing else, which is
what reserves `0xD0..0xDF`: the first byte is an ordinary STRUCT descriptor, so
the other 240 values are guaranteed never to be colbin and an application may use
them as its own framing. But a caller with a complex ORM column in its hand holds
`[]Grant`, not a struct wrapping one, and the previous format took it. Refusing it
made the format unusable for exactly the storage case it was written for.

**Decision** — A non-struct root is encoded as a one-field message: key 0, the
value, nothing else. `Marshal([]Grant{...})` writes byte for byte what marshalling
a hand-written wrapper whose only field is tagged `cb:"0"` writes, and `Unmarshal`
into a `*[]Grant` reads it back. Slices, arrays and maps are wrapped; a bare
scalar is still refused. `codec/envelope.go`.

**Rationale** — A root list would have spent one of the four root detail bits and
forced every port — the Rust crate, the browser module — to grow a second root
shape before it could read one blob. The envelope needs none of that: what lands
on the wire is an ordinary message, so every reader that exists already parses it
without being taught anything, and the reserved range keeps meaning what root.go
says it means. It costs two bytes, the key and the descriptor. There is no copy
and no allocation — a struct with one field at offset 0 has the address of that
field, so the caller's pointer *is* the pointer the envelope plan reads through,
and the synthetic type exists only to carry a cached plan.

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

## One byte-aligned format: what the wire gave up, and what it refused to

`BYTE_ALIGNED_PLAN.md` is the design; this is the record of the decisions that
changed while implementing it, and of the ones the measurements reversed.

### Little-endian, where the format was big-endian

Every multi-byte quantity is now little-endian. A fixed-width element becomes one
native load on every machine this runs on, and a variable-width integer becomes a
`Uint64` load and a mask rather than a shift chain. The cost is that the format
no longer reads left-to-right in a hex dump, which is a cost paid by whoever
debugs it and not by anything in production.

### Size codes are byte counts, and no code is unassigned

An integer's three size bits were a code into `{1,2,3,4,6,8}` with code 7
reserved. They are now the byte count outright: 0 means the value is one, 1..6
are themselves, 7 is eight. That makes five-byte integers representable — a byte
saved on every value in `[2^32, 2^40)`, which is where a millisecond timestamp
offset or a large row id lands — and it costs nothing, because a seven-byte
magnitude already rounded up to eight.

It also removes the unassigned code, and with it `ErrBadSizeCode`. A reserved
door in a four-bit descriptor was never going to be wide enough for the
self-describing variant it was being kept for; that variant is the eight-bit key
width, which has a whole class field.

### No varint anywhere, including where it was cheapest

LEB128 continuations are gone from sizes, counts and element lengths. A header
now carries the common size and, when it does not fit, names the *width* of the
size that follows. Reading a length is a branch and one load instead of a loop.

This deletes `appendSizeExtension`, `continuedSize`, the nine-byte continuation
guard and the class of bug they existed to prevent: a peer-controlled run that
accumulates past what an `int` holds and wraps into a small size whose bounds
check then passes. What is left to refuse is a size this platform cannot
address.

The escape costs less than the varint did, not more. When `more` is set the three
size bits carry nothing, so they name the escape width instead: a 5 KB string
pays one extra byte where LEB128 paid three.

### A float trims the opposite end from an integer

The plan said a float rides in the integer shape "with trailing zero bytes
trimmed". Under the little-endian rule that trims the *high* bytes — the exponent
and sign, which are never zero for a non-zero float. Measured, it saved nothing:
8.00 bytes per value on every float column tried.

An integer's zero bytes are its most significant; a float's are its *least*. So a
float writes its pattern with the bytes reversed, and the integer path then works
unchanged. Round quarters go from 7.99 bytes to 2.93, and a `float64` holding a
value that is exactly a `float32` has twenty-nine zero low bits — three whole
bytes and five over — and costs five. Nothing was added to detect that case; it
falls out.

### A pointer must not appear in a generic signature

`WriteInts[T](w *Writer, ...)` is reached through a shape dictionary, and the
escape information a caller **in another package** gets for it is conservative
enough to put the writer on the heap. That is one allocation per message and
3 ns per record on codec's plan walk, for a function whose body allocates
nothing.

So there is a concrete `Int32s`, `Uint16s` and so on beside the generic form, and
the generic work underneath them takes slices and values only. The repetition is
the point, and it is why the generic entry points now document that they cost the
caller an allocation.

This is invisible in the source, invisible in `go vet`, and only visible in
`-gcflags=-m` at `derefs=0`. It will be reintroduced by anyone who tidies the
concrete methods into wrappers.

### The column codec: bits inside byte-aligned blocks

The `(k, M)` bit varint is replaced by blocks of 128 residuals at an exact bit
width chosen per block. The arithmetic that makes it work is that 128 values at
`w` bits occupy `128w/8 = 16w` bytes — a whole number of bytes for every `w`, and
a whole number of 64-bit words too. A block is therefore byte-aligned at both
ends, wastes no padding, and carries no state across a boundary, while the width
ladder stays one bit fine.

Measured over five column shapes it is **smaller than the codec it replaced on
every one of them**, from −59% to +1%, and 2–3× faster to decode, 4–5× faster to
encode. This was the one place where byte alignment looked like a size sacrifice,
and it turned out not to be one.

Three rules the measurements insisted on, each of which was wrong first:

- **The base goes in the clear.** Folding delta's first value into the residuals
  sets the first block's width from one element: +97% on a column of timestamps.
- **The transform is scored against its blocked cost**, not the column's widest
  residual. The latter picks frame-of-reference for a column of small ids and
  loses 76% where delta loses 14%.
- **Constant is scored, not taken on sight.** A single value taken as "constant"
  costs nine bytes where raw costs three. And a column of zeros is *not*
  constant: a width-0 block carries nothing, so 256 zeros are three bytes against
  constant's nine. The encoder finds this; the test that expected otherwise was
  wrong.

The old `trFixed` fallback is gone. It existed to bound the output at the element
width; the widest block width bounds it now, at one byte per 128 values — +0.1%
on incompressible data, and a byte or two on a column of one to three elements.

This broke `compact.GetInts`, which bounded array counts by "at least one byte
per element". That floor no longer exists: 128 residuals at width zero occupy one
byte, so a thousand elements can arrive in nine, and valid messages were being
refused as truncated. A column is bounded by its header and one width byte per
block, and nothing else.

### The key width is split across functions, not tested per field

Measured on a ten-field record: one decoder carrying a `k8 bool` runs at 11.7 ns
where a decoder per width runs at 8.7. The branch is not the cost — it goes the
same way on every field and predicts perfectly. The cost is that a width the
compiler cannot see is a width it cannot fold: the field stride stops being a
constant, the descriptor's position stops being a constant, and the function
grows past the budget that would have inlined it.

So the framing is written once per key width and the value codecs are written
once and shared. The seam is: reading the key and decoding the descriptor differ;
the magnitude load, the blob slice and the array elements do not.

Two corollaries. In Rust this is a const generic and the source is written once.
In Go it cannot be — two distinct empty type parameters share a GC shape, so a
generic reader goes through a dictionary and the constant never folds — so the
duplication is the honest cost of the approach on this side.

### The presence bitmap is smaller and *slower*, which reverses the plan

`BYTE_ALIGNED_PLAN.md` §2.7 argued that replacing a wide key run's per-field keys
with a presence bitmap is smaller *and* faster, and used that to argue K4 might
be droppable. Half of that is right.

Smaller, measured on the ten-field record: **10 bytes against the narrow key's 11
and the wide key's 12**, and 15 against 20 and 22 with every field set. Better
than the plan predicted, because the plan counted a key byte for a field that has
none.

But slower, by a lot, and it stayed slower after two rounds of optimisation:

| ten-field record, five set | encode | decode | bytes |
|---|---:|---:|---:|
| narrow key | 5.2 ns | 14.3 ns | 11 |
| wide key | 6.0 ns | 24.0 ns | 12 |
| wide key, presence bitmap | 18.7 ns | 38.4 ns | **10** |

Width-typed writers took the encode from 18.9 to 18.1 and a trailing-zero scan
over a word took the decode from 48 to 38.4. Holding the bitmap in a register
rather than read-modify-writing the buffer removed 22% of the encode profile and
gave it all back to the patch loop that replaced it.

The remaining gap is that a bitmap run cannot be walked with the cursor
arithmetic a keyed run is walked with: the reader has to find the next set bit,
back the cursor up, and dispatch, and none of those inline into a caller's
switch the way `Reader.U16` does.

**So K4 stays, and the case for it is stronger than before.** The bitmap is the
right choice for a wire that is size-bound and the wrong one for a wire that is
latency-bound, which is the case this format exists to serve.

### Negative results, recorded so they are not retried

- **`//go:noinline` on the wide fallbacks.** The idea was to keep `U32` small
  enough for the inliner by stopping `u32Wide` being folded into it. Measured
  14.1 ns against 13.9 — nothing. Reverted.
- **Splitting the column unpack's tail.** The profiler attributes 9% to
  `unpackTail`; running the fast path for the values whose eight-byte load still
  fits, and gathering only the rest, measured within noise both on a 1024-element
  column and on a 16-element one that is *entirely* tail. Reverted. Computing the
  boundary per block rather than per column measured 8% *slower*.
- **A presence bitmap under K4.** Rejected on arithmetic rather than
  measurement: K4's key rides in the descriptor byte the field needs anyway, so
  removing the key removes nothing and the bitmap is pure addition.

## Composites, tables and a generator

### A composite's length is written forward and patched

A nested struct's length is not known until its body is. The writer reserves one
byte, writes the body, and patches it; a body past 255 bytes makes room for four
by shifting what follows. The alternative — walking the value twice to size it
first — costs a pass over every nested value on every message to save a rare
copy on the large ones, which are the ones least able to notice it.

The reserved byte is what makes this affordable. Reserving four would cost three
bytes on every nested struct in every message, and most nested structs are small.

### Every composite carries a byte length, and that is the point

`compact.ErrSkipComposite` exists to say that a nested struct, array or map
cannot be stepped over without its sub-schema. Spending a length on each one is
what removes that limitation: `Reader8.Skip` steps over a struct, a list, a map,
a table and a column without knowing anything about what is inside. Measured on
a record holding all four, a reader that knows only its last field still finds
it.

### A table is a field class, not a mode

The columnar layout is now the `TABLE` class: a length, a row count, and a key
run whose fields are *columns* rather than values. That is exactly what the old
standard mode was — the key a row-wise list spends once per field per element is
spent once per column — except that it is now a per-field decision rather than a
per-message one. A record can hold a three-element list and a ten-thousand-row
table and encode each the right way.

Measured, on a four-field row:

| rows | table | list of structs |
|---:|---:|---:|
| 1 | 21 B | 18 B |
| 3 | 50 B | 57 B |
| 128 | 1034 B | 2524 B |
| 4096 | 31896 B (7.8 B/row) | 84371 B (20.6 B/row) |

The crossover is three rows, and the table is 2.6× smaller by four thousand. The
old format put that threshold at three records too, and chose it once for a whole
message; it is now chosen per field and measured rather than asserted.

An all-zero column is not written, and an absent column key is what says so. That
is the old format's omit-empty version byte deleted: the columns are keyed, so
absence already carries the information, and there is nothing to turn on.

### The generator emits what a hand-written codec would

The plan walk costs 16.7 ns to encode a ten-field record where the straight-line
calls it stands in for cost 5.3. The difference is not the reflection, which
happens once — it is the switch, the offsets loaded from the plan and the
`unsafe.Add` per field, none of which the compiler can fold because none is a
constant.

`GenerateMinimal` turns them into constants, and the result is measured at the
hand-written number:

| ten-field record | plan walk | generated | hand-written |
|---|---:|---:|---:|
| encode | 16.7 ns | **5.4 ns** | 5.3 ns |
| decode | 26.4 ns | **16.5 ns** | 14.5 ns |

It works from a `reflect.Type` and the existing plan builder rather than by
parsing source. That is a deliberate constraint: there is one definition of what
a field id is, which type maps to which width-typed call, and which fields are
encodable, and the generator cannot disagree with the reflective path about any
of it. The cost is that a caller writes a three-line generator program instead of
adding a `//go:generate` comment.

Two things the generated code carries that hand-written code usually forgets:
the compile-time key assertion the writer's documentation prescribes, and a test
that regenerating produces the same source — so a change to the format cannot
leave stale generated code quietly compiling against it.

## Phase 6: one format, and the repository that says so

The three modes are gone. What is left is the format, and the packages are named
for what they are rather than for which mode they served.

	wire     was minimal/   the format: framing, both key widths, composites, tables
	column   was varint/    the column codec, which has not been a varint since it
	                        became blocks of 128 residuals
	codec                   the reflection façade and the source generator
	packed5                 an opt-in string encoding, off by default

Deleted outright: `compact/`, the standard mode's half of `codec/` (encode,
decode, typeinfo, schema, any, null maps, omit-empty, the float SIMD scan, the
column pool), `comparison/`, and the Go-side vector generators for the old wire.
Around six thousand lines. Nothing was left deprecated: a mode that no longer
exists is not a compatibility surface, it is dead code with tests holding it up.

What went with them is real and worth naming: nested structs, maps, interfaces,
JSON mode and the self-describing schema are not reachable through `Marshal`
again until the façade grows to use the composites `wire` now has. The wire can
express all of them; the reflection over it cannot yet.

### The root descriptor, at last

`Marshal` now writes byte 0 as the root value's own descriptor — `0xD0` for a
narrow-keyed struct, `0xD8` for a wide-keyed one — and the decoder dispatches on
it. That is the last piece of §2.1: the mode bit, the shape bits, the
omit-empty flag and `ALL_POSITIVE` are all either in that byte or gone, and no
message the old format wrote can be mistaken for one, because every legal root
byte is even and above 0x90.

It costs one byte and, on the encode path, about 1.5 ns.

### packed5 is a flag, not a mode

`SetPacked5` is off by default and process-wide. It is a **writer** setting: the
encoding lives in a BLOB's own descriptor, so a decoder reads either form without
being told which, and turning it on is a size decision rather than a wire
version.

It only reaches the wide key width — four descriptor bits have no room for an
encoding code — so a type with strings goes wide when the flag is on. That is
the honest shape of the trade rather than a special case: the flag buys about a
third of a short upper-case token and costs a pass over every string on both
sides, plus the byte per field the wide key costs.

The encoder still chooses per string. `packed5.Size` is exact, so a token that
would not pack smaller is written raw and the descriptor says so — which means
turning the flag on can never make a message larger.

### Two regressions the root descriptor introduced, and the profile that found them

Decode went from 27 ns to 45 the moment the root byte landed, which had nothing
to do with the byte:

- **Boxing.** `rootOf(data, dst)` took the record as an `any` so it could name
  the type in an error. That allocated on every decode — to describe a failure
  that does not happen. It now returns a bool and the caller formats the error
  out of line.
- **`plan.find` was a linear scan**, O(fields) per field and so O(fields²) per
  record: 24% of the profile, the largest single line in it. It is now a
  `[]int16` lookup table sized to the largest declared key, built with the plan.
  This is the optimisation §5.2 of the plan has recommended from the start; it
  took a profile to make it the obvious thing to do rather than the next thing.

Both together: 45 ns back to 26, which is under where it started.

## Against protocol buffers

`bench/` is the comparison: the same fields with the same numbers on both sides,
protobuf through its generated code — its fast path, not its reflective one —
and colbin three ways.

| one flat record, six fields | protobuf | colbin | |
|---|---:|---:|---|
| encode, reusing a buffer | 110 ns | **23.1** straight-line · 33.0 handle | **4.8× · 3.3×** |
| encode, allocating | 126 ns | **70.3** | 1.8× |
| decode | 103 ns | **54.6** straight-line · 60.6 handle · 75.1 façade | **1.9× · 1.7× · 1.4×** |
| bytes | 32 B | **27 B** | |

| one order, three nested lines | protobuf | colbin | |
|---|---:|---:|---|
| encode | 241 ns | **84.2** | **2.9×** |
| decode | 458 ns | **264** | **1.7×** |
| bytes | **49 B** | 55 B | |

Faster everywhere, and smaller on the flat record. The nested record is six bytes
larger, and the reason is worth writing down rather than rounding off.

### Where the six bytes go, and the fix that is not written yet

A struct holding a composite is forced to eight-bit keys, because a composite
carries a byte length and only a wide descriptor has room for a class to hang it
on. The keys themselves are 1, 3, 5, 6 — all of which four bits would hold. So
every *scalar* in that struct pays a byte it does not need, to make room for the
two fields that do.

The format already has the answer and the implementation does not use it:
§2.5 gives the narrow nibble a composite form, `[key:4][lw:2][k8:1][—:1]`,
because a narrow reader takes the class from the schema and needs no class bits
on the wire. Writing that would let a struct with nested fields stay narrow and
close most of the gap.

What *is* implemented, and was worth five of the eleven bytes it started at: the
key width is per scope, so a nested run uses four-bit keys whenever its own ids
allow, however wide its parent is. That is the `k8` bit in the `STRUCT`
descriptor doing the job it was specified for.

### Two optimisations found by profiling the comparison

- **`Marshal` grew its buffer three times.** protobuf sizes a message before
  writing it, which is why its `Marshal` is one allocation. A bound taken from
  the plan — the root byte, plus the widest a fixed field can be, plus a little
  for the variable ones — gets the same answer for every record without a pass
  over the value. Three allocations to one, 95 ns to 83.
- **It then resolved the plan twice**, once to size the buffer and once inside
  `Append`. 83 ns to 68.

### A negative result: the exact-width store

`appendMagnitude` stores eight bytes and cuts back rather than storing exactly
`width` through a switch. The over-store looked like the thing to fix when the
allocation profile pointed at it, so it was replaced with a switch — and a
six-field encode went from 23.0 ns to 30.5. The jump table costs more than the
discarded stores, and the capacity the over-store leaves behind is wanted anyway
by the next field. Reverted, with the number in the comment.

The allocations it was blamed for were ordinary slice growth from a nil buffer,
which is what the size hint above actually fixed.

## Finishing the façade: tables, maps, and the narrow composite

### A slice of structs picks its own layout, per value

The row count is data, not type, so the plan cannot decide between a list of
structs and a transposed table. The encoder does, at `tableThreshold` rows, and
because the two are different descriptor classes the reader dispatches on what it
finds rather than on anything it was told. That is the old standard-vs-compact
mode decision made per field instead of per message.

Measured on a six-field row: 16.9 bytes a row as a list at seven rows, 11.0 as a
table at a thousand.

Not every slice can be one. A column has to be a column *of* something, so a
struct with a nested struct or a slice inside it stays row-wise however long it
gets. That is a property of the element type and the plan resolves it once.

The transposition buffers are held on a scratch and reserved once per table.
Growing them column by column cost twenty-one allocations on a thousand-row
table; reserving costs three.

### Maps pay reflection, and the comment says so

Every other field kind is reached by offset with no reflection left at encode
time. A map cannot be — Go gives no way to walk or fill one through an unsafe
pointer — so it uses `MapRange` and `SetMapIndex` and pays for them. That is the
cost of the kind rather than an oversight: a map field is already a hash lookup
per entry on both sides, and the reflection sits on top of something that was
never going to be a strided store.

Keys are strings or integers; values add floats and bools. A map of structs is
refused with the field named, because silently dropping it would be worse than
saying there is no form for it yet.

### The narrow composite, and the seven bytes it was worth

Until it, a struct holding any composite had to use eight-bit keys: the composite
needed a byte length, a byte length needed a class, and a class needed the wide
descriptor. Every *scalar* in that struct then paid a byte it did not need, to
make room for the one or two fields that did.

§2.5 said the narrow nibble could carry a composite — a narrow reader has the
schema, so it already knows the shape and needs only the length. Implementing it
took a four-field order with three nested lines from **55 bytes to 48**, against
protobuf's 49, and dropped the nested encode from 84 ns to 72 with its last
allocation.

The detail nibble turned out to be bit for bit the wide one's. The wide
descriptor spends its extra nibble on the class and K4 takes the class from the
schema; everything below that is shared, including the length-widening backpatch.

What it does not buy is skipping: a narrow reader still cannot step over a field
it does not recognise, composite or not. That is K4's standing trade and the
reason the wide width still exists.

### A bug the narrow elements introduced

`ElementUint(0)` wrote size code 0, which means *the value is one* — the code
that makes a true bool a single byte. A scalar field never reaches it with a
zero, because a zero field is omitted; a map's value is not a field, and zero is
a value it can legitimately hold. A `map[string]bool` round-tripped every `false`
as `true` until the element writer spent a byte to say so.

The wide element writer never had the bug, because its inline form encodes zero
as itself. Two implementations of the same idea, and only one of them met the
case.

### Against protocol buffers, finally

| | protobuf | colbin | |
|---|---:|---:|---|
| flat record, encode | 110 ns | **24.5 ns** | 4.5× |
| flat record, decode | 98 ns | **48.7 ns** | 2.0× |
| flat record, bytes | 32 | **27** | |
| nested, encode | 238 ns | **71.8 ns** | 3.3× |
| nested, decode | 415 ns | **268 ns** | 1.5× |
| nested, bytes | 49 | **48** | |

Faster and smaller on both shapes, with packed5 off — which is what it was for.

### Pointers: the one place a zero goes on the wire

Every other field in this format expresses "zero" by not being there. A pointer
cannot, and that is the whole difficulty: `nil` and `new(int32)` are different
values, and the omit-zero rule would write neither of them.

So the rule splits. A nil pointer is omitted, which costs nothing and reads back
nil because the decoder zeroes the record first — absence was already the
encoding for it. A **non-nil pointer to a zero value** writes an explicit zero:
`Writer.Zero` for a number, `EmptyString` for a string. That is the only value in
the format written solely to be distinguishable from its own absence.

`Zero` writes an integer descriptor, not a typed one, and the float and bool
readers both go through `Uint` — so one two-byte form covers `*int32`, `*bool`,
`*float64` and the rest without a case per type. A `*string` is a zero-length
blob.

**Scalars only.** A pointer to a struct, a slice or a map is refused. Those carry
a length already, so they *can* express absence — but a nil composite and an
empty one are the same thing on this wire today, and deciding what `*[]T` nil
means is a format question nothing has asked yet. Refusing is reversible;
guessing is not.

The cost to everything else is a `case` in four switches and a `fieldOp` the
jump tables already had room for. A type with no pointer in it never reaches any
of it.

### The scalar walk, and the cost the narrow composite had been hiding

Landing §2.5 made `writePlan` and `readField` handle composites on the narrow
path, which they had not had to before — a composite used to force the wide
width. Three new arms, each calling out of line and taking a scratch buffer.

The bill went to every record, nested or not. On a ten-field flat record the
handle encode went 18.7 → 23.9 ns and the decode 25.5 → 29.0, a fifth of both,
for cases those types never reach. The hand-written path was untouched at 5.5 ns,
which is what pinned the cost to the plan walk rather than to `wire` or to the
machine.

So the walk splits on a flag resolved with the plan. `simple` means no struct,
slice-of-struct, map or pointer field, and a simple plan goes to `appendScalars`
/ `readScalars` in scalars.go — the same switch with the out-of-line arms absent,
so there is no scratch buffer and nothing to spill around a call.

| | before | after |
|---|---:|---:|
| ten-field handle, encode | 23.9 ns | **19.0 ns** |
| ten-field handle, decode | 29.0 ns | **22.4 ns** |
| six-field handle, decode | 68.5 ns | **57.5 ns** |

It is the two-key-widths argument again: a case the compiler can see is absent is
a case it can stop paying for. It duplicates a switch, which is the same price
`wire` pays to keep K4 and K8 apart, and for the same reason.

Nested types are unchanged — they take the general walk, as they must.

### Correcting the benchmark table

The numbers this file and the README carried for the flat record (24.5 ns encode,
48.7 ns decode) do not reproduce. They cannot: the *hand-written* straight-line
encoder for that same six-field record measures 28.7 ns, so no reflective path
was ever below it. The table has been replaced with a single sweep in which both
sides are measured together, and the generated and handle paths are now listed
separately rather than collapsed.

colbin is still faster and smaller than protobuf on both shapes — 3.3× encode and
1.8× decode on the flat record, 3.4× and 1.6× nested — which was the claim. The
margin on decode was overstated.

### Two integer encodings, one per key width

K8 cost a whole byte more than K4 for the same small value, and the complaint
that started this was that it should cost four bits more, not eight.

It cannot. Four of those eight bits *are* the extra key bits — that is what 256
ids means — and byte alignment cannot spend twelve. What was reclaimable is the
descriptor waste on each side, and the two sides waste differently, so they are
now encoded differently. The logic branches, which is the same trade `wire`
already makes to keep the widths in separate files.

**K4: the sign bit on an unsigned field.** A narrow reader has the schema, so it
already knows the field is unsigned — and `Uint` was setting a `positive` bit
that is *always* set, one bit of four, on the width most of this wire uses. The
nibble is now a sixteen-code table: `0..7` is the value itself with no payload,
`8..15` is a magnitude of one to eight bytes.

Two things fall out. Small unsigned values become a single whole byte, key
included. And the widths are exact — a seven-byte magnitude costs seven, where
the signed form still rounds it up to eight because it has only three bits to
name a width with.

It also deleted a special case rather than adding one: `ElementUint(0)` used to
spend two bytes explaining that it meant zero, because the signed code 0 means
"the value is one". Zero is now just the code 0.

**K8: a varint, but only when it wins.** A wide field has already spent a byte
on its key, so its descriptor byte begins with none of the value in it — and
class INT spends all four detail bits on a sign and a byte count. 300 therefore
cost four bytes: key, descriptor, two magnitude bytes.

The new form puts three value bits in the descriptor and continues seven at a
time, under `SPECIAL` with detail bit 3 set. 300 costs three.

The measurements are why it did not simply *replace* the byte-count form. Seven
bits per byte loses to eight once a value is wide:

| average bytes per field | byte-count | varint only | writer picks |
|---|---:|---:|---:|
| small ids 0..1000 | 3.60 | 3.34 | **3.34** |
| deltas −1000..1000 | 3.67 | 3.41 | **3.42** |
| random int32 | 5.99 | 6.49 | **5.99** |
| random int64 | 9.99 | 10.97 | **9.99** |

So both forms stay and the writer emits whichever is shorter. That makes the
change strictly a saving — there is no value anywhere in ±2²² that got larger,
and a test asserts it against the old size function directly.

A signed field zigzags into the varint and an unsigned one does not, which would
be ambiguous if anything read it without the schema. Nothing does: the same
split is what K4 has always relied on. An unknown field is still skippable,
because the varint is self-delimiting and sits under a class.

**What it bought.** The nested order went from 48 bytes to **46**, against
protobuf's 49. Encode and decode ratios did not move.

**What it also did, unexpectedly.** The bitmap experiment in `bitmap.go` used to
beat the narrow key on its best case — ten small fields, 13 bytes against 20.
Reclaiming the sign bit put seven of those ten values entirely inside their
nibble and narrow now ties it at 13, while still carrying a key per field. On
the five-of-ten shape narrow now wins outright, 9 against 10. The bitmap's
remaining argument is thinner than it was.

### What the K4 nibble actually costs, measured properly

A first reading said the unsigned nibble had made the hand-written ten-field
encode 25% slower, 5.47 ns against 6.84. That was machine drift: re-running the
*unchanged* code an hour later reproduced the slow number, and a 7 ns benchmark
on a P/E-core laptop under load swings ±20% between runs.

The honest measurement is an A/B in one process, both encodings writing the same
record:

| | bytes | ns |
|---|---:|---:|
| sign bit and size code | 11 | 5.25 |
| unsigned nibble | **9** | 5.46 |

**3% slower, 18% smaller.** One extra compare on the write path, and it is only
one because the omit-zero test folds into the inline test — zero is a code the
nibble carries, so `value <= 7` subsumes `value == 0` and the compare count is
what it was.

The lesson is about the method rather than the result: no absolute benchmark
number in this repo is comparable against one taken in a different run. Where a
claim is about a change rather than about protobuf, it needs both arms measured
together.

### Untagged structs, and the escape trap the wide side still had

Benchmarking the three ways a caller can hand colbin a struct — tagged,
untagged, packed5 — needed the untagged one to exist. It had been refused, with
an error telling the caller to number the fields.

The previous format's rule is restored, which is the one already-written readers
expect: `fnv8` of the field name, then a linear probe upward past whatever is
taken. Explicit ids are reserved in a first pass so a hash can never squat on a
number somebody asked for; unnumbered fields probe in a second. A derived id
lands anywhere in 0..255, so any type with one uses eight-bit keys. Numbering
the fields is therefore also how a type asks for the narrow width.

**What the benchmark then exposed.** The untagged record allocated where the
tagged one did not — once on encode, twice on decode — and that had nothing to do
with tagging. Two separate causes, both on the wide path only:

1. `writeVec[T](w *Writer8, ...)` and `readVec[T](r *Reader8, ...)` put a pointer
   in a *generic* signature. This is the exact trap recorded above for the narrow
   writers, which were fixed by keeping pointers out and passing slices. The wide
   twins were never fixed. `appendVec` now takes and returns the buffer; the
   readers share framing through an ordinary method and decode through a generic
   that sees only slices.

2. The wide walk had no scalar fast path, so a flat wide record carried a
   scratch buffer for a transposition it could never do — and because escape
   analysis is per *variable*, declaring one writer for both branches heaped it
   on the flat path too. The branches now declare their own.

| untagged flat record | before | after |
|---|---:|---:|
| encode | 55.4 ns, 1 alloc | **41.3 ns, 0 allocs** |
| decode | 80.2 ns, 2 allocs | **63.4 ns, 1 alloc** |

The lesson is the one the narrow side already taught, and the reason it recurred
is that the fix was applied where it was measured rather than everywhere it
applied. A generic function taking a `*Writer` or a `*Reader` is a bug in this
package, not a style preference.

## The Rust port, rewritten against the one format

**Context** — The Rust crate implemented the *previous* three modes: a compact
bitstream, a columnar standard mode, an early minimal mode with big-endian
magnitudes and LEB128 continuations, and a schema-driven `Kind`/`Value` tree to
read them through. None of that describes the wire any more. Worse, its tests
passed: it asserted against its own copy of a vector rather than one the Go side
generates, so the two ports had diverged in silence and neither build said so.

**Decision** — Deleted, and written again against the one format. The crate is
now `wire` (all three key framings), `column`, `packed5` and a `codec` module
holding the `Colbin` trait, the root descriptor and the id assignment — the same
four layers the Go side has, file for file. The dynamic `Schema`/`Kind`/`Value`
path is gone entirely: `#[derive(Colbin)]` emits the straight-line calls, which
is what `codec.Generate` emits for Go and measured 5.4 ns against a plan walk's
18. There is no reflective path in Rust to fall back to, so there is no reason
to carry a second, slower one.

**Rationale** — Four choices are worth naming.

*The widths are separate types, not a flag.* `Writer`/`Reader`,
`Writer8`/`Reader8` and `BitmapWriter`/`BitmapReader`, exactly as in `wire/`.
§5 of the plan suggests a const generic here, and it would work — but the two
widths differ in more than a number: K4 has no class in its descriptor and K8
has an inline value form, so a single body would be a `match` on the width in
every method rather than a parameter the monomorphiser folds. The value codecs
underneath are shared, which is where the duplication would have cost something.

*Sticky errors, as in Go.* A reader holds its first failure and parks its cursor
at the end, every later read answers zero, and a decode checks once at the end.
A `Result` per field would have been more idiomatic and would have put a branch
on every read of a message that almost never fails; this way the generated
`match` arms are assignments.

*Ids are a `const fn`, not a third implementation.* An untagged field's id is
`fnv8` of its name, linear probed past the explicit ones — and that rule now
lives once, in `colbin::assign_ids`, which the derive *calls* at compile time
rather than reimplementing. The keys stay literal constants, so the decode is
still a `match` on constants and not an if-chain.

*The corpus is the specification, and it is generated.* `rust/vectors/main.go`
writes every message with `colbin.Marshal`, every field id with
`colbin.FieldIDs` and every column with `column.AppendArray`; the Rust tests
hold the same values and assert both directions. `go test ./rust/vectors` fails
if the committed corpus is stale, which is the check whose absence let the
previous port drift.

### What the corpus caught immediately

A `[]uint64` holding `u64::MAX`. The Rust port encoded it as an eight-byte
magnitude; Go encodes it as one byte of two's complement, because
`isSigned[uint64]()` answers **true** — `-1` converted to a `uint64` and read
back through `int64` is still −1. The comment beside that test says it is what
"keeps a `uint64` past 2^63 from being mistaken for a negative number", and it
does not do that.

It is not a data bug: the narrowing and the sign extension are exact inverses,
so Go round-trips such a column, and it is *smaller* — one byte against eight.
It is an inconsistency between what the code does and what it says, and between
`uint64` and the three narrower unsigned types, which do take the magnitude
form. The Rust side mirrors the behaviour rather than the comment, because the
wire is what Go writes; both are worth correcting together, and neither can be
corrected alone now that a corpus fails when they disagree.

The same corpus also pinned `packed5` byte for byte, where the Go README's own
worked-example table had drifted — it quotes 17, 22, 18 and 26 bytes for four
frames the encoder now writes in 16, 21, 17 and 25. The numbers in a table go
stale; the bytes in a test do not.

### A protobuf comparison without protoc

The corpus needed protobuf twins, and `protoc` is not installed on this machine.
It turned out not to be needed. protoc's only job in the pipeline is turning
`.proto` text into a FileDescriptorProto — and a descriptor is an ordinary
protobuf message, which can be built directly. `protoc-gen-go` is a plain
program reading a CodeGeneratorRequest on stdin, and it ships inside the
`google.golang.org/protobuf` module the repo already depends on.

So `internal/protogen` builds the descriptor in Go, pipes it through the plugin
out of the module cache, and writes `bench/corpus.pb.go`. No protoc, no network,
no new dependency. The cost is that the Go program is the source of truth and
`bench/corpus.proto` is written out as documentation rather than read back,
because nothing here parses `.proto` text.

**The comparison gives protobuf its best form, not the matching one.** Cents are
`int64` rather than `sint64`: sint64 zigzags, which costs a bit, and every
amount in the corpus is non-negative. Using sint64 because colbin's field is
signed would have been a handicap dressed up as fairness.

### What the corpus then showed

| | protobuf | colbin | |
|---|---:|---:|---:|
| users, 100 rows | 6 275 B | 6 187 B | −1.4% |
| products, 200 rows | 12 996 B | 12 876 B | −0.9% |
| sales, 300 rows | 33 489 B | **26 856 B** | **−19.8%** |
| metrics, 2 000 rows | 22 000 B | 21 680 B | −1.5% |

The flat tables are a wash, and that is the honest result: both formats omit
zero fields and spend a key on each present one, so on a flat record there is
almost nothing between them. Every byte colbin wins overall comes from the
nested one, where a slice of integer-only structs is transposed into columns.

That is worth stating plainly because the earlier single-record benchmarks
implied a broader size advantage than exists. On speed the margin is real and
general — 4× encoding a flat row, 1.7–1.9× on the nested one — but on size, the
claim is specifically about columnar data.

**One thing the corpus exposed and nobody has fixed.** Encoding the 300 sales
allocates 108 times: two buffers per sale that crosses the table threshold, and
54 of them do. `Codec.Append` declares `var buf scratch` per call, so the
transposition buffer cannot be reused across records. Holding it on the Codec
would break the promise that a Codec is safe for concurrent use; a sync.Pool
would not. Not done, because it is a change to a public guarantee and nobody has
asked for the allocation back yet.

## The inline budget is part of the format's speed, so it is now asserted

**Context** — `wire`'s width-typed writers exist because a `uint16` can only be
one byte or two: `U16` is a few appends with no call under them and Go inlines
it, where a generic `Uint` carrying all seven widths does not fit. That is the
whole reason the entry points are duplicated per width — and nothing was
checking it. Two changes since then quietly broke it. The unsigned inline nibble
added a branch to every narrow writer, and the K8 varint added a length
comparison to every wide one. Both are wins on the wire. Both pushed their
writers past the inliner's budget:

| ten-field record, five fields set | encode |
|---|---:|
| narrow key, as the change left it | 7.6 ns |
| wide key, as the change left it | 18.9 ns |

The wide figure is the one that gives it away. It was 6.2 ns when the table in
`wire/README.md` was written, and nothing about the bytes it writes had got
three times harder. `Writer8.U16` cost 107 units against a budget of 80, so
seven of the record's ten fields went out of line — and a method that does not
inline sends *every* value through a call, not only the wide one it grew the
branch for.

**Decision** — Shape the integer writers around the budget, and check it.

A call costs 57 of the 80 units, which leaves room for the inline-value case,
the omit-zero test and one call. Three things buy that back:

- **The omit-zero test folds into the inline comparison** as `value-1 <
  uintInlineMax`, because zero wraps. One compare where the obvious spelling
  takes two, and zero — the value that writes nothing — still costs nothing.
- **The cold half sits behind `//go:noinline`.** A one-line wrapper that the
  inliner *would* fold back in is what put `Writer8.U32` at 81 against 80. The
  pragma reads backwards and is the point.
- **`Writer8.U16` decides the varint against a constant** rather than measuring
  both forms: at two bytes the varint wins in exactly one window, 256..1023, so
  the decision is a comparison rather than a length computation.

| ten-field record, five fields set | encode | |
|---|---:|---:|
| narrow key | 7.6 → **5.3 ns** | 1.4× |
| wide key | 18.9 → **5.4 ns** | 3.5× |

Nothing on the wire moved: the corpus in `rust/vectors` is byte-identical, which
is what says so.

**What guards it now.** `TestWideWidthTypedWritersMatchUint` writes every
`uint16` there is through both `U16` and `Uint` and requires the same bytes, so
the hand-placed window cannot drift from the length computation it stands in
for; the corpus gained `wide.integer widths`, which pins the same boundary
across the two ports. The rule itself is item 3 of `wire/README.md`'s speed
list, with the one-line check —
`go build -gcflags=-m=2 ./wire | grep 'inline (\*Writer'` — because the failure
mode here is silent: the tests pass, the bytes are right, and the record simply
costs three times more to write.

**What is still out of line.** `Writer.Int` and `Writer.I32`. A signed field's
cheapest form is a descriptor *and* a magnitude byte, and a two-argument append
costs four units more than the one-argument append an unsigned nibble needs —
84 against the budget's 80. Left alone rather than contorted: the four units
would have to come out of the append itself, and there is nothing there to cut.
