//! A second sink beside [`crate::json`]: columns into a flat buffer instead of
//! decimal text, for a caller that can build its own objects from typed views.
//!
//! Mirrors `PACKAGE_PLAN.md` §5 in the browser package this crate feeds. Only
//! the table shape is covered — a root wrapped in the one-field envelope
//! (`Plan::is_envelope`), whose field is an array of structs the wire actually
//! encoded as a transposed table ([`crate::wire::Reader::is_table`]). Every
//! other shape returns `Ok(None)`, which tells the caller to fall back to
//! [`crate::walk::to_json`]: a table row can only hold a columnable op — see
//! [`plan::columnable_op`] — so a table never needs a null bitmap, a nested
//! struct or a map here, and the general document walk stays the one place
//! that handles those.
//!
//! # The buffer has no offset table
//!
//! Every run's byte length is derivable from `rowCount` and the field's own
//! kind — a bitmap is `ceil(rows/8)` bytes, an int or float run is `rows*8`,
//! and a string run's blob length is its own offset table's last entry. So the
//! reader walks the buffer once, in field order, computing each run's length
//! as it goes, rather than trusting a second copy of lengths that could
//! disagree with the data. See the module's `write_buffer` for the exact
//! layout, which the JavaScript reader mirrors.

use alloc::vec::Vec;

use crate::Error;
use crate::plan::{self, OP_STRING, OP_STRUCTS, Plan};
use crate::section::Schema;
use crate::walk::{Gathered, MAX_ROWS};
use crate::wire::{Reader, Reader8};

/// The largest integer magnitude a JavaScript `Number` holds exactly. Above
/// this the reader must pay for a `BigInt`, and the flag says so per column
/// rather than the reader guessing from the value itself.
const MAX_SAFE_INT: u64 = 9_007_199_254_740_991;

const KIND_BOOL: u8 = 0;
const KIND_INT: u8 = 1;
const KIND_FLOAT: u8 = 2;
const KIND_STRING: u8 = 3;

const FLAG_SIGNED: u8 = 1 << 0;
const FLAG_EXCEEDS_SAFE_INT: u8 = 1 << 1;
const FLAG_ASCII: u8 = 1 << 2;

/// One field's data, already in the shape its wire run will take.
enum Column {
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
    fn kind_and_flags(&self) -> (u8, u8) {
        match self {
            Column::Bool(_) => (KIND_BOOL, 0),
            Column::Int {
                signed, exceeds, ..
            } => {
                let mut flags = 0;
                if *signed {
                    flags |= FLAG_SIGNED;
                }
                if *exceeds {
                    flags |= FLAG_EXCEEDS_SAFE_INT;
                }
                (KIND_INT, flags)
            }
            Column::Float(_) => (KIND_FLOAT, 0),
            Column::String { ascii, .. } => {
                let flags = if *ascii { FLAG_ASCII } else { 0 };
                (KIND_STRING, flags)
            }
        }
    }
}

/// A message's columns, written into a flat buffer — or `None` when the root
/// is not the table shape this covers, which is not an error: the caller
/// falls back to [`crate::walk::to_json`].
///
/// # Errors
///
/// Any malformed message, the same as [`crate::walk::to_json`] would refuse.
pub fn to_buffer(schema: &Schema, message: &[u8], wide: bool) -> Result<Option<Vec<u8>>, Error> {
    let plan = schema.root();
    if !plan.is_envelope || plan.fields.len() != 1 {
        return Ok(None);
    }
    let field = &plan.fields[0];
    if field.op != OP_STRUCTS {
        return Ok(None);
    }
    let at = field.sub.ok_or(Error::BadSection)?;
    let sub_plan = schema.plan(at).ok_or(Error::BadSection)?;

    let (rows, gathered) = if wide {
        let mut reader = Reader8::new(message);
        if !(reader.more() && reader.key() == field.key) {
            return Ok(None);
        }
        if !reader.is_table() {
            return Ok(None);
        }
        let (rows, mut columns) = reader.table().ok_or(Error::Truncated)?;
        if rows > MAX_ROWS {
            return Err(Error::TooManyRows);
        }
        let mut gathered = Gathered::new(sub_plan.fields.len());
        while columns.more() {
            let Some(index) = sub_plan.field_of(columns.key()) else {
                columns.skip();
                columns.err()?;
                continue;
            };
            let op = sub_plan.fields[index].op;
            gathered.take8(&mut columns, index, op, rows);
            columns.err()?;
        }
        columns.err()?;
        reader.err()?;
        (rows, gathered)
    } else {
        let mut reader = Reader::new(message);
        if !(reader.more() && reader.key() == field.key) {
            return Ok(None);
        }
        if !reader.is_table() {
            return Ok(None);
        }
        let (rows, mut columns) = reader.counted().ok_or(Error::Truncated)?;
        if rows > MAX_ROWS {
            return Err(Error::TooManyRows);
        }
        let mut gathered = Gathered::new(sub_plan.fields.len());
        while columns.more() {
            let key = columns.key();
            let index = sub_plan.field_of(key).ok_or(Error::UnknownKey(key))?;
            let op = sub_plan.fields[index].op;
            gathered.take(&mut columns, index, op, rows);
            columns.err()?;
        }
        columns.err()?;
        reader.err()?;
        (rows, gathered)
    };

    let columns = build_columns(sub_plan, &gathered, rows)?;
    Ok(Some(write_buffer(sub_plan, rows, &columns)))
}

/// One gathered column, transformed to the value it means — the same
/// reinterpretation `Walker::column_value` does for the text sink, applied to
/// every row instead of one.
fn build_columns(table: &Plan, gathered: &Gathered, rows: usize) -> Result<Vec<Column>, Error> {
    let mut columns = Vec::with_capacity(table.fields.len());
    for (index, field) in table.fields.iter().enumerate() {
        let present = gathered.present[index];
        let raw_at = |row: usize| -> i64 {
            if present {
                gathered.ints[index].get(row).copied().unwrap_or(0)
            } else {
                0
            }
        };

        let column = match field.op {
            plan::OP_BOOL => {
                let mut bitmap = alloc::vec![0u8; rows.div_ceil(8)];
                for row in 0..rows {
                    if raw_at(row) == 1 {
                        bitmap[row >> 3] |= 1 << (row & 7);
                    }
                }
                Column::Bool(bitmap)
            }
            op if plan::is_integer_op(op) => {
                let signed = plan::is_signed_op(op);
                let width = plan::width_of_op(op);
                let mut bytes = Vec::with_capacity(rows * 8);
                let mut exceeds = false;
                for row in 0..rows {
                    let raw = raw_at(row);
                    let value: i64 = if signed {
                        if raw.unsigned_abs() > MAX_SAFE_INT {
                            exceeds = true;
                        }
                        raw
                    } else {
                        // Same truncation `column_value` applies: the column
                        // codec stores signed residuals, so a column narrower
                        // than 64 bits comes back sign-extended.
                        let value = if width == 8 {
                            raw as u64
                        } else {
                            (raw as u64) & (u64::MAX >> (64 - width * 8))
                        };
                        if value > MAX_SAFE_INT {
                            exceeds = true;
                        }
                        value as i64
                    };
                    bytes.extend_from_slice(&value.to_le_bytes());
                }
                Column::Int {
                    bytes,
                    signed,
                    exceeds,
                }
            }
            plan::OP_FLOAT32 => {
                let mut bytes = Vec::with_capacity(rows * 8);
                for row in 0..rows {
                    let value = f64::from(f32::from_bits(raw_at(row) as u32));
                    if value.is_nan() || value.is_infinite() {
                        return Err(Error::NotJson);
                    }
                    bytes.extend_from_slice(&value.to_le_bytes());
                }
                Column::Float(bytes)
            }
            plan::OP_FLOAT64 => {
                let mut bytes = Vec::with_capacity(rows * 8);
                for row in 0..rows {
                    let value = f64::from_bits(raw_at(row) as u64);
                    if value.is_nan() || value.is_infinite() {
                        return Err(Error::NotJson);
                    }
                    bytes.extend_from_slice(&value.to_le_bytes());
                }
                Column::Float(bytes)
            }
            OP_STRING => {
                let mut offsets = Vec::with_capacity((rows + 1) * 4);
                let mut blob = Vec::new();
                let mut ascii = true;
                offsets.extend_from_slice(&0u32.to_le_bytes());
                for row in 0..rows {
                    let value: &[u8] = if present {
                        gathered.strings[index].get(row).copied().unwrap_or(&[])
                    } else {
                        &[]
                    };
                    if ascii && !value.is_ascii() {
                        ascii = false;
                    }
                    blob.extend_from_slice(value);
                    let end = u32::try_from(blob.len()).map_err(|_| Error::SizeTooLarge)?;
                    offsets.extend_from_slice(&end.to_le_bytes());
                }
                Column::String {
                    offsets,
                    blob,
                    ascii,
                }
            }
            // `Plan::finish` refuses a table whose element is not fully
            // columnable, so nothing else can reach here.
            op => return Err(Error::UnwalkableOp(op)),
        };
        columns.push(column);
    }
    Ok(columns)
}

fn pad_to_8(out: &mut Vec<u8>) {
    let pad = out.len().next_multiple_of(8) - out.len();
    out.resize(out.len() + pad, 0);
}

/// The layout the module doc describes: a header naming every field, then one
/// data run per field in the same order, each aligned to 8 bytes so a
/// `Float64Array`/`BigInt64Array` view over it needs no copy.
fn write_buffer(table: &Plan, rows: usize, columns: &[Column]) -> Vec<u8> {
    let mut out = Vec::new();
    out.extend_from_slice(&1u32.to_le_bytes()); // version
    out.extend_from_slice(&(rows as u32).to_le_bytes());
    out.extend_from_slice(&(table.fields.len() as u32).to_le_bytes());

    for (name, column) in table.names.iter().zip(columns) {
        let (kind, flags) = column.kind_and_flags();
        out.push(kind);
        out.push(flags);
        let name_bytes = name.as_bytes();
        out.extend_from_slice(&(name_bytes.len() as u16).to_le_bytes());
        out.extend_from_slice(name_bytes);
    }
    pad_to_8(&mut out);

    for column in columns {
        match column {
            Column::Bool(bitmap) => out.extend_from_slice(bitmap),
            Column::Int { bytes, .. } | Column::Float(bytes) => out.extend_from_slice(bytes),
            Column::String { offsets, blob, .. } => {
                out.extend_from_slice(offsets);
                out.extend_from_slice(blob);
            }
        }
        pad_to_8(&mut out);
    }
    out
}
