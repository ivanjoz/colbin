//! **Minimal mode**: a byte-aligned key/value layout for one record of at most
//! sixteen primitive fields. The Rust half of the Go `minimal` package, whose
//! `README.md` is the format's specification; this file mirrors it and adds
//! nothing.
//!
//! A field is `[key][what follows]`, a zero-valued field is not written at all,
//! and nothing is bit-packed across a byte boundary — so a decode is a `match` on
//! the key and a few loads, not a bitstream walk.
//!
//! # Nothing has a size ceiling
//!
//! A header carries a size's low bits and a flag saying whether more follow; when
//! it is set, LEB128 continuation bytes carry the rest. The common size costs
//! nothing extra and no size is refused — which is also why [`Writer`] has no
//! error: there is nothing it can be asked to write that it cannot.
//!
//! ```text
//! integer       [key:4][positive:1][size:3]              [magnitude: size bytes]
//!               size code 0→1B 1→2B 2→3B 3→4B 4→6B 5→8B 6→no bytes, magnitude 1
//! string/bytes  [key:4][more:1][size:11]                 [bytes: size]
//!               [key:4][more:1][size:11][+LEB128]        [bytes: size]  (more = 1)
//! int array     [key:4][positive:1][width:2][more:1][count:8]  then count × width
//!               ... [+LEB128] when more = 1
//!               width code 0→1B 1→2B 2→4B 3→8B
//! string array  [key:4][more:1][count:11]  then per element [size: LEB128][bytes]
//! ```
//!
//! A message ends when its buffer ends: the frame that carries it already states
//! its length. A header says how *wide* a field is, never what it means, so the
//! reader takes the type from the key and an unknown key cannot be skipped.

use crate::Error;

/// What four key bits buy: keys 0..15. A record needing a seventeenth field
/// needs a different format, not a wider key.
pub const MAX_MINIMAL_FIELDS: u8 = 16;

const SIZE_CODE_1: u8 = 0;
const SIZE_CODE_2: u8 = 1;
const SIZE_CODE_3: u8 = 2;
const SIZE_CODE_4: u8 = 3;
const SIZE_CODE_6: u8 = 4;
const SIZE_CODE_8: u8 = 5;
/// Carries no bytes: the magnitude is one, which is what makes a true bool a
/// single byte.
const SIZE_CODE_ONE: u8 = 6;

/// An array header byte is `[key:4][positive:1][width:2][more:1]`; a blob header
/// byte is `[key:4][more:1][size hi:3]`. These name those positions.
const MORE_SIZE_FLAG: u8 = 0b1000;
const MORE_ARRAY_LEN_FLAG: u8 = 0b0001;
const ARRAY_WIDTH_SHIFT: u8 = 1;
/// Bit 3 of an integer header, directly above the three size-code bits. An
/// integer needs no continuation flag: its size code already reaches eight bytes.
const INT_POSITIVE_FLAG: u8 = 0b1000;
const ARRAY_POSITIVE_FLAG: u8 = 0b1000;

/// What each header carries before a continuation byte is needed. Past these a
/// size is not refused, only continued.
const INLINE_BLOB_SIZE: usize = (1 << 11) - 1;
const INLINE_ARRAY_COUNT: usize = (1 << 8) - 1;
const INLINE_STRING_ARRAY_LEN: usize = (1 << 11) - 1;

/// Nine continuation bytes is already past any frame this port can carry. A run
/// longer than that is refused rather than accumulated into a wrapped size.
const MAX_SIZE_CONTINUATION_BYTES: usize = 9;

/// Reads one message, field by field. The caller matches on [`MinimalReader::key`] and
/// calls the read for the type that key holds.
///
/// Every read is bounds checked: this is what parses bytes from a socket, so a
/// length that runs past the buffer has to be an error rather than a panic.
pub struct MinimalReader<'a> {
    buffer: &'a [u8],
    at: usize,
}

impl<'a> MinimalReader<'a> {
    pub fn new(message: &'a [u8]) -> Self {
        Self {
            buffer: message,
            at: 0,
        }
    }

    /// Whether another field follows.
    #[inline]
    pub fn more(&self) -> bool {
        self.at < self.buffer.len()
    }

    /// The key of the field the cursor is on. `None` at the end of the message.
    #[inline]
    pub fn key(&self) -> Option<u8> {
        self.buffer.get(self.at).map(|header| header >> 4)
    }

    /// How many bytes are left, which a caller checks to refuse trailing input.
    pub fn remaining(&self) -> usize {
        self.buffer.len() - self.at
    }

    #[inline]
    fn header(&self) -> Result<u8, Error> {
        self.buffer
            .get(self.at)
            .copied()
            .ok_or(Error::Truncated)
    }

    /// Reads an unsigned integer field.
    #[inline]
    pub fn uint(&mut self) -> Result<u64, Error> {
        let field = &self.buffer[self.at..];
        if field.len() >= 2 && field[0] & 0b111 == SIZE_CODE_1 {
            self.at += 2;
            return Ok(u64::from(field[1]));
        }
        self.uint_wide()
    }

    #[inline(never)]
    fn uint_wide(&mut self) -> Result<u64, Error> {
        let header = self.header()?;
        let size_code = header & 0b111;
        if size_code == SIZE_CODE_ONE {
            self.at += 1;
            return Ok(1);
        }
        let width = match size_code {
            SIZE_CODE_1 => 1,
            SIZE_CODE_2 => 2,
            SIZE_CODE_3 => 3,
            SIZE_CODE_4 => 4,
            SIZE_CODE_6 => 6,
            SIZE_CODE_8 => 8,
            other => return Err(Error::MinimalSizeCode(other)),
        };
        let bytes = self
            .buffer
            .get(self.at + 1..self.at + 1 + width)
            .ok_or(Error::Truncated)?;
        self.at += 1 + width;
        Ok(be_uint(bytes))
    }

    /// Reads a signed integer field.
    pub fn int(&mut self) -> Result<i64, Error> {
        let positive = self.header()? & INT_POSITIVE_FLAG != 0;
        let magnitude = self.uint()?;
        if positive {
            Ok(magnitude as i64)
        } else {
            // wrapping_neg, because the magnitude of i64::MIN is 2^63 and negating
            // it is exactly i64::MIN again — an overflow in a debug build and the
            // right answer in both.
            Ok((magnitude as i64).wrapping_neg())
        }
    }

    /// Reads an integer field that must fit the record's declared width. A peer
    /// writing a wider one is a schema disagreement, not a value to truncate.
    #[inline]
    pub fn uint_at_most(&mut self, ceiling: u64) -> Result<u64, Error> {
        let key = self.key().unwrap_or(0);
        let value = self.uint()?;
        if value > ceiling {
            return Err(Error::MinimalFieldTooWide(key));
        }
        Ok(value)
    }

    #[inline]
    pub fn u16(&mut self) -> Result<u16, Error> {
        Ok(self.uint_at_most(u64::from(u16::MAX))? as u16)
    }

    #[inline]
    pub fn u32(&mut self) -> Result<u32, Error> {
        Ok(self.uint_at_most(u64::from(u32::MAX))? as u32)
    }

    #[inline]
    pub fn bool(&mut self) -> Result<bool, Error> {
        Ok(self.uint()? == 1)
    }

    /// Reads a blob field as a slice of the message: nothing is copied.
    pub fn bytes(&mut self) -> Result<&'a [u8], Error> {
        let header = self.header()?;
        let low = *self.buffer.get(self.at + 1).ok_or(Error::Truncated)?;
        let inline = usize::from(header & 0b111) << 8 | usize::from(low);
        let (size, start) = if header & MORE_SIZE_FLAG == 0 {
            (inline, self.at + 2)
        } else {
            self.continued_size(inline, 11, self.at + 2)?
        };
        let value = self
            .buffer
            .get(start..start + size)
            .ok_or(Error::Truncated)?;
        self.at = start + size;
        Ok(value)
    }

    /// Reads the LEB128 run that follows a header whose more flag is set, and
    /// returns the whole size with the header's low bits under it.
    ///
    /// The run is what an unauthenticated peer controls, so two things are
    /// refused rather than believed: a run that never ends inside the message,
    /// and one describing more than a `usize` holds — which would wrap into a
    /// small size whose bounds check then passes.
    fn continued_size(
        &self,
        low: usize,
        low_bits: u32,
        mut at: usize,
    ) -> Result<(usize, usize), Error> {
        let mut value = low as u64;
        let mut shift = low_bits;
        for read in 0.. {
            let part = *self.buffer.get(at).ok_or(Error::Truncated)?;
            if read >= MAX_SIZE_CONTINUATION_BYTES || shift >= 64 {
                return Err(Error::MinimalSizeTooLarge);
            }
            at += 1;
            value |= u64::from(part & 0x7F) << shift;
            shift += 7;
            if part & 0x80 == 0 {
                break;
            }
        }
        if value > usize::MAX as u64 {
            return Err(Error::MinimalSizeTooLarge);
        }
        Ok((value as usize, at))
    }

    /// Reads a string field, borrowed from the message.
    pub fn str(&mut self) -> Result<&'a str, Error> {
        let bytes = self.bytes()?;
        std::str::from_utf8(bytes).map_err(|_| Error::NotUtf8)
    }

    /// Appends an integer array's elements to `into`.
    pub fn ints(&mut self, into: &mut Vec<i64>) -> Result<(), Error> {
        let header = self.header()?;
        let (count, start) = self.array_count(header)?;
        let width = 1_usize << ((header >> ARRAY_WIDTH_SHIFT) & 0b11);
        let elements = self
            .buffer
            .get(start..start + count * width)
            .ok_or(Error::Truncated)?;
        let positive = header & ARRAY_POSITIVE_FLAG != 0;
        into.reserve(count);
        for element in elements.chunks_exact(width) {
            let raw = be_uint(element);
            into.push(if positive {
                raw as i64
            } else {
                sign_extend(raw, width)
            });
        }
        self.at = start + count * width;
        Ok(())
    }

    fn array_count(&self, header: u8) -> Result<(usize, usize), Error> {
        let low = *self.buffer.get(self.at + 1).ok_or(Error::Truncated)?;
        if header & MORE_ARRAY_LEN_FLAG == 0 {
            return Ok((usize::from(low), self.at + 2));
        }
        self.continued_size(usize::from(low), 8, self.at + 2)
    }

    /// Appends a string array's elements to `into`, each borrowed from the
    /// message.
    pub fn strs(&mut self, into: &mut Vec<&'a str>) -> Result<(), Error> {
        let header = self.header()?;
        let low = *self.buffer.get(self.at + 1).ok_or(Error::Truncated)?;
        let inline = usize::from(header & 0b111) << 8 | usize::from(low);
        let (count, mut at) = if header & MORE_SIZE_FLAG == 0 {
            (inline, self.at + 2)
        } else {
            self.continued_size(inline, 11, self.at + 2)?
        };
        // No reserve on `count`: it is a number a peer chose, and reserving on it
        // before a single element has been read is how a two-byte frame asks for
        // a gigabyte. The pushes below grow the vector as elements actually
        // arrive, each one already bounds checked against the message.
        for _ in 0..count {
            let (size, next) = self.continued_size(0, 0, at)?;
            let element = self
                .buffer
                .get(next..next + size)
                .ok_or(Error::Truncated)?;
            into.push(std::str::from_utf8(element).map_err(|_| Error::NotUtf8)?);
            at = next + size;
        }
        self.at = at;
        Ok(())
    }
}

/// Appends fields to a buffer the caller owns.
///
/// Keys are not checked. The reader defends against the network; the writer
/// trusts its own program, and a key is a constant of the record definition
/// rather than data. A key above fifteen shifts into the next field's bits.
#[derive(Debug, Default)]
pub struct MinimalWriter {
    pub buffer: Vec<u8>,
}

impl MinimalWriter {
    pub fn new(buffer: Vec<u8>) -> Self {
        Self { buffer }
    }

    /// Empties the buffer without giving up its capacity.
    pub fn reset(&mut self) {
        self.buffer.clear();
    }

    /// Writes an unsigned integer, and nothing at all when it is zero.
    #[inline]
    pub fn uint(&mut self, key: u8, value: u64) {
        if value == 0 {
            return;
        }
        if value <= 0xFF {
            self.buffer
                .extend_from_slice(&[key << 4 | INT_POSITIVE_FLAG | SIZE_CODE_1, value as u8]);
            return;
        }
        self.uint_wide(key << 4 | INT_POSITIVE_FLAG, value);
    }

    /// Writes a signed integer as a sign bit and a magnitude.
    pub fn int(&mut self, key: u8, value: i64) {
        if value == 0 {
            return;
        }
        if value > 0 {
            self.uint(key, value as u64);
            return;
        }
        // Negating through u64 keeps i64::MIN, whose positive counterpart is not
        // an i64.
        self.uint_wide(key << 4, (value as u64).wrapping_neg());
    }

    pub fn bool(&mut self, key: u8, value: bool) {
        if value {
            self.buffer
                .push(key << 4 | INT_POSITIVE_FLAG | SIZE_CODE_ONE);
        }
    }

    fn uint_wide(&mut self, header: u8, magnitude: u64) {
        if magnitude == 1 {
            self.buffer.push(header | SIZE_CODE_ONE);
            return;
        }
        let (code, width) = match magnitude {
            0..=0xFF => (SIZE_CODE_1, 1),
            0x100..=0xFFFF => (SIZE_CODE_2, 2),
            0x1_0000..=0xFF_FFFF => (SIZE_CODE_3, 3),
            0x100_0000..=0xFFFF_FFFF => (SIZE_CODE_4, 4),
            0x1_0000_0000..=0xFFFF_FFFF_FFFF => (SIZE_CODE_6, 6),
            _ => (SIZE_CODE_8, 8),
        };
        self.buffer.push(header | code);
        self.buffer
            .extend_from_slice(&magnitude.to_be_bytes()[8 - width..]);
    }

    /// Writes a blob, and nothing when it is empty.
    pub fn bytes(&mut self, key: u8, value: &[u8]) {
        if value.is_empty() {
            return;
        }
        let size = value.len();
        if size <= INLINE_BLOB_SIZE {
            self.buffer
                .extend_from_slice(&[key << 4 | (size >> 8) as u8 & 0b111, size as u8]);
        } else {
            self.buffer.extend_from_slice(&[
                key << 4 | MORE_SIZE_FLAG | (size >> 8) as u8 & 0b111,
                size as u8,
            ]);
            self.append_size_extension((size >> 11) as u64);
        }
        self.buffer.extend_from_slice(value);
    }

    pub fn str(&mut self, key: u8, value: &str) {
        self.bytes(key, value.as_bytes());
    }

    /// Writes the bits of a size that did not fit its header, as LEB128
    /// continuation bytes: seven bits each, the high bit set on all but the last.
    fn append_size_extension(&mut self, mut remaining: u64) {
        loop {
            let mut part = (remaining & 0x7F) as u8;
            remaining >>= 7;
            if remaining != 0 {
                part |= 0x80;
            }
            self.buffer.push(part);
            if remaining == 0 {
                return;
            }
        }
    }

    /// Writes an integer array at one width, chosen from the widest element.
    pub fn ints(&mut self, key: u8, values: &[i64]) {
        if values.is_empty() {
            return;
        }
        let mut all_positive = true;
        let mut widest = 0_u64;
        for value in values {
            let magnitude = if *value < 0 {
                all_positive = false;
                (*value as u64).wrapping_neg()
            } else {
                *value as u64
            };
            if magnitude > widest {
                widest = magnitude;
            }
        }
        let (width, width_code) = array_width(widest, all_positive);

        let mut header = key << 4 | width_code << ARRAY_WIDTH_SHIFT;
        if all_positive {
            header |= ARRAY_POSITIVE_FLAG;
        }
        if values.len() <= INLINE_ARRAY_COUNT {
            self.buffer
                .extend_from_slice(&[header, values.len() as u8]);
        } else {
            self.buffer
                .extend_from_slice(&[header | MORE_ARRAY_LEN_FLAG, values.len() as u8]);
            self.append_size_extension((values.len() >> 8) as u64);
        }
        for value in values {
            self.buffer
                .extend_from_slice(&(*value as u64).to_be_bytes()[8 - width..]);
        }
    }

    /// Writes a string array: a count, then each element behind its own LEB128
    /// length — a varint for the same reason the header sizes are continued, and
    /// because it makes an element under 128 bytes cost one byte instead of two.
    pub fn strs(&mut self, key: u8, values: &[&str]) {
        if values.is_empty() {
            return;
        }
        let count = values.len();
        if count <= INLINE_STRING_ARRAY_LEN {
            self.buffer
                .extend_from_slice(&[key << 4 | (count >> 8) as u8 & 0b111, count as u8]);
        } else {
            self.buffer.extend_from_slice(&[
                key << 4 | MORE_SIZE_FLAG | (count >> 8) as u8 & 0b111,
                count as u8,
            ]);
            self.append_size_extension((count >> 11) as u64);
        }
        for value in values {
            self.append_size_extension(value.len() as u64);
            self.buffer.extend_from_slice(value.as_bytes());
        }
    }
}

/// The narrowest element width that holds every element. A two's complement
/// array needs one bit more than its magnitude.
fn array_width(widest: u64, all_positive: bool) -> (usize, u8) {
    let needed = if all_positive {
        widest
    } else if widest > 1 << 62 {
        // Already fills the top bit: shifting would wrap and pick one byte for
        // i64::MIN.
        return (8, 3);
    } else {
        widest << 1
    };
    match needed {
        0..=0xFF => (1, 0),
        0x100..=0xFFFF => (2, 1),
        0x1_0000..=0xFFFF_FFFF => (4, 2),
        _ => (8, 3),
    }
}

fn be_uint(bytes: &[u8]) -> u64 {
    match bytes.len() {
        1 => u64::from(bytes[0]),
        2 => u64::from(u16::from_be_bytes([bytes[0], bytes[1]])),
        4 => u64::from(u32::from_be_bytes([bytes[0], bytes[1], bytes[2], bytes[3]])),
        8 => u64::from_be_bytes([
            bytes[0], bytes[1], bytes[2], bytes[3], bytes[4], bytes[5], bytes[6], bytes[7],
        ]),
        _ => bytes.iter().fold(0_u64, |value, byte| {
            value << 8 | u64::from(*byte)
        }),
    }
}

fn sign_extend(raw: u64, width: usize) -> i64 {
    let shift = 64 - 8 * width;
    ((raw << shift) as i64) >> shift
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn integers_round_trip_at_every_width() {
        for value in [
            1_i64,
            -1,
            2,
            255,
            256,
            65535,
            1 << 23,
            i64::from(i32::MAX),
            1 << 40,
            i64::MAX,
            i64::MIN,
            -1_234_567,
            1_767_225_600_123,
        ] {
            let mut writer = MinimalWriter::default();
            writer.int(1, value);
            let mut reader = MinimalReader::new(&writer.buffer);
            assert_eq!(reader.key(), Some(1));
            assert_eq!(reader.int().unwrap(), value, "value {value}");
            assert!(!reader.more(), "value {value} left trailing bytes");
        }
    }

    #[test]
    fn a_zero_valued_field_is_not_written() {
        let mut writer = MinimalWriter::default();
        writer.int(1, 0);
        writer.uint(2, 0);
        writer.bool(3, false);
        writer.str(4, "");
        writer.ints(5, &[]);
        assert!(writer.buffer.is_empty());
    }

    #[test]
    fn a_true_bool_is_one_byte() {
        let mut writer = MinimalWriter::default();
        writer.bool(9, true);
        assert_eq!(writer.buffer.len(), 1);
        let mut reader = MinimalReader::new(&writer.buffer);
        assert_eq!(reader.key(), Some(9));
        assert!(reader.bool().unwrap());
    }

    #[test]
    fn strings_round_trip_at_every_size() {
        for size in [1, 200, INLINE_BLOB_SIZE, INLINE_BLOB_SIZE + 1, 300_000, 1 << 22] {
            let value = "x".repeat(size);
            let mut writer = MinimalWriter::default();
            writer.str(2, &value);
            let mut header = 2;
            let mut remaining = size >> 11;
            while remaining > 0 {
                header += 1;
                remaining >>= 7;
            }
            assert_eq!(writer.buffer.len(), header + size, "size {size}");
            let mut reader = MinimalReader::new(&writer.buffer);
            assert_eq!(reader.str().unwrap(), value);
            assert!(!reader.more());
        }
    }

    #[test]
    fn arrays_take_the_width_their_widest_element_needs() {
        let mut writer = MinimalWriter::default();
        writer.ints(1, &[1, 2, 3, 4]);
        assert_eq!(writer.buffer.len(), 2 + 4);

        let mut writer = MinimalWriter::default();
        writer.ints(1, &[1, 1_234_567]);
        assert_eq!(writer.buffer.len(), 2 + 8);
    }

    #[test]
    fn arrays_round_trip_signed_and_unsigned() {
        for values in [
            vec![1_i64],
            vec![0, 1, 255],
            vec![1_234_567, 7_654_321],
            vec![-1, 5],
            vec![i64::MIN, i64::MAX],
        ] {
            let mut writer = MinimalWriter::default();
            writer.ints(3, &values);
            let mut reader = MinimalReader::new(&writer.buffer);
            let mut back = Vec::new();
            reader.ints(&mut back).unwrap();
            assert_eq!(back, values);
            assert!(!reader.more());
        }
    }

    #[test]
    fn string_arrays_round_trip() {
        let values = ["responses.go:539", "", "product-stock.go:1204"];
        let mut writer = MinimalWriter::default();
        writer.strs(4, &values);
        let mut reader = MinimalReader::new(&writer.buffer);
        let mut back = Vec::new();
        reader.strs(&mut back).unwrap();
        assert_eq!(back, values);
        assert!(!reader.more());
    }

    #[test]
    fn a_wider_field_than_the_type_is_refused() {
        let mut writer = MinimalWriter::default();
        writer.uint(1, 70_000);
        let mut reader = MinimalReader::new(&writer.buffer);
        assert_eq!(reader.u16(), Err(Error::MinimalFieldTooWide(1)));

        let mut writer = MinimalWriter::default();
        writer.uint(1, 65_535);
        let mut reader = MinimalReader::new(&writer.buffer);
        assert_eq!(reader.u16().unwrap(), 65_535);
    }

    /// A length that runs past the buffer is the one thing a wire parser must
    /// never act on, so every prefix of a valid message has to fail rather than
    /// decode something — and never panic.
    #[test]
    fn every_truncation_is_refused() {
        let mut writer = MinimalWriter::default();
        writer.int(1, 1_767_225_600_123);
        writer.str(2, "responses.go:539");
        writer.ints(3, &[1_234_567, 7_654_321]);
        writer.strs(4, &["product-stock.go:1204"]);
        let full = writer.buffer.clone();

        for cut in 1..full.len() {
            let mut reader = MinimalReader::new(&full[..cut]);
            while reader.more() {
                let outcome = match reader.key() {
                    Some(1) => reader.int().map(|_| ()),
                    Some(2) => reader.str().map(|_| ()),
                    Some(3) => reader.ints(&mut Vec::new()),
                    Some(4) => reader.strs(&mut Vec::new()),
                    Some(other) => Err(Error::UnknownField(other)),
                    None => Err(Error::Truncated),
                };
                if outcome.is_err() {
                    break;
                }
            }
        }
    }


    /// Byte-for-byte agreement with the Go writer on every shape the continued
    /// sizes touch: an array count past its inline bits, LEB128 element lengths,
    /// and a blob past the eleven the header carries.
    ///
    /// The whole message is 3701 bytes, so it is pinned by its length, its first
    /// 64 bytes and an FNV-1a of all of it rather than by pasted hex. Regenerate
    /// from `/tmp/kvvec` (see KV16_DRAFT.md) if the format changes on purpose.
    #[test]
    fn agrees_with_the_go_writer_on_continued_sizes() {
        let mut writer = MinimalWriter::default();
        writer.int(0, -1_234_567);
        writer.uint(1, 1_767_225_600_123);
        writer.bool(2, true);
        writer.str(3, "responses.go:539");
        writer.ints(4, &[1_234_567, -7_654_321, 3]);
        writer.strs(
            5,
            &["responses.go:539", "no se pudo obtener el registro", ""],
        );
        let ids: Vec<i64> = (0..300).collect();
        writer.ints(6, &ids);
        let long = "z".repeat(3000);
        writer.str(7, &long);

        assert_eq!(writer.buffer.len(), 3701);
        const GO_PREFIX: [u8; 64] = [
            0x02, 0x12, 0xD6, 0x87, 0x1C, 0x01, 0x9B, 0x76, 0xDA, 0xA8, 0x7B, 0x2E, 0x30, 0x10,
            0x72, 0x65, 0x73, 0x70, 0x6F, 0x6E, 0x73, 0x65, 0x73, 0x2E, 0x67, 0x6F, 0x3A, 0x35,
            0x33, 0x39, 0x44, 0x03, 0x00, 0x12, 0xD6, 0x87, 0xFF, 0x8B, 0x34, 0x4F, 0x00, 0x00,
            0x00, 0x03, 0x50, 0x03, 0x10, 0x72, 0x65, 0x73, 0x70, 0x6F, 0x6E, 0x73, 0x65, 0x73,
            0x2E, 0x67, 0x6F, 0x3A, 0x35, 0x33, 0x39, 0x1E,
        ];
        assert_eq!(&writer.buffer[..64], &GO_PREFIX);
        let mut checksum = 14_695_981_039_346_656_037_u64;
        for byte in &writer.buffer {
            checksum ^= u64::from(*byte);
            checksum = checksum.wrapping_mul(1_099_511_628_211);
        }
        assert_eq!(checksum, 0xEE2B_368A_BF8D_34A8);

        // And the message this side wrote reads back as the record that produced it.
        let mut reader = MinimalReader::new(&writer.buffer);
        let mut texts = Vec::new();
        let mut counted_ids = Vec::new();
        while reader.more() {
            match reader.key() {
                Some(0) => assert_eq!(reader.int().unwrap(), -1_234_567),
                Some(1) => assert_eq!(reader.uint().unwrap(), 1_767_225_600_123),
                Some(2) => assert!(reader.bool().unwrap()),
                Some(3) => assert_eq!(reader.str().unwrap(), "responses.go:539"),
                Some(4) => {
                    let mut values = Vec::new();
                    reader.ints(&mut values).unwrap();
                    assert_eq!(values, vec![1_234_567, -7_654_321, 3]);
                }
                Some(5) => reader.strs(&mut texts).unwrap(),
                Some(6) => reader.ints(&mut counted_ids).unwrap(),
                Some(7) => assert_eq!(reader.str().unwrap().len(), 3000),
                other => panic!("unexpected key {other:?}"),
            }
        }
        assert_eq!(texts, ["responses.go:539", "no se pudo obtener el registro", ""]);
        assert_eq!(counted_ids, ids);
    }

    /// Sizes past what the old fixed escape could describe are now ordinary.
    #[test]
    fn nothing_has_a_size_ceiling() {
        let big = "y".repeat(1 << 20);
        let mut writer = MinimalWriter::default();
        writer.str(1, &big);
        let mut reader = MinimalReader::new(&writer.buffer);
        assert_eq!(reader.str().unwrap().len(), 1 << 20);

        let values: Vec<i64> = (0..70_000).map(|index| index % 251).collect();
        let mut writer = MinimalWriter::default();
        writer.ints(2, &values);
        let mut reader = MinimalReader::new(&writer.buffer);
        let mut back = Vec::new();
        reader.ints(&mut back).unwrap();
        assert_eq!(back, values);
    }

    /// A continuation run is what an unauthenticated peer controls.
    #[test]
    fn a_runaway_size_is_refused() {
        let mut message = vec![0b0001_1000, 0x00];
        message.extend_from_slice(&[0xFF; 12]);
        let mut reader = MinimalReader::new(&message);
        assert_eq!(reader.bytes(), Err(Error::MinimalSizeTooLarge));

        let mut reader = MinimalReader::new(&[0b0001_1000, 0x00, 0xFF]);
        assert_eq!(reader.bytes(), Err(Error::Truncated));
    }

    #[test]
    fn an_unassigned_size_code_is_refused() {
        let mut reader = MinimalReader::new(&[0b0001_0111]);
        assert_eq!(reader.uint(), Err(Error::MinimalSizeCode(7)));
    }

    /// The vectors the Go writer produces, pasted in verbatim. Everything else
    /// here round-trips through this file's own writer, which would agree with
    /// itself even if both halves drifted from Go together.
    #[test]
    fn decodes_bytes_produced_by_the_go_writer() {
        // kv16EncodeCharge on {CompanyID: 7, UserID: 42, RouteID: 103, CPU: 5,
        // Access1: 0x0139}: keys 0..3 and 6.
        let message = [
            0x08, 0x07, 0x18, 0x2A, 0x28, 0x67, 0x38, 0x05, 0x69, 0x01, 0x39,
        ];
        let mut reader = MinimalReader::new(&message);
        let mut company = 0_u32;
        let mut user = 0_u32;
        let mut route = 0_u16;
        let mut cpu = 0_u16;
        let mut access1 = 0_u16;
        while reader.more() {
            match reader.key() {
                Some(0) => company = reader.u32().unwrap(),
                Some(1) => user = reader.u32().unwrap(),
                Some(2) => route = reader.u16().unwrap(),
                Some(3) => cpu = reader.u16().unwrap(),
                Some(6) => access1 = reader.u16().unwrap(),
                other => panic!("unexpected key {other:?}"),
            }
        }
        assert_eq!((company, user, route, cpu, access1), (7, 42, 103, 5, 0x0139));
    }
}
