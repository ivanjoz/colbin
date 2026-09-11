//! The compact-mode encoder: the inverse of `compact.rs`, over the same
//! [`Schema`] and the same [`Record`]/[`Value`] tree.
//!
//! Only compact mode. The columnar layout amortises per-column framing over many
//! records and is what Go reaches for past three of them; compact mode is what a
//! single record *is*, and a single record is what crosses this boundary.
//!
//! # What it reproduces exactly, and what it does not
//!
//! Byte for byte identical to Go's encoder except for strings. Every framing
//! decision is the same one Go makes — the omit-zero rule, `ALL_POSITIVE`,
//! `NARROW_KEYS`, and the integer array codec's full transform search — so a
//! message Rust writes and a message Go writes for the same value differ only
//! where a string does.
//!
//! Strings go out as raw packed5 frames rather than packed ones. See
//! [`crate::packed5::encode_raw`] for the trade.

use crate::bitstream::BitWriter;
use crate::packed5;
use crate::varint;
use crate::{Error, Kind, Record, Schema, Value};

/// The schema facts an encode needs, resolved once.
///
/// `narrow_keys` is a recursive walk of the whole schema tree and
/// `composite_reaches_signed` is another; recomputing them per message measured
/// 12 ns of a 134 ns encode, for answers that cannot change between calls. A
/// [`crate::Codec`] holds one of these; the free functions build one per call,
/// which is what they are for.
#[derive(Clone, Copy, Debug)]
pub(crate) struct Plan {
    pub(crate) narrow: bool,
    pub(crate) key_bits: u8,
    pub(crate) terminator: u64,
    /// Whether a signed integer is reachable through a composite, which is when
    /// Go gives up on the ALL_POSITIVE pre-scan.
    pub(crate) signed_reach: bool,
}

impl Plan {
    pub(crate) fn new(schema: &Schema) -> Self {
        let narrow = schema.narrow_keys();
        Self {
            narrow,
            key_bits: if narrow { 4 } else { 8 },
            terminator: if narrow {
                NARROW_TERMINATOR_KEY
            } else {
                TERMINATOR_KEY
            },
            signed_reach: schema
                .fields()
                .iter()
                .any(|field| composite_reaches_signed(&field.kind)),
        }
    }
}

/// The largest array compact mode can carry: the shape is two header bits.
pub const MAX_RECORDS: usize = 3;

pub(crate) const SHAPE_STRUCT: u64 = 0;
const TERMINATOR_KEY: u64 = 255;
const NARROW_TERMINATOR_KEY: u64 = 15;

const MODE_BITS: u8 = 1;
const FLAG_BITS: u8 = 1;
const SHAPE_BITS: u8 = 2;
const NARROW_BITS: u8 = 1;

/// Encodes one record as a lone struct — shape 0, which is what Go's
/// `Marshal(&value)` produces for a single struct.
///
/// ```
/// use colbin::{Kind, Record, Schema, Value};
///
/// let schema = Schema::from_ids([(1, Kind::Int32), (2, Kind::String)]).unwrap();
/// let record = Record::new().with(1, Value::Int(7)).with(2, Value::String("hi".into()));
/// let message = colbin::encode_one(&schema, &record).unwrap();
///
/// assert!(colbin::is_compact(&message));
/// assert_eq!(colbin::decode_one(&message, &schema).unwrap(), record);
/// ```
pub fn encode_one(schema: &Schema, record: &Record) -> Result<Vec<u8>, Error> {
    let mut out = Vec::new();
    write_message(
        &mut out,
        schema,
        &Plan::new(schema),
        std::slice::from_ref(record),
        SHAPE_STRUCT,
    )?;
    Ok(out)
}

/// Encodes 1 to [`MAX_RECORDS`] records as an array — shapes 1 to 3, which is
/// what Go's `Marshal(&[]T{..})` produces.
///
/// A lone struct and an array of one differ only in how a JSON reader renders
/// them; both carry one record, and a schema-driven decode does not have to care
/// which it was given.
pub fn encode(schema: &Schema, records: &[Record]) -> Result<Vec<u8>, Error> {
    if records.is_empty() || records.len() > MAX_RECORDS {
        return Err(Error::RecordCount(records.len()));
    }
    let mut out = Vec::new();
    write_message(
        &mut out,
        schema,
        &Plan::new(schema),
        records,
        records.len() as u64,
    )?;
    Ok(out)
}

/// Appends one message to `out` and returns how many bytes it occupies.
pub(crate) fn write_message(
    out: &mut Vec<u8>,
    schema: &Schema,
    plan: &Plan,
    records: &[Record],
    shape: u64,
) -> Result<usize, Error> {
    if records.is_empty() || records.len() > MAX_RECORDS {
        return Err(Error::RecordCount(records.len()));
    }
    let mut writer = Writer {
        // ALL_POSITIVE is worth one payload bit on every signed integer, and
        // deciding it needs a pass over the whole message before the first bit
        // is written -- which is why it is settled here rather than inferred.
        // The schema half of the question came from the plan; only the values
        // have to be looked at per message.
        all_positive: all_positive(schema, plan, records),
        key_bits: plan.key_bits,
        terminator: plan.terminator,
        bits: BitWriter::new(out),
    };

    // The five header bits, in the order compact/compact.go documents.
    writer.bits.put(1, MODE_BITS);
    writer.bits.put(u64::from(writer.all_positive), FLAG_BITS);
    writer.bits.put(shape, SHAPE_BITS);
    writer.bits.put(u64::from(plan.narrow), NARROW_BITS);

    for record in records {
        write_record(&mut writer, schema, record)?;
    }
    Ok(writer.bits.finish())
}

pub(crate) struct Writer<'a> {
    bits: BitWriter<'a>,
    all_positive: bool,
    key_bits: u8,
    terminator: u64,
}

impl<'a> Writer<'a> {
    /// A writer positioned after the header, for the typed path, which knows its
    /// fields at compile time and so needs the value writers without the
    /// schema-driven dispatch around them.
    pub(crate) fn headed(
        out: &'a mut Vec<u8>,
        plan: &Plan,
        all_positive: bool,
        shape: u64,
    ) -> Self {
        let mut writer = Self {
            all_positive,
            key_bits: plan.key_bits,
            terminator: plan.terminator,
            bits: BitWriter::new(out),
        };
        writer.bits.put(1, MODE_BITS);
        writer.bits.put(u64::from(all_positive), FLAG_BITS);
        writer.bits.put(shape, SHAPE_BITS);
        writer.bits.put(u64::from(plan.narrow), NARROW_BITS);
        writer
    }

    pub(crate) fn finish(self) -> usize {
        self.bits.finish()
    }

    pub(crate) fn key(&mut self, id: u8) {
        self.bits.put(u64::from(id), self.key_bits);
    }

    pub(crate) fn end(&mut self) {
        self.bits.put(self.terminator, self.key_bits);
    }

    /// A signed integer: the payload is the magnitude when ALL_POSITIVE is set
    /// and the zigzag when it is not.
    pub(crate) fn int(&mut self, value: i64) {
        let payload = if self.all_positive {
            value as u64
        } else {
            varint::zigzag(value)
        };
        varint::put_varint(&mut self.bits, payload);
    }

    pub(crate) fn uint(&mut self, value: u64) {
        varint::put_varint(&mut self.bits, value);
    }

    pub(crate) fn count(&mut self, n: usize) {
        varint::put_varint(&mut self.bits, n as u64);
    }

    pub(crate) fn str(&mut self, text: &str) {
        packed5::write_raw(&mut self.bits, text.as_bytes());
    }

    pub(crate) fn bytes_ref(&mut self, blob: &[u8]) {
        varint::put_varint(&mut self.bits, blob.len() as u64);
        self.bits.put_bytes(blob);
    }

    pub(crate) fn bit(&mut self, value: bool) {
        self.bits.put_bool(value);
    }

    pub(crate) fn f32(&mut self, value: f32) {
        self.bits.put(u64::from(value.to_bits()), 32);
    }

    pub(crate) fn f64(&mut self, value: f64) {
        self.bits.put(value.to_bits(), 64);
    }

    /// A bitmap, one bit per element: the one array form that does not match
    /// standard mode, which carries bools through an integer column.
    pub(crate) fn bools(&mut self, vals: &[bool]) {
        self.count(vals.len());
        for bit in vals {
            self.bits.put_bool(*bit);
        }
    }

    pub(crate) fn strings(&mut self, vals: &[String]) {
        self.count(vals.len());
        for text in vals {
            packed5::write_raw(&mut self.bits, text.as_bytes());
        }
    }

    pub(crate) fn f32s(&mut self, vals: &[f32]) {
        self.count(vals.len());
        for value in vals {
            self.bits.put(u64::from(value.to_bits()), 32);
        }
    }

    pub(crate) fn f64s(&mut self, vals: &[f64]) {
        self.count(vals.len());
        for value in vals {
            self.bits.put(value.to_bits(), 64);
        }
    }

    pub(crate) fn ints(&mut self, vals: &[i64], width: u8) {
        varint::put_varint(&mut self.bits, vals.len() as u64);
        if vals.is_empty() {
            // The array codec's own header is not written for an empty slice:
            // `AppendArray` on nothing still emits one, and `DecodeArray` never
            // reads it back.
            return;
        }
        varint::encode_array(&mut self.bits, vals, width);
    }
}

/// Writes one record's present fields and closes it.
///
/// A field holding its zero value is skipped, which is compact mode's whole
/// presence rule: the key run *is* the presence information. That is applied here
/// rather than left to the caller so that a record built by hand and a record
/// that came back from `decode` both encode to the same bytes Go would write.
fn write_record(writer: &mut Writer<'_>, schema: &Schema, record: &Record) -> Result<(), Error> {
    for (id, value) in record.iter() {
        let kind = schema.kind(id).ok_or(Error::UnknownField(id))?;
        if is_zero(value) {
            continue;
        }
        writer.key(id);
        write_value(writer, kind, value, id)?;
    }
    writer.end();
    Ok(())
}

/// Writes one value positionally — a field's payload, an array element, a map key
/// or a map value. Nothing is omitted here: for an element, position is the
/// identity, and a field's own omission was already decided by `write_record`.
fn write_value(writer: &mut Writer<'_>, kind: &Kind, value: &Value, id: u8) -> Result<(), Error> {
    let mismatch = || Error::ValueKind { id };
    match kind {
        Kind::Bool => writer.bit(value.as_bool().ok_or_else(mismatch)?),
        Kind::Int8 | Kind::Int16 | Kind::Int32 | Kind::Int64 => {
            writer.int(value.as_i64().ok_or_else(mismatch)?)
        }
        Kind::Uint8 | Kind::Uint16 | Kind::Uint32 | Kind::Uint64 => {
            writer.uint(value.as_u64().ok_or_else(mismatch)?)
        }
        Kind::Float32 => match value {
            Value::Float32(v) => writer.f32(*v),
            _ => return Err(mismatch()),
        },
        Kind::Float64 => match value {
            Value::Float64(v) => writer.f64(*v),
            _ => return Err(mismatch()),
        },
        Kind::String => {
            let text = value.as_str().ok_or_else(mismatch)?;
            writer.str(text);
        }
        Kind::Bytes => {
            // Borrowed straight from the value: this used to copy into a Vec, for
            // no reason beyond the borrow checker being easier to satisfy.
            match value {
                Value::Bytes(blob) => writer.bytes_ref(blob),
                _ => return Err(mismatch()),
            }
        }

        // Integer slices go to the array codec at the width the kind names, which
        // is what makes an array cost the same in either wire mode.
        Kind::Int8s | Kind::Int16s | Kind::Int32s | Kind::Int64s => {
            let (width, _) = kind.slice_int().expect("an integer slice kind");
            match value {
                Value::Ints(vals) => writer.ints(vals, width),
                _ => return Err(mismatch()),
            }
        }
        Kind::Uint16s | Kind::Uint32s | Kind::Uint64s => {
            let (width, _) = kind.slice_int().expect("an integer slice kind");
            match value {
                // An unsigned slice rides the same-width signed codec, which
                // preserves the bit pattern exactly as codec/integer.go does.
                Value::Uints(vals) => {
                    let signed: Vec<i64> = vals
                        .iter()
                        .map(|v| varint::sign_extend(*v, width))
                        .collect();
                    writer.ints(&signed, width)
                }
                _ => return Err(mismatch()),
            }
        }
        Kind::Bools => match value {
            Value::Bools(vals) => writer.bools(vals),
            _ => return Err(mismatch()),
        },
        Kind::Strings => match value {
            Value::Strings(vals) => writer.strings(vals),
            _ => return Err(mismatch()),
        },
        Kind::Float32s => match value {
            Value::Float32s(vals) => writer.f32s(vals),
            _ => return Err(mismatch()),
        },
        Kind::Float64s => match value {
            Value::Float64s(vals) => writer.f64s(vals),
            _ => return Err(mismatch()),
        },

        // Composites. A nested struct is another key run closed by the same
        // terminator; an array and a map are a count then their values.
        Kind::Struct(nested) => {
            let sub = value.as_record().ok_or_else(mismatch)?;
            write_record(writer, nested, sub)?;
        }
        Kind::Array(elem) => {
            let vals = value.as_array().ok_or_else(mismatch)?;
            writer.count(vals.len());
            for element in vals {
                write_value(writer, elem, element, id)?;
            }
        }
        Kind::Map(key_kind, val_kind) => {
            let entries = value.as_map().ok_or_else(mismatch)?;
            writer.count(entries.len());
            for (key, val) in entries {
                write_value(writer, key_kind, key, id)?;
                write_value(writer, val_kind, val, id)?;
            }
        }
    }
    Ok(())
}

/// Whether a value is the one compact mode omits. It mirrors
/// `compactWriteRecord`'s per-kind tests, including comparing floats on their
/// bits so that negative zero survives: `-0.0 == 0.0` is true, and omitting it
/// would decode back as `+0.0`.
fn is_zero(value: &Value) -> bool {
    match value {
        Value::Bool(v) => !*v,
        Value::Int(v) => *v == 0,
        Value::Uint(v) => *v == 0,
        Value::Float32(v) => v.to_bits() == 0,
        Value::Float64(v) => v.to_bits() == 0,
        Value::String(v) => v.is_empty(),
        Value::Bytes(v) => v.is_empty(),
        Value::Ints(v) => v.is_empty(),
        Value::Uints(v) => v.is_empty(),
        Value::Bools(v) => v.is_empty(),
        Value::Strings(v) => v.is_empty(),
        Value::Float32s(v) => v.is_empty(),
        Value::Float64s(v) => v.is_empty(),
        // A nested struct all of whose fields are zero is omitted entirely, the
        // same rule a zero scalar gets -- recursively, since one of those fields
        // may itself be a struct.
        Value::Record(record) => record.iter().all(|(_, value)| is_zero(value)),
        Value::Array(v) => v.is_empty(),
        Value::Map(v) => v.is_empty(),
    }
}

/// Whether every signed integer this message writes is non-negative.
///
/// It reproduces Go's answer, which is deliberately conservative: when a signed
/// integer sits inside a composite, `compactAllPositivePlan` reports false rather
/// than walking the whole value to find it. Being *more* accurate here would
/// produce a smaller message than Go's for the same value, which is exactly the
/// divergence this port exists not to have.
fn all_positive(schema: &Schema, plan: &Plan, records: &[Record]) -> bool {
    if plan.signed_reach {
        return false;
    }
    // No composite can hide one, so the top-level signed scalars are all there
    // is to scan. An absent field is a zero, which is not negative.
    for record in records {
        for (id, value) in record.iter() {
            let Some(kind) = schema.kind(id) else {
                continue;
            };
            if is_signed_scalar(kind) && value.as_i64().is_some_and(|v| v < 0) {
                return false;
            }
        }
    }
    true
}

fn is_signed_scalar(kind: &Kind) -> bool {
    matches!(kind, Kind::Int8 | Kind::Int16 | Kind::Int32 | Kind::Int64)
}

/// Whether a signed integer is reachable *through* this composite kind, which is
/// what makes Go give up on the pre-scan. An integer slice does not count: the
/// array codec has its own encoding and `ALL_POSITIVE` says nothing about it.
fn composite_reaches_signed(kind: &Kind) -> bool {
    match kind {
        Kind::Struct(schema) => schema
            .fields()
            .iter()
            .any(|field| is_signed_scalar(&field.kind) || composite_reaches_signed(&field.kind)),
        Kind::Array(elem) => composite_reaches_signed(elem),
        Kind::Map(key, val) => {
            is_signed_scalar(key)
                || is_signed_scalar(val)
                || composite_reaches_signed(key)
                || composite_reaches_signed(val)
        }
        _ => false,
    }
}
