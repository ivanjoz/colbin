//! LSB-first bit reader and writer, mirroring `compact/bitstream.go`.
//!
//! Values are written low bit first and read back in the same order, so nothing
//! in a compact message is byte aligned except the final pad. Reads are sticky:
//! once one runs past the end every later read returns zero and leaves the error
//! set, so a caller may decode a whole record and check once at the end rather
//! than after every field.

use crate::Error;

/// LSB-first writer, the inverse of [`BitReader`].
///
/// Writes are split into chunks of at most 32 bits so the accumulator, which
/// holds up to 31 leftover bits between calls, can never overflow: 31 + 32 < 64.
/// It writes into a buffer the caller owns, so a caller encoding many messages
/// hands back the same `Vec` and the encoder allocates nothing at all. Growing a
/// fresh `Vec` from zero measured 48 ns of a 134 ns message -- the single largest
/// cost in the encoder, and the reason this borrows rather than owns.
pub(crate) struct BitWriter<'a> {
    buf: &'a mut Vec<u8>,
    /// Pending bits not yet flushed to `buf`; always fewer than 32 between calls.
    current: u64,
    nbits: u8,
    /// Where this message began, so `bits` measures the message rather than the
    /// buffer it was appended to.
    base: usize,
}

impl<'a> BitWriter<'a> {
    pub(crate) fn new(buf: &'a mut Vec<u8>) -> Self {
        let base = buf.len();
        Self {
            buf,
            current: 0,
            nbits: 0,
            base,
        }
    }

    /// Appends the low `width` bits of `value` (width 0..=64).
    pub(crate) fn put(&mut self, mut value: u64, mut width: u8) {
        while width > 32 {
            self.chunk(value & 0xFFFF_FFFF, 32);
            value >>= 32;
            width -= 32;
        }
        self.chunk(value, width);
    }

    pub(crate) fn put_bool(&mut self, bit: bool) {
        self.chunk(u64::from(bit), 1);
    }

    /// Writes `width <= 32` bits, keeping every shift inside `u64` range.
    pub(crate) fn chunk(&mut self, value: u64, width: u8) {
        if width == 0 {
            return;
        }
        let value = value & ((1_u64 << width) - 1);
        self.current |= value << self.nbits;
        self.nbits += width;
        if self.nbits >= 32 {
            self.buf
                .extend_from_slice(&(self.current as u32).to_le_bytes());
            self.current >>= 32;
            self.nbits -= 32;
        }
    }

    /// Writes `bytes` as consecutive 8-bit units. When the stream already sits on
    /// a byte boundary they are copied straight in rather than shifted through
    /// the accumulator one at a time — which is the case for a packed5 frame
    /// following a whole number of bytes.
    pub(crate) fn put_bytes(&mut self, bytes: &[u8]) {
        if self.nbits % 8 == 0 {
            self.flush_whole();
            self.buf.extend_from_slice(bytes);
            return;
        }
        for byte in bytes {
            self.chunk(u64::from(*byte), 8);
        }
    }

    /// Moves every complete pending byte into `buf`. Only correct to call when
    /// what follows starts on a byte boundary.
    fn flush_whole(&mut self) {
        while self.nbits >= 8 {
            self.buf.push(self.current as u8);
            self.current >>= 8;
            self.nbits -= 8;
        }
    }

    /// Writes any remaining partial byte, zero padded in its high bits, and
    /// returns how many bytes this message occupies. The padding is never read
    /// back: a record ends at its terminator and a message at its last record,
    /// so a decoder stops before reaching it.
    pub(crate) fn finish(mut self) -> usize {
        self.flush_whole();
        if self.nbits > 0 {
            self.buf.push(self.current as u8);
        }
        self.buf.len() - self.base
    }
}

pub(crate) struct BitReader<'a> {
    buf: &'a [u8],
    /// Next bit to consume, as an offset in bits rather than bytes.
    ///
    /// Authoritative. `aligned_tail` and `advance_bytes` work in these terms, so
    /// the cache below is derived from it and dropped whenever they move it.
    pos: usize,
    /// Total bits available, `buf.len() * 8`.
    limit: usize,
    err: Option<Error>,
    /// Bits already loaded, low bit first: `nbits` of them, starting at `pos`.
    ///
    /// A read that fits is a shift and a mask; only a refill touches the buffer.
    /// Loading a window on every read instead measured about 7 ns per read and
    /// was most of a decoded field, because a field is three or four reads of
    /// four to eight bits and the window covers sixty-four.
    acc: u64,
    nbits: u8,
    /// Reused buffer for shifting an unaligned byte-structured payload into
    /// place. One per reader rather than one per field: a record of seven
    /// strings was allocating seven of these and throwing each away.
    scratch: Vec<u8>,
}

/// The most bits `take` will be asked for, and so the least a refill must leave
/// buffered. Wider reads are split into chunks of this size by `get`.
const TAKE_BITS: u8 = 32;

impl<'a> BitReader<'a> {
    pub(crate) fn new(buf: &'a [u8]) -> Self {
        Self {
            buf,
            pos: 0,
            limit: buf.len() * 8,
            err: None,
            acc: 0,
            nbits: 0,
            scratch: Vec::new(),
        }
    }

    /// The first error any read hit, or `None`.
    ///
    /// Borrowed, not cloned. It is consulted after every key and every value, so
    /// handing back an owned `Option<Error>` meant constructing one twice per
    /// field for an answer that is almost always `None`.
    pub(crate) fn err(&self) -> Option<&Error> {
        self.err.as_ref()
    }

    /// Whether any read has failed, for the hot checks that do not need the
    /// error itself.
    pub(crate) fn failed(&self) -> bool {
        self.err.is_some()
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
        while width > TAKE_BITS {
            out |= self.take(TAKE_BITS) << shift;
            shift += TAKE_BITS;
            width -= TAKE_BITS;
        }
        out | (self.take(width) << shift)
    }

    pub(crate) fn get_bool(&mut self) -> bool {
        self.get(1) == 1
    }

    /// Reads `width <= 32` bits. The caller has bounds checked the whole read,
    /// so this only has to assemble bytes: a field of up to 32 bits starting mid
    /// byte spans at most five, so eight always cover it and the shift stays
    /// inside `u64` (7 + 32 = 39).
    /// Takes `width <= TAKE_BITS` bits out of the accumulator, refilling it
    /// first if it does not hold that many. The caller has bounds checked the
    /// whole read.
    fn take(&mut self, width: u8) -> u64 {
        if width == 0 {
            return 0;
        }
        if self.nbits < width {
            self.refill();
        }
        let out = self.acc & ((1_u64 << width) - 1);
        self.acc >>= width;
        self.nbits -= width;
        self.pos += usize::from(width);
        out
    }

    /// Reloads the accumulator from `pos`, discarding what it held.
    ///
    /// Discarding rather than appending is what keeps this simple: bits can only
    /// be loaded from a byte boundary, so appending to a partially drained
    /// accumulator would mean tracking two offsets. Reloading from `pos` needs
    /// one shift, and the bits thrown away are re-read by the same load.
    ///
    /// It always leaves at least `width` bits for the read that asked. With
    /// eight bytes available that is at least 64 - 7 = 57; with fewer, `take` is
    /// the whole remainder and the caller's bounds check already established
    /// that the remainder covers the read. The eight-byte window is copied
    /// rather than loaded straight from the buffer for the reason measurement
    /// gave: an `index + 8 <= len` fast path around an unaligned load -- what the
    /// Go reader does -- came out slower at every message size in the corpus,
    /// because a compact message is small enough that the fast path rarely
    /// applies and `take` being provably at most eight is worth more.
    fn refill(&mut self) {
        let index = self.pos / 8;
        let shift = (self.pos % 8) as u32;
        let mut window = [0_u8; 8];
        let take = (self.buf.len() - index).min(8);
        window[..take].copy_from_slice(&self.buf[index..index + take]);
        self.acc = u64::from_le_bytes(window) >> shift;
        self.nbits = ((take as u32 * 8).saturating_sub(shift)).min(64) as u8;
    }

    /// Drops the accumulator, for the two callers that move `pos` themselves.
    fn invalidate(&mut self) {
        self.acc = 0;
        self.nbits = 0;
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
    pub(crate) fn aligned_tail(&mut self) -> &[u8] {
        // Copied out of self, so the shift below can borrow self mutably: `buf`
        // is a shared reference and lives as long as the reader does.
        let buf: &'a [u8] = self.buf;
        let index = self.pos / 8;
        let shift = self.pos % 8;
        if shift == 0 {
            return &buf[index..];
        }
        let tail = &buf[index..];
        self.scratch.clear();
        self.scratch.reserve(tail.len());
        for (offset, byte) in tail.iter().enumerate() {
            let mut value = byte >> shift;
            if let Some(next) = tail.get(offset + 1) {
                value |= next << (8 - shift);
            }
            self.scratch.push(value);
        }
        &self.scratch
    }

    /// Steps the cursor over a byte-structured payload a sub-codec has just
    /// consumed, refusing one that would run into the final pad.
    pub(crate) fn advance_bytes(&mut self, n: usize) -> bool {
        if n > self.remaining() / 8 {
            self.fail(Error::Truncated);
            return false;
        }
        self.pos += 8 * n;
        // pos moved without going through `take`, so the buffered bits no longer
        // start at pos.
        self.invalidate();
        true
    }
}
