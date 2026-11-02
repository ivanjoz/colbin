//! A second sink beside [`crate::json`]: columns into a flat buffer instead of
//! decimal text, for a caller that can build its own objects from typed views.
//!
//! Mirrors `INTERNALS.md` §16.3 in the browser package this crate feeds. It
//! covers the tables at the root of a message:
//!
//! - an **envelope** (`Plan::is_envelope`, the top level was a bare array)
//!   whose one field the wire encoded as a transposed table
//!   ([`crate::wire::Reader::is_table`]) — the reader gets the array;
//! - a **root struct whose every field is a slice of structs written as a
//!   table** (`struct { Rows []Row }`, or several tables side by side) — the
//!   reader gets an object with one array per field, in the order the message
//!   wrote them, which is the order the JSON walk renders them in.
//!
//! A table's columns may be plain, nullable (a pointer field: a presence bitmap
//! rides beside the values) or dictionary strings (resolved to their strings
//! here), the row type may be paged (a page's fields are columns in line, where
//! the JSON walk writes them), and the table may be grouped. Every other shape
//! — a root field that is not a table or is absent, a list column, a dynamic
//! document —
//! returns `Ok(None)`, which tells the caller to fall back to
//! [`crate::walk::to_json`].
//!
//! # The buffer
//!
//! ```text
//! u32 version (2)
//! u32 shape        0: the root is the one table's array, 1: an object of tables
//! u32 tableCount
//! per table:
//!   u16 nameLen, name          the root field's name (empty for an envelope)
//!   pad to 8
//!   u32 rowCount, u32 fieldCount
//!   per field: u8 kind, u8 flags, u16 nameLen, name
//!   pad to 8
//!   per field, in header order, each padded to 8:
//!     [nullable only] validity bitmap, ceil(rows/8) bytes, bit set = a value; pad to 8
//!     bool    a bitmap, ceil(rows/8) bytes
//!     int     rows × i64
//!     float   rows × f64
//!     string  (rows + 1) × u32 offsets, then the bytes
//! ```
//!
//! A null row holds a zero, or an empty string, in the value run. There is no
//! offset table: every run's byte length is derivable from `rowCount` and the
//! field's own kind and flags, so the reader walks the buffer once, rather than
//! trusting a second copy of lengths that could disagree with the data.

use alloc::vec::Vec;

use crate::Error;
use crate::plan::{self, OP_POINTER, OP_STRING, OP_STRUCTS, Plan, PlanField};
use crate::section::Schema;
use crate::walk::{Gathered, MAX_ROWS};
use crate::wire::{self, Reader, Reader8, Runs};

/// The largest integer magnitude a JavaScript `Number` holds exactly. Above
/// this the reader must pay for a `BigInt`, and the flag says so per column
/// rather than the reader guessing from the value itself.
const MAX_SAFE_INT: u64 = 9_007_199_254_740_991;

const VERSION: u32 = 2;

const SHAPE_ARRAY: u32 = 0;
const SHAPE_OBJECT: u32 = 1;

const KIND_BOOL: u8 = 0;
const KIND_INT: u8 = 1;
const KIND_FLOAT: u8 = 2;
const KIND_STRING: u8 = 3;

const FLAG_SIGNED: u8 = 1 << 0;
const FLAG_EXCEEDS_SAFE_INT: u8 = 1 << 1;
const FLAG_ASCII: u8 = 1 << 2;
const FLAG_NULLABLE: u8 = 1 << 3;

/// One field's data, already in the shape its runs will take, built a group at
/// a time.
struct Column {
    /// The op of the values: the field's, or a pointer's element op.
    op: u8,
    data: Data,
    /// A nullable column's validity bitmap.
    valid: Option<Vec<u8>>,
}

enum Data {
    Bool(Vec<u8>),
    Int {
        bytes: Vec<u8>,
        signed: bool,
        exceeds: bool,
    },
    Float(Vec<u8>),
    String {
        offsets: Vec<u8>,
        blob: Vec<u8>,
        ascii: bool,
    },
}

impl Column {
    fn new(field: &PlanField, rows: usize) -> Result<Self, Error> {
        let nullable = field.op == OP_POINTER;
        let op = if nullable { field.elem_op } else { field.op };
        let data = match op {
            plan::OP_BOOL => Data::Bool(alloc::vec![0u8; rows.div_ceil(8)]),
            op if plan::is_integer_op(op) => Data::Int {
                bytes: Vec::with_capacity(rows * 8),
                signed: plan::is_signed_op(op),
                exceeds: false,
            },
            plan::OP_FLOAT32 | plan::OP_FLOAT64 => Data::Float(Vec::with_capacity(rows * 8)),
            OP_STRING => {
                let mut offsets = Vec::with_capacity((rows + 1) * 4);
                offsets.extend_from_slice(&0u32.to_le_bytes());
                Data::String {
                    offsets,
                    blob: Vec::new(),
                    ascii: true,
                }
            }
            // `Plan::finish` refuses a table whose element is not fully
            // columnable, so nothing else can reach here.
            op => return Err(Error::UnwalkableOp(op)),
        };
        Ok(Column {
            op,
            data,
            valid: nullable.then(|| alloc::vec![0u8; rows.div_ceil(8)]),
        })
    }

    fn kind_and_flags(&self) -> (u8, u8) {
        let nullable = if self.valid.is_some() {
            FLAG_NULLABLE
        } else {
            0
        };
        match &self.data {
            Data::Bool(_) => (KIND_BOOL, nullable),
            Data::Int {
                signed, exceeds, ..
            } => {
                let mut flags = nullable;
                if *signed {
                    flags |= FLAG_SIGNED;
                }
                if *exceeds {
                    flags |= FLAG_EXCEEDS_SAFE_INT;
                }
                (KIND_INT, flags)
            }
            Data::Float(_) => (KIND_FLOAT, nullable),
            Data::String { ascii, .. } => {
                (KIND_STRING, nullable | if *ascii { FLAG_ASCII } else { 0 })
            }
        }
    }

    /// Appends row `row` (of the whole table): `Some(raw)` an integer, a bool or
    /// a float's bits, `None` a null or an absent column's zero.
    fn push_raw(&mut self, row: usize, raw: Option<i64>) -> Result<(), Error> {
        if let (Some(valid), Some(_)) = (self.valid.as_mut(), raw) {
            valid[row >> 3] |= 1 << (row & 7);
        }
        let raw = raw.unwrap_or(0);
        match &mut self.data {
            Data::Bool(bitmap) => {
                if raw == 1 {
                    bitmap[row >> 3] |= 1 << (row & 7);
                }
            }
            Data::Int {
                bytes,
                signed,
                exceeds,
            } => {
                let value: i64 = if *signed {
                    if raw.unsigned_abs() > MAX_SAFE_INT {
                        *exceeds = true;
                    }
                    raw
                } else {
                    // Same truncation `column_value` applies: the column codec
                    // stores signed residuals, so a column narrower than 64 bits
                    // comes back sign-extended.
                    let width = plan::width_of_op(self.op);
                    let value = if width == 8 {
                        raw as u64
                    } else {
                        (raw as u64) & (u64::MAX >> (64 - width * 8))
                    };
                    if value > MAX_SAFE_INT {
                        *exceeds = true;
                    }
                    value as i64
                };
                bytes.extend_from_slice(&value.to_le_bytes());
            }
            Data::Float(bytes) => {
                let value = if self.op == plan::OP_FLOAT32 {
                    f64::from(f32::from_bits(raw as u32))
                } else {
                    f64::from_bits(raw as u64)
                };
                if value.is_nan() || value.is_infinite() {
                    return Err(Error::NotJson);
                }
                bytes.extend_from_slice(&value.to_le_bytes());
            }
            Data::String { .. } => return self.push_text(row, None),
        }
        Ok(())
    }

    /// Appends a string row, `None` for a null or an absent column's "".
    fn push_text(&mut self, row: usize, value: Option<&[u8]>) -> Result<(), Error> {
        if let (Some(valid), Some(_)) = (self.valid.as_mut(), value) {
            valid[row >> 3] |= 1 << (row & 7);
        }
        let Data::String {
            offsets,
            blob,
            ascii,
        } = &mut self.data
        else {
            return Err(Error::UnwalkableOp(self.op));
        };
        let value = value.unwrap_or(&[]);
        if *ascii && !value.is_ascii() {
            *ascii = false;
        }
        blob.extend_from_slice(value);
        let end = u32::try_from(blob.len()).map_err(|_| Error::SizeTooLarge)?;
        offsets.extend_from_slice(&end.to_le_bytes());
        Ok(())
    }
}

/// One root table, materialized: its name and its columns.
struct Table<'p> {
    name: &'p str,
    /// The column names, a paged row type's pages' fields in line.
    names: Vec<&'p str>,
    rows: usize,
    columns: Vec<Column>,
}

/// A message's tables, written into a flat buffer — or `None` when the root
/// is not a shape this covers, which is not an error: the caller falls back to
/// [`crate::walk::to_json`].
///
/// # Errors
///
/// Any malformed message, the same as [`crate::walk::to_json`] would refuse.
pub fn to_buffer(schema: &Schema, message: &[u8], wide: bool) -> Result<Option<Vec<u8>>, Error> {
    to_buffer_with(schema, message, wide, &[])
}

fn to_buffer_with(
    schema: &Schema,
    message: &[u8],
    wide: bool,
    dicts: &[Vec<Vec<u8>>],
) -> Result<Option<Vec<u8>>, Error> {
    let plan = schema.root();
    if plan.fields.is_empty() || plan.fields.iter().any(|field| field.op != OP_STRUCTS) {
        return Ok(None);
    }
    let envelope = plan.is_envelope;
    if envelope && plan.fields.len() != 1 {
        return Ok(None);
    }

    // The root's key run, at its width: every key must be one of the plan's
    // table fields, each written as a table. A field left out is null in the
    // JSON walk (a nil slice), which this does not represent.
    let mut found: Vec<(usize, usize, usize, &[u8])> = Vec::with_capacity(plan.fields.len());
    let mut seen = alloc::vec![false; plan.fields.len()];
    if wide {
        let mut reader = Reader8::new(message);
        while reader.more() {
            let Some(index) = plan.field_of(reader.key()) else {
                return Ok(None);
            };
            if seen[index] || !reader.is_table() {
                return Ok(None);
            }
            seen[index] = true;
            let (rows, group_rows, body) = reader.table().ok_or(Error::Truncated)?;
            reader.err()?;
            found.push((index, rows, group_rows, body));
        }
        reader.err()?;
    } else {
        let mut reader = Reader::new(message);
        while reader.more() {
            let Some(index) = plan.field_of(reader.key()) else {
                return Ok(None);
            };
            if seen[index] || !reader.is_table() {
                return Ok(None);
            }
            seen[index] = true;
            let (rows, group_rows, body) = reader.table().ok_or(Error::Truncated)?;
            reader.err()?;
            found.push((index, rows, group_rows, body));
        }
        reader.err()?;
    }
    if seen.iter().any(|seen| !seen) {
        return Ok(None);
    }

    let mut tables = Vec::with_capacity(found.len());
    for (index, rows, group_rows, body) in found {
        if rows > MAX_ROWS {
            return Err(Error::TooManyRows);
        }
        let field = &plan.fields[index];
        let at = field.sub.ok_or(Error::BadSection)?;
        let sub = schema.plan(at).ok_or(Error::BadSection)?;
        // The columns are keyed at the row type's width, which for wide rows is
        // not a narrow parent's — see `walk::Walker::narrow_table`.
        let columns_wide = wide || sub.is_wide;
        let Some((names, columns)) =
            read_table(schema, at, sub, rows, group_rows, body, columns_wide, dicts)?
        else {
            return Ok(None);
        };
        tables.push(Table {
            name: if envelope {
                ""
            } else {
                plan.names[index].as_str()
            },
            names,
            rows,
            columns,
        });
    }
    let shape = if envelope { SHAPE_ARRAY } else { SHAPE_OBJECT };
    Ok(Some(write_buffer(shape, &tables)))
}

/// One column of a table's row type, as the JSON walk writes it: a field of
/// the row type, or of one of its pages, reached through the page links at
/// `pages` (field indexes, outermost first).
struct Slot<'p> {
    pages: Vec<usize>,
    index: usize,
    field: &'p PlanField,
    name: &'p str,
}

/// A row type's columns in the order `walk::Walker::row_fields` writes them,
/// a page's fields where its link is — or `None` for a field with no flat form
/// (a list column, a nested struct).
fn slots<'p>(
    schema: &'p Schema,
    plan: &'p Plan,
    pages: &[usize],
) -> Result<Option<Vec<Slot<'p>>>, Error> {
    let flat = |op: u8| {
        op == plan::OP_BOOL
            || op == OP_STRING
            || plan::is_integer_op(op)
            || op == plan::OP_FLOAT32
            || op == plan::OP_FLOAT64
    };
    let mut out = Vec::with_capacity(plan.fields.len());
    for (index, field) in plan.fields.iter().enumerate() {
        let page = (field.op == plan::OP_STRUCT)
            .then_some(field.sub)
            .flatten()
            .and_then(|at| schema.plan(at))
            .filter(|sub| sub.is_page);
        if let Some(page) = page {
            let mut path = pages.to_vec();
            path.push(index);
            let Some(inner) = slots(schema, page, &path)? else {
                return Ok(None);
            };
            out.extend(inner);
        } else if flat(field.op) || (field.op == OP_POINTER && flat(field.elem_op)) {
            out.push(Slot {
                pages: pages.to_vec(),
                index,
                field,
                name: plan.names.get(index).ok_or(Error::BadSection)?.as_str(),
            });
        } else {
            return Ok(None);
        }
    }
    Ok(Some(out))
}

/// A table's column names and columns, in the same order.
type NamedColumns<'p> = (Vec<&'p str>, Vec<Column>);

/// A table's columns, a group at a time, each row put back the way
/// `walk::Walker::row_fields` writes it — or `None` for a list column.
#[allow(clippy::too_many_arguments)]
fn read_table<'p>(
    schema: &'p Schema,
    at: u32,
    sub: &'p Plan,
    rows: usize,
    group_rows: usize,
    body: &[u8],
    wide: bool,
    dicts: &[Vec<Vec<u8>>],
) -> Result<Option<NamedColumns<'p>>, Error> {
    let Some(slots) = slots(schema, sub, &[])? else {
        return Ok(None);
    };
    let mut columns = slots
        .iter()
        .map(|slot| Column::new(slot.field, rows))
        .collect::<Result<Vec<_>, _>>()?;
    let mut start = 0;
    for run in Runs::new(rows, group_rows, body) {
        let (size, run) = run?;
        let mut whole = Gathered::new(sub.fields.len());
        whole.read(schema, at, run, size, wide, !dicts.is_empty(), 0)?;
        if whole.has_lists() {
            return Ok(None);
        }
        for (slot, column) in slots.iter().zip(&mut columns) {
            // The page the field is on, or `None` when the message left that
            // page out — every field of it is then absent, as in the JSON walk.
            let mut gathered = Some(&whole);
            for &link in &slot.pages {
                gathered = gathered
                    .filter(|g| g.present[link])
                    .and_then(|g| g.pages[link].as_deref());
            }
            let (index, field) = (slot.index, slot.field);
            let Some(gathered) = gathered.filter(|g| g.present[index]) else {
                // A column whose every value was zero — or, for a pointer
                // column, every row nil — is not written at all.
                for row in start..start + size {
                    if column.op == OP_STRING {
                        let zero: Option<&[u8]> = (field.op != OP_POINTER).then_some(&[]);
                        column.push_text(row, zero)?;
                    } else if field.op == OP_POINTER {
                        column.push_raw(row, None)?;
                    } else {
                        column.push_raw(row, Some(0))?;
                    }
                }
                continue;
            };
            let presence = gathered.presence[index];
            let mut next = 0;
            for local in 0..size {
                let row = start + local;
                let mut at = local;
                if let Some(presence) = presence {
                    if !wire::present(presence, local) {
                        if column.op == OP_STRING {
                            column.push_text(row, None)?;
                        } else {
                            column.push_raw(row, None)?;
                        }
                        continue;
                    }
                    at = next;
                    next += 1;
                }
                if column.op == OP_STRING {
                    let value: &[u8] = if let Some(number) = gathered.dict[index] {
                        let id = gathered.ints[index].get(at).copied().unwrap_or(0);
                        usize::try_from(id)
                            .ok()
                            .and_then(|id| dicts.get(number)?.get(id))
                            .ok_or(Error::BadDictionary)?
                    } else {
                        gathered.strings[index].get(at).copied().unwrap_or(&[])
                    };
                    column.push_text(row, Some(value))?;
                } else {
                    // A column shorter than its rows is a message that disagrees
                    // with itself; a zero is what the JSON walk writes for it.
                    let raw = gathered.ints[index].get(at).copied().unwrap_or(0);
                    column.push_raw(row, Some(raw))?;
                }
            }
        }
        start += size;
    }
    let names = slots.iter().map(|slot| slot.name).collect();
    Ok(Some((names, columns)))
}

/// [`to_buffer`] for a whole message, root byte and section included: the
/// message's own section takes precedence over `held`.
///
/// # Errors
///
/// Any malformed message, a root this version does not read, and
/// [`Error::NoSchema`] for a typed message with no section anywhere.
pub fn message_to_buffer(held: Option<&Schema>, message: &[u8]) -> Result<Option<Vec<u8>>, Error> {
    let split = crate::walk::split_root(message)?;
    let schema = split.schema.as_ref().or(held);
    match split.root {
        0xD0 | 0xD8 => to_buffer(
            schema.ok_or(Error::NoSchema)?,
            split.body,
            split.root == 0xD8,
        ),
        // A message with dictionaries: its string columns are ids into them,
        // resolved here as the JSON walk resolves them.
        0xD2 | 0xDA => {
            let schema = schema.ok_or(Error::NoSchema)?;
            let (dicts, body) = crate::walk::parse_dictionaries(split.body)?;
            to_buffer_with(schema, body, split.root == 0xDA, &dicts)
        }
        // A dynamic document is not a table.
        0xD1 | 0xD3 => Ok(None),
        root => Err(Error::BadRoot(root)),
    }
}

fn pad_to_8(out: &mut Vec<u8>) {
    let pad = out.len().next_multiple_of(8) - out.len();
    out.resize(out.len() + pad, 0);
}

fn write_buffer(shape: u32, tables: &[Table<'_>]) -> Vec<u8> {
    let mut out = Vec::new();
    out.extend_from_slice(&VERSION.to_le_bytes());
    out.extend_from_slice(&shape.to_le_bytes());
    out.extend_from_slice(&(tables.len() as u32).to_le_bytes());
    for table in tables {
        out.extend_from_slice(&(table.name.len() as u16).to_le_bytes());
        out.extend_from_slice(table.name.as_bytes());
        pad_to_8(&mut out);

        out.extend_from_slice(&(table.rows as u32).to_le_bytes());
        out.extend_from_slice(&(table.names.len() as u32).to_le_bytes());
        for (name, column) in table.names.iter().zip(&table.columns) {
            let (kind, flags) = column.kind_and_flags();
            out.push(kind);
            out.push(flags);
            let name_bytes = name.as_bytes();
            out.extend_from_slice(&(name_bytes.len() as u16).to_le_bytes());
            out.extend_from_slice(name_bytes);
        }
        pad_to_8(&mut out);

        for column in &table.columns {
            if let Some(valid) = &column.valid {
                out.extend_from_slice(valid);
                pad_to_8(&mut out);
            }
            match &column.data {
                Data::Bool(bitmap) => out.extend_from_slice(bitmap),
                Data::Int { bytes, .. } | Data::Float(bytes) => out.extend_from_slice(bytes),
                Data::String { offsets, blob, .. } => {
                    out.extend_from_slice(offsets);
                    out.extend_from_slice(blob);
                }
            }
            pad_to_8(&mut out);
        }
    }
    out
}
