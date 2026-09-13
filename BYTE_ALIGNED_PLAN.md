# One byte-aligned mode

A proposal to collapse colbin's three wire formats into one, built on the shape
minimal mode already proved: `[key][descriptor][payload]`, nothing packed across
a byte boundary, every size escalating to a fixed width rather than a varint run.

Everything below is a design; nothing here is implemented yet. The measurements
are real and reproducible — `go test ./experiments/bytealigned -bench .` and
`go test ./codec -bench Minimal`, both on the i7-1355U this repo already
benchmarks on.

---

## 1. What the evidence says

Two numbers frame the whole proposal.

**Byte alignment is worth ~10× on a small record, and costs ~10% of its size.**
From `codec/minimal_bench_test.go`, same ten-field record, five fields set:

| | encode | decode | bytes |
|---|---:|---:|---:|
| minimal (byte-aligned, straight-line) | 5.9 ns | 19 ns | 11 |
| minimal through a cached plan | 18.3 ns | 34 ns | 11 |
| compact (bitstream) through `Codec[T]` | 88 ns | 130 ns | 10 |

**Columns do not have to trade size for speed at all.** From
`experiments/bytealigned`, 1024 `int64` per column, against the current `varint`
(k,M) bit codec. Two candidates: a **byte-blocked** column, one byte width per
128 residuals, and a **bit-blocked** one, one exact bit width per 128 residuals.

Size, bytes per element:

| column shape | bit-varint | byte-blocks | bit-blocks |
|---|---:|---:|---:|
| small dense ids (1..900) | 1.00 | 1.14 (+14%) | **0.41 (−59%)** |
| wide base, 4096 span | 1.94 | 2.01 (+4%) | **1.52 (−22%)** |
| unix-millis timestamps | 1.53 | 2.01 (+32%) | **1.14 (−25%)** |
| prices, 0..50000, no structure | 2.00 | 2.02 (+1%) | 2.02 (+1%) |
| full-width random int64 | 8.00 | 8.01 (+0%) | 8.01 (+0%) |

Time, nanoseconds per element:

| | bit-varint | byte-blocks | bit-blocks |
|---|---:|---:|---:|
| encode | 6.6 – 14.3 | **2.6 – 2.7** | 3.0 – 3.4 |
| decode | 2.6 – 4.5 | **0.76 – 1.11** | 1.19 – 1.45 |

**The bit-blocked column is smaller than today's bit varint on every shape and
2–3× faster to decode, 2–4× faster to encode.** It is not a compromise; it
dominates. What makes that possible is one piece of arithmetic: 128 values at `w`
bits occupy `128w/8 = 16w` bytes — a whole number of bytes for every `w`, and a
whole number of 64-bit words too. A bit-packed block is byte-aligned at both
ends, wastes no padding, and carries no state across a block boundary.

Four things decide the design:

1. **Per-block widths are what make any of this work.** One width for a whole
   column costs +100% on the small-ids shape — a single outlier residual widens
   every element. A width per 128 residuals costs one header byte per 128 values
   (0.8%) and removes that entirely.
2. **The transform has to be scored against the blocked cost**, not against the
   column's widest residual. Choosing it the old way picked frame-of-reference
   for small ids and lost 76%; scoring the three candidates by what they would
   actually occupy in blocks picked delta.
3. **The width has to leave the element loop.** A width-generic inner loop
   decodes at 2.2–7.0 ns/elem, *slower* than the bit varint on wide data. The
   same data through a `switch width` decodes at 0.78–1.11. That is the
   difference between a format that is byte-aligned and one that is fast, and it
   is why no layout below lets a width, a length or an element count be anything
   but a known shape at the point of use.
4. **Bit packing keeps that property** as long as a value never depends on the
   one before it. At `w ≤ 57` the unpack is one unaligned 64-bit load, a shift
   and a mask — `v = (LE64(buf[pos>>3:]) >> (pos&7)) & mask` — with no carry
   between iterations, which is what keeps it within 0.3 ns/elem of the byte
   widths. The cost is that the decoder reads up to 8 bytes past the value it
   wants, so a read buffer needs that much slack or the last block needs a slow
   path.

So the honest trade is no longer size against speed for columns. It is
**bit-blocks (smaller, 1.2–1.5 ns/elem) against byte-blocks (0.8–1.1 ns/elem and
zero-copy aliasable)**, and §3 gives each its own job.

---

## 2. The one format

### 2.1 The header is one byte, and it is not a header

```
message := [root descriptor:1] [payload]
```

There is no header. Byte 0 is an ordinary descriptor byte (§2.3) describing the
root value, read by the same code that reads every other descriptor in the
message. Everything the three current headers carry is either subsumed into it
or moved to where the decision is actually made:

| carried today | where it goes |
|---|---|
| `formatVersion` 0x02 / 0x06 / 0x04 / 0x08 | the root descriptor is the discriminator (below); versions past the first use the 0xFE escape |
| compact's mode bit | gone — there is one format |
| compact's `shape` (object vs array of 1..3) | the root descriptor's class: `STRUCT` renders an object, `LIST`/`TABLE` an array |
| compact's `NARROW_KEYS` | the `k8` bit of whichever descriptor opened the key run — per scope, not per message (§2.2) |
| compact's `ALL_POSITIVE` | gone — sign is the `pos` bit of each integer's own descriptor, so the message-wide pre-scan that costs 60–90 ns per record disappears |
| standard's `recordCount:uvarint` | the root `LIST`/`TABLE`'s own count |
| standard's `colCount` | gone — a table's columns are keyed and bounded by its length |
| the omit-empty version byte | gone — omission is unconditional. A zero field is never written, and inside a `TABLE` an absent column key means "every row was zero", which needs no flag because the columns are keyed |

**Two rules apply to the root and nowhere else**, both because the root is the
one value whose container is the frame rather than another value:

1. **No length field.** The frame that delivers the message already states its
   length, exactly as minimal mode assumes today. So the root's `lw` bits do not
   select a byte length; for `LIST`, `MAP` and `TABLE` they still select the
   width of the element *count*, restricted to `lw ∈ {0, 2}` — a 1-byte or
   4-byte count. A root `BLOB`, `VEC` or `COL` has neither, and takes its extent
   from the frame: a root `VEC` holds `(len(message) - 1) >> w` elements.
2. **An integer is not a message.** The root class is one of `BLOB`, `VEC`,
   `COL`, `LIST`, `STRUCT`, `MAP` or `TABLE`. `INT`, `INT INLINE` and `SPECIAL`
   are refused there.

Those two rules make every legal root byte **even and ≥ 0x90**:

| root byte | root value |
|---|---|
| `0x90 0x94 0x98 0x9C` | `BLOB` — a bare string or `[]byte` |
| `0xA0`..`0xAE` even | `VEC` — a fixed-width numeric array |
| `0xB0 0xB4 0xB8 0xBC` | `COL` — a transformed numeric column |
| `0xC0 0xC2 0xC8 0xCA` | `LIST` — `0xC0`/`0xC2` heterogeneous, `0xC8`/`0xCA` homogeneous |
| `0xD0` / `0xD8` | `STRUCT`, narrow keys / wide keys |
| `0xE0` / `0xE2` | `MAP` |
| `0xE8`..`0xEE` even | `TABLE` |
| `0xFE` | a version byte follows, then a root descriptor |

That set is worth the two rules that produce it, because it is unreachable by
every message colbin writes today:

- A standard-mode message starts with 0x02, 0x04, 0x06 or 0x08.
- A compact-mode message has bit 0 set, so it is always odd.
- A minimal-mode message starts with a field header, `[key:4][flags:4]`, whose
  top nibble is a key 0..15 — so it is ≥ 0x90 only for keys 9..15, and even only
  for some of those.

So an old reader handed a new message rejects it on its version check instead of
misparsing it, and `compact.IsCompact` answers false rather than walking into a
bitstream that is not there. Only minimal mode overlaps, and minimal mode has no
discriminator to defend today either.

**0xFE is the door.** The first version spends no bytes on a version at all;
a later one spends two, by writing 0xFE and then a version byte ahead of the real
root descriptor. This is the reserved `SPECIAL` code §2.3 sets aside, chosen even
so that it inherits the property above.

### 2.2 A field is a key, a descriptor and a payload

```
narrow keys (K4):   [key:4 | desc:4]   [payload...]
wide keys   (K8):   [key:8] [desc:8]   [payload...]
```

**Key width is a property of a key run, not of a message.** It is the `k8` bit of
the descriptor that opened the run, and there are exactly three descriptors that
open one:

- **`STRUCT`** — its `k8` names the width of the field ids in the key run that
  follows. This is the root's case too: `0xD0` is a narrow-keyed root struct and
  `0xD8` a wide-keyed one.
- **`TABLE`** — its `k8` names the width of the *column* keys. A table writes one
  key per column rather than one per field per row, so the bit is doing the same
  job one level up.
- **`LIST` of structs** — under K8, through the element descriptor it carries
  (§2.3), which is itself a `STRUCT` descriptor and so brings its own `k8`.
  Under K4 a list carries no element descriptor at all and the width comes from
  the schema, like everything else K4 leaves off the wire.

A **`MAP` has no key width**, because a map's keys are values — strings, ints —
not field ids. Its `k8` position is reserved.

So a twelve-field root struct uses K4 while a forty-field nested struct inside it
uses K8, and neither constrains the other. This is the direct answer to "any
shape must be able to use 4-bit or 8-bit keys": today compact mode's
`NARROW_KEYS` is message-wide, so one badly-tagged nested struct forces every key
in the message to eight bits.

**No terminator key.** Every key run is bounded by a byte length in the
descriptor that opened it (§2.4), so a run ends when its bytes end. That gives
K4 all sixteen keys 0..15 and K8 all 256, rather than compact mode's 14 and 254,
and it removes a per-record byte.

**Keys ascend.** A writer emits fields in increasing key order. Nothing in the
format depends on it, but it lets a decoder walk its plan and the message in
lockstep instead of looking each key up.

### 2.3 The wide descriptor byte — the layout you asked for

One byte, prefix-coded so the most common field costs the least:

```
7 6 5 4 3 2 1 0
0 v v v v v v v      INT INLINE   non-negative value 0..127, no payload at all
1 c c c d d d d      class ccc, detail dddd
```

| class | name | detail | payload |
|---|---|---|---|
| 0 | `INT` | `[pos:1][n:3]` | `n` = 0 → the value is 1; 1..6 → that many magnitude bytes; 7 → 8 bytes. `pos` = 0 makes it negative. A float rides here too, but trims the **opposite end** — see below. |
| 1 | `BLOB` | `[enc:2][lw:2]` | `[len: lw]` then `len` bytes. `enc`: 0 raw, 1 packed5 lower, 2 dictionary reference, 3 packed5 upper. A packed string carries no frame header of its own — the descriptor's size and `enc` are the two things it would have repeated, and the third, the case mode its unit stream opens in, is what the second packed5 code buys. |
| 2 | `VEC` | `[w:2][pos:1][lw:1]` | `[bytelen: lw]` then the bytes. Element width `w`: 0→1B 1→2B 2→4B 3→8B. Count is `bytelen >> w` — never stored. |
| 3 | `COL` | `[kind:2][lw:2]` | `[bytelen: lw]` then a transformed column (§3): the varint replacement. |
| 4 | `LIST` | `[homog:1][—:1][lw:2]` | `[bytelen: lw][count: lw]` then the elements, shaped by `homog` (below). |
| 5 | `STRUCT` | `[k8:1][lw:2][—:1]` | `[bytelen: lw]` then a key run at the width `k8` names. |
| 6 | `MAP/TABLE` | `[sub:1][k8:1][lw:2]` | `sub` 0 = map (a run of key/value pairs), 1 = table (columnar, §4). `k8` is the column-key width when `sub`=1 and reserved when `sub`=0 — a map's keys are values, not field ids. |
| 7 | `SPECIAL` | 0 null · 1 true · 2 false · 3 NaN · 4 +Inf · 5 −Inf · 6 present-but-empty · 7 `ANY` (a type tag byte follows, then a full descriptor) · 8..13 reserved · 14 the version escape, legal at the root only (§2.1) · 15 reserved | mostly none |

`lw` selects the width of the length field that follows: `0→1 byte, 1→2, 2→4,
3→8`. There is no LEB128 anywhere. Reading a length is a branch and one
`LittleEndian.Uint{8,16,32,64}`, never a loop, never a continuation guard.

**A float trims its low end, an integer its high end.** An integer's zero bytes
are the most significant ones, so writing it little-endian and dropping the top
puts the zeros where `n` can elide them. A float's are the *least* significant —
the low mantissa bits — while its exponent and sign are never zero. Trimming a
float the same way as an integer therefore saves nothing at all, measured:

| float64 column | trimmed like an integer | trimmed at the low end |
|---|---:|---:|
| round quarters (`x/4`) | 7.99 B/value | **2.93** |
| a `float64` field fed by `float32` data | 8.00 | **4.87** |
| prices, two decimals | 8.00 | 7.81 |
| coordinates | 8.00 | 7.97 |

(`go test ./experiments/bytealigned -run TestFloatTrim -v`.) So a float writes
its IEEE-754 pattern **big-endian** and trims the trailing zero bytes, which is
the same operation on the other end of the word. The reader knows which rule
applies because it knows the field is a float — from the schema under K4, from
the `ANY` type tag under K8. Two things fall out for free: `0.0` is omitted like
every other zero, and a `float64` holding a value that is exactly a `float32` has
29 zero low bits — three whole bytes and five over — and trims to five without
anything being added to detect it.

Under K8, a **`LIST`'s elements carry a descriptor of their own**, which is how a
list of structs gets a key width, a list of blobs gets an encoding, and a list of
anys gets a type at all. `homog` says whether they share one:

```
homog = 1   [bytelen][count] [element desc:1]   ( [len: lw] [body] )*
homog = 0   [bytelen][count]                    ( [desc:1] [payload] )*
```

The homogeneous form is the normal one — the element descriptor is written once
and every element is then just a length and a body, so a list of structs costs
one byte for the whole list rather than one per element. `homog = 0` is what an
`[]any` needs, and what a list whose elements differ in size class can fall back
to. Either way the element's own descriptor is where `k8`, `enc` and `lw` come
from, so `LIST` itself spends no detail bits naming its element type. K4 takes
all of that from the schema and carries a bare count instead (§2.6).

Sizes this produces, per present field:

| field | K8 | K4 |
|---|---:|---:|
| `uint` 0..127 | 2 B | 2 B |
| `uint` 128..255 | 3 B | 2 B |
| `bool true` | 2 B | 1 B |
| `string`, ≤255 B | 3 + n | 2 + n |
| nested struct | 3 + len bytes + body | 2 + len bytes + body |

(Against compact mode the comparison is only meaningful per message, since its
fields do not occupy whole bytes: the benchmark record is 11 bytes byte-aligned
against 10 bit-packed.)

**A wide key costs exactly one byte per present field over a narrow one**, plus
one more for integers in 128..255. That is the honest price of 256 keys, and it
is why the inline form exists: without `[0][value:7]`, every small integer would
have paid two bytes extra instead of none.

Except that the key does not have to be written at all — see §2.7, which is where
that price goes away and takes the case for K4 with it.

### 2.4 Everything wide is skippable

Every class except `INT`, `INT INLINE` and `SPECIAL` begins its payload with a
byte length, and those three have a length derivable from the descriptor alone.
So a reader that has never heard of a key can step over it:

```go
switch {
case desc < 0x80:            at += 2                       // inline integer
case desc>>4 == 0x8:         at += 2 + intBytes(desc)       // integer
case desc>>4 == 0xF:         at += 2                        // special
default:                     at += 2 + lwBytes(desc) + length
}
```

This is the single largest capability gain over all three of today's modes, none
of which can skip an unknown field. It buys:

- **Schema evolution without a coordinated deploy.** Minimal mode's documented
  "what it cannot do" disappears.
- **Interfaces.** `SPECIAL/ANY` carries a type tag and the value that follows is
  an ordinary field — self-describing, and skippable even when the tag is
  unknown.
- **Lazy decode.** Pull one field out of a fifty-field record at `memcpy` speed
  without touching the other forty-nine. This is a performance gain that the
  length prefixes are *paying for*, which is the shape of the whole trade.

K4 does not get this: four descriptor bits cannot hold both a class and a size.
**K4 is the schema-driven form and K8 is the self-describing one**, and because
key width is per-scope a message can use each where it fits.

### 2.5 The narrow descriptor nibble

K4 has four descriptor bits and no room for a class, so the class comes from the
schema and the nibble means something different under each — which is what
minimal mode already does. One rule decides how each class bounds itself:

> **A keyed run is bounded by a byte length. A counted run is bounded by a count
> and self-delimiting elements.**
>
> `STRUCT` and `TABLE` hold keyed runs and carry a byte length. `BLOB`, `VEC`,
> `LIST` and `MAP` hold counted runs and carry a count. K8 additionally gives
> *every* composite a byte length, because a K8 reader may not have the schema
> and has to be able to skip blind (§2.4); K4 never needs one, because a K4
> reader always has the schema.

That rule, plus "no class bits", is the whole of the difference between the two
key widths. §2.6 writes both out bit by bit.

Four changes from today's minimal:

- **The integer size code becomes a direct byte count**, so the widths are
  `1,2,3,4,5,6,8` where today they are `{1,2,3,4,6,8}`. 5-byte integers become
  representable — a byte saved on every value in `[2^32, 2^40)`, which is where
  a millisecond timestamp offset or a large row id lands — and nothing is lost,
  since 7-byte values already round up to 8.
- **`more=1` escalates to a fixed 4-byte size, not a LEB128 run.** Costs 2–3
  bytes on a blob past 2047 bytes (≤0.15% of it) and deletes
  `appendSizeExtension`, `continuedSize`, the nine-byte continuation guard and
  `ErrSizeTooLarge` along with the class of bug they exist to prevent. The same
  goes for a string array's per-element length: one byte, with `0xFF` escaping
  to a `uint32`, rather than LEB128. The escape can pick its own width for free:
  when `more = 1` the three size-high bits carry nothing, so they select a 2-,
  4- or 8-byte length instead, and a 5 KB string pays one extra byte rather than
  three.
- **Little-endian**, where minimal is big-endian today. A fixed-width element is
  then one native load on every machine this runs on, and a variable-width
  integer is a `Uint64` load and a mask rather than a shift chain.
- **No `ANY` in K4.** A dynamic type cannot come from a schema, so an interface
  field forces its scope to K8. This is the sharpest statement of what the two
  widths are: K4 is the schema-driven form, K8 the self-describing one.

### 2.6 Every field, bit by bit

`B.b` is byte `B`, bit `b`, with bits numbered 1..8 from the most significant.
So `0.1–0.4` is the high nibble of byte 0, `0.5` is the bit below it, and
`1.1–1.8` is all of byte 1. Offsets are from the start of the field, not of the
message.

Two things hold in every row:

- **K4 packs the key and the descriptor into one byte; K8 spends a byte on
  each.** So a K8 field's descriptor is byte 1, and its payload starts at byte 2.
- **The root descriptor is always the K8 form** (§2.1), because the root has no
  key to share a byte with. Its `k8` bit then names the key width used *inside*
  it, which is how a message can be wide at the root and narrow within, or the
  reverse.

| class | name | 4-BIT KEY (K4) | 8-BIT KEY (K8) |
|---|---|---|---|
| 0 | `INT` `FLOAT`<br>**signed** | `0.1–0.4` Key (0..15)<br>`0.5` Positive? 1 = magnitude, 0 = negative<br>`0.6–0.8` n: 0 → the value is 1 · 1–6 → n bytes · 7 → 8 bytes<br>`1.1–…` Magnitude, n bytes LE (absent when n = 0)<br>*a float instead writes its pattern big-endian, low zero bytes trimmed (§2.3)* | `0.1–0.8` Key (0..255)<br>`1.1` **0 = inline**<br>`1.2–1.8` Value, 0..127 — no payload<br>— or —<br>`1.1` **1 = explicit**<br>`1.2–1.4` Class `000`<br>`1.5` Positive?<br>`1.6–1.8` n, as K4<br>`2.1–…` Magnitude, n bytes LE<br>— or the varint below, when it is shorter — |
| 0 | `INT` `FLOAT`<br>**unsigned**<br>*(K4 only)* | `0.1–0.4` Key (0..15)<br>`0.5–0.8` code: 0–7 → **the value itself**, no payload · 8–15 → a magnitude of (code − 7) bytes<br>`1.1–…` Magnitude, (code − 7) bytes LE<br>*no sign bit: a K4 reader has the schema and knows the field is unsigned, so all sixteen codes carry information and the widths are exact — seven bytes costs seven* | *K8 has no separate unsigned form: its key already costs a byte, so the inline and varint forms below cover the same ground* |
| 7 | `INT` **varint**<br>*(K8 only)* | *—* | `0.1–0.8` Key<br>`1.1` 1<br>`1.2–1.4` Class `111`<br>`1.5` **1 = varint**<br>`1.6–1.8` Value bits 0–2<br>`2.1–…` ( `x.1` More? · `x.2–x.8` next 7 value bits )+ — at least one byte<br>*raw when the field is unsigned, zigzag when signed; the writer picks this over class `000` only when it is shorter, so no value ever grew* |
| 1 | `BLOB` | `0.1–0.4` Key<br>`0.5` More? 0 = 11-bit size · 1 = escape<br>`0.6–0.8` Size, high 3 bits *(More=0)*<br>`1.1–1.8` Size, low 8 bits *(More=0)* → 0..2047<br>`0.6–0.8` escape *(More=1)*: 0→u16 size · 1→u32 · 2→u64 · **3→packed5 lower, u8 size** · **4→packed5 upper, u8 size** · **5→packed5 lower, u32 size** · **6→packed5 upper, u32 size** · 7 rsv<br>`1.1–…` Size, the escape's width<br>`then` Content, Size bytes<br>*a raw blob's enc comes from the schema; a packed one says so here, because the schema cannot know whether a given value packed* | `0.1–0.8` Key<br>`1.1` 1<br>`1.2–1.4` Class `001`<br>`1.5–1.6` enc: 0 raw · 1 packed5 lower · 2 dict ref · **3 packed5 upper**<br>`1.7–1.8` lw: 0→1B · 1→2B · 2→4B · 3→8B<br>`2.1–…` Size, lw bytes LE<br>`then` Content, Size bytes |
| 2 | `VEC` | `0.1–0.4` Key<br>`0.5` Positive? 0 = two's complement at width w<br>`0.6–0.7` w: 0→1B · 1→2B · 2→4B · 3→8B<br>`0.8` More? 0 = 8-bit count · 1 = 32-bit count<br>`1.1–1.8` Count *(More=0)* → 0..255<br>`1.1–4.8` Count, u32 LE *(More=1)*<br>`then` Count × w bytes, each element LE | `0.1–0.8` Key<br>`1.1` 1<br>`1.2–1.4` Class `010`<br>`1.5–1.6` w<br>`1.7` Positive?<br>`1.8` lw: 0→1B · 1→4B<br>`2.1–…` Byte length, lw bytes LE<br>`then` that many bytes; Count = length >> w |
| 3 | `COL` | `0.1–0.4` Key<br>`0.5–0.6` kind: 0 = blocked (§3) · 1–3 rsv<br>`0.7–0.8` lw<br>`1.1–…` Byte length, lw bytes LE<br>`then` `[transform:2][zigzag:1][—:5]`, a 8-byte base if delta or FOR, then blocks | `0.1–0.8` Key<br>`1.1` 1<br>`1.2–1.4` Class `011`<br>`1.5–1.6` kind<br>`1.7–1.8` lw<br>`2.1–…` Byte length, lw bytes LE<br>`then` the column, as K4 |
| 4 | `LIST` | `0.1–0.4` Key<br>`0.5` More? 0 = 11-bit count · 1 = 32-bit count<br>`0.6–0.8` Count, high 3 bits *(More=0)*<br>`1.1–1.8` Count, low 8 bits *(More=0)* → 0..2047<br>`1.1–4.8` Count, u32 LE *(More=1)*<br>`then` Count × ( Len: 1 byte, `0xFF` → u32 LE follows · Body )<br>*element class comes from the schema; no `[]any`* | `0.1–0.8` Key<br>`1.1` 1<br>`1.2–1.4` Class `100`<br>`1.5` homog<br>`1.6` —<br>`1.7–1.8` lw<br>`2.1–…` Byte length, lw bytes LE<br>`…` Count, lw bytes LE<br>*homog=1:* one element descriptor byte, then Count × ( Len: lw · Body )<br>*homog=0:* Count × ( Descriptor: 1 byte · Payload ) — this is `[]any` |
| 5 | `STRUCT` | `0.1–0.4` Key<br>`0.5` k8 — key width *inside*: 0 = 4-bit, 1 = 8-bit<br>`0.6–0.7` lw<br>`0.8` —<br>`1.1–…` Byte length, lw bytes LE<br>`then` a key run of exactly that many bytes | `0.1–0.8` Key<br>`1.1` 1<br>`1.2–1.4` Class `101`<br>`1.5` k8<br>`1.6–1.7` lw<br>`1.8` —<br>`2.1–…` Byte length, lw bytes LE<br>`then` a key run of exactly that many bytes |
| 6 | `MAP`<br>(sub=0) | `0.1–0.4` Key<br>`0.5` sub = 0<br>`0.6` More? 0 = 10-bit count · 1 = 32-bit count<br>`0.7–0.8` Count, high 2 bits *(More=0)*<br>`1.1–1.8` Count, low 8 bits *(More=0)* → 0..1023<br>`1.1–4.8` Count, u32 LE *(More=1)*<br>`then` Count × ( key payload · value payload ), both shaped by the schema | `0.1–0.8` Key<br>`1.1` 1<br>`1.2–1.4` Class `110`<br>`1.5` sub = 0<br>`1.6` — *(a map has no key width: its keys are values)*<br>`1.7–1.8` lw<br>`2.1–…` Byte length, lw bytes LE<br>`…` Count, lw bytes LE<br>`then` Count × ( Desc·Payload key · Desc·Payload value ) |
| 6 | `TABLE`<br>(sub=1) | `0.1–0.4` Key<br>`0.5` sub = 1<br>`0.6` k8 — width of the **column** keys<br>`0.7–0.8` lw<br>`1.1–…` Byte length, lw bytes LE<br>`…` Row count, lw bytes LE<br>`then` one keyed column per field, each a `VEC`, `COL` or `LIST` of Row count elements | `0.1–0.8` Key<br>`1.1` 1<br>`1.2–1.4` Class `110`<br>`1.5` sub = 1<br>`1.6` k8<br>`1.7–1.8` lw<br>`2.1–…` Byte length, lw bytes LE<br>`…` Row count, lw bytes LE<br>`then` the columns, as K4 |
| 7 | `SPECIAL` | `0.1–0.4` Key<br>`0.5–0.8` detail: 0 null · 1 true · 2 false · 3 NaN · 4 +Inf · 5 −Inf · 6 present-but-empty · 7 unavailable<br>*details 8–15 are the varint integer above — bit `.5` set means the rest is a value, not a code*<br>*no payload* | `0.1–0.8` Key<br>`1.1` 1<br>`1.2–1.4` Class `111`<br>`1.5–1.8` detail, as K4 but 7 = `ANY`<br>*`ANY`:* `2.1–2.8` type tag, then a full descriptor and its payload |

Worked example — the benchmark record of `codec/minimal_bench_test.go`,
`CompanyID=7 UserID=42 RouteID=103 CPU=5 Access1=0x0139`, five of ten fields set,
all keys ≤ 9 so the root picks K4:

```
byte  0   D0            root: STRUCT, k8=0 (narrow keys), no length — the frame bounds it
byte  1   09            0.1-0.4 = 0   key 0, CompanyID
                        0.5     = 1   positive
                        0.6-0.8 = 1   one magnitude byte
byte  2   07            the 7
byte  3   19            key 1, positive, one byte
byte  4   2A            the 42
byte  5   29            key 2, positive, one byte
byte  6   67            the 103
byte  7   39            key 3, positive, one byte
byte  8   05            the 5
byte  9   6A            key 6, positive, two bytes
byte 10   39            0x0139 little-endian, low byte first
byte 11   01
```

12 bytes: the root descriptor plus the 11 that minimal mode writes today. Keys
4, 5, 7, 8 and 9 hold zero and cost nothing.

### A note on the wide integer

The sketch this came from spends the wide form's second byte on `[Is > 0][7-bit
varint]` and continues with LEB128. The layout above keeps the first half of that
— `1.1 = 0` with a 7-bit value is the same two-byte field for the same 0..127 —
and then diverges: instead of continuing, it switches to `[Positive][n]` and a
counted run of magnitude bytes. The reason is §1's third finding. A continuation
is a loop whose trip count is data, and every loop of that shape in this format
has been replaced by a length and a straight run.

### 2.7 Don't write the key — write a presence bitmap

A `STRUCT` writes its fields in ascending key order (§2.2) and omits the zero
ones. So the keys it writes are a *subset of a known set, in order* — which is a
bitmap, not a sequence of numbers. Under the `dense` bit of the `STRUCT`
descriptor:

```
dense = 0    ( [key] [desc] [payload] )*                      as §2.6
dense = 1    [bitmap: b bytes] ( [desc] [payload] )*          keys implied by set bits
```

Bit *i* of the bitmap means key *i* is present; the fields follow in that order,
each stripped of its key. Skipping still works — the descriptors still carry
their lengths — and so does naming an unknown field, because its key is its bit
position.

> **Measured, and half of this was wrong.** It is smaller — more so than the
> arithmetic below predicted — and it is **slower**, by about 3.5×, after two
> rounds of optimisation. The corrected numbers and what they mean for K4 are in
> `RATIONALE.md`; this section is kept as written, with its error marked, because
> the reasoning is what the measurement had to be run against.

**It was expected to be smaller and faster at the same time.** Faster because
there is no key to read and no key to look up: the decoder walks its plan and the
set bits in lockstep, which is the O(f²) `find` scan of §5.2 deleted rather than
optimised. ~~Smaller~~ — that part holds — whenever `1 + b < p` for `p` present
fields, so with `b = 2` (keys 0..15) it wins from four present fields up.

The arithmetic on the benchmark record — ten fields, five set, values 7, 42, 103,
5 and 313:

| | bytes |
|---|---:|
| K8, keys written | 13 |
| K4, keys written | 12 |
| **K8, bitmap** | **11** |

and on the same ten fields all present and all small:

| | bytes |
|---|---:|
| K4, keys written | 21 |
| K8, keys written | 21 |
| **K8, bitmap** | **14** |

K8 with a bitmap wins those two because a small integer is *one byte* there — the
inline form `[0][value:7]` is the whole field — where K4 spends a descriptor byte
and a value byte and has no nibble left to inline anything into.

**But it does not win everywhere, and the field-by-field table is what decides
it**, not the two examples above:

| field | K4 | K8, bitmap |
|---|---:|---:|
| `uint` ≤ 127 | 2 B | **1 B** |
| `uint` 128..65535 | 2–3 B | 2–3 B |
| `bool true` | 1 B | 1 B |
| `string` ≤ 255 B | 2 + n | 2 + n |
| **`string` 256..2047 B** | **2 + n** | 3 + n |
| float, array, nested struct | equal | equal |
| fixed overhead | **0** | 1 length byte + ⌈keys/8⌉ bitmap bytes |

So K4 still wins on:

- **Very sparse records.** Two present fields of ten: 5 bytes against 6.
- **Blob-heavy records.** K4's 11-bit inline size covers a 2047-byte string in
  two header bytes; K8's `lw` ladder needs three past 255. Three 300-byte strings
  and two small integers: 911 bytes against 915.
- **Anything where the fixed overhead is not amortised**, which is the same
  statement as the first two.

And K8-with-bitmap wins on records with several present small integers, on every
struct past sixteen fields — which K4 cannot carry at all — and on everything
that needs skipping, `ANY` or schema evolution.

**So the case for dropping K4 is not that it loses on bytes. It is that it is a
second encoding of everything.** After §2.7 there are three ways to frame a key
run — K4 keyed, K8 keyed, K8 bitmap — and each needs its own writer, reader,
fuzz corpus and Rust port. K8 keyed and K8 bitmap are both load-bearing (sparse
against dense, and the writer picks per record for free). K4 is the third, it
wins by one or two bytes on a narrow band, and it is the only one of the three
that also costs the sixteen-field ceiling, `Skip`, and `ANY`.

That is an argument, not a conclusion — and the measurement settled it the other
way. **K4 stays.** The bitmap, which was the case for dropping it, is 3.5× slower
than the narrow key; the third framing costs a prologue per port, not a format,
and all three are now implemented. K4 is what a latency-bound wire wants, the
bitmap is what a size-bound one wants, and the keyed wide form is what a struct
past sixteen fields or an unknown field wants.

---

## 3. `varint/`, byte-blocked

The (k,M) bit varint is replaced by a blocked column. The transforms survive —
they are where most of the compression actually came from — and only the
*unbounded* bit packing goes: bits still pack, but never across a block.

```
column := [transform:2][zigzag:1][—:5]
          [base: 8 bytes]            (delta and frame-of-reference only)
          block*                     128 residuals each
block  := [width: 1 byte] [payload]
```

**Two block kinds, selected by `COL`'s `kind` field, for two different jobs.**

- **`kind = 0`, bit-blocked.** `width` is a bit count 0..64 and the payload is
  `16 × width` bytes. The default for a column: smaller than today's varint on
  every shape measured, and 2–3× faster to read.
- **`kind = 1`, byte-blocked.** `width` is a byte count from `0,1,2,3,4,6,8` and
  the payload is `128 × width` bytes. Larger — +1% to +32% — but 0.8–1.1 ns/elem
  instead of 1.2–1.5, and a block at width 4 or 8 is a **native little-endian
  array that can be aliased rather than decoded**. That is what a caller wants
  when the column is about to be handed to a numeric kernel.

In both kinds **width 0 is a block of nothing but zeros and occupies one byte**,
which is what makes a constant or sparse column nearly free.

Four rules the measurements insist on:

1. Delta and frame-of-reference write their base in the clear at full width
   ahead of the blocks. Folding the first value into the residuals sets the
   column's width from one element — that alone cost +97% on the timestamp
   shape before it was fixed.
2. The transform is chosen by scoring each candidate's *blocked* size, not its
   widest residual. Three O(n) scans, no bit work; the experiment ORs the
   residuals together per block and takes one `bits.Len64`, so there is not even
   a compare in the inner loop.
3. Encode and decode both hoist the width out of the element loop. Without that
   the format is byte-aligned and slow.
4. The bit unpack must stay carry-free: one load, one shift, one mask per value,
   never a running accumulator. That holds for `width ≤ 57`; 58..64 take an
   out-of-line path, and a column that reaches those widths is incompressible
   anyway.

The element type stays out of the wire, as it is today.

### Two column kinds worth adding, both smaller *and* faster

- **`kind = 2`, constant.** Every value is the same: the payload is that one
  value and the decode is a fill loop. Common in a `TABLE` — a status column, a
  tenant id, a version. Strictly smaller and strictly faster than 128-value
  blocks of a repeated number.
- **Dictionary string columns.** `BLOB`'s `enc = 2` is already reserved for a
  dictionary reference; a `TABLE` string column should use it. For `N` rows drawn
  from `C` distinct values of average length `L`, raw costs `N(1+L)` and the
  dictionary costs `C(1+L) + Nw` with `w` one or two bytes. Ten thousand rows of
  an eight-value status string: ~110 KB against ~10 KB. And it *reads* faster,
  because the codes are a `VEC` and the `C` strings are materialised once instead
  of `N` times. This is the largest size win available anywhere in the format and
  it costs nothing in time.

### Considered and not recommended

- **Stream-vbyte** (2 control bits per element in a side stream, 1..4 data
  bytes, decoded through a 256-entry shuffle table). Faster still on x86, but it
  is fixed at four widths, needs SIMD to beat the blocked form, and its control
  stream is a second cursor to bounds-check. The blocked forms get the speed with
  one cursor and no intrinsics.
- **Keeping the (k,M) bit varint.** Its whole advantage was sub-byte
  granularity, and the bit-blocked column has that with a finer ladder and a
  carry-free read. There is now no shape where it wins.

---

## 4. The columnar shape, inside the one format

The standard mode does not disappear — it stops being a *mode* and becomes a
field class.

```
[key][desc = MAP/TABLE, sub=1][bytelen][rowcount]
    [key][desc][payload]     one column per field: a VEC, a COL, a LIST
    ...
    [presence bitmaps]
```

A column's payload holds `rowcount` elements and carries no per-row keys, which
is the entire reason columnar layout wins: the key that costs 4 or 8 bits per
field per row is written once for the column.

So an array of structs has two encodings and the writer picks:

- **`LIST` of `STRUCT`** — a key run per element. Wins while the per-column
  framing has too few rows to amortise, which is today's compact-vs-standard
  decision at three records.
- **`TABLE`** — transposed. Wins from some threshold upward, and its columns
  decode through `loadRun`, so a large table decodes at the ~1 ns/element the
  table in §1 measures rather than the ~20 ns/record a key run costs.

The threshold is one measurement, not a constant to argue about, and it is now a
*per-field* decision rather than a per-message one: a record can hold a
three-element list and a ten-thousand-row table and encode each the right way.
Today that record has to pick one mode for the whole message.

---

## 5. Where the remaining performance is

Byte alignment is the floor, not the ceiling. In rough order of payoff:

1. **Generated codecs.** Measured today: 5.9 ns straight-line against 18.3 ns
   through a cached plan on encode, 19 ns against 34 ns on decode. A `go:generate`
   that emits the `Writer`/`Reader` calls is a 3× encode win with no format
   change at all, and it is the single biggest lever in this document.
2. **Kill the linear key lookup.** `minimalPlan.find` scans the field list per
   field — O(f²) per record. A `[16]int8` for K4 and a cached `[256]int8` for K8
   make it one load. This is pure profit on every reflective decode.
3. **Pre-grow, then store by index.** Every `append` in the writer re-checks
   capacity. A per-type upper bound on the fixed part of a record lets the
   encoder grow once and store, turning ten capacity checks into one.
4. **Zero-copy strings, opt-in.** `UnmarshalMinimal` allocates per string. A
   reader flag saying "the buffer outlives the record" lets a string field be
   `unsafe.String` over the message — which is only possible *because* the
   payload is byte-aligned. In a bitstream a string is not addressable at all.
5. **SIMD on `VEC` and `COL`.** `loadRun`'s widening loads are exactly
   `VPMOVZXBQ`/`VPMOVZXWQ`/`VPMOVZXDQ`. The scalar hoist already gives 3–4×; the
   vector form is the next factor.
6. **Lazy field access**, per §2.4.

### Dispatch on key width once per key run, never per field

The key width must not be a variable the field loop can see. Measured, the same
five-of-ten-field record, `go test ./experiments/bytealigned -bench KeyRun`:

| | one decoder carrying `k8 bool` | a decoder per width |
|---|---:|---:|
| K4 | 11.7 ns | **8.70 ns** (1.35×) |
| K8 | 10.2 ns | **8.74 ns** (1.17×) |

The branch itself is not what costs. It goes the same way on every field of every
record, so it predicts perfectly; what costs is that a width the compiler cannot
see is a width it cannot fold — the field stride stops being a constant, the
descriptor's position stops being a constant, and the function grows past the
budget that would have inlined it. This repo has met that effect before: the
minimal README records a ten-field encode going from 16.9 ns to 5.8 ns purely by
giving each Go width its own entry point so the inliner would take it.

Three rules follow, and the third is the one that keeps this from doubling the
codebase:

1. **Split at the function, not inside the loop.** `if k8 { … } else { … }`
   *per field* is the 11.7 ns column above — it pays the duplication and keeps
   the variable. Two functions, each with the width baked in, is the 8.70.
2. **Dispatch per key run, not per message.** Key width is a property of a scope
   (§2.2), so a K4 struct may hold a K8 one. One indirect call as a run is
   entered, amortised over all its fields, is the right granularity — and for the
   plan-driven writer it means the plan holds `writeRun` selected once per type,
   rather than a width field every field has to test.
3. **Split the framing, share the value codecs.** What differs between the widths
   is reading the key and decoding the descriptor. The magnitude load, the blob
   slice, the column unpack and the composite recursion are byte-identical and
   must stay single-sourced — the benchmark above shares its `store` and still
   gets the full win. Duplicating those would double the bug surface for nothing.

Two notes for the ports. In **Rust** this is a const generic — `Reader<const
KEY_BITS: usize>` monomorphises for free and the source is written once. In
**Go** it has to be duplicated source or generated code: two distinct empty type
parameters share a GC shape, so a generic reader goes through a dictionary and
the constant never folds. And under **generated codecs** (item 1 above) the
question disappears for the hot path entirely, because the width is a literal in
the emitted code — the split only matters for the reflective decoder.

This also sharpens §2.7's open question. There are three framings to split
across, not two — K4, K8 keyed and K8 bitmap — and each is a separate function
in both ports.

---

## 6. What this costs, stated plainly

After the second analysis round, most of what this was going to cost has been
bought back. What is left:

| | cost |
|---|---|
| Small records | ~+10% bytes against compact mode (11 vs 10 on the benchmark record), and −1 with the §2.7 bitmap — a wash |
| Integer columns | **none.** Bit-blocked columns are −59% to +1% against today's varint (§1) |
| `bool true` | 1 byte, against ~1 bit packed. Only a record of many *true* bools loses; a false one is omitted |
| Strings | packed5's ~5 bits/char is gone by default. It survives as `BLOB enc=1`/`enc=3`, opt-in, and a dictionary column (§3) beats it outright on the repetitive data packed5 was best at. It no longer forces K8: a narrow blob header says "packed" in its escape code, so the encoding costs what it weighs instead of a byte per key on the whole message |
| Sparse wide-key records | the §2.7 bitmap loses ~1 byte below three present fields |
| Blobs over 2047 B | +1 byte for the fixed-width length escape, once the escape picks its own width (§2.5) |
| Read buffers | a bit-blocked column reads up to 8 bytes past its end, so a buffer needs that slack or the last block needs a slow path |
| Compatibility | Total. Three wire formats are replaced by one; the mode discriminator, the version bytes and the cross-language vectors all go |

The honest summary is no longer "trade ~10% of size for ~10× of speed". It is
**the same size or smaller, and several times faster** — with the compatibility
break, the 8-byte read-ahead, and the loss of packed5-by-default as the real
prices.

The one idea still rejected on arithmetic is a presence bitmap under **K4**,
where it loses: K4's key rides in the descriptor byte the field needs anyway, so
removing the key removes nothing and the bitmap is pure addition. That asymmetry
is the whole reason §2.7 is a K8 optimisation and an argument for dropping K4.

---

## 7. Order of work

Each phase is independently useful and independently revertable.

1. **`varint/` → blocked columns**, bit-blocked by default. Self-contained,
   already prototyped and measured in `experiments/bytealigned`, and the only
   phase that can land without touching anything else — it is also the one phase
   that is a strict improvement on today's bytes as well as today's speed, so it
   needs no argument. It changes the standard mode's wire, so the vectors
   regenerate and the Rust port moves with it.
2. **Fixed-width size escapes in `minimal/`**, replacing LEB128, and the float
   trim fix (§2.3). Small, isolated, deletes code.
3. **The wide descriptor byte and K8 keys in `minimal/`**, with key width per
   scope, and the §2.7 bitmap measured against a real corpus — because that
   measurement is what decides whether K4 is built at all. This is where minimal
   stops refusing types with more than sixteen fields, and where skipping starts
   working.
4. **Composites**: `STRUCT`, `MAP`, `LIST` — the same recursion
   `COMPACT_COMPOSITES_PLAN.md` describes, on the byte-aligned frame.
5. **`TABLE`**, and the writer's row-wise-vs-columnar choice. At this point the
   standard mode is expressible in the new format and can be deleted.
6. **Retire compact mode**, the mode discriminator and `packed5` as a default.
7. **Generated codecs** (§5.1), which is where the numbers in §1 stop being a
   ceiling.
8. **Rust port and cross-language vectors**, moving with phases 1, 3 and 5
   rather than after them — the repo's existing discipline, where neither side
   can move without the other failing.

---

## 8. Open questions

- **The threshold** in §4 where `TABLE` overtakes `LIST` of `STRUCT`. Needs
  measuring, not deciding.
- **Block size.** 128 was assumed, not tuned. 64 halves the outlier blast radius
  and doubles the header overhead to 1.6%; worth a sweep across the five shapes.
- **Whether K4 should exist at all** — still open, but sharper after §2.7. The
  question is no longer "is K4 smaller" (sometimes: sparse records, and blobs of
  256..2047 bytes) but "is a third framing of a key run worth one or two bytes on
  that band". Deciding it needs a corpus of real records, not more arithmetic.
  Note that it is a decision about *structs only*: a key width is a property of a
  key run, and `VEC`, `COL`, `BLOB` and `LIST` have no field keys to widen.
- **The root count width.** Restricting the root's `lw` to `{0, 2}` (§2.1) is
  what keeps every root byte even, and it costs three bytes on a root array of
  256..65535 elements that would otherwise have used a 2-byte count. That is a
  small price for the property, but it is a real one and it is the only place in
  the format where a size is rounded up for a reason that is not speed.
