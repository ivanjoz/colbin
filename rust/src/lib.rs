//! A decoder for [colbin](https://github.com/ivanjoz/colbin), the columnar
//! binary format whose specification is the Go codecs in this repository.
//!
//! # What it reads
//!
//! Both wire modes, which a message discriminates on bit 0 of byte 0:
//!
//! * **compact mode** — one record, or an array of at most three, written as a
//!   run of `[key][value]` pairs in a single LSB-first bitstream. This is what a
//!   single-record message always is, and therefore what a session token, an SSE
//!   frame or any other one-off payload arrives as.
//! * **standard mode** — the columnar layout: a version byte, a record count,
//!   and one self-identifying column per field.
//!
//! # What it does not read
//!
//! There is no encoder here: nothing in Rust writes colbin, and an unused
//! encoder would be a second copy of the specification to keep honest for free.
//!
//! The value kinds are the ones compact mode itself can carry — scalars,
//! strings, byte blobs and slices of primitives. Nested structs, maps and
//! `interface{}` columns are standard-mode-only shapes and are rejected rather
//! than half-decoded, as are the self-describing messages `MarshalJSON` writes.
//!
//! # The schema
//!
//! Neither mode is self-describing, so the caller supplies the field layout —
//! the same contract the Go and AssemblyScript ports have. [`Schema::from_go`]
//! builds it from the Go struct's field names in declaration order, which is
//! what decides the wire ids; [`Schema::from_ids`] takes ids that are already
//! known.
//!
//! ```
//! use colbin::{Kind, Schema};
//!
//! // struct UsuarioToken { CompanyID, ID, Created int32; Hash uint64; User string }
//! let schema = Schema::from_go(&[
//!     ("CompanyID", Kind::Int32),
//!     ("ID", Kind::Int32),
//!     ("Created", Kind::Int32),
//!     ("Hash", Kind::Uint64),
//!     ("User", Kind::String),
//! ])
//! .unwrap();
//! assert_eq!(schema.ids(), [202, 53, 159, 26, 106]);
//! ```
//!
//! A field the Go struct tagged `cb:"5"` keeps that id instead of a hashed one;
//! see [`Schema::from_fields`].

#![forbid(unsafe_code)]

mod bitstream;
mod compact;
mod fieldid;
mod packed5;
mod standard;
mod varint;

pub use fieldid::{MAX_FIELDS, RESERVED_FIELD_ID, fnv8};

use std::fmt;

/// Everything that can go wrong reading a message or building a schema.
#[derive(Clone, Debug, PartialEq, Eq)]
#[non_exhaustive]
pub enum Error {
    /// Byte 0 is not a version this decoder knows.
    BadVersion(u8),
    /// A self-describing message from `MarshalJSON`, which carries a schema
    /// section this decoder does not read.
    SelfDescribing(u8),
    /// A read ran past the end of the message.
    Truncated,
    /// The record count is not a well-formed uvarint.
    BadRecordCount,
    /// The message holds a different number of records than the caller expected.
    RecordCount(usize),
    /// The message declares more values than the process can allocate room for.
    TooLarge(u64),
    /// The message names a field the schema does not have. It cannot be skipped:
    /// the wire carries no type tag, so there is no way to know how far to step.
    UnknownField(u8),
    /// A column's type byte disagrees with the kind the schema declared.
    ColumnType {
        id: u8,
        expected: &'static str,
        found: u8,
    },
    /// A column whose type this decoder does not carry: a nested struct, a map
    /// or an `interface{}`.
    UnsupportedColumn { id: u8, class: u8 },
    /// A string field does not hold a valid packed5 frame.
    BadString(&'static str),
    /// A decoded string is not valid UTF-8. The Go codec is byte exact and
    /// permits it; a Rust `String` cannot be.
    NotUtf8,
    /// A schema names the same field id twice.
    DuplicateFieldId(u8),
    /// A schema gives a field an id outside 0..=254.
    FieldIdOutOfRange(u8),
    /// A schema has more than [`MAX_FIELDS`] fields.
    TooManyFields(usize),
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::BadVersion(v) => write!(f, "colbin: bad version byte 0x{v:02x}"),
            Self::SelfDescribing(v) => write!(
                f,
                "colbin: message 0x{v:02x} is self-describing (MarshalJSON), which this decoder does not read"
            ),
            Self::Truncated => write!(f, "colbin: message truncated"),
            Self::BadRecordCount => write!(f, "colbin: bad record count"),
            Self::RecordCount(n) => write!(f, "colbin: message holds {n} records"),
            Self::TooLarge(n) => {
                write!(
                    f,
                    "colbin: message declares {n} values, which cannot be allocated"
                )
            }
            Self::UnknownField(id) => {
                write!(f, "colbin: unknown field id {id} (schema mismatch)")
            }
            Self::ColumnType {
                id,
                expected,
                found,
            } => write!(
                f,
                "colbin: field {id} is declared {expected} but its column type byte is 0x{found:02x}"
            ),
            Self::UnsupportedColumn { id, class } => write!(
                f,
                "colbin: field {id} is a type class {class} column (nested struct, map or any), which this decoder does not read"
            ),
            Self::BadString(why) => write!(f, "colbin: bad packed5 string: {why}"),
            Self::NotUtf8 => write!(f, "colbin: string is not valid UTF-8"),
            Self::DuplicateFieldId(id) => write!(f, "colbin: duplicate field id {id}"),
            Self::FieldIdOutOfRange(id) => {
                write!(f, "colbin: field id {id} out of range 0..=254")
            }
            Self::TooManyFields(n) => {
                write!(
                    f,
                    "colbin: {n} fields exceeds the {MAX_FIELDS} field ceiling"
                )
            }
        }
    }
}

impl std::error::Error for Error {}

/// The value forms a field can hold.
///
/// These are the Go types compact mode admits. The integer kinds name a width
/// and a signedness because neither reaches the wire: the width decides where a
/// column ends, and the signedness decides whether a compact integer was
/// zigzagged. `Uint8s` is absent because `[]uint8` is Go's `[]byte`, which is
/// [`Kind::Bytes`]; `int` and `uint` are [`Kind::Int64`] and [`Kind::Uint64`],
/// which is the width Go gives them on the wire.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub enum Kind {
    Bool,
    Int8,
    Int16,
    Int32,
    Int64,
    Uint8,
    Uint16,
    Uint32,
    Uint64,
    Float32,
    Float64,
    String,
    Bytes,
    Int8s,
    Int16s,
    Int32s,
    Int64s,
    Uint16s,
    Uint32s,
    Uint64s,
    Bools,
    Strings,
    Float32s,
    Float64s,
}

impl Kind {
    /// Width in bits and signedness, for the scalar integer kinds.
    pub(crate) fn scalar_int(self) -> Option<(u8, bool)> {
        match self {
            Self::Int8 => Some((8, true)),
            Self::Int16 => Some((16, true)),
            Self::Int32 => Some((32, true)),
            Self::Int64 => Some((64, true)),
            Self::Uint8 => Some((8, false)),
            Self::Uint16 => Some((16, false)),
            Self::Uint32 => Some((32, false)),
            Self::Uint64 => Some((64, false)),
            _ => None,
        }
    }

    /// Element width in bits and signedness, for the integer slice kinds.
    ///
    /// An unsigned slice rides the signed codec of the same width, which
    /// preserves the bit pattern; the signedness only decides how the values are
    /// handed back.
    pub(crate) fn slice_int(self) -> Option<(u8, bool)> {
        match self {
            Self::Int8s => Some((8, true)),
            Self::Int16s => Some((16, true)),
            Self::Int32s => Some((32, true)),
            Self::Int64s => Some((64, true)),
            Self::Uint16s => Some((16, false)),
            Self::Uint32s => Some((32, false)),
            Self::Uint64s => Some((64, false)),
            _ => None,
        }
    }
}

/// One decoded value.
#[derive(Clone, Debug, PartialEq)]
pub enum Value {
    Bool(bool),
    Int(i64),
    Uint(u64),
    Float32(f32),
    Float64(f64),
    String(String),
    Bytes(Vec<u8>),
    Ints(Vec<i64>),
    Uints(Vec<u64>),
    Bools(Vec<bool>),
    Strings(Vec<String>),
    Float32s(Vec<f32>),
    Float64s(Vec<f64>),
}

impl Value {
    /// The value as a signed integer, for the integer and bool kinds.
    pub fn as_i64(&self) -> Option<i64> {
        match self {
            Self::Int(v) => Some(*v),
            Self::Uint(v) => Some(*v as i64),
            Self::Bool(v) => Some(i64::from(*v)),
            _ => None,
        }
    }

    /// The value as an unsigned integer, for the integer and bool kinds.
    pub fn as_u64(&self) -> Option<u64> {
        match self {
            Self::Uint(v) => Some(*v),
            Self::Int(v) => Some(*v as u64),
            Self::Bool(v) => Some(u64::from(*v)),
            _ => None,
        }
    }

    pub fn as_f64(&self) -> Option<f64> {
        match self {
            Self::Float64(v) => Some(*v),
            Self::Float32(v) => Some(f64::from(*v)),
            _ => None,
        }
    }

    pub fn as_bool(&self) -> Option<bool> {
        match self {
            Self::Bool(v) => Some(*v),
            _ => None,
        }
    }

    pub fn as_str(&self) -> Option<&str> {
        match self {
            Self::String(v) => Some(v),
            _ => None,
        }
    }

    pub fn as_bytes(&self) -> Option<&[u8]> {
        match self {
            Self::Bytes(v) => Some(v),
            _ => None,
        }
    }
}

/// One field of a schema: the wire id the message names it by, and the kind that
/// says how to read its value.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Field {
    pub id: u8,
    pub kind: Kind,
}

/// One field of a Go struct, as the schema builder needs to see it.
///
/// `name` is the name colbin hashes: the `cb` tag's name token if the struct has
/// one, else the Go field name. `id` is the explicit id from a `cb:"5"` tag, or
/// `None` for the usual hashed field.
#[derive(Clone, Copy, Debug)]
pub struct FieldSpec<'a> {
    pub name: &'a str,
    pub id: Option<u8>,
    pub kind: Kind,
}

impl<'a> FieldSpec<'a> {
    /// A field whose id colbin derives by hashing its name.
    pub fn hashed(name: &'a str, kind: Kind) -> Self {
        Self {
            name,
            id: None,
            kind,
        }
    }

    /// A field the Go struct pinned to an explicit id with `cb:"N"`.
    pub fn tagged(name: &'a str, id: u8, kind: Kind) -> Self {
        Self {
            name,
            id: Some(id),
            kind,
        }
    }
}

/// The field layout a message is read against.
///
/// Neither wire mode is self-describing, so this is what supplies the names'
/// worth of information the bytes leave out.
#[derive(Clone, Debug)]
pub struct Schema {
    fields: Vec<Field>,
    /// Index into `fields`, plus one, so that zero means "no such field".
    by_id: Box<[u8; 256]>,
}

impl Schema {
    /// Builds a schema from fields whose ids are already known.
    pub fn from_ids<I: IntoIterator<Item = (u8, Kind)>>(fields: I) -> Result<Self, Error> {
        let fields: Vec<Field> = fields
            .into_iter()
            .map(|(id, kind)| Field { id, kind })
            .collect();
        Self::build(fields)
    }

    /// Builds a schema from a Go struct's fields, in declaration order.
    ///
    /// Order matters: colbin assigns hashed ids by linear probing, so the field
    /// declared first wins a collision. Every field the Go struct encodes must
    /// be listed, including ones this message happens not to carry, or the
    /// probing lands elsewhere and every id after the collision is wrong.
    pub fn from_go(fields: &[(&str, Kind)]) -> Result<Self, Error> {
        let specs: Vec<FieldSpec<'_>> = fields
            .iter()
            .map(|(name, kind)| FieldSpec::hashed(name, *kind))
            .collect();
        Self::from_fields(&specs)
    }

    /// Builds a schema from a Go struct's fields, honouring explicit `cb:"N"`
    /// ids. Explicit ids are reserved before any name is hashed, exactly as the
    /// Go builder does.
    pub fn from_fields(specs: &[FieldSpec<'_>]) -> Result<Self, Error> {
        let named: Vec<(&str, Option<u8>)> =
            specs.iter().map(|spec| (spec.name, spec.id)).collect();
        let ids = fieldid::assign_ids(&named)?;
        Self::build(
            ids.into_iter()
                .zip(specs)
                .map(|(id, spec)| Field {
                    id,
                    kind: spec.kind,
                })
                .collect(),
        )
    }

    fn build(fields: Vec<Field>) -> Result<Self, Error> {
        if fields.len() > MAX_FIELDS {
            return Err(Error::TooManyFields(fields.len()));
        }
        let mut by_id = Box::new([0_u8; 256]);
        for (index, field) in fields.iter().enumerate() {
            if field.id == RESERVED_FIELD_ID {
                return Err(Error::FieldIdOutOfRange(field.id));
            }
            if by_id[usize::from(field.id)] != 0 {
                return Err(Error::DuplicateFieldId(field.id));
            }
            // index + 1 fits a u8 because the field ceiling is 254.
            by_id[usize::from(field.id)] = (index + 1) as u8;
        }
        Ok(Self { fields, by_id })
    }

    /// The fields, in the order they were declared.
    pub fn fields(&self) -> &[Field] {
        &self.fields
    }

    /// The wire ids, in declaration order. Useful to pin the hash against the
    /// ids the Go side actually assigned.
    pub fn ids(&self) -> Vec<u8> {
        self.fields.iter().map(|field| field.id).collect()
    }

    /// The kind declared for a wire id, or `None` if the schema has no such field.
    pub(crate) fn kind(&self, id: u8) -> Option<Kind> {
        match self.by_id[usize::from(id)] {
            0 => None,
            index => Some(self.fields[usize::from(index) - 1].kind),
        }
    }
}

/// One decoded record: the fields the message actually carried, in wire order.
///
/// Compact mode omits a field holding its zero value — the key run is the
/// presence information — so a record is sparse by design, and a field that is
/// absent is one that held Go's zero value. The typed readers return that zero
/// value rather than an option, which is the same answer the Go decoder writes
/// into the destination struct.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct Record {
    fields: Vec<(u8, Value)>,
}

impl Record {
    pub(crate) fn with_capacity(n: usize) -> Self {
        Self {
            fields: Vec::with_capacity(n),
        }
    }

    pub(crate) fn push(&mut self, id: u8, value: Value) {
        self.fields.push((id, value));
    }

    /// The value the message carried for `id`, or `None` if it carried none.
    pub fn get(&self, id: u8) -> Option<&Value> {
        self.fields
            .iter()
            .find(|(field, _)| *field == id)
            .map(|(_, value)| value)
    }

    /// Removes and returns the value for `id`, so a caller can move a `String`
    /// or a `Vec` out of the record rather than clone it.
    pub fn remove(&mut self, id: u8) -> Option<Value> {
        let at = self.fields.iter().position(|(field, _)| *field == id)?;
        Some(self.fields.remove(at).1)
    }

    /// The fields the message carried, in the order it carried them.
    pub fn iter(&self) -> impl Iterator<Item = (u8, &Value)> {
        self.fields.iter().map(|(id, value)| (*id, value))
    }

    pub fn len(&self) -> usize {
        self.fields.len()
    }

    pub fn is_empty(&self) -> bool {
        self.fields.is_empty()
    }

    /// The field as a signed integer, or 0 if the message omitted it.
    pub fn i64(&self, id: u8) -> i64 {
        self.get(id).and_then(Value::as_i64).unwrap_or(0)
    }

    /// The field as an unsigned integer, or 0 if the message omitted it.
    pub fn u64(&self, id: u8) -> u64 {
        self.get(id).and_then(Value::as_u64).unwrap_or(0)
    }

    /// The field as a float, or 0.0 if the message omitted it.
    pub fn f64(&self, id: u8) -> f64 {
        self.get(id).and_then(Value::as_f64).unwrap_or(0.0)
    }

    /// The field as a bool, or false if the message omitted it.
    pub fn bool(&self, id: u8) -> bool {
        self.get(id).and_then(Value::as_bool).unwrap_or(false)
    }

    /// The field as a string, or "" if the message omitted it.
    pub fn str(&self, id: u8) -> &str {
        self.get(id).and_then(Value::as_str).unwrap_or("")
    }
}

/// A vector sized from a count the message declared, which a corrupt message can
/// name far past what the buffer could hold. `try_reserve` turns that into an
/// error rather than an abort, and the payload that follows is what actually
/// bounds it.
pub(crate) fn try_vec<T>(count: usize) -> Result<Vec<T>, Error> {
    let mut out = Vec::new();
    out.try_reserve_exact(count)
        .map_err(|_| Error::TooLarge(count as u64))?;
    Ok(out)
}

/// Reports whether `data` holds a compact-mode message, which is the whole of
/// the mode discrimination: bit 0 of byte 0.
pub fn is_compact(data: &[u8]) -> bool {
    matches!(data.first(), Some(byte) if byte & 1 == 1)
}

/// Decodes a message into its records, reading it against `schema`.
///
/// Both wire modes are accepted; which one a message uses is its own business
/// and does not change what comes back. Standard mode's *value* layout — what
/// `Marshal` writes for a top-level map, scalar or `[]*T` — carries no marker
/// distinguishing it from the records layout, so it is not readable here; a
/// struct or a slice of structs, which is everything a schema describes, always
/// uses the records layout.
pub fn decode(data: &[u8], schema: &Schema) -> Result<Vec<Record>, Error> {
    if is_compact(data) {
        compact::decode(data, schema)
    } else {
        standard::decode(data, schema)
    }
}

/// Decodes a message that must hold exactly one record, which is what a single
/// struct is encoded as.
pub fn decode_one(data: &[u8], schema: &Schema) -> Result<Record, Error> {
    let mut records = decode(data, schema)?;
    if records.len() != 1 {
        return Err(Error::RecordCount(records.len()));
    }
    Ok(records.remove(0))
}
