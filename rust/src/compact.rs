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

/// Closes a record under `Keys8`; the id the standard mode also reserves.
const TERMINATOR_KEY: u64 = 255;
/// Closes a record under `Keys4`: the same role one nibble down.
const NARROW_TERMINATOR_KEY: u64 = 15;

pub(crate) fn decode(data: &[u8], schema: &Schema) -> Result<Vec<Record>, Error> {
    let mut reader = Reader::new(data)?;
    let count = reader.records;
    let mut out = Vec::with_capacity(count);
    for _ in 0..count {
        out.push(read_record(&mut reader, schema)?);
    }
    reader.check()?;
    Ok(out)
}

struct Reader<'a> {
    bits: BitReader<'a>,
    all_positive: bool,
    key_bits: u8,
    terminator: u64,
    records: usize,
}

impl<'a> Reader<'a> {
    fn new(data: &'a [u8]) -> Result<Self, Error> {
        let mut bits = BitReader::new(data);
        bits.get(1); // the compact bit, which decode() already discriminated on
        let all_positive = bits.get_bool();
        let shape = bits.get(2);
        let narrow = bits.get_bool();
        if let Some(err) = bits.err() {
            return Err(err);
        }
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
            Some(err) => Err(err),
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
    fn int(&mut self) -> i64 {
        let raw = varint::get_varint(&mut self.bits);
        if self.all_positive {
            raw as i64
        } else {
            varint::unzigzag(raw)
        }
    }

    fn uint(&mut self) -> u64 {
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
    fn string(&mut self) -> Result<String, Error> {
        self.check()?;
        let tail = self.bits.aligned_tail();
        let (bytes, consumed) = packed5::decode(&tail)?;
        // A well formed frame never extends into the final pad, so one that
        // would is corrupt rather than a short read of valid data.
        if !self.bits.advance_bytes(consumed) {
            return Err(Error::Truncated);
        }
        String::from_utf8(bytes).map_err(|_| Error::NotUtf8)
    }

    fn bytes(&mut self) -> Result<Vec<u8>, Error> {
        let count = self.array_len(8)?;
        let mut out = Vec::with_capacity(count);
        for _ in 0..count {
            out.push(self.bits.get(8) as u8);
        }
        self.check()?;
        Ok(out)
    }

    /// An integer slice, written with the same array codec that backs an integer
    /// column in standard mode — which is why an array costs the same in either
    /// mode.
    fn ints(&mut self, width: u8) -> Result<Vec<i64>, Error> {
        // The array codec spends at least one byte per element.
        let count = self.array_len(8)?;
        if count == 0 {
            return Ok(Vec::new());
        }
        let tail = self.bits.aligned_tail();
        let (values, consumed) = varint::decode_array(&tail, count, width)?;
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
fn read_record(reader: &mut Reader<'_>, schema: &Schema) -> Result<Record, Error> {
    let mut record = Record::with_capacity(schema.fields().len());
    loop {
        let key = reader.key();
        reader.check()?;
        if key == TERMINATOR_KEY {
            return Ok(record);
        }
        let id = key as u8;
        let kind = schema.kind(id).ok_or(Error::UnknownField(id))?;
        let value = read_value(reader, kind)?;
        reader.check()?;
        record.push(id, value);
    }
}

fn read_value(reader: &mut Reader<'_>, kind: Kind) -> Result<Value, Error> {
    if let Some((width, signed)) = kind.scalar_int() {
        // The value is truncated to the field's width, which never reaches the
        // wire, exactly as the Go decoder's `*(*int8)(q) = int8(r.Int())` does.
        return Ok(if signed {
            Value::Int(varint::sign_extend(reader.int() as u64, width))
        } else {
            Value::Uint(mask(reader.uint(), width))
        });
    }
    if let Some((width, signed)) = kind.slice_int() {
        let values = reader.ints(width)?;
        return Ok(if signed {
            Value::Ints(values)
        } else {
            Value::Uints(values.into_iter().map(|v| mask(v as u64, width)).collect())
        });
    }
    Ok(match kind {
        Kind::Bool => Value::Bool(reader.bits.get_bool()),
        Kind::Float32 => Value::Float32(f32::from_bits(reader.bits.get(32) as u32)),
        Kind::Float64 => Value::Float64(f64::from_bits(reader.bits.get(64))),
        Kind::String => Value::String(reader.string()?),
        Kind::Bytes => Value::Bytes(reader.bytes()?),
        Kind::Bools => {
            // The one array form that does not match standard mode, which
            // carries bools through an integer column: a compact bool is already
            // a single bit, and a slice of them is a bitmap.
            let count = reader.array_len(1)?;
            let mut out = Vec::with_capacity(count);
            for _ in 0..count {
                out.push(reader.bits.get_bool());
            }
            Value::Bools(out)
        }
        Kind::Strings => {
            let count = reader.array_len(8)?;
            let mut out = Vec::with_capacity(count);
            for _ in 0..count {
                out.push(reader.string()?);
            }
            Value::Strings(out)
        }
        Kind::Float32s => {
            let count = reader.array_len(32)?;
            let mut out = Vec::with_capacity(count);
            for _ in 0..count {
                out.push(f32::from_bits(reader.bits.get(32) as u32));
            }
            Value::Float32s(out)
        }
        Kind::Float64s => {
            let count = reader.array_len(64)?;
            let mut out = Vec::with_capacity(count);
            for _ in 0..count {
                out.push(f64::from_bits(reader.bits.get(64)));
            }
            Value::Float64s(out)
        }
        // Every other kind was handled by the two width tables above.
        _ => unreachable!("kind {kind:?} has no compact value form"),
    })
}

/// Truncates an unsigned value to its declared width.
fn mask(value: u64, width: u8) -> u64 {
    if width >= 64 {
        value
    } else {
        value & ((1_u64 << width) - 1)
    }
}
