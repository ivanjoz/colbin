# compact

`github.com/ivanjoz/colbin/compact`

colbin's **compact mode**: a bit-level wire format for a single struct, or an
array of at most three, where the standard columnar mode has nothing to amortise
its per-column framing over.

```go
import "github.com/ivanjoz/colbin/compact"

w := compact.NewWriter(nil, compact.ShapeStruct, true, compact.Keys8)
w.Key(0x35); w.Int(1234)
w.Key(0x9a); w.Str("Usuario1")
w.End()
buf := w.Done()

r, err := compact.NewReader(buf)
for r.Err() == nil {
    k := r.Key()
    if k == compact.TerminatorKey { break }
    // dispatch on k using the caller's type information
}
```

This package owns the wire format only. It has no reflection and no type
registry — like `varint` and `packed5`, it is driven by a caller that already
knows the Go type.

## Mode discrimination

Bit 0 of byte 0 selects the format, and the two branches share nothing else:

```
bit 0 == 0    standard mode: the byte is a version byte, columnar layout
bit 0 == 1    compact mode: the remaining bits are this package's header
```

A compact message has no version field, no record-count varint and no column
count, because it needs none of them.

## Header

Five bits, after which the message is one LSB-first bitstream with **no
alignment anywhere** until a final pad to a byte boundary.

| bits | field | meaning |
|---|---|---|
| 0 | mode | always `1` |
| 1 | `ALL_POSITIVE` | every signed integer in the message is `>= 0` |
| 2-3 | shape | `0` lone struct · `1`/`2`/`3` array of that many records |
| 4 | `NARROW_KEYS` | field ids are 4 bits wide rather than 8 |

Shape `0` and shape `1` both carry one record; they differ only in whether it
renders as an object or an array of one, which the binary path takes from the
destination Go type but a JSON reader needs told.

## Records

A record is a run of `[key][value]` pairs closed by the terminator key. A field
holding its zero value is **omitted entirely** — the key run *is* the presence
information, so an absent field costs nothing beyond the terminator the record
already owes.

Keys are on the wire so a reader can resolve fields by id rather than position,
and so it can `Skip` a field its type does not know.

## Key width

A key is 8 bits by default — the standard mode's field id unchanged, with `255`
closing the record, which is the id that mode already reserves. When every id a
message writes is `MaxNarrowKey` (14) or less, the header's `NARROW_KEYS` bit
selects a **4-bit key** instead, with `15` closing the record: the same
arrangement one nibble down.

| | key | terminator | ids |
|---|---|---|---|
| `Keys8` | 8 bits | 255 | 0..254 |
| `Keys4` | 4 bits | 15 | 0..14 |

A record of `f` present fields spends `4*(f+1)` fewer bits — the fields plus the
terminator — against one bit for the whole message. It pays for itself on the
first field of the first record, and at seven fields it is four bytes:

| 7-field struct, one record | bytes |
|---|---|
| standard mode | 35 B |
| compact, `Keys8` | 21 B |
| compact, `Keys4` | **17 B** (−19%) |

**Ids decide it, not the field count.** A `cb` tag that only renames a field
leaves its id to an FNV hash of the name, which lands anywhere in 0..254 and puts
the whole type back on the wide key. Narrow keys want the ids written out:

```go
type SaleOrderProductStats struct {
    Quantity                int32 `cb:"1,minimal"`
    QuantityPendingDelivery int32 `cb:"2"`
    SubQuantity             int16 `cb:"3"`
    ...
}
```

`Reader.Key` reports a narrow terminator as `TerminatorKey` rather than as 15, so
the loop that closes a record is written once and reads the same at either width.
Nothing is lost by folding them: `Keys4` reserves 15 exactly as `Keys8` reserves
255, so no field can hold it. In the other direction `Writer.Key` **panics** on an
id above `MaxNarrowKey` under `Keys4` — truncating it would silently rename the
field or forge a terminator, and the writer has no error to return mid-message.

The flag is in the header rather than derived from the caller's type on both
sides, which would have cost nothing at all. A derived width breaks the moment
the two ends disagree about the type: a service that adds a field with a large id
would start reading its own older narrow messages at the wrong width and decode
garbage, where the header bit makes that message decode correctly and any genuine
id mismatch report itself as one. One bit is the price of that, and it is only
ever visible when it pushes the final pad into another byte.

## Integers

`ALL_POSITIVE` decides how a signed value becomes its unsigned payload: with the
flag set the payload is the **magnitude**, without it the **zigzag**. That is
worth one payload bit on every integer in the message. Determining it needs a
pass over the whole message before the first bit is written, which is free at
three records and is why it is a constructor parameter rather than inferred.

The payload then goes through a varint whose first unit is one bit narrower than
LEB128's, spending that bit on a selector:

```
unit 0    [selector:1] [cont:1] [payload:6]   (+ [payload:4] if selector)
unit i    [cont:1] [payload:7]
```

`cont` says another unit follows. The nibble is a **one-time offset on unit 0**,
not a per-unit addition, so there are two capacity ladders — `6+7k` without it
and `10+7k` with it, at sizes `8+8k` and `12+8k` bits — and the encoder takes
whichever reaches the value first.

```
bits      0-6    7   8-10  11-13   14  15-17  18-20   21  22-24
LEB128      8    8     16     16   16     24     24   24     32
compact     8   12     12     16   20     20     24   28     28
```

Three ties, three wins of four bits, one loss of four, per seven bit-lengths.
Over bit-lengths 0..64 that is **25 wins, 9 losses, 31 ties**.

The selector costs nothing outright: it takes unit 0's seventh payload bit,
which any value wider than six bits was going to spill into a second unit
anyway. The loss falls only where LEB128's first byte was exactly full — `b`
divisible by 7.

## Values

| kind | encoding |
|---|---|
| key | 8 bits, or 4 with `NARROW_KEYS` |
| int | varint over magnitude or zigzag, per `ALL_POSITIVE` |
| uint | varint over the value; never zigzagged |
| bool | **1 bit** |
| float32 / float64 | raw IEEE-754, 32 or 64 bits |
| string | a `packed5` frame, self-delimiting, no length alongside it |
| bytes | varint length, then the bytes |
| `[]int8/16/32/64` | element count, then `varint.AppendArray` |
| `[]string` | element count, then consecutive `packed5` frames |
| `[]float32/64` | element count, then raw IEEE-754 elements |
| `[]bool` | element count, then a bitmap, one bit each |

`packed5` is byte-structured, so a string frame simply starts wherever the
bitstream happens to be. When the reader is byte aligned the underlying buffer
passes straight through with no copy; otherwise the remaining bytes are shifted
into a reused scratch buffer, one pass per string field. Compact messages hold
at most three records, so that remainder is bounded by a small constant.

## Arrays of primitives

An array field uses **exactly the codec the standard mode uses**, so it costs the
same in either mode. Compact mode changes how records are framed, not how a
column of values is compressed: a run of correlated ids gets varint's delta
transform here just as it would in a columnar message.

| n (sorted int32) | compact array field | bare columnar codec |
|---|---|---|
| 4 | 8 B | 7 B |
| 16 | 20 B | 19 B |
| 100 | 104 B | 103 B |
| 1000 | 1004 B | 1003 B |

The one-byte gap is the element count. Encoding those same values one at a time
through the compact varint would take 2500 B at n=1000 — the delta transform is
the whole difference, and it is why the array codec must not be replaced by
per-value encoding just because the record framing changed.

`PutInts`/`GetInts` are generic over `varint.Signed`. The element width comes
from the Go type on both sides and never reaches the wire, so the two must be
instantiated with the same `T`. `int` is excluded for varint's reason: its width
is platform dependent, so an `[]int` written on a 64-bit host would decode
silently wrong on a 32-bit one. Unsigned slices convert through the same-width
signed type, preserving the bit pattern, exactly as `codec/integer.go` does.

`[]bool` is the one form that departs from the standard mode, which carries bools
through an integer column. Compact mode already spends a single bit on a scalar
bool, so a slice becomes a bitmap.

## Not supported

There is no kind for a **nested struct**, an **array of structs**, a **map** or
an `interface{}`. A type containing any of those must use the standard mode.

Arrays of structs are the meaningful exclusion, and the reason is the premise
compact mode rests on. A single record has nothing to amortise per-record framing
over — that is why compact mode exists — but a struct holding an array of 100
sub-structs contains a hundred records' worth of columnar-friendly data, and the
premise no longer holds. A *singular* nested struct is a different case, since
inlining its fields costs exactly what having them at the top level costs, and
would be a reasonable extension.

Because a slice's length is not in its type — `[]int32` may hold two elements or
a hundred thousand — a caller choosing between the modes needs a value check at
encode time, not only a type filter.

The final pad is never read back: a record ends at its terminator and the
message ends at its last record, so the decoder stops before reaching it. No
terminator bits are needed at the message level.

## Sizes, measured

Against the standard mode and `encoding/json`, the same `Usuario` records. Their
`cb` tags name their fields rather than number them, so the ids are hashed and
the keys are wide:

| | standard | compact | json |
|---|---|---|---|
| n=1 | 34 B | **25 B** (−26%) | 76 B |
| n=1, three fields zero | 28 B | **13 B** (−54%) | — |
| n=2 | 50 B | 50 B (tie) | 154 B |
| n=3 | 62 B | 65 B (**+5%**) | 220 B |

**Compact mode loses at n=3 on hashed ids.** Keys are the reason: compact pays 6
bytes of framing per record (5 keys + terminator) where the columnar mode pays
its 5 keys once and amortises them. That is the deliberate trade — keys buy `Skip` and
resolution by id, and they are worth it exactly while there is one record to
carry them.

Numbering those same five fields `cb:"1"`..`cb:"5"` halves that framing, and the
trade changes shape — the crossover moves past n=3 entirely:

| | standard | compact, hashed ids | compact, `cb:"1"`.. |
|---|---|---|---|
| n=1 | 34 B | 25 B | **22 B** (−35%) |
| n=1, three fields zero | 28 B | 13 B | **11 B** (−61%) |
| n=2 | 50 B | 50 B | **44 B** (−12%) |
| n=3 | 62 B | 65 B | **57 B** (−8%) |

Keys are still 6 per record against the columnar mode's 5 once, but at half width
that is 3 bytes a record instead of 6, and the columnar mode's type bytes no
longer buy it back inside three records.

So: **use compact unconditionally at n=1, and at n=2-3 encode both and keep the
smaller.** At these sizes the double encode costs a few hundred nanoseconds and
makes the mode bit self-tuning rather than a guess.

## Reuse

`Writer.Reset` and `Reader.Reset` re-aim an existing writer or reader at another
message, which is what makes both poolable. What they keep is the scratch buffer
each grew: the writer's holds the `packed5` frame and the `varint` array it
builds before shifting into the bitstream, and the reader's holds the bytes it
shifts an unaligned frame into. Those are the allocations a per-message
writer pays again every time.

`Reset(out, ...)` appends to `out` exactly as `NewWriter` does, so a caller
passing `buf[:0]` reuses its own buffer and drops the last allocation with it.
`Reader.Reset` releases the previous message even when it rejects the new one, so
`Reset(nil)` is how a pooled reader lets go of what it just read.

`codec` drives both from `sync.Pool`, which is why encoding a record through a
`colbin.Codec[T]` onto a buffer the caller keeps allocates nothing at all.

## Errors

Reads are **sticky**: once one runs past the end of the stream, every later read
returns a zero value and leaves the error set, so a caller may decode a whole
record and check `Err` once rather than after every field.

Corrupt input can never panic or read out of bounds. Declared lengths are
compared against the bits actually remaining *before* being converted or used to
size an allocation — a varint naming a length near 2^64 would otherwise overflow
`int` back into a value that passes a naive bounds check.

## Files

| file | role |
|---|---|
| `compact.go` | format constants, `Shape`, errors, `IsCompact` |
| `bitstream.go` | LSB-first bit writer/reader, bounds-checked, `alignedTail` |
| `varint.go` | the two-ladder selector varint |
| `array.go` | arrays of primitives, delegating to `varint` and `packed5` |
| `writer.go` | `Writer` |
| `reader.go` | `Reader`, `Skip`, `Kind` |
