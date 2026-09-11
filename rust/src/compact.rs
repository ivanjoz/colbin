//! Compact mode, mirroring the `compact` package.
//!
//! One record, or an array of at most three, in a single LSB-first bitstream
//! with no alignment anywhere until a final pad to a byte boundary. Five header
//! bits, then one record per shape:
//!
//! ```text
//! bit  0    1              compact mode
//! bit  1    ALL_POSITIVE   every integer in the message is >= 0
//! bits 2-3  shape          0 = lone struct, 1..3 = array of that many records
//! bit  4    NARROW_KEYS    field ids are 4 bits wide rather than 8
//! ```
//!
//! A record is a run of `[key][value]` pairs closed by the terminator key. A
//! field holding its zero value is omitted entirely, so the key run *is* the
//! presence information and an absent field costs nothing beyond the terminator
//! the record already owes.

use crate::bitstream::BitReader;
use crate::{Error, Kind, Record, Schema, Value, packed5, varint};

const SHAPE_STRUCT: u64 = 0;
/// The largest array compact mode can carry.
const MAX_RECORDS: u64 = 3;

/// The header: the compact bit, ALL_POSITIVE, two shape bits and NARROW_KEYS.
const HEADER_BITS: u8 = 5;

/// Closes a record under `Keys8`; the id the standard mode also reserves.
const TERMINATOR_KEY: u64 = 255;
/// Closes a record under `Keys4`: the same role one nibble down.
const NARROW_TERMINATOR_KEY: u64 = 15;

pub(crate) fn decode(data: &[u8], schema: &Schema) -> Result<Vec<Record>, Error> {
    let mut out = Vec::new();
    decode_into(data, schema, &mut out)?;
    Ok(out)
}

/// Decodes into `dst`, reusing the `Record`s it already holds and their field
/// vectors. A reused record is cleared for the reason the module documents: an
/// omitted field is simply not written, so the previous message's value would
/// otherwise survive underneath it.
pub(crate) fn decode_into(
    data: &[u8],
    schema: &Schema,
    dst: &mut Vec<Record>,
) -> Result<(), Error> {
    let mut reader = Reader::new(data)?;
    let count = reader.records;
    // Grown, never shrunk: a caller alternating a long message with a short one
    // keeps the capacity the long one needed.
    if dst.len() < count {
        dst.resize_with(count, Record::new);
    }
    for record in dst.iter_mut().take(count) {
        read_record_into(&mut reader, schema, record)?;
    }
    dst.truncate(count);
    reader.check()
}

/// Decodes a one-record message straight into a derived struct's fields.
///
/// No `Record` and no `Value`: the schema maps each wire id to a declaration
/// index and the generated `colbin_read` assigns to the field at that position.
pub(crate) fn decode_one_typed<T: crate::Colbin>(
    data: &[u8],
    schema: &Schema,
    dst: &mut T,
) -> Result<(), Error> {
    let mut reader = Reader::new(data)?;
    if reader.records != 1 {
        return Err(Error::RecordCount(reader.records));
    }
    // An omitted field is not written at all, so the destination starts zeroed
    // rather than being merged into.
    *dst = T::colbin_zero();
    loop {
        let key = reader.key();
        reader.check()?;
        if key == TERMINATOR_KEY {
            return reader.check();
        }
        let id = key as u8;
        let index = schema.index(id).ok_or(Error::UnknownField(id))?;
        let mut field = crate::FieldReader {
            reader: &mut reader,
        };
        dst.colbin_read(index, &mut field)?;
        reader.check()?;
    }
}

/// Decodes a one-record message into `dst`, without the `Vec<Record>` that
/// holding the record in a slice would cost.
pub(crate) fn decode_one_into(data: &[u8], schema: &Schema, dst: &mut Record) -> Result<(), Error> {
    let mut reader = Reader::new(data)?;
    if reader.records != 1 {
        return Err(Error::RecordCount(reader.records));
    }
    read_record_into(&mut reader, schema, dst)?;
    reader.check()
}

pub(crate) struct Reader<'a> {
    bits: BitReader<'a>,
    all_positive: bool,
    key_bits: u8,
    terminator: u64,
    records: usize,
}

impl<'a> Reader<'a> {
    fn new(data: &'a [u8]) -> Result<Self, Error> {
        let mut bits = BitReader::new(data);
        // The five header bits in one read. Four reads of one, one, two and one
        // bit cost four bounds checks and four window loads for what is a single
        // shift and mask, and this is paid once per message.
        let header = bits.get(HEADER_BITS);
        if let Some(err) = bits.err() {
            return Err(err.clone());
        }
        // bit 0 is the compact bit, which decode() already discriminated on.
        let all_positive = header & 0b0_0010 != 0;
        let shape = (header >> 2) & 0b11;
        let narrow = header & 0b1_0000 != 0;
        // Shape 0 and shape 1 both carry one record; they differ only in whether
        // a JSON reader renders it as an object or as an array of one, which is
        // not something a schema-driven decode has to decide.
        let records = if shape == SHAPE_STRUCT {
            1
        } else {
            shape.min(MAX_RECORDS) as usize
        };
        let (key_bits, terminator) = if narrow {
            (4, NARROW_TERMINATOR_KEY)
        } else {
            (8, TERMINATOR_KEY)
        };
        Ok(Self {
            bits,
            all_positive,
            key_bits,
            terminator,
            records,
        })
    }

    fn check(&self) -> Result<(), Error> {
        match self.bits.err() {
            Some(err) => Err(err.clone()),
            None => Ok(()),
        }
    }

    /// Reads a field id at whichever width the header declared, reporting the
    /// terminator as 255 on both paths: `Keys4` reserves 15 the way `Keys8`
    /// reserves 255, so no field can hold either.
    fn key(&mut self) -> u64 {
        let key = self.bits.get(self.key_bits);
        if key == self.terminator {
            TERMINATOR_KEY
        } else {
            key
        }
    }

    /// A signed integer: the payload is the magnitude when ALL_POSITIVE is set
    /// and the zigzag when it is not.
    pub(crate) fn int(&mut self) -> i64 {
        let raw = varint::get_varint(&mut self.bits);
        if self.all_positive {
            raw as i64
        } else {
            varint::unzigzag(raw)
        }
    }

    pub(crate) fn uint(&mut self) -> u64 {
        varint::get_varint(&mut self.bits)
    }

    /// An element count, checked against the bits actually remaining given the
    /// fewest bits one element can occupy. A corrupt count near 2^64 has to be
    /// rejected before it is used to size anything.
    fn array_len(&mut self, min_bits_per_elem: usize) -> Result<usize, Error> {
        let count = varint::get_varint(&mut self.bits);
        self.check()?;
        if count > (self.bits.remaining() / min_bits_per_elem) as u64 {
            return Err(Error::Truncated);
        }
        Ok(count as usize)
    }

    /// A packed5 frame. The codec is byte structured, so the frame simply starts
    /// wherever the bitstream happens to be and the tail is shifted into place
    /// when that is not a byte boundary.
    pub(crate) fn string(&mut self) -> Result<String, Error> {
        self.check()?;
        // The borrow of the shifted tail ends here, before advance_bytes needs
        // the reader again.
        let (bytes, consumed) = packed5::decode(self.bits.aligned_tail())?;
        // A well formed frame never extends into the final pad, so one that
        // would is corrupt rather than a short read of valid data.
        if !self.bits.advance_bytes(consumed) {
            return Err(Error::Truncated);
        }
        String::from_utf8(bytes).map_err(|_| Error::NotUtf8)
    }

    pub(crate) fn bytes(&mut self) -> Result<Vec<u8>, Error> {
        let count = self.array_len(8)?;
        let mut out = Vec::with_capacity(count);
        for _ in 0..count {
            out.push(self.bits.get(8) as u8);
        }
        self.check()?;
        Ok(out)
    }

    /// One bit.
    pub(crate) fn bit(&mut self) -> bool {
        self.bits.get_bool()
    }

    pub(crate) fn f32(&mut self) -> f32 {
        f32::from_bits(self.bits.get(32) as u32)
    }

    pub(crate) fn f64(&mut self) -> f64 {
        f64::from_bits(self.bits.get(64))
    }

    /// A bitmap, one bit per element: the one array form that does not match
    /// standard mode, which carries bools through an integer column.
    pub(crate) fn bools(&mut self) -> Result<Vec<bool>, Error> {
        let count = self.array_len(1)?;
        let mut out = crate::try_vec(count)?;
        for _ in 0..count {
            out.push(self.bits.get_bool());
        }
        Ok(out)
    }

    pub(crate) fn strings(&mut self) -> Result<Vec<String>, Error> {
        let count = self.array_len(8)?;
        let mut out = crate::try_vec(count)?;
        for _ in 0..count {
            out.push(self.string()?);
        }
        Ok(out)
    }

    pub(crate) fn f32s(&mut self) -> Result<Vec<f32>, Error> {
        let count = self.array_len(32)?;
        let mut out = crate::try_vec(count)?;
        for _ in 0..count {
            out.push(f32::from_bits(self.bits.get(32) as u32));
        }
        Ok(out)
    }

    pub(crate) fn f64s(&mut self) -> Result<Vec<f64>, Error> {
        let count = self.array_len(64)?;
        let mut out = crate::try_vec(count)?;
        for _ in 0..count {
            out.push(f64::from_bits(self.bits.get(64)));
        }
        Ok(out)
    }

    /// An integer slice, written with the same array codec that backs an integer
    /// column in standard mode — which is why an array costs the same in either
    /// mode.
    pub(crate) fn ints(&mut self, width: u8) -> Result<Vec<i64>, Error> {
        // The array codec spends at least one byte per element.
        let count = self.array_len(8)?;
        if count == 0 {
            return Ok(Vec::new());
        }
        let (values, consumed) = varint::decode_array(self.bits.aligned_tail(), count, width)?;
        if !self.bits.advance_bytes(consumed) {
            return Err(Error::Truncated);
        }
        Ok(values)
    }
}

/// Fills one record, stopping at its terminator.
///
/// An id the schema does not know is an error rather than a skip: the wire holds
/// no type tag, so there is no way to know how far to step over it.
///
/// This is also how a nested struct is read, and how one element of an array of
/// structs is: a nested struct is another record closed by the same terminator,
/// so the recursion changes the depth and not the framing. `Schema::build`
/// already bounded that depth, so no counter is threaded through here.
fn read_record(reader: &mut Reader<'_>, schema: &Schema) -> Result<Record, Error> {
    let mut record = Record::with_capacity(schema.fields().len());
    read_record_into(reader, schema, &mut record)?;
    Ok(record)
}

/// `read_record` into a record the caller owns, so its field vector survives
/// between messages.
fn read_record_into(
    reader: &mut Reader<'_>,
    schema: &Schema,
    record: &mut Record,
) -> Result<(), Error> {
    record.clear();
    record.reserve(schema.fields().len());
    loop {
        let key = reader.key();
        reader.check()?;
        if key == TERMINATOR_KEY {
            return Ok(());
        }
        let id = key as u8;
        let kind = schema.kind(id).ok_or(Error::UnknownField(id))?;
        let value = read_value(reader, kind)?;
        reader.check()?;
        record.push(id, value);
    }
}

/// The fewest bits one value of this kind can occupy, which is what
/// bounds-checking a composite's count needs: a corrupt count near 2^64 has to
/// be rejected before it sizes an allocation. It mirrors `compactElemMinBits`.
fn min_bits(reader: &Reader<'_>, kind: &Kind) -> usize {
    match kind {
        Kind::Bool => 1,
        Kind::Float32 => 32,
        Kind::Float64 => 64,
        // An empty record is its terminator alone.
        Kind::Struct(_) => usize::from(reader.key_bits),
        // Everything else opens with a varint unit, a packed5 frame or a count.
        _ => 8,
    }
}

/// Reads one value at the kind the schema declares.
///
/// **One exhaustive match**, deliberately. This used to walk up to four
/// sequential matches on `kind` — a composite check, then `scalar_int`, then
/// `slice_int`, then a final match — and dispatch was the whole cost of a field:
/// a `bool` field, which is a single bit on the wire, measured the same 34 ns as
/// an `i32`, and an `f64` measured *less*. Collapsing them to one jump table
/// took a seven-field record from 231 ns to well under half that.
///
/// The width tables (`Kind::scalar_int`, `Kind::slice_int`) still exist for the
/// standard-mode reader, which needs the width as a value rather than as a
/// branch. Here the width is a constant per arm, which is the point.
fn read_value(reader: &mut Reader<'_>, kind: &Kind) -> Result<Value, Error> {
    // A signed value is truncated to the field's width, which never reaches the
    // wire, exactly as the Go decoder's `*(*int8)(q) = int8(r.Int())` does.
    Ok(match kind {
        Kind::Bool => Value::Bool(reader.bit()),
        Kind::Int8 => Value::Int(varint::sign_extend(reader.int() as u64, 8)),
        Kind::Int16 => Value::Int(varint::sign_extend(reader.int() as u64, 16)),
        Kind::Int32 => Value::Int(varint::sign_extend(reader.int() as u64, 32)),
        // Already 64 bits wide, so there is nothing to sign extend from.
        Kind::Int64 => Value::Int(reader.int()),
        Kind::Uint8 => Value::Uint(mask(reader.uint(), 8)),
        Kind::Uint16 => Value::Uint(mask(reader.uint(), 16)),
        Kind::Uint32 => Value::Uint(mask(reader.uint(), 32)),
        Kind::Uint64 => Value::Uint(reader.uint()),
        Kind::Float32 => Value::Float32(reader.f32()),
        Kind::Float64 => Value::Float64(reader.f64()),
        Kind::String => Value::String(reader.string()?),
        Kind::Bytes => Value::Bytes(reader.bytes()?),

        Kind::Int8s => Value::Ints(reader.ints(8)?),
        Kind::Int16s => Value::Ints(reader.ints(16)?),
        Kind::Int32s => Value::Ints(reader.ints(32)?),
        Kind::Int64s => Value::Ints(reader.ints(64)?),
        // An unsigned slice rode the same-width signed codec, which preserved the
        // bit pattern; masking is what hands it back unsigned.
        Kind::Uint16s => Value::Uints(unsigned(reader.ints(16)?, 16)),
        Kind::Uint32s => Value::Uints(unsigned(reader.ints(32)?, 32)),
        Kind::Uint64s => Value::Uints(unsigned(reader.ints(64)?, 64)),
        Kind::Bools => Value::Bools(reader.bools()?),
        Kind::Strings => Value::Strings(reader.strings()?),
        Kind::Float32s => Value::Float32s(reader.f32s()?),
        Kind::Float64s => Value::Float64s(reader.f64s()?),

        // Composites recurse through the same bitstream: a nested key run, a
        // counted run of values, a counted run of key/value pairs. Every form is
        // self delimiting against the schema, which is all the format asks.
        Kind::Struct(schema) => Value::Record(read_record(reader, schema)?),
        Kind::Array(elem) => {
            let count = reader.array_len(min_bits(reader, elem))?;
            let mut out = crate::try_vec(count)?;
            for _ in 0..count {
                out.push(read_value(reader, elem)?);
            }
            Value::Array(out)
        }
        Kind::Map(key, val) => {
            let count = reader.array_len(min_bits(reader, key) + min_bits(reader, val))?;
            let mut out = crate::try_vec(count)?;
            for _ in 0..count {
                let k = read_value(reader, key)?;
                let v = read_value(reader, val)?;
                out.push((k, v));
            }
            Value::Map(out)
        }
    })
}

/// Masks a signed-codec slice back to its unsigned element width.
fn unsigned(values: Vec<i64>, width: u8) -> Vec<u64> {
    values.into_iter().map(|v| mask(v as u64, width)).collect()
}

/// Truncates an unsigned value to its declared width.
fn mask(value: u64, width: u8) -> u64 {
    if width >= 64 {
        value
    } else {
        value & ((1_u64 << width) - 1)
    }
}
