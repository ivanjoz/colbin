//! The typed path: [`Colbin`] and [`TypedCodec`], where a message is read into
//! and written out of a Rust struct's own fields.
//!
//! The dynamic [`crate::Codec`] hands back a [`crate::Record`] — a
//! `Vec<(u8, Value)>` with a 32-byte `Value` per field — so a decode allocates a
//! vector, moves forty bytes per field into it, and leaves the caller to match
//! on `Value` afterwards. That is the right shape when the layout is only known
//! at runtime. When it is known at compile time it is all overhead, and it was
//! the whole of the remaining gap against Go, which writes into a struct.
//!
//! `#[derive(Colbin)]` generates both halves: a `match` on the field's
//! declaration position whose arms assign to the field, and a write that tests
//! each field for zero and emits it. No `Value` exists anywhere on this path.
//!
//! ```
//! # #[cfg(feature = "derive")]
//! # fn main() -> Result<(), colbin::Error> {
//! use colbin::{Colbin, TypedCodec};
//!
//! #[derive(Colbin, Debug, PartialEq)]
//! struct Stats {
//!     #[cb(1)] quantity: i32,
//!     #[cb(2)] total: i32,
//!     #[cb(3)] note: String,
//! }
//!
//! let codec = TypedCodec::<Stats>::new()?;
//! let stats = Stats { quantity: 480, total: 145_900, note: "ok".into() };
//!
//! let mut buf = Vec::new();
//! codec.append(&mut buf, &stats)?;              // no allocation per message
//!
//! let mut out = Stats::colbin_zero();
//! codec.decode_into(&buf, &mut out)?;           // straight into the fields
//! assert_eq!(out, stats);
//! # Ok(())
//! # }
//! # #[cfg(not(feature = "derive"))]
//! # fn main() {}
//! ```
//!
//! A `TypedCodec` writes ordinary compact messages, byte for byte what the
//! dynamic `Codec` writes for the same value: both resolve the same schema and
//! drive the same bitstream, and the value writers are shared rather than
//! reimplemented.

use std::marker::PhantomData;
use std::sync::atomic::{AtomicU32, Ordering};

use crate::encode::{self, Plan, Writer};
use crate::{Error, FieldSpec, Schema, compact};

/// A struct the derive has generated a typed encode and decode for.
///
/// Implemented by `#[derive(Colbin)]`. Writing one by hand is possible and not
/// the point; the contract is that `colbin_fields` and the three dispatches
/// agree on the order of the fields, since the index they dispatch on is a
/// position in that list.
pub trait Colbin: Sized {
    /// How many encodable fields the struct has.
    const COLBIN_FIELDS: usize;

    /// The fields, in declaration order — which is what decides hashed ids, so
    /// the order is part of the wire contract and not a detail.
    fn colbin_fields() -> Vec<FieldSpec<'static>>;

    /// The schema those fields imply.
    fn colbin_schema() -> Result<Schema, Error> {
        Schema::from_fields(&Self::colbin_fields())
    }

    /// Every field at its zero value.
    ///
    /// A decode starts from this, because compact mode does not write an omitted
    /// field at all: whatever was in the destination would otherwise survive
    /// underneath the fields the message does carry.
    fn colbin_zero() -> Self;

    /// Reads the value for the field at `index` in declaration order, into the
    /// field it belongs to.
    fn colbin_read(&mut self, index: usize, r: &mut FieldReader<'_, '_>) -> Result<(), Error>;

    /// Writes every field that is not at its zero value.
    fn colbin_write(&self, w: &mut FieldWriter<'_, '_>) -> Result<(), Error>;

    /// Whether every signed integer field is non-negative, which decides the
    /// message's `ALL_POSITIVE` bit.
    ///
    /// It has to be settled before the first bit is written, which is why it is
    /// its own pass rather than something the writer notices as it goes.
    fn colbin_all_positive(&self) -> bool;
}

/// A per-type handle: [`crate::Codec`] for a derived struct.
///
/// It resolves the schema once — including the key width, which is a recursive
/// walk — and carries the same size hint, so a fresh buffer is one allocation
/// rather than four. Immutable after construction, so it is `Send + Sync`:
/// build one at start-up, share it, and let each caller own its buffer.
#[derive(Debug)]
pub struct TypedCodec<T> {
    schema: Schema,
    plan: Plan,
    /// The wire id of each field, by declaration index. This is all the generated
    /// code needs from the schema, and looking it up here is why the derive does
    /// not have to hash names itself.
    ids: Vec<u8>,
    size_hint: AtomicU32,
    marker: PhantomData<fn() -> T>,
}

/// The floor for a sized buffer, so a type that has not encoded anything yet
/// still gets one allocation rather than four.
const MIN_HINT: usize = 32;
/// The ceiling on a size hint, so one huge message cannot leave a figure that
/// makes every later allocation wild.
const MAX_HINT: usize = 1 << 20;

impl<T: Colbin> TypedCodec<T> {
    /// Builds the handle from `T`'s derived layout.
    pub fn new() -> Result<Self, Error> {
        let schema = T::colbin_schema()?;
        let plan = Plan::new(&schema);
        Ok(Self {
            ids: schema.ids(),
            schema,
            plan,
            size_hint: AtomicU32::new(0),
            marker: PhantomData,
        })
    }

    pub fn schema(&self) -> &Schema {
        &self.schema
    }

    /// Whether this type's ids all fit the 4-bit key.
    pub fn narrow_keys(&self) -> bool {
        self.plan.narrow
    }

    // --- encode --------------------------------------------------------------

    /// Appends one record as a lone struct onto `dst`.
    ///
    /// The allocation-free path: hand back the same buffer each time, cleared,
    /// and nothing is allocated at all.
    pub fn append(&self, dst: &mut Vec<u8>, value: &T) -> Result<(), Error> {
        // A derived struct is flat, so `signed_reach` is false and the flag is
        // decided by the values -- which the generated pass answers without
        // building anything.
        let all_positive = !self.plan.signed_reach && value.colbin_all_positive();
        let mut field = FieldWriter {
            writer: Writer::headed(dst, &self.plan, all_positive, encode::SHAPE_STRUCT),
            ids: &self.ids,
        };
        value.colbin_write(&mut field)?;
        field.writer.end();
        let written = field.writer.finish();
        self.size_hint
            .store((written + 1).min(MAX_HINT) as u32, Ordering::Relaxed);
        Ok(())
    }

    /// Encodes onto a fresh buffer, sized from what this type last measured.
    /// Prefer [`TypedCodec::append`] in a loop.
    pub fn encode(&self, value: &T) -> Result<Vec<u8>, Error> {
        let mut out = Vec::with_capacity(self.hint());
        self.append(&mut out, value)?;
        Ok(out)
    }

    fn hint(&self) -> usize {
        (self.size_hint.load(Ordering::Relaxed) as usize).max(MIN_HINT)
    }

    // --- decode --------------------------------------------------------------

    /// Decodes a one-record message into `value`, field by field.
    ///
    /// `value` is reset to its zero state first: compact mode omits a zero-valued
    /// field, so anything already there would survive as a stale value.
    pub fn decode_into(&self, data: &[u8], value: &mut T) -> Result<(), Error> {
        compact::decode_one_typed(data, &self.schema, value)
    }

    /// Decodes a one-record message into a fresh `T`.
    pub fn decode(&self, data: &[u8]) -> Result<T, Error> {
        let mut value = T::colbin_zero();
        self.decode_into(data, &mut value)?;
        Ok(value)
    }
}

/// Reads one field's value at the type the generated code knows it to be.
///
/// Every method is the read the dynamic path performs for the same kind, minus
/// the `Value` it would wrap the result in — the readers themselves are shared,
/// so the two paths cannot drift.
pub struct FieldReader<'a, 'r> {
    pub(crate) reader: &'r mut compact::Reader<'a>,
}

impl FieldReader<'_, '_> {
    pub fn bool(&mut self) -> bool {
        self.reader.bit()
    }

    // A signed value is truncated to the field's width, which never reaches the
    // wire, exactly as Go's `*(*int8)(q) = int8(r.Int())` does.
    pub fn i8(&mut self) -> i8 {
        self.reader.int() as i8
    }
    pub fn i16(&mut self) -> i16 {
        self.reader.int() as i16
    }
    pub fn i32(&mut self) -> i32 {
        self.reader.int() as i32
    }
    pub fn i64(&mut self) -> i64 {
        self.reader.int()
    }

    pub fn u8(&mut self) -> u8 {
        self.reader.uint() as u8
    }
    pub fn u16(&mut self) -> u16 {
        self.reader.uint() as u16
    }
    pub fn u32(&mut self) -> u32 {
        self.reader.uint() as u32
    }
    pub fn u64(&mut self) -> u64 {
        self.reader.uint()
    }

    pub fn f32(&mut self) -> f32 {
        self.reader.f32()
    }
    pub fn f64(&mut self) -> f64 {
        self.reader.f64()
    }

    pub fn string(&mut self) -> Result<String, Error> {
        self.reader.string()
    }
    pub fn bytes(&mut self) -> Result<Vec<u8>, Error> {
        self.reader.bytes()
    }

    pub fn i8s(&mut self) -> Result<Vec<i8>, Error> {
        Ok(cast(self.reader.ints(8)?, |v| v as i8))
    }
    pub fn i16s(&mut self) -> Result<Vec<i16>, Error> {
        Ok(cast(self.reader.ints(16)?, |v| v as i16))
    }
    pub fn i32s(&mut self) -> Result<Vec<i32>, Error> {
        Ok(cast(self.reader.ints(32)?, |v| v as i32))
    }
    pub fn i64s(&mut self) -> Result<Vec<i64>, Error> {
        self.reader.ints(64)
    }
    // An unsigned slice rode the same-width signed codec, which preserved the
    // bit pattern; the cast is what hands it back unsigned.
    pub fn u16s(&mut self) -> Result<Vec<u16>, Error> {
        Ok(cast(self.reader.ints(16)?, |v| v as u16))
    }
    pub fn u32s(&mut self) -> Result<Vec<u32>, Error> {
        Ok(cast(self.reader.ints(32)?, |v| v as u32))
    }
    pub fn u64s(&mut self) -> Result<Vec<u64>, Error> {
        Ok(cast(self.reader.ints(64)?, |v| v as u64))
    }

    pub fn bools(&mut self) -> Result<Vec<bool>, Error> {
        self.reader.bools()
    }
    pub fn strings(&mut self) -> Result<Vec<String>, Error> {
        self.reader.strings()
    }
    pub fn f32s(&mut self) -> Result<Vec<f32>, Error> {
        self.reader.f32s()
    }
    pub fn f64s(&mut self) -> Result<Vec<f64>, Error> {
        self.reader.f64s()
    }
}

fn cast<T>(values: Vec<i64>, to: impl Fn(i64) -> T) -> Vec<T> {
    values.into_iter().map(to).collect()
}

/// Writes one field as `[key][value]`, at the type the generated code knows.
///
/// The key is looked up by declaration index, which is the only thing this adds
/// over the writer the dynamic path uses.
pub struct FieldWriter<'a, 'w> {
    pub(crate) writer: Writer<'a>,
    /// Wire ids by declaration index.
    pub(crate) ids: &'w [u8],
}

impl FieldWriter<'_, '_> {
    fn key(&mut self, index: usize) {
        // The index comes from the generated code, which counted the same fields
        // the ids were built from, so it is always in range. Falling back to the
        // reserved id rather than panicking keeps a hand-written impl from
        // taking the process down.
        self.writer.key(self.ids.get(index).copied().unwrap_or(255));
    }

    pub fn bool(&mut self, index: usize, value: bool) {
        self.key(index);
        self.writer.bit(value);
    }

    pub fn i8(&mut self, index: usize, value: i8) {
        self.int(index, i64::from(value));
    }
    pub fn i16(&mut self, index: usize, value: i16) {
        self.int(index, i64::from(value));
    }
    pub fn i32(&mut self, index: usize, value: i32) {
        self.int(index, i64::from(value));
    }
    pub fn i64(&mut self, index: usize, value: i64) {
        self.int(index, value);
    }

    fn int(&mut self, index: usize, value: i64) {
        self.key(index);
        self.writer.int(value);
    }

    pub fn u8(&mut self, index: usize, value: u8) {
        self.uint(index, u64::from(value));
    }
    pub fn u16(&mut self, index: usize, value: u16) {
        self.uint(index, u64::from(value));
    }
    pub fn u32(&mut self, index: usize, value: u32) {
        self.uint(index, u64::from(value));
    }
    pub fn u64(&mut self, index: usize, value: u64) {
        self.uint(index, value);
    }

    fn uint(&mut self, index: usize, value: u64) {
        self.key(index);
        self.writer.uint(value);
    }

    pub fn f32(&mut self, index: usize, value: f32) {
        self.key(index);
        self.writer.f32(value);
    }
    pub fn f64(&mut self, index: usize, value: f64) {
        self.key(index);
        self.writer.f64(value);
    }

    pub fn str(&mut self, index: usize, value: &str) {
        self.key(index);
        self.writer.str(value);
    }
    pub fn bytes(&mut self, index: usize, value: &[u8]) {
        self.key(index);
        self.writer.bytes_ref(value);
    }

    // Every integer slice is widened to the `i64` the array codec takes. The
    // codec derives its element width from the declared width rather than from
    // these values, so an unsigned slice cast through `as` keeps its bit pattern
    // -- which is exactly what codec/integer.go does for a column.
    pub fn i8s(&mut self, index: usize, values: &[i8]) {
        self.ints(index, values.iter().map(|v| i64::from(*v)), 8);
    }
    pub fn i16s(&mut self, index: usize, values: &[i16]) {
        self.ints(index, values.iter().map(|v| i64::from(*v)), 16);
    }
    pub fn i32s(&mut self, index: usize, values: &[i32]) {
        self.ints(index, values.iter().map(|v| i64::from(*v)), 32);
    }
    pub fn i64s(&mut self, index: usize, values: &[i64]) {
        self.key(index);
        self.writer.ints(values, 64);
    }
    // An unsigned slice is *reinterpreted* at its own width, not widened: Go
    // hands the same bytes to the signed codec (`PutInts` over `(*int32)(data)`),
    // so 4_000_000_000 as a u32 goes out as the i32 with that bit pattern. The
    // narrowing cast before the widening one is what preserves it -- widening
    // straight to i64 changes the value the codec sees and the message with it.
    pub fn u16s(&mut self, index: usize, values: &[u16]) {
        self.ints(index, values.iter().map(|v| i64::from(*v as i16)), 16);
    }
    pub fn u32s(&mut self, index: usize, values: &[u32]) {
        self.ints(index, values.iter().map(|v| i64::from(*v as i32)), 32);
    }
    pub fn u64s(&mut self, index: usize, values: &[u64]) {
        self.ints(index, values.iter().map(|v| *v as i64), 64);
    }

    fn ints(&mut self, index: usize, values: impl Iterator<Item = i64>, width: u8) {
        self.key(index);
        let widened: Vec<i64> = values.collect();
        self.writer.ints(&widened, width);
    }

    pub fn bools(&mut self, index: usize, values: &[bool]) {
        self.key(index);
        self.writer.bools(values);
    }
    pub fn strings(&mut self, index: usize, values: &[String]) {
        self.key(index);
        self.writer.strings(values);
    }
    pub fn f32s(&mut self, index: usize, values: &[f32]) {
        self.key(index);
        self.writer.f32s(values);
    }
    pub fn f64s(&mut self, index: usize, values: &[f64]) {
        self.key(index);
        self.writer.f64s(values);
    }
}
