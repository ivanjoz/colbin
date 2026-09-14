//! The schema section: a type, in bytes, for a reader that has not got it.
//!
//! Mirrors `codec/schema_plan.go`.
//!
//! ```text
//! section   := [byteLength] [structCount] structDef{structCount}
//! structDef := [flags:1] [fieldCount] field{fieldCount}
//! field     := [key:1] [nameLen] name [desc]
//! desc      := [op:1] extra
//! ```
//!
//! `extra` by op:
//!
//! ```text
//! scalars, string, bytes, arrays   —   (the op names the element type)
//! opStruct, opStructs              [structIndex]
//! opMap                            [keyKind:1] [valueKind:1]
//! opPointer                        [elemOp:1]
//! ```
//!
//! Every length is the format's own — one byte escaping to four behind `0xFF` —
//! rather than a varint, for the reason nothing else here is a varint.
//! `byteLength` covers everything after itself, so a reader that wants the body
//! and not the schema adds it to the cursor and is done.
//!
//! # Struct hoisting
//!
//! Structs are an indexed table rather than inlined, so a self-referential type
//! (`type Node struct{ Kids []Node }`) describes itself in finite space. Every
//! plan exists before any is filled, so a field may point forward as well as
//! back — which is exactly where a crafted section could otherwise recurse
//! forever.
//!
//! # Everything here is untrusted
//!
//! A section arrives from a peer, so every length is checked against what is
//! actually there and every code against what this version assigns. Nothing may
//! allocate on a number the section merely claims: a struct table is bounded by
//! the bytes left to hold it, which is the cheapest honest bound there is.

use alloc::string::String;
use alloc::vec::Vec;

use crate::Error;
use crate::plan::{
    MAP_KIND_COUNT, OP_COUNT, OP_MAP, OP_POINTER, OP_STRUCT, OP_STRUCTS, Plan, PlanField,
};

/// The smallest a definition can be, which is what bounds a declared count
/// against the bytes actually left.
const MIN_STRUCT_DEF_BYTES: usize = 2;
const MIN_FIELD_BYTES: usize = 3;

/// The structDef flag saying the run it describes uses eight-bit keys.
const SCHEMA_WIDE_KEYS: u8 = 0x01;

/// The structDef flag saying this struct is a one-field envelope round a
/// document whose top level was not an object.
///
/// It lives here rather than in the root byte because the root's unallocated
/// detail bits are reserved by the *format* and every reader refuses the ones it
/// does not assign — so a message marked there would be refused outright. A
/// structDef's flags byte has seven spare bits and a parser reads only bit 0,
/// ignoring the rest, which is exactly the room the section was designed to have.
///
/// Go writes it too, for the slice or map it carries at a root in an envelope
/// (`codec/schema.go`, `schemaEnvelope`), and unwraps it on the way back — so
/// the three ports render the same document for the same bytes.
const SCHEMA_ENVELOPE: u8 = 0x02;

/// What eight key bits buy, and therefore the most fields a section may name.
const MAX_WIDE_FIELDS: usize = 256;

const LENGTH_ESCAPE: u8 = 0xff;

/// A parsed section: the struct table, with the root at index 0.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Schema {
    /// Every struct the section named. Index 0 is the root.
    pub plans: Vec<Plan>,
    /// The section's whole length, so a caller can step over it to the body.
    pub size: usize,
}

impl Schema {
    /// The root struct.
    #[must_use]
    #[inline]
    pub fn root(&self) -> &Plan {
        &self.plans[0]
    }

    /// A child plan by the index a [`PlanField::sub`] holds.
    #[must_use]
    #[inline]
    pub fn plan(&self, at: u32) -> Option<&Plan> {
        self.plans.get(at as usize)
    }
}

/// A length and the bytes it took, the way this format writes every count: one
/// byte, escaping to four behind `0xFF`. Reading one back is a compare and a
/// load rather than a loop whose trip count is data.
fn read_length(buf: &[u8], at: usize) -> Option<(usize, usize)> {
    let first = *buf.get(at)?;
    if first != LENGTH_ESCAPE {
        return Some((first as usize, 1));
    }
    let wide = buf.get(at + 1..at + 5)?;
    let value = u32::from_le_bytes([wide[0], wide[1], wide[2], wide[3]]);
    if value > 0x7fff_ffff {
        return None;
    }
    Some((value as usize, 5))
}

/// The cursor a parse carries, so the failure has one place to live.
struct Cursor<'a> {
    buf: &'a [u8],
    at: usize,
}

impl<'a> Cursor<'a> {
    /// The one error every length check ends at. A section that ends early is
    /// corrupt or truncated, and which byte ran out first says nothing a caller
    /// can act on.
    const fn short<T>() -> Result<T, Error> {
        Err(Error::BadSection)
    }

    fn length(&mut self) -> Result<usize, Error> {
        let (value, width) = read_length(self.buf, self.at).ok_or(Error::BadSection)?;
        self.at += width;
        Ok(value)
    }

    fn byte(&mut self) -> Result<u8, Error> {
        let b = *self.buf.get(self.at).ok_or(Error::BadSection)?;
        self.at += 1;
        Ok(b)
    }

    const fn left(&self) -> usize {
        self.buf.len() - self.at
    }
}

/// Reads a section written by `Schema.Bytes`, or the one carried in front of a
/// self-describing message.
///
/// # Errors
///
/// [`Error::BadSection`] for anything malformed: a declared length the bytes
/// cannot hold, a type code this version does not assign, or a struct index
/// outside the table.
pub fn parse(data: &[u8]) -> Result<Schema, Error> {
    let (outer, width) = read_length(data, 0).ok_or(Error::BadSection)?;
    if outer > data.len() - width {
        return Err(Error::BadSection);
    }
    let size = width + outer;
    let body = &data[width..size];

    let mut cursor = Cursor { buf: body, at: 0 };
    let count = cursor.length()?;
    if count == 0 {
        return Err(Error::BadSection);
    }
    // Bounded by the bytes actually left rather than by the number claimed, so a
    // section declaring four billion structs allocates nothing.
    if count > cursor.left() / MIN_STRUCT_DEF_BYTES {
        return Err(Error::BadSection);
    }

    // Every plan exists before any is filled, so a field may point forward as
    // well as back and a self-referential type resolves rather than recursing.
    let mut plans = alloc::vec![Plan::default(); count];
    for index in 0..count {
        parse_struct_def(&mut cursor, &mut plans, index, count)?;
    }
    for plan in &mut plans {
        plan.finish();
    }

    // Bytes past the last definition are left alone rather than refused: the
    // byteLength is what a reader steps over, and room behind the definitions is
    // where a later version would put something this one does not know about.
    Ok(Schema { plans, size })
}

fn parse_struct_def(
    cursor: &mut Cursor<'_>,
    plans: &mut [Plan],
    index: usize,
    table_len: usize,
) -> Result<(), Error> {
    let flags = cursor.byte()?;
    let is_wide = flags & SCHEMA_WIDE_KEYS != 0;
    let is_envelope = flags & SCHEMA_ENVELOPE != 0;

    let count = cursor.length()?;
    if count > cursor.left() / MIN_FIELD_BYTES || count > MAX_WIDE_FIELDS {
        return Cursor::short();
    }

    let mut fields = Vec::with_capacity(count);
    let mut names = Vec::with_capacity(count);
    for _ in 0..count {
        let key = cursor.byte()?;
        let name_len = cursor.length()?;
        let bytes = cursor
            .buf
            .get(cursor.at..cursor.at + name_len)
            .ok_or(Error::BadSection)?;
        // A field name that is not UTF-8 is a corrupt section rather than a
        // name to render as replacement characters: the walk puts it straight
        // into JSON output, where mojibake would be indistinguishable from a
        // field that was really called that.
        let name = core::str::from_utf8(bytes).map_err(|_| Error::NotUtf8)?;
        names.push(String::from(name));
        cursor.at += name_len;

        fields.push(parse_desc(cursor, key, table_len)?);
    }

    let plan = &mut plans[index];
    plan.fields = fields;
    plan.names = names;
    plan.is_wide = is_wide;
    plan.is_envelope = is_envelope;
    Ok(())
}

fn parse_desc(cursor: &mut Cursor<'_>, key: u8, table_len: usize) -> Result<PlanField, Error> {
    let op = cursor.byte()?;
    if op >= OP_COUNT {
        return Err(Error::BadSection);
    }
    let mut field = PlanField {
        key,
        op,
        ..PlanField::default()
    };

    match op {
        OP_STRUCT | OP_STRUCTS => {
            let at = cursor.length()?;
            if at >= table_len {
                return Err(Error::BadSection);
            }
            field.sub = Some(at as u32);
        }
        OP_MAP => {
            let key_kind = cursor.byte()?;
            let value_kind = cursor.byte()?;
            if key_kind >= MAP_KIND_COUNT || value_kind >= MAP_KIND_COUNT {
                return Err(Error::BadSection);
            }
            field.key_kind = key_kind;
            field.value_kind = value_kind;
        }
        OP_POINTER => {
            let elem_op = cursor.byte()?;
            if elem_op >= OP_COUNT {
                return Err(Error::BadSection);
            }
            field.elem_op = elem_op;
        }
        // Everything else is named by its op alone: an array's element type is
        // in the op, and a string and a blob are different ops.
        _ => {}
    }
    Ok(field)
}

// ---- the write half ---------------------------------------------------------

/// Serialises a struct table as a section.
///
/// The mirror of [`parse`], and the only thing between an inferred schema and a
/// reader that has not got the type. The table is written in the order it is
/// held, with the root at index 0 — [`crate::infer`] builds it in the pre-order
/// a reader's own hoisting would produce, so a section written here and one
/// written by Go for the same shape are the same bytes.
///
/// Every length goes out the way the format writes every count: one byte,
/// escaping to four behind `0xFF`.
#[cfg(feature = "encode")]
#[must_use]
pub fn build(schema: &Schema) -> Vec<u8> {
    let mut body = Vec::with_capacity(64 * schema.plans.len());
    append_length(&mut body, schema.plans.len());
    for plan in &schema.plans {
        let mut flags = 0u8;
        if plan.is_wide {
            flags |= SCHEMA_WIDE_KEYS;
        }
        if plan.is_envelope {
            flags |= SCHEMA_ENVELOPE;
        }
        body.push(flags);
        append_length(&mut body, plan.fields.len());
        for (at, field) in plan.fields.iter().enumerate() {
            body.push(field.key);
            // A plan whose names ran short would write a field a reader cannot
            // name. It cannot happen — `parse` pushes the two together and
            // `infer` does too — and an empty name is a better answer than a
            // panic if it ever did.
            let name = plan.names.get(at).map_or("", |name| name.as_str());
            append_length(&mut body, name.len());
            body.extend_from_slice(name.as_bytes());
            append_desc(&mut body, field);
        }
    }

    let mut out = Vec::with_capacity(body.len() + 5);
    append_length(&mut out, body.len());
    out.extend_from_slice(&body);
    out
}

/// One field's type: the op, and whatever the op does not say by itself.
#[cfg(feature = "encode")]
fn append_desc(out: &mut Vec<u8>, field: &PlanField) {
    out.push(field.op);
    match field.op {
        OP_STRUCT | OP_STRUCTS => append_length(out, field.sub.unwrap_or(0) as usize),
        OP_MAP => {
            out.push(field.key_kind);
            out.push(field.value_kind);
        }
        OP_POINTER => out.push(field.elem_op),
        // Everything else is named by its op alone.
        _ => {}
    }
}

/// A length, the way this format writes every count: one byte, escaping to four
/// behind `0xFF`. Not a varint, for the reason nothing here is a varint.
#[cfg(feature = "encode")]
fn append_length(out: &mut Vec<u8>, value: usize) {
    if value < LENGTH_ESCAPE as usize {
        out.push(value as u8);
        return;
    }
    out.push(LENGTH_ESCAPE);
    out.extend_from_slice(&(value as u32).to_le_bytes());
}
