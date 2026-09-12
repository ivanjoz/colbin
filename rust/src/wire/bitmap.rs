//! The third framing of a key run: a presence bitmap instead of a key per
//! field.
//!
//! A struct writes its fields in ascending key order and omits the zero ones, so
//! the keys it writes are an ordered subset of a known set — which is a bitmap,
//! not a sequence of numbers. Bit *i* set means key *i* is present; the fields
//! follow in that order, each stripped of its key and reduced to a descriptor
//! and a payload.
//!
//! ```text
//! [bitmap bytes:1] [bitmap] ( [descriptor] [payload] )*
//! ```
//!
//! # It is the smallest and the slowest
//!
//! Smaller whenever `1 + b < p` for `p` present fields and a `b`-byte bitmap —
//! so with keys under sixteen, from four present fields up. On the ten-field
//! benchmark record with five fields set it is ten bytes against the narrow
//! key's eleven and the wide keyed form's twelve, and it decodes at about 3.5×
//! the cost. It is for a wire that is size-bound; the narrow key is for one that
//! is not.
//!
//! # Why it is a wide-key idea only
//!
//! At four key bits the key rides in the descriptor byte the field needs anyway,
//! so removing the key removes nothing and the bitmap is pure addition. The gain
//! here is exactly the byte K8 spends on a key and K4 does not.
//!
//! # Skipping still works
//!
//! The descriptors are unchanged, so every field still carries or implies its
//! own length, and an unknown field is still nameable: its key is its bit
//! position.

use super::wide::{CLASS_BLOB, CLASS_INT, Reader8, descriptor};
use super::{INT_POSITIVE_FLAG, append_magnitude, length_code_for, size_code_for};
use crate::Error;

/// The largest key a bitmap run carries. It is 63 rather than 255 because the
/// writer holds the bitmap in one word: a wider one would have to live in
/// memory, and the read-modify-write that costs is the whole reason this framing
/// is fast enough to be worth having. A struct with more than sixty-four fields
/// uses the keyed wide form, which has no such bound.
pub const MAX_BITMAP_KEY: u8 = 63;

/// What [`MAX_BITMAP_KEY`] implies.
const MAX_BITMAP_BYTES: usize = (MAX_BITMAP_KEY as usize + 1) / 8;

/// Writes a key run whose keys are a bitmap. Keys must be written in ascending
/// order; the writer does not check, for the reason the keyed writers do not
/// check a key — it is a constant of the record definition, not data.
pub struct BitmapWriter<'a> {
    buf: &'a mut Vec<u8>,
    /// The presence bitmap, held in a register until [`BitmapWriter::finish`]
    /// patches it. OR-ing into a local rather than into the buffer is the
    /// difference between a register operation and a bounds-checked
    /// read-modify-write, and measured 22% of a ten-field encode.
    bits: u64,
    /// Where the bitmap will be written in the buffer.
    offset: usize,
    /// Its byte count.
    width: usize,
}

impl<'a> BitmapWriter<'a> {
    /// Starts a run over a buffer, reserving a bitmap wide enough for keys
    /// `0..=max_key`. The bitmap is written zeroed and filled in as fields
    /// arrive, which is why this costs no second pass.
    #[allow(clippy::cast_possible_truncation)]
    pub fn new(buf: &'a mut Vec<u8>, max_key: u8) -> Self {
        let width = usize::from(max_key) / 8 + 1;
        buf.push(width as u8);
        let offset = buf.len();
        buf.resize(offset + width, 0);
        Self {
            buf,
            bits: 0,
            offset,
            width,
        }
    }

    /// Patches the bitmap into the bytes reserved for it. Call it once, after
    /// the last field.
    #[allow(clippy::cast_possible_truncation)]
    pub fn finish(self) {
        let mut bits = self.bits;
        for index in 0..self.width {
            self.buf[self.offset + index] = bits as u8;
            bits >>= 8;
        }
    }

    /// Marks a key present.
    fn present(&mut self, key: u8) {
        self.bits |= 1 << key;
    }

    // The write methods mirror the wide writer's, minus the key byte. Each is
    // the same omit-zero test, the same descriptor, and one OR.

    /// Writes an unsigned integer, and nothing at all when it is zero.
    #[allow(clippy::cast_possible_truncation)]
    pub fn u64(&mut self, key: u8, value: u64) {
        if value == 0 {
            return;
        }
        self.present(key);
        if value <= 0x7F {
            self.buf.push(value as u8);
            return;
        }
        let (code, width) = size_code_for(value);
        self.buf.push(descriptor(CLASS_INT, INT_POSITIVE_FLAG | code));
        append_magnitude(self.buf, value, width);
    }

    /// Writes a signed integer.
    #[allow(clippy::cast_sign_loss)]
    pub fn i64(&mut self, key: u8, value: i64) {
        if value >= 0 {
            self.u64(key, value as u64);
            return;
        }
        self.present(key);
        let magnitude = (value as u64).wrapping_neg();
        let (code, width) = size_code_for(magnitude);
        self.buf.push(descriptor(CLASS_INT, code));
        append_magnitude(self.buf, magnitude, width);
    }

    /// Writes one byte when true and nothing when false.
    pub fn bool(&mut self, key: u8, value: bool) {
        if !value {
            return;
        }
        self.present(key);
        self.buf.push(1);
    }

    /// Writes a field whose type cannot exceed two bytes.
    ///
    /// The width-typed entry points are here for the reason they are on the
    /// keyed writers: a `u64` parameter forces the method to carry every width
    /// the format has, which pushes it past the inline budget.
    #[allow(clippy::cast_possible_truncation)]
    pub fn u16(&mut self, key: u8, value: u16) {
        if value == 0 {
            return;
        }
        self.present(key);
        if value <= 0x7F {
            self.buf.push(value as u8);
            return;
        }
        if value <= 0xFF {
            self.buf
                .push(descriptor(CLASS_INT, INT_POSITIVE_FLAG | 1));
            self.buf.push(value as u8);
            return;
        }
        self.buf
            .push(descriptor(CLASS_INT, INT_POSITIVE_FLAG | 2));
        self.buf.extend_from_slice(&value.to_le_bytes());
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
        self.present(key);
        if value <= 0x7F {
            self.buf.push(value as u8);
            return;
        }
        let (code, width) = size_code_for(u64::from(value));
        self.buf.push(descriptor(CLASS_INT, INT_POSITIVE_FLAG | code));
        append_magnitude(self.buf, u64::from(value), width);
    }

    /// Writes a signed field no wider than four bytes.
    pub fn i32(&mut self, key: u8, value: i32) {
        self.i64(key, i64::from(value));
    }

    /// Writes an `f32` as its reversed bit pattern.
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
        self.present(key);
        let (code, width) = length_code_for(value.len() as u64);
        self.buf.push(descriptor(CLASS_BLOB, code));
        append_magnitude(self.buf, value.len() as u64, width);
        self.buf.extend_from_slice(value);
    }
}

/// Walks a run written by [`BitmapWriter`], yielding the set bits in order as
/// keys.
pub struct BitmapReader<'a> {
    inner: Reader8<'a>,
    bitmap: &'a [u8],
    /// The set bits of the current chunk, consumed lowest first.
    word: u64,
    /// The key of bit 0 of `word`.
    base: usize,
    current: u8,
    valid: bool,
}

impl<'a> BitmapReader<'a> {
    /// Starts a reader over one run.
    pub fn new(message: &'a [u8]) -> Self {
        let Some(width) = message.first().map(|byte| usize::from(*byte)) else {
            let mut inner = Reader8::new(message);
            inner.fail(Error::Truncated);
            return Self::failed(inner);
        };
        if width < 1 || width > MAX_BITMAP_BYTES || message.len() < 1 + width {
            let mut inner = Reader8::new(message);
            inner.fail(Error::BadBitmap);
            return Self::failed(inner);
        }
        let mut reader = Self {
            inner: Reader8::at(message, 1 + width),
            bitmap: &message[1..1 + width],
            word: 0,
            base: 0,
            current: 0,
            valid: false,
        };
        reader.word = bitmap_word(reader.bitmap, 0);
        reader.advance();
        reader
    }

    fn failed(inner: Reader8<'a>) -> Self {
        Self {
            inner,
            bitmap: &[],
            word: 0,
            base: 0,
            current: 0,
            valid: false,
        }
    }

    /// Moves to the next set bit, which is the next key present.
    ///
    /// A trailing-zero count over a word finds it in two instructions and
    /// clearing it takes one more, so a sparse bitmap costs nothing for the keys
    /// it does not hold. Scanning the bits one at a time was the first version
    /// and it measured 48 ns against the narrow reader's 14 on a ten-field
    /// record.
    #[allow(clippy::cast_possible_truncation)]
    fn advance(&mut self) {
        while self.word == 0 {
            let next = self.base + 64;
            if next >= self.bitmap.len() * 8 {
                self.valid = false;
                return;
            }
            self.base = next;
            self.word = bitmap_word(self.bitmap, next / 8);
        }
        let bit = self.word.trailing_zeros() as usize;
        self.word &= self.word - 1;
        self.current = (self.base + bit) as u8;
        self.valid = true;
    }

    /// Reports whether another field follows.
    pub fn more(&self) -> bool {
        self.valid && self.inner.err().is_ok()
    }

    /// The field the cursor is on.
    pub fn key(&self) -> u8 {
        self.current
    }

    /// The first failure.
    pub fn err(&self) -> Result<(), Error> {
        self.inner.err()
    }

    // The read methods reach the same descriptor readers the wide reader uses,
    // with the cursor stepped back one byte because there is no key. Backing it
    // up is what lets the whole of that value handling be shared rather than
    // written a third time.

    /// Reads an unsigned integer field.
    pub fn u64(&mut self) -> u64 {
        self.inner.step_back();
        let value = self.inner.u64();
        self.advance();
        value
    }

    /// Reads a signed integer field.
    pub fn i64(&mut self) -> i64 {
        self.inner.step_back();
        let value = self.inner.i64();
        self.advance();
        value
    }

    /// Reads a bool field.
    pub fn bool(&mut self) -> bool {
        self.u64() == 1
    }

    /// Reads a `u8` field.
    pub fn u8(&mut self) -> u8 {
        self.inner.step_back();
        let value = self.inner.u8();
        self.advance();
        value
    }

    /// Reads a `u16` field.
    pub fn u16(&mut self) -> u16 {
        self.inner.step_back();
        let value = self.inner.u16();
        self.advance();
        value
    }

    /// Reads a `u32` field.
    pub fn u32(&mut self) -> u32 {
        self.inner.step_back();
        let value = self.inner.u32();
        self.advance();
        value
    }

    /// Reads an `i32` field.
    pub fn i32(&mut self) -> i32 {
        self.inner.step_back();
        let value = self.inner.i32();
        self.advance();
        value
    }

    /// Reads an `i16` field.
    pub fn i16(&mut self) -> i16 {
        self.inner.step_back();
        let value = self.inner.i16();
        self.advance();
        value
    }

    /// Reads an `i8` field.
    pub fn i8(&mut self) -> i8 {
        self.inner.step_back();
        let value = self.inner.i8();
        self.advance();
        value
    }

    /// Reads an `f32` field.
    pub fn f32(&mut self) -> f32 {
        self.inner.step_back();
        let value = self.inner.f32();
        self.advance();
        value
    }

    /// Reads an `f64` field.
    pub fn f64(&mut self) -> f64 {
        self.inner.step_back();
        let value = self.inner.f64();
        self.advance();
        value
    }

    /// Returns the field's bytes as a sub-slice of the message.
    pub fn bytes(&mut self) -> &'a [u8] {
        self.inner.step_back();
        let value = self.inner.bytes();
        self.advance();
        value
    }

    /// Copies the field into a `String`.
    pub fn string(&mut self) -> String {
        self.inner.step_back();
        let value = self.inner.string();
        self.advance();
        value
    }

    /// Steps over a field whose key this reader does not know, which works here
    /// for the same reason it works under a wide key: the descriptor sizes the
    /// field.
    pub fn skip(&mut self) -> bool {
        self.inner.step_back();
        if !self.inner.skip() {
            return false;
        }
        self.advance();
        true
    }
}

/// Reads up to eight bitmap bytes as one little-endian word, zero padded. A
/// struct with sixty-four keys or fewer — which is every struct this framing
/// carries — needs exactly one, so the refill above never runs.
fn bitmap_word(bitmap: &[u8], at: usize) -> u64 {
    let mut word = 0_u64;
    for index in 0..8 {
        match bitmap.get(at + index) {
            Some(byte) => word |= u64::from(*byte) << (8 * index),
            None => break,
        }
    }
    word
}
