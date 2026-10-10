//! The four-bit key width, K4: the key shares one byte with a nibble that says
//! how long the field is, and the field's type comes from the schema.
//!
//! This is the default and the fast path. Four bits have no room for a class,
//! so the nibble does the one thing a reader needs before it knows the type —
//! it sizes the field — and the type decides only what the bytes mean:
//!
//! ```text
//! [key:4][nibble:4] [payload]
//!
//!   0..3             no payload
//!   4..11            exactly nibble − 3 bytes, 1..8
//!   12..15 (11 f1f0) a length, then that many bytes; f1f0 is the type's flag
//!
//!   length           one byte ≤ 253 · 0xFE then a u16 · 0xFF then a u32
//! ```
//!
//! That is what makes [`Reader::skip`] work under narrow keys exactly as
//! [`super::Reader8::skip`] does under wide ones: a type may drop a field, or
//! gain one, and still read every row already written.
//!
//! Composites are the length form with a body, and their flag tells apart the
//! shapes one Rust type can take:
//!
//! ```text
//! struct  [key:4][1 1 0 k8]  [length] [key run]
//! list    [key:4][1 1 0 0]   [length] [count] ( [elementLength] [key run] )*
//! table   [key:4][1 1 0 1]   [length] [rows]  column*
//! map     [key:4][1 1 0 0]   [length] [count] ( key element, value element )*
//! ```

use super::{
    ELEMENT_SIZE_ESCAPE, INLINE_ELEMENT_SIZE, INT_POSITIVE_FLAG, Integer, LENGTH_WIDTH,
    MAGNITUDE_WIDTH, Mark, UINT_INLINE_MAX, UINT_WIDTH_BASE, append_array, append_count,
    append_elements, append_magnitude, bit_length, le_uint, length_code_for, read_count,
    size_code_for,
};
use crate::Error;
use crate::column;
use alloc::borrow::ToOwned;
use alloc::string::String;
use alloc::vec::Vec;

/// The last nibble that carries no payload. For a number it *is* the value —
/// `nibble + 1`, or −1 at nibble 3 for a signed field — and for a bool `true`.
const NIBBLE_INLINE_LAST: u8 = 3;

/// What a sized nibble is above its byte count: nibble 4 is one byte and
/// nibble 11 eight, so a value of up to eight bytes carries no length at all.
const NIBBLE_SIZED_BIAS: u8 = 3;

/// The length form, `1 1 f1 f0`: a length follows the header, and the low two
/// bits are a flag whose meaning is the type's. Every nibble from here up is
/// sized the same way, which is why a flag can never change the field's size.
const NIBBLE_LENGTH: u8 = 0b1100;

/// The flag bits of the length form.
const FLAG_MASK: u8 = 0b11;

/// A string's flags: the bytes as they are, or packed5 opening in lower or in
/// upper case (`INTERNALS.md` §8). `11` is unassigned and refused.
const STRING_RAW: u8 = 0b00;
const STRING_PACKED_LOWER: u8 = 0b01;
const STRING_PACKED_UPPER: u8 = 0b10;

/// A composite's `f0`: the nested run's own key width for a struct, and table
/// rather than list for a slice of structs. A struct and a map are different
/// Rust types, so their flags overlap nothing a reader could confuse.
const COMPOSITE_STRUCT_WIDE: u8 = 0b01;
const COMPOSITE_TABLE: u8 = 0b01;

/// The longest length one byte holds. 254 and 255 are the escapes, so a length
/// of either is spelled the long way.
const LENGTH_INLINE_MAX: usize = 253;
const LENGTH_ESCAPE_U16: u8 = 0xFE;
const LENGTH_ESCAPE_U32: u8 = 0xFF;

/// The widest a narrow field can be: its length is at most a u32. A blob
/// larger than that is a wide field's job.
const NARROW_FIELD_LIMIT: &str = "colbin: a narrow field holds at most 2^32 − 1 bytes";

/// What a length of `size` occupies in front of its bytes.
const fn length_bytes(size: usize) -> usize {
    if size <= LENGTH_INLINE_MAX {
        1
    } else if size <= 0xFFFF {
        3
    } else {
        5
    }
}

/// Writes a length in the shortest of its three forms.
#[allow(clippy::cast_possible_truncation)]
fn append_length(buf: &mut Vec<u8>, size: usize) {
    if size <= LENGTH_INLINE_MAX {
        buf.push(size as u8);
    } else if size <= 0xFFFF {
        buf.push(LENGTH_ESCAPE_U16);
        buf.extend_from_slice(&(size as u16).to_le_bytes());
    } else {
        let size = u32::try_from(size).expect(NARROW_FIELD_LIMIT);
        buf.push(LENGTH_ESCAPE_U32);
        buf.extend_from_slice(&size.to_le_bytes());
    }
}

/// The bytes a value of `bits` significant bits occupies, at least one.
const fn bytes_for_bits(bits: usize) -> usize {
    if bits == 0 { 1 } else { bits.div_ceil(8) }
}

/// The element width code for an array that needs `bytes` per element, and the
/// width it names: 1, 2, 4 or 8.
const fn array_width_for(bytes: usize) -> (usize, u8) {
    match bytes {
        1 => (1, 0),
        2 => (2, 1),
        3 | 4 => (4, 2),
        _ => (8, 3),
    }
}

/// Picks the narrowest element width that holds every element of an array.
///
/// The form is the element *type's*, not the values': an unsigned type writes
/// magnitudes and a signed one two's complement, whatever the elements happen
/// to be. A signed array that is all positive could have been magnitudes a bit
/// narrower, but then the reader would need a flag to know which, and the
/// length form's two flag bits are already the width. The OR keeps the loop to
/// one operation per element: its bit length is the widest element's.
fn array_plan<T: Integer>(values: &[T]) -> (usize, u8) {
    let mut seen = 0_u64;
    if T::SIGNED_TYPE {
        for value in values {
            let signed = value.as_i64();
            // A negative value's significant bits are its complement's, so −1
            // needs only the sign bit and −128 fits a byte.
            seen |= (signed ^ (signed >> 63)).cast_unsigned();
        }
        return array_width_for(bytes_for_bits(bit_length(seen) + 1));
    }
    for value in values {
        seen |= value.as_u64();
    }
    array_width_for(bytes_for_bits(bit_length(seen)))
}

/// Appends fields with four-bit keys onto a buffer the caller owns.
///
/// # A write cannot fail
///
/// There is no error to check. Every value has a form, and a length escalates
/// from one byte to three to five as the field grows. The one ceiling is the
/// u32 length itself — a narrow field holds at most 2³² − 1 bytes — and a
/// program that hands a narrow writer more than four gigabytes in one field has
/// chosen the wrong key width, which panics rather than writing a field nothing
/// could read back.
///
/// # Keys are not checked, on purpose
///
/// The reader defends against the network; the writer trusts its own program. A
/// key is a constant of the record definition, never data, so a key above
/// fifteen is a compile-time mistake — and one that would shift into the next
/// field's bits and write a record nothing can read back. `#[derive(Colbin)]`
/// refuses such a type at compile time, which is where the check belongs.
pub struct Writer<'a> {
    /// The buffer being appended to. It is public because a nested run at the
    /// other key width writes onto the same bytes: both writers are a buffer and
    /// nothing else, so handing it over is a reborrow rather than a copy.
    pub buf: &'a mut Vec<u8>,
}

impl<'a> Writer<'a> {
    /// Starts a writer over a buffer, appending to whatever is already there.
    pub fn new(buf: &'a mut Vec<u8>) -> Self {
        Self { buf }
    }

    /// Writes an unsigned integer, and nothing at all when it is zero.
    ///
    /// 1..=4 are the nibble itself, one byte for the whole field; anything
    /// larger is its little-endian bytes behind a nibble that counts them. Zero
    /// has no inline code because the writer never writes one except behind a
    /// pointer, where it is the explicit zero of [`Writer::zero`].
    ///
    /// Zero is tested first even though it is below the inline range: an
    /// omitted field is the common case on a record the format is built to
    /// leave half empty, so it is the one that should cost a single compare.
    #[allow(clippy::cast_possible_truncation)]
    pub fn u64(&mut self, key: u8, value: u64) {
        if value == 0 {
            return;
        }
        if value <= u64::from(NIBBLE_INLINE_LAST) + 1 {
            self.buf.push(key << 4 | (value as u8 - 1));
            return;
        }
        self.sized(key, value);
    }

    /// Writes a value of 1..8 bytes behind the nibble that counts them, which
    /// is the unsigned form and a signed field's positive one.
    #[allow(clippy::cast_possible_truncation)]
    fn sized(&mut self, key: u8, value: u64) {
        let width = bytes_for_bits(bit_length(value));
        self.buf.push(key << 4 | (width as u8 + NIBBLE_SIZED_BIAS));
        append_magnitude(self.buf, value, width);
    }

    /// Writes a signed integer.
    ///
    /// A positive value costs what an unsigned one does, except that the inline
    /// codes stop at 3 to give −1 the fourth: −1 is the one negative common
    /// enough to deserve a byte. Every other negative is its magnitude in the
    /// length form, a byte more than the magnitude — the trade that keeps a
    /// signed field's positives, which are most of them, free of a sign bit.
    #[allow(clippy::cast_possible_truncation, clippy::cast_sign_loss)]
    pub fn i64(&mut self, key: u8, value: i64) {
        match value {
            0 => {}
            1..=3 => self.buf.push(key << 4 | (value as u8 - 1)),
            -1 => self.buf.push(key << 4 | NIBBLE_INLINE_LAST),
            4.. => self.sized(key, value as u64),
            _ => {
                // Negating through u64 rather than i64 keeps i64::MIN, whose
                // positive counterpart does not exist as an i64.
                let magnitude = (value as u64).wrapping_neg();
                let width = bytes_for_bits(bit_length(magnitude));
                self.buf.push(key << 4 | NIBBLE_LENGTH);
                self.buf.push(width as u8);
                append_magnitude(self.buf, magnitude, width);
            }
        }
    }

    /// Writes one byte when true and nothing when false. True is nibble 0, the
    /// unsigned value 1, which is a whole field in one byte.
    pub fn bool(&mut self, key: u8, value: bool) {
        if !value {
            return;
        }
        self.buf.push(key << 4);
    }

    /// Writes a `u16` field. The width-typed entry points mirror Go's and the
    /// wide writer's; under narrow keys a value's size depends only on the
    /// value, so they all take the one path.
    pub fn u16(&mut self, key: u8, value: u16) {
        self.u64(key, u64::from(value));
    }

    /// Writes a `u8` field.
    pub fn u8(&mut self, key: u8, value: u8) {
        self.u64(key, u64::from(value));
    }

    /// Writes a `u32` field.
    pub fn u32(&mut self, key: u8, value: u32) {
        self.u64(key, u64::from(value));
    }

    /// Writes a signed field no wider than four bytes.
    pub fn i32(&mut self, key: u8, value: i32) {
        self.i64(key, i64::from(value));
    }

    /// Writes a `f32` as its reversed bit pattern.
    ///
    /// The reversal is what makes the trim work at all. An integer's zero bytes
    /// are its most significant ones; a float's are its *least* significant — the
    /// low mantissa bits — while its exponent and sign are never zero. Reversing
    /// swaps the two ends and the integer path then works unchanged: 1.0 costs
    /// two bytes rather than five, and an `f64` holding a value that is exactly
    /// an `f32` costs five.
    pub fn f32(&mut self, key: u8, value: f32) {
        self.u64(key, u64::from(value.to_bits().swap_bytes()));
    }

    /// Writes an `f64` as its reversed bit pattern.
    pub fn f64(&mut self, key: u8, value: f64) {
        self.u64(key, value.to_bits().swap_bytes());
    }

    /// Writes a string, and nothing when it is empty.
    pub fn string(&mut self, key: u8, value: &str) {
        self.bytes(key, value.as_bytes());
    }

    /// Writes a string in the packed5 encoding when that is smaller than the raw
    /// bytes, and raw when it is not.
    ///
    /// The nibble has no `enc` field, so the encoding rides in the length
    /// form's flag, which also carries the one bit the schema cannot know: the
    /// case mode the unit stream opens in. A packed string therefore always
    /// pays for a length where a raw one of up to eight bytes does not — which
    /// is why the choice compares whole fields rather than payloads.
    pub fn packed_string(&mut self, key: u8, value: &str) {
        self.packed_bytes(key, value.as_bytes());
    }

    /// The same, for a caller holding UTF-8 it has not made a `str` of — which
    /// is every string the JSON encoder writes, since those are slices into the
    /// scanner's text arena.
    pub fn packed_bytes(&mut self, key: u8, value: &[u8]) {
        if value.is_empty() {
            return;
        }
        let Some((stream, upper)) = crate::packed5::payload(value) else {
            self.bytes(key, value);
            return;
        };
        // Packed is chosen only when strictly smaller, so turning packing on can
        // never make a message larger — and a tie keeps the raw form, which is
        // a sub-slice of the message on the way back in rather than a decode.
        if length_bytes(stream.len()) + stream.len() >= raw_size(value.len()) {
            self.bytes(key, value);
            return;
        }
        let flag = if upper {
            STRING_PACKED_UPPER
        } else {
            STRING_PACKED_LOWER
        };
        self.buf.push(key << 4 | NIBBLE_LENGTH | flag);
        append_length(self.buf, stream.len());
        self.buf.extend_from_slice(&stream);
    }

    /// Writes a blob, and nothing when it is empty. One of up to eight bytes is
    /// sized by its nibble and carries no length.
    #[allow(clippy::cast_possible_truncation)]
    pub fn bytes(&mut self, key: u8, value: &[u8]) {
        match value.len() {
            0 => return,
            size @ 1..=8 => self.buf.push(key << 4 | (size as u8 + NIBBLE_SIZED_BIAS)),
            size => {
                self.buf.push(key << 4 | NIBBLE_LENGTH | STRING_RAW);
                append_length(self.buf, size);
            }
        }
        self.buf.extend_from_slice(value);
    }

    /// Writes an array of integers at one width, chosen from the widest element,
    /// and nothing when the array is empty.
    ///
    /// The flag bits are the width and the length is the bytes, so the count is
    /// `length >> flag` and never stored.
    pub fn ints<T: Integer>(&mut self, key: u8, values: &[T]) {
        if values.is_empty() {
            return;
        }
        let (width, code) = array_plan(values);
        self.buf.push(key << 4 | NIBBLE_LENGTH | code);
        append_length(self.buf, values.len() * width);
        append_elements(self.buf, values, width);
    }

    /// Writes each element behind its own size, and nothing when the array is
    /// empty.
    ///
    /// There is no count: the reader recovers it by walking the sizes it has to
    /// walk anyway. The element size is one byte with an escape rather than a
    /// fixed two for the reason every length here escalates: it removes the
    /// ceiling, and the common element — anything under 255 bytes — costs one
    /// byte. Every size is known before a byte is written, so the field's
    /// length is too, and unlike a composite it needs no backpatch.
    #[allow(clippy::cast_possible_truncation)]
    pub fn strings<S: AsRef<[u8]>>(&mut self, key: u8, values: &[S]) {
        if values.is_empty() {
            return;
        }
        let length = values
            .iter()
            .map(|value| element_size_bytes(value.as_ref().len()) + value.as_ref().len())
            .sum();
        self.buf.push(key << 4 | NIBBLE_LENGTH);
        append_length(self.buf, length);
        for value in values {
            let value = value.as_ref();
            if value.len() <= INLINE_ELEMENT_SIZE {
                self.buf.push(value.len() as u8);
            } else {
                let size = u32::try_from(value.len()).expect(NARROW_FIELD_LIMIT);
                self.buf.push(ELEMENT_SIZE_ESCAPE);
                self.buf.extend_from_slice(&size.to_le_bytes());
            }
            self.buf.extend_from_slice(value);
        }
    }

    /// Writes a field the omit-zero rule would otherwise drop: the length form
    /// with nothing in it, `[key | 1100] 00`.
    ///
    /// It exists for optional fields. An absent key means `None`, so a `Some`
    /// holding a zero value has to put something on the wire or the two would be
    /// indistinguishable — which is the one place this format needs to say
    /// "zero" out loud rather than by omission. Every type reads this one form
    /// as its zero, so there is one writer for all of them rather than one per
    /// table of codes.
    pub fn zero(&mut self, key: u8) {
        self.buf.push(key << 4 | NIBBLE_LENGTH);
        self.buf.push(0);
    }

    // --- composites ----------------------------------------------------------

    /// Writes the length form's header and a one-byte length placeholder, which
    /// [`Writer::close`] patches once the body is written.
    fn open_composite(&mut self, key: u8, flag: u8) -> Mark {
        self.buf.push(key << 4 | NIBBLE_LENGTH | flag);
        self.buf.push(0);
        Mark {
            at: self.buf.len() - 1,
        }
    }

    /// Begins a nested key run under `key`, at four key bits inside.
    pub fn open_struct(&mut self, key: u8) -> Mark {
        self.open_composite(key, 0)
    }

    /// Begins a nested run whose own keys are eight bits, which is how a narrow
    /// struct holds a type that needs more than sixteen ids. The width is a
    /// property of the scope, so either may hold the other.
    pub fn open_struct_wide(&mut self, key: u8) -> Mark {
        self.open_composite(key, COMPOSITE_STRUCT_WIDE)
    }

    /// Begins a list of `count` elements under `key`. Each element is opened
    /// with [`Writer::open_element`], because a narrow element carries a length
    /// and no descriptor — its shape is the schema's to know.
    pub fn open_list(&mut self, key: u8, count: usize) -> Mark {
        let mark = self.open_composite(key, 0);
        append_count(self.buf, count);
        mark
    }

    /// Begins a map of `count` entries under `key`.
    pub fn open_map(&mut self, key: u8, count: usize) -> Mark {
        let mark = self.open_composite(key, 0);
        append_count(self.buf, count);
        mark
    }

    /// Begins a table of `rows` rows under `key`.
    pub fn open_table(&mut self, key: u8, rows: usize) -> Mark {
        let mark = self.open_composite(key, COMPOSITE_TABLE);
        append_count(self.buf, rows);
        mark
    }

    /// Begins one element of a narrow list: a length and a body, with no
    /// descriptor between them.
    pub fn open_element(&mut self) -> Mark {
        self.buf.push(0);
        Mark {
            at: self.buf.len() - 1,
        }
    }

    /// Patches a composite's length, widening the placeholder when the body
    /// outgrew it.
    ///
    /// Sizing the value first would cost a pass over every nested one; this way
    /// the common composite is one reserved byte and one store, and a body past
    /// 253 bytes makes room for the escape and its u16 or u32 by shifting the
    /// body up — a memmove on a buffer already in cache, and only for composites
    /// large enough not to care. The header in front is never touched: the
    /// length says its own width, so nothing else has to.
    #[allow(clippy::cast_possible_truncation)]
    pub fn close(&mut self, mark: Mark) {
        let body = self.buf.len() - (mark.at + 1);
        if body <= LENGTH_INLINE_MAX {
            self.buf[mark.at] = body as u8;
            return;
        }
        if body <= 0xFFFF {
            make_room(self.buf, mark, 2);
            self.buf[mark.at] = LENGTH_ESCAPE_U16;
            self.buf[mark.at + 1..mark.at + 3].copy_from_slice(&(body as u16).to_le_bytes());
            return;
        }
        let body = u32::try_from(body).expect(NARROW_FIELD_LIMIT);
        make_room(self.buf, mark, 4);
        self.buf[mark.at] = LENGTH_ESCAPE_U32;
        self.buf[mark.at + 1..mark.at + 5].copy_from_slice(&body.to_le_bytes());
    }

    /// Patches a narrow list element's length.
    ///
    /// It is not [`Writer::close`]: an element's length is the list element
    /// form, one byte up to 254 or `0xFF` and a u32, which [`Reader::element`]
    /// reads — not a field's length, whose one-byte form stops at 253. The two
    /// agree below 254 and nowhere above it, so a list whose element body
    /// reached 254 bytes would be written in one spelling and read in the
    /// other. That happened once already, when `close` widened by OR-ing into
    /// the byte before the placeholder, which for an element is the previous
    /// element's tail.
    ///
    /// Mirrors `Writer.CloseElement` in wire/narrow_composite.go.
    #[allow(clippy::cast_possible_truncation)]
    pub fn close_element(&mut self, mark: Mark) {
        let body = self.buf.len() - (mark.at + 1);
        if body <= INLINE_ELEMENT_SIZE {
            self.buf[mark.at] = body as u8;
            return;
        }
        let body = u32::try_from(body).expect(NARROW_FIELD_LIMIT);
        make_room(self.buf, mark, 4);
        self.buf[mark.at] = ELEMENT_SIZE_ESCAPE;
        self.buf[mark.at + 1..mark.at + 5].copy_from_slice(&body.to_le_bytes());
    }

    // Element writers, the key-less values a narrow map's entries are made of. A
    // narrow list's elements are whole key runs and go through open_element.
    //
    // They keep a code table of their own rather than the field nibble: an
    // element sits inside a composite whose length already steps over it, so it
    // does not need to size itself, and the denser table is what it can spend
    // that freedom on.

    /// Writes an unsigned map key or value: code 0..=7 is the value itself and
    /// 8..=15 is a magnitude of `code − 7` bytes.
    ///
    /// A map value of zero is one byte and needs no special case, because the
    /// omit-zero rule that keeps a field away from a zero code does not cover
    /// an element.
    #[allow(clippy::cast_possible_truncation)]
    pub fn element_uint(&mut self, value: u64) {
        if value <= UINT_INLINE_MAX {
            self.buf.push(value as u8);
            return;
        }
        let width = bit_length(value).div_ceil(8);
        self.buf.push(UINT_WIDTH_BASE + width as u8 - 1);
        append_magnitude(self.buf, value, width);
    }

    /// Writes the signed `[positive:1][size:3]` form. A positive value does not
    /// go through [`Writer::element_uint`]: the two codes are different tables,
    /// and [`Reader::element_int`] is what will read this back.
    #[allow(clippy::cast_sign_loss)]
    pub fn element_int(&mut self, value: i64) {
        if value >= 0 {
            let magnitude = value as u64;
            if magnitude == 0 {
                // Size code 0 means "the magnitude is one". A map value of zero is
                // legitimate, so it spends a byte saying so.
                self.buf.push(INT_POSITIVE_FLAG | 1);
                self.buf.push(0);
                return;
            }
            let (code, width) = size_code_for(magnitude);
            self.buf.push(INT_POSITIVE_FLAG | code);
            append_magnitude(self.buf, magnitude, width);
            return;
        }
        let magnitude = (value as u64).wrapping_neg();
        let (code, width) = size_code_for(magnitude);
        self.buf.push(code);
        append_magnitude(self.buf, magnitude, width);
    }

    /// Writes a string map key or value.
    pub fn element_string(&mut self, value: &str) {
        let (code, width) = length_code_for(value.len() as u64);
        self.buf.push(code);
        append_magnitude(self.buf, value.len() as u64, width);
        self.buf.extend_from_slice(value.as_bytes());
    }

    /// Writes an integer column under `key` for a narrow-keyed table, and
    /// nothing at all when every value is zero: an absent column key means a
    /// column of zeros. A column is the length form with the column codec as
    /// its body, so it skips like any other field.
    pub fn column<T: column::Signed + PartialEq>(&mut self, key: u8, values: &[T]) {
        if values.is_empty() || values.iter().all(|value| value.as_i64() == 0) {
            return;
        }
        let mark = self.open_composite(key, 0);
        column::append_array(self.buf, values);
        self.close(mark);
    }
}

/// What a raw blob of `size` bytes costs after its header byte: no length up to
/// eight bytes, a length past that.
const fn raw_size(size: usize) -> usize {
    if size <= 8 {
        size
    } else {
        length_bytes(size) + size
    }
}

/// What a string-array element's size occupies.
const fn element_size_bytes(size: usize) -> usize {
    if size <= INLINE_ELEMENT_SIZE { 1 } else { 5 }
}

/// Grows the one-byte placeholder at `mark` by `extra` bytes, shifting the body
/// up behind it.
fn make_room(buf: &mut Vec<u8>, mark: Mark, extra: usize) {
    let end = buf.len();
    buf.resize(end + extra, 0);
    buf.copy_within(mark.at + 1..end, mark.at + 1 + extra);
}

/// Walks a message field by field. The caller switches on [`Reader::key`] and
/// calls the read for the type that key holds, which it knows from the record
/// definition — or [`Reader::skip`] for a key it does not know.
///
/// Errors are sticky: the first failure parks the cursor at the end of the
/// buffer, so a decode loop ends rather than spinning, every later read answers
/// zero, and one check of [`Reader::err`] at the end covers the whole message.
pub struct Reader<'a> {
    buf: &'a [u8],
    at: usize,
    err: Option<Error>,
}

impl<'a> Reader<'a> {
    /// Starts a reader over one message.
    pub fn new(message: &'a [u8]) -> Self {
        Self {
            buf: message,
            at: 0,
            err: None,
        }
    }

    /// Reports whether another field follows. It does not test the error because
    /// [`Reader::fail`] parks the cursor at the end, so one comparison answers
    /// both questions.
    pub fn more(&self) -> bool {
        self.at < self.buf.len()
    }

    /// Where the cursor sits in the buffer this reader was given.
    #[must_use]
    #[inline]
    pub fn cursor(&self) -> usize {
        self.at
    }

    /// The buffer's length, so a span walk can turn a relative cursor into an
    /// absolute offset without exposing the slice.
    #[must_use]
    #[inline]
    pub fn buf_len(&self) -> usize {
        self.buf.len()
    }

    /// The field the cursor is on. It does not advance: the typed read does.
    /// Only meaningful while [`Reader::more`] reports true.
    pub fn key(&self) -> u8 {
        self.buf.get(self.at).map_or(0, |byte| byte >> 4)
    }

    /// The first failure. A decode must check it before using the record: every
    /// read answers zero after a failure, which is indistinguishable from a
    /// field that was legitimately omitted.
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

    /// Records an error from a sub-reader on its parent, which is what a caller
    /// descending into a composite needs: the child has its own cursor and its
    /// own error, and a failure inside it must stop the parent rather than be
    /// read past.
    pub fn fail_with(&mut self, result: Result<(), Error>) {
        if let Err(err) = result {
            self.fail(err);
        }
    }

    /// Reads the field at the cursor as its nibble and its payload, advancing
    /// past the whole of it. Every read starts here, which is what keeps the
    /// size of a field the nibble's business alone: a read can only disagree
    /// with [`Reader::skip`] about what the bytes mean, never about where the
    /// next field starts.
    fn field(&mut self) -> Option<(u8, &'a [u8])> {
        let Some(&header) = self.buf.get(self.at) else {
            self.fail(Error::Truncated);
            return None;
        };
        let nibble = header & 0b1111;
        let (size, start) = if nibble <= NIBBLE_INLINE_LAST {
            (0, self.at + 1)
        } else if nibble < NIBBLE_LENGTH {
            (usize::from(nibble - NIBBLE_SIZED_BIAS), self.at + 1)
        } else {
            self.length(self.at + 1)?
        };
        if size > self.buf.len() - start {
            self.fail(Error::Truncated);
            return None;
        }
        self.at = start + size;
        Some((nibble, &self.buf[start..start + size]))
    }

    /// Reads the length that begins at `at` and returns it with the offset just
    /// past it. Each of its three forms is a branch and a fixed-width load, so
    /// there is no continuation run for a peer to make unbounded.
    fn length(&mut self, at: usize) -> Option<(usize, usize)> {
        let Some(&first) = self.buf.get(at) else {
            self.fail(Error::Truncated);
            return None;
        };
        let (value, start) = match first {
            LENGTH_ESCAPE_U16 => {
                let Some(bytes) = self.buf.get(at + 1..at + 3) else {
                    self.fail(Error::Truncated);
                    return None;
                };
                (u64::from(u16::from_le_bytes([bytes[0], bytes[1]])), at + 3)
            }
            LENGTH_ESCAPE_U32 => {
                let Some(bytes) = self.buf.get(at + 1..at + 5) else {
                    self.fail(Error::Truncated);
                    return None;
                };
                (le_uint(bytes, 4), at + 5)
            }
            size => (u64::from(size), at + 1),
        };
        let Ok(size) = usize::try_from(value) else {
            self.fail(Error::SizeTooLarge);
            return None;
        };
        Some((size, start))
    }

    /// Steps over the field at the cursor without knowing what it is, and
    /// reports whether it could.
    ///
    /// The nibble sizes every field, so this is one branch and, for the length
    /// form, one length read — the same capability [`super::Reader8::skip`]
    /// gives a wide run, and what lets a type drop a field or gain one without
    /// stranding the rows already written. It does not look inside: a skipped
    /// composite is its length and nothing more, so a hostile message cannot
    /// recurse through it.
    pub fn skip(&mut self) -> bool {
        self.field().is_some()
    }

    /// Reads an unsigned integer field.
    pub fn u64(&mut self) -> u64 {
        let Some((nibble, payload)) = self.field() else {
            return 0;
        };
        match nibble {
            0..=NIBBLE_INLINE_LAST => u64::from(nibble) + 1,
            NIBBLE_LENGTH if payload.is_empty() => 0,
            _ if nibble < NIBBLE_LENGTH => le_uint(payload, payload.len()),
            // A length form with a payload is a negative number or a string,
            // and neither is something an unsigned field can hold.
            _ => {
                self.fail(Error::BadDescriptor);
                0
            }
        }
    }

    /// Reads a signed integer field.
    ///
    /// Nibbles 0..=2 are 1..=3 and nibble 3 is −1; a sized nibble is a positive
    /// magnitude and the length form a negative one. A magnitude past what an
    /// `i64` holds is refused rather than wrapped into a different number.
    pub fn i64(&mut self) -> i64 {
        let Some((nibble, payload)) = self.field() else {
            return 0;
        };
        let (positive, magnitude) = match nibble {
            NIBBLE_INLINE_LAST => return -1,
            0..NIBBLE_INLINE_LAST => return i64::from(nibble) + 1,
            NIBBLE_LENGTH if payload.is_empty() => return 0,
            NIBBLE_LENGTH if payload.len() <= 8 => (false, le_uint(payload, payload.len())),
            NIBBLE_LENGTH => {
                self.fail(Error::FieldTooWide);
                return 0;
            }
            _ if nibble < NIBBLE_LENGTH => (true, le_uint(payload, payload.len())),
            _ => {
                self.fail(Error::BadDescriptor);
                return 0;
            }
        };
        match super::signed(positive, magnitude) {
            Some(value) => value,
            None => {
                self.fail(Error::FieldTooWide);
                0
            }
        }
    }

    /// Reads a field written by [`Writer::bool`]. An absent field never reaches
    /// here: a false bool is not written, so the key is simply missing.
    pub fn bool(&mut self) -> bool {
        self.u64() == 1
    }

    /// Reads a `u16` field, refusing anything wider — a schema disagreement
    /// rather than a value to truncate into something plausible.
    #[allow(clippy::cast_possible_truncation)]
    pub fn u16(&mut self) -> u16 {
        let value = self.u64();
        if value > 0xFFFF {
            self.fail(Error::FieldTooWide);
            return 0;
        }
        value as u16
    }

    /// Reads a `u8` field, refusing anything wider.
    #[allow(clippy::cast_possible_truncation)]
    pub fn u8(&mut self) -> u8 {
        let value = self.u64();
        if value > 0xFF {
            self.fail(Error::FieldTooWide);
            return 0;
        }
        value as u8
    }

    /// Reads a `u32` field, refusing anything wider.
    #[allow(clippy::cast_possible_truncation)]
    pub fn u32(&mut self) -> u32 {
        let value = self.u64();
        if value > 0xFFFF_FFFF {
            self.fail(Error::FieldTooWide);
            return 0;
        }
        value as u32
    }

    /// Reads a field written by [`Writer::i32`], refusing anything wider.
    pub fn i32(&mut self) -> i32 {
        let value = self.i64();
        self.within(value)
    }

    /// Reads an `i16` field, refusing anything wider.
    pub fn i16(&mut self) -> i16 {
        let value = self.i64();
        self.within(value)
    }

    /// Reads an `i8` field, refusing anything wider.
    pub fn i8(&mut self) -> i8 {
        let value = self.i64();
        self.within(value)
    }

    /// Narrows a signed value to its field's type, or fails the read: a value
    /// past the type is a schema disagreement, not one to truncate.
    fn within<T: TryFrom<i64> + Default>(&mut self, value: i64) -> T {
        T::try_from(value).unwrap_or_else(|_| {
            self.fail(Error::FieldTooWide);
            T::default()
        })
    }

    /// Reads a field written by [`Writer::f32`]. Its reversed bits are a `u32`,
    /// so a field wider than four bytes is refused rather than truncated into a
    /// different float.
    pub fn f32(&mut self) -> f32 {
        f32::from_bits(self.u32().swap_bytes())
    }

    /// Reads a field written by [`Writer::f64`].
    pub fn f64(&mut self) -> f64 {
        f64::from_bits(self.u64().swap_bytes())
    }

    /// Returns the field's bytes as a sub-slice of the message, without copying.
    ///
    /// A packed string is not bytes — its payload is a unit stream — and is
    /// refused here rather than handed over as though it were the text;
    /// [`Reader::packed_string`] reads it.
    pub fn bytes(&mut self) -> &'a [u8] {
        match self.string_span() {
            Some((payload, None)) => payload,
            Some((_, Some(_))) => {
                self.fail(Error::BadEscape);
                &[]
            }
            None => &[],
        }
    }

    /// Copies the field into a `String`, failing rather than replacing bytes
    /// that are not UTF-8.
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

    /// The payload of a string field of either encoding, and whether it is
    /// packed in upper case. Advances past the field.
    ///
    /// A string is never inline — it has at least one byte, or it is the empty
    /// length form of an explicit zero — so nibbles 0..=3 are refused, as is the
    /// unassigned flag `11`.
    fn string_span(&mut self) -> Option<(&'a [u8], Option<bool>)> {
        let (nibble, payload) = self.field()?;
        if nibble <= NIBBLE_INLINE_LAST {
            self.fail(Error::BadDescriptor);
            return None;
        }
        if nibble < NIBBLE_LENGTH {
            return Some((payload, None));
        }
        match nibble & FLAG_MASK {
            STRING_RAW => Some((payload, None)),
            STRING_PACKED_LOWER => Some((payload, Some(false))),
            STRING_PACKED_UPPER => Some((payload, Some(true))),
            _ => {
                self.fail(Error::BadEscape);
                None
            }
        }
    }

    /// Reads a string written by [`Writer::packed_string`] or by
    /// [`Writer::string`]. The header's flag says which, so a reader needs no
    /// configuration and cannot be wrong about it.
    pub fn packed_string(&mut self) -> String {
        let Some((payload, upper)) = self.string_span() else {
            return String::new();
        };
        let Some(upper) = upper else {
            return match core::str::from_utf8(payload) {
                Ok(value) => value.to_owned(),
                Err(_) => {
                    self.fail(Error::NotUtf8);
                    String::new()
                }
            };
        };
        let mut bytes = Vec::new();
        match crate::packed5::append_string(&mut bytes, payload, upper) {
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

    /// Appends an integer array field's elements to `dst`, which may be empty.
    /// The element type says the form: two's complement for a signed one,
    /// magnitudes for an unsigned one.
    pub fn ints_into<T: Integer>(&mut self, dst: &mut Vec<T>) {
        self.ints_into_as(dst, T::SIGNED_TYPE, size_of::<T>());
    }

    /// [`Reader::ints_into`] for a caller whose element type is not `T`: a walk
    /// that has only a schema op reads every array into `i64`, and the op is
    /// what says whether the elements are signed and how wide they may be.
    ///
    /// An element wider than `max_width` bytes is refused rather than truncated,
    /// and so is a length that is not a whole number of elements.
    pub fn ints_into_as<T: Integer>(&mut self, dst: &mut Vec<T>, signed: bool, max_width: usize) {
        let Some((nibble, elements)) = self.field() else {
            return;
        };
        if nibble < NIBBLE_LENGTH {
            self.fail(Error::BadDescriptor);
            return;
        }
        let width = 1_usize << (nibble & FLAG_MASK);
        if width > max_width {
            self.fail(Error::FieldTooWide);
            return;
        }
        if elements.len() % width != 0 {
            self.fail(Error::Truncated);
            return;
        }
        append_array(dst, elements, width, !signed);
    }

    /// Reads an integer array field.
    pub fn ints<T: Integer>(&mut self) -> Vec<T> {
        let mut dst = Vec::new();
        self.ints_into(&mut dst);
        dst
    }

    /// Returns each element of a string array as a sub-slice of the message,
    /// without copying.
    ///
    /// The array carries no count; the elements are whatever its length holds,
    /// walked size by size. An element running past the length is refused
    /// rather than read from the field after it.
    pub fn strings_bytes(&mut self) -> Vec<&'a [u8]> {
        let mut dst = Vec::new();
        let Some((nibble, payload)) = self.field() else {
            return dst;
        };
        if nibble != NIBBLE_LENGTH {
            self.fail(Error::BadDescriptor);
            return dst;
        }
        let mut at = 0;
        while at < payload.len() {
            let Some((size, next)) = self.element_size(payload, at) else {
                return dst;
            };
            if size > payload.len() - next {
                self.fail(Error::Truncated);
                return dst;
            }
            dst.push(&payload[next..next + size]);
            at = next + size;
        }
        dst
    }

    /// Reads one string-array element size inside `payload`: one byte, or four
    /// more behind the escape.
    fn element_size(&mut self, payload: &[u8], at: usize) -> Option<(usize, usize)> {
        let Some(&size) = payload.get(at) else {
            self.fail(Error::Truncated);
            return None;
        };
        if size != ELEMENT_SIZE_ESCAPE {
            return Some((usize::from(size), at + 1));
        }
        let Some(bytes) = payload.get(at + 1..at + 5) else {
            self.fail(Error::Truncated);
            return None;
        };
        let Ok(size) = usize::try_from(le_uint(bytes, 4)) else {
            self.fail(Error::SizeTooLarge);
            return None;
        };
        Some((size, at + 5))
    }

    /// Copies each element of a string array into a `String`.
    pub fn strings(&mut self) -> Vec<String> {
        let mut dst = Vec::new();
        self.strings_into(&mut dst);
        dst
    }

    /// Appends each element of a string array onto `dst`, reusing its capacity.
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

    // --- composites ----------------------------------------------------------

    /// Reads a composite, the length form with flags `accept` allows, and
    /// returns its flags and body, advancing past the whole field. Anything
    /// else — a value form, or a flag the shape does not assign, such as `f1` —
    /// is refused rather than read as a body it is not.
    fn composite(&mut self, accept: impl Fn(u8) -> bool) -> Option<(u8, &'a [u8])> {
        let (nibble, body) = self.field()?;
        if nibble & !FLAG_MASK != NIBBLE_LENGTH || !accept(nibble & FLAG_MASK) {
            self.fail(Error::BadDescriptor);
            return None;
        }
        Some((nibble & FLAG_MASK, body))
    }

    /// Returns a nested run's bytes and the key width it uses, advancing past
    /// the whole field. The caller reads the body with [`Reader::new`] or
    /// [`super::Reader8::new`] accordingly — the per-scope key width, spent
    /// where it is decided.
    pub fn struct_body(&mut self) -> Option<(&'a [u8], bool)> {
        let (flag, body) = self.composite(|flag| flag <= COMPOSITE_STRUCT_WIDE)?;
        Some((body, flag == COMPOSITE_STRUCT_WIDE))
    }

    /// Reports whether the field at the cursor is a table rather than a list. A
    /// writer chooses between them on the row count, so a reader has to ask.
    pub fn is_table(&self) -> bool {
        self.buf
            .get(self.at)
            .is_some_and(|header| header & 0b1111 == NIBBLE_LENGTH | COMPOSITE_TABLE)
    }

    /// Returns the element count of a list or a map and a reader over what
    /// follows it.
    pub fn counted(&mut self) -> Option<(usize, Reader<'a>)> {
        let (_, body) = self.composite(|flag| flag == 0)?;
        let Some((count, at)) = read_count(body) else {
            self.fail(Error::Truncated);
            return None;
        };
        Some((count, Reader::new(&body[at..])))
    }

    /// Returns a table's row count and its columns' bytes, which the caller
    /// reads at the row type's key width: four bits when that type's are, and
    /// eight when the rows are wide, whatever this run's width is.
    pub fn table(&mut self) -> Option<(usize, &'a [u8])> {
        let (_, body) = self.composite(|flag| flag == COMPOSITE_TABLE)?;
        let Some((rows, at)) = read_count(body) else {
            self.fail(Error::Truncated);
            return None;
        };
        Some((rows, &body[at..]))
    }

    /// Returns one narrow list element's body: a length and then a key run.
    pub fn element(&mut self) -> Option<&'a [u8]> {
        let Some(first) = self.buf.get(self.at) else {
            self.fail(Error::Truncated);
            return None;
        };
        let mut size = usize::from(*first);
        let mut start = self.at + 1;
        if *first == ELEMENT_SIZE_ESCAPE {
            let Some(bytes) = self.buf.get(self.at + 1..self.at + 5) else {
                self.fail(Error::Truncated);
                return None;
            };
            let Ok(wide) = usize::try_from(le_uint(bytes, 4)) else {
                self.fail(Error::SizeTooLarge);
                return None;
            };
            size = wide;
            start = self.at + 5;
        }
        if size > self.buf.len() - start {
            self.fail(Error::Truncated);
            return None;
        }
        self.at = start + size;
        Some(&self.buf[start..start + size])
    }

    /// The code byte at the cursor of a key-less element, or `None` at the end.
    fn element_code(&mut self) -> Option<u8> {
        match self.buf.get(self.at) {
            Some(byte) => Some(*byte),
            None => {
                self.fail(Error::Truncated);
                None
            }
        }
    }

    /// Reads an unsigned map key or value.
    pub fn element_uint(&mut self) -> u64 {
        let Some(code) = self.element_code() else {
            return 0;
        };
        let code = u64::from(code & 0b1111);
        if code <= UINT_INLINE_MAX {
            self.at += 1;
            return code;
        }
        #[allow(clippy::cast_possible_truncation)]
        let width = code as usize - usize::from(UINT_WIDTH_BASE) + 1;
        let rest = &self.buf[self.at + 1..];
        if rest.len() < width {
            self.fail(Error::Truncated);
            return 0;
        }
        self.at += 1 + width;
        le_uint(rest, width)
    }

    /// Reads a signed map key or value: `[positive:1][size:3]`, where size code
    /// 0 means the magnitude is one, 1..=6 are that many bytes and 7 is eight.
    /// It is its own table rather than [`Reader::element_uint`]'s, so the same
    /// four bits mean different things and only the schema says which.
    #[allow(clippy::cast_possible_wrap)]
    pub fn element_int(&mut self) -> i64 {
        let Some(code) = self.element_code() else {
            return 0;
        };
        let positive = code & INT_POSITIVE_FLAG != 0;
        let width = MAGNITUDE_WIDTH[usize::from(code & 0b111)];
        let rest = &self.buf[self.at + 1..];
        if rest.len() < width {
            self.fail(Error::Truncated);
            return 0;
        }
        self.at += 1 + width;
        let magnitude = if width == 0 { 1 } else { le_uint(rest, width) };
        if positive {
            return magnitude as i64;
        }
        (magnitude as i64).wrapping_neg()
    }

    /// Reads a string map key or value.
    pub fn element_string(&mut self) -> String {
        let Some(code) = self.element_code() else {
            return String::new();
        };
        let width = LENGTH_WIDTH[usize::from(code & 0b11)];
        let rest = &self.buf[self.at + 1..];
        if rest.len() < width {
            self.fail(Error::Truncated);
            return String::new();
        }
        let size = le_uint(rest, width);
        let start = self.at + 1 + width;
        let Ok(size) = usize::try_from(size) else {
            self.fail(Error::SizeTooLarge);
            return String::new();
        };
        if size > self.buf.len() - start {
            self.fail(Error::Truncated);
            return String::new();
        }
        self.at = start + size;
        match core::str::from_utf8(&self.buf[start..start + size]) {
            Ok(value) => value.to_owned(),
            Err(_) => {
                self.fail(Error::NotUtf8);
                String::new()
            }
        }
    }

    /// Reports whether another key-less value follows.
    pub fn more_elements(&self) -> bool {
        self.err.is_none() && self.at < self.buf.len()
    }

    /// Reads a column written by [`Writer::column`] into `dst`. The row count
    /// comes from the table, which said it once for every column.
    pub fn column<T: column::Signed>(&mut self, rows: usize, dst: &mut Vec<T>) {
        let Some((_, body)) = self.composite(|flag| flag == 0) else {
            return;
        };
        dst.clear();
        dst.resize(rows, T::from_i64(0));
        if let Err(err) = column::decode_array(body, rows, dst) {
            self.fail(err);
        }
    }
}
