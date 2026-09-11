//! Packed-5 frame decoding, mirroring `packed5/decode.go`.
//!
//! A value is one self-delimiting frame. Byte 0 carries the flags and, for all
//! but the longest payloads, the payload length as well:
//!
//! ```text
//! bit  0     PACKED_5             0 = raw payload, 1 = packed stream
//! bit  1     UPPERCASE_DOMINANT   default case of the packed stream
//! bit  2     ENABLE_NUMBER_0_1023 opcode 31 is a 10-bit integer, not '-'
//! bits 3-7   length code          payload byte length, or 31 = "see uvarint"
//! ```
//!
//! The packed payload opens with a 3-bit count of the unused bits in its final
//! byte, which is what terminates the token stream: every 5-bit value is a legal
//! opcode, so trailing zero padding would otherwise decode as extra `a`s.
//!
//! Frames carry bytes rather than runes — the Go codec is byte exact for input
//! that is not valid UTF-8 — so decoding yields a `Vec<u8>` and the caller
//! decides what to do with a frame that is not UTF-8.

use crate::Error;
use crate::bitstream::BitWriter;

const FLAG_PACKED5: u8 = 1 << 0;
const FLAG_UPPERCASE: u8 = 1 << 1;
const FLAG_NUMBER: u8 = 1 << 2;

const LEN_SHIFT: u8 = 3;
/// The largest payload length the header's five length bits can hold.
const LEN_INLINE: usize = 30;
/// The length code that defers to a uvarint.
const LEN_ESCAPE: usize = 31;

const OP_SPACE: u32 = 26;
const OP_CASE_SIMPLE: u32 = 27;
const OP_CASE_LONG: u32 = 28;
const OP_SYMBOL: u32 = 29;
const OP_SIMPLE: u32 = 30;

/// The `simpleTable` index that introduces a raw-byte escape.
const ESCAPE_CODE: u32 = 15;
/// The first unassigned `SYM_TABLE` index.
const SYM_RESERVED: u32 = 30;
/// Width of the pad count that opens a packed stream.
const PAD_BITS_WIDTH: u8 = 3;

/// Opcode 29's operand table. The case mode does not apply to these, which is
/// why the accented capitals go through the escape instead.
const SYM_TABLE: [&str; 30] = [
    "<", ">", "/", "\"", "'", "%", "#", "|", "(", ")", "!", "?", "$", "~", "`", "€", "@", "\\",
    "[", "]", "^", "{", "}", "_", "ñ", "á", "é", "í", "ó", "ú",
];

/// Opcode 30's operand table. Index 15 is not a character; it is [`ESCAPE_CODE`].
const SIMPLE_TABLE: [u8; 15] = [
    b'0', b'1', b'2', b'3', b'4', b'5', b'6', b'7', b'8', b'9', b'.', b'-', b'+', b'*', b'=',
];

/// Decodes one frame from the front of `buf`, returning its bytes together with
/// the number of bytes the frame occupied, so frames can be read back to back.
pub(crate) fn decode(buf: &[u8]) -> Result<(Vec<u8>, usize), Error> {
    let (payload, consumed, header) = frame(buf)?;
    if header & FLAG_PACKED5 == 0 {
        return Ok((payload.to_vec(), consumed));
    }
    let out = decode_stream(
        payload,
        header & FLAG_UPPERCASE != 0,
        header & FLAG_NUMBER != 0,
    )?;
    Ok((out, consumed))
}

/// Splits off the header byte and the length prefix.
fn frame(buf: &[u8]) -> Result<(&[u8], usize, u8), Error> {
    let header = *buf.first().ok_or(Error::BadString("truncated frame"))?;
    let mut pos = 1;
    let mut length = usize::from(header >> LEN_SHIFT);
    if length == LEN_ESCAPE {
        let (value, used) = read_uvarint(&buf[pos..])?;
        // The escape must not re-encode a length the header could have held: one
        // length has one encoding, so a frame has one byte representation.
        if value <= LEN_INLINE {
            return Err(Error::BadString("overlong length prefix"));
        }
        length = value;
        pos += used;
    }
    if length > buf.len() - pos {
        return Err(Error::BadString("truncated frame"));
    }
    Ok((&buf[pos..pos + length], pos + length, header))
}

/// Reads an LEB128 length, rejecting overlong encodings and values that cannot
/// be a length.
fn read_uvarint(buf: &[u8]) -> Result<(usize, usize), Error> {
    let mut value = 0_u64;
    let mut shift = 0_u32;
    for (index, byte) in buf.iter().enumerate() {
        if index >= 9 {
            return Err(Error::BadString("bad length prefix"));
        }
        value |= u64::from(byte & 0x7F) << shift;
        if *byte < 0x80 {
            if value > u64::from(u32::MAX >> 1) {
                return Err(Error::BadString("bad length prefix"));
            }
            return Ok((value as usize, index + 1));
        }
        shift += 7;
    }
    Err(Error::BadString("truncated frame"))
}

/// Walks the packed bitstream. Its whole state is the payload position, the
/// current case mode, and whether a simple toggle is pending.
///
/// A pending simple toggle is cleared only by a letter, and a long toggle leaves
/// it alone. The encoder emits `CASE_TOGGLE_SIMPLE` only immediately before the
/// letter it applies to, so neither case arises in a frame the Go codec wrote;
/// they are defined so a corrupt stream still decodes deterministically.
fn decode_stream(payload: &[u8], upper: bool, number: bool) -> Result<Vec<u8>, Error> {
    let mut reader = StreamReader::new(payload);
    let pad = reader
        .read(PAD_BITS_WIDTH)
        .ok_or(Error::BadString("truncated frame"))?;
    if pad as usize > reader.remaining() {
        return Err(Error::BadString("bad stream padding"));
    }
    reader.limit -= pad as usize;

    let mut out = Vec::new();
    let mut current = upper;
    let mut pending = false;

    while reader.remaining() >= 5 {
        let op = reader.read(5).ok_or(Error::BadString("truncated frame"))?;
        match op {
            op if op < OP_SPACE => {
                // Uppercase exactly when the case mode and the pending toggle disagree.
                let base = if current != pending { b'A' } else { b'a' };
                out.push(base + op as u8);
                pending = false;
            }
            OP_SPACE => out.push(b' '),
            OP_CASE_SIMPLE => pending = true,
            OP_CASE_LONG => current = !current,
            OP_SYMBOL => {
                let index = reader.read(5).ok_or(Error::BadString("truncated frame"))?;
                if index >= SYM_RESERVED {
                    return Err(Error::BadString("reserved symbol index"));
                }
                out.extend_from_slice(SYM_TABLE[index as usize].as_bytes());
            }
            OP_SIMPLE => {
                let index = reader.read(4).ok_or(Error::BadString("truncated frame"))?;
                if index != ESCAPE_CODE {
                    out.push(SIMPLE_TABLE[index as usize]);
                    continue;
                }
                let count = reader.read(2).ok_or(Error::BadString("truncated frame"))?;
                for _ in 0..=count {
                    let byte = reader.read(8).ok_or(Error::BadString("truncated frame"))?;
                    out.push(byte as u8);
                }
            }
            // opNumber: a 10-bit integer, or '-' when the flag is clear.
            _ => {
                if !number {
                    out.push(b'-');
                    continue;
                }
                let value = reader.read(10).ok_or(Error::BadString("truncated frame"))?;
                out.extend_from_slice(value.to_string().as_bytes());
            }
        }
    }
    // Fewer than five bits left is the stream's end; anything else means the pad
    // count did not account for the tokens exactly, which the encoder never does.
    if reader.remaining() != 0 {
        return Err(Error::BadString("truncated frame"));
    }
    Ok(out)
}

/// LSB-first reader over a packed payload. `limit` is the number of bits that
/// are payload rather than trailing pad, so a read past the end of the token
/// stream fails instead of returning zeros.
struct StreamReader<'a> {
    buf: &'a [u8],
    bit: usize,
    limit: usize,
}

impl<'a> StreamReader<'a> {
    fn new(buf: &'a [u8]) -> Self {
        Self {
            buf,
            bit: 0,
            limit: buf.len() * 8,
        }
    }

    fn remaining(&self) -> usize {
        self.limit - self.bit
    }

    /// Returns the next `width` bits (`width <= 24`), or `None` if fewer than
    /// `width` payload bits remain.
    fn read(&mut self, width: u8) -> Option<u32> {
        if self.bit + usize::from(width) > self.limit {
            return None;
        }
        let index = self.bit / 8;
        let shift = (self.bit % 8) as u32;
        let mut window = [0_u8; 8];
        let take = (self.buf.len() - index).min(8);
        window[..take].copy_from_slice(&self.buf[index..index + take]);
        self.bit += usize::from(width);
        Some(((u64::from_le_bytes(window) >> shift) & ((1_u64 << width) - 1)) as u32)
    }
}

// --- encoding ----------------------------------------------------------------

/// Writes `bytes` as a frame with `PACKED_5` clear: the header, the length, and
/// the payload verbatim.
///
/// Written straight through the bitstream rather than built into a scratch
/// buffer first, which is what the Go encoder has to do because `packed5.Append`
/// returns a slice. A raw frame is three sequential pieces, so there is nothing
/// to assemble: the header and the length go out as 8-bit units, which leaves
/// the stream's alignment exactly as it found it, so the payload still takes
/// `put_bytes`'s memcpy path when the frame began byte aligned.
///
/// This is a *frame*, not a bare string — a bare string would not be readable by
/// anything. It is the unpacked branch of the same format, which Go's encoder
/// already emits whenever raw is the cheaper of the two, so it is a path that was
/// on the wire and being read before this encoder existed.
///
/// The packed branch is deliberately not ported. Its encoder is a greedy scan
/// over case-toggle runs, digit runs, a symbol table and multi-byte escapes,
/// which additionally costs the exact size of all four header-flag settings
/// computed per string to pick the cheapest. Skipping it is a CPU win and a size
/// loss: +1 byte on the short identifiers a record usually carries, and up to
/// ~60% on long lowercase text. See `rust/PLAN.md`.
pub(crate) fn write_raw(writer: &mut BitWriter<'_>, bytes: &[u8]) {
    let length = bytes.len();
    if length <= LEN_INLINE {
        writer.chunk((length as u64) << LEN_SHIFT, 8);
    } else {
        // The header's five length bits cannot hold it, so they carry the escape
        // code and a uvarint follows. A length the header *could* have held must
        // not use the escape: one length has one encoding, and the decoder
        // rejects an overlong prefix.
        writer.chunk((LEN_ESCAPE as u64) << LEN_SHIFT, 8);
        let mut remaining = length as u64;
        while remaining >= 0x80 {
            writer.chunk((remaining & 0x7F) | 0x80, 8);
            remaining >>= 7;
        }
        writer.chunk(remaining, 8);
    }
    writer.put_bytes(bytes);
}
