# colbin internals

How colbin is built: the bytes on the wire, the Go encoder and decoder that
define them, the Rust port, the browser module and the npm package around it, and
how the three are kept in step. This is a description of the code as it is. The
reasons behind the decisions, and the alternatives that were measured and
dropped, are in `RATIONALE.md`.

Bits are numbered 7 (most significant) to 0. Every multi-byte number on the wire
is little-endian. A field's **id** is the number its tag gives, counted from one;
its **key** is what goes on the wire, `id − 1`.

---

## 1. The repository

| path | what it is |
|---|---|
| `colbin.go`, `doc.go` | the public Go API: one-line forwards to `codec`, plus type aliases |
| `codec/` | the Go encoder, decoder, schema section, JSON walk and code generator |
| `wire/` | the field-level readers and writers for both key widths |
| `column/` | the blocked integer column codec |
| `packed5/` | the opt-in 5-bit string codec |
| `corpus/` | a seeded generator of realistic records, for sizes and benchmarks |
| `rust/` | the `colbin` crate: the same format in Rust, plus a JSON→colbin encoder |
| `rust/derive/` | `#[derive(Colbin)]` |
| `rust/wasm/` | the browser module: a `cdylib` with a C ABI over the crate |
| `js/` | the npm package `colbin`, which wraps the module |
| `web/` | the demo site, which consumes `js/` as a workspace package |
| `rust/vectors/`, `js/vectors/` | Go programs that write the cross-language corpora |

Go is the specification. The Rust crate reads everything Go writes except where
§20 says otherwise, and the browser module is that crate compiled to WebAssembly.

---

## 2. The message

```
message := [root:1] [schema section]? [body]
         | [narrow key run]          a typed narrow message: no root (§2.1)
```

A message has no length of its own; it ends where the buffer that delivers it
ends.

### 2.1 The root byte

The root byte is an ordinary wide STRUCT descriptor (§4.2), `1 101 dddd`, in
**0xD0..0xDF** (`codec/root.go`), and it says what the message is.

**A typed narrow message has none.** A struct at four key bits, with no schema
section and no dictionary, is most messages, and its key run is the whole
message; an empty message is the record with every field zero. A reader tells
it from the rest by the first byte: one in the range is a root byte, anything
else a key run's first field. A key run can begin in the range — a first field
of key 13 has a header of `0xD_` — and that message keeps `0xD0` in front, which
a reader steps over as it would any root byte (`wire.FinishNarrow`). Only a
record whose keys 0 to 12 are all zero and whose key 13 is not pays it.

The four detail bits are assigned:

| bit | mask | meaning |
|---|---|---|
| 3 | 0x08 | the body's key run uses eight-bit keys |
| 2 | 0x04 | a schema section (§10) comes before the body |
| 1 | 0x02 | a dictionary section (§6.3) comes before the body — prototype |
| 0 | 0x01 | the body is a dynamic document, not a key run (§9.1) |

A dynamic document has no key width and no dictionaries, so under bit 0 the
wide bit is unassigned and bit 1 says which document it is. The root bytes are:

| byte | body |
|---|---|
| none | narrow keys: the message is the key run |
| `0xD0` | narrow keys, written only before a key run that begins in the range |
| `0xD8` | wide keys |
| `0xD4` | schema section, then narrow keys |
| `0xDC` | schema section, then wide keys |
| `0xD2` | dictionary section, then narrow keys |
| `0xDA` | dictionary section, then wide keys |
| `0xD1` | a `map[string]any`: its entries, compact, to the end of the message |
| `0xD3` | a `[]any`: one compact value |
| `0xD5`, `0xD7` | the same behind a schema section |

and a reader refuses the other values in the range rather than guess at them.
A grouped table (§6.3) says so in its own descriptor and nowhere else.

A reader dispatches the first four in one switch and reaches the dictionary
roots only through the branch that would refuse the byte, so a message without
dictionaries is read exactly as it was before they existed.

The other 240 first bytes are a typed narrow message's. They used to be left to
an application's own framing; they are not any more, and a stream that carries
colbin beside other data tags it itself. The byte was what protobuf won by on a
document of a few dozen bytes. `colbin.IsColbin` is a range check on
0xD0..0xDF, so it says whether a message has a root byte, not whether it is
colbin (`RootFirst`/`RootLast`). There is no version byte: an unassigned root detail is
how a later version would announce itself, and an older reader refuses it. Every
detail bit is assigned, so the next change an older reader has to refuse needs
another door — a combination no writer produces, such as the dictionary and
schema bits together, a dynamic root with the wide bit, or SPECIAL `0xF6` inside
the body.

A reader decodes the body at the width the root byte declares, or at four bits
when there is none, not the width its own type would have chosen.

### 2.2 What can be at the root

The body is a struct's key run, or a dynamic document: a `map[string]any` or a
`[]any` at the root is the document itself (§9.1), which costs the root byte and
nothing else. Any other slice or map at the root is carried in
an **envelope**: a one-field struct whose field is the value, under key 0, named
`rows` in the schema section and flagged as an envelope there (§10). The JSON walk
unwraps it, so the document is the array or the object rather than a wrapper
holding it (`codec/envelope.go`). The envelope costs two bytes and no copy — the
field is at offset 0, so the caller's pointer is used as the record.

An array or a bare scalar is refused at the root.

---

## 3. Narrow key runs (K4)

A narrow field is one byte of header and a payload:

```
[key:4 | nibble:4] [payload]
```

Keys are 0..15, so a narrow run holds at most sixteen fields (`wire.MaxFields`).

### 3.1 The nibble sizes the field

Four bits have no room for a type, so the nibble does the one thing a reader needs
before it knows the type: it says **how long the field is**.

| nibble | payload |
|---|---|
| `0..3` | none |
| `4..11` | exactly `nibble − 3` bytes, 1..8 |
| `12..15`, `1 1 f1 f0` | a length, then that many bytes |

A length is one of four forms:

| first byte | length |
|---|---|
| `0x00–0xEF` | the byte itself, 0..239 |
| `0xF0–0xFD` | two bytes: `240 + ((first − 0xF0) << 8 \| second)`, 240..3823 |
| `0xFE` | a u16 follows |
| `0xFF` | a u32 follows |

Writers use the shortest form; readers accept any. The two-byte form is what a
varint spends on a body of a few hundred bytes, where a u16 escape spent three.
It costs the bodies of 240 to 253 bytes a second byte. The flag bits `f1 f0`
mean something different per type and never change the size.

The type decides only what the bytes **mean**, so a narrow reader **steps over a
key it does not know** exactly as a wide one does (`Reader.Skip`: one branch and,
for the length form, one length read). A type can drop a field, or gain one, at
any id and still read what was written before. The cost of that is one bit of the
old nibble, measured in `RATIONALE.md`.

### 3.2 Values

| type | `0..3` | `4..11` | `1 1 f1 f0` |
|---|---|---|---|
| unsigned, bool, float | the value `nibble + 1`, 1..4 | the value, little-endian | `1100` with length 0: an explicit zero |
| signed | 1, 2, 3; nibble 3 is −1 | a positive value | `1100`: the magnitude of a negative value, 1..8 bytes; length 0 is an explicit zero |
| string, blob | refused | 1..8 raw bytes | `f1 f0`: `00` raw, `01` packed5 opening in lower case, `10` packed5 opening in upper case (§8), `11` refused |
| integer array | refused | refused | `f1 f0` is the element width: 0 → 1 byte, 1 → 2, 2 → 4, 3 → 8. The length is a multiple of it and the count is `length >> f1f0` |
| string array | refused | refused | `1100`: the elements, each `[size][bytes]`, size one byte up to 254 or `0xFF` and a u32. There is no count: it is the number of elements the length holds. `1110`: one element, its bytes alone |

Values use the fewest bytes that hold them, and a string of 1..8 bytes carries no
length at all. The writer omits a field holding its zero value, so an absent key
means zero; bool `true` is nibble 0. A float rides as its byte-reversed IEEE bits in
the unsigned form (§5.1).

A signed field's positive values are unsigned magnitudes, so they cost what an
unsigned field's do; a negative value other than −1 rides the length form and
costs one byte more than its magnitude would. A value wider than the field's type
is refused (`ErrFieldTooWide`), never truncated.

An integer array of an unsigned type holds magnitudes; one of a signed type holds
two's complement at the narrowest width that holds every element, sign-extended on
read. A reader working from a schema section takes the signedness from the op, so
a `[]uint64` is magnitudes like every other unsigned array.

**Explicit zeros.** A non-nil pointer to a zero value has to be told apart from
nil (§6.6), so it is the one place a zero is written, and every type writes it the
same way: `[key | 1100] 00`, the length form with nothing in it (`Zero`).

A float is zero only when its bits are, so a pointer to `-0.0` writes the value
normally.

### 3.3 Composites

A narrow composite is the length form with a body:

```
[key:4 | 1 1 f1 f0] [length] [body]
```

| composite | `f1 f0` | body |
|---|---|---|
| struct | `0` k8: the nested run's own key width | a key run of exactly `length` bytes |
| list | `00` | `[count]` then `count × ( [elementLength] [key run] )` |
| list of one | `10` | the element's key run, with no count and no element length |
| table | `01` | `[rows]` then the column run (§6.3) |
| map | `00` | `[count]` then `count × ( key element, value element )` |
| map of one | `10` | the entry, with no count |

A list and a table are both a `[]Struct`, which is why one bit tells them apart; a
struct and a map are other Go types, so their flags overlap nothing. `f1 = 1` is a
list or a map of one element, a table whose rows are in groups (nibble `1111`,
§6.3) and, in a table's column run, a nullable column (§6.6); a struct refuses
it. A table's string column is always the counted form, because there `f1` is
taken.

A list of one is what a configuration document's lists mostly are — one
`extends`, one rule, one job — and protobuf writes one as a tag, a length and
the element. The counted form spent a count and an element length on top.

A count is one byte up to 254, or `0xFF` and a u32. A list element carries a
length and **no descriptor**: its shape is the schema's to know, which is a byte
saved per element and the reason a narrow list of small structs is smaller than a
wide one. Its length is one byte up to 254, or `0xFF` and a u32.

**Backpatching.** A composite's length is not known until its body is written, so
the writer reserves one byte, writes the body and patches it (`Close`). A body of
up to 239 bytes fits the reserved byte; a longer one moves up by one (the
two-byte form), two (`0xFE` and a u16) or four (`0xFF` and a u32). A list element
has no header and keeps its own escape (`CloseElement`). Sizing a value before
writing it would cost a pass over every nested value; a backpatch costs a
`memmove` only for bodies past 239 bytes.

A narrow field holds at most 2³² − 1 bytes. A larger blob is a wide field's job.

---

## 4. Wide key runs (K8)

```
[key:8] [descriptor:8] [payload]
```

Keys are 0..255, so a wide run holds 256 fields (`wire.MaxWideFields`). The
descriptor names a class, which is what lets a wide reader **step over a key it
does not know**: every field is sized by its descriptor alone. That is the reason
the wide width exists, and why a type that has to evolve past its readers uses it.

### 4.1 The descriptor

```
0 v v v v v v v      inline: the value 0..127, no payload
1 c c c d d d d      explicit: class ccc, detail dddd
```

| class | bytes | detail | payload |
|---|---|---|---|
| 0 INT | 0x80–0x8F | `[pos:1][n:3]`: `n = 0` means the magnitude is 1, 1..6 that many bytes, 7 eight bytes; `pos = 0` makes it negative | the magnitude |
| 1 BLOB | 0x90–0x9F | `[0 0 lw:2]` raw; `[0 1 up lw:1]` packed5, opening in upper case when `up`, `lw` a 1- or 2-byte size; `[1 n:3]` raw, `n + 1` bytes and no size | the size, then the bytes |
| 2 VEC | 0xA0–0xAF | `[w:2][pos:1][lw:1]`; `lw` 0 a one-byte, 1 a four-byte byte length | the elements; the count is `length >> w` and never stored |
| 3 COL | 0xB0–0xBF | `[—:2][lw:2]` | the length, then a column (§7) |
| 4 LIST | 0xC0–0xCF | `[homog:1][—:1][lw:2]` | `[length][count]` then the elements |
| 5 STRUCT | 0xD0–0xDF | `[k8:1][—:1][lw:2]` | `[length]` then a key run at the width `k8` names |
| 6 MAP | 0xE0–0xEF | `[sub:1][grp:1][lw:2]`; `sub = 1` is a table, and `grp = 1` a table in groups (§6.3) | `[length][count or rows]` then entries or columns |
| 7 SPECIAL | 0xF0–0xFF | below | varies |

`lw` selects a length of 1, 2, 4 or 8 bytes, except in VEC where its single bit
selects 1 or 4. No length anywhere is a varint: reading one is a branch and a
fixed-width load.

A BLOB of 1..8 bytes is the narrow nibble's sized string at eight key bits
(`0x98–0x9F`): the key, the descriptor and the bytes, which is protobuf's tag
and length. With a size byte as well, every short string of a type of more than
sixteen fields cost a byte more than protobuf's. Its codes were a dictionary
encoding, reserved and never written, and packed5 sizes past 65 535 bytes, which
no string packed5 is for reaches.

| SPECIAL | meaning |
|---|---|
| `0xF0` | null |
| `0xF1`, `0xF2` | true, false — in dynamic values only (§9) |
| `0xF3`, `0xF4` | the float64 / float32 that follows (§9) |
| `0xF5` | the BLOB that follows is bytes, not text (§9) |
| `0xF6` | unassigned, refused |
| `0xF7` | TYPED: a struct index, then a STRUCT, LIST or TABLE (§9) |
| `0xF8`–`0xFF` | varint: the descriptor holds the value's low three bits, LEB128 bytes the rest |

**The varint** is chosen by the writer only when it is strictly shorter than the
magnitude form — raw for an unsigned field, zigzagged for a signed one — so no
value ever grows. A reader refuses one carrying more than 64 bits.

A typed bool `true` is `[key] 01`, the inline 1; `false` is omitted. `Zero` is
`[key] 00` and `EmptyString` is `[key] 90 00`. A string array is a homogeneous
LIST: `[key][0xC8 | lw][length][count]` then `[elementLength][bytes]` per element.

### 4.2 Composites, and why a key run can change width

Structs, lists and maps open with a key, a descriptor and a one-byte length
placeholder, and close with a backpatch: a body shorter than 255 bytes fits the
placeholder, and a longer one makes room for a u32 and ORs `lw = 2` into the
descriptor.

**Key width is a property of a key run, not of a message.** A STRUCT descriptor's
`k8` bit says the width of the run inside it, at both widths (§3.3), so a wide
record can hold a narrow struct and the other way round. The root byte is the
same descriptor, which is how the root states its own width.

### 4.3 Skipping

`Skip` steps over one field without descending into it: an inline value is one
byte, an INT one plus its width, every length-carrying class its declared length.
A SPECIAL is checked one level down — a float detail has to be followed by an
integer, BYTES by a BLOB, TYPED by a STRUCT, LIST or MAP. `Skip` is not
recursive, so a hostile message cannot overflow the stack through it. It refuses
truncation, a length past `maxInt`, detail `0xF6`, and an overlong varint.

---

## 5. Encodings shared by both widths

### 5.1 Floats

A float's zero bytes are its low mantissa bytes, where an integer's are its high
ones. So a scalar float field is written as **the byte-reversed IEEE bit pattern,
in the unsigned integer form**: the trim that drops an integer's high zero bytes
drops the float's low ones. `2.0` is `[key] 40` at eight key bits, a round price
is a few bytes, and `0.0` is omitted like any other zero.

| where | encoding |
|---|---|
| scalar field, both widths | reversed bits as an unsigned integer |
| map value, both widths | reversed bits as `ElementUint` |
| dynamic value | `0xF3` / `0xF4`, then reversed bits as `ElementUint` |
| table column | **plain** bits, widened to int64 (float32 as a `uint32`) |

A column has no trim to feed — the column codec packs residuals — so it carries
the plain pattern. Reading either as the other gives a number rather than an
error, which is why the two are never handled by the same line of code.

### 5.2 Key-less elements

List elements and map entries have no key.

- **Wide:** `[descriptor][payload]`, exactly a field without its key.
  `ElementUint` is inline or INT (never varint), `ElementInt` is a negative INT or
  `ElementUint`, `ElementString` a raw BLOB (never packed). A struct element is a
  STRUCT descriptor with its own `k8` bit and a length.
- **Narrow:** a code byte and a payload, in a code table of their own rather than
  the field nibble of §3.1 — an element sits inside a composite whose length already
  steps over it, so it does not need to size itself. An unsigned value's code is
  0..7 for the value itself or 8..15 for `code − 7` bytes of magnitude; a signed
  one's is `[pos][n:3]` as the wide INT detail (§4.1); a string's is `lw` and a size,
  then the bytes; a struct is `[length][key run]`, at the width the schema says.

---

## 6. Composites

### 6.1 Nested structs

A nested struct is always written, even with an empty body. That makes an absent
key unreachable from this encoder for a by-value struct, and it is what lets a
pointer to a struct need no explicit zero (§6.6). The one exception is a page
(§6.5).

### 6.2 Slices of structs: a list or a table

A `[]Struct` is written row-wise as a LIST, or **transposed into a table** when
both hold:

- at least `tableThreshold = 8` rows, and
- every field of the struct is *columnable*: bool, an integer, a float or a
  string.

The decision is made per value on the row count, and a list and a table are
different descriptor classes, so a reader dispatches on what it finds. A wide
list's elements each carry a full STRUCT descriptor (`homog = 0`); a narrow
list's carry a length only.

### 6.3 Tables

```
wide:    [key] [0xE8 | lw] [length] [rows] column*
narrow:  [key | 1101]      [length] [rows] column*
```

A table is one keyed column per field instead of one key per field per row.

| column | wide | narrow |
|---|---|---|
| integer, bool, float | COL: `[key][0xB0 \| lw][length]` and the column codec (§7) | `[key \| 1100][length]` and the column codec |
| string | a homogeneous LIST of strings | a narrow string array (§3.2), one element per row |

**Column keys use the wider of the parent's width and the row type's**: under a
wide parent always eight bits; under a narrow parent eight bits if the row type is
wide (a `wire.Writer8` over the same buffer) and four otherwise. A reader takes the
row type's width from the schema.

An integer column whose every value is zero is not written, and its absence means
zeros. A string column of only empty strings is omitted too, at both widths, and
its absence means empty strings.

**Grouped tables** (`wire/groups.go`). A table's rows can be cut into groups,
each group the column run a table of its rows would hold:

```
wide:    [key] [0xEC | lw] [length] [rows] [groupRows] group*
narrow:  [key | 1111]      [length] [rows] [groupRows] group*
group := [byteLength:u32] column*
```

- `groupRows` is a count (§3.3), at least 1. Every group holds `groupRows` rows
  but the last, which holds the rest, and there are exactly ⌈rows / groupRows⌉
  of them: a group missing, one running past the table, or bytes after the last
  are refused.
- A group's columns are keyed as the table's would be, and are absent, zeros or
  dictionary ids by the same rules — per group, so a column can be absent from
  one group and present in the next.
- The group's length is a fixed four bytes rather than a length form, so the
  writer patches it in place: a group is far past what one byte holds, and a
  widening backpatch would move it.
- Only a table field is grouped. A table in a dynamic value (§9) never is, and a
  reader refuses one that says it is.

Grouping is the writer's choice, and a reader takes `groupRows` from the message.
Go groups a table field of more than 4 096 rows into groups of 4 096
(`wirefmt.TableGroupRows`); Rust writes every table whole. The point is the
writer's memory: a table is transposed through one buffer holding every column
of the rows being written, and for a table written whole that is every column
of every row — 90 MB for TPC-H's lineitem at a tenth of scale, dropped by the
pool at the next GC and allocated again. A group's columns fit in cache between
their gather and their write. Encoding TPC-H's normalized tables was 26% faster
on Lambda x86 and 18% on Graviton2, and decoding 4–7%, for 0.04% more bytes: a
group's four-byte length, its column headers, and a FOR base or delta seed per
group.

**Dictionary columns** (prototype, `SetDictionary`, `codec/dictionary.go`). A
string column whose values repeat can go out as ids into a dictionary the message
holds once, ahead of its body:

```
message := [root | 0x02] [dictionary section] [body]
section := [byteLength] [dictCount] ( [entryCount] ( [size] [bytes] )* )*
wide:    [key] [0xB0 | lw] [length] [dict] [column codec over the ids]
narrow:  [key | 1101]      [length] [dict] [column codec over the ids]
```

There is one dictionary per column field — a row type and a key — so tables of
the same row type anywhere in the message share it. The writer decides per
column: the column's new entries and its ids have to cost less than its
strings, it may add new values for at most half its rows — and a column whose
first 1024 rows are more than 7/8 new is given up there — and a dictionary
holds at most 2¹⁶ entries. The writer's index is its own pair of
open-addressed tables (`codec/dictindex.go`): a string of up to sixteen bytes
is its own key — two words and its length — held in the slot, and a longer one
is hashed from three words of it; a probe past 32 slots gives the column up
rather than degrade. On amd64 and arm64 the common case — a short string the
dictionary already holds — runs in `dictKernel` (`codec/dictkernel_amd64.s`,
`codec/dictkernel_arm64.s`): one masked sixteen-byte load (SSE2 on amd64, an
LDP pair on arm64), the hash and the probe, handing anything else back to Go.
The load never starts in a page's last sixteen bytes, so it cannot fault. A
message whose columns all decline is byte for byte what the writer produces
with dictionaries off. A dictionary column is written even
when every id is zero, since its absence would mean empty strings. Only the typed
decoders read it yet; the JSON walks, `DecodeAny` and the Rust and JS readers
refuse the root byte.

On the Go side a table, or a group of one, is gathered into one reusable column
buffer (`scratch`, pooled up to 2¹⁶ rows) that holds every column of its rows,
and decoded through the same buffer before it is scattered into the
destination slice. The JSON walk holds every column of a table too — a row
needs every column's value — and of a grouped table one group's at a time (§12).

### 6.4 Maps

```
wide:    [key] [0xE0 | lw] [length] [count] ( key element, value element )*
narrow:  [key | 1100]      [length] [count] ( key element, value element )*
         [key | 1110]      [length] key element, value element         one entry
```

Entries are pairs of key-less elements (§5.2). A typed map's are sorted by key; a
map of `any` (`mapAny`) is a dynamic container, its keys and values compact (§9.1)
and in whatever order the writer's map handed them over. Keys are strings or
integers. Values are a string, an integer, a float, a bool (an unsigned 0 or 1),
`any`, or a struct.

| map kind | value | number |
|---|---|---|
| `mapString` | | 0 |
| `mapInt` | | 1 |
| `mapUint` | | 2 |
| `mapFloat64` | | 3 |
| `mapBool` | | 4 |
| `mapFloat32` | | 5 |
| `mapAny` | a dynamic value (§9); wide only | 6 |
| `mapStruct` | a struct element | 7 |

The numbers are format: a schema section names them. The two float widths are
separate kinds because a reader working from a section has no Go field to read
the width off.

**A struct value** is written the way a list writes its element — at eight key
bits with a STRUCT descriptor, at four with a length — and the schema section
names which struct by its index in the struct table (§10). So a map of structs
needs no self-describing message: a section sent once per connection describes
it. `map[K]*Struct` is refused, because a nil value inside a map has no form.

Go cannot walk a map through an unsafe pointer, so this is the one field kind
read and written with reflection per entry. A map value has no address either, so
each struct value is copied into one holder per map on the way out, and read into
it — zeroed first — on the way in.

### 6.5 Pages: more than 255 fields

A key is one byte, so a key run holds 256 fields. A type that numbers a field past
255 is split into **pages** of 255 fields (`codec/pages.go`):

```
page = (id − 1) / 255        key = (id − 1) % 255
```

Key 255 of each page holds the next page as an ordinary nested struct. Each page
is a plan over the same Go record, and the link is an `opStruct` field at offset
0, so the encoder and the typed decoder have no paging code at all.

- **An empty page is not written** (`Writer8.CloseNonEmpty`). The link is a
  page's last field, so a page holding only an empty link is empty too, and pages
  past the last field in use cost nothing.
- Page 0 is always wide, because of the link at key 255; the last page can be
  narrow.
- The schema section flags each page (§10), and the JSON walks write a page's
  fields into the object that links it.
- Every field of a paged type needs an explicit id, since a derived key could land
  on 255. Duplicates are refused.
- At most 16 pages, so ids run 1..4080 (`maxFieldID`).
- `FieldIDs` reports ids from `plan.ids`, since a key no longer names a field.
- `Generate` refuses a paged type: the code it writes is one key run.

### 6.6 Pointers

A nil pointer is omitted and costs nothing, like any zero. What differs is the
non-nil one.

| field | non-nil |
|---|---|
| `*scalar`, `*string` | the pointee, written **explicitly when it is zero** (§3.2, §4.1), so a pointer to zero does not read back nil |
| `*Struct` | the struct body. A body is written whether or not it is empty, so absent is nil and an empty body is a pointer to a zero struct — no explicit zero needed |
| `*[]T`, `*map[K]V` | the slice or map, exactly the bytes of the plain field |

**A pointer to a slice or a map is not on the wire.** The plan marks the field
`indirect` and describes the pointee, so the schema names the pointee's op and a
reader in another language sees an ordinary slice or map. The two walks that touch
the field's memory step through the pointer: encode skips a nil one, decode
allocates the pointee when the key is present. An empty slice writes nothing, so
`&[]T{}` reads back nil — the one distinction this does not keep.

Still refused: `**T`, a pointer to an array, `*any`.

---

## 7. The column codec (`column/`)

An integer column is one transform and then blocks of 128 residuals, each at its
own bit width.

```
column := [header:1] [base: 8 bytes]? block*
block  := [width:1] [⌈count·width / 8⌉ bytes]        a full block is 16·width bytes
header := [—:5] [zigzag:1] [transform:2]
```

| transform | residuals | base |
|---|---|---|
| 0 raw | the values, zigzagged when any is negative | none |
| 1 delta | `zigzag(v[i+1] − v[i])`, n − 1 of them | `zigzag(v[0])` |
| 2 frame of reference | `v − min` | `zigzag(min)` |
| 3 constant | none, no blocks | `zigzag(value)` |

Only headers 0x00, 0x04, 0x05, 0x02 and 0x03 are accepted, and an empty column is
the single byte `0x00`.

- **Widths are per block**, so one outlier widens 128 values rather than the
  whole column. Width 0 is a block of zeros and costs one byte, which makes a
  sparse column nearly free.
- **The transform is chosen by its real blocked cost**, each candidate scored by
  what its blocks would occupy: raw, then frame of reference, then delta (skipped
  on overflow and for one value), a later one winning only if strictly smaller.
  Constant is taken when it beats them.
- **Packing is LSB-first with no gaps**, and 128 values at `w` bits are `16w`
  bytes — a whole number of bytes and of 64-bit words for every `w` — so a block is
  byte-aligned at both ends and carries no state across a block boundary.
- **The unpack is carry-free**: for `w ≤ 57` each value is one unaligned 8-byte
  load, a shift and a mask. A tail without eight bytes of slack takes a gather
  path, and widths 58..64 an out-of-line one, so a caller needs no padding.
- **Width leaves the inner loop**: the decoder switches on the width once per
  block, never per element.

A reader refuses a width past 64, truncation, and a base or value outside the
destination type's range.

---

## 8. packed5 (`packed5/`)

An opt-in string encoding for short text of letters, digits and Spanish accents:
5-bit units, packed LSB-first, eight units per five bytes.

| unit | meaning |
|---|---|
| 0..25 | a letter, in the current case mode |
| 26 | space |
| 27 | invert the case of the next letter only |
| 28 | invert the case mode |
| 29 + 1 unit | a symbol: digits and punctuation |
| 30 + 1 unit | an extended character: accents, `€`, …; 31 escapes to raw bytes |
| 31 + 2 units | a number 0..1023 |

The stream opens in the case of the first ASCII letter, and that mode is what the
two packed codes on the wire distinguish (lower and upper). A packed string
carries no header of its own: the field's descriptor states its size and that it
is packed.

| width | how a packed string is flagged |
|---|---|
| K4 | blob escapes 3 / 4 (u8 size) and 5 / 6 (u32 size); a 5 / 6 declaring ≤ 255 is refused |
| K8 | BLOB `[0 1 up lw]`: `up` 0 (lower) or 1 (upper), a size of 1 or 2 bytes; a longer packed string is written raw |

The writer packs only when the payload is shorter than the raw string, and at
four key bits only when header and payload together are shorter too, so turning it
on never makes a message larger.

`SetPacked5` is a process-wide writer setting, off by default. It covers string
fields and pointers to strings at both widths, and nothing else: string arrays,
table columns, map keys and values, dynamic strings and `Generate`'s output are
always raw. Every reader reads either form.

---

## 9. Dynamic values

A field of type `any`, `[]any` or `map[K]any` holds a value whose type is not
declared, so the value says what it is. This is the one place colbin puts a type
on the wire, and it only exists at eight key bits: a four-bit descriptor has no
room for a class, so **a type holding a dynamic field is wide**.

| kind | written as |
|---|---|
| nil | `F0` |
| bool | `F1` / `F2` |
| integer | inline, INT or varint — int64, or uint64 past it |
| float64 / float32 | `F3` / `F4`, then the reversed bits |
| string | BLOB, raw |
| `[]byte` | `F5`, then a BLOB |
| slice, array | LIST, its values compact (§9.1) |
| map | MAP, its keys and values compact (§9.1), in no particular order |
| struct | `F7` + struct index, then a STRUCT (self-describing) — or a MAP of field names (plain `Marshal`) |
| `[]Struct`, or `[]any` of one struct type | `F7` + struct index, then a LIST, or a TABLE from eight rows |

That is a dynamic value at a field, where a reader that does not know the field
has to step over it by its descriptor. Inside a LIST or a MAP — and at a dynamic
root — nothing steps over anything, and the values are compact.

**TYPED** (`0xF7`) is what keeps an array of records inside an `any` from writing
its field names per row: it says "what follows is struct number n, which the
section already describes", and the rows are then written by the ordinary
composite path — a list, or a transposed table. It needs a section, so
`MarshalSelfDescribing` on a type with a dynamic field builds the struct table
while it writes the body and serialises the section afterwards
(`marshalDynamic`), rather than using the cached section. Without a section a
struct inside an `any` is written as a map of its field names.

On decode, a dynamic value becomes `nil`, `bool`, `int64`/`uint64`,
`float64`/`float32`, `string`, `[]byte`, `[]any` or `map[string]any`; a message's
own section is parsed lazily, the first time a TYPED value needs it.

A value the format has no form for — a channel, a function, a complex number, an
opaque struct without `encoding.TextMarshaler` — is an encode error, as is a value
that holds itself.

### 9.1 The compact form

Inside a dynamic container a value is a code byte from a table of its own
(`wire/compact.go`), msgpack's shape with colbin's numbers:

| code | value |
|---|---|
| `00`–`7F` | an unsigned integer, the code itself |
| `80`–`8F` | a map of 0..15 entries, each a key and a value |
| `90`–`9F` | a list of 0..15 values |
| `A0`–`BF` | a string of 0..31 bytes |
| `C0` / `C1` / `C2` | null / false / true |
| `C3`–`C5` | a string, its length in 1, 2 or 4 bytes |
| `C6`–`C8` | bytes, their length in 1, 2 or 4 bytes |
| `C9` / `CA` | a map / a list, its count after (one byte up to 254, or `FF` and a u32) |
| `CB` | TYPED: a struct index, then a STRUCT, LIST or TABLE element as in §9 |
| `CC` | a float64 holding a whole number, the compact integer after |
| `D0`–`D7` | an unsigned integer of 1..8 bytes |
| `D8`–`DF` | a negative integer, its magnitude in 1..8 bytes |
| `E0`–`E8` | a float64, its byte-reversed bits trimmed to 0..8 bytes |
| `E9`–`ED` | a float32, the same in 0..4 bytes |
| `F0`–`FF` | a float64 holding 0..15 |

`CD`–`CF` and `EE`–`EF` are unassigned and refused.

A container inside a container carries its count and **no length**: stepping
over one means reading it, which nothing inside a dynamic container has to do —
the outermost one, at a field, has its length, and that is what `Skip` uses. A
float64 holding a whole number, which is every number `encoding/json` hands Go,
is written as the integer when that is shorter than its trimmed bits.

**At the root** a `map[string]any` is its entries to the end of the message, with
no count, and a `[]any` is one compact value. A dynamic document therefore costs
its root byte over msgpack's encoding of it, and the maps are written in the
order Go's iteration hands their entries over: the same document encodes to
different bytes from one call to the next, and is the same document decoded.

---

## 10. The schema section

The wire carries no type: a field is a key and a payload, and what the payload
means comes from a schema both sides already have. A browser, a `jq`-style tool or
a dynamically typed client has none, so the schema can be sent as bytes — once per
connection (`Schema.Bytes`), or in front of a message (`MarshalSelfDescribing`,
root bytes 0xD4 / 0xDC). The body behind a section is byte for byte what `Marshal`
writes, and `Unmarshal` steps over a section it does not need.

```
section   := [byteLength] [structCount] structDef{structCount}
structDef := [flags:1] [fieldCount] field{fieldCount}
field     := [key:1] [nameLength] name [op:1] extra
```

Every length is one byte, or `0xFF` and a u32. `byteLength` covers everything
after it, so a reader that wants only the body adds it to the cursor.

| flag | meaning |
|---|---|
| 0x01 | the run uses eight-bit keys |
| 0x02 | an envelope (§2.2): render the field, not the struct |
| 0x04 | a page (§6.5): merge its fields into the object that links it |

| op | extra |
|---|---|
| `opStruct`, `opStructs`, `opPointerStruct` | `[structIndex]` |
| `opMap` | `[keyKind:1][valueKind:1]`, then `[structIndex]` when the value kind is `mapStruct` |
| `opPointer` | `[elemOp:1]` |
| everything else | nothing: an array's element type is in its op |

The field ops (`codec/codec.go`) are format and pinned by a test:

| # | op | # | op |
|---|---|---|---|
| 0 | bool | 14 | `[]int16` |
| 1 | int8 | 15 | `[]int32` |
| 2 | int16 | 16 | `[]int64` |
| 3 | int32 | 17 | `[]uint16` |
| 4 | int64 | 18 | `[]uint32` |
| 5 | uint8 | 19 | `[]uint64` |
| 6 | uint16 | 20 | `[]string` |
| 7 | uint32 | 21 | struct |
| 8 | uint64 | 22 | `[]struct` |
| 9 | float32 | 23 | map |
| 10 | float64 | 24 | pointer to a scalar or string |
| 11 | string | 25 | `any` |
| 12 | `[]byte` | 26 | `[]any` |
| 13 | `[]int8` | 27 | pointer to a struct |

A Go `int`, `uint` or `uintptr` is named by its 64-bit op, so a section does not
depend on the platform that wrote it.

**Structs are an indexed table, not inlined**, root at index 0, so a recursive type
(`type Node struct{ Kids []Node }`) describes itself in finite space. The builder
reserves a struct's index before walking its fields, so a field that reaches back
resolves to an index already assigned.

**A section is untrusted input.** `ParseSchema` refuses: a declared count the
remaining bytes cannot hold (a struct is at least 2 bytes, a field 3); an unknown
flag, op or map kind; a narrow key ≥ 16; a duplicate key; a dynamic op in a narrow
struct; a struct index outside the table; a struct holding itself by value; a
by-value nesting deeper than 128 levels or whose zero value expands past 2²⁰
fields; and a page reached any way but key 255 of a wide struct. Bytes after the
last definition are left alone: that is where a later version would add
something.

---

## 11. The Go encoder and decoder (`codec/`)

### 11.1 Plans

The first use of a type resolves it, once, into a **plan**: per field a key, an
offset, an op, and for composites the child plan and what a slice of them needs
(`planField`, `typePlan`). After that, encoding is a walk over the plan reading
fields by offset through an unsafe pointer, with no reflection — except maps
(§6.4) and dynamic values (§9). Plans are cached per type. A recursive type
terminates because the half-built plan is registered before its fields are
walked, so a field that reaches back finds it.

Field names are kept beside the fields rather than in them: a name is read once
per type, by the schema and JSON paths, and a field is loaded once per message.

### 11.2 Ids and keys

```go
Count  int64  `cb:"1"`        // id 1, key 0
Label  string `cb:"name,2"`   // id 2, named "name" in the schema
Notes  string `cb:"-"`        // not carried
Region string                 // no id: derived from the name
```

Explicit ids are assigned first; a field without one takes `fnv8` of its name —
FNV-1a folded to eight bits, which is format — and probes upward past every taken
key. So a derived key never displaces a declared one. The id-to-key subtraction
happens in exactly one place (`assignKeys`).

### 11.3 The key width

A type uses eight-bit keys when any of these holds, and four otherwise:

- an id above 16;
- a field with no explicit id, whose derived key lands anywhere in 0..255;
- a dynamic field (§9).

The width is resolved once per type and held in the plan.

### 11.4 Two walks, one per width

`writePlan` / `readField` / `readNarrowRun` are the narrow walk, and `appendWide`
/ `readWideField` / `readRun` the wide one. They are separate functions rather
than one with a width flag, because a width the compiler cannot see is a width it
cannot fold: the descriptor position and the key stride stop being constants and
the function grows past the inlining budget. The same reason keeps a separate
reader or writer variable per branch — escape analysis is per variable.

A decode **zeroes the record first**, so an omitted key leaves the field zero.
An unknown key is skipped at both widths (§3.1, §4.3). Fields are
written in declaration order.

### 11.5 The flat path

A plan with no composite, pointer, map or dynamic field is marked `simple` and
goes through `appendScalars` / `readScalars`, which take no scratch buffer and no
deferred release and inline into their caller. Most records are flat, and the
general walk's frame and spills cost them for nothing.

Those walks, `writeValue` / `readValue` and the JSON scalar renderers are
generated from one table, `valueOps` in `codec/ops.go`, into `codec/ops_gen.go`,
so the per-op switches the hot paths run cannot drift apart. (A few smaller
switches over value ops — zero tests, table gather and scatter — are still
written by hand.) Regenerate it with:

```sh
COLBIN_UPDATE=1 go test ./codec -run TestValueOpsAreGenerated
```

### 11.6 Handles and the public API

| | |
|---|---|
| `Marshal`, `Append`, `Unmarshal` | resolve the plan per call; `Append` returns `dst` unchanged on failure |
| `Codec[T]` | the plan resolved once and held: no type lookup, no reflection entry, no boxing. Safe for concurrent use |
| `MarshalSelfDescribing` | section, then body |
| `SchemaFor[T]`, `SchemaOf`, `ParseSchema` | the section; built schemas are cached per root type |
| `ToJSON`, `AppendJSON`, `DecodeAny` | a message without the Go type (§12) |
| `FieldIDs` | every field's id, so a reader in another language need not read tags |
| `Generate` | Go source for flat records (§11.7) |

Encoding a flat plan cannot fail. A nested one can: a value nesting deeper than
128 levels, or a dynamic value with no form (§9).

### 11.7 Generated codecs

`codec.Generate` writes `AppendColbin` and `UnmarshalColbin` for named, flat,
unpaged structs — the straight-line calls a hand-written codec would make, and a
decode that switches on a constant key. It works from the same plan and the same
`valueOps` table, so it cannot disagree with the reflective path about ids or
types. A field of a named type is converted; a native `int` is written at 64 bits
and range-checked on read. It refuses composites, pointers, maps, `any`, slices of
named element types and paged types. Generated code writes strings raw and reads
both forms.

---

## 12. Rendering JSON without the Go type

`ToJSON(schema, message)` walks a message against a plan parsed from a section. A
message's own section takes precedence over the one passed in; with neither, the
call fails and says how to fix it.

- **Fields** are written in the order the message holds them, then every field the
  message omitted, in schema order, as its zero: `false`, `0`, `""`, an object of
  zeros for a by-value struct, and `null` for a blob, array, list, map, pointer or
  `any`.
- **Pages** are merged into the object that links them.
- **Tables** are transposed back into an array of objects: every column is read
  first, after spending the row budget (§13), and rows are emitted in schema field
  order. A missing column, or one shorter than the table, renders zeros. A
  grouped table is one array, read and emitted a group at a time.
- **Maps** render in wire order: key order for a typed map, the writer's
  iteration order for a dynamic one. An integer key becomes a quoted decimal.
- **Envelopes** are unwrapped.
- **Numbers** follow `encoding/json`: integers exactly, floats in their shortest
  form at their own width, exponent form below 1e-6 and from 1e21. **NaN and
  ±Inf are refused** rather than written as `null`; `DecodeAny` keeps them.
- **Strings** are escaped as `encoding/json` escapes them, `<`, `>`, `&`, U+2028
  and U+2029 included; invalid UTF-8 becomes U+FFFD. A `[]byte` is base64.

An empty slice or map is indistinguishable from nil on the wire and renders
`null`.

`DecodeAny` walks the same way into `map[string]any`, `[]any` (never nil, so `[]`
stays distinct from null), `int64` / `uint64` by signedness, `float64`, `string`
and `[]byte`.

---

## 13. Limits every reader enforces

Every decoder treats its input as hostile: whatever bytes arrive, the caller gets a
value or an error — never a panic, a hang, or an allocation the message did not
pay for.

| limit | value | where |
|---|---|---|
| nesting depth, every walk, encode and decode | 128 | `maxSchemaDepth`; Rust `MAX_DEPTH` |
| rows in one table | 2²² | `maxTableRows`; Rust `MAX_ROWS` |
| cells (rows × declared columns) per message | `2²² + 64 × len(message)` | `rowBudget` |
| a list's count | ≤ its body's bytes | every element is at least one byte |
| a map's count | ≤ its body's bytes / 2 | |
| a grouped table's groups | `groupRows` ≥ 1, exactly ⌈rows / groupRows⌉, none past the table and nothing after them | `wire.ErrBadGroup`; Rust `Error::BadGroup` |
| an integer array's count | ≤ bytes / width | |
| an integer that does not fit its field | refused (`ErrFieldTooWide`) | a scalar is never truncated |
| fields per run | 16 narrow, 256 wide | |
| pages | 16, ids 1..4080 | |

A table's row count is the one number a message cannot bound by its own size — an
absent column is a column of zeros, so a million rows can be a few bytes — which
is why rows are budgeted per message rather than checked against bytes. The Go
typed decoder, the Go JSON walk and the Rust walk spend the same budget.

---

## 14. The Rust crate (`rust/`)

The same format, `no_std` + `alloc` unless `std` is on, with `#![forbid(unsafe_code)]`
and no runtime dependencies. It reads and writes everything the Go format has
(the gaps are in §20); `rust/vectors/vectors.json` is the corpus Go writes and
`rust/tests/` reads.

| module | role |
|---|---|
| `wire/narrow.rs`, `wire/wide.rs`, `wire/mod.rs` | the two framings and the shared value codecs |
| `wire/dynamic.rs`, `wire/compact.rs` | dynamic values and TYPED, and the compact dynamic documents (§9.1), read only |
| `wire/root.rs` | the root byte a typed narrow message has no need of: `0xD0` only when its key run begins in the root range |
| `wire/nullable.rs`, `wire/dictionary.rs`, `wire/listcolumn.rs` | the readers of nullable, dictionary and list columns |
| `column.rs`, `packed5.rs` | the column codec (decimal ALP columns included) and packed5 |
| `plan.rs` | op and map-kind numbers, `Plan`, `PlanField` |
| `section.rs` | parse a schema section; build one (`encode`) |
| `walk.rs` | a message to JSON against a section (`message_to_json` reads the root, section and dictionaries) — envelopes unwrapped, pages merged, nullable, dictionary, paged and list columns, dynamic documents |
| `json.rs`, `json/parse.rs` | a sink that writes what `encoding/json` writes; an exact scanner (integers exact, floats correctly rounded) |
| `infer.rs`, `build.rs`, `verify.rs`, `diag.rs` | JSON text → colbin: infer a schema, write the body, optionally read it back and compare (`encode`) |
| `materialize.rs` | a table → the flat typed buffer of §16.3 (`materialize`) |
| `inspect.rs` | a span per field and value, for a hex view |
| `codec.rs` | the `Colbin` trait and the helpers `#[derive(Colbin)]` calls |

| feature | | default |
|---|---|---|
| `std` | the `std::error::Error` impl, the crate's only use of the standard library | on |
| `encode` | the JSON scanner, inference, build, self-check | on |
| `materialize` | the flat typed buffer | on |
| `derive` | `#[derive(Colbin)]` | off |

**Root.** A typed narrow message has no root byte: a first byte outside
`0xD0..=0xDF`, or an empty message, is its key run. `walk::split_root` is the one
place that cuts a message at its root, and `decode`, `materialize` and `inspect`
all use it. A reader that indexes byte 0 as a root is wrong for the narrow case.

**JSON → colbin** (`build::encode`) is the one thing the Rust side does that Go
has no counterpart for. Its rules — what a document infers to, what is refused,
what only warns — are specified in `rust/ENCODER.md`. Ids are assigned in
first-seen order and the envelope field is named `rows`, as Go names it.

**`#[derive(Colbin)]`** generates a typed encode and decode for a named-field
struct: scalars, `String`, `Vec<u8>`, integer and string vectors, `Option` of a
scalar or string, nested `Colbin` types, `Vec` of them (a table from eight rows
when columnable), and `HashMap` / `BTreeMap` of scalars and strings. Attributes:
`#[cb(N)]`, `#[cb(name = "…")]`, `#[cb(skip)]`, and on the struct `#[cb(wide)]`
and `#[cb(packed5)]`. Ids run 1..255: the derive does not page.

---

## 15. The browser module (`rust/wasm/`)

A `cdylib` over the crate with `default-features = false`, in its own Cargo
workspace so its release profile can set `panic = "abort"` without breaking
`cargo test` elsewhere. The profile is `opt-level = 3`, fat LTO, one codegen unit,
`strip = "debuginfo"` (which keeps the `target_features` section `wasm-opt`
reads).

| feature | | default |
|---|---|---|
| `encode` | JSON text → colbin | on |
| `materialize` | the flat typed buffer | on |
| `inspect` | the span walk, for the demo site's hex view | off |

**Protocol.** The module imports nothing and exports its memory. The host calls
`alloc(n)`, writes `n` bytes at the returned pointer, then calls an export with
the length; output is copied from `result_ptr()` for the length returned. Memory
views must be taken after `alloc`, since a grow detaches earlier ones. No path
traps: a failure returns −1 and leaves a JSON diagnostic for `last_error()`.

| export | |
|---|---|
| `alloc(n)`, `result_ptr()` | the input buffer, the output buffer |
| `set_schema(len)` | parse a section and hold it; 0 clears |
| `decode(len)` | a message → JSON text (`walk::message_to_json`); a message's own section wins over the held one |
| `materialize(len)` | a table → the flat buffer; 0 means "not this shape", which includes nullable, dictionary, paged and list columns and the dictionary and dynamic roots |
| `encode(len, flags)` | JSON → colbin; flags 1 self-describing, 2 verify, 4 pack strings |
| `section()` | the section the last `encode` built |
| `inspect_message(len)` | spans, `inspect` builds only |
| `last_error()` | the last diagnostic |

The diagnostic is `{code, offset, line, path, message, warnings}`.

---

## 16. The npm package (`js/`)

### 16.1 Entries

| import | finds the module by |
|---|---|
| `colbin` (browser, bundler) | base64 inlined into the bundle: no bundler configuration, no asset to 404 |
| `colbin` (Node) | `readFile` next to the entry |
| `colbin/asset` | `fetch(new URL('./colbin.wasm', import.meta.url))`, for a bundler that emits assets |
| `colbin/inspect` | the same, for the `inspect` build |

Each exports `Codec` (with `open`, `fromModule`, `preload`) and the free
functions `unmarshal`, `columns`, `toJSONText`, `marshal` and `inspect`, which
share one lazily opened handle.

### 16.2 The handle

A `Codec` holds **one** module instance. The compile happens in `Codec.open()`,
so every operation is a straight run of synchronous wasm calls with no `await`
between writing its input and reading its output — which makes concurrent calls
on one handle (`Promise.all([codec.unmarshal(a), codec.unmarshal(b)])`) safe by
construction. The format's recommended delivery is a schema once per connection,
and a held instance is that delivery's shape: `setSchema` stores the section and
the instance parses it once, at the next call, and an identity check keeps every
later message from re-parsing it.

| method | |
|---|---|
| `unmarshal(bytes)` | objects. Tries `materialize`; falls back to `JSON.parse(toJSONText())` |
| `columns(bytes)` | the table as columns — typed-array views for numeric columns — or `null` if it is not one |
| `toJSONText(bytes)` | the JSON text |
| `marshal(value, options)` | colbin bytes, with the section available separately or composed in front |
| `inspect(bytes)` | spans, `colbin/inspect` only |

A failure is a thrown `ColbinError` carrying the diagnostic's fields.

### 16.3 Why the client does not go through JSON text

A table is columns of bit-packed integers. Rendering it as JSON text and handing
that to `JSON.parse` goes columns → decimal text laid out as rows → re-parse →
objects, where the middle two steps exist only to hand V8 something it knows how
to turn into objects. So `unmarshal` does not produce text when it can avoid it:
the module decodes the table into a flat typed buffer, and JavaScript builds the
rows from it. CI holds that path to at most 1.5× the cost of `JSON.parse` on the
equivalent JSON (`js/tests/bench.mjs --check`).

The buffer (`materialize.rs`, read by `js/src/materialize.ts`):

```
u32 version = 1, u32 rows, u32 fieldCount
per field:  u8 kind (0 bool, 1 int, 2 float, 3 string), u8 flags, u16 nameLength, name
padding to 8, then one run per field, each padded to 8:
  bool    a bitmap, ⌈rows / 8⌉ bytes
  int     rows × i64
  float   rows × f64
  string  (rows + 1) × u32 offsets, then the column's bytes
```

Flags: bit 0 signed, bit 1 a value exceeds 2⁵³ (the column is read as `BigInt`),
bit 2 every string is ASCII. A row builder is generated per field signature with
`new Function` and cached; under a CSP that forbids it, a generic builder is used.

It covers what a table at the root is: an envelope (§2.2) whose field is a
`[]struct` written as a table — eight or more rows of columnable fields. Anything
else takes the JSON path, which is exact except that integers above 2⁵³ round in
`JSON.parse`.

---

## 17. Building the package

`bun run build` in `js/`:

1. `wasm:lean` — `cargo build --release --target wasm32-unknown-unknown` of
   `rust/wasm`, then `scripts/optimize.mjs`.
2. `wasm:inspect` — the same with `--features inspect`.
3. `optimize.mjs` runs `wasm-opt -O3 --strip-debug --strip-producers
   --strip-target-features` in place. `-O3` rather than `-Oz`: it is smaller
   once gzipped (`-Oz` wins 1.5 KB raw, which gzip gives back) and it does not
   trade speed away, which is what the module is for. Without `wasm-opt` it warns and
   continues, unless `--required` or `COLBIN_REQUIRE_WASM_OPT=1` — which CI sets.
   `WASM_OPT` overrides the binary.
4. `dist` — `scripts/inline.mjs` writes the base64 module into
   `src/generated/`, then `tsc`, then both modules and the licence into `dist/`.

`build/`, `dist/` and `src/generated/` are build output and never committed.

At v0.4.0 the optimised modules are 216 007 B (lean) and 230 992 B (inspect).

`bun run test` regenerates the vectors, builds, runs `node --test`, packs a
tarball and installs it outside the repository with `--ignore-scripts`
(`scripts/tarball.mjs`), and re-emits `js/vectors/web_encoded.json`.

---

## 18. Keeping three implementations in step

Go is the oracle, and every corpus is written by Go and checked in. CI regenerates
each one and fails on a diff, so neither side can move without the other failing.

| corpus | written by | checked by |
|---|---|---|
| `rust/vectors/vectors.json`: field ids, columns, typed cases, walks, numbers, texts | `go run ./rust/vectors` | `rust/tests/vectors.rs` both directions; `walk.rs`, `section.rs`, `materialize.rs` render every `doc.` walk and compare with Go's JSON; `dynamic.rs` under both deliveries; `parse.rs` every number against `strconv` |
| `js/vectors/vectors.json`: per type the section, message, self-describing message, JSON and how many single-byte corruptions Go refuses | `go run ./js/vectors` | `message.test.mjs` byte-equal JSON both deliveries; `fuzz.test.mjs` the module refuses at least every corruption Go refuses; `inspect.test.mjs` spans tile the body |
| `js/vectors/web_encoded.json`: documents encoded by the Rust encoder | `js/tests/emit.mjs` | Go parses and renders each (`go test ./js/vectors`); `rust/tests/{build,infer,verify}.rs` pin the bytes |

`go test ./rust/vectors` and `go test ./js/vectors` also fail if a committed
corpus is stale. The comparison is on bytes, not on parsed values, so a float
printed one digit differently is a failure.

The Go side is further covered by fuzz targets over every decoder and codec
(`FuzzUnmarshal`, `FuzzParseSchema`, `FuzzJSONWalk`, `FuzzReader`, `FuzzReader8`,
`FuzzRoundtrip`, `FuzzArrayRoundtrip`, `FuzzArrayDecode`, `FuzzAppendString`),
`-race`, a `GOARCH=386` run for platform-width integers, and staticcheck.

---

## 19. CI, versioning and release

`.github/workflows/ci.yml` runs on pushes to `main`, `v*` tags and pull requests.

| job | |
|---|---|
| `build` | Go: no dependencies, gofmt, vet, `go test -race`. Rust: vectors regenerated and diffed, fmt, clippy over the crate's and the module's feature sets, tests; the decode-only module must stay under ⅔ of the default. JS: vectors regenerated and diffed, `bun run test`, `bench:check`, `web_encoded.json` diffed. Web: `check`, `build`, `browser.mjs` in headless Chrome. Uploads both modules as the `colbin-wasm` artifact |
| `go-checks` | staticcheck, `GOARCH=386 go test`, ten seconds per fuzz target |
| `deploy` | `main` only: the demo site to GitHub Pages |
| `release` | `v*` tags only (below) |

**One version for everything.** The same `X.Y.Z` goes into `js/package.json`,
`rust/Cargo.toml` (and its `colbin-derive = "X.Y"` pin), `rust/derive/Cargo.toml`,
`rust/wasm/Cargo.toml`, `Cargo.lock` and `rust/wasm/Cargo.lock`, and the tag
`vX.Y.Z` is the Go module version. On 0.x the minor version is the compatibility
unit: a new op or map kind is a minor bump, because an older reader refuses a
section that names it. So is a new root byte, for the same reason.

**Releasing:**

1. Bump the files above, commit, push `main`.
2. Tag `vX.Y.Z` on that commit and push the tag.
3. The `release` job downloads the `colbin-wasm` artifact — the modules the tests
   ran against, not a second compile — creates the GitHub release with both
   modules attached, checks that the tag equals `js/package.json`'s version,
   builds `dist/` around those modules, and runs `npm publish --provenance
   --access public --ignore-scripts` through npm's trusted publishing (OIDC; no
   token is stored).

**Trusted publishing needs the package to exist**, so the first version is
published by hand, once: build with `wasm-opt` on the PATH and
`COLBIN_REQUIRE_WASM_OPT=1` (or put the release's modules into `js/build/` and run
`bun run --cwd js dist`), `npm publish --access public` from `js/`, then add the
repository and this workflow as a trusted publisher on npmjs.com. Until that is
done every `release` run creates the GitHub release and fails at `npm publish`
with E404.

---

## 20. Known gaps

What the code does not do yet, or does inconsistently:

- **Rust does not know op 27** (pointer to a struct): its `OP_COUNT` is 27, so the
  Rust walk, the module and the npm package refuse a section from a Go type with a
  `*Struct` field. The walk would also need to render an absent one as `null`.
- **The Rust walk refuses a map in a narrow run** (`Error::Unsupported`); a narrow
  map's elements take their types from the schema and need a second element
  reader. Wide maps, including maps of structs, render. `inspect.rs` steps over
  narrow maps instead, and does not merge pages.
- **Rust has no typed encode or decode of list columns, paged tables or
  dictionary columns**: the derive and `build.rs` write and read nullable columns,
  and the walk renders all of them (the corpus covers it), but a struct with a
  list column, a page or a dictionary column cannot be derived.
- **Rust does not decode a dictionary-section root (`D2`/`DA`) into a typed
  value**, only to JSON through the walk.
- **`materialize` falls back to the JSON path** for a table with a nullable,
  dictionary, paged or list column; the fast path covers plain columns only.
- **`hasDynamic` is not transitive**: only an `any` field of the root type (or its
  pages) makes `MarshalSelfDescribing` take the TYPED path; an `any` in a nested
  struct writes its structs as maps of field names.
- **The encoder does not enforce the row limit**: a transposable slice past 2²²
  rows is written as a table every decoder refuses.
- **A table column is scattered into a narrower field without a range check**,
  where a scalar field would be refused.
- **The derive does not do `Option<Struct>`, maps of structs or pages**, and the
  JSON encoder writes no maps, blobs or pointers to structs (`rust/ENCODER.md`).
- **The module's decode diagnostics use code 2**, which the encoder's diagnostics
  use for a number error.
- **`bun.lock` still records the `js` workspace at 0.1.0.**
