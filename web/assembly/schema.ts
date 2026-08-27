// The inferred schema: colbin's own type classes, plus the field ids the wire
// carries.
//
// These constants are colbin's, not this module's — codec/format.go is the
// source. A value here that drifts from there produces a message that decodes
// as something else, which is the failure PLAN.md §4.5 exists to prevent.

export const FT_INT: u8 = 0
export const FT_FLOAT: u8 = 1
export const FT_STRING: u8 = 2
export const FT_BYTES: u8 = 3
export const FT_ARRAY: u8 = 4
export const FT_STRUCT: u8 = 5
export const FT_MAP: u8 = 6
export const FT_ANY: u8 = 7

// Scalar kinds. Only the four the inferrer can produce are named; the rest exist
// in the format and are only ever read, never written by this encoder.
export const SK_BOOL: u8 = 0
export const SK_INT64: u8 = 4
export const SK_UINT64: u8 = 9
export const SK_FLOAT64: u8 = 12

/** 255 is the terminator, so a struct may hold at most 254 fields. */
export const RESERVED_FIELD_ID: u8 = 255
export const MAX_FIELDS: i32 = 254

export class Field {
  name: Uint8Array
  id: u8 = 0
  type: Type
  /** Records in which the key was present; short of the total means nullable. */
  present: i32 = 0

  constructor(name: Uint8Array, type: Type) {
    this.name = name
    this.type = type
  }
}

export class Type {
  ft: u8 = FT_INT
  kind: u8 = SK_INT64
  nullable: bool = false
  elem: Type | null = null
  fields: Array<Field> = []

  constructor(ft: u8, kind: u8) {
    this.ft = ft
    this.kind = kind
  }
}

/** FNV-1a 32-bit over the name, xor-folded down to 8 bits. */
export function fnv8(name: Uint8Array): u8 {
  let h: u32 = 2166136261
  for (let i = 0; i < name.length; i++) {
    h ^= <u32>unchecked(name[i])
    h *= 16777619
  }
  return <u8>(h ^ (h >> 8) ^ (h >> 16) ^ (h >> 24))
}

/**
 * Assigns wire ids to a struct's fields, in field order.
 *
 * The hash is probed forward past ids already taken, wrapping at 256, with 255
 * pre-marked so it is never assigned. Order matters: the same field names in a
 * different order can land on different ids, which is why the walk fixes
 * first-seen order (PLAN.md §3.3).
 *
 * Returns false if two fields end up sharing an id. That cannot happen —
 * probing only picks free slots — and is checked anyway, because a duplicate id
 * makes the decoder read one column into the wrong field and §4.5 says the
 * probe is checked rather than trusted.
 */
export function assignFieldIDs(fields: Array<Field>): bool {
  const used = new StaticArray<bool>(256)
  unchecked((used[RESERVED_FIELD_ID] = true))

  for (let i = 0; i < fields.length; i++) {
    const f = unchecked(fields[i])
    let id = fnv8(f.name)
    while (unchecked(used[id])) id++ // wraps at 256; 255 is pre-marked
    unchecked((used[id] = true))
    f.id = id
  }

  const seen = new StaticArray<bool>(256)
  for (let i = 0; i < fields.length; i++) {
    const id = unchecked(fields[i]).id
    if (unchecked(seen[id])) return false
    unchecked((seen[id] = true))
  }
  return true
}
