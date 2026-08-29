//! Field ids, mirroring `codec/typeinfo.go`.
//!
//! A colbin message names fields by a one-byte id rather than by name. Explicit
//! ids from a `cb:"5"` tag are reserved first; every other field takes the
//! FNV-1a hash of its name, linear probed past the slots already taken. Id 255
//! is reserved as the compact-mode terminator and is never assigned.
//!
//! Declaration order therefore matters: it decides which field wins a hash
//! collision, so a schema built here must list its fields in the order the Go
//! struct declares them.

use crate::Error;

/// The id no field may hold: it closes a record in compact mode.
pub const RESERVED_FIELD_ID: u8 = 255;

/// The most fields a struct may encode, leaving the id space one free slot.
pub const MAX_FIELDS: usize = 254;

/// FNV-1a 32-bit over `name`, xor-folded down to 8 bits.
pub fn fnv8(name: &str) -> u8 {
    let mut hash = 2_166_136_261_u32;
    for byte in name.as_bytes() {
        hash ^= u32::from(*byte);
        hash = hash.wrapping_mul(16_777_619);
    }
    (hash ^ (hash >> 8) ^ (hash >> 16) ^ (hash >> 24)) as u8
}

/// Assigns a wire id to every field, in declaration order.
///
/// `fields` pairs each field's hash name with its explicit id, if the Go struct
/// tagged one. The two passes are the Go builder's: explicit ids are reserved
/// before any hash is probed, so adding a hashed field can never displace a
/// tagged one.
pub(crate) fn assign_ids(fields: &[(&str, Option<u8>)]) -> Result<Vec<u8>, Error> {
    if fields.len() > MAX_FIELDS {
        return Err(Error::TooManyFields(fields.len()));
    }
    let mut used = [false; 256];
    used[usize::from(RESERVED_FIELD_ID)] = true;
    let mut ids = vec![0_u8; fields.len()];

    for (index, (_, explicit)) in fields.iter().enumerate() {
        let Some(id) = *explicit else { continue };
        if id == RESERVED_FIELD_ID {
            return Err(Error::FieldIdOutOfRange(id));
        }
        if used[usize::from(id)] {
            return Err(Error::DuplicateFieldId(id));
        }
        used[usize::from(id)] = true;
        ids[index] = id;
    }
    for (index, (name, explicit)) in fields.iter().enumerate() {
        if explicit.is_some() {
            continue;
        }
        let mut candidate = fnv8(name);
        while used[usize::from(candidate)] {
            // Wraps at 256; 255 is pre-marked, so the terminator is skipped.
            candidate = candidate.wrapping_add(1);
        }
        used[usize::from(candidate)] = true;
        ids[index] = candidate;
    }
    Ok(ids)
}
