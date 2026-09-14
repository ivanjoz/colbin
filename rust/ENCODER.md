# JSON text in, colbin bytes out

The specification of `colbin::build::encode` — the one thing colbin does that
needs no type at compile time. Everything else in this repository is checked
against Go, because Go is the specification. This is not: `codec/` has no
inference, the `README` says so outright, and `#[derive(Colbin)]` knows its
fields before it runs. So the rules live here, and the implementation in
`rust/src/{json,infer,build,verify}.rs` is held to them by
`rust/tests/{parse,infer,build,verify}.rs`.

They were written for the AssemblyScript module this replaced (`web/PLAN.md` §3
and §4, and `web/REFACTOR_PLAN.md` §5.1–5.2, both deleted with it in
`RUST_WASM_PLAN.md` phase 8). The rules did not change with the language: a
document has to infer to the same schema in either, or the same input encodes to
different bytes.

---

## 1. Shape

A colbin message is **one struct**. JSON's top level often is not, so:

| JSON top level | what is built |
|---|---|
| an object | that object is the record; the root plan is its struct |
| anything else | a one-field **envelope**, named `rows`, holding the document |
| `[]`, `null` | refused — there is nothing to infer |

The envelope is the same one `codec/envelope.go` puts round a Go slice, down to
the field being called `rows`, because two ports writing the same document have
no business writing different bytes for it. It is marked in the schema section —
a `structDef`'s flags byte, bit 1 — rather than guessed at on the way back. The
root byte was the wrong place for it: those detail bits belong to the format and
a reader refuses every one it does not assign, so a message marked there would
have been refused outright.

Two things follow, and the implementation holds to both:

- **The envelope is only ever built when the top level is not an object**, so it
  can never collide with a caller's own `rows` key.
- **The walk unwraps it**, so encode-then-decode returns the array the caller
  gave.

The first draft of this rule cost a textual disagreement — Go, which did not know
the bit, rendered `{"rows":[…]}` where the module rendered `[…]`. It does not any
more: `codec/schema.go` writes `schemaEnvelope` for the slice or map it carries
at a root and unwraps it on the way back, so both ports render the same document
for the same bytes. Go still refuses a *bare scalar* at the root, which this
wraps; that asymmetry is on the writing side only.

## 2. Types

Every record is walked before anything is decided; each field unifies across all
of them.

| observed across records | inferred |
|---|---|
| `true` / `false` | `bool` |
| integers only | `int64` — **never narrowed** |
| an integer above int64 max, every value non-negative | `uint64` |
| **any one value fractional or exponential** | `float64` for the whole field |
| string | `string` |
| object | a nested struct, recursively |
| array of objects | a slice of structs — list or table, decided per field on the row count |
| array of integers / of strings | the matching array op |
| `null`, or the key absent from some record | a **pointer** to the inferred type |

`int` + `float` in one field unify to float64. That is the only widening;
everything else is a conflict (§4).

Integers are never narrowed to the smallest type that holds them, which sounds
like free bytes and is not: the wire already omits a zero field entirely and
trims an integer's leading zero bytes per value, so a `u8` column and an `i64`
column of the same small numbers cost the same. Narrowing would only add a way
for record 500 to not fit what record 0 implied.

## 3. Field ids are sequential, in first-seen order

**First-seen order**, across the record array, depth-first per object — and the
id *is* the position: 0, 1, 2.

Go derives an untagged field's id from `fnv8` of its name and probes past
collisions, which lands anywhere in 0..255 and so puts every message on eight-bit
keys. Numbering instead puts any object of sixteen fields or fewer on the
four-bit fast path, which is a byte per present field smaller and about 1.7×
faster to decode. The section carries the ids, so nothing that reads the section
can be confused by it.

What that trades away: an encoded message no longer drops into an *untagged* Go
struct with no section at all. That property only held when the Go struct was
declared in the same order as the JSON's first-seen keys, which is not a promise
anyone should lean on, and it cost the fast path on every message to keep.

First-seen order stays exactly as load-bearing as it was, for a different reason:
it is no longer the input to a hash, it *is* the id. Two implementations that
walk differently produce different bytes for identical input.

With §2 in front of it, this is the **only** thing that puts a run on the wide
path: more than sixteen fields in one object.

---

## 4. What is refused, and what only warns

A field holding two irreconcilable types across records is refused:

```json
{ "code": 4, "offset": 412, "line": 14, "path": "[3].qty",
  "message": "type conflict: string, but records 0-2 had number", "warnings": [] }
```

Refused rather than promoted to an untyped column, deliberately. A column of
"whatever this row had" is not a column: it abandons the layout for that field
and the caller never learns why. Refusing is the honest failure.

The rest, all of them errors:

| condition | why |
|---|---|
| more than 256 fields in one object | a key is one byte |
| a number fitting neither int64, uint64 nor float64 exactly | silent precision loss is the failure mode colbin exists to avoid |
| nesting deeper than 64 | the encoder recurses, and unbounded input must not reach the stack. At or below the decoder's bound, so what this writes it can always read back |
| an array of floats, of booleans, or of arrays | the format has no op for one. `sliceOp` in `codec/codec.go` resolves integers, strings and structs and refuses the rest |
| malformed JSON | reported with a byte offset and a line |

Three cases **warn** rather than refuse, because the rest of the document is
still exactly what it was:

- **A null where an object belongs.** The format has no pointer to a composite,
  so the null is omitted and comes back as an object of zeros.
- **A field that was only ever null**, or an array that was always empty: there
  is nothing to infer an element type from, so it is taken as a nullable string.
- **Integers above 2^63 in a field one float promoted.** The column is float64
  and those integers lose precision — which is a conflict the format cannot
  represent and the caller can.

Every failure has one shape — parse, conflict, limit, corrupt — carrying `path`,
byte `offset`, `line`, and a message naming both sides of the disagreement.
Warnings travel in the same envelope under `warnings`, and a *successful* encode
still has them to hand back.

---

## 5. Never emit what cannot be read

The one property worth paying for. Four structural rules make it hold by
construction rather than by care:

1. **Inference is a complete pass over every record before a single byte is
   emitted.** Never incremental. A schema decided from record 0 that record 500
   contradicts is exactly how an encoder emits a body its own schema does not
   describe.
2. **The body is generated from the inferred plan, never from the raw JSON
   values.** One source of truth means a value that does not fit its field
   cannot be written — it is a §4 conflict at fill time instead of a byte that
   decodes as something else.
3. **Field ids are positions**, so two fields cannot collide by construction.
   The hash-and-probe this replaced needed an assertion instead.
4. **The encoder's depth and size bounds are at or below the decoder's**, so it
   can always read back what it just wrote.

**The self-check.** Structural rules are an argument; this is a proof. Under
`VERIFY` — on in every caller that has a choice — `encode` decodes its own output
through the same walk any other reader would use and compares it to the parsed
input before returning.

It is on by default because a well-formed-but-wrong message is the failure mode
that does not announce itself. The corruption sweep in `js/tests/fuzz.test.mjs`
puts a number on it: of every single-byte corruption of every message in the
corpus, about five sixths decode to well-formed, *wrong* JSON. A decoder cannot
tell. An encoder can, because it still has the input.

It costs **about 65%** on top of an encode of a thousand records (`bun run bench` in
`js/`) — a decode, a parse of the text that came out, and a walk of the two
trees against each other. An earlier estimate of "roughly one decode" counted
only the first of the three.

It compares **values, not bytes**. "It decoded without error" is far too weak,
and comparing the two texts would be too strong — the decoder writes every field
of the schema, in wire order, where the input wrote only what it had in the order
the author typed it. Three differences are expected rather than failures, and
each is §6.

Three hazards are worth naming because each was found rather than foreseen:

| hazard | what goes wrong |
|---|---|
| **`1e400`** | syntactically valid JSON. A naive `strtod` yields `+Inf`, and JSON has no spelling for one. Refused, which is what `encoding/json` does |
| **lone surrogates** | `"\uD800"` unpaired is accepted by many JSON parsers. Defined here as `encoding/json` defines it: replaced with U+FFFD, so what reaches packed5 is always valid UTF-8 |
| **duplicate keys** | the last occurrence wins, as `JSON.parse` does. Taking the first was a real bug, and the self-check caught it the first time it ran |

And one reason the scanner exists at all rather than a host's `JSON.parse`:
`JSON.parse` turns 7295013456321098765 into 7295013456321098800. colbin has an
exact int64 column, so the parse has to be exact too, and that means reading the
digits here. The float half needs no arbitrary-precision arithmetic in Rust the
way it did in AssemblyScript — `str::parse::<f64>` is correctly rounded, and the
513 float literals in `rust/vectors/vectors.json`, every expected bit pattern
from Go's `strconv.ParseFloat`, are what say so.

---

## 6. What a round trip does not restore

Inherent to a dense columnar layout, stated here because each will look like a
bug to anyone who has not read the README:

- **A missing key and an explicit `null` decode identically.** A field holding
  its zero is not written at all, so
  `[{"id":1,"note":"hi"},{"id":2,"note":null},{"id":3}]` comes back with a third
  record of `{"id":3,"note":null}`.
- **An empty array and `null` decode identically.**
- **An integer in a field one float promoted comes back as that float.**

None is fixable without a wire change, and none is worth one. The self-check
treats exactly these three as expected; anything else is a bug in the encoder.

---

## 7. Coverage

| colbin field | encoder emits | notes |
|---|---|---|
| integers, bool | yes | int64 / uint64 / bool |
| floats | yes | float64 |
| strings | yes | raw, or packed behind `PACK_STRINGS` |
| nested structs | yes | |
| slices of structs | yes | list or table, decided per field on the row count |
| integer and string arrays | yes | |
| pointers | yes | a null or an absent key |
| `[]byte` | **no** | JSON has no bytes type; a `"base64:"` convention would be inventing format semantics the library does not have |
| maps | **no** | a JSON object is a struct here; a map needs a declared key type |
| float / bool / nested arrays | **no** | §4 — the format has no op |

The **decoder** handles all of them regardless, asymmetrically and on purpose: a
Go service that had a real struct will send `[]byte`, maps and eight-bit-keyed
fields, and refusing to read them would make the decoder useless for the case it
exists for.

`PACK_STRINGS` offers every string to the packed5 encoding and keeps it where it
is smaller. It is off by default, as Go's `SetPacked5` is, and for the same
reason: a raw blob is a sub-slice of the message on the way out and a `memcpy` on
the way in, where a packed one is a pass over every character on both sides. It
is never a correctness question — the encoding is recorded in each string's own
descriptor, so a decoder reads either form without being told, and the raw form
wins ties, so turning it on cannot make a message larger.

It reaches a scalar string field and not a string *column*: a table's strings are
written as a string array rather than one blob per row, and neither Go's
`StringColumn` nor this packs that. Whether it should is a format question rather
than the encoder's.

---

## 8. Where this is checked

| | |
|---|---|
| `rust/tests/parse.rs` | the scanner: exact integers, the float corpus, the escape rules, every limit |
| `rust/tests/infer.rs` | §1–§4 against documents chosen for one rule each |
| `rust/tests/build.rs` | the bytes, against messages committed in `js/vectors/web_encoded.json` |
| `rust/tests/verify.rs` | §5: that the self-check refuses what it should |
| `go test ./js/vectors` | the other direction, which no byte-for-byte corpus can test — the module encodes, the **Go** decoder reads it back |

The last one is the contract a caller actually has: a browser client encodes, a
Go service decodes. What is *not* checked is that Go and this pick the same
**layout** for the same data — byte parity would need §1–§3 implemented a second
time in Go forever, and since a message ships its section, Go does not need to
agree about ids or layout to read one. The table threshold and the key-width rule
are held by the comments that cite Go's constants and by nothing else.
