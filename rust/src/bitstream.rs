//! LSB-first bit reader, mirroring `compact/bitstream.go`.
//!
//! Values are written low bit first and read back in the same order, so nothing
//! in a compact message is byte aligned except the final pad. Reads are sticky:
//! once one runs past the end every later read returns zero and leaves the error
//! set, so a caller may decode a whole record and check once at the end rather
//! than after every field.

use std::borrow::Cow;

use crate::Error;

pub(crate) struct BitReader<'a> {
    buf: &'a [u8],
    /// Next bit to consume, as an offset in bits rather than bytes.
    pos: usize,
    /// Total bits available, `buf.len() * 8`.
    limit: usize,
    err: Option<Error>,
}

impl<'a> BitReader<'a> {
    pub(crate) fn new(buf: &'a [u8]) -> Self {
        Self {
            buf,
            pos: 0,
            limit: buf.len() * 8,
            err: None,
        }
    }

    /// The first error any read hit, or `None`.
    pub(crate) fn err(&self) -> Option<Error> {
        self.err.clone()
    }

    pub(crate) fn fail(&mut self, err: Error) {
        if self.err.is_none() {
            self.err = Some(err);
        }
    }

    /// Bits not yet consumed.
    pub(crate) fn remaining(&self) -> usize {
        self.limit - self.pos
    }

    /// Returns the next `width` bits (0..=64) as the low bits of the result.
    pub(crate) fn get(&mut self, width: u8) -> u64 {
        if self.err.is_some() {
            return 0;
        }
        if self.pos + usize::from(width) > self.limit {
            self.err = Some(Error::Truncated);
            return 0;
        }
        let mut out = 0_u64;
        let mut shift = 0_u8;
        let mut width = width;
        while width > 32 {
            out |= self.chunk(32) << shift;
            shift += 32;
            width -= 32;
        }
        out | (self.chunk(width) << shift)
    }

    pub(crate) fn get_bool(&mut self) -> bool {
        self.get(1) == 1
    }

    /// Reads `width <= 32` bits. The caller has bounds checked the whole read,
    /// so this only has to assemble bytes: a field of up to 32 bits starting mid
    /// byte spans at most five, so eight always cover it and the shift stays
    /// inside `u64` (7 + 32 = 39).
    fn chunk(&mut self, width: u8) -> u64 {
        if width == 0 {
            return 0;
        }
        let index = self.pos / 8;
        let shift = (self.pos % 8) as u32;
        let mut window = [0_u8; 8];
        let take = (self.buf.len() - index).min(8);
        window[..take].copy_from_slice(&self.buf[index..index + take]);
        self.pos += usize::from(width);
        (u64::from_le_bytes(window) >> shift) & ((1_u64 << width) - 1)
    }

    /// The unread bits as a byte slice starting on a byte boundary, so packed5
    /// and the varint array codec — both byte structured — can be read out of a
    /// stream that is not byte aligned.
    ///
    /// When the cursor already sits on a boundary the underlying buffer is
    /// borrowed and nothing is copied. Otherwise the tail is shifted into a
    /// fresh buffer, which costs one pass over the remaining bytes; a compact
    /// message holds at most three records, so that remainder is bounded by a
    /// small constant rather than by the payload size.
    pub(crate) fn aligned_tail(&self) -> Cow<'a, [u8]> {
        let index = self.pos / 8;
        let shift = self.pos % 8;
        if shift == 0 {
            return Cow::Borrowed(&self.buf[index..]);
        }
        let tail = &self.buf[index..];
        let mut out = Vec::with_capacity(tail.len());
        for (offset, byte) in tail.iter().enumerate() {
            let mut value = byte >> shift;
            if let Some(next) = tail.get(offset + 1) {
                value |= next << (8 - shift);
            }
            out.push(value);
        }
        Cow::Owned(out)
    }

    /// Steps the cursor over a byte-structured payload a sub-codec has just
    /// consumed, refusing one that would run into the final pad.
    pub(crate) fn advance_bytes(&mut self, n: usize) -> bool {
        if n > self.remaining() / 8 {
            self.fail(Error::Truncated);
            return false;
        }
        self.pos += 8 * n;
        true
    }
}
