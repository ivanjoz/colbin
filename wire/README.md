# wire

`github.com/ivanjoz/colbin/wire`

colbin's wire format: a byte-aligned `[key][descriptor][payload]` layout where a
zero-valued field is not written at all.

```go
import "github.com/ivanjoz/colbin/wire"

w := wire.Writer{}          // four-bit keys
w.U32(0, companyID)
w.U16(1, routeID)
w.String(2, name)
send(w.Buffer)

r := wire.NewReader(message)
for r.More() {
    switch r.Key() {
    case 0:
        companyID = r.U32()
    case 1:
        routeID = r.U16()
    case 2:
        name = r.String()
    default:
        r.Skip() // a field this reader does not know: step over it
    }
}
err := r.Err()
```

This package owns the field framing and nothing else. It has no reflection and
no type registry — like `column` and `packed5`, it is driven by a caller that
already knows the Go type. `codec` is the reflection façade over it, and
`codec.Generate` emits source that drives it directly.

## Three framings of a key run

| | file | key | skip | fields |
|---|---|---|---|---|
| `Writer` / `Reader` | `narrow.go` | 4 bits, shares the descriptor byte | **yes** | 16 |
| `Writer8` / `Reader8` | `wide.go` | 8 bits, own byte | **yes** | 256 |
| `BitmapWriter` / `BitmapReader` | `bitmap.go` | a presence bitmap, no key at all | yes | 64 |

They are separate types, not a flag, and the duplication is deliberate: a key
width the compiler cannot see is a width it cannot fold, which measured 11.7 ns
against 8.7 on a ten-field decode. The framing is written three times; the value
codecs under it — magnitudes, blobs, array elements — are written once.

Measured on the same ten-field record, five fields set:

| | encode | decode | bytes |
|---|---:|---:|---:|
| narrow key | **5.3 ns** | **14.8 ns** | **9** |
| wide key | 5.4 ns | 25.4 ns | 11 |
| wide key, presence bitmap | 21.4 ns | 44.4 ns | 10 |

The two key widths now cost the same to write — what the wide one buys is paid
for on the read and in the byte count, not at the writer. The bitmap is the
slowest of the three and only wins on size when most fields are present: ten of
ten it is 15 bytes against the narrow key's 18, and five of ten it is a byte
larger. It is for a wire that is size-bound and full; the narrow key is for
every other one.

## Composites

`composite.go` adds a nested key run, a list and a map; `table.go` adds the
columnar shape as a field class rather than a mode. Every one carries a byte
length, which is what makes a field skippable without its sub-schema.

A table's body is a key run whose fields are *columns* of `rowcount` values. On a
four-field row it overtakes a list of structs at three rows and is 2.6× smaller
by four thousand.

## Wire format

A message is a sequence of fields and ends when its buffer ends — the frame that
carries it already states its length. Keys are 0..15. Every multi-byte quantity
is little-endian, which is one native load on every machine this runs on. A field
whose value is zero, empty or false is omitted, which is where most of the saving
comes from.

The nibble beside the key says how long the field is, whatever its type; the type
says only what the bytes mean. That is what lets `Skip` step over a key it does
not know.

```text
size           [key:4][nibble:4]
                   nibble 0..3    no payload
                   nibble 4..11   nibble−3 bytes
                   nibble 12..15  [1 1 f1 f0] [length] [bytes: length]
                   length         one byte ≤ 253 · 0xFE then a u16 · 0xFF then a u32

unsigned       0..3 the value 1..4 · 4..11 the value · 1100 with length 0 a zero

signed         0..2 the value 1..3, 3 is −1 · 4..11 a positive value
               · 1100 the magnitude of a negative one (length 0: a zero)

float          the unsigned shape, carrying the IEEE-754 bit pattern with its
               bytes reversed

    A float's zero bytes are its low mantissa bytes, where an integer's are its
    high ones, so reversing puts them where the byte count can elide them. 1.0
    costs two bytes rather than eight, and a float64 holding an exact float32
    costs five.

string/bytes   4..11 one to eight raw bytes, and no length at all
               · 11 f1 f0: 00 raw, 01 / 10 packed5 opening lower / upper case

integer array  11 f1 f0, f1f0 the element width: 0→1B 1→2B 2→4B 3→8B
               count = length >> f1f0 · an unsigned type's elements are
               magnitudes, a signed type's two's complement

string array   1100, then per element
               [size: 1 byte, 0xFF → 4 bytes follow] [bytes: size]

    no count: the reader counts the sizes it walks anyway

composite      11 0 f0 [length] [body] · f0: a struct's k8, or 1 for a table
```

A signed field's positive values cost what an unsigned field's do; a negative
other than −1 costs a byte more, which is the price of the length form being
where it is. `INTERNALS.md` §3 is the specification and `RATIONALE.md` has the
measurements that chose this layout over four others.

### Nothing has a size ceiling, and nothing is a varint

Every size escalates to a width that holds it, named by the header or by the
length's first byte rather than discovered byte by byte. The common size costs
nothing extra and **no read is ever a loop whose trip count is data** — which is
also why there is no continuation run for an unauthenticated peer to make
unbounded. What is left to refuse is a size larger than this platform can
address.

That is also why **a write does not fail**: `Writer` has no error and no `Err`
method. The one thing a narrow field cannot express is a payload of 4 GiB or
more, which is a wide field's job.

### A write trusts, a read does not

The reader defends against the network; the writer trusts its own program.

- The writer checks **nothing** — not the key, not the size. A key is a constant
  of the record definition rather than data, and checking `key < 16` once per
  field measured 8 ns of a 25 ns ten-field encode. A key above fifteen shifts
  into the next field's bits and writes a record nothing can read back, so assert
  it where the constants live:

  ```go
  const _ = uint(15 - chargeKeyAccess4) // fails to build if a key exceeds 15
  ```

- The reader bounds-checks every read, and carries two guards the writer does not
  need: a size describing more than an `int` holds is refused
  (`ErrSizeTooLarge`) rather than wrapped into a small size whose bounds check
  would then pass, and an unassigned escape code is refused (`ErrBadEscape`)
  rather than guessed at. There is a test that walks every prefix of a valid
  message and requires an error rather than a panic.

## What it cannot do

- **Say what an unknown field was.** A narrow reader can step over a key it does
  not know, but not read it: the bytes mean what the schema's type says. Turning
  a message into JSON without the Go type takes the schema section
  (`INTERNALS.md` §10); the wide width carries a class in each descriptor
  (`INTERNALS.md` §4).
- **Carry more than sixteen fields**, or a dynamic value. Past sixteen a type
  takes eight-bit keys, and `any` is wide-only.
- **Beat a fixed layout on a record with no zero fields.** It writes a key per
  field; a fixed layout writes none. What it wins back is the zeros, and the
  offsets it does not have.

## Speed, if you are changing it

Six things carry the numbers above, each worth re-measuring after any edit:

1. **Byte alignment.** No bitstream, no cross-byte packing, and no varint.
2. **Width-typed entry points** — `U16`, `U32` beside the generic `Uint`. A
   `uint64` parameter forces the method to carry all seven widths, which pushes
   it past Go's inline budget; a `uint16` can only be one byte or two, so `U16`
   is two appends with no call underneath and it inlines. Using the writer that
   matches each field's Go type took a ten-field encode from 16.9 ns to 5.3 ns.
3. **Every integer writer stays under the inliner's budget**, which is 80 units
   and of which a call costs 57. So a writer may hold its inline-value case, its
   omit-zero test and *one* call, and no more — which is why the omit-zero test
   is folded into the inline comparison by the wrap `value-1 < uintInlineMax`,
   why the cold half sits behind `//go:noinline`, and why `Writer8.U16` decides
   the varint against a constant rather than measuring both forms. A writer that
   drops out sends **every** field of the record through a call, not only the
   wide one: that is what made the wide key 18.9 ns to write against 5.4 now.
   `go build -gcflags=-m=2 ./wire | grep 'inline (\*Writer'` is the check, and
   the boundary that spelling hard-codes is asserted by
   `TestWideWidthTypedWritersMatchUint` for every `uint16` there is.
4. **Both of a type's widths inline, not just the narrow one.** `Reader.U16`
   handled one byte inline and sent two bytes to a call — but a `uint16` holding
   something above 255 is what the type is *for*. Inlining the second width, with
   `More` no longer testing an error the parked cursor already answers, took a
   ten-field decode from 19.2 ns to 13.9.
5. **No pointer in a generic signature.** `WriteInts[T](w *Writer, ...)` is
   reached through a shape dictionary, and the escape information a caller in
   *another package* gets for it is conservative enough to heap-allocate the
   writer. That is why there is a concrete `Int32s`, `Uint16s` and so on beside
   the generic form, and why the generic work below them takes slices and values
   only. Getting the pointer out was worth an allocation and 3 ns per record on
   codec's plan walk.
6. **No per-field key check**, as above.

## The Rust port

`rust/src/wire/`, which mirrors this package file for file: `narrow.rs`,
`wide.rs` and `bitmap.rs` are `Writer`/`Reader`, `Writer8`/`Reader8` and
`BitmapWriter`/`BitmapReader`, and the composites, the table and the packed
string sit where they do here.

It is pinned to this side rather than to its own copy of anything.
`rust/vectors/main.go` writes the corpus with these packages and
`rust/tests/vectors.rs` asserts both directions against it — Go's bytes decoded
to Rust's values, and Rust's values encoded to Go's bytes — so a change here
fails there. `go test ./rust/vectors` fails if the committed corpus is stale.

> **One thing to know when changing `arrayPlanOf`.** `isSigned[uint64]()`
> answers **true**: `-1` converted to a `uint64` and read back through `int64`
> is still −1. So a `[]uint64` travels as two's complement where a `[]uint32`
> travels as magnitudes — which is lossless in both directions, and usually
> smaller, but is not what the comment beside the test says it is doing. The
> Rust side mirrors the behaviour rather than the comment, because the wire is
> what this package writes.
