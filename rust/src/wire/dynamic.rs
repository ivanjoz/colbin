//! Values that say what they are.
//!
//! Mirrors `wire/dynamic.go`, and exists for the same reason. Everything else in
//! this module is schema-driven: a key names a field and the section says the
//! payload is a float, an unsigned integer, a string. A `map[string]any` has no
//! such declaration to lean on — its values have only the type each of them
//! happens to have — so those values carry it.
//!
//! Most of the work was already done. A key-less element under eight-bit keys is
//! `[descriptor][payload]`, and a descriptor names its class, so an integer, a
//! blob, a list, a map and a struct are self-describing today. Four SPECIAL
//! details fill the gaps:
//!
//! ```text
//! 3  FLOAT64   the bits, byte-reversed, as an ordinary integer value
//! 4  FLOAT32   the same at the narrower width
//! 5  BYTES     an ordinary blob, but opaque bytes rather than text
//! 7  TYPED     a struct-table index, then an ordinary value
//! ```
//!
//! `null`, `true` and `false` were declared from the start and are now written.
//!
//! TYPED is what keeps an array of records cheap. The expensive way to put a
//! `[]Sale` inside an `any` is a list of maps, which writes every field name on
//! every row; TYPED says "what follows is struct number three, which the section
//! describes", and the rows behind it are an ordinary LIST or TABLE.
//!
//! This is a reader. The crate encodes through `#[derive(Colbin)]`, which knows
//! its own fields and has no dynamic value to write.

use super::wide::{
    CLASS_BLOB, CLASS_COL, CLASS_INT, CLASS_LIST, CLASS_MAP, CLASS_SPECIAL, CLASS_STRUCT,
    CLASS_VEC, DESC_EXPLICIT, SPECIAL_VARINT, TABLE_FLAG, read_varint_at,
};
use super::{INT_POSITIVE_FLAG, LENGTH_WIDTH, MAGNITUDE_WIDTH, Reader8, le_uint, read_count};
use crate::Error;

/// The SPECIAL details this file assigns. 0..2 are null, true and false; 6 is
/// unspent; 8..15 are the varint integer.
///
/// # These numbers are format
///
/// A detail is four bits and every combination is a valid descriptor, so moving
/// one of these retypes every dynamic value already written — a float read as a
/// blob, with nothing looking wrong until the bytes come out.
pub(crate) const SPECIAL_NULL: u8 = 0;
pub(crate) const SPECIAL_TRUE: u8 = 1;
pub(crate) const SPECIAL_FALSE: u8 = 2;
pub(crate) const SPECIAL_FLOAT64: u8 = 3;
pub(crate) const SPECIAL_FLOAT32: u8 = 4;
pub(crate) const SPECIAL_BYTES: u8 = 5;
pub(crate) const SPECIAL_TYPED: u8 = 7;

/// What a value's descriptor says it is, for a reader with no schema to ask.
///
/// Coarser than the class table on purpose: `Int` covers the inline form, both
/// signs and the varint, because a reader building a document wants "an integer"
/// and not "an integer in one of four framings". The ones that are genuinely
/// different documents — a string against a blob, a list against a map — stay
/// apart.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Kind {
    /// A descriptor this version does not assign.
    Invalid,
    Null,
    Bool,
    Int,
    Float64,
    Float32,
    String,
    Bytes,
    List,
    Struct,
    Map,
    Table,
    /// A value whose type the section names: a struct index, and then an
    /// ordinary struct, list or table of it.
    Typed,
}

/// Classifies one descriptor byte.
#[must_use]
pub const fn kind_of(desc: u8) -> Kind {
    if desc < DESC_EXPLICIT {
        return Kind::Int; // the descriptor is a small positive value
    }
    match (desc >> 4) & 0b111 {
        CLASS_INT => Kind::Int,
        CLASS_BLOB => Kind::String,
        CLASS_LIST => Kind::List,
        CLASS_STRUCT => Kind::Struct,
        CLASS_MAP => {
            if desc & TABLE_FLAG != 0 {
                Kind::Table
            } else {
                Kind::Map
            }
        }
        CLASS_SPECIAL => {
            if desc & SPECIAL_VARINT != 0 {
                return Kind::Int;
            }
            match desc & 0b1111 {
                SPECIAL_NULL => Kind::Null,
                SPECIAL_TRUE | SPECIAL_FALSE => Kind::Bool,
                SPECIAL_FLOAT64 => Kind::Float64,
                SPECIAL_FLOAT32 => Kind::Float32,
                SPECIAL_BYTES => Kind::Bytes,
                SPECIAL_TYPED => Kind::Typed,
                _ => Kind::Invalid,
            }
        }
        _ => Kind::Invalid,
    }
}

/// How many bytes the value whose descriptor sits at `at` occupies, that
/// descriptor included.
///
/// Takes the buffer rather than the reader because it is used from both
/// conventions — a keyed field, where the descriptor is one past the cursor, and
/// a key-less element, where it is the cursor — and because sizing a value must
/// not move one.
pub(super) fn value_size(buf: &[u8], at: usize) -> Result<usize, Error> {
    let Some(desc) = buf.get(at).copied() else {
        return Err(Error::Truncated);
    };
    if desc < DESC_EXPLICIT {
        return Ok(1); // the descriptor is the value
    }
    match (desc >> 4) & 0b111 {
        CLASS_INT => {
            let width = MAGNITUDE_WIDTH[usize::from(desc & 0b111)];
            if buf.len() - (at + 1) < width {
                return Err(Error::Truncated);
            }
            Ok(1 + width)
        }
        CLASS_SPECIAL => special_size(buf, at, desc),
        class @ (CLASS_BLOB | CLASS_VEC | CLASS_LIST | CLASS_STRUCT | CLASS_MAP | CLASS_COL) => {
            // Every one of these declares a byte length covering the whole of its
            // payload, which is the property that makes an unknown field
            // skippable without its sub-schema.
            let (length, start) = declared_length(buf, at, class)?;
            if length > buf.len() - start {
                return Err(Error::Truncated);
            }
            Ok(start + length - at)
        }
        _ => Err(Error::BadDescriptor),
    }
}

/// [`value_size`]'s SPECIAL arm: the details that carry nothing, the varint, and
/// the ones that are a detail byte in front of an ordinary value.
fn special_size(buf: &[u8], at: usize, desc: u8) -> Result<usize, Error> {
    if desc & SPECIAL_VARINT != 0 {
        // A varint is self-delimiting, so a reader that does not know the key can
        // still step over it: walk the continuation bits to their end.
        let Some((_, length)) = read_varint_at(buf, at, desc) else {
            return Err(Error::Truncated);
        };
        return Ok(length);
    }
    match desc & 0b1111 {
        SPECIAL_FLOAT64 | SPECIAL_FLOAT32 | SPECIAL_BYTES => {
            // A detail byte and then one ordinary value, so the size is one plus
            // that value's — which is what keeps a dynamic field skippable by a
            // reader that has never heard of these details.
            Ok(1 + value_size(buf, at + 1)?)
        }
        SPECIAL_TYPED => {
            let Some((_, width)) = read_count(&buf[at + 1..]) else {
                return Err(Error::Truncated);
            };
            Ok(1 + width + value_size(buf, at + 1 + width)?)
        }
        _ => Ok(1),
    }
}

/// The length a length-carrying class declares, and the offset just past it.
/// [`Reader8::length_of`] without a cursor.
pub(super) fn declared_length(buf: &[u8], at: usize, want: u8) -> Result<(usize, usize), Error> {
    let Some(desc) = buf.get(at).copied() else {
        return Err(Error::Truncated);
    };
    if desc < DESC_EXPLICIT || (desc >> 4) & 0b111 != want {
        return Err(Error::BadDescriptor);
    }
    let width = if want == CLASS_VEC {
        if desc & 1 == 0 { 1 } else { 4 }
    } else {
        LENGTH_WIDTH[usize::from(desc & 0b11)]
    };
    let rest = &buf[at + 1..];
    if rest.len() < width {
        return Err(Error::Truncated);
    }
    let Ok(value) = usize::try_from(le_uint(rest, width)) else {
        return Err(Error::SizeTooLarge);
    };
    Ok((value, at + 1 + width))
}

impl<'a> Reader8<'a> {
    /// Steps the cursor from a field's key onto its value, so that the element
    /// readers below can read what a keyed field holds.
    ///
    /// A dynamic field is the one place where the framing and the value are read
    /// by different code — the key is a field's and everything behind it is a
    /// key-less value's — and moving the cursor once is cheaper to hold in the
    /// head than a keyed twin of every reader here.
    pub fn payload(&mut self) -> bool {
        if self.at >= self.buf.len() {
            self.fail(Error::Truncated);
            return false;
        }
        self.at += 1;
        true
    }

    /// What the value at the cursor is, without advancing.
    ///
    /// Answers [`Kind::Invalid`] for a descriptor this version does not assign,
    /// and the caller is expected to fail on that rather than guess: every byte
    /// is a plausible descriptor, so an unassigned one is a message from
    /// something newer and not a value to step past quietly.
    #[must_use]
    pub fn element_kind(&self) -> Kind {
        if self.err.is_some() {
            return Kind::Invalid;
        }
        match self.buf.get(self.at).copied() {
            Some(desc) => kind_of(desc),
            None => Kind::Invalid,
        }
    }

    /// Whether the integer at the cursor is a negative one, without advancing.
    ///
    /// A reader with a schema never asks: the field is signed or it is not. A
    /// reader without one has to, because a magnitude past 2^63 is a value this
    /// format carries and `i64` is not where it fits.
    #[must_use]
    pub fn element_negative(&self) -> bool {
        self.buf.get(self.at).copied().is_some_and(|desc| {
            desc >= DESC_EXPLICIT
                && (desc >> 4) & 0b111 == CLASS_INT
                && desc & INT_POSITIVE_FLAG == 0
        })
    }

    /// Steps over a null.
    pub fn element_null(&mut self) {
        if self.at < self.buf.len() {
            self.at += 1;
            return;
        }
        self.fail(Error::Truncated);
    }

    /// Reads true or false.
    pub fn element_bool(&mut self) -> bool {
        let Some(desc) = self.buf.get(self.at).copied() else {
            self.fail(Error::Truncated);
            return false;
        };
        self.at += 1;
        desc & 0b1111 == SPECIAL_TRUE && (desc >> 4) & 0b111 == CLASS_SPECIAL
    }

    /// Reads a float64 written as a dynamic value.
    pub fn element_f64(&mut self) -> f64 {
        f64::from_bits(self.element_float_bits().swap_bytes())
    }

    /// Reads a float32 written as a dynamic value.
    pub fn element_f32(&mut self) -> f32 {
        f32::from_bits((self.element_float_bits() as u32).swap_bytes())
    }

    /// Steps over the detail byte and reads the integer behind it.
    fn element_float_bits(&mut self) -> u64 {
        if self.at >= self.buf.len() {
            self.fail(Error::Truncated);
            return 0;
        }
        self.at += 1;
        self.element_uint()
    }

    /// An opaque blob as a sub-slice of the message.
    pub fn element_bytes(&mut self) -> &'a [u8] {
        if self.at >= self.buf.len() {
            self.fail(Error::Truncated);
            return &[];
        }
        self.at += 1;
        self.element_blob()
    }

    /// [`Reader8::element_bytes`] for a blob with no detail byte in front of it,
    /// which is what a string element is.
    pub fn element_blob(&mut self) -> &'a [u8] {
        self.step_back();
        self.bytes()
    }

    /// Reads the tag that names a struct def and leaves the cursor on the value
    /// behind it.
    pub fn element_typed_index(&mut self) -> Option<u32> {
        if self.at >= self.buf.len() {
            self.fail(Error::Truncated);
            return None;
        }
        let Some((index, width)) = read_count(&self.buf[self.at + 1..]) else {
            self.fail(Error::Truncated);
            return None;
        };
        self.at += 1 + width;
        u32::try_from(index).ok().or_else(|| {
            self.fail(Error::BadSection);
            None
        })
    }

    /// The element count and a reader over a key-less list.
    pub fn element_list(&mut self) -> Option<(usize, Reader8<'a>)> {
        self.step_back();
        self.list()
    }

    /// The entry count and a reader over a key-less map.
    pub fn element_map(&mut self) -> Option<(usize, Reader8<'a>)> {
        self.step_back();
        self.map()
    }

    /// The row count and a reader over a key-less table's columns.
    pub fn element_table(&mut self) -> Option<(usize, Reader8<'a>)> {
        self.step_back();
        self.table()
    }

    /// Steps over one key-less value of any kind, which is what lets a reader
    /// walk past a dynamic value it has no use for.
    pub fn skip_element(&mut self) -> bool {
        match value_size(self.buf, self.at) {
            Ok(size) => {
                self.at += size;
                true
            }
            Err(err) => {
                self.fail(err);
                false
            }
        }
    }
}
