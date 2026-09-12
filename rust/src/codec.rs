//! The bridge from a Rust type to the format: the [`Colbin`] trait, the root
//! descriptor, the field-id assignment, and the composite helpers that
//! `#[derive(Colbin)]` generates calls to.
//!
//! # A message is one value
//!
//! ```text
//! [root descriptor:1] [key run]
//! ```
//!
//! There is no header. Byte 0 is an ordinary descriptor naming the root's class
//! — always a struct here — and the key width used inside it. Everything the old
//! format's version bytes carried is either in that byte or gone: the mode bit,
//! because there is one format; the shape bits, because the class says it; the
//! omit-empty flag, because omission is unconditional.
//!
//! # Choosing the key width
//!
//! Narrow keys are the default and the fast path. A type goes wide when it has
//! to: a field id above fifteen, which four key bits cannot carry, or an id
//! derived from a field name, which lands anywhere in 0..=255. Numbering the
//! fields is therefore also how a type asks for the narrow width.
//!
//! # Why the generated code is straight-line
//!
//! Go reaches the wire through a plan resolved per type and a switch per field,
//! which costs 18 ns to encode a ten-field record where the straight-line calls
//! it stands in for cost 5.4 — the difference being the switch, the offsets
//! loaded from the plan and the pointer arithmetic, none of which the compiler
//! can fold. `codec.Generate` emits the straight-line form for Go; the derive is
//! the same idea for Rust, and there is no reflective path here to fall back to.

use crate::Error;
use crate::wire::{Reader, Reader8, Writer, Writer8};

/// Root descriptors, from `BYTE_ALIGNED_PLAN.md` §2.1. Both are even and above
/// 0x90, which is what no message the old format wrote could begin with — so an
/// old reader rejects a new message rather than misparsing it.
pub const ROOT_STRUCT_NARROW: u8 = 0xD0;
/// The root descriptor of a message whose top-level key run is wide-keyed.
pub const ROOT_STRUCT_WIDE: u8 = 0xD8;

/// Where a table overtakes a list of structs.
///
/// Measured on a four-field row it crosses at three; this is deliberately above
/// that, because the table also costs a transposition pass and a scratch
/// buffer, and those are worth paying only once there are rows enough to
/// amortise them. It is a property of the *data*, so it is decided here at
/// encode time and never by the type — and both shapes are self-describing, so
/// the reader dispatches on what it finds.
pub const TABLE_THRESHOLD: usize = 8;

/// One column of a transposable type: the wire key of the field and whether it
/// is a string column rather than an integer one.
#[derive(Clone, Copy, Debug)]
pub struct ColumnSpec {
    /// The field's wire key, written once for the whole column.
    pub key: u8,
    /// What the column holds, which decides the codec it goes through.
    pub kind: ColumnKind,
}

/// What a column carries. Everything but a string travels as an `i64` — a bool
/// as 0 or 1, a float as its bit pattern — because that is what the blocked
/// column codec takes.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ColumnKind {
    /// An integer, bool or float column, through the blocked column codec.
    Int,
    /// A string column, which is a list of blobs under the column's key.
    Str,
}

/// A type the format carries: one struct, encoded as a key run.
///
/// `#[derive(Colbin)]` implements it. A hand-written impl is possible and is
/// what the derive's output looks like, but the ids and the key width have to
/// agree with the other side's, so deriving is the way to keep them honest.
pub trait Colbin: Sized {
    /// The key width of this type's own run: eight bits when any id is above
    /// fifteen or derived from a name, four otherwise.
    const WIDE_KEYS: bool;

    /// The type's columns, in the order a table writes them, or empty when the
    /// type holds something the column codec does not carry and so can never be
    /// transposed.
    const COLUMNS: &'static [ColumnSpec];

    /// A value with every field at its zero, which is what a decode starts from:
    /// an omitted key means the field was zero, so the destination is cleared
    /// rather than left holding whatever it had.
    fn colbin_zero() -> Self;

    /// Writes this value's key run — the fields and nothing else — onto `buf`.
    ///
    /// It takes the buffer rather than a writer because the two key widths are
    /// different types and a nested run may be either: both writers are a buffer
    /// and nothing else, so handing it over is a reborrow rather than a copy.
    fn colbin_write_run(&self, buf: &mut Vec<u8>);

    /// Reads a key run at the width the *wire* declares, which is not
    /// necessarily this type's own: a peer may have written the other one, and
    /// the descriptor that opened the run is what says.
    fn colbin_read_run(body: &[u8], wide_keys: bool) -> Result<Self, Error>;

    /// Reads one column of this value, widened to `i64`. Only called for a
    /// [`ColumnKind::Int`] column of a transposable type.
    fn colbin_column_i64(&self, _column: usize) -> i64 {
        0
    }

    /// Reads one string column of this value.
    fn colbin_column_str(&self, _column: usize) -> &str {
        ""
    }

    /// Writes one decoded column back into this value.
    fn colbin_set_column_i64(&mut self, _column: usize, _value: i64) {}

    /// Writes one decoded string column back into this value.
    fn colbin_set_column_str(&mut self, _column: usize, _value: String) {}

    /// Encodes the value onto `dst`, which may already hold other bytes.
    fn append_encoded(&self, dst: &mut Vec<u8>) {
        dst.push(if Self::WIDE_KEYS {
            ROOT_STRUCT_WIDE
        } else {
            ROOT_STRUCT_NARROW
        });
        self.colbin_write_run(dst);
    }

    /// Encodes the value onto a fresh buffer.
    fn encode(&self) -> Vec<u8> {
        let mut out = Vec::new();
        self.append_encoded(&mut out);
        out
    }

    /// Decodes a message, refusing a byte 0 that is not a root descriptor this
    /// version writes.
    fn decode(message: &[u8]) -> Result<Self, Error> {
        let Some((root, body)) = message.split_first() else {
            return Err(Error::Truncated);
        };
        match *root {
            ROOT_STRUCT_NARROW => Self::colbin_read_run(body, false),
            ROOT_STRUCT_WIDE => Self::colbin_read_run(body, true),
            other => Err(Error::BadRoot(other)),
        }
    }
}

/// FNV-1a 32-bit over `name`, xor-folded down to a byte — the hash a field
/// without an explicit id takes.
///
/// The fold is not the low byte: the low byte of FNV-1a moves with only the last
/// character or two, so a struct of similarly named fields would collide on
/// nearly every one of them.
pub const fn fnv8(name: &str) -> u8 {
    const OFFSET32: u32 = 2_166_136_261;
    const PRIME32: u32 = 16_777_619;

    let bytes = name.as_bytes();
    let mut hash = OFFSET32;
    let mut index = 0;
    while index < bytes.len() {
        hash ^= bytes[index] as u32;
        hash = hash.wrapping_mul(PRIME32);
        index += 1;
    }
    (hash ^ (hash >> 8) ^ (hash >> 16) ^ (hash >> 24)) as u8
}

/// Assigns every field its wire key: the declared ones first, so that the
/// derived ones can probe past them.
///
/// The order matters and is the contract, and it is the same one Go's
/// `assignKeys` follows. A field that declares 5 gets 5 whatever else is in the
/// type; a field that declares nothing takes [`fnv8`] of its name and then the
/// next free slot upward. Doing it the other way round would let a hash squat on
/// a number somebody had asked for.
///
/// `declared` holds the explicit id or a negative number for "derive it". It is
/// a `const fn` so that the ids are constants in the generated code, and so that
/// there is one implementation of the rule rather than one per caller.
pub const fn assign_ids<const N: usize>(names: &[&str; N], declared: &[i16; N]) -> [u8; N] {
    let mut taken = [false; 256];
    let mut ids = [0_u8; N];

    let mut index = 0;
    while index < N {
        if declared[index] >= 0 {
            let key = declared[index] as u8;
            taken[key as usize] = true;
            ids[index] = key;
        }
        index += 1;
    }
    index = 0;
    while index < N {
        if declared[index] < 0 {
            let mut id = fnv8(names[index]);
            // Wrapping terminates because a type is refused above 256 fields.
            while taken[id as usize] {
                id = id.wrapping_add(1);
            }
            taken[id as usize] = true;
            ids[index] = id;
        }
        index += 1;
    }
    ids
}

// --- composites ---------------------------------------------------------------
//
// The generated code calls these rather than inlining the recursion, because
// what they do is the same for every type and the part that differs — the
// fields — is what the generated `colbin_write_run` already is.

/// Writes a nested struct under `key`, at whichever key width the child needs.
/// The width is a property of the scope, so a narrow run may hold a wide one and
/// the other way round.
pub fn write_struct<T: Colbin>(w: &mut Writer<'_>, key: u8, value: &T) {
    let mark = if T::WIDE_KEYS {
        w.open_struct_wide(key)
    } else {
        w.open_struct(key)
    };
    value.colbin_write_run(w.buf);
    w.close(mark);
}

/// [`write_struct`] for a wide-keyed parent.
pub fn write_struct8<T: Colbin>(w: &mut Writer8<'_>, key: u8, value: &T) {
    let mark = if T::WIDE_KEYS {
        w.open_struct_wide(key)
    } else {
        w.open_struct(key)
    };
    value.colbin_write_run(w.buf);
    w.close(mark);
}

/// Reads a nested struct.
pub fn read_struct<T: Colbin>(r: &mut Reader<'_>) -> T {
    let Some((body, wide_keys)) = r.struct_body() else {
        return T::colbin_zero();
    };
    match T::colbin_read_run(body, wide_keys) {
        Ok(value) => value,
        Err(err) => {
            r.fail(err);
            T::colbin_zero()
        }
    }
}

/// [`read_struct`] for a wide-keyed parent.
pub fn read_struct8<T: Colbin>(r: &mut Reader8<'_>) -> T {
    let Some((body, wide_keys)) = r.struct_body() else {
        return T::colbin_zero();
    };
    match T::colbin_read_run(body, wide_keys) {
        Ok(value) => value,
        Err(err) => {
            r.fail(err);
            T::colbin_zero()
        }
    }
}

/// Writes a slice of structs under `key`, transposed past
/// [`TABLE_THRESHOLD`] rows, and nothing at all when it is empty — an absent key
/// means an empty slice, exactly as it means a zero scalar.
pub fn write_structs<T: Colbin>(w: &mut Writer<'_>, key: u8, values: &[T]) {
    if values.is_empty() {
        return;
    }
    if !T::COLUMNS.is_empty() && values.len() >= TABLE_THRESHOLD {
        write_table(w, key, values);
        return;
    }
    let list = w.open_list(key, values.len());
    for value in values {
        // A narrow list's element carries a length and no descriptor — its shape
        // is the schema's to know — which is a byte per element, and the reason a
        // narrow list of small structs is smaller than a wide one.
        let element = w.open_element();
        value.colbin_write_run(w.buf);
        w.close(element);
    }
    w.close(list);
}

/// [`write_structs`] for a wide-keyed parent, whose elements each carry a
/// descriptor because a wide reader may not know what the element is.
pub fn write_structs8<T: Colbin>(w: &mut Writer8<'_>, key: u8, values: &[T]) {
    if values.is_empty() {
        return;
    }
    if !T::COLUMNS.is_empty() && values.len() >= TABLE_THRESHOLD {
        write_table8(w, key, values);
        return;
    }
    let list = w.open_list(key, values.len());
    for value in values {
        let element = if T::WIDE_KEYS {
            w.open_element_struct_wide()
        } else {
            w.open_element_struct()
        };
        value.colbin_write_run(w.buf);
        w.close(element);
    }
    w.close(list);
}

/// Writes a slice of structs transposed: one keyed column per field of the
/// element type.
fn write_table<T: Colbin>(w: &mut Writer<'_>, key: u8, values: &[T]) {
    let mark = w.open_table(key, values.len());
    let mut ints: Vec<i64> = Vec::with_capacity(values.len());
    let mut strings: Vec<&str> = Vec::with_capacity(values.len());
    for (index, spec) in T::COLUMNS.iter().enumerate() {
        match spec.kind {
            ColumnKind::Str => {
                strings.clear();
                strings.extend(values.iter().map(|value| value.colbin_column_str(index)));
                // A narrow table writes the column even when every row is empty,
                // where the wide one omits it. Both are legal and each reader
                // takes its own, but they are not the same bytes, so the two must
                // not be folded together.
                w.strings(spec.key, &strings);
            }
            ColumnKind::Int => {
                ints.clear();
                ints.extend(values.iter().map(|value| value.colbin_column_i64(index)));
                w.column(spec.key, &ints);
            }
        }
    }
    w.close(mark);
}

/// [`write_table`] for a wide-keyed parent.
fn write_table8<T: Colbin>(w: &mut Writer8<'_>, key: u8, values: &[T]) {
    let mark = w.open_table(key, values.len());
    let mut ints: Vec<i64> = Vec::with_capacity(values.len());
    let mut strings: Vec<&str> = Vec::with_capacity(values.len());
    for (index, spec) in T::COLUMNS.iter().enumerate() {
        match spec.kind {
            ColumnKind::Str => {
                strings.clear();
                strings.extend(values.iter().map(|value| value.colbin_column_str(index)));
                w.string_column(spec.key, &strings);
            }
            ColumnKind::Int => {
                ints.clear();
                ints.extend(values.iter().map(|value| value.colbin_column_i64(index)));
                w.column(spec.key, &ints);
            }
        }
    }
    w.close(mark);
}

/// Reads a slice of structs, dispatching on what the writer actually chose: a
/// list and a table are different classes, so the reader never has to be told.
pub fn read_structs<T: Colbin>(r: &mut Reader<'_>) -> Vec<T> {
    if r.is_table() {
        return read_table(r);
    }
    let Some((count, mut elements)) = r.counted() else {
        return Vec::new();
    };
    let mut out = Vec::with_capacity(count);
    for _ in 0..count {
        let Some(body) = elements.element() else {
            r.fail_with(elements.err());
            return out;
        };
        // A narrow element carries no descriptor, so its key width comes from
        // the schema rather than from the wire.
        match T::colbin_read_run(body, T::WIDE_KEYS) {
            Ok(value) => out.push(value),
            Err(err) => {
                r.fail(err);
                return out;
            }
        }
    }
    out
}

/// [`read_structs`] for a wide-keyed parent.
pub fn read_structs8<T: Colbin>(r: &mut Reader8<'_>) -> Vec<T> {
    if r.is_table() {
        return read_table8(r);
    }
    let Some((count, mut elements)) = r.list() else {
        return Vec::new();
    };
    let mut out = Vec::with_capacity(count);
    for _ in 0..count {
        let Some((body, wide_keys)) = elements.element_struct_body() else {
            r.fail_with(elements.err());
            return out;
        };
        match T::colbin_read_run(body, wide_keys) {
            Ok(value) => out.push(value),
            Err(err) => {
                r.fail(err);
                return out;
            }
        }
    }
    out
}

/// Reads a table back into a slice of structs, one column at a time. Each column
/// is decoded whole and then scattered across the rows, which is the order the
/// column codec wants: it fills a run and the scatter is a strided store.
fn read_table<T: Colbin>(r: &mut Reader<'_>) -> Vec<T> {
    let Some((rows, mut columns)) = r.counted() else {
        return Vec::new();
    };
    let mut out = Vec::with_capacity(rows);
    out.resize_with(rows, T::colbin_zero);
    let mut ints: Vec<i64> = Vec::new();
    let mut strings: Vec<String> = Vec::new();
    while columns.more() {
        let key = columns.key();
        let Some(index) = column_index::<T>(key) else {
            // A narrow key cannot be skipped, so an unknown column ends the table
            // rather than being stepped over.
            r.fail(Error::UnknownKey(key));
            return out;
        };
        match T::COLUMNS[index].kind {
            ColumnKind::Str => {
                strings.clear();
                columns.strings_into(&mut strings);
                scatter_strings(&mut out, index, &mut strings, rows);
            }
            ColumnKind::Int => {
                columns.column(rows, &mut ints);
                scatter_ints(&mut out, index, &ints, rows);
            }
        }
    }
    r.fail_with(columns.err());
    out
}

/// [`read_table`] for a wide-keyed parent, where an unknown column is stepped
/// over rather than refused.
fn read_table8<T: Colbin>(r: &mut Reader8<'_>) -> Vec<T> {
    let Some((rows, mut columns)) = r.table() else {
        return Vec::new();
    };
    let mut out = Vec::with_capacity(rows);
    out.resize_with(rows, T::colbin_zero);
    let mut ints: Vec<i64> = Vec::new();
    let mut strings: Vec<String> = Vec::new();
    while columns.more() {
        let key = columns.key();
        let Some(index) = column_index::<T>(key) else {
            if !columns.skip() {
                r.fail_with(columns.err());
                return out;
            }
            continue;
        };
        match T::COLUMNS[index].kind {
            ColumnKind::Str => {
                strings.clear();
                columns.strings_into(&mut strings);
                scatter_strings(&mut out, index, &mut strings, rows);
            }
            ColumnKind::Int => {
                columns.column(rows, &mut ints);
                scatter_ints(&mut out, index, &ints, rows);
            }
        }
    }
    r.fail_with(columns.err());
    out
}

/// The column a wire key names, or `None` when the type does not declare it.
fn column_index<T: Colbin>(key: u8) -> Option<usize> {
    T::COLUMNS.iter().position(|spec| spec.key == key)
}

fn scatter_ints<T: Colbin>(out: &mut [T], column: usize, values: &[i64], rows: usize) {
    if values.len() < rows {
        return;
    }
    for (row, value) in out.iter_mut().zip(values).take(rows) {
        row.colbin_set_column_i64(column, *value);
    }
}

fn scatter_strings<T: Colbin>(out: &mut [T], column: usize, values: &mut Vec<String>, rows: usize) {
    if values.len() < rows {
        return;
    }
    for (row, value) in out.iter_mut().zip(values.drain(..)).take(rows) {
        row.colbin_set_column_str(column, value);
    }
}

// --- map keys and values ------------------------------------------------------

/// What a map's keys and values may be: the set the wire carries as a bare,
/// key-less element.
///
/// A map's keys are *values*, not field ids, so there is no key width to choose
/// and nothing to look up — the entries are pairs of ordinary elements, the same
/// shape a list's are.
pub trait MapValue: Sized {
    /// Writes the value as a key-less element onto a narrow run.
    fn write_element(&self, w: &mut Writer<'_>);
    /// Writes the value as a key-less element onto a wide run.
    fn write_element8(&self, w: &mut Writer8<'_>);
    /// Reads the value back from a narrow run.
    fn read_element(r: &mut Reader<'_>) -> Self;
    /// Reads the value back from a wide run.
    fn read_element8(r: &mut Reader8<'_>) -> Self;
}

impl MapValue for String {
    fn write_element(&self, w: &mut Writer<'_>) {
        w.element_string(self);
    }
    fn write_element8(&self, w: &mut Writer8<'_>) {
        w.element_string(self);
    }
    fn read_element(r: &mut Reader<'_>) -> Self {
        r.element_string()
    }
    fn read_element8(r: &mut Reader8<'_>) -> Self {
        r.element_string()
    }
}

macro_rules! impl_map_signed {
    ($($type:ty),*) => {$(
        impl MapValue for $type {
            fn write_element(&self, w: &mut Writer<'_>) { w.element_int(i64::from(*self)); }
            fn write_element8(&self, w: &mut Writer8<'_>) { w.element_int(i64::from(*self)); }
            #[allow(clippy::cast_possible_truncation)]
            fn read_element(r: &mut Reader<'_>) -> Self { r.element_int() as Self }
            #[allow(clippy::cast_possible_truncation)]
            fn read_element8(r: &mut Reader8<'_>) -> Self { r.element_int() as Self }
        }
    )*};
}

macro_rules! impl_map_unsigned {
    ($($type:ty),*) => {$(
        impl MapValue for $type {
            fn write_element(&self, w: &mut Writer<'_>) { w.element_uint(u64::from(*self)); }
            fn write_element8(&self, w: &mut Writer8<'_>) { w.element_uint(u64::from(*self)); }
            #[allow(clippy::cast_possible_truncation)]
            fn read_element(r: &mut Reader<'_>) -> Self { r.element_uint() as Self }
            #[allow(clippy::cast_possible_truncation)]
            fn read_element8(r: &mut Reader8<'_>) -> Self { r.element_uint() as Self }
        }
    )*};
}

impl_map_signed!(i8, i16, i32, i64);
impl_map_unsigned!(u8, u16, u32, u64);

// A float rides in the integer shape with its bytes reversed, the same way a
// scalar float field does, so a round value costs two bytes here as well.
impl MapValue for f32 {
    fn write_element(&self, w: &mut Writer<'_>) {
        w.element_uint(u64::from(self.to_bits().swap_bytes()));
    }
    fn write_element8(&self, w: &mut Writer8<'_>) {
        w.element_uint(u64::from(self.to_bits().swap_bytes()));
    }
    #[allow(clippy::cast_possible_truncation)]
    fn read_element(r: &mut Reader<'_>) -> Self {
        Self::from_bits((r.element_uint() as u32).swap_bytes())
    }
    #[allow(clippy::cast_possible_truncation)]
    fn read_element8(r: &mut Reader8<'_>) -> Self {
        Self::from_bits((r.element_uint() as u32).swap_bytes())
    }
}

impl MapValue for f64 {
    fn write_element(&self, w: &mut Writer<'_>) {
        w.element_uint(self.to_bits().swap_bytes());
    }
    fn write_element8(&self, w: &mut Writer8<'_>) {
        w.element_uint(self.to_bits().swap_bytes());
    }
    fn read_element(r: &mut Reader<'_>) -> Self {
        Self::from_bits(r.element_uint().swap_bytes())
    }
    fn read_element8(r: &mut Reader8<'_>) -> Self {
        Self::from_bits(r.element_uint().swap_bytes())
    }
}

impl MapValue for bool {
    fn write_element(&self, w: &mut Writer<'_>) {
        w.element_uint(u64::from(*self));
    }
    fn write_element8(&self, w: &mut Writer8<'_>) {
        w.element_uint(u64::from(*self));
    }
    fn read_element(r: &mut Reader<'_>) -> Self {
        r.element_uint() == 1
    }
    fn read_element8(r: &mut Reader8<'_>) -> Self {
        r.element_uint() == 1
    }
}
