# Plan — compact mode carries composites, and `MarshalForceCompact`

Two changes, in this order:

1. **Extend compact mode** to nested structs, arrays of structs, maps and nested
   slices — the recursion the format always allowed and the implementation never
   built.
2. **Add `colbin.MarshalForceCompact`**, which takes compact whenever the type
   permits it, instead of letting the encoder pick on size.

---

## 1. What compact mode can and cannot represent

Compact mode writes **no type tag**. Every value's type comes from the schema —
the Go `typeInfo`/`compactPlan` on the Go side, the `Schema` on the Rust side.
So the only question that decides representability is:

> can the reader name this value's type from the schema alone?

For a nested struct, a struct element, a map key and a map value: **yes** — those
types are static, `describeKind` already resolved them, and every value form in
the format is self-delimiting (`[count]`-prefixed arrays, self-delimiting packed5
frames, terminator-closed key runs). The exclusions were an implementation gap.

Genuinely impossible, and staying out:

| Excluded | Why it is structural |
| --- | --- |
| `any` / interface | The concrete type is a property of the **value**, not the type. No schema can name it. Would need a wire type tag, i.e. a different format. |
| `*T` pointing at `T`'s zero value, with omit-empty **off** | Presence *is* "named by a key"; a zero value is never named. `nil` and `&0` are the same bytes. Inherent to the omit-zero rule. Already admitted under omit-empty, where the caller has given the distinction up. |
| Cyclic types (`type Node struct{ Kids []Node }`) | Plan construction recurses without terminating. The *wire* would be fine (an empty slice is omitted, which cuts the depth), but the plan is a tree of sub-plans and building one needs a cycle memo. Out of scope; `fm.cyclic` keeps returning `opNone`. |
| More than 3 top-level records | 2 header bits. A format change, not a flag. |
| `[]int` / `[]uint` elements | Platform-dependent width never reaches the wire — the same refusal `varint` already makes. Unchanged. |

## 2. Wire forms to add

All four are appended to `compact/compact.go`'s package doc, which is the format's
definition. Nothing existing changes on the wire, so **every message written today
is byte-for-byte identical after this change**.

### 2.1 Nested struct — a nested key run

```text
[key: field id][ [key][value] ... [terminator] ]
```

The sub-record is closed by the same terminator, at the message-wide key width.
Two consequences:

- **`NARROW_KEYS` becomes message-wide, not root-wide.** Today the root's ids
  decide it. It must now fold in every nested plan's ids: one nested field with a
  hashed id ≥ 15 puts the whole message on `Keys8`.
- **A nested struct whose every field is zero is omitted entirely** — no key at
  all — so an untouched nested struct costs nothing, matching the omit-zero rule
  for scalars. This needs a recursive zero test (§3.3).

### 2.2 Array of structs

```text
[key][count: varint][ record ][ record ] ...
```

Each element is a terminator-closed key run. Omitted when `len == 0`, exactly as
scalar slices are today.

### 2.3 Map

```text
[key][count: varint][ k ][ v ][ k ][ v ] ...
```

Key restricted as `describeKind` already restricts it (int / uint / float /
string). Value may be any compact-carryable form, nested struct included.
Omitted when `len == 0`.

**Iteration order is Go's, i.e. random**, so the same map does not produce the
same bytes twice. This matches the columnar path (`null_map.go:80` iterates
unordered), so it is not a new property — but it means compact output is not
byte-stable for map-bearing types. *Flagged: say the word and I sort scalar keys
instead, at the cost of diverging from the columnar path and a sort per map.*

### 2.4 Nested slices (`[][]int`, `[]map[string]int`, ...)

Falls out of a recursive value writer for free:
`[key][count][ inner ][ inner ] ...`. Including it is less code than excluding it,
and it is what makes the rule "everything except `any`" rather than a list.

## 3. Go implementation

### 3.1 `compactOp` keeps its 16 bytes

Today: `offset uintptr` (8) + `id`,`kind`,`aux` (3) = 11, padded to 16. A composite
op needs to point at a sub-plan, so it gains `sub uint16` → 13 bytes, **still 16
after padding**. The flat op list and the single-switch hot loop survive intact;
a scalar field's cost does not change.

```go
type compactOp struct {
    offset uintptr
    id     uint8
    kind   uint8
    aux    uint8  // pointee index+1, unchanged
    sub    uint16 // index+1 into pl.subs, for composite kinds
}
```

`compactPlan` gains `subs []compactSub`, where a `compactSub` is either a
`*compactPlan` (nested struct, struct element) or the element/key/value descriptor
pair a map or nested slice needs.

### 3.2 New opcodes

`opStruct`, `opStructs`, `opMap`, `opSlice` (a slice whose element is itself
composite). The existing 23 scalar/scalar-slice opcodes are untouched, so they
keep their direct-cast fast path.

`compactValueOp` becomes recursive, returning `(kind, sub)`.

### 3.3 Recursive zero test

`compactWriteRecord` currently tests each scalar inline (`if v := *(*int8)(q); v != 0`).
Composite ops need `compactZeroAt(op, ptr) bool`:

- `opStruct`: every sub-op zero, recursively.
- `opStructs` / `opMap` / `opSlice`: `len == 0`.

Cost falls only on types that have composite fields. Rewinding the bitstream after
writing an empty sub-record was the alternative and is worse — a bit-level writer
cannot un-write.

### 3.3b Where the value switches live

Both the record writer and the record reader keep their scalar cases inlined, and
delegate only the composite kinds to a shared positional path. The writer must —
it fuses the zero test into a single load per field. The reader looked like it
could share, and measured 4% slower doing so (`BenchmarkRecordUnmarshalCodec`,
6 samples): the shared switch cannot inline, and a flat single-record decode is
nothing but those calls. Inlining the fifteen scalar cases recovered it exactly.

### 3.4 `ALL_POSITIVE` scan goes recursive

`compactAllPositivePlan` walks `pl.signed`, a flat precomputed list. It must now
descend into nested structs, struct elements and map keys/values to find signed
integers. A type with no composite field keeps the current flat walk and pays
nothing.

### 3.5 `compact` package additions

Small, since `Key`/`End` already write a nested key run:

- `Writer.Count(n uint64)` and `Reader.ArrayLen(minBits int) (int, bool)` —
  exporting what `putVarint`/`arrayLen` already do, for the composite counts.
- `Kind` gains `KindStruct`, `KindStructs`, `KindMap`, `KindSlice`. `Reader.Skip`
  **refuses** these with an error rather than guessing: stepping over a composite
  needs its sub-schema, which `Skip`'s signature does not carry. (`Skip` is
  already only usable by a caller who knows the schema; `compactReadRecord` errors
  on an unknown id regardless.)

### 3.6 `MarshalForceCompact`

```go
func colbin.MarshalForceCompact(v any) ([]byte, error)   // + codec.MarshalForceCompact
```

Package function only, per your answer — `Codec[T].Append` already forces compact
at one record, so only the slice paths could gain anything and they can wait.

Errors, rather than silently falling back, so a caller learns their type has an
`any` in it:

```text
colbin: MarshalForceCompact: type main.T cannot use compact mode (field Payload: any)
colbin: MarshalForceCompact: 7 records exceeds compact's maximum of 3
```

When it returns bytes, `compact.IsCompact(out)` is true.

### 3.7 Two usability notions, deliberately

This is the part worth arguing about. `compactUsable`'s current doc rejects arrays
of structs on a **size** premise, and that premise is correct:

> a struct holding a hundred sub-structs contains a hundred records' worth of
> columnar data and the premise [nothing to amortise over] fails.

A hundred nested structs in compact mode pay a hundred key runs where columnar
pays one column per field. Compact would often be **larger**. So automatic
`Marshal` must not blindly switch those types over:

- `compactPossible(ti)` — everything but the §1 table. Drives `MarshalForceCompact`.
- Automatic `Marshal`:
  - flat types (what compact carries today) → take compact at one record, **exactly
    as now**, no extra encode;
  - types that gained composite support → build both forms and keep the smaller,
    which is the rule 2–3 records already uses (`encode.go:127,139`).

That way no existing message gets bigger, today's fast path is untouched, and the
force function has a real job: guaranteeing compact where the encoder would have
measured columnar as smaller.

## 4. Rust decoder — deliberately left flat

`rust/src/lib.rs`'s `Kind`, `Value` and `Schema` are flat: they support **no**
composite in either mode, standard included. Nested schemas there mean
`Kind::Struct(Schema)`, `Kind::Map(..)`, `Value::Record`, `Value::Records`,
`Value::Map` and a recursive `read_value` on both `compact.rs` and `standard.rs`.

Out of scope, and it is not a regression: the Rust decoder cannot read a
nested-struct message today either. Existing vectors keep passing untouched
(no wire change to what they cover), and I will add **no** composite vectors —
they would fail on the Rust side by construction.

*Flagged: if you want the Rust decoder to keep up, that is a second plan, roughly
the same size as this one.*

## 5. Files

| File | Change |
| --- | --- |
| `compact/compact.go` | Package doc: the four new value forms, message-wide `NARROW_KEYS`. |
| `compact/writer.go` | `Count`. |
| `compact/reader.go` | `ArrayLen`, four `Kind`s, `Skip` refusal. |
| `codec/compact_plan.go` | The bulk: `sub` field, `subs` table, 4 opcodes, recursive write/read/zero/all-positive. |
| `codec/compact_mode.go` | `compactPossible` vs the automatic rule; fold nested key widths. |
| `codec/encode.go` | Thread `forceCompact`; the build-both rule for composite types. |
| `codec/{compact_mode,compact_plan}_test.go` | Round-trip every new form, nested and mixed; the `any` refusal; force-vs-auto size. |
| `colbin.go`, `doc.go` | `MarshalForceCompact` + doc. |
| `README.md`, `RATIONALE.md` | Format section and the decisions above. |

## 6. Settled

1. **Map key order** — Go's, i.e. random, matching the columnar path. Compact
   output is not byte-stable for map-bearing types, and that is not a new
   property.
2. **The automatic rule** — §3.7 as written: flat types keep today's zero-cost
   "always compact at one record"; composite types build both and keep the
   smaller. `MarshalForceCompact` is what overrides it.
3. **Field ids stay hashed.** No dense-`0..14` reassignment. `Keys4` remains
   opt-in through explicit `cb:"1"`, `cb:"2"` tags, exactly as documented today,
   so nothing already stored changes. The one consequence of §2.1: because the
   key width is now message-wide, a **nested** struct must be tagged too — one
   untagged nested field puts the whole message on `Keys8`.
4. **Compact never embeds a columnar sub-table.** A nested struct is compact's
   own nested key run. The two modes share value codecs (varint, packed5), not
   framing.
