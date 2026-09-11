# minimal

`github.com/ivanjoz/colbin/minimal`

colbin's **minimal mode**: a byte-aligned `[key][value]` layout for one record of
at most sixteen primitive fields, where a zero-valued field is not written at all.

```go
import "github.com/ivanjoz/colbin/minimal"

w := minimal.Writer{}
w.U32(0, companyID)
w.U16(1, routeID)
w.String(2, name)
send(w.Buffer)

r := minimal.NewReader(message)
for r.More() {
    switch r.Key() {
    case 0:
        companyID = r.U32()
    case 1:
        routeID = r.U16()
    case 2:
        name = r.String()
    default:
        r.Skip() // refuses: an unknown key cannot be stepped over
    }
}
err := r.Err()
```

This package owns the wire format only. It has no reflection and no type
registry — like `varint`, `packed5` and `compact`, it is driven by a caller that
already knows the Go type. `colbin.MarshalMinimal` is the reflection façade over
it.

## Why a third mode

Standard mode transposes records into columns and amortises per-column framing
over many of them. Compact mode drops that framing for one to three records and
writes a bitstream instead. Both still spend **60–90 ns per record** on framing
work — a plan walk, an `ALL_POSITIVE` pre-scan, a transform search across each
array column — which is the right trade when the record is one of hundreds, and
pure loss when it is one record of eight scalars on a hot path.

Minimal mode is for that case: a wire frame, a token, a row key. Nothing is
bit-packed across a byte boundary, so an encode is a header byte and a few
stores, and a decode is a `switch` over the key.

| one 10-field record, 5 fields set | encode | decode | allocs |
|---|---:|---:|---:|
| `minimal.Writer` straight-line, as a generator emits | **5.8 ns** | **18.0 ns** | 0 |
| `MinimalCodec[T]`, plan held in the handle | 17.6 ns | 32.8 ns | 0 |
| `AppendMinimal`, onto the caller's buffer | 32.0 ns | 52.3 ns | 0 |
| `MarshalMinimal` | 62.5 ns | — | 1 |
| compact mode, `Codec[T].Append` | 92.2 ns | 127.8 ns | 0 |

11 bytes against compact mode's 10, and against 20 for the fixed layout the
straight-line row replaces.

Measured on an i7-1355U, Go 1.27, medians of `-count=5`
(`go test ./codec -bench Minimal`). The same record parses in 12 ns in Rust.

The rows are layers, not alternatives: `MarshalMinimal` is the convenient one and
`MinimalCodec[T]` is what a hot path should hold, while the `Writer` underneath
is there for a generated or hand-written codec that wants the last 12 ns. Against
a hand-written *fixed* layout — 2.6 ns to encode, 6.3 ns to decode, 20 bytes —
what minimal mode buys is that no offset appears anywhere, a field can be added
or reordered without shifting another, and a zero field costs nothing.

## Wire format

A message is a sequence of fields and ends when its buffer ends — the frame that
carries it already states its length. Keys are 0..15. Every integer is
big-endian. A field whose value is zero, empty or false is omitted, which is
where most of the saving comes from.

```text
integer        [key:4][positive:1][size:3]                    [magnitude: size bytes]

    size code  0→1B  1→2B  2→3B  3→4B  4→6B  5→8B  6→no bytes, the magnitude is 1
    positive   1 = the bytes are a magnitude; 0 = negative, bytes are |value|
    code 7 is unassigned, reserved for a future extended header
    an integer needs no continuation flag: its size code already reaches 8 bytes

float          the integer shape, carrying the IEEE-754 bit pattern

    the reader knows the field is a float from its key, so nothing new is needed,
    and a pattern with zero high bytes simply costs fewer of them

string/bytes   [key:4][more:1][size:11]                       [bytes: size]
               [key:4][more:1][size:11][+LEB128]              [bytes: size]  (more=1)

    2047 bytes fit the two header bytes; past that the continuation carries
    size>>11, seven bits per byte, low group first

integer array  [key:4][positive:1][width:2][more:1][count:8]  [count × width bytes]
               ... [+LEB128] when more = 1, carrying count>>8

    width code 0→1B  1→2B  2→4B  3→8B, taken from the widest element
    positive   1 = magnitudes; 0 = two's complement at that width

string array   [key:4][more:1][count:11]   then per element [size: LEB128][bytes: size]

    the element length is a varint too: under 128 bytes — which is most short
    strings — it costs one byte rather than two
```

### Nothing has a size ceiling

A header carries a size's low bits and a flag saying whether more follow; when it
is set, LEB128 continuation bytes carry the rest. The common size costs nothing
extra and no size is refused.

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
  need: a continuation run longer than nine bytes, or one describing more than an
  `int` holds, is refused (`ErrSizeTooLarge`) rather than wrapped into a small
  size whose bounds check would then pass. Both ports have a test that walks
  every prefix of a valid message and requires an error rather than a panic.

## What it cannot do

- **Skip an unknown key.** A header sizes a field but does not say which of the
  four layouts it is, so a reader that does not know the key cannot step over it.
  Adding a field is a coordinated deploy of both sides — the same trade compact
  mode makes. Size code 7 is the reserved door if a self-describing variant is
  ever wanted.
- **Carry more than sixteen fields**, or a nested struct, map, pointer or
  interface. A field's whole layout has to follow from its key.
- **Be recognised by `Unmarshal`.** A minimal message has no version byte and no
  mode bit — its first byte is a field header — so `colbin.UnmarshalMinimal` is
  the only way back. `Unmarshal` refuses it.
- **Beat a fixed layout on a record with no zero fields.** It writes a key per
  field; a fixed layout writes none. What it wins back is the zeros, and the
  offsets it does not have.

## Speed, if you are changing it

Three things carry the numbers above, each worth re-measuring after any edit:

1. **Byte alignment.** No bitstream, no cross-byte packing.
2. **Width-typed entry points** — `U16`, `U32` beside the generic `Uint`. A
   `uint64` parameter forces the method to carry all six widths, which pushes it
   past Go's inline budget; a `uint16` can only be one byte or two, so `U16` is
   two appends with no call underneath and it inlines. Using the writer that
   matches each field's Go type took a ten-field encode from 16.9 ns to 5.8 ns.
   The Rust port does the same with `#[inline]` on the narrow reads and
   `#[inline(never)]` on the wide fallback: 19.8 ns → 9.8 ns.
3. **No per-field key check**, as above.

## The Rust port

`rust/src/minimal.rs`, exposed as `colbin::MinimalReader` and
`colbin::MinimalWriter`. Both sides assert the same cross-language vector — a
3701-byte message covering every continued size, pinned by its length, its first
64 bytes and an FNV-1a — so neither can move without the other failing.
