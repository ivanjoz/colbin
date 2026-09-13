//! The eight-bit key width, K8: a byte for the key and a byte for a descriptor
//! that names the field's class, where the four-bit width shares one byte and
//! takes the class from the schema.
//!
//! # What the extra byte buys
//!
//! * **256 fields**, against sixteen.
//! * **Skip.** Every class either carries a byte length or has one derivable
//!   from the descriptor alone, so a reader that has never heard of a key can
//!   step over it — which is schema evolution, and the foundation for a
//!   dynamically typed field.
//! * **An inline value.** A descriptor whose top bit is clear *is* the value,
//!   0..=127, so a small integer costs two bytes — the same as under K4. That is
//!   why the wide key is not simply a byte worse per field: it is a byte worse
//!   only above 127.
//!
//! # Descriptor
//!
//! ```text
//! 0 vvvvvvv                   the value, 0..127, no payload
//! 1 ccc dddd                  class ccc, detail dddd
//!
//! class 0 INT      [pos:1][n:3]         n magnitude bytes, as K4
//! class 1 BLOB     [enc:2][lw:2]        [size: lw] then size bytes
//! class 2 VEC      [w:2][pos:1][lw:1]   [bytelen: lw] then the elements
//! class 3 COL      [kind:2][lw:2]       [bytelen: lw] then a blocked column
//! class 4 LIST     [homog:1][-:1][lw:2] [bytelen: lw][count] then elements
//! class 5 STRUCT   [k8:1][lw:2][-:1]    [bytelen: lw] then a key run
//! class 6 MAP      [sub:1][k8:1][lw:2]  sub 0 = map, 1 = table
//! class 7 SPECIAL  [detail:4]           null, true, false, the varint integer
//! ```
//!
//! `lw` names the width of the length that follows: 0 → 1 byte, 1 → 2, 2 → 4,
//! 3 → 8. There is no varint anywhere in a length, so reading one is a branch
//! and one load rather than a loop whose trip count is data.

use super::{
    ELEMENT_SIZE_ESCAPE, INLINE_COMPOSITE_LENGTH, INLINE_ELEMENT_SIZE, INT_POSITIVE_FLAG, Integer,
    MAGNITUDE_WIDTH, Mark, append_array, append_count, append_elements, append_magnitude,
    array_plan_of, count_bytes, le_uint, length_code_for, narrow::widen_length, read_count,
    size_code_for,
};
use crate::{Error, column, packed5};
use alloc::borrow::ToOwned;
use alloc::string::String;
use alloc::vec::Vec;

// Descriptor classes, in bits 6..4 of an explicit descriptor.
pub(crate) const CLASS_INT: u8 = 0;
pub(crate) const CLASS_BLOB: u8 = 1;
pub(crate) const CLASS_VEC: u8 = 2;
pub(crate) const CLASS_COL: u8 = 3;
pub(crate) const CLASS_LIST: u8 = 4;
pub(crate) const CLASS_STRUCT: u8 = 5;
pub(crate) const CLASS_MAP: u8 = 6;
pub(crate) const CLASS_SPECIAL: u8 = 7;

/// The top bit: set means the descriptor names a class, clear means the
/// remaining seven bits are the value itself.
pub(super) const DESC_EXPLICIT: u8 = 0x80;
/// The largest integer a descriptor can be.
const MAX_INLINE_VALUE: u64 = 0x7F;

/// The SPECIAL detail bit that says the rest of the field is a varint integer,
/// which takes the upper half of the detail nibble.
///
/// # Why a second integer form exists
///
/// A wide field spends a whole byte on its key, so its descriptor begins with
/// nothing of the value in it, and the INT class then spends all four detail
/// bits on a sign and a byte count. A value of 300 therefore costs four bytes.
/// This form puts three value bits in the descriptor and continues seven at a
/// time, so 300 costs three.
///
/// It does not replace the byte-count form: seven bits per byte loses to eight
/// once a value is wide, so a random `i64` costs eleven bytes as a varint
/// against ten as a sign and a magnitude. **The writer emits whichever is
/// shorter**, which makes this strictly a saving — 7% on small ids and on
/// deltas, and no value anywhere that got larger.
pub(super) const SPECIAL_VARINT: u8 = 0b1000;

/// How much of the value the descriptor's detail nibble carries.
const VARINT_BITS: u32 = 3;

/// The LIST detail bit saying the elements share one shape and carry no
/// descriptor of their own — which is what a list of strings is.
const LIST_HOMOGENEOUS: u8 = 0b1000;

/// The STRUCT descriptor's `k8` bit, set when the key run inside uses eight-bit
/// keys. It is in the descriptor that *opens* the run rather than in a header,
/// which is what makes the key width a property of the scope.
pub(crate) const STRUCT_WIDE_KEYS: u8 = 0b1000;

/// The MAP class's `sub` bit: clear is a map, set is a table. They share a class
/// because they are the same shape — a length, a count and a body — and differ
/// only in what the body holds.
pub(crate) const TABLE_FLAG: u8 = 0b1000;

/// The BLOB descriptor's `enc` codes, in bits 3-2.
///
/// packed5 owns two of the four. A packed string writes no frame header of its
/// own — the descriptor already carries the size and the encoding, which is two
/// of the three things a header would hold — so the spare code buys the third:
/// the case mode the unit stream opens in.
const ENC_PACKED5: u8 = 1 << 2;
const ENC_PACKED5_UP: u8 = 3 << 2;
const ENC_MASK: u8 = 3 << 2;

/// Folds a signed value so that small negatives stay small, which is what makes
/// the varint worth using for a delta. An unsigned value is not folded: it would
/// pay a bit for a sign it does not have.
#[allow(clippy::cast_sign_loss)]
const fn zigzag(value: i64) -> u64 {
    ((value << 1) ^ (value >> 63)) as u64
}

#[allow(clippy::cast_possible_wrap)]
const fn unzigzag(value: u64) -> i64 {
    ((value >> 1) as i64) ^ -((value & 1) as i64)
}

/// The total field length the varint form would take, key byte included, which
/// is what the writer compares against the byte-count form.
const fn varint_len(value: u64) -> usize {
    let mut rest = value >> VARINT_BITS;
    let mut length = 2; // the key and the descriptor
    loop {
        length += 1;
        if rest < 0x80 {
            return length;
        }
        rest >>= 7;
    }
}

/// Writes the descriptor and the continuation bytes.
#[allow(clippy::cast_possible_truncation)]
fn append_varint(buf: &mut Vec<u8>, key: u8, value: u64) {
    buf.push(key);
    buf.push(descriptor(
        CLASS_SPECIAL,
        SPECIAL_VARINT | (value & 0b111) as u8,
    ));
    let mut rest = value >> VARINT_BITS;
    while rest >= 0x80 {
        buf.push(rest as u8 | 0x80);
        rest >>= 7;
    }
    buf.push(rest as u8);
}

/// Reads the varint form back, returning the value and the byte length of the
/// whole field.
fn read_varint(field: &[u8], detail: u8) -> Option<(u64, usize)> {
    let (value, length) = read_varint_at(field, 1, detail)?;
    Some((value, length + 1)) // the key byte this form does not read
}

/// [`read_varint`] anchored on the descriptor rather than on the key, which is
/// what a key-less element needs: `at` is where the descriptor sits and the
/// continuation bytes follow it. The length counts from the descriptor.
pub(super) fn read_varint_at(buf: &[u8], at: usize, detail: u8) -> Option<(u64, usize)> {
    let mut value = u64::from(detail & 0b111);
    let mut shift = VARINT_BITS;
    for (index, byte) in buf.iter().enumerate().skip(at + 1) {
        value |= u64::from(byte & 0x7F) << shift;
        if *byte < 0x80 {
            return Some((value, index + 1 - at));
        }
        shift += 7;
        if shift > 63 + VARINT_BITS {
            return None;
        }
    }
    None
}

pub(super) const fn descriptor(class: u8, detail: u8) -> u8 {
    DESC_EXPLICIT | class << 4 | detail
}

/// Appends fields with eight-bit keys. Like [`super::Writer`] it holds no state
/// but the buffer, checks nothing, and cannot fail.
pub struct Writer8<'a> {
    /// The buffer being appended to, public for the reason the narrow writer's
    /// is: a nested run at the other key width writes onto the same bytes.
    pub buf: &'a mut Vec<u8>,
}

impl<'a> Writer8<'a> {
    /// Starts a writer over a buffer, appending to whatever is already there.
    pub fn new(buf: &'a mut Vec<u8>) -> Self {
        Self { buf }
    }

    /// Writes an unsigned integer, and nothing at all when it is zero.
    ///
    /// Values to 127 are the descriptor itself, which is the whole reason a wide
    /// key is affordable: the common small field is two bytes here and two under
    /// K4.
    #[allow(clippy::cast_possible_truncation)]
    pub fn u64(&mut self, key: u8, value: u64) {
        if value == 0 {
            return;
        }
        if value <= MAX_INLINE_VALUE {
            self.buf.push(key);
            self.buf.push(value as u8);
            return;
        }
        self.uint_wide(key, value);
    }

    fn uint_wide(&mut self, key: u8, value: u64) {
        let (code, width) = size_code_for(value);
        // An unsigned value is not zigzagged: it has no sign to fold, and folding
        // it would cost a bit for nothing.
        if varint_len(value) < 2 + width {
            append_varint(self.buf, key, value);
            return;
        }
        self.buf.push(key);
        self.buf
            .push(descriptor(CLASS_INT, INT_POSITIVE_FLAG | code));
        append_magnitude(self.buf, value, width);
    }

    /// Writes a signed integer, as a zigzag varint or as a sign and a magnitude,
    /// whichever is shorter.
    ///
    /// A positive value does not go through [`Writer8::u64`]. The varint under
    /// SPECIAL is raw when `u64` wrote it and zigzagged when this did — the
    /// schema picks the reader, so the two never meet, but only if each writer
    /// stays on its own side.
    #[allow(clippy::cast_possible_truncation, clippy::cast_sign_loss)]
    pub fn i64(&mut self, key: u8, value: i64) {
        if value == 0 {
            return;
        }
        if value > 0 && value as u64 <= MAX_INLINE_VALUE {
            self.buf.push(key);
            self.buf.push(value as u8);
            return;
        }
        let mut magnitude = value as u64;
        let mut detail = INT_POSITIVE_FLAG;
        if value < 0 {
            // Negating through u64 rather than i64 keeps i64::MIN, whose positive
            // counterpart does not exist as an i64.
            magnitude = (value as u64).wrapping_neg();
            detail = 0;
        }
        let (code, width) = size_code_for(magnitude);
        let folded = zigzag(value);
        if varint_len(folded) < 2 + width {
            append_varint(self.buf, key, folded);
            return;
        }
        self.buf.push(key);
        self.buf.push(descriptor(CLASS_INT, detail | code));
        append_magnitude(self.buf, magnitude, width);
    }

    /// Writes two bytes when true and nothing when false. True rides in the
    /// inline form as the value one, so it needs no special code of its own.
    pub fn bool(&mut self, key: u8, value: bool) {
        if !value {
            return;
        }
        self.buf.push(key);
        self.buf.push(1);
    }

    /// Writes a field whose type cannot exceed two bytes.
    #[allow(clippy::cast_possible_truncation)]
    pub fn u16(&mut self, key: u8, value: u16) {
        if value == 0 {
            return;
        }
        if u64::from(value) <= MAX_INLINE_VALUE {
            self.buf.push(key);
            self.buf.push(value as u8);
            return;
        }
        if value <= 0xFF {
            // Three bytes either way at this width, so the fast path keeps it.
            self.buf.push(key);
            self.buf.push(descriptor(CLASS_INT, INT_POSITIVE_FLAG | 1));
            self.buf.push(value as u8);
            return;
        }
        // Past a byte the varint can be shorter, and a u16 should not encode
        // differently from a u32 holding the same value. `uint_wide` measures
        // both forms; Go's `U16` instead compares against `varintWinsToU16`,
        // the one window where the varint wins at this width, because it has an
        // inline budget to stay inside and this does not. The corpus case
        // `wide.integer widths` is what holds the two answers together.
        self.uint_wide(key, u64::from(value));
    }

    /// Writes a `u8` field, which travels in the `u16` shape.
    pub fn u8(&mut self, key: u8, value: u8) {
        self.u16(key, u16::from(value));
    }

    /// Writes a field whose type cannot exceed four bytes.
    #[allow(clippy::cast_possible_truncation)]
    pub fn u32(&mut self, key: u8, value: u32) {
        if value == 0 {
            return;
        }
        if u64::from(value) <= MAX_INLINE_VALUE {
            self.buf.push(key);
            self.buf.push(value as u8);
            return;
        }
        self.uint_wide(key, u64::from(value));
    }

    /// Writes a signed field no wider than four bytes.
    pub fn i32(&mut self, key: u8, value: i32) {
        self.i64(key, i64::from(value));
    }

    /// Writes an `f32` as its reversed bit pattern, exactly as the narrow writer
    /// does.
    pub fn f32(&mut self, key: u8, value: f32) {
        self.u64(key, u64::from(value.to_bits().swap_bytes()));
    }

    /// Writes an `f64` as its reversed bit pattern.
    pub fn f64(&mut self, key: u8, value: f64) {
        self.u64(key, value.to_bits().swap_bytes());
    }

    /// Writes a length-prefixed string, and nothing when it is empty.
    pub fn string(&mut self, key: u8, value: &str) {
        self.bytes(key, value.as_bytes());
    }

    /// Writes a length-prefixed blob, and nothing when it is empty.
    pub fn bytes(&mut self, key: u8, value: &[u8]) {
        if value.is_empty() {
            return;
        }
        self.blob_header(key, value.len());
        self.buf.extend_from_slice(value);
    }

    fn blob_header(&mut self, key: u8, size: usize) {
        let (code, width) = length_code_for(size as u64);
        self.buf.push(key);
        self.buf.push(descriptor(CLASS_BLOB, code));
        append_magnitude(self.buf, size as u64, width);
    }

    /// Writes a string in the packed5 encoding when that is smaller than the raw
    /// bytes, and raw when it is not.
    ///
    /// Choosing per string rather than per message is what keeps the encoding
    /// from ever costing anything: a token that does not pack is written raw, and
    /// the descriptor says so — which is also why a reader never has to be told
    /// which to expect.
    pub fn packed_string(&mut self, key: u8, value: &str) {
        if value.is_empty() {
            return;
        }
        let Some((stream, upper)) = packed5::payload(value.as_bytes()) else {
            self.string(key, value);
            return;
        };
        let enc = if upper { ENC_PACKED5_UP } else { ENC_PACKED5 };
        let (code, width) = length_code_for(stream.len() as u64);
        self.buf.push(key);
        self.buf.push(descriptor(CLASS_BLOB, enc | code));
        append_magnitude(self.buf, stream.len() as u64, width);
        self.buf.extend_from_slice(&stream);
    }

    /// Writes an array of integers at one width, chosen from the widest element,
    /// and nothing when the array is empty.
    pub fn ints<T: Integer>(&mut self, key: u8, values: &[T]) {
        if values.is_empty() {
            return;
        }
        let (all_positive, width, width_code) = array_plan_of(values);
        let byte_length = values.len() * width;
        // A vector's length is in bytes, not elements, so a reader that does not
        // know the key can still skip it. The count is byteLength >> widthCode and
        // never goes on the wire.
        let (length_code, length_bytes) = if byte_length > 0xFF { (1, 4) } else { (0, 1) };
        let mut detail = width_code << 2 | length_code;
        if all_positive {
            detail |= 0b10;
        }
        self.buf.push(key);
        self.buf.push(descriptor(CLASS_VEC, detail));
        append_magnitude(self.buf, byte_length as u64, length_bytes);
        append_elements(self.buf, values, width);
    }

    /// Writes a count and then each element behind its own length, inside a byte
    /// length that lets the whole field be skipped.
    #[allow(clippy::cast_possible_truncation)]
    pub fn strings<S: AsRef<str>>(&mut self, key: u8, values: &[S]) {
        if values.is_empty() {
            return;
        }
        let mut payload = 0;
        for value in values {
            let length = value.as_ref().len();
            payload += if length <= INLINE_ELEMENT_SIZE {
                1 + length
            } else {
                5 + length
            };
        }
        // The declared length covers the count as well as the elements, which is
        // what makes a skip uniform across every composite: two bytes of framing,
        // the length field, and then exactly that many bytes.
        let counted = payload + count_bytes(values.len());
        let (code, width) = length_code_for(counted as u64);
        self.buf.push(key);
        self.buf
            .push(descriptor(CLASS_LIST, LIST_HOMOGENEOUS | code));
        append_magnitude(self.buf, counted as u64, width);
        append_count(self.buf, values.len());
        for value in values {
            let value = value.as_ref().as_bytes();
            if value.len() <= INLINE_ELEMENT_SIZE {
                self.buf.push(value.len() as u8);
            } else {
                self.buf.push(ELEMENT_SIZE_ESCAPE);
                self.buf
                    .extend_from_slice(&(value.len() as u32).to_le_bytes());
            }
            self.buf.extend_from_slice(value);
        }
    }

    /// Writes an explicit zero, for an optional field holding one.
    pub fn zero(&mut self, key: u8) {
        self.buf.push(key);
        self.buf.push(0);
    }

    /// Writes a blob of no bytes.
    pub fn empty_string(&mut self, key: u8) {
        self.buf.push(key);
        self.buf.push(descriptor(CLASS_BLOB, 0));
        self.buf.push(0);
    }

    // --- composites ----------------------------------------------------------

    /// Writes a key, a descriptor and a one-byte length placeholder.
    fn open_composite(&mut self, key: u8, class: u8, detail: u8) -> Mark {
        self.buf.push(key);
        self.buf.push(descriptor(class, detail));
        self.buf.push(0);
        Mark {
            at: self.buf.len() - 1,
        }
    }

    /// Begins a nested key run under `key`, narrow-keyed inside.
    pub fn open_struct(&mut self, key: u8) -> Mark {
        self.open_composite(key, CLASS_STRUCT, 0)
    }

    /// Begins a nested key run whose own keys are eight bits.
    pub fn open_struct_wide(&mut self, key: u8) -> Mark {
        self.open_composite(key, CLASS_STRUCT, STRUCT_WIDE_KEYS)
    }

    /// Begins a struct as a list element or a map value: a descriptor and a
    /// length, with no key.
    pub fn open_element_struct(&mut self) -> Mark {
        self.buf.push(descriptor(CLASS_STRUCT, 0));
        self.buf.push(0);
        Mark {
            at: self.buf.len() - 1,
        }
    }

    /// [`Writer8::open_element_struct`] for a wide-keyed element.
    pub fn open_element_struct_wide(&mut self) -> Mark {
        self.buf.push(descriptor(CLASS_STRUCT, STRUCT_WIDE_KEYS));
        self.buf.push(0);
        Mark {
            at: self.buf.len() - 1,
        }
    }

    /// Begins a list of `count` values under `key`. Each element is written as a
    /// bare descriptor and payload.
    pub fn open_list(&mut self, key: u8, count: usize) -> Mark {
        let mark = self.open_composite(key, CLASS_LIST, 0);
        append_count(self.buf, count);
        mark
    }

    /// Begins a map of `count` entries under `key`. Each entry is a key value
    /// then a value value, both written as a bare descriptor and payload.
    pub fn open_map(&mut self, key: u8, count: usize) -> Mark {
        let mark = self.open_composite(key, CLASS_MAP, 0);
        append_count(self.buf, count);
        mark
    }

    /// Begins a table of `rows` rows under `key`. Write one column per field
    /// with the column writers, then [`Writer8::close`].
    pub fn open_table(&mut self, key: u8, rows: usize) -> Mark {
        let mark = self.open_composite(key, CLASS_MAP, TABLE_FLAG);
        append_count(self.buf, rows);
        mark
    }

    /// Patches a composite's length. Every open must have exactly one close, and
    /// they must nest.
    #[allow(clippy::cast_possible_truncation)]
    pub fn close(&mut self, mark: Mark) {
        let body = self.buf.len() - (mark.at + 1);
        if body < INLINE_COMPOSITE_LENGTH {
            self.buf[mark.at] = body as u8;
            return;
        }
        widen_length(self.buf, mark, body);
    }

    /// Writes an integer element.
    #[allow(clippy::cast_possible_truncation)]
    pub fn element_uint(&mut self, value: u64) {
        if value <= MAX_INLINE_VALUE {
            self.buf.push(value as u8);
            return;
        }
        let (code, width) = size_code_for(value);
        self.buf
            .push(descriptor(CLASS_INT, INT_POSITIVE_FLAG | code));
        append_magnitude(self.buf, value, width);
    }

    /// Writes a signed integer element.
    #[allow(clippy::cast_sign_loss)]
    pub fn element_int(&mut self, value: i64) {
        if value >= 0 {
            self.element_uint(value as u64);
            return;
        }
        let magnitude = (value as u64).wrapping_neg();
        let (code, width) = size_code_for(magnitude);
        self.buf.push(descriptor(CLASS_INT, code));
        append_magnitude(self.buf, magnitude, width);
    }

    /// Writes a string element.
    pub fn element_string(&mut self, value: &str) {
        let (code, width) = length_code_for(value.len() as u64);
        self.buf.push(descriptor(CLASS_BLOB, code));
        append_magnitude(self.buf, value.len() as u64, width);
        self.buf.extend_from_slice(value.as_bytes());
    }

    /// Writes an integer column under `key`, and nothing at all when every value
    /// is zero: an absent column key means a column of zeros.
    pub fn column<T: column::Signed>(&mut self, key: u8, values: &[T]) {
        if values.is_empty() || values.iter().all(|value| value.as_i64() == 0) {
            return;
        }
        let mark = self.open_composite(key, CLASS_COL, 0);
        column::append_array(self.buf, values);
        self.close(mark);
    }

    /// Writes a string column, which is a list of blobs under the column's key.
    ///
    /// A column of nothing but empty strings is omitted, for the same reason an
    /// all-zero integer column is: its absence already says so, and writing it
    /// would cost a byte per row to say nothing.
    pub fn string_column<S: AsRef<str>>(&mut self, key: u8, values: &[S]) {
        if values.iter().all(|value| value.as_ref().is_empty()) {
            return;
        }
        self.strings(key, values);
    }
}

/// Walks a message written by [`Writer8`]. Errors are sticky, exactly as in the
/// narrow reader.
pub struct Reader8<'a> {
    pub(super) buf: &'a [u8],
    pub(super) at: usize,
    pub(super) err: Option<Error>,
}

impl<'a> Reader8<'a> {
    /// Starts a reader over one message.
    pub fn new(message: &'a [u8]) -> Self {
        Self {
            buf: message,
            at: 0,
            err: None,
        }
    }

    /// Starts a reader whose cursor begins at `at`, which is what the bitmap
    /// framing needs: its fields follow a bitmap rather than starting at byte
    /// zero.
    pub(super) fn at(message: &'a [u8], at: usize) -> Self {
        Self {
            buf: message,
            at,
            err: None,
        }
    }

    /// Steps the cursor back onto the byte before the descriptor, which is how a
    /// key-less value reaches the keyed reads: they expect a key byte there and
    /// never look at it.
    pub(super) fn step_back(&mut self) {
        self.at = self.at.saturating_sub(1);
    }

    /// Starts a reader over a list's or a map's elements, whose values carry a
    /// descriptor but no key.
    fn elements(body: &'a [u8]) -> Self {
        // The element reader starts one byte in, so that each read can step the
        // cursor back to find its descriptor — a key-less value is one byte
        // shorter than a keyed one, and backing up is what lets both share the
        // keyed value handling instead of a second copy of it.
        Self {
            buf: body,
            at: 1,
            err: None,
        }
    }

    /// Reports whether another field follows.
    pub fn more(&self) -> bool {
        self.at + 1 < self.buf.len()
    }

    /// The field the cursor is on. It does not advance: the typed read does.
    pub fn key(&self) -> u8 {
        self.buf.get(self.at).copied().unwrap_or(0)
    }

    /// The first failure.
    pub fn err(&self) -> Result<(), Error> {
        match &self.err {
            Some(err) => Err(err.clone()),
            None => Ok(()),
        }
    }

    /// Records a failure, parking the cursor at the end of the message.
    pub fn fail(&mut self, err: Error) {
        if self.err.is_none() {
            self.err = Some(err);
        }
        self.at = self.buf.len();
    }

    /// Records an error from a sub-reader on its parent.
    pub fn fail_with(&mut self, result: Result<(), Error>) {
        if let Err(err) = result {
            self.fail(err);
        }
    }

    /// Reads an unsigned integer field, ignoring the sign bit.
    pub fn u64(&mut self) -> u64 {
        if let Some(desc) = self.buf.get(self.at + 1)
            && *desc < DESC_EXPLICIT
        {
            self.at += 2;
            return u64::from(*desc);
        }
        self.uint_wide()
    }

    fn uint_wide(&mut self) -> u64 {
        let Some(desc) = self.buf.get(self.at + 1).copied() else {
            self.fail(Error::Truncated);
            return 0;
        };
        if desc < DESC_EXPLICIT {
            self.at += 2;
            return u64::from(desc);
        }
        let class = (desc >> 4) & 0b111;
        if class != CLASS_INT {
            if class == CLASS_SPECIAL && desc & SPECIAL_VARINT != 0 {
                let Some((value, length)) = read_varint(&self.buf[self.at..], desc) else {
                    self.fail(Error::Truncated);
                    return 0;
                };
                self.at += length;
                return value;
            }
            self.fail(Error::BadDescriptor);
            return 0;
        }
        let width = MAGNITUDE_WIDTH[usize::from(desc & 0b111)];
        let rest = &self.buf[self.at + 2..];
        if rest.len() < width {
            self.fail(Error::Truncated);
            return 0;
        }
        self.at += 2 + width;
        if width == 0 {
            return 1;
        }
        le_uint(rest, width)
    }

    /// Reads a signed integer field.
    #[allow(clippy::cast_possible_wrap)]
    pub fn i64(&mut self) -> i64 {
        let Some(desc) = self.buf.get(self.at + 1).copied() else {
            self.fail(Error::Truncated);
            return 0;
        };
        // A varint written by the signed writer is zigzagged, which carries its
        // own sign.
        if desc >= DESC_EXPLICIT
            && (desc >> 4) & 0b111 == CLASS_SPECIAL
            && desc & SPECIAL_VARINT != 0
        {
            return unzigzag(self.uint_wide());
        }
        let negative = desc >= DESC_EXPLICIT
            && (desc >> 4) & 0b111 == CLASS_INT
            && desc & INT_POSITIVE_FLAG == 0;
        let magnitude = self.uint_wide();
        if negative {
            return (magnitude as i64).wrapping_neg();
        }
        magnitude as i64
    }

    /// Reads a field written by [`Writer8::bool`].
    pub fn bool(&mut self) -> bool {
        self.u64() == 1
    }

    /// Reads a `u16`, refusing a field wider than the type this side declares.
    #[allow(clippy::cast_possible_truncation)]
    pub fn u16(&mut self) -> u16 {
        let value = self.u64();
        if value > 0xFFFF {
            self.fail(Error::FieldTooWide);
            return 0;
        }
        value as u16
    }

    /// Reads a `u8`, refusing a field wider than the type this side declares.
    #[allow(clippy::cast_possible_truncation)]
    pub fn u8(&mut self) -> u8 {
        let value = self.u64();
        if value > 0xFF {
            self.fail(Error::FieldTooWide);
            return 0;
        }
        value as u8
    }

    /// Reads a `u32`, refusing a field wider than the type this side declares.
    #[allow(clippy::cast_possible_truncation)]
    pub fn u32(&mut self) -> u32 {
        let value = self.u64();
        if value > 0xFFFF_FFFF {
            self.fail(Error::FieldTooWide);
            return 0;
        }
        value as u32
    }

    /// Reads an `i32` field.
    #[allow(clippy::cast_possible_truncation)]
    pub fn i32(&mut self) -> i32 {
        self.i64() as i32
    }

    /// Reads an `i16` field.
    #[allow(clippy::cast_possible_truncation)]
    pub fn i16(&mut self) -> i16 {
        self.i64() as i16
    }

    /// Reads an `i8` field.
    #[allow(clippy::cast_possible_truncation)]
    pub fn i8(&mut self) -> i8 {
        self.i64() as i8
    }

    /// Reads a field written by [`Writer8::f32`].
    #[allow(clippy::cast_possible_truncation)]
    pub fn f32(&mut self) -> f32 {
        f32::from_bits((self.u64() as u32).swap_bytes())
    }

    /// Reads a field written by [`Writer8::f64`].
    pub fn f64(&mut self) -> f64 {
        f64::from_bits(self.u64().swap_bytes())
    }

    /// Returns the field's bytes as a sub-slice of the message, without copying.
    pub fn bytes(&mut self) -> &'a [u8] {
        let Some((size, start)) = self.length_of(CLASS_BLOB) else {
            return &[];
        };
        if size > self.buf.len() - start {
            self.fail(Error::Truncated);
            return &[];
        }
        self.at = start + size;
        &self.buf[start..start + size]
    }

    /// Copies the field into a `String`.
    pub fn string(&mut self) -> String {
        let bytes = self.bytes();
        match core::str::from_utf8(bytes) {
            Ok(value) => value.to_owned(),
            Err(_) => {
                self.fail(Error::NotUtf8);
                String::new()
            }
        }
    }

    /// Reads a string written by [`Writer8::packed_string`] or by
    /// [`Writer8::string`]: the descriptor's `enc` code says which, so a reader
    /// needs no configuration and cannot be wrong about it.
    pub fn packed_string(&mut self) -> String {
        let Some(desc) = self.buf.get(self.at + 1).copied() else {
            self.fail(Error::Truncated);
            return String::new();
        };
        let enc = desc & ENC_MASK;
        let raw = self.bytes();
        if enc == 0 {
            return match core::str::from_utf8(raw) {
                Ok(value) => value.to_owned(),
                Err(_) => {
                    self.fail(Error::NotUtf8);
                    String::new()
                }
            };
        }
        if enc == 2 << 2 {
            self.fail(Error::BadDescriptor); // a dictionary reference, not a string
            return String::new();
        }
        let mut bytes = Vec::new();
        match packed5::append_string(&mut bytes, raw, enc == ENC_PACKED5_UP) {
            Ok(()) => match String::from_utf8(bytes) {
                Ok(value) => value,
                Err(_) => {
                    self.fail(Error::NotUtf8);
                    String::new()
                }
            },
            Err(err) => {
                self.fail(err.into());
                String::new()
            }
        }
    }

    /// Reads the length a class's descriptor declares and returns it with the
    /// offset just past it. It also checks the class, so a reader asking for a
    /// string and finding an array is told rather than handed nonsense.
    fn length_of(&mut self, want: u8) -> Option<(usize, usize)> {
        match super::dynamic::declared_length(self.buf, self.at + 1, want) {
            Ok(found) => Some(found),
            Err(err) => {
                self.fail(err);
                None
            }
        }
    }

    /// Reads an integer array field.
    pub fn ints<T: Integer>(&mut self) -> Vec<T> {
        let mut dst = Vec::new();
        self.ints_into(&mut dst);
        dst
    }

    /// Appends an integer array field's elements onto `dst`.
    pub fn ints_into<T: Integer>(&mut self, dst: &mut Vec<T>) {
        let Some((byte_length, start)) = self.length_of(CLASS_VEC) else {
            return;
        };
        let desc = self.buf[self.at + 1];
        let width = 1_usize << ((desc >> 2) & 0b11);
        if byte_length % width != 0 || byte_length > self.buf.len() - start {
            self.fail(Error::Truncated);
            return;
        }
        let elements = &self.buf[start..start + byte_length];
        self.at = start + byte_length;
        append_array(dst, elements, width, desc & 0b10 != 0);
    }

    /// Copies each element of a string list into a `String`.
    pub fn strings(&mut self) -> Vec<String> {
        let mut dst = Vec::new();
        self.strings_into(&mut dst);
        dst
    }

    /// Appends each element of a string list onto `dst`, reusing its capacity.
    pub fn strings_into(&mut self, dst: &mut Vec<String>) {
        for raw in self.strings_bytes() {
            match core::str::from_utf8(raw) {
                Ok(value) => dst.push(value.to_owned()),
                Err(_) => {
                    self.fail(Error::NotUtf8);
                    return;
                }
            }
        }
    }

    /// Appends each element of a string list as a sub-slice of the message.
    pub fn strings_bytes(&mut self) -> Vec<&'a [u8]> {
        let mut dst = Vec::new();
        let Some((count, mut at)) = self.list_header() else {
            return dst;
        };
        for _ in 0..count {
            let Some((size, next)) = self.element_size(at) else {
                return dst;
            };
            if size > self.buf.len() - next {
                self.fail(Error::Truncated);
                return dst;
            }
            dst.push(&self.buf[next..next + size]);
            at = next + size;
        }
        self.at = at;
        dst
    }

    /// Reads a list's byte length and count and returns the count with the
    /// offset of the first element.
    fn list_header(&mut self) -> Option<(usize, usize)> {
        let (length, start) = self.length_of(CLASS_LIST)?;
        if length > self.buf.len() - start {
            self.fail(Error::Truncated);
            return None;
        }
        let Some((count, consumed)) = read_count(&self.buf[start..start + length]) else {
            self.fail(Error::Truncated);
            return None;
        };
        Some((count, start + consumed))
    }

    /// Reads one list element length: one byte, or four more behind the escape.
    fn element_size(&mut self, at: usize) -> Option<(usize, usize)> {
        let Some(size) = self.buf.get(at).copied() else {
            self.fail(Error::Truncated);
            return None;
        };
        if size != ELEMENT_SIZE_ESCAPE {
            return Some((usize::from(size), at + 1));
        }
        let Some(bytes) = self.buf.get(at + 1..at + 5) else {
            self.fail(Error::Truncated);
            return None;
        };
        let bytes: [u8; 4] = bytes.try_into().expect("four bytes");
        Some((u32::from_le_bytes(bytes) as usize, at + 5))
    }

    /// Steps over the field at the cursor without knowing what it is, which is
    /// the capability the wide key exists for.
    pub fn skip(&mut self) -> bool {
        let Some(size) = self.field_size() else {
            return false;
        };
        self.at += size;
        true
    }

    /// The whole of the skip: every class either carries a byte length or has
    /// one derivable from its descriptor.
    ///
    /// The key is one byte and the value is [`super::dynamic::value_size`]'s
    /// business, so that a key-less element — a list's, a map's, a dynamic
    /// value's — is sized by the same code rather than by a second copy of it
    /// that could disagree.
    fn field_size(&mut self) -> Option<usize> {
        match super::dynamic::value_size(self.buf, self.at + 1) {
            Ok(size) => Some(1 + size),
            Err(err) => {
                self.fail(err);
                None
            }
        }
    }

    // --- composites ----------------------------------------------------------

    /// Checks the class, reads the length and returns the body as a sub-slice,
    /// advancing the cursor past the whole field.
    fn composite_body(&mut self, want: u8) -> Option<&'a [u8]> {
        let (length, start) = self.length_of(want)?;
        if length > self.buf.len() - start {
            self.fail(Error::Truncated);
            return None;
        }
        self.at = start + length;
        Some(&self.buf[start..start + length])
    }

    /// [`Reader8::composite_body`] for the classes whose body begins with a
    /// count.
    fn counted_body(&mut self, want: u8) -> Option<(usize, Reader8<'a>)> {
        let body = self.composite_body(want)?;
        let Some((count, at)) = read_count(body) else {
            self.fail(Error::Truncated);
            return None;
        };
        Some((count, Reader8::elements(&body[at - 1..])))
    }

    /// Returns a reader over a nested key run and advances past it. It is only
    /// correct for a run the descriptor says is wide-keyed;
    /// [`Reader8::struct_body`] is what a caller that handles both widths uses.
    pub fn struct_reader(&mut self) -> Option<Reader8<'a>> {
        let body = self.composite_body(CLASS_STRUCT)?;
        Some(Reader8::new(body))
    }

    /// Returns a nested run's bytes and the key width it uses, advancing past
    /// the whole field.
    pub fn struct_body(&mut self) -> Option<(&'a [u8], bool)> {
        let Some(desc) = self.buf.get(self.at + 1).copied() else {
            self.fail(Error::Truncated);
            return None;
        };
        let wide_keys = desc & STRUCT_WIDE_KEYS != 0;
        let body = self.composite_body(CLASS_STRUCT)?;
        Some((body, wide_keys))
    }

    /// [`Reader8::struct_body`] for a key-less element.
    pub fn element_struct_body(&mut self) -> Option<(&'a [u8], bool)> {
        self.step_back();
        self.struct_body()
    }

    /// Reports whether another key-less value follows. It differs from
    /// [`Reader8::more`] because a key-less value is one byte rather than two at
    /// its shortest.
    pub fn more_elements(&self) -> bool {
        self.err.is_none() && self.at < self.buf.len()
    }

    /// Returns the element count and a reader over a list's elements.
    pub fn list(&mut self) -> Option<(usize, Reader8<'a>)> {
        self.counted_body(CLASS_LIST)
    }

    /// Returns the entry count and a reader over a map's entries, which
    /// alternate key and value.
    pub fn map(&mut self) -> Option<(usize, Reader8<'a>)> {
        self.counted_body(CLASS_MAP)
    }

    /// Reads an integer element.
    pub fn element_uint(&mut self) -> u64 {
        self.step_back();
        self.u64()
    }

    /// Reads a signed integer element.
    pub fn element_int(&mut self) -> i64 {
        self.step_back();
        self.i64()
    }

    /// Reads a string element.
    pub fn element_string(&mut self) -> String {
        self.step_back();
        self.string()
    }

    /// Reports whether the field at the cursor is a table rather than a list. A
    /// writer chooses between them on the row count, so a reader has to ask.
    pub fn is_table(&self) -> bool {
        self.buf.get(self.at + 1).is_some_and(|desc| {
            *desc >= DESC_EXPLICIT && (desc >> 4) & 0b111 == CLASS_MAP && desc & TABLE_FLAG != 0
        })
    }

    /// Returns the row count and a reader over the columns, which are an
    /// ordinary key run.
    pub fn table(&mut self) -> Option<(usize, Reader8<'a>)> {
        let Some(desc) = self.buf.get(self.at + 1).copied() else {
            self.fail(Error::Truncated);
            return None;
        };
        if desc & TABLE_FLAG == 0 {
            self.fail(Error::BadDescriptor);
            return None;
        }
        let body = self.composite_body(CLASS_MAP)?;
        let Some((rows, at)) = read_count(body) else {
            self.fail(Error::Truncated);
            return None;
        };
        // The columns are keyed, so unlike a list's elements they read as an
        // ordinary run and need no cursor offset.
        Some((rows, Reader8::new(&body[at..])))
    }

    /// Reads a column into `dst`. The row count comes from the table, which said
    /// it once for every column.
    pub fn column<T: column::Signed>(&mut self, rows: usize, dst: &mut Vec<T>) {
        let Some(body) = self.composite_body(CLASS_COL) else {
            return;
        };
        dst.clear();
        dst.resize(rows, T::from_i64(0));
        if let Err(err) = column::decode_array(body, rows, dst) {
            self.fail(err);
        }
    }
}
