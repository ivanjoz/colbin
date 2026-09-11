//! Standard mode, mirroring `codec/decode.go`.
//!
//! The columnar layout: a version byte, a record count, a column count, and then
//! one self-identifying column per field.
//!
//! ```text
//! [version:1] [recordCount:uvarint] [colCount:1] ([id:1] [column])*
//! ```
//!
//! A column opens with a type byte whose low three bits name its class. Bit 7 of
//! that byte says the column held nothing but empty values and carries no
//! payload at all, which is what a zero value costs under omit-empty; version
//! 0x06 rather than 0x02 is how a message says it may contain one, so that a
//! decoder predating the flag rejects the message instead of reading past the
//! column it did not know was absent.

use crate::{Error, Kind, Record, Schema, Value, packed5, try_vec, varint};

/// Version bytes: dense, and written with omit-empty on.
const VERSION_DENSE: u8 = 0x02;
const VERSION_OMIT_EMPTY: u8 = 0x06;
/// The two self-describing versions `MarshalJSON` writes.
const VERSION_JSON: u8 = 0x04;
const VERSION_JSON_OMIT_EMPTY: u8 = 0x08;

// Field type classes, in the low three bits of a column's type byte.
const FT_INT: u8 = 0;
const FT_FLOAT: u8 = 1;
const FT_STRING: u8 = 2;
const FT_BYTES: u8 = 3;
const FT_ARRAY: u8 = 4;

/// Set in a column's type byte to say the column holds nothing but empty values.
const EMPTY_COLUMN_BIT: u8 = 1 << 7;

pub(crate) fn decode(data: &[u8], schema: &Schema) -> Result<Vec<Record>, Error> {
    let mut cursor = Cursor { data, pos: 0 };
    match cursor.byte()? {
        VERSION_DENSE | VERSION_OMIT_EMPTY => {}
        version @ (VERSION_JSON | VERSION_JSON_OMIT_EMPTY) => {
            return Err(Error::SelfDescribing(version));
        }
        version => return Err(Error::BadVersion(version)),
    }

    let count = cursor.uvarint()?;
    if count > usize::MAX as u64 {
        return Err(Error::TooLarge(count));
    }
    let count = count as usize;
    let mut records: Vec<Record> = try_vec(count)?;
    records.resize_with(count, Record::default);

    let columns = cursor.byte()?;
    for _ in 0..columns {
        let id = cursor.byte()?;
        let kind = schema.kind(id).ok_or(Error::UnknownField(id))?;
        for (record, value) in records
            .iter_mut()
            .zip(read_column(&mut cursor, id, kind, count)?)
        {
            record.push(id, value);
        }
    }
    Ok(records)
}

/// One column's worth of values, one per record.
fn read_column(
    cursor: &mut Cursor<'_>,
    id: u8,
    kind: &Kind,
    count: usize,
) -> Result<Vec<Value>, Error> {
    if let Some(elem) = Elem::of_slice(kind) {
        // An array column is a per-record length sub-column followed by every
        // record's elements flattened into one element column.
        let flags = cursor.byte()?;
        if flags & 7 != FT_ARRAY {
            return Err(column_type_error(id, "an array", flags));
        }
        let lengths = read_int_column(cursor, id, count, 64)?;
        let mut total = 0_usize;
        for length in &lengths {
            let length = usize::try_from(*length).map_err(|_| Error::Truncated)?;
            total = total.checked_add(length).ok_or(Error::Truncated)?;
        }
        let flattened = read_elem_column(cursor, id, elem, total)?;
        return split(kind, flattened, &lengths);
    }
    // A composite kind reaches here only in standard mode, which this decoder
    // reads flat: compact mode carries nested structs, arrays of structs and maps,
    // the columnar layout carries them as sub-tables, and only the former is
    // ported. Refused rather than panicked on.
    let elem = Elem::of_scalar(kind).ok_or(Error::CompositeUnsupported { id })?;
    Ok(read_elem_column(cursor, id, elem, count)?.into_values(kind))
}

/// The scalar forms a column can hold, which are also the element forms an array
/// column's flattened sub-column can hold.
#[derive(Clone, Copy)]
enum Elem {
    /// Bools travel in an integer column here, unlike compact mode's bitmap.
    Bool,
    Int {
        bits: u8,
        signed: bool,
    },
    Float32,
    Float64,
    Str,
    Bytes,
}

impl Elem {
    fn of_scalar(kind: &Kind) -> Option<Self> {
        if let Some((bits, signed)) = kind.scalar_int() {
            return Some(Self::Int { bits, signed });
        }
        Some(match kind {
            Kind::Bool => Self::Bool,
            Kind::Float32 => Self::Float32,
            Kind::Float64 => Self::Float64,
            Kind::String => Self::Str,
            Kind::Bytes => Self::Bytes,
            _ => return None,
        })
    }

    fn of_slice(kind: &Kind) -> Option<Self> {
        if let Some((bits, signed)) = kind.slice_int() {
            return Some(Self::Int { bits, signed });
        }
        Some(match kind {
            Kind::Bools => Self::Bool,
            Kind::Strings => Self::Str,
            Kind::Float32s => Self::Float32,
            Kind::Float64s => Self::Float64,
            _ => return None,
        })
    }
}

/// A decoded column, before it is cut into per-record values.
enum Column {
    Ints(Vec<i64>),
    Uints(Vec<u64>),
    Bools(Vec<bool>),
    Float32s(Vec<f32>),
    Float64s(Vec<f64>),
    Strings(Vec<String>),
    Blobs(Vec<Vec<u8>>),
}

impl Column {
    /// Hands each value out as the scalar kind the schema declared.
    fn into_values(self, kind: &Kind) -> Vec<Value> {
        match self {
            Self::Ints(values) => values.into_iter().map(Value::Int).collect(),
            Self::Uints(values) => values.into_iter().map(Value::Uint).collect(),
            Self::Bools(values) => values.into_iter().map(Value::Bool).collect(),
            // A float column declares its own width, which is what says how many
            // bytes it occupies; the schema's kind is what the value is handed
            // back as, exactly as the Go decoder stores into the field's type.
            Self::Float32s(values) => match kind {
                Kind::Float64 => values
                    .into_iter()
                    .map(|v| Value::Float64(v.into()))
                    .collect(),
                _ => values.into_iter().map(Value::Float32).collect(),
            },
            Self::Float64s(values) => match kind {
                Kind::Float32 => values
                    .into_iter()
                    .map(|v| Value::Float32(v as f32))
                    .collect(),
                _ => values.into_iter().map(Value::Float64).collect(),
            },
            Self::Strings(values) => values.into_iter().map(Value::String).collect(),
            Self::Blobs(values) => values.into_iter().map(Value::Bytes).collect(),
        }
    }
}

/// Cuts a flattened element column into one slice value per record.
fn split(kind: &Kind, column: Column, lengths: &[i64]) -> Result<Vec<Value>, Error> {
    let mut out = Vec::with_capacity(lengths.len());
    let mut at = 0_usize;
    // Every length was range checked while the total was summed.
    let mut next = |n: i64| {
        let start = at;
        at += n as usize;
        start..at
    };
    match column {
        Column::Ints(values) => {
            for length in lengths {
                out.push(Value::Ints(values[next(*length)].to_vec()));
            }
        }
        Column::Uints(values) => {
            for length in lengths {
                out.push(Value::Uints(values[next(*length)].to_vec()));
            }
        }
        Column::Bools(values) => {
            for length in lengths {
                out.push(Value::Bools(values[next(*length)].to_vec()));
            }
        }
        Column::Float32s(values) => {
            for length in lengths {
                let range = next(*length);
                out.push(match kind {
                    Kind::Float64s => {
                        Value::Float64s(values[range].iter().map(|v| f64::from(*v)).collect())
                    }
                    _ => Value::Float32s(values[range].to_vec()),
                });
            }
        }
        Column::Float64s(values) => {
            for length in lengths {
                let range = next(*length);
                out.push(match kind {
                    Kind::Float32s => {
                        Value::Float32s(values[range].iter().map(|v| *v as f32).collect())
                    }
                    _ => Value::Float64s(values[range].to_vec()),
                });
            }
        }
        Column::Strings(values) => {
            for length in lengths {
                out.push(Value::Strings(values[next(*length)].to_vec()));
            }
        }
        // A slice of blobs is `[][]byte`, whose element is an ftBytes column.
        // No kind names it, so `Elem::of_slice` never produces one.
        Column::Blobs(_) => unreachable!("no slice kind has a bytes element"),
    }
    Ok(out)
}

fn read_elem_column(
    cursor: &mut Cursor<'_>,
    id: u8,
    elem: Elem,
    count: usize,
) -> Result<Column, Error> {
    Ok(match elem {
        Elem::Int { bits, signed } => {
            let values = read_int_column(cursor, id, count, bits)?;
            if signed {
                Column::Ints(values)
            } else {
                Column::Uints(values.into_iter().map(|v| mask(v as u64, bits)).collect())
            }
        }
        Elem::Bool => Column::Bools(
            read_int_column(cursor, id, count, 8)?
                .into_iter()
                .map(|v| v != 0)
                .collect(),
        ),
        Elem::Float32 | Elem::Float64 => read_float_column(cursor, id, count)?,
        Elem::Str => read_string_column(cursor, id, count)?,
        Elem::Bytes => {
            let flags = cursor.byte()?;
            if flags & 7 != FT_BYTES {
                return Err(column_type_error(id, "bytes", flags));
            }
            let lengths = read_int_column(cursor, id, count, 64)?;
            let mut out = try_vec(count)?;
            for length in lengths {
                let length = usize::try_from(length).map_err(|_| Error::Truncated)?;
                out.push(cursor.take(length)?.to_vec());
            }
            Column::Blobs(out)
        }
    })
}

/// A colbin type byte followed by one varint array frame. The empty bit says the
/// column held nothing but zeros and carries no frame at all.
fn read_int_column(
    cursor: &mut Cursor<'_>,
    id: u8,
    count: usize,
    width: u8,
) -> Result<Vec<i64>, Error> {
    let flags = cursor.byte()?;
    if flags & 7 != FT_INT {
        return Err(column_type_error(id, "an integer", flags));
    }
    let mut out: Vec<i64> = try_vec(count)?;
    if flags & EMPTY_COLUMN_BIT != 0 {
        out.resize(count, 0);
        return Ok(out);
    }
    let (values, consumed) = varint::decode_array(cursor.rest(), count, width)?;
    cursor.advance(consumed)?;
    Ok(values)
}

/// A flags byte carrying the precision, then raw IEEE-754 payload.
fn read_float_column(cursor: &mut Cursor<'_>, id: u8, count: usize) -> Result<Column, Error> {
    let flags = cursor.byte()?;
    if flags & 7 != FT_FLOAT {
        return Err(column_type_error(id, "a float", flags));
    }
    let wide = (flags >> 4) & 7 == 1;
    if flags & EMPTY_COLUMN_BIT != 0 {
        return Ok(if wide {
            let mut out = try_vec(count)?;
            out.resize(count, 0.0_f64);
            Column::Float64s(out)
        } else {
            let mut out = try_vec(count)?;
            out.resize(count, 0.0_f32);
            Column::Float32s(out)
        });
    }
    if wide {
        let mut out = try_vec(count)?;
        for _ in 0..count {
            let bytes: [u8; 8] = cursor.take(8)?.try_into().expect("take returned 8 bytes");
            out.push(f64::from_bits(u64::from_le_bytes(bytes)));
        }
        Ok(Column::Float64s(out))
    } else {
        let mut out = try_vec(count)?;
        for _ in 0..count {
            let bytes: [u8; 4] = cursor.take(4)?.try_into().expect("take returned 4 bytes");
            out.push(f32::from_bits(u32::from_le_bytes(bytes)));
        }
        Ok(Column::Float32s(out))
    }
}

/// A flags byte, then `count` consecutive self-delimiting packed5 frames.
fn read_string_column(cursor: &mut Cursor<'_>, id: u8, count: usize) -> Result<Column, Error> {
    let flags = cursor.byte()?;
    if flags & 7 != FT_STRING {
        return Err(column_type_error(id, "a string", flags));
    }
    let mut out: Vec<String> = try_vec(count)?;
    if flags & EMPTY_COLUMN_BIT != 0 {
        out.resize(count, String::new());
        return Ok(Column::Strings(out));
    }
    for _ in 0..count {
        let (bytes, consumed) = packed5::decode(cursor.rest())?;
        cursor.advance(consumed)?;
        out.push(String::from_utf8(bytes).map_err(|_| Error::NotUtf8)?);
    }
    Ok(Column::Strings(out))
}

/// A column whose class the schema did not expect. The three classes this
/// decoder does not carry at all are reported as such, since "expected an
/// integer, found a map" is a scope answer rather than a schema-mismatch one.
fn column_type_error(id: u8, expected: &'static str, flags: u8) -> Error {
    match flags & 7 {
        class @ 5..=7 => Error::UnsupportedColumn { id, class },
        _ => Error::ColumnType {
            id,
            expected,
            found: flags,
        },
    }
}

fn mask(value: u64, width: u8) -> u64 {
    if width >= 64 {
        value
    } else {
        value & ((1_u64 << width) - 1)
    }
}

/// Walks the byte stream; each column computes its own span, so the cursor can
/// step to the next one.
struct Cursor<'a> {
    data: &'a [u8],
    pos: usize,
}

impl<'a> Cursor<'a> {
    fn byte(&mut self) -> Result<u8, Error> {
        let byte = *self.data.get(self.pos).ok_or(Error::Truncated)?;
        self.pos += 1;
        Ok(byte)
    }

    fn take(&mut self, length: usize) -> Result<&'a [u8], Error> {
        let end = self.pos.checked_add(length).ok_or(Error::Truncated)?;
        let slice = self.data.get(self.pos..end).ok_or(Error::Truncated)?;
        self.pos = end;
        Ok(slice)
    }

    fn rest(&self) -> &'a [u8] {
        &self.data[self.pos..]
    }

    fn advance(&mut self, length: usize) -> Result<(), Error> {
        self.take(length).map(|_| ())
    }

    /// The record count, as an LEB128 uvarint.
    fn uvarint(&mut self) -> Result<u64, Error> {
        let mut value = 0_u64;
        let mut shift = 0_u32;
        loop {
            let byte = self.byte().map_err(|_| Error::BadRecordCount)?;
            value |= u64::from(byte & 0x7F) << shift;
            if byte & 0x80 == 0 {
                return Ok(value);
            }
            shift += 7;
            if shift > 63 {
                return Err(Error::BadRecordCount);
            }
        }
    }
}
