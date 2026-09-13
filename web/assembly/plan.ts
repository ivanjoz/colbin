// The plan: a key, a name and an op per field, plus a child plan for the
// composites.
//
// This is the hinge of the module. Go's insight in JSON_MODE_PLAN.md §2 is that
// `codec.typePlan` already *is* the schema — strip `offset`, `sliceType` and
// `stride`, the three things that exist only to write into a Go struct, and what
// is left is exactly what a decoder without the type needs. That insight is
// worth more here than there, because AssemblyScript has no reflection and never
// had those three fields to strip.
//
// So one type sits in the middle of the module and everything points at it: the
// inferrer builds a plan, section.ts serialises and parses one, and both walks
// consume one.
//
// # These numbers are format
//
// `fieldOp` and `mapKind` go on the wire in a schema section. Reordering either
// block retypes every field of every section already written — an op is a byte,
// every byte is a plausible op, and a reader would decode a string as an integer
// without anything looking wrong. New ops go on the end and none of these ever
// moves. The generator emits the Go values and a test pins them against these.

export const OP_BOOL: u8 = 0
export const OP_INT8: u8 = 1
export const OP_INT16: u8 = 2
export const OP_INT32: u8 = 3
export const OP_INT64: u8 = 4
export const OP_UINT8: u8 = 5
export const OP_UINT16: u8 = 6
export const OP_UINT32: u8 = 7
export const OP_UINT64: u8 = 8
export const OP_FLOAT32: u8 = 9
export const OP_FLOAT64: u8 = 10
export const OP_STRING: u8 = 11
export const OP_BYTES: u8 = 12
export const OP_INT8S: u8 = 13
export const OP_INT16S: u8 = 14
export const OP_INT32S: u8 = 15
export const OP_INT64S: u8 = 16
export const OP_UINT16S: u8 = 17
export const OP_UINT32S: u8 = 18
export const OP_UINT64S: u8 = 19
export const OP_STRINGS: u8 = 20
export const OP_STRUCT: u8 = 21
export const OP_STRUCTS: u8 = 22
export const OP_MAP: u8 = 23
export const OP_POINTER: u8 = 24

/** Bounds the block, so a section naming an op this version does not assign is
 * refused rather than indexed on. */
export const OP_COUNT: u8 = 25

export const MAP_STRING: u8 = 0
export const MAP_INT: u8 = 1
export const MAP_UINT: u8 = 2
export const MAP_FLOAT64: u8 = 3
export const MAP_BOOL: u8 = 4
export const MAP_FLOAT32: u8 = 5
export const MAP_KIND_COUNT: u8 = 6

/**
 * Whether a field of this op can be a *column*, which is what lets a slice of
 * the struct holding it be transposed into a table.
 *
 * Scalars and strings, and nothing else: a nested struct, an array, a map or a
 * pointer has no column form, and one such field disqualifies the whole table.
 */
export function columnableOp(op: u8): bool {
  return (op >= OP_BOOL && op <= OP_FLOAT64) || op == OP_STRING
}

/** Whether an op is an integer scalar the wire writes through the integer shape. */
export function isIntegerOp(op: u8): bool {
  return op >= OP_BOOL && op <= OP_UINT64
}

/** Whether an integer op is signed, which is what decides how it renders. */
export function isSignedOp(op: u8): bool {
  return op >= OP_INT8 && op <= OP_INT64
}

/** The element width in bytes of an integer op, which a table column needs. */
export function widthOfOp(op: u8): i32 {
  if (op == OP_BOOL || op == OP_INT8 || op == OP_UINT8) return 1
  if (op == OP_INT16 || op == OP_UINT16) return 2
  if (op == OP_INT32 || op == OP_UINT32 || op == OP_FLOAT32) return 4
  return 8
}

/** Whether an op is an array of integers, and of what element op. */
export function arrayElementOp(op: u8): u8 {
  if (op == OP_INT8S) return OP_INT8
  if (op == OP_INT16S) return OP_INT16
  if (op == OP_INT32S) return OP_INT32
  if (op == OP_INT64S) return OP_INT64
  if (op == OP_UINT16S) return OP_UINT16
  if (op == OP_UINT32S) return OP_UINT32
  if (op == OP_UINT64S) return OP_UINT64
  return OP_COUNT
}

export class PlanField {
  key: u8 = 0
  op: u8 = OP_BOOL
  /** The child plan, for opStruct and opStructs. */
  sub: Plan | null = null
  /** What a pointer points at. */
  elemOp: u8 = OP_BOOL
  keyKind: u8 = MAP_STRING
  valueKind: u8 = MAP_STRING
}

export class Plan {
  fields: Array<PlanField> = new Array<PlanField>()
  names: Array<string> = new Array<string>()
  /** Whether the run this plan describes uses eight-bit keys.
   *
   * The schema says it for every struct rather than only where the wire cannot.
   * A *narrow* list's element is a length and a body with no descriptor between
   * them — deliberately, because that byte per element is what makes a narrow
   * list of small structs smaller than a wide one — so its key width is the
   * schema's to know, and one rule is cheaper to hold than an exception. */
  isWide: bool = false
  /**
   * Whether a slice of this struct may be transposed into a table: every field
   * of it is a column the column codec carries. Read at encode time only — a
   * reader dispatches on what it finds on the wire, not on this.
   */
  canTable: bool = false
  /**
   * Whether this plan is the one-field wrapper an encoder puts round a document
   * whose top level is not an object (REFACTOR_PLAN.md §5.1). It is recorded in
   * the section, so the way back is a fact rather than a guess.
   */
  isEnvelope: bool = false

  /**
   * The field names as UTF-8, which is how the encoder has to compare them.
   *
   * Held rather than encoded per comparison, and that is not a micro-optimism:
   * the encoder looks a name up once per field *per record*, so encoding it
   * there put `String.UTF8.encode` — an allocation — inside the innermost loop
   * of the whole module. On a thousand seven-field records it ran forty-nine
   * thousand times for seven distinct strings, and it cost 40% of the encode.
   */
  nameBytes: Array<Uint8Array> = new Array<Uint8Array>()

  /**
   * Field positions by key, so a walk resolves a key in one load.
   *
   * -1 means the message holds a key this plan does not list, which under eight
   * bits is a field to step over and under four is the end of the decode.
   */
  byKey: Int16Array = new Int16Array(0)

  /**
   * Resolves everything that follows from the fields: the key width, whether a
   * slice of this struct is transposable, and the key index.
   *
   * The width is now exactly "more than sixteen fields". packed5 used to be the
   * other thing that forced eight-bit keys and it is out of this module
   * (REFACTOR_PLAN.md §4.4), so an object of sixteen keys or fewer always takes
   * the four-bit fast path.
   */
  finish(): void {
    this.isWide = this.fields.length > 16
    this.canTable = this.fields.length > 0
    for (let index = 0; index < this.fields.length; index++) {
      if (!columnableOp(unchecked(this.fields[index]).op)) {
        this.canTable = false
        break
      }
    }
    this.encodeNames()
    this.indexKeys()
  }

  /**
   * finish for a plan that came off the wire: the section already stated the key
   * width, so only what it does not carry is derived.
   *
   * `canTable` is not on the wire and does not need to be — a reader dispatches
   * on the descriptor it finds, not on what the writer would have chosen — but
   * deriving it keeps a parsed plan and an inferred one the same shape.
   */
  finishParsed(): void {
    this.canTable = this.fields.length > 0
    for (let index = 0; index < this.fields.length; index++) {
      if (!columnableOp(unchecked(this.fields[index]).op)) {
        this.canTable = false
        break
      }
    }
    this.encodeNames()
    this.indexKeys()
  }

  private encodeNames(): void {
    const out = new Array<Uint8Array>(this.names.length)
    for (let index = 0; index < this.names.length; index++) {
      out[index] = Uint8Array.wrap(String.UTF8.encode(unchecked(this.names[index]), false))
    }
    this.nameBytes = out
  }

  indexKeys(): void {
    const index = new Int16Array(256)
    for (let at = 0; at < 256; at++) unchecked((index[at] = -1))
    for (let at = 0; at < this.fields.length; at++) {
      unchecked((index[unchecked(this.fields[at]).key] = <i16>at))
    }
    this.byKey = index
  }

  @inline positionOf(key: u8): i32 {
    return <i32>unchecked(this.byKey[key])
  }
}
