//! The plan: a key, a name and an op per field, plus a child plan for the
//! composites.
//!
//! This is what a schema section parses into, and what a schema-driven decode
//! walks against. `#[derive(Colbin)]` needs none of it — a derived type knows
//! its own fields — so everything here exists for the other case: a reader that
//! was handed bytes and a section and no type at all.
//!
//! # These numbers are format
//!
//! `Op` and `MapKind` go on the wire in a schema section. Reordering either
//! block retypes every field of every section already written: an op is a byte,
//! every byte is a plausible op, and a reader would decode a string as an
//! integer without anything looking wrong. New ops go on the end and none of
//! these ever moves.
//!
//! # Children are indices, not pointers
//!
//! A field's child struct is a `u32` into [`Schema::plans`], because that is
//! what the wire holds: the section hoists structs into an indexed table so a
//! self-referential type describes itself in finite space. Keeping the index
//! rather than resolving it to a reference is also what keeps this representable
//! without `Rc` — a cyclic type is expressible in the format, and an owning
//! pointer graph could not hold one.

use alloc::string::String;
use alloc::vec::Vec;

pub const OP_BOOL: u8 = 0;
pub const OP_INT8: u8 = 1;
pub const OP_INT16: u8 = 2;
pub const OP_INT32: u8 = 3;
pub const OP_INT64: u8 = 4;
pub const OP_UINT8: u8 = 5;
pub const OP_UINT16: u8 = 6;
pub const OP_UINT32: u8 = 7;
pub const OP_UINT64: u8 = 8;
pub const OP_FLOAT32: u8 = 9;
pub const OP_FLOAT64: u8 = 10;
pub const OP_STRING: u8 = 11;
pub const OP_BYTES: u8 = 12;
pub const OP_INT8S: u8 = 13;
pub const OP_INT16S: u8 = 14;
pub const OP_INT32S: u8 = 15;
pub const OP_INT64S: u8 = 16;
pub const OP_UINT16S: u8 = 17;
pub const OP_UINT32S: u8 = 18;
pub const OP_UINT64S: u8 = 19;
pub const OP_STRINGS: u8 = 20;
pub const OP_STRUCT: u8 = 21;
pub const OP_STRUCTS: u8 = 22;
pub const OP_MAP: u8 = 23;
pub const OP_POINTER: u8 = 24;
/// A field of type `any`, and a slice of them: a value with no declared type,
/// which says on the wire what it is. See [`crate::wire::Kind`].
pub const OP_ANY: u8 = 25;
pub const OP_ANYS: u8 = 26;

/// Bounds the block, so a section naming an op this version does not assign is
/// refused rather than indexed on.
pub const OP_COUNT: u8 = 27;

pub const MAP_STRING: u8 = 0;
pub const MAP_INT: u8 = 1;
pub const MAP_UINT: u8 = 2;
pub const MAP_FLOAT64: u8 = 3;
pub const MAP_BOOL: u8 = 4;
pub const MAP_FLOAT32: u8 = 5;
/// A map value with no declared type: every entry says what it is. It is what
/// makes `map[string]any` carriable.
pub const MAP_ANY: u8 = 6;
pub const MAP_KIND_COUNT: u8 = 7;

/// Whether a field of this op can be a *column*, which is what lets a slice of
/// the struct holding it be transposed into a table.
///
/// Scalars and strings, and nothing else: a nested struct, an array, a map or a
/// pointer has no column form, and one such field disqualifies the whole table.
#[must_use]
pub const fn columnable_op(op: u8) -> bool {
    (op <= OP_FLOAT64) || op == OP_STRING
}

/// Whether an op is an integer scalar the wire writes through the integer shape.
#[must_use]
pub const fn is_integer_op(op: u8) -> bool {
    op <= OP_UINT64
}

/// Whether an integer op is signed, which is what decides how it renders.
#[must_use]
pub const fn is_signed_op(op: u8) -> bool {
    op >= OP_INT8 && op <= OP_INT64
}

/// The element width in bytes of an integer op, which a table column needs.
#[must_use]
pub const fn width_of_op(op: u8) -> usize {
    match op {
        OP_BOOL | OP_INT8 | OP_UINT8 => 1,
        OP_INT16 | OP_UINT16 => 2,
        OP_INT32 | OP_UINT32 | OP_FLOAT32 => 4,
        _ => 8,
    }
}

/// The element op of an array op, or [`OP_COUNT`] when the op is not an array.
#[must_use]
pub const fn array_element_op(op: u8) -> u8 {
    match op {
        OP_INT8S => OP_INT8,
        OP_INT16S => OP_INT16,
        OP_INT32S => OP_INT32,
        OP_INT64S => OP_INT64,
        OP_UINT16S => OP_UINT16,
        OP_UINT32S => OP_UINT32,
        OP_UINT64S => OP_UINT64,
        _ => OP_COUNT,
    }
}

/// One field: what it is called, what number it answers to, and what it holds.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PlanField {
    pub key: u8,
    pub op: u8,
    /// The child plan's index in [`Schema::plans`], for `OP_STRUCT` and
    /// `OP_STRUCTS`.
    pub sub: Option<u32>,
    /// What a pointer points at.
    pub elem_op: u8,
    pub key_kind: u8,
    pub value_kind: u8,
}

impl Default for PlanField {
    fn default() -> Self {
        Self {
            key: 0,
            op: OP_BOOL,
            sub: None,
            elem_op: OP_BOOL,
            key_kind: MAP_STRING,
            value_kind: MAP_STRING,
        }
    }
}

/// One struct: its fields, in the order a record writes them.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Plan {
    pub fields: Vec<PlanField>,
    pub names: Vec<String>,
    /// Whether the run this plan describes uses eight-bit keys.
    ///
    /// The schema says it for every struct rather than only where the wire
    /// cannot. A *narrow* list's element is a length and a body with no
    /// descriptor between them — deliberately, because that byte per element is
    /// what makes a narrow list of small structs smaller than a wide one — so
    /// its key width is the schema's to know.
    pub is_wide: bool,
    /// Whether a slice of this struct may be transposed into a table. Read at
    /// encode time only: a reader dispatches on what it finds on the wire.
    pub can_table: bool,
    /// Whether this plan is the one-field wrapper an encoder puts round a
    /// document whose top level is not an object.
    pub is_envelope: bool,
    /// Field positions by key, so a walk resolves a key in one load. `-1` means
    /// the message holds a key this plan does not list, which under eight bits
    /// is a field to step over and under four is the end of the decode.
    by_key: Vec<i16>,
}

impl Plan {
    /// Derives what the section does not carry: the key index, and whether a
    /// slice of this struct could be transposed.
    pub fn finish(&mut self) {
        self.can_table = !self.fields.is_empty() && self.fields.iter().all(|f| columnable_op(f.op));

        let mut index = alloc::vec![-1i16; 256];
        for (at, field) in self.fields.iter().enumerate() {
            // A section may repeat a key. Last writer wins, matching the way the
            // AssemblyScript and Go readers index, so a crafted duplicate cannot
            // make the three disagree about which field it named.
            index[field.key as usize] = at as i16;
        }
        self.by_key = index;
    }

    /// The field position for a key, or `None` when this plan does not list it.
    #[must_use]
    #[inline]
    pub fn field_of(&self, key: u8) -> Option<usize> {
        match self.by_key.get(key as usize).copied().unwrap_or(-1) {
            -1 => None,
            at => Some(at as usize),
        }
    }
}
