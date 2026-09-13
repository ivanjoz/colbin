//! The Packed-5 string codec, mirroring the Go `packed5` package: a compact,
//! self-delimiting representation for short strings dominated by ASCII letters,
//! spaces, common punctuation, digits and Spanish accented characters.
//!
//! ```
//! use colbin::packed5;
//!
//! let mut buf = Vec::new();
//! packed5::append(&mut buf, b"el nino comio jamon");
//! let (bytes, read) = packed5::decode(&buf)?;
//! assert_eq!(bytes, b"el nino comio jamon");
//! assert_eq!(read, buf.len());
//! # Ok::<(), colbin::Error>(())
//! ```
//!
//! It is an opt-in encoding rather than the default: a raw blob is a sub-slice
//! of the message on the way out and a memcpy on the way in, where a packed one
//! is a pass over every character on both sides. A `BLOB` descriptor carries the
//! choice per field, so a message can hold both and a reader is never told which
//! to expect — see [`crate::wire::Writer8::packed_string`].
//!
//! # Units
//!
//! The alphabet is 32 five-bit codes and **every token is a whole number of
//! them**. Eight units are forty bits are five bytes exactly, so the packer
//! builds one `u64` with fixed shifts, writes eight bytes and advances five,
//! and the reader is the mirror. Nothing straddles the byte grid.
//!
//! # Wire format
//!
//! Byte 0 carries the header flags and, for all but the longest payloads, the
//! payload length as well:
//!
//! ```text
//! bit  0     PACKED_5     0 = raw payload, 1 = packed unit stream
//! bit  1     UPPERCASE    the stream starts in uppercase mode
//! bit  2     reserved     must be zero
//! bits 3-7   length code  payload byte length, or 31 = "see uvarint"
//! ```
//!
//! In packed mode the payload is the unit stream, five bits per unit, LSB-first
//! within each byte, starting at bit 0 of the first payload byte. There is no
//! pad prefix: the unit count is `payload.len() * 8 / 5`, because the encoder
//! pads to the grid with a trailing `CASE_TOGGLE_SIMPLE`, which applies to no
//! letter and so decodes to nothing.
//!
//! ```text
//! 0..25   letter a..z, cased by the current case mode      1 unit
//! 26      space                                            1 unit
//! 27      CASE_TOGGLE_SIMPLE   the next letter only         1 unit
//! 28      CASE_TOGGLE_LONG     until the next one           1 unit
//! 29      + symbol table index                             2 units
//! 30      + extension table index, or 31 to escape         2 units
//! 31      + a 10-bit integer, low five bits first          3 units
//! ```
//!
//! A raw escape is `30, 31`, one unit holding `n-1` for `n` in 1..=4, then two
//! units per byte — the low five bits, then the high three. It carries **bytes**,
//! not runes, which is what makes the codec byte exact for input that is not
//! valid UTF-8 — and why this module works in `[u8]` and leaves the caller to
//! decide what a frame that is not UTF-8 means.

use crate::error::Packed5Error;

// Header flag bits, in byte 0 of every frame.
const FLAG_PACKED5: u8 = 1 << 0;
const FLAG_UPPERCASE: u8 = 1 << 1;
const FLAG_RESERVED: u8 = 1 << 2;

const LEN_SHIFT: u8 = 3;
const LEN_INLINE: usize = 30;
const LEN_ESCAPE: usize = 31;

// The alphabet.
const OP_SPACE: u8 = 26;
const OP_CASE_SIMPLE: u8 = 27;
const OP_CASE_LONG: u8 = 28;
const OP_SYMBOL: u8 = 29;
const OP_EXT: u8 = 30;
const OP_NUMBER: u8 = 31;

/// The extension table operand that introduces a run of raw bytes.
const EXT_ESCAPE: u8 = 31;
/// The first unassigned extension table index.
const EXT_RESERVED: u8 = 28;

const MAX_ESCAPE_RUN: usize = 4;
const NUMBER_MAX: u32 = 1023;
const NUMBER_MAX_DIGITS: usize = 4;

/// The opcode 29 operand table: what a short record is made of once letters and
/// spaces are accounted for. All 32 entries are assigned.
const SYM_TABLE: [u8; 32] = [
    b'0', b'1', b'2', b'3', b'4', b'5', b'6', b'7', b'8', b'9', b'.', b',', b'-', b'/', b':', b';',
    b'_', b'(', b')', b'%', b'#', b'"', b'\'', b'!', b'?', b'@', b'=', b'+', b'*', b'&', b'<', b'>',
];

/// The opcode 30 operand table. Indices 28..=30 are reserved and 31 is the
/// escape, so a decoder rejects them rather than reinterpreting them later.
const EXT_TABLE: [&str; 28] = [
    "ñ", "á", "é", "í", "ó", "ú", "ü", "Ñ", "Á", "É", "Í", "Ó", "Ú", "€", "$", "~", "`", "\\", "[",
    "]", "^", "{", "}", "|", "\n", "\t", "\r", "¿",
];

/// The multi-byte entries of `EXT_TABLE`, matched on their UTF-8 bytes.
const EXT_MULTI: [(&[u8], u8); 15] = [
    ("ñ".as_bytes(), 0),
    ("á".as_bytes(), 1),
    ("é".as_bytes(), 2),
    ("í".as_bytes(), 3),
    ("ó".as_bytes(), 4),
    ("ú".as_bytes(), 5),
    ("ü".as_bytes(), 6),
    ("Ñ".as_bytes(), 7),
    ("Á".as_bytes(), 8),
    ("É".as_bytes(), 9),
    ("Í".as_bytes(), 10),
    ("Ó".as_bytes(), 11),
    ("Ú".as_bytes(), 12),
    ("€".as_bytes(), 13),
    ("¿".as_bytes(), 27),
];

fn is_letter(c: u8) -> bool {
    (c | 0x20).wrapping_sub(b'a') < 26
}

fn is_digit(c: u8) -> bool {
    c.wrapping_sub(b'0') < 10
}

fn is_upper(c: u8) -> bool {
    c & 0x20 == 0
}

fn letter_index(c: u8) -> u8 {
    (c | 0x20) - b'a'
}

/// The symbol table index of an ASCII byte, or `None`.
fn ascii_sym(c: u8) -> Option<u8> {
    SYM_TABLE.iter().position(|&s| s == c).map(|i| i as u8)
}

/// The extension table index of a single-byte entry, or `None`.
fn ascii_ext(c: u8) -> Option<u8> {
    if c >= 0x80 {
        return None;
    }
    EXT_TABLE
        .iter()
        .position(|s| s.as_bytes() == [c])
        .map(|i| i as u8)
}

/// The extension table index of the multi-byte character at `bytes[at]`, with
/// its width.
fn ext_multi(bytes: &[u8], at: usize) -> Option<(u8, usize)> {
    for (utf8, index) in EXT_MULTI {
        if bytes.len() - at >= utf8.len() && &bytes[at..at + utf8.len()] == utf8 {
            return Some((index, utf8.len()));
        }
    }
    None
}

/// Whether the byte at `at` has no token of its own.
fn escapes(bytes: &[u8], at: usize) -> bool {
    let c = bytes[at];
    if is_letter(c) || c == b' ' {
        return false;
    }
    if c < 0x80 {
        return ascii_sym(c).is_none() && ascii_ext(c).is_none();
    }
    ext_multi(bytes, at).is_none()
}

/// How many units a payload of `n` bytes holds. The encoder pads to the grid, so
/// this is exact rather than an upper bound.
fn payload_units(n: usize) -> usize {
    n * 8 / 5
}

/// How many bytes `n` units occupy.
fn payload_bytes(n: usize) -> usize {
    (n * 5).div_ceil(8)
}

/// Whether the stream should open in uppercase: the first letter's case.
///
/// It is never worse than starting lower. A leading run of `k` opposite-case
/// letters costs `2k` units from lower for `k` of one or two and `1+k` from
/// three up, where starting in that mode costs `k` plus the one toggle that
/// returns — equal at `k>=3` and strictly better below it.
fn opens_upper(bytes: &[u8]) -> bool {
    bytes
        .iter()
        .find(|&&c| is_letter(c))
        .is_some_and(|&c| is_upper(c))
}

/// Packs units into a byte buffer, eight at a time.
struct Writer {
    out: Vec<u8>,
    acc: u64,
    pending: u8,
    units: usize,
}

impl Writer {
    fn new(capacity: usize) -> Self {
        Self {
            out: Vec::with_capacity(capacity),
            acc: 0,
            pending: 0,
            units: 0,
        }
    }

    fn put(&mut self, unit: u8) {
        self.acc |= u64::from(unit) << (5 * self.pending);
        self.pending += 1;
        self.units += 1;
        if self.pending == 8 {
            self.out.extend_from_slice(&self.acc.to_le_bytes()[..5]);
            self.acc = 0;
            self.pending = 0;
        }
    }

    /// Flushes the partial group and pads the stream to the unit grid.
    fn finish(mut self) -> Vec<u8> {
        if payload_units(payload_bytes(self.units)) > self.units {
            self.put(OP_CASE_SIMPLE);
        }
        if self.pending > 0 {
            let width = payload_bytes(usize::from(self.pending));
            self.out.extend_from_slice(&self.acc.to_le_bytes()[..width]);
        }
        self.out
    }
}

/// The greedy left-to-right scan, fused with the packer. There is no planning
/// pass: the format has no behavioural header flags left to price.
fn tokenize(w: &mut Writer, bytes: &[u8], upper: bool) {
    let mut cur = upper;
    let mut at = 0;
    while at < bytes.len() {
        let c = bytes[at];
        if is_letter(c) {
            let index = letter_index(c);
            if is_upper(c) == cur {
                w.put(index);
                at += 1;
                continue;
            }
            // Two simple toggles cost what a pair of long ones does, so the long
            // form only pays from a run of three.
            let mut run = 0;
            let mut scan = at;
            while scan < bytes.len() && is_letter(bytes[scan]) && is_upper(bytes[scan]) != cur {
                run += 1;
                scan += 1;
            }
            if run >= 3 {
                w.put(OP_CASE_LONG);
                cur = !cur;
                continue; // re-read the letter, now in the matching mode
            }
            w.put(OP_CASE_SIMPLE);
            w.put(index);
            at += 1;
        } else if c == b' ' {
            w.put(OP_SPACE);
            at += 1;
        } else if is_digit(c) {
            // The longest prefix ten bits can hold. A token may not carry a
            // leading zero, since it decodes as a plain decimal integer and
            // "00123" must not come back as "123"; a lone "0" is fine.
            let (mut value, mut best, mut best_len) = (0u32, 0u32, 0usize);
            for len in 1..=NUMBER_MAX_DIGITS {
                if at + len > bytes.len() {
                    break;
                }
                let digit = bytes[at + len - 1];
                if !is_digit(digit) || (len > 1 && bytes[at] == b'0') {
                    break;
                }
                value = value * 10 + u32::from(digit - b'0');
                if value > NUMBER_MAX {
                    break;
                }
                best = value;
                best_len = len;
            }
            if best_len == 1 {
                w.put(OP_SYMBOL);
                w.put(c - b'0');
            } else {
                w.put(OP_NUMBER);
                w.put((best & 31) as u8);
                w.put((best >> 5) as u8);
            }
            at += best_len;
        } else if let Some(index) = ascii_sym(c) {
            w.put(OP_SYMBOL);
            w.put(index);
            at += 1;
        } else if let Some(index) = ascii_ext(c) {
            w.put(OP_EXT);
            w.put(index);
            at += 1;
        } else if let Some((index, width)) = ext_multi(bytes, at) {
            w.put(OP_EXT);
            w.put(index);
            at += width;
        } else {
            let mut run = 1;
            while run < MAX_ESCAPE_RUN && at + run < bytes.len() && escapes(bytes, at + run) {
                run += 1;
            }
            w.put(OP_EXT);
            w.put(EXT_ESCAPE);
            w.put((run - 1) as u8);
            for k in 0..run {
                let b = bytes[at + k];
                w.put(b & 31);
                w.put(b >> 5);
            }
            at += run;
        }
    }
}

/// The packed unit stream for `bytes`, with the case mode it opens in, or `None`
/// when the packed form would not be smaller than the raw bytes.
///
/// This is the form an embedded string takes: a container that already carries a
/// length and a place to record the case mode — a `BLOB` descriptor does both —
/// needs no frame header of its own.
pub fn payload(bytes: &[u8]) -> Option<(Vec<u8>, bool)> {
    if bytes.is_empty() {
        return None;
    }
    let upper = opens_upper(bytes);
    let mut w = Writer::new(bytes.len() + 8);
    tokenize(&mut w, bytes, upper);
    let out = w.finish();
    if out.len() >= bytes.len() {
        return None;
    }
    Some((out, upper))
}

/// Appends one self-delimiting frame for `bytes`.
///
/// The packed form is used only when its frame is strictly smaller, so this
/// never enlarges a string.
pub fn append(out: &mut Vec<u8>, bytes: &[u8]) {
    if bytes.is_empty() {
        out.push(0); // raw, empty payload
        return;
    }
    match payload(bytes) {
        Some((stream, upper)) => {
            let mut flags = FLAG_PACKED5;
            if upper {
                flags |= FLAG_UPPERCASE;
            }
            append_header(out, flags, stream.len());
            out.extend_from_slice(&stream);
        }
        None => {
            append_header(out, 0, bytes.len());
            out.extend_from_slice(bytes);
        }
    }
}

/// The number of bytes [`append`] would add for `bytes`.
pub fn size(bytes: &[u8]) -> usize {
    if bytes.is_empty() {
        return 1;
    }
    let raw = frame_overhead(bytes.len()) + bytes.len();
    match payload(bytes) {
        Some((stream, _)) => {
            let packed = frame_overhead(stream.len()) + stream.len();
            if packed < raw { packed } else { raw }
        }
        None => raw,
    }
}

fn append_header(out: &mut Vec<u8>, flags: u8, payload_len: usize) {
    if payload_len <= LEN_INLINE {
        out.push(flags | (payload_len as u8) << LEN_SHIFT);
        return;
    }
    out.push(flags | (LEN_ESCAPE as u8) << LEN_SHIFT);
    append_uvarint(out, payload_len);
}

fn append_uvarint(out: &mut Vec<u8>, mut value: usize) {
    while value >= 0x80 {
        out.push((value as u8) | 0x80);
        value >>= 7;
    }
    out.push(value as u8);
}

fn uvarint_len(mut value: usize) -> usize {
    let mut n = 1;
    while value >= 0x80 {
        value >>= 7;
        n += 1;
    }
    n
}

fn frame_overhead(n: usize) -> usize {
    if n <= LEN_INLINE {
        1
    } else {
        1 + uvarint_len(n)
    }
}

fn read_uvarint(buf: &[u8]) -> Result<(usize, usize), Packed5Error> {
    let mut value: u64 = 0;
    let mut shift = 0;
    for (i, &b) in buf.iter().enumerate() {
        if i >= 9 {
            return Err(Packed5Error::BadLength);
        }
        value |= u64::from(b & 0x7F) << shift;
        if b < 0x80 {
            if value > u64::from(u32::MAX >> 1) {
                return Err(Packed5Error::BadLength);
            }
            return Ok((value as usize, i + 1));
        }
        shift += 7;
    }
    Err(Packed5Error::Truncated)
}

/// One parsed frame prefix.
struct Frame<'a> {
    payload: &'a [u8],
    total: usize,
    packed: bool,
    upper: bool,
}

fn frame(buf: &[u8]) -> Result<Frame<'_>, Packed5Error> {
    let &header = buf.first().ok_or(Packed5Error::Truncated)?;
    if header & FLAG_RESERVED != 0 {
        return Err(Packed5Error::BadHeader);
    }
    let mut at = 1;
    let mut length = usize::from(header >> LEN_SHIFT);
    if length == LEN_ESCAPE {
        let (value, read) = read_uvarint(&buf[at..])?;
        // The escape must not re-encode a length the header could have held; one
        // length has one encoding, so a frame has one byte representation.
        if value <= LEN_INLINE {
            return Err(Packed5Error::BadLength);
        }
        length = value;
        at += read;
    }
    if length > buf.len() - at {
        return Err(Packed5Error::Truncated);
    }
    Ok(Frame {
        payload: &buf[at..at + length],
        total: at + length,
        packed: header & FLAG_PACKED5 != 0,
        upper: header & FLAG_UPPERCASE != 0,
    })
}

/// Unpacks the payload into units, eight at a time.
fn units_of(payload: &[u8]) -> Vec<u8> {
    let count = payload_units(payload.len());
    let mut units = Vec::with_capacity(count);
    let mut at = 0;
    while units.len() < count {
        let mut word = [0u8; 8];
        let take = (payload.len() - at).min(8);
        word[..take].copy_from_slice(&payload[at..at + take]);
        let value = u64::from_le_bytes(word);
        for k in 0..8 {
            if units.len() == count {
                break;
            }
            units.push(((value >> (5 * k)) & 31) as u8);
        }
        at += 5;
    }
    units
}

/// Walks a bare unit stream — a payload with no frame header, as [`payload`]
/// writes it — and appends the bytes it holds to `dst`.
pub fn append_string(
    dst: &mut Vec<u8>,
    payload: &[u8],
    upper: bool,
) -> Result<(), Packed5Error> {
    let units = units_of(payload);
    let mut cur = upper;
    let mut pending = false;
    let mut at = 0;
    while at < units.len() {
        let op = units[at];
        at += 1;
        match op {
            0..=25 => {
                // Uppercase exactly when the two disagree.
                dst.push(if cur != pending { b'A' } else { b'a' } + op);
                pending = false;
            }
            OP_SPACE => dst.push(b' '),
            OP_CASE_SIMPLE => pending = true,
            OP_CASE_LONG => cur = !cur,
            OP_SYMBOL => {
                let &index = units.get(at).ok_or(Packed5Error::Truncated)?;
                at += 1;
                dst.push(SYM_TABLE[usize::from(index)]);
            }
            OP_EXT => {
                let &index = units.get(at).ok_or(Packed5Error::Truncated)?;
                at += 1;
                if index != EXT_ESCAPE {
                    if index >= EXT_RESERVED {
                        return Err(Packed5Error::ReservedSymbol);
                    }
                    dst.extend_from_slice(EXT_TABLE[usize::from(index)].as_bytes());
                    continue;
                }
                let &count = units.get(at).ok_or(Packed5Error::Truncated)?;
                at += 1;
                let count = usize::from(count) + 1;
                if count > MAX_ESCAPE_RUN {
                    return Err(Packed5Error::BadEscape);
                }
                if at + 2 * count > units.len() {
                    return Err(Packed5Error::Truncated);
                }
                for _ in 0..count {
                    let (low, high) = (units[at], units[at + 1]);
                    if high > 7 {
                        return Err(Packed5Error::BadEscape); // a byte is five bits and three
                    }
                    dst.push(low | high << 5);
                    at += 2;
                }
            }
            _ => {
                if at + 2 > units.len() {
                    return Err(Packed5Error::Truncated);
                }
                let value = u32::from(units[at]) | u32::from(units[at + 1]) << 5;
                at += 2;
                let mut buf = [0u8; 4];
                let mut len = 0;
                let mut v = value;
                loop {
                    buf[len] = b'0' + (v % 10) as u8;
                    len += 1;
                    v /= 10;
                    if v == 0 {
                        break;
                    }
                }
                for k in (0..len).rev() {
                    dst.push(buf[k]);
                }
            }
        }
    }
    Ok(())
}

/// Reads one frame from the front of `buf`, returning its bytes and the number
/// of bytes consumed, so frames can be read back to back.
pub fn decode(buf: &[u8]) -> Result<(Vec<u8>, usize), Packed5Error> {
    let mut out = Vec::new();
    let read = append_decoded(&mut out, buf)?;
    Ok((out, read))
}

/// [`decode`] appending onto a caller's buffer, for a run of frames gathered
/// into one backing array.
pub fn append_decoded(dst: &mut Vec<u8>, buf: &[u8]) -> Result<usize, Packed5Error> {
    let frame = frame(buf)?;
    if !frame.packed {
        dst.extend_from_slice(frame.payload);
        return Ok(frame.total);
    }
    append_string(dst, frame.payload, frame.upper)?;
    Ok(frame.total)
}
