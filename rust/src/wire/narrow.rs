//! The four-bit key width, K4: the key shares one byte with the descriptor and
//! the field's class comes from the schema.
//!
//! This is the default and the fast path. What it gives up is the one thing four
//! descriptor bits cannot carry: a class, and so the ability to step over a
//! field whose key the reader has never heard of. [`Reader::skip`] refuses;
//! [`super::Reader8::skip`] works.
//!
//! Composites are here too. A narrow descriptor has no room for a class and does
//! not need one — a narrow reader has the schema — so what it takes from the
//! wire is the same byte length the wide form uses, in the same four detail
//! bits:
//!
//! ```text
//! struct  [key:4][k8:1][—:1][lw:2]     [len: lw] [key run]
//! list    [key:4][homog:1][—:1][lw:2]  [len: lw] [count] ( [len] [body] )*
//! map     [key:4][sub=0:1][—:1][lw:2]  [len: lw] [count] ( key value )*
//! table   [key:4][sub=1:1][k8:1][lw:2] [len: lw] [rows]  ( [key][desc][column] )*
//! ```

use super::{
    ARRAY_POSITIVE_FLAG, ARRAY_WIDTH_SHIFT, ELEMENT_SIZE_ESCAPE, ESCAPE_2BYTES, ESCAPE_4BYTES,
    ESCAPE_8BYTES, INLINE_ARRAY_COUNT, INLINE_BLOB_SIZE, INLINE_COMPOSITE_LENGTH,
    INLINE_ELEMENT_SIZE, INT_POSITIVE_FLAG, Integer, LENGTH_WIDTH, MAGNITUDE_WIDTH,
    MORE_ARRAY_LEN_FLAG, MORE_SIZE_FLAG, Mark, SIZE_CODE_ONE, UINT_INLINE_MAX, UINT_WIDTH_BASE,
    append_array, append_count, append_elements, append_magnitude, array_plan_of, bit_length,
    le_uint, length_code_for, read_count, size_code_for,
};
use crate::Error;
use crate::column;

/// Appends fields with four-bit keys onto a buffer the caller owns.
///
/// # A write cannot fail
///
/// There is no error to check. Sizes escalate rather than cap, so nothing a
/// caller can hold in memory is too large to describe.
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
    /// Zero is tested first even though it is inside the inline range: folding
    /// it into the inline test saves a compare on a small non-zero value and
    /// spends one on every omitted field, which is the common case on a record
    /// the format is built to leave half empty. Go measured 7.6 ns against 6.5
    /// for that order on a ten-field record; LLVM is indifferent here.
    ///
    /// `wire/narrow.go` then goes further and folds the two compares into
    /// `value-1 < uintInlineMax`, and hides the wide half behind
    /// `//go:noinline`, to stay inside Go's inline budget. Neither is worth
    /// copying: this compiles to the same code either way, and the plain
    /// spelling is the one that says what it means. See RATIONALE.md, "The
    /// inline budget is part of the format's speed".
    #[allow(clippy::cast_possible_truncation)]
    pub fn u64(&mut self, key: u8, value: u64) {
        if value == 0 {
            return;
        }
        if value <= UINT_INLINE_MAX {
            self.buf.push(key << 4 | value as u8);
            return;
        }
        if value <= 0xFF {
            self.buf.push(key << 4 | UINT_WIDTH_BASE);
            self.buf.push(value as u8);
            return;
        }
        self.uint_wide(key, value);
    }

    #[allow(clippy::cast_possible_truncation)]
    fn uint_wide(&mut self, key: u8, value: u64) {
        let width = bit_length(value).div_ceil(8);
        self.buf
            .push(key << 4 | (UINT_WIDTH_BASE + width as u8 - 1));
        append_magnitude(self.buf, value, width);
    }

    /// Writes a signed integer as a sign bit and a magnitude.
    #[allow(clippy::cast_possible_truncation)]
    pub fn i64(&mut self, key: u8, value: i64) {
        if value > 0 && value <= 0xFF {
            self.buf.push(key << 4 | INT_POSITIVE_FLAG | 1);
            self.buf.push(value as u8);
            return;
        }
        if value == 0 {
            return;
        }
        self.int_wide(key, value);
    }

    #[allow(clippy::cast_sign_loss)]
    fn int_wide(&mut self, key: u8, value: i64) {
        let mut header = key << 4 | INT_POSITIVE_FLAG;
        let mut magnitude = value as u64;
        if value < 0 {
            header = key << 4;
            // Negating through u64 rather than i64 keeps i64::MIN, whose positive
            // counterpart does not exist as an i64.
            magnitude = (value as u64).wrapping_neg();
        }
        self.magnitude(header, magnitude);
    }

    /// Writes one byte when true and nothing when false. True is the unsigned
    /// inline value 1, which is a whole field in one byte.
    pub fn bool(&mut self, key: u8, value: bool) {
        if !value {
            return;
        }
        self.buf.push(key << 4 | 1);
    }

    /// Writes the header and as many bytes as the value actually needs.
    fn magnitude(&mut self, header: u8, magnitude: u64) {
        if magnitude == 1 {
            self.buf.push(header | SIZE_CODE_ONE);
            return;
        }
        let (code, width) = size_code_for(magnitude);
        self.buf.push(header | code);
        append_magnitude(self.buf, magnitude, width);
    }

    /// Writes a field whose type cannot exceed two bytes.
    ///
    /// The width-typed entry points exist because a field's Go or Rust type
    /// already fixes how wide it can be: a `u16` is one byte or two and never
    /// three.
    #[allow(clippy::cast_possible_truncation)]
    pub fn u16(&mut self, key: u8, value: u16) {
        if value == 0 {
            return;
        }
        if u64::from(value) <= UINT_INLINE_MAX {
            self.buf.push(key << 4 | value as u8);
            return;
        }
        if value <= 0xFF {
            self.buf.push(key << 4 | UINT_WIDTH_BASE);
            self.buf.push(value as u8);
            return;
        }
        self.buf.push(key << 4 | (UINT_WIDTH_BASE + 1));
        self.buf.extend_from_slice(&value.to_le_bytes());
    }

    /// Writes a `u8` field, which travels in the `u16` shape exactly as it does
    /// in Go.
    pub fn u8(&mut self, key: u8, value: u8) {
        self.u16(key, u16::from(value));
    }

    /// Writes a field whose type cannot exceed four bytes.
    #[allow(clippy::cast_possible_truncation)]
    pub fn u32(&mut self, key: u8, value: u32) {
        if value == 0 {
            return;
        }
        if u64::from(value) <= UINT_INLINE_MAX {
            self.buf.push(key << 4 | value as u8);
            return;
        }
        if value <= 0xFF {
            self.buf.push(key << 4 | UINT_WIDTH_BASE);
            self.buf.push(value as u8);
            return;
        }
        self.u32_wide(key, value);
    }

    #[allow(clippy::cast_possible_truncation)]
    fn u32_wide(&mut self, key: u8, value: u32) {
        if value <= 0xFFFF {
            self.buf.push(key << 4 | (UINT_WIDTH_BASE + 1));
            self.buf.extend_from_slice(&(value as u16).to_le_bytes());
        } else if value <= 0x00FF_FFFF {
            self.buf.push(key << 4 | (UINT_WIDTH_BASE + 2));
            self.buf.extend_from_slice(&value.to_le_bytes()[..3]);
        } else {
            self.buf.push(key << 4 | (UINT_WIDTH_BASE + 3));
            self.buf.extend_from_slice(&value.to_le_bytes());
        }
    }

    /// Writes a signed field no wider than four bytes.
    #[allow(clippy::cast_possible_truncation)]
    pub fn i32(&mut self, key: u8, value: i32) {
        if value > 0 && value <= 0xFF {
            self.buf.push(key << 4 | INT_POSITIVE_FLAG | 1);
            self.buf.push(value as u8);
            return;
        }
        if value == 0 {
            return;
        }
        self.int_wide(key, i64::from(value));
    }

    /// Writes a `f32` as its reversed bit pattern.
    ///
    /// The reversal is what makes the trim work at all. An integer's zero bytes
    /// are its most significant ones; a float's are its *least* significant — the
    /// low mantissa bits — while its exponent and sign are never zero. Reversing
    /// swaps the two ends and the integer path then works unchanged: 1.0 costs
    /// two bytes rather than eight, and an `f64` holding a value that is exactly
    /// an `f32` costs five.
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

    /// Writes the header a string, blob or string array carries: two bytes
    /// holding an eleven-bit size, or one byte and a wider size when that will
    /// not hold it.
    #[allow(clippy::cast_possible_truncation)]
    fn blob_header(&mut self, key: u8, size: usize) {
        if size <= INLINE_BLOB_SIZE {
            self.buf.push(key << 4 | ((size >> 8) as u8 & 0b111));
            self.buf.push(size as u8);
            return;
        }
        self.escaped_size(key, size as u64);
    }

    /// Writes the one-byte header whose three size bits name the width of the
    /// size that follows it.
    #[allow(clippy::cast_possible_truncation)]
    fn escaped_size(&mut self, key: u8, size: u64) {
        if size <= 0xFFFF {
            self.buf.push(key << 4 | MORE_SIZE_FLAG | ESCAPE_2BYTES);
            self.buf.extend_from_slice(&(size as u16).to_le_bytes());
        } else if size <= 0xFFFF_FFFF {
            self.buf.push(key << 4 | MORE_SIZE_FLAG | ESCAPE_4BYTES);
            self.buf.extend_from_slice(&(size as u32).to_le_bytes());
        } else {
            self.buf.push(key << 4 | MORE_SIZE_FLAG | ESCAPE_8BYTES);
            self.buf.extend_from_slice(&size.to_le_bytes());
        }
    }

    /// Writes an array of integers at one width, chosen from the widest element,
    /// and nothing when the array is empty.
    pub fn ints<T: Integer>(&mut self, key: u8, values: &[T]) {
        if values.is_empty() {
            return;
        }
        let (all_positive, width, width_code) = array_plan_of(values);
        let mut header = key << 4 | width_code << ARRAY_WIDTH_SHIFT;
        if all_positive {
            header |= ARRAY_POSITIVE_FLAG;
        }
        self.array_header(header, values.len());
        append_elements(self.buf, values, width);
    }

    /// Writes two bytes holding an eight-bit count, or one byte and a four-byte
    /// count when that will not hold it.
    #[allow(clippy::cast_possible_truncation)]
    fn array_header(&mut self, header: u8, count: usize) {
        if count <= INLINE_ARRAY_COUNT {
            self.buf.push(header);
            self.buf.push(count as u8);
            return;
        }
        self.buf.push(header | MORE_ARRAY_LEN_FLAG);
        self.buf.extend_from_slice(&(count as u32).to_le_bytes());
    }

    /// Writes a count and then each element behind its own length.
    ///
    /// The element length is one byte with an escape rather than a fixed two for
    /// the same reason the header sizes escalate: it removes the ceiling, and it
    /// makes the common element — anything under 255 bytes — cost one byte.
    #[allow(clippy::cast_possible_truncation)]
    pub fn strings<S: AsRef<str>>(&mut self, key: u8, values: &[S]) {
        if values.is_empty() {
            return;
        }
        self.blob_header(key, values.len());
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

    // Zero and EmptyString write a field the omit-zero rule would otherwise drop.
    //
    // They exist for optional fields. An absent key means `None`, so a `Some`
    // holding a zero value has to put something on the wire or the two would be
    // indistinguishable — which is the one place this format needs to say "zero"
    // out loud rather than by omission.

    /// Writes an explicit zero in the unsigned form, which is what
    /// [`Reader::u64`], [`Reader::bool`], [`Reader::f32`] and [`Reader::f64`]
    /// all read. It is a single byte: the unsigned nibble carries 0..=7
    /// outright.
    pub fn zero(&mut self, key: u8) {
        self.buf.push(key << 4);
    }

    /// Writes an explicit zero in the signed form, for a field [`Reader::i64`]
    /// will read. The two nibbles are different tables, so a zero has to be
    /// written in the one its reader will use.
    pub fn zero_signed(&mut self, key: u8) {
        self.buf.push(key << 4 | INT_POSITIVE_FLAG | 1);
        self.buf.push(0);
    }

    /// Writes a blob of no bytes.
    pub fn empty_string(&mut self, key: u8) {
        self.buf.push(key << 4);
        self.buf.push(0);
    }

    // --- composites ----------------------------------------------------------

    /// Writes a key, a detail nibble and a one-byte length placeholder, exactly
    /// as the wide form does minus the class.
    fn open_narrow_composite(&mut self, key: u8, detail: u8) -> Mark {
        self.buf.push(key << 4 | detail);
        self.buf.push(0);
        Mark {
            at: self.buf.len() - 1,
        }
    }

    /// Begins a nested key run under `key`, at four key bits inside.
    pub fn open_struct(&mut self, key: u8) -> Mark {
        self.open_narrow_composite(key, 0)
    }

    /// Begins a nested run whose own keys are eight bits, which is how a narrow
    /// struct holds a type that needs more than sixteen ids. The width is a
    /// property of the scope, so either may hold the other.
    pub fn open_struct_wide(&mut self, key: u8) -> Mark {
        self.open_narrow_composite(key, super::wide::STRUCT_WIDE_KEYS)
    }

    /// Begins a list of `count` elements under `key`. Each element is opened
    /// with [`Writer::open_element`], because a narrow element carries a length
    /// and no descriptor — its shape is the schema's to know.
    pub fn open_list(&mut self, key: u8, count: usize) -> Mark {
        let mark = self.open_narrow_composite(key, 0);
        append_count(self.buf, count);
        mark
    }

    /// Begins a map of `count` entries under `key`.
    pub fn open_map(&mut self, key: u8, count: usize) -> Mark {
        let mark = self.open_narrow_composite(key, 0);
        append_count(self.buf, count);
        mark
    }

    /// Begins a table of `rows` rows under `key`, its columns keyed at four
    /// bits.
    pub fn open_table(&mut self, key: u8, rows: usize) -> Mark {
        let mark = self.open_narrow_composite(key, super::wide::TABLE_FLAG);
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
    /// 255 bytes makes room for four by shifting what follows — a memmove on a
    /// buffer already in cache, and only for composites large enough not to
    /// care.
    #[allow(clippy::cast_possible_truncation)]
    pub fn close(&mut self, mark: Mark) {
        let body = self.buf.len() - (mark.at + 1);
        if body < INLINE_COMPOSITE_LENGTH {
            self.buf[mark.at] = body as u8;
            return;
        }
        widen_length(self.buf, mark, body);
    }

    // Element writers, the key-less values a narrow map's entries are made of. A
    // narrow list's elements are whole key runs and go through open_element.

    /// Writes an unsigned map key or value.
    ///
    /// The unsigned nibble carries 0..=7 outright, so a map value of zero is one
    /// byte and needs no special case — where the signed form's code 0 means
    /// "the value is one", and a map value is not a field, so the omit-zero rule
    /// that keeps a field away from that code does not cover it.
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
    /// go through [`Writer::element_uint`]: the two nibbles are different tables,
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
    /// column of zeros.
    pub fn column<T: column::Signed + PartialEq>(&mut self, key: u8, values: &[T]) {
        if values.is_empty() || values.iter().all(|value| value.as_i64() == 0) {
            return;
        }
        let mark = self.open_narrow_composite(key, 0);
        column::append_array(self.buf, values);
        self.close(mark);
    }

    /// Writes a string column, which is a list of blobs under the column's key.
    /// Strings have no residual to transform, so a column of them is the same
    /// shape a list of them is.
    pub fn string_column<S: AsRef<str>>(&mut self, key: u8, values: &[S]) {
        if values.iter().all(|value| value.as_ref().is_empty()) {
            return;
        }
        self.strings(key, values);
    }
}

/// Turns a one-byte length placeholder into four, shifting the body up to make
/// room. Shared by both key widths, which spell the placeholder the same way.
#[allow(clippy::cast_possible_truncation)]
pub(crate) fn widen_length(buf: &mut Vec<u8>, mark: Mark, body: usize) {
    buf.extend_from_slice(&[0, 0, 0]);
    let end = buf.len() - 3;
    buf.copy_within(mark.at + 1..end, mark.at + 4);
    buf[mark.at..mark.at + 4].copy_from_slice(&(body as u32).to_le_bytes());
    // lw code 2 is a four-byte length, in the descriptor's low two bits.
    buf[mark.at - 1] |= 2;
}

/// Walks a message field by field. The caller switches on [`Reader::key`] and
/// calls the read for the type that key holds, which it knows from the record
/// definition.
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

    /// The header byte at the cursor, or `None` at the end of the message.
    fn header(&mut self) -> Option<u8> {
        match self.buf.get(self.at) {
            Some(byte) => Some(*byte),
            None => {
                self.fail(Error::Truncated);
                None
            }
        }
    }

    /// Reads an unsigned integer field.
    pub fn u64(&mut self) -> u64 {
        let Some(header) = self.header() else {
            return 0;
        };
        let code = u64::from(header & 0b1111);
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

    /// Reads a signed integer field.
    ///
    /// The magnitude goes through [`Reader::signed_magnitude`] and not through
    /// [`Reader::u64`]: a signed nibble is `[positive:1][size:3]` and an
    /// unsigned one is a sixteen-code table, so the same four bits mean
    /// different things and only the schema says which.
    #[allow(clippy::cast_possible_wrap)]
    pub fn i64(&mut self) -> i64 {
        let Some(header) = self.header() else {
            return 0;
        };
        let positive = header & INT_POSITIVE_FLAG != 0;
        let magnitude = self.signed_magnitude();
        if positive {
            return magnitude as i64;
        }
        (magnitude as i64).wrapping_neg()
    }

    /// Reads the `[size:3]` form: code 0 means the magnitude is one, codes
    /// 1..=6 are that many bytes, and code 7 is eight.
    fn signed_magnitude(&mut self) -> u64 {
        let Some(header) = self.header() else {
            return 0;
        };
        let width = MAGNITUDE_WIDTH[usize::from(header & 0b111)];
        if width == 0 {
            self.at += 1;
            return 1;
        }
        let rest = &self.buf[self.at + 1..];
        if rest.len() < width {
            self.fail(Error::Truncated);
            return 0;
        }
        self.at += 1 + width;
        le_uint(rest, width)
    }

    /// Reads a field written by [`Writer::bool`]. An absent field never reaches
    /// here: a false bool is not written, so the key is simply missing.
    pub fn bool(&mut self) -> bool {
        self.u64() == 1
    }

    /// Reads a field written by [`Writer::u16`], or by any writer that kept it
    /// under 65 536.
    #[allow(clippy::cast_possible_truncation)]
    pub fn u16(&mut self) -> u16 {
        let value = self.u64();
        if value > 0xFFFF {
            self.fail(Error::FieldTooWide);
            return 0;
        }
        value as u16
    }

    /// Reads a `u8` field, refusing anything wider — a schema disagreement
    /// rather than a value to truncate into something plausible.
    #[allow(clippy::cast_possible_truncation)]
    pub fn u8(&mut self) -> u8 {
        let value = self.u64();
        if value > 0xFF {
            self.fail(Error::FieldTooWide);
            return 0;
        }
        value as u8
    }

    /// Reads a field written by [`Writer::u32`].
    #[allow(clippy::cast_possible_truncation)]
    pub fn u32(&mut self) -> u32 {
        let value = self.u64();
        if value > 0xFFFF_FFFF {
            self.fail(Error::FieldTooWide);
            return 0;
        }
        value as u32
    }

    /// Reads a field written by [`Writer::i32`].
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

    /// Reads a field written by [`Writer::f32`].
    #[allow(clippy::cast_possible_truncation)]
    pub fn f32(&mut self) -> f32 {
        f32::from_bits((self.u64() as u32).swap_bytes())
    }

    /// Reads a field written by [`Writer::f64`].
    pub fn f64(&mut self) -> f64 {
        f64::from_bits(self.u64().swap_bytes())
    }

    /// Returns the field's bytes as a sub-slice of the message, without copying.
    pub fn bytes(&mut self) -> &'a [u8] {
        let Some((size, start)) = self.blob_size() else {
            return &[];
        };
        if size > self.buf.len() - start {
            self.fail(Error::Truncated);
            return &[];
        }
        self.at = start + size;
        &self.buf[start..start + size]
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

    /// Reads a blob or string-array header and returns the size it declares with
    /// the offset just past it.
    fn blob_size(&mut self) -> Option<(usize, usize)> {
        let header = self.header()?;
        if header & MORE_SIZE_FLAG == 0 {
            let Some(low) = self.buf.get(self.at + 1) else {
                self.fail(Error::Truncated);
                return None;
            };
            return Some((
                usize::from(header & 0b111) << 8 | usize::from(*low),
                self.at + 2,
            ));
        }
        self.escaped_size(header & 0b111)
    }

    /// Reads the wide size that follows a header whose `more` flag is set. Its
    /// width is named by the header rather than discovered byte by byte, so
    /// there is no continuation run for a peer to make unbounded — the only
    /// thing refused here is a size this platform cannot address.
    fn escaped_size(&mut self, escape: u8) -> Option<(usize, usize)> {
        let width = match escape {
            ESCAPE_2BYTES => 2,
            ESCAPE_4BYTES => 4,
            ESCAPE_8BYTES => 8,
            _ => {
                self.fail(Error::BadEscape);
                return None;
            }
        };
        let rest = &self.buf[self.at + 1..];
        if rest.len() < width {
            self.fail(Error::Truncated);
            return None;
        }
        let value = le_uint(rest, width);
        let Ok(size) = usize::try_from(value) else {
            self.fail(Error::SizeTooLarge);
            return None;
        };
        Some((size, self.at + 1 + width))
    }

    /// Appends the array's elements to `dst`, which may be empty.
    pub fn ints_into<T: Integer>(&mut self, dst: &mut Vec<T>) {
        let Some((elements, positive, width)) = self.array_elements() else {
            return;
        };
        append_array(dst, elements, width, positive);
    }

    /// Reads an integer array field.
    pub fn ints<T: Integer>(&mut self) -> Vec<T> {
        let mut dst = Vec::new();
        self.ints_into(&mut dst);
        dst
    }

    /// Reads an array field's header and returns its payload as a sub-slice of
    /// the message, with the element width and sign the header declares. It
    /// advances the cursor: the caller only has to turn bytes into elements.
    fn array_elements(&mut self) -> Option<(&'a [u8], bool, usize)> {
        let header = self.header()?;
        let (count, start) = self.array_count(header)?;
        let width = 1_usize << ((header >> ARRAY_WIDTH_SHIFT) & 0b11);
        if count > (self.buf.len() - start) / width {
            self.fail(Error::Truncated);
            return None;
        }
        self.at = start + count * width;
        Some((
            &self.buf[start..start + count * width],
            header & ARRAY_POSITIVE_FLAG != 0,
            width,
        ))
    }

    /// Reads an integer array's header and returns its element count with the
    /// offset just past it.
    fn array_count(&mut self, header: u8) -> Option<(usize, usize)> {
        if header & MORE_ARRAY_LEN_FLAG == 0 {
            let Some(count) = self.buf.get(self.at + 1) else {
                self.fail(Error::Truncated);
                return None;
            };
            return Some((usize::from(*count), self.at + 2));
        }
        let Some(bytes) = self.buf.get(self.at + 1..self.at + 5) else {
            self.fail(Error::Truncated);
            return None;
        };
        let bytes: [u8; 4] = bytes.try_into().expect("four bytes");
        Some((u32::from_le_bytes(bytes) as usize, self.at + 5))
    }

    /// Appends each element of a string array to `dst` as a sub-slice of the
    /// message, without copying.
    pub fn strings_bytes(&mut self) -> Vec<&'a [u8]> {
        let mut dst = Vec::new();
        let Some((count, mut at)) = self.blob_size() else {
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

    /// Reads one string-array element length: one byte, or four more behind the
    /// escape.
    fn element_size(&mut self, at: usize) -> Option<(usize, usize)> {
        let Some(size) = self.buf.get(at) else {
            self.fail(Error::Truncated);
            return None;
        };
        if *size != ELEMENT_SIZE_ESCAPE {
            return Some((usize::from(*size), at + 1));
        }
        let Some(bytes) = self.buf.get(at + 1..at + 5) else {
            self.fail(Error::Truncated);
            return None;
        };
        let bytes: [u8; 4] = bytes.try_into().expect("four bytes");
        Some((u32::from_le_bytes(bytes) as usize, at + 5))
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

    /// Refused rather than guessed: the header says how wide a field is only
    /// once the reader knows which of the four layouts it is reading, and that
    /// comes from the key. An unknown key is a record definition the two sides
    /// no longer share.
    pub fn skip(&mut self) {
        let key = self.key();
        self.fail(Error::UnknownKey(key));
    }

    // --- composites ----------------------------------------------------------

    /// Reads a narrow composite's length and returns its body, advancing past
    /// the whole field.
    fn composite_body(&mut self) -> Option<&'a [u8]> {
        if self.at + 2 > self.buf.len() {
            self.fail(Error::Truncated);
            return None;
        }
        let width = LENGTH_WIDTH[usize::from(self.buf[self.at] & 0b11)];
        let rest = &self.buf[self.at + 1..];
        if rest.len() < width {
            self.fail(Error::Truncated);
            return None;
        }
        let Ok(length) = usize::try_from(le_uint(rest, width)) else {
            self.fail(Error::SizeTooLarge);
            return None;
        };
        let start = self.at + 1 + width;
        if length > self.buf.len() - start {
            self.fail(Error::Truncated);
            return None;
        }
        self.at = start + length;
        Some(&self.buf[start..start + length])
    }

    /// Returns a nested run's bytes and the key width it uses, advancing past
    /// the whole field. The caller reads the body with [`Reader::new`] or
    /// [`super::Reader8::new`] accordingly — the per-scope key width, spent
    /// where it is decided.
    pub fn struct_body(&mut self) -> Option<(&'a [u8], bool)> {
        let Some(header) = self.buf.get(self.at) else {
            self.fail(Error::Truncated);
            return None;
        };
        let wide_keys = header & super::wide::STRUCT_WIDE_KEYS != 0;
        let body = self.composite_body()?;
        Some((body, wide_keys))
    }

    /// Reports whether the field at the cursor is a table rather than a list. A
    /// writer chooses between them on the row count, so a reader has to ask.
    pub fn is_table(&self) -> bool {
        self.buf
            .get(self.at)
            .is_some_and(|header| header & super::wide::TABLE_FLAG != 0)
    }

    /// Returns the element count of a list, map or table and a reader over what
    /// follows it.
    pub fn counted(&mut self) -> Option<(usize, Reader<'a>)> {
        let body = self.composite_body()?;
        let Some((count, at)) = read_count(body) else {
            self.fail(Error::Truncated);
            return None;
        };
        Some((count, Reader::new(&body[at..])))
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
            let bytes: [u8; 4] = bytes.try_into().expect("four bytes");
            size = u32::from_le_bytes(bytes) as usize;
            start = self.at + 5;
        }
        if size > self.buf.len() - start {
            self.fail(Error::Truncated);
            return None;
        }
        self.at = start + size;
        Some(&self.buf[start..start + size])
    }

    /// Reads an unsigned map key or value.
    pub fn element_uint(&mut self) -> u64 {
        let Some(header) = self.header() else {
            return 0;
        };
        let code = u64::from(header & 0b1111);
        if code <= UINT_INLINE_MAX {
            self.at += 1;
            return code;
        }
        let width = code as usize - usize::from(UINT_WIDTH_BASE) + 1;
        let rest = &self.buf[self.at + 1..];
        if rest.len() < width {
            self.fail(Error::Truncated);
            return 0;
        }
        self.at += 1 + width;
        le_uint(rest, width)
    }

    /// Reads a signed map key or value.
    #[allow(clippy::cast_possible_wrap)]
    pub fn element_int(&mut self) -> i64 {
        let Some(header) = self.header() else {
            return 0;
        };
        let positive = header & INT_POSITIVE_FLAG != 0;
        let magnitude = self.element_magnitude();
        if positive {
            return magnitude as i64;
        }
        (magnitude as i64).wrapping_neg()
    }

    /// [`Reader::signed_magnitude`] for a key-less element, which
    /// [`Reader::element_int`] needs for the same reason [`Reader::i64`] needs
    /// it: the signed and unsigned nibbles are different tables over the same
    /// four bits.
    fn element_magnitude(&mut self) -> u64 {
        let Some(header) = self.header() else {
            return 0;
        };
        let width = MAGNITUDE_WIDTH[usize::from(header & 0b111)];
        let rest = &self.buf[self.at + 1..];
        if rest.len() < width {
            self.fail(Error::Truncated);
            return 0;
        }
        self.at += 1 + width;
        if width == 0 {
            return 1;
        }
        le_uint(rest, width)
    }

    /// Reads a string map key or value.
    pub fn element_string(&mut self) -> String {
        let Some(header) = self.header() else {
            return String::new();
        };
        let width = LENGTH_WIDTH[usize::from(header & 0b11)];
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
        let Some(body) = self.composite_body() else {
            return;
        };
        dst.clear();
        dst.resize(rows, T::from_i64(0));
        if let Err(err) = column::decode_array(body, rows, dst) {
            self.fail(err);
        }
    }
}
