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
        r.Skip() // refuses: a narrow key cannot be stepped over
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
| `Writer` / `Reader` | `narrow.go` | 4 bits, shares the descriptor byte | no | 16 |
| `Writer8` / `Reader8` | `wide.go` | 8 bits, own byte | **yes** | 256 |
| `BitmapWriter` / `BitmapReader` | `bitmap.go` | a presence bitmap, no key at all | yes | 64 |

They are separate types, not a flag, and the duplication is deliberate: a key
width the compiler cannot see is a width it cannot fold, which measured 11.7 ns
against 8.7 on a ten-field decode. The framing is written three times; the value
codecs under it — magnitudes, blobs, array elements — are written once.

Measured on the same ten-field record, five fields set:

| | encode | decode | bytes |
|---|---:|---:|---:|
| narrow key | **5.5 ns** | **14.0 ns** | 11 |
| wide key | 6.2 ns | 24.7 ns | 12 |
| wide key, presence bitmap | 20.9 ns | 39.0 ns | **10** |

The bitmap is the smallest and the slowest: it is for a wire that is size-bound,
and the narrow key is for one that is not.

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

```text
integer        [key:4][positive:1][n:3]                       [magnitude: n bytes]

    n          0 → no bytes, the value is 1 (which is what makes a true bool
               one byte) · 1..6 → that many bytes · 7 → eight bytes
    positive   1 = the bytes are a magnitude; 0 = negative, bytes are |value|
    an integer needs no continuation flag: n already reaches 8 bytes

float          the integer shape, carrying the IEEE-754 bit pattern with its
               bytes reversed

    A float's zero bytes are its low mantissa bytes, where an integer's are its
    high ones, so reversing puts them where n can elide them. 1.0 costs two bytes
    rather than eight, and a float64 holding an exact float32 costs five.

string/bytes   [key:4][more:1][size hi:3] [size lo:8]         [bytes: size]
               [key:4][1][escape:3]       [size: 2|4|8]       [bytes: size]

    2047 bytes fit the two header bytes. Past that the three size bits carry
    nothing, so they name the width of the size that follows instead: a 5 KB
    string pays one extra byte, not four.

integer array  [key:4][positive:1][width:2][more:1] [count:8] [count × width]
               ... [count: 4 bytes] instead, when more = 1

    width code 0→1B  1→2B  2→4B  3→8B, taken from the widest element
    positive   1 = magnitudes; 0 = two's complement at that width

string array   [key:4][more:1][count:11]   then per element
               [size: 1 byte, 0xFF → 4 bytes follow] [bytes: size]

    under 255 bytes — which is most short strings — an element length costs one
    byte rather than two
```

### Nothing has a size ceiling, and nothing is a varint

Every size escalates to a width that holds it, named by the header rather than
discovered byte by byte. The common size costs nothing extra, no size is refused,
and **no read is ever a loop whose trip count is data** — which is also why there
is no continuation run for an unauthenticated peer to make unbounded. What is
left to refuse is a size larger than this platform can address.

That is also why **a write cannot fail**: `Writer` has no error and no `Err`
method. Anything the format could not express would have to be a value that does
not fit in memory.

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

- **Skip an unknown key.** A header sizes a field but does not say which of the
  four layouts it is, so a reader that does not know the key cannot step over it.
  Adding a field is a coordinated deploy of both sides — the same trade compact
  mode makes. The self-describing variant is the 8-bit key width, which carries a
  class in its descriptor; see `BYTE_ALIGNED_PLAN.md` §2.3.
- **Carry more than sixteen fields**, or a nested struct, map, pointer or
  interface. A field's whole layout has to follow from its key.
- **Be recognised by `Unmarshal`.** A narrow message has no version byte and no
  mode bit — its first byte is a field header — so `colbin.UnmarshalMinimal` is
  the only way back. `Unmarshal` refuses it.
- **Beat a fixed layout on a record with no zero fields.** It writes a key per
  field; a fixed layout writes none. What it wins back is the zeros, and the
  offsets it does not have.

## Speed, if you are changing it

Five things carry the numbers above, each worth re-measuring after any edit:

1. **Byte alignment.** No bitstream, no cross-byte packing, and no varint.
2. **Width-typed entry points** — `U16`, `U32` beside the generic `Uint`. A
   `uint64` parameter forces the method to carry all seven widths, which pushes
   it past Go's inline budget; a `uint16` can only be one byte or two, so `U16`
   is two appends with no call underneath and it inlines. Using the writer that
   matches each field's Go type took a ten-field encode from 16.9 ns to 5.5 ns.
3. **Both of a type's widths inline, not just the narrow one.** `Reader.U16`
   handled one byte inline and sent two bytes to a call — but a `uint16` holding
   something above 255 is what the type is *for*. Inlining the second width, with
   `More` no longer testing an error the parked cursor already answers, took a
   ten-field decode from 19.2 ns to 13.9.
4. **No pointer in a generic signature.** `WriteInts[T](w *Writer, ...)` is
   reached through a shape dictionary, and the escape information a caller in
   *another package* gets for it is conservative enough to heap-allocate the
   writer. That is why there is a concrete `Int32s`, `Uint16s` and so on beside
   the generic form, and why the generic work below them takes slices and values
   only. Getting the pointer out was worth an allocation and 3 ns per record on
   codec's plan walk.
5. **No per-field key check**, as above.

## The Rust port

`rust/src/narrow.rs`, exposed as `colbin::MinimalReader` and
`colbin::MinimalWriter`.

> **Stale.** The Rust port still implements the previous wire — big-endian, the
> old size codes, LEB128 continuations — and its tests pass because it asserts
> its own copy of the cross-language vector rather than a shared one. The two
> ports have silently diverged and must not be used against each other until the
> port is brought over. That the divergence is silent is itself a defect: the
> vector belongs in `rust/vectors/`, read by both sides, like the other modes'.
