//! [`Codec`]: a per-schema handle for encoding and decoding the same shape many
//! times.
//!
//! The free [`crate::encode_one`] and [`crate::decode_one`] resolve the same
//! facts on every call and allocate a message every time. For one payload that
//! is nothing; for a request path moving thousands of one-record messages it is
//! most of the cost. Measured on a seven-field record, a 134 ns encode spent
//!
//! * **48 ns** growing a fresh `Vec` from zero, four reallocations for 17 bytes,
//! * **12 ns** in `Schema::narrow_keys`, a recursive walk of the whole schema
//!   tree whose answer cannot change between calls,
//!
//! and a 254 ns decode spent 23 ns allocating each record's field vector plus
//! another allocation for the `Vec<Record>` that [`crate::decode_one`] builds
//! only to take one record out of.
//!
//! A `Codec` removes all of that: the schema-derived facts are resolved once at
//! construction, and the buffer-shaped costs move to the caller, who can hand the
//! same `Vec` back on every call.
//!
//! ```
//! use colbin::{Codec, Kind, Record, Schema, Value};
//!
//! let codec = Codec::new(Schema::from_ids([(1, Kind::Int32), (2, Kind::String)])?);
//!
//! let mut buf = Vec::new();
//! let mut record = Record::new();
//! for id in 1..=3 {
//!     record.clear();
//!     record.push(1, Value::Int(id));
//!     record.push(2, Value::String(format!("row {id}")));
//!
//!     buf.clear();                             // reuse: no allocation per message
//!     codec.append_one(&mut buf, &record)?;
//!     assert!(colbin::is_compact(&buf));
//! }
//! # Ok::<(), colbin::Error>(())
//! ```
//!
//! It is immutable after construction and therefore `Send + Sync`: build one at
//! start-up, share it, and let each caller own its own buffer. The size hint is
//! the one piece of shared mutable state and it is a hint — a stale value costs a
//! wasted guess and nothing else, which is why it is `Relaxed`.

use std::sync::atomic::{AtomicU32, Ordering};

use crate::encode::{self, Plan};
use crate::{Error, Record, Schema, compact, standard};

/// A schema with its derived facts resolved once. See the module documentation.
#[derive(Debug)]
pub struct Codec {
    schema: Schema,
    plan: Plan,
    /// What the last message of this schema measured. Used to size a fresh buffer
    /// up front rather than grow into it a chunk at a time, which is the
    /// difference between one allocation and four.
    size_hint: AtomicU32,
}

/// The floor for a sized buffer, so a schema that has not encoded anything yet
/// still gets one allocation rather than four.
const MIN_HINT: usize = 32;

/// The ceiling on a size hint, so one huge message cannot leave a figure that
/// makes every later allocation wild. Past it a buffer grows the ordinary way,
/// and the reallocations matter less the larger the payload already is.
const MAX_HINT: usize = 1 << 20;

impl Codec {
    /// Resolves the schema's derived facts: the key width, the terminator, and
    /// whether a signed integer is reachable through a composite.
    pub fn new(schema: Schema) -> Self {
        let plan = Plan::new(&schema);
        Self {
            schema,
            plan,
            size_hint: AtomicU32::new(0),
        }
    }

    pub fn schema(&self) -> &Schema {
        &self.schema
    }

    /// Whether this schema's ids all fit the 4-bit key — resolved at
    /// construction, where [`Schema::narrow_keys`] walks the tree on every call.
    pub fn narrow_keys(&self) -> bool {
        self.plan.narrow
    }

    // --- encode --------------------------------------------------------------

    /// Appends one record as a lone struct onto `dst`.
    ///
    /// This is the allocation-free path: hand back the same buffer each time
    /// (`buf.clear()` between messages) and the encoder allocates nothing at all.
    pub fn append_one(&self, dst: &mut Vec<u8>, record: &Record) -> Result<(), Error> {
        self.write(dst, std::slice::from_ref(record), encode::SHAPE_STRUCT)
    }

    /// Appends 1 to [`crate::MAX_RECORDS`] records as an array onto `dst`.
    pub fn append(&self, dst: &mut Vec<u8>, records: &[Record]) -> Result<(), Error> {
        self.write(dst, records, records.len() as u64)
    }

    /// Encodes one record onto a fresh buffer, sized from what this schema last
    /// measured. Prefer [`Codec::append_one`] in a loop.
    pub fn encode_one(&self, record: &Record) -> Result<Vec<u8>, Error> {
        let mut out = Vec::with_capacity(self.hint());
        self.append_one(&mut out, record)?;
        Ok(out)
    }

    /// Encodes records onto a fresh buffer. Prefer [`Codec::append`] in a loop.
    pub fn encode(&self, records: &[Record]) -> Result<Vec<u8>, Error> {
        let mut out = Vec::with_capacity(self.hint() * records.len().max(1));
        self.append(&mut out, records)?;
        Ok(out)
    }

    fn write(&self, dst: &mut Vec<u8>, records: &[Record], shape: u64) -> Result<(), Error> {
        let written = encode::write_message(dst, &self.schema, &self.plan, records, shape)?;
        // One more than measured, so a message that grows by a byte still fits.
        self.size_hint
            .store((written + 1).min(MAX_HINT) as u32, Ordering::Relaxed);
        Ok(())
    }

    fn hint(&self) -> usize {
        (self.size_hint.load(Ordering::Relaxed) as usize).max(MIN_HINT)
    }

    // --- decode --------------------------------------------------------------

    /// Decodes a message that must hold exactly one record.
    ///
    /// Unlike [`crate::decode_one`] this does not build a `Vec<Record>` and take
    /// one record out of it.
    pub fn decode_one(&self, data: &[u8]) -> Result<Record, Error> {
        let mut record = Record::new();
        self.decode_one_into(data, &mut record)?;
        Ok(record)
    }

    /// Decodes a one-record message into `dst`, reusing its field vector.
    ///
    /// `dst` is cleared, not merged into: compact mode omits a zero-valued field
    /// entirely, so anything already in it would survive as a stale value.
    pub fn decode_one_into(&self, data: &[u8], dst: &mut Record) -> Result<(), Error> {
        if crate::is_compact(data) {
            return compact::decode_one_into(data, &self.schema, dst);
        }
        // Columnar, which is every message past three records. It carries its own
        // record count, so a one-record demand is checked rather than assumed.
        let mut records = standard::decode(data, &self.schema)?;
        if records.len() != 1 {
            return Err(Error::RecordCount(records.len()));
        }
        *dst = records.remove(0);
        Ok(())
    }

    /// Decodes a message of any record count.
    pub fn decode(&self, data: &[u8]) -> Result<Vec<Record>, Error> {
        let mut out = Vec::new();
        self.decode_into(data, &mut out)?;
        Ok(out)
    }

    /// Decodes into `dst`, reusing its `Record`s and their field vectors, so a
    /// loop that hoists its destination allocates for the records once rather
    /// than per message.
    ///
    /// `dst` must not be a slice the caller still needs: it is overwritten.
    pub fn decode_into(&self, data: &[u8], dst: &mut Vec<Record>) -> Result<(), Error> {
        if crate::is_compact(data) {
            return compact::decode_into(data, &self.schema, dst);
        }
        *dst = standard::decode(data, &self.schema)?;
        Ok(())
    }
}
