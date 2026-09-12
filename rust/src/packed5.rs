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
//! # Wire format
//!
//! Byte 0 carries the header flags and, for all but the longest payloads, the
//! payload length as well:
//!
//! ```text
//! bit  0     PACKED_5             0 = raw payload, 1 = packed stream
//! bit  1     UPPERCASE_DOMINANT   default case of the packed stream
//! bit  2     ENABLE_NUMBER_0_1023 opcode 31 is a 10-bit integer, not '-'
//! bits 3-7   length code          payload byte length, or 31 = "see uvarint"
//! ```
//!
//! In packed mode the payload is a bitstream, LSB-first within each byte: a
//! 3-bit count of the unused bits in the final byte, then 5-bit opcodes with
//! their operands. That pad count is what terminates the stream — every 5-bit
//! value is a legal opcode, so trailing zero padding would otherwise decode as
//! extra `a`s.
//!
//! ```text
//! 0..25   letter a..z, cased by the current case mode
//! 26      space
//! 27      CASE_TOGGLE_SIMPLE   invert the case of the next letter only
//! 28      CASE_TOGGLE_LONG     invert the case mode until the next 28
//! 29      + 5-bit index into the symbol table
//! 30      + 4-bit index into the simple table; index 15 starts a UTF-8 escape
//! 31      + 10-bit integer if ENABLE_NUMBER_0_1023, otherwise '-'
//! ```
//!
//! The escape carries **bytes**, not runes, which is what makes the codec byte
//! exact for input that is not valid UTF-8 — and why this module works in
//! `[u8]` and leaves the caller to decide what a frame that is not UTF-8 means.

use crate::error::Packed5Error;

// Header flag bits, in byte 0 of every frame.
const FLAG_PACKED5: u8 = 1 << 0;
const FLAG_UPPERCASE: u8 = 1 << 1;
const FLAG_NUMBER: u8 = 1 << 2;

const LEN_SHIFT: u8 = 3;
/// The largest payload length the header's five length bits can hold; 31 is the
/// escape code that defers to a uvarint.
const LEN_INLINE: usize = 30;
const LEN_ESCAPE: usize = 31;

// Base alphabet opcodes above the 26 letters.
const OP_SPACE: u32 = 26;
const OP_CASE_SIMPLE: u32 = 27;
const OP_CASE_LONG: u32 = 28;
const OP_SYMBOL: u32 = 29;
const OP_SIMPLE: u32 = 30;
const OP_NUMBER: u32 = 31; // NUMBER_0_1023, or '-' when the flag is clear

/// The simple-table index that introduces a raw-byte escape.
const ESCAPE_CODE: u32 = 15;
/// How many raw bytes one escape can carry; the count is stored as 2 bits.
const MAX_ESCAPE_RUN: usize = 4;
/// The first unassigned symbol-table index.
const SYM_RESERVED: u32 = 30;
/// The width of the pad count that opens a packed stream.
const PAD_BITS_WIDTH: u8 = 3;

/// The largest integer opcode 31 can carry in its ten bits, and the digit count
/// of that largest integer.
const NUMBER_MAX: u32 = 1023;
const NUMBER_MAX_DIGITS: usize = 4;

/// Opcode 29's operand table. The case mode does not apply to these, which is
/// why the accented capitals go through the escape instead.
const SYM_TABLE: [&str; 30] = [
    "<", ">", "/", "\"", "'", "%", "#", "|", "(", ")", "!", "?", "$", "~", "`", "€", "@", "\\",
    "[", "]", "^", "{", "}", "_", "ñ", "á", "é", "í", "ó", "ú",
];

/// Opcode 30's operand table. Index 15 is not a character; it is
/// [`ESCAPE_CODE`].
const SIMPLE_TABLE: [u8; 15] = [
    b'0', b'1', b'2', b'3', b'4', b'5', b'6', b'7', b'8', b'9', b'.', b'-', b'+', b'*', b'=',
];

/// The non-ASCII symbol-table entries, as the UTF-8 bytes they are matched by.
/// Go decodes a rune and looks it up; matching the sequences is the same answer
/// without a decoder, because every other multi-byte rune escapes anyway.
const RUNE_SYMBOLS: [(&[u8], u32); 7] = [
    ("€".as_bytes(), 15),
    ("ñ".as_bytes(), 24),
    ("á".as_bytes(), 25),
    ("é".as_bytes(), 26),
    ("í".as_bytes(), 27),
    ("ó".as_bytes(), 28),
    ("ú".as_bytes(), 29),
];

// Token bit costs. A letter, a space, a long toggle and a bare '-' are a lone
// 5-bit opcode; every other token carries an operand.
const COST_LETTER: usize = 5;
const COST_LETTER_CASED: usize = 10; // CASE_TOGGLE_SIMPLE + letter
const COST_SPACE: usize = 5;
const COST_TOGGLE_LONG: usize = 5;
const COST_SYMBOL: usize = 10; // opcode + 5-bit index
const COST_SIMPLE: usize = 9; // opcode + 4-bit index
const COST_DASH: usize = 5;
const COST_NUMBER: usize = 15; // opcode + 10-bit integer
const COST_ESCAPE_BASE: usize = 11; // opcode + escape code + 2-bit count
const COST_ESCAPE_BYTE: usize = 8;

/// Reports whether `c` is an ASCII letter of either case. Setting bit 5 folds
/// `A..Z` onto `a..z` and moves nothing else into that range, so one unsigned
/// range test decides both cases at once.
const fn is_letter(c: u8) -> bool {
    (c | 0x20).wrapping_sub(b'a') < 26
}

const fn is_digit(c: u8) -> bool {
    c.wrapping_sub(b'0') < 10
}

/// The case of a byte already known to be an ASCII letter: for those, bit 5 is
/// the case bit.
const fn is_upper(c: u8) -> bool {
    c & 0x20 == 0
}

/// The 0..=25 alphabet position of an ASCII letter, either case.
const fn letter_index(c: u8) -> u32 {
    (c | 0x20) as u32 - b'a' as u32
}

/// What the byte at `i` costs and how it is spelled, or `None` when it has no
/// token of its own and must be escaped.
enum Symbol {
    /// Opcode 31 as `'-'`, which costs five bits rather than the nine the simple
    /// table would charge. Only reachable with number mode off.
    Dash,
    /// Opcode 29 and a 5-bit index.
    Sym(u32),
    /// Opcode 30 and a 4-bit index.
    Simple(u32),
}

/// Returns the non-letter, non-space token for the byte at `i` with its width in
/// bytes and its bit cost.
fn symbol_at(s: &[u8], i: usize, number: bool) -> Option<(Symbol, usize, usize)> {
    let c = s[i];
    if c < 0x80 {
        // With number mode off, opcode 31 carries '-' for five bits.
        if !number && c == b'-' {
            return Some((Symbol::Dash, 1, COST_DASH));
        }
        if let Some(index) = ascii_symbol(c) {
            return Some((Symbol::Sym(index), 1, COST_SYMBOL));
        }
        if let Some(index) = ascii_simple(c) {
            return Some((Symbol::Simple(index), 1, COST_SIMPLE));
        }
        return None;
    }
    for (bytes, index) in RUNE_SYMBOLS {
        if s[i..].starts_with(bytes) {
            return Some((Symbol::Sym(index), bytes.len(), COST_SYMBOL));
        }
    }
    None
}

/// The symbol-table index of a single-byte entry.
fn ascii_symbol(c: u8) -> Option<u32> {
    #[allow(clippy::cast_possible_truncation)]
    SYM_TABLE
        .iter()
        .take(SYM_RESERVED as usize)
        .position(|entry| entry.len() == 1 && entry.as_bytes()[0] == c)
        .map(|index| index as u32)
}

/// The simple-table index of a byte.
fn ascii_simple(c: u8) -> Option<u32> {
    #[allow(clippy::cast_possible_truncation)]
    SIMPLE_TABLE
        .iter()
        .position(|entry| *entry == c)
        .map(|index| index as u32)
}

/// Reports whether the byte at `i` has no token of its own.
fn must_escape(s: &[u8], i: usize, number: bool) -> bool {
    if is_letter(s[i]) || s[i] == b' ' {
        return false;
    }
    symbol_at(s, i, number).is_none()
}

/// Appends `bytes` as one self-delimiting frame.
///
/// The packed form is used only when its frame is strictly smaller than the raw
/// one, so this never inflates a string: the result is never longer than
/// `bytes.len()` plus the framing.
#[allow(clippy::cast_possible_truncation)]
pub fn append(out: &mut Vec<u8>, bytes: &[u8]) {
    if bytes.is_empty() {
        out.push(0); // raw, empty payload
        return;
    }
    let (bits, upper, number) = plan(bytes);
    let payload = payload_bytes(bits);
    if frame_overhead(payload) + payload >= frame_overhead(bytes.len()) + bytes.len() {
        append_header(out, 0, bytes.len());
        out.extend_from_slice(bytes);
        return;
    }

    let mut flags = FLAG_PACKED5;
    if upper {
        flags |= FLAG_UPPERCASE;
    }
    if number {
        flags |= FLAG_NUMBER;
    }
    append_header(out, flags, payload);

    // The stream is exactly `payload` bytes, so one reservation replaces the
    // repeated growth an unsized append would do while the tokens are written.
    out.reserve(payload);
    let mut writer = BitWriter::new(out);
    writer.write_bits(
        (payload * 8 - usize::from(PAD_BITS_WIDTH) - bits) as u32,
        PAD_BITS_WIDTH,
    );
    write_stream(&mut writer, bytes, upper, number);
    writer.flush();
}

/// The number of bytes [`append`] would add, without encoding.
pub fn size(bytes: &[u8]) -> usize {
    if bytes.is_empty() {
        return 1;
    }
    let (bits, _, _) = plan(bytes);
    let payload = payload_bytes(bits);
    let raw = frame_overhead(bytes.len()) + bytes.len();
    let packed = frame_overhead(payload) + payload;
    if packed < raw { packed } else { raw }
}

/// The packed payload size for a token cost of `bits`.
const fn payload_bytes(bits: usize) -> usize {
    (PAD_BITS_WIDTH as usize + bits).div_ceil(8)
}

/// The number of framing bytes a payload of `n` bytes needs: the header byte,
/// plus a uvarint once the length outgrows the header's length bits.
const fn frame_overhead(n: usize) -> usize {
    if n <= LEN_INLINE {
        1
    } else {
        1 + uvarint_len(n)
    }
}

const fn uvarint_len(mut value: usize) -> usize {
    let mut length = 1;
    while value >= 0x80 {
        value >>= 7;
        length += 1;
    }
    length
}

/// Writes the header byte and, when the payload length outgrows the header's
/// five length bits, the uvarint that carries it.
#[allow(clippy::cast_possible_truncation)]
fn append_header(out: &mut Vec<u8>, flags: u8, payload_len: usize) {
    if payload_len <= LEN_INLINE {
        out.push(flags | (payload_len as u8) << LEN_SHIFT);
        return;
    }
    out.push(flags | (LEN_ESCAPE as u8) << LEN_SHIFT);
    let mut value = payload_len;
    while value >= 0x80 {
        out.push((value as u8) | 0x80);
        value >>= 7;
    }
    out.push(value as u8);
}

/// Performs the greedy walk and emits each token as bits at the point it is
/// decided.
///
/// It follows the rules the specification lays out: an opposite-case run of one
/// or two letters takes a `CASE_TOGGLE_SIMPLE` each and three or more takes a
/// `CASE_TOGGLE_LONG`; a decimal run takes the longest prefix `NUMBER_0_1023`
/// can legally carry, except that a lone digit takes the cheaper 4-bit simple
/// symbol; and a byte with no token of its own is escaped, merged with the
/// following unrepresentable bytes up to the escape's four-byte limit.
#[allow(clippy::cast_possible_truncation)]
fn write_stream(writer: &mut BitWriter<'_>, s: &[u8], upper: bool, number: bool) {
    let mut current = upper;
    let mut i = 0;
    while i < s.len() {
        let c = s[i];
        if is_letter(c) {
            let index = letter_index(c);
            if is_upper(c) == current {
                writer.write_bits(index, 5);
                i += 1;
                continue;
            }
            // The length of the consecutive opposite-case letter run at i. Two
            // simple toggles cost the same as a pair of long ones, so the long
            // form only wins from three.
            let mut run = 0;
            let mut j = i;
            while j < s.len() && is_letter(s[j]) && is_upper(s[j]) != current {
                run += 1;
                j += 1;
            }
            if run >= 3 {
                writer.write_bits(OP_CASE_LONG, 5);
                current = !current;
                continue; // re-read the letter, now in the matching mode
            }
            writer.write_bits(OP_CASE_SIMPLE, 5);
            writer.write_bits(index, 5);
            i += 1;
            continue;
        }
        if c == b' ' {
            writer.write_bits(OP_SPACE, 5);
            i += 1;
            continue;
        }
        if number && is_digit(c) {
            // The longest prefix that fits in ten bits. A token may not carry a
            // leading zero, since it decodes as a plain decimal integer and
            // "00123" must not come back as "123"; a lone "0" is fine.
            let (best, best_len) = longest_number(s, i, s.len());
            if best_len == 1 {
                writer.write_bits(OP_SIMPLE, 5);
                writer.write_bits(ascii_simple(c).expect("a digit is in the simple table"), 4);
            } else {
                writer.write_bits(OP_NUMBER, 5);
                writer.write_bits(best, 10);
            }
            i += best_len;
            continue;
        }
        if let Some((symbol, width, _)) = symbol_at(s, i, number) {
            match symbol {
                Symbol::Dash => writer.write_bits(OP_NUMBER, 5),
                Symbol::Sym(index) => {
                    writer.write_bits(OP_SYMBOL, 5);
                    writer.write_bits(index, 5);
                }
                Symbol::Simple(index) => {
                    writer.write_bits(OP_SIMPLE, 5);
                    writer.write_bits(index, 4);
                }
            }
            i += width;
            continue;
        }
        let mut n = 1;
        while n < MAX_ESCAPE_RUN && i + n < s.len() && must_escape(s, i + n, number) {
            n += 1;
        }
        writer.write_bits(OP_SIMPLE, 5);
        writer.write_bits(ESCAPE_CODE, 4);
        writer.write_bits(n as u32 - 1, 2);
        for k in 0..n {
            writer.write_bits(u32::from(s[i + k]), 8);
        }
        i += n;
    }
}

/// The longest decimal prefix at `i` that `NUMBER_0_1023` can legally carry,
/// with its digit count. `end` bounds the run.
fn longest_number(s: &[u8], i: usize, end: usize) -> (u32, usize) {
    let mut value = 0_u32;
    let mut best = 0_u32;
    let mut best_len = 0;
    for length in 1..=NUMBER_MAX_DIGITS {
        if i + length > end {
            break;
        }
        let digit = s[i + length - 1];
        if !is_digit(digit) || (length > 1 && s[i] == b'0') {
            break;
        }
        value = value * 10 + u32::from(digit - b'0');
        if value > NUMBER_MAX {
            break;
        }
        best = value;
        best_len = length;
    }
    (best, best_len)
}

/// Computes the exact greedy-scan cost for all four flag combinations in one
/// pass, and returns the cheapest.
///
/// Case costs depend only on homogeneous ASCII-letter runs; number mode affects
/// only decimal runs and the spelling of `'-'`. Keeping those two parts separate
/// avoids costing candidates that will be discarded. Counting letters to pick
/// the dominant case is the obvious shortcut and it is wrong often enough to
/// matter: what decides the flag is the number of case *runs*, not of letters.
fn plan(s: &[u8]) -> (usize, bool, bool) {
    let (mut lower_case_bits, mut upper_case_bits) = (0_usize, 0_usize);
    let (mut lower_mode, mut upper_mode) = (false, true);
    let (mut plain_bits, mut number_bits) = (0_usize, 0_usize);

    let mut i = 0;
    while i < s.len() {
        let c = s[i];
        if is_letter(c) {
            let run_upper = is_upper(c);
            let mut j = i + 1;
            while j < s.len() && is_letter(s[j]) && is_upper(s[j]) == run_upper {
                j += 1;
            }
            let run = j - i;
            if run_upper == lower_mode {
                lower_case_bits += COST_LETTER * run;
            } else if run >= 3 {
                lower_case_bits += COST_TOGGLE_LONG + COST_LETTER * run;
                lower_mode = run_upper;
            } else {
                lower_case_bits += COST_LETTER_CASED * run;
            }
            if run_upper == upper_mode {
                upper_case_bits += COST_LETTER * run;
            } else if run >= 3 {
                upper_case_bits += COST_TOGGLE_LONG + COST_LETTER * run;
                upper_mode = run_upper;
            } else {
                upper_case_bits += COST_LETTER_CASED * run;
            }
            i = j;
            continue;
        }

        if c == b' ' {
            plain_bits += COST_SPACE;
            number_bits += COST_SPACE;
            i += 1;
            continue;
        }

        if is_digit(c) {
            let mut j = i + 1;
            while j < s.len() && is_digit(s[j]) {
                j += 1;
            }
            plain_bits += COST_SIMPLE * (j - i);
            while i < j {
                let (_, best_len) = longest_number(s, i, j);
                if best_len == 1 {
                    number_bits += COST_SIMPLE;
                } else {
                    number_bits += COST_NUMBER;
                }
                i += best_len;
            }
            continue;
        }

        if let Some((_, width, cost)) = symbol_at(s, i, false) {
            plain_bits += cost;
            if c == b'-' {
                number_bits += COST_SIMPLE;
            } else {
                number_bits += cost;
            }
            i += width;
            continue;
        }

        let mut n = 1;
        while n < MAX_ESCAPE_RUN && i + n < s.len() && must_escape(s, i + n, false) {
            n += 1;
        }
        let cost = COST_ESCAPE_BASE + COST_ESCAPE_BYTE * n;
        plain_bits += cost;
        number_bits += cost;
        i += n;
    }

    let mut bits = lower_case_bits + plain_bits;
    let (mut upper, mut number) = (false, false);
    if upper_case_bits + plain_bits < bits {
        bits = upper_case_bits + plain_bits;
        upper = true;
    }
    if lower_case_bits + number_bits < bits {
        bits = lower_case_bits + number_bits;
        upper = false;
        number = true;
    }
    if upper_case_bits + number_bits < bits {
        bits = upper_case_bits + number_bits;
        upper = true;
        number = true;
    }
    (bits, upper, number)
}

/// Decodes one frame from the front of `buf`, returning its bytes together with
/// the number of bytes the frame occupied, so frames can be read back to back.
pub fn decode(buf: &[u8]) -> Result<(Vec<u8>, usize), Packed5Error> {
    let mut out = Vec::new();
    let read = append_decoded(&mut out, buf)?;
    Ok((out, read))
}

/// Decodes one frame from the front of `buf`, appending the decoded bytes onto
/// `dst`, and returns the frame length.
///
/// It exists so a caller decoding a run of frames can gather them into a single
/// backing array and slice the strings out of it, rather than pay one allocation
/// per frame.
pub fn append_decoded(dst: &mut Vec<u8>, buf: &[u8]) -> Result<usize, Packed5Error> {
    let header = frame(buf)?;
    if !header.packed {
        dst.extend_from_slice(&buf[header.start..header.start + header.length]);
        return Ok(header.consumed);
    }
    decode_stream(
        dst,
        &buf[header.start..header.start + header.length],
        header.upper,
        header.number,
    )?;
    Ok(header.consumed)
}

/// One parsed frame prefix: where the payload is, how long the whole frame is,
/// and the flags that say how to read it.
struct Header {
    start: usize,
    length: usize,
    consumed: usize,
    packed: bool,
    upper: bool,
    number: bool,
}

/// Splits off the header byte and the length prefix.
fn frame(buf: &[u8]) -> Result<Header, Packed5Error> {
    let header = *buf.first().ok_or(Packed5Error::Truncated)?;
    let mut pos = 1;
    let mut length = usize::from(header >> LEN_SHIFT);
    if length == LEN_ESCAPE {
        let (value, used) = read_uvarint(&buf[pos..])?;
        // The escape must not re-encode a length the header could have held; one
        // length has one encoding, so a frame has one byte representation.
        if value <= LEN_INLINE {
            return Err(Packed5Error::BadLength);
        }
        length = value;
        pos += used;
    }
    if length > buf.len() - pos {
        return Err(Packed5Error::Truncated);
    }
    Ok(Header {
        start: pos,
        length,
        consumed: pos + length,
        packed: header & FLAG_PACKED5 != 0,
        upper: header & FLAG_UPPERCASE != 0,
        number: header & FLAG_NUMBER != 0,
    })
}

/// Reads an LEB128 length, rejecting overlong encodings and values that cannot
/// be a length on this platform.
fn read_uvarint(buf: &[u8]) -> Result<(usize, usize), Packed5Error> {
    let mut value = 0_u64;
    let mut shift = 0_u32;
    for (index, byte) in buf.iter().enumerate() {
        if index >= 9 {
            return Err(Packed5Error::BadLength);
        }
        value |= u64::from(byte & 0x7F) << shift;
        if *byte < 0x80 {
            let size = usize::try_from(value).map_err(|_| Packed5Error::BadLength)?;
            if size > i32::MAX as usize {
                return Err(Packed5Error::BadLength);
            }
            return Ok((size, index + 1));
        }
        shift += 7;
    }
    Err(Packed5Error::Truncated)
}

/// Walks the packed bitstream, appending the decoded bytes to `dst`. Its whole
/// state is the payload position, the current case mode, and whether a simple
/// toggle is pending.
///
/// A pending simple toggle is cleared only by a letter, and a long toggle leaves
/// it alone. The encoder emits `CASE_TOGGLE_SIMPLE` only immediately before the
/// letter it applies to, so neither case arises in a frame this codec wrote;
/// they are defined so that a corrupt stream still decodes deterministically
/// rather than by accident.
#[allow(clippy::cast_possible_truncation)]
fn decode_stream(
    dst: &mut Vec<u8>,
    payload: &[u8],
    upper: bool,
    number: bool,
) -> Result<(), Packed5Error> {
    let mut reader = BitReader::new(payload);
    let pad = reader.read(PAD_BITS_WIDTH).ok_or(Packed5Error::Truncated)?;
    if pad as usize > reader.remaining() {
        return Err(Packed5Error::BadPadding);
    }
    reader.limit -= pad as usize;

    let restore = dst.len();
    let mut current = upper;
    let mut pending = false;

    while reader.remaining() >= 5 {
        let op = reader.read(5).ok_or(Packed5Error::Truncated)?;
        match op {
            op if op < OP_SPACE => {
                // Uppercase exactly when the case mode and the pending toggle
                // disagree.
                let base = if current != pending { b'A' } else { b'a' };
                dst.push(base + op as u8);
                pending = false;
            }
            OP_SPACE => dst.push(b' '),
            OP_CASE_SIMPLE => pending = true,
            OP_CASE_LONG => current = !current,
            OP_SYMBOL => {
                let index = reader.read(5).ok_or(Packed5Error::Truncated)?;
                if index >= SYM_RESERVED {
                    dst.truncate(restore);
                    return Err(Packed5Error::ReservedSymbol);
                }
                dst.extend_from_slice(SYM_TABLE[index as usize].as_bytes());
            }
            OP_SIMPLE => {
                let index = reader.read(4).ok_or(Packed5Error::Truncated)?;
                if index != ESCAPE_CODE {
                    dst.push(SIMPLE_TABLE[index as usize]);
                    continue;
                }
                let count = reader.read(2).ok_or(Packed5Error::Truncated)?;
                for _ in 0..=count {
                    let byte = reader.read(8).ok_or(Packed5Error::Truncated)?;
                    dst.push(byte as u8);
                }
            }
            // OP_NUMBER: a 10-bit integer, or '-' when the flag is clear.
            _ => {
                if !number {
                    dst.push(b'-');
                    continue;
                }
                let value = reader.read(10).ok_or(Packed5Error::Truncated)?;
                push_decimal(dst, value);
            }
        }
    }
    // Fewer than five bits left is the stream's end. Anything else means the pad
    // count did not account for the tokens exactly, which the encoder never does.
    if reader.remaining() != 0 {
        dst.truncate(restore);
        return Err(Packed5Error::Truncated);
    }
    Ok(())
}

/// Appends `value` as plain decimal digits, which is what `NUMBER_0_1023`
/// decodes to.
#[allow(clippy::cast_possible_truncation)]
fn push_decimal(dst: &mut Vec<u8>, value: u32) {
    if value >= 1000 {
        dst.push(b'0' + (value / 1000) as u8);
    }
    if value >= 100 {
        dst.push(b'0' + (value / 100 % 10) as u8);
    }
    if value >= 10 {
        dst.push(b'0' + (value / 10 % 10) as u8);
    }
    dst.push(b'0' + (value % 10) as u8);
}

/// Appends bits LSB-first onto a buffer the caller owns, so the frame header can
/// be appended first and the stream written in place.
///
/// The accumulator is 64 bits wide and drains four bytes at a time. Tokens are 5
/// to 15 bits, so a byte-at-a-time drain would run its loop on nearly every
/// write; this way roughly one write in four touches the buffer at all.
struct BitWriter<'a> {
    buf: &'a mut Vec<u8>,
    /// Pending bits not yet drained; always fewer than 32 between calls.
    current: u64,
    nbits: u8,
}

impl<'a> BitWriter<'a> {
    fn new(buf: &'a mut Vec<u8>) -> Self {
        Self {
            buf,
            current: 0,
            nbits: 0,
        }
    }

    /// Appends the low `width` bits of `value` (`width <= 24`).
    #[allow(clippy::cast_possible_truncation)]
    fn write_bits(&mut self, value: u32, width: u8) {
        let mask = (1_u32 << width) - 1;
        self.current |= u64::from(value & mask) << self.nbits;
        self.nbits += width;
        if self.nbits >= 32 {
            self.buf
                .extend_from_slice(&(self.current as u32).to_le_bytes());
            self.current >>= 32;
            self.nbits -= 32;
        }
    }

    /// Emits the pending bits, zero-padded in the high bits of the last byte.
    #[allow(clippy::cast_possible_truncation)]
    fn flush(mut self) {
        while self.nbits > 0 {
            self.buf.push(self.current as u8);
            self.current >>= 8;
            if self.nbits <= 8 {
                break;
            }
            self.nbits -= 8;
        }
    }
}

/// Reads bits LSB-first. `limit` is the number of bits that are payload rather
/// than trailing pad, so reads past the end of the token stream fail instead of
/// returning zeros.
struct BitReader<'a> {
    buf: &'a [u8],
    /// Unread bits, with the next field in the low bits.
    acc: u64,
    /// Payload bits already consumed.
    bit: usize,
    /// Total readable bits.
    limit: usize,
    /// The next byte not yet loaded into `acc`.
    pos: usize,
    /// Valid low bits in `acc`.
    nbits: u8,
}

impl<'a> BitReader<'a> {
    fn new(buf: &'a [u8]) -> Self {
        Self {
            buf,
            acc: 0,
            bit: 0,
            limit: buf.len() * 8,
            pos: 0,
            nbits: 0,
        }
    }

    fn remaining(&self) -> usize {
        self.limit - self.bit
    }

    /// Returns the next `width` bits (`width <= 24`), or `None` if fewer than
    /// `width` payload bits remain.
    #[allow(clippy::cast_possible_truncation)]
    fn read(&mut self, width: u8) -> Option<u32> {
        if self.bit + usize::from(width) > self.limit {
            return None;
        }
        // After every read fewer than eight bits remain, so this loop loads only
        // the new bytes the next field needs.
        while self.nbits < width {
            self.acc |= u64::from(*self.buf.get(self.pos)?) << self.nbits;
            self.pos += 1;
            self.nbits += 8;
        }
        let value = (self.acc & ((1_u64 << width) - 1)) as u32;
        self.acc >>= width;
        self.nbits -= width;
        self.bit += usize::from(width);
        Some(value)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn round_trip(input: &[u8]) -> usize {
        let mut buf = Vec::new();
        append(&mut buf, input);
        assert_eq!(buf.len(), size(input), "size disagreed with append");
        let (out, read) = decode(&buf).expect("decode");
        assert_eq!(out, input, "round trip");
        assert_eq!(read, buf.len(), "frame length");
        buf.len()
    }

    /// The frames Go's encoder produces for the specification's own worked
    /// examples, byte for byte. Two ports of a greedy scan agreeing on a length
    /// is not the same as agreeing on a frame, so the bytes are what is pinned.
    #[test]
    fn matches_the_go_encoder_byte_for_byte() {
        for (input, expected) in [
            ("hello", "213c642d07"),
            ("helloWorld", "413e642db7ad8b6b00"),
            ("product123", "3d7bd10d2ae6df03"),
            ("the quick brown fox", "699e87684891503a44679b2eee02"),
            ("THE QUICK BROWN FOX", "6b9e87684891503a44679b2eee02"),
            ("el niño comió jamón", "79274b37d4b1d3c231d4b94e8075de00"),
            (
                "Móvil Samsung Galaxy S23",
                "a5dfacf38a96de1230499bd1db80052ed65bfe0b00",
            ),
            ("Factura 2024-1023", "65d805884923d05f194f7effff"),
            (
                "user.name@example.com",
                "81a79244af0d30d221b980bd45bc0ac700",
            ),
            (
                "{\"id\":1023,\"name\":\"ana\"}",
                "c07b226964223a313032332c226e616d65223a22616e61227d",
            ),
        ] {
            let mut buf = Vec::new();
            append(&mut buf, input.as_bytes());
            let hex: String = buf.iter().map(|byte| format!("{byte:02x}")).collect();
            assert_eq!(hex, expected, "{input}");
            round_trip(input.as_bytes());
        }
    }

    #[test]
    fn round_trips_arbitrary_bytes() {
        round_trip(b"");
        for byte in 0..=255_u8 {
            round_trip(&[byte]);
        }
        // Every two-byte input, which is where the escape merging and the case
        // toggles interact most densely.
        for first in 0..=255_u8 {
            for second in [0_u8, 9, b'A', b'a', b'0', b'-', 0x80, 0xC3, 0xFF] {
                round_trip(&[first, second]);
            }
        }
        round_trip(&[0xC3, 0xB1, 0xE2, 0x82, 0xAC, 0xFF, 0x00]);
        round_trip(&vec![b'a'; 600]);
        round_trip("00123".as_bytes());
        round_trip("0".as_bytes());
    }

    /// Arbitrary and truncated input must be refused rather than panicked on.
    #[test]
    fn garbage_never_panics() {
        let mut buf = Vec::new();
        append(&mut buf, "el niño comió jamón 2024".as_bytes());
        for cut in 0..buf.len() {
            let _ = decode(&buf[..cut]);
        }
        for first in 0..=255_u8 {
            for second in 0..=255_u8 {
                let _ = decode(&[first, second, 0x41, 0x00, 0xFF]);
            }
        }
    }
}
