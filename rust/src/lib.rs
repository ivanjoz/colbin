//! A Rust port of [colbin](https://github.com/ivanjoz/colbin), the columnar
//! binary format whose specification is the Go codecs in this repository.
//!
//! # What it reads
//!
//! Both wire modes, which a message discriminates on bit 0 of byte 0:
//!
//! * **compact mode** — one record, or an array of at most three, written as a
//!   run of `[key][value]` pairs in a single LSB-first bitstream. This is what a
//!   single-record message always is, and therefore what a session token, an SSE
//!   frame or any other one-off payload arrives as. Nested structs, arrays of
//!   structs, maps and nested slices are all carried, recursing through the same
//!   bitstream.
//! * **standard mode** — the columnar layout: a version byte, a record count,
//!   and one self-identifying column per field. Read **flat** only: a composite
//!   is a sub-table there, which is a separate layout and is refused rather than
//!   half-decoded.
//!
//! # What it writes
//!
//! Compact mode, through [`encode`] and [`encode_one`] — the inverse of
//! [`decode`], over the same [`Schema`] and the same [`Record`]/[`Value`] tree.
//! That is what a single record is, and a single record is what crosses a
//! Rust↔Go boundary; the columnar layout exists to amortise framing over many
//! records and Go reaches for it past three.
//!
//! It is byte for byte identical to what Go's encoder writes for the same value,
//! **except for strings**: it writes raw packed5 frames, where Go picks the
//! cheaper of raw and packed. Go picks raw for anything short, so the two agree
//! exactly far more often than that suggests — and where they do not, the message
//! costs a few bytes more and decodes identically. Every other decision is Go's:
//! the omit-zero rule, `ALL_POSITIVE`, `NARROW_KEYS`, and the integer array
//! codec's full transform search.
//!
//! # Encoding or decoding the same shape many times
//!
//! There are two handles, and which one to reach for depends on whether the
//! layout is known at compile time.
//!
//! [`TypedCodec`] with `#[derive(Colbin)]` is the fast one: the message is read
//! into and written out of the struct's own fields, with no intermediate
//! `Record` and no `Value` at all. On a seven-field record it measures 46 ns to
//! encode and 103 ns to decode — against Go's `Codec[T]` at roughly 120 and 150.
//! It needs the `derive` feature.
//!
//! ```
//! # #[cfg(feature = "derive")]
//! # fn main() -> Result<(), colbin::Error> {
//! use colbin::{Colbin, TypedCodec};
//!
//! #[derive(Colbin, Debug, PartialEq)]
//! struct Token {
//!     #[cb(1)] company_id: i32,
//!     #[cb(2)] user: String,
//! }
//!
//! let codec = TypedCodec::<Token>::new()?;
//! let mut buf = Vec::new();
//! codec.append(&mut buf, &Token { company_id: 7, user: "tester".into() })?;
//! assert_eq!(codec.decode(&buf)?.user, "tester");
//! # Ok(())
//! # }
//! # #[cfg(not(feature = "derive"))]
//! # fn main() {}
//! ```
//!
//! [`Codec`] is the dynamic one, for a layout only known at runtime: the same
//! cached schema facts and caller-owned buffers, over [`Record`] and [`Value`].
//! It is what carries the composites the derive does not, and it is what the
//! free functions would be if they cached anything.
//!
//! ```
//! use colbin::{Codec, Kind, Record, Schema, Value};
//!
//! let codec = Codec::new(Schema::from_ids([(1, Kind::Int32)])?);
//! let mut buf = Vec::new();
//! for id in 1..=3 {
//!     buf.clear();
//!     codec.append_one(&mut buf, &Record::new().with(1, Value::Int(id)))?;
//! }
//! # Ok::<(), colbin::Error>(())
//! ```
//!
//! The free functions rebuild the schema facts per call and allocate a message
//! every time, which is what they are for — a single payload.
//!
//! # What it does not do
//!
//! An `interface{}` field has no compact form at any depth — a value's concrete
//! type is a property of the value, and the wire has no tag for it — and neither
//! do the self-describing messages `MarshalJSON` writes. There is no standard-mode
//! encoder.
//!
//! `#[derive(Colbin)]` carries the flat forms: `bool`, the fixed-width integers
//! and floats, `String`, `Vec<u8>` and slices of those. A nested struct, a slice
//! of structs or a map goes through [`Codec`], which reads and writes all of
//! them — the derive would need the schema of the type one level down at the
//! point of each read, which the flat forms do not.
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
mod codec;
mod compact;
mod encode;
pub mod minimal;
mod fieldid;
mod packed5;
mod standard;
mod typed;
mod varint;

pub use codec::Codec;
pub use typed::{Colbin, FieldReader, FieldWriter, TypedCodec};

/// The derive macro for [`Colbin`], behind the `derive` feature.
#[cfg(feature = "derive")]
pub use colbin_derive::Colbin;
pub use encode::{MAX_RECORDS, encode, encode_one};
pub use minimal::{MAX_MINIMAL_FIELDS, MinimalReader, MinimalWriter};
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
    /// A schema nests deeper than [`MAX_DEPTH`], which the recursive readers
    /// refuse rather than descend.
    TooDeep(usize),
    /// A composite value appeared where the wire mode has no form for it: a
    /// nested struct, array of structs or map in a standard-mode column, which
    /// this decoder reads only in compact mode.
    CompositeUnsupported { id: u8 },
    /// A value handed to the encoder is not the form its field's kind declares —
    /// a `Value::String` for a `Kind::Int32`, say.
    ValueKind { id: u8 },
    /// A typed read was handed a field position the derived layout does not
    /// have. Only reachable through a hand-written [`Colbin`] impl.
    FieldIndex(usize),
    /// A schema declares an array of scalars, which colbin has no wire form for:
    /// a slice of scalars is its own kind (`Int32s`, `Strings`, ...) and rides a
    /// bulk codec, so `Array` is only ever a slice of composites or of `Bytes`.
    ArrayOfScalars { id: u8 },
    /// Minimal mode: an integer header carries size code 7, which is unassigned.
    MinimalSizeCode(u8),
    /// Minimal mode: a field is wider than the type the record declares for it,
    /// which is a schema disagreement rather than a value to truncate.
    MinimalFieldTooWide(u8),
    /// Minimal mode: a continued size describes more than this platform can
    /// address, or its continuation run does not end inside the message.
    MinimalSizeTooLarge,
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::BadVersion(v) => write!(f, "colbin: bad version byte 0x{v:02x}"),
            Self::MinimalSizeCode(code) => {
                write!(f, "colbin: minimal mode: unassigned integer size code {code}")
            }
            Self::MinimalFieldTooWide(key) => write!(
                f,
                "colbin: minimal mode: field {key} is wider than the type this record declares for it"
            ),
            Self::MinimalSizeTooLarge => write!(
                f,
                "colbin: minimal mode: a declared size is larger than this platform can address"
            ),
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
            Self::TooDeep(n) => write!(
                f,
                "colbin: schema nests {n} levels, past the {MAX_DEPTH} level ceiling"
            ),
            Self::CompositeUnsupported { id } => write!(
                f,
                "colbin: field {id} is a nested struct, array or map in a standard-mode column, which this decoder reads only in compact mode"
            ),
            Self::ValueKind { id } => write!(
                f,
                "colbin: the value given for field {id} is not the form its kind declares"
            ),
            Self::FieldIndex(index) => {
                write!(f, "colbin: field index {index} is past the derived layout")
            }
            Self::ArrayOfScalars { id } => write!(
                f,
                "colbin: field {id} declares an array of scalars; a slice of scalars is its own kind (Int32s, Strings, ...) and Array carries only composites or Bytes"
            ),
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
#[derive(Clone, Debug, PartialEq, Eq, Hash)]
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

    /// A nested struct: another key run, closed by the same terminator. Compact
    /// mode has no column framing to embed, so nesting reuses the only framing
    /// it has — see the `compact` package's documentation.
    Struct(Box<Schema>),
    /// A slice whose elements are not scalars: a count, then the elements. A
    /// slice *of* scalars keeps its own kind above and its bulk codec.
    Array(Box<Kind>),
    /// A map: a count, then that many key/value pairs. Entries arrive in the
    /// order the writer produced them, which for Go is its map iteration order.
    Map(Box<Kind>, Box<Kind>),
}

impl Kind {
    /// How many levels of nesting this kind introduces. A schema's own depth is
    /// one more than the deepest kind in it; [`MAX_DEPTH`] bounds it so that the
    /// recursive readers cannot be driven off the stack by a crafted schema.
    fn depth(&self) -> usize {
        match self {
            Self::Struct(schema) => schema.depth,
            Self::Array(elem) => elem.depth(),
            Self::Map(key, val) => key.depth().max(val.depth()),
            _ => 0,
        }
    }

    /// Width in bits and signedness, for the scalar integer kinds.
    pub(crate) fn scalar_int(&self) -> Option<(u8, bool)> {
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
    pub(crate) fn slice_int(&self) -> Option<(u8, bool)> {
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

    /// A nested struct, which is itself a sparse record: the nested key run
    /// names only the fields that were not zero.
    Record(Record),
    /// The elements of a non-scalar array, in wire order.
    Array(Vec<Value>),
    /// A map's entries, as pairs in the order the message carried them.
    ///
    /// A `Vec` rather than a map type on purpose. Go writes entries in its own
    /// iteration order, and a colbin map key may be a float, which is neither
    /// `Ord` nor `Hash` — so any real map type would either reorder what the
    /// wire said or refuse a message the format permits.
    Map(Vec<(Value, Value)>),
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

    /// The nested record, for a [`Kind::Struct`] field.
    pub fn as_record(&self) -> Option<&Record> {
        match self {
            Self::Record(v) => Some(v),
            _ => None,
        }
    }

    /// The elements, for a [`Kind::Array`] field.
    pub fn as_array(&self) -> Option<&[Value]> {
        match self {
            Self::Array(v) => Some(v),
            _ => None,
        }
    }

    /// The entries, for a [`Kind::Map`] field, in the order the message carried
    /// them.
    pub fn as_map(&self) -> Option<&[(Value, Value)]> {
        match self {
            Self::Map(v) => Some(v),
            _ => None,
        }
    }
}

/// One field of a schema: the wire id the message names it by, and the kind that
/// says how to read its value.
#[derive(Clone, Debug, PartialEq, Eq, Hash)]
pub struct Field {
    pub id: u8,
    pub kind: Kind,
}

/// One field of a Go struct, as the schema builder needs to see it.
///
/// `name` is the name colbin hashes: the `cb` tag's name token if the struct has
/// one, else the Go field name. `id` is the explicit id from a `cb:"5"` tag, or
/// `None` for the usual hashed field.
#[derive(Clone, Debug)]
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
#[derive(Clone, Debug, PartialEq, Eq, Hash)]
pub struct Schema {
    fields: Vec<Field>,
    /// Index into `fields`, plus one, so that zero means "no such field".
    by_id: Box<[u8; 256]>,
    /// One more than the deepest nested kind, checked against [`MAX_DEPTH`] when
    /// the schema is built. Storing it makes nesting a schema O(1) to validate
    /// instead of a walk per level, and it is what lets the recursive readers
    /// trust their own depth.
    depth: usize,
}

/// The deepest a schema may nest. A recursive reader descends once per level, so
/// this is what keeps a crafted schema from exhausting the stack. Nothing real
/// comes close: it bounds the *type*, not the data.
pub const MAX_DEPTH: usize = 32;

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
            .map(|(name, kind)| FieldSpec::hashed(name, kind.clone()))
            .collect();
        Self::from_fields(&specs)
    }

    /// The nested schema of a `Kind::Struct` field, ready to hand to
    /// [`Kind::Struct`]. It is the same builder one level down — colbin assigns
    /// ids per struct, so a nested struct's ids are hashed among its own fields
    /// and nothing about the outer layout reaches them.
    ///
    /// ```
    /// use colbin::{Kind, Schema};
    ///
    /// // struct Outer { ID int64 `cb:"1"`; Sub Inner `cb:"2"` }
    /// // struct Inner { X int32  `cb:"1"`; Y string `cb:"2"` }
    /// let inner = Schema::from_ids([(1, Kind::Int32), (2, Kind::String)]).unwrap();
    /// let outer = Schema::from_ids([
    ///     (1, Kind::Int64),
    ///     (2, Kind::Struct(Box::new(inner))),
    /// ])
    /// .unwrap();
    /// assert_eq!(outer.ids(), [1, 2]);
    /// ```
    pub fn nested(self) -> Kind {
        Kind::Struct(Box::new(self))
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
                    kind: spec.kind.clone(),
                })
                .collect(),
        )
    }

    fn build(fields: Vec<Field>) -> Result<Self, Error> {
        if fields.len() > MAX_FIELDS {
            return Err(Error::TooManyFields(fields.len()));
        }
        // One deeper than the deepest field, so a flat schema is depth 1. Checked
        // here, once, rather than by the readers on every descent.
        let depth = 1 + fields.iter().map(|f| f.kind.depth()).max().unwrap_or(0);
        if depth > MAX_DEPTH {
            return Err(Error::TooDeep(depth));
        }
        let mut by_id = Box::new([0_u8; 256]);
        for (index, field) in fields.iter().enumerate() {
            if field.id == RESERVED_FIELD_ID {
                return Err(Error::FieldIdOutOfRange(field.id));
            }
            if by_id[usize::from(field.id)] != 0 {
                return Err(Error::DuplicateFieldId(field.id));
            }
            // Kind::Array of a scalar names no Go type: a slice of scalars is its
            // own kind and rides a bulk codec, so `Array` carries only composites
            // or `Bytes`. Refused when the schema is built rather than when it is
            // used, because the encoder would otherwise write a message element
            // by element that Go's decoder would read as a bulk-coded column.
            if array_of_scalars(&field.kind).is_some() {
                return Err(Error::ArrayOfScalars { id: field.id });
            }
            // index + 1 fits a u8 because the field ceiling is 254.
            by_id[usize::from(field.id)] = (index + 1) as u8;
        }
        Ok(Self {
            fields,
            by_id,
            depth,
        })
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

    /// One more than the deepest nested kind; 1 for a schema of scalars.
    pub fn depth(&self) -> usize {
        self.depth
    }

    /// The declaration position of a wire id, or `None` if the schema has no
    /// such field. This is what the typed path dispatches on, and it is why the
    /// derive does not have to hash field names itself.
    pub(crate) fn index(&self, id: u8) -> Option<usize> {
        match self.by_id[usize::from(id)] {
            0 => None,
            index => Some(usize::from(index) - 1),
        }
    }

    /// The kind declared for a wire id, or `None` if the schema has no such
    /// field. Borrowed rather than copied: a `Kind` can now hold a whole nested
    /// schema.
    pub(crate) fn kind(&self, id: u8) -> Option<&Kind> {
        match self.by_id[usize::from(id)] {
            0 => None,
            index => Some(&self.fields[usize::from(index) - 1].kind),
        }
    }

    /// Whether every id in this schema and every schema reachable from it fits
    /// the 4-bit key. Compact mode spends the same key bits at every depth, so
    /// this is a property of the whole message: one untagged nested field widens
    /// every key in it.
    pub fn narrow_keys(&self) -> bool {
        self.fields
            .iter()
            .all(|field| field.id <= MAX_NARROW_KEY && kind_narrow_keys(&field.kind))
    }
}

/// Whether this kind is, or contains, a `Kind::Array` whose element is a *bare*
/// scalar — the one schema shape no Go type produces, because Go always sends a
/// slice of scalars to a bulk codec under its own kind.
///
/// Every other element form is real: `[][]int32` is `Array(Int8s..Float64s)`,
/// `[][]byte` is `Array(Bytes)`, `[]map[..]..` is `Array(Map(..))`, and a slice
/// of structs is `Array(Struct(..))`. A nested schema validated its own fields
/// when it was built, so only the kinds outside one are walked here.
fn array_of_scalars(kind: &Kind) -> Option<()> {
    match kind {
        Kind::Array(elem) => {
            if is_bare_scalar(elem) {
                return Some(());
            }
            array_of_scalars(elem)
        }
        Kind::Map(key, val) => array_of_scalars(key).or_else(|| array_of_scalars(val)),
        // Kind::Struct holds a Schema, which validated its own fields.
        _ => None,
    }
}

/// A single value rather than a run of them: what a slice of must use a bulk
/// kind instead of `Kind::Array`. `Bytes` is not one — `[][]byte` is real.
fn is_bare_scalar(kind: &Kind) -> bool {
    matches!(
        kind,
        Kind::Bool
            | Kind::Int8
            | Kind::Int16
            | Kind::Int32
            | Kind::Int64
            | Kind::Uint8
            | Kind::Uint16
            | Kind::Uint32
            | Kind::Uint64
            | Kind::Float32
            | Kind::Float64
            | Kind::String
    )
}

/// `Schema::narrow_keys` through a kind, which may hold schemas of its own.
fn kind_narrow_keys(kind: &Kind) -> bool {
    match kind {
        Kind::Struct(schema) => schema.narrow_keys(),
        Kind::Array(elem) => kind_narrow_keys(elem),
        Kind::Map(key, val) => kind_narrow_keys(key) && kind_narrow_keys(val),
        _ => true,
    }
}

/// The largest field id the 4-bit key can carry; 15 closes a record.
pub const MAX_NARROW_KEY: u8 = 14;

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
    /// An empty record, to be filled with [`Record::push`]. This is the input
    /// side: a caller encoding a message builds records the same way the decoder
    /// hands them back.
    pub fn new() -> Self {
        Self::default()
    }

    pub(crate) fn with_capacity(n: usize) -> Self {
        Self {
            fields: Vec::with_capacity(n),
        }
    }

    /// Appends a field. Ids are not checked for duplicates or order here — the
    /// encoder writes them in the order given, and a schema is what says which
    /// ids are legal.
    pub fn push(&mut self, id: u8, value: Value) {
        self.fields.push((id, value));
    }

    /// Empties the record, keeping its field vector so a decode into it does not
    /// allocate one again.
    pub fn clear(&mut self) {
        self.fields.clear();
    }

    /// Reserves room for `n` fields.
    pub fn reserve(&mut self, n: usize) {
        self.fields.reserve(n);
    }

    /// A record from its fields, in the order they should be written.
    pub fn from_fields(fields: impl IntoIterator<Item = (u8, Value)>) -> Self {
        Self {
            fields: fields.into_iter().collect(),
        }
    }

    /// Appends a field and returns the record, for building one in an expression.
    #[must_use]
    pub fn with(mut self, id: u8, value: Value) -> Self {
        self.push(id, value);
        self
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
