//! The JSON text sink: what a walk puts its findings into.
//!
//! Mirrors `codec/jsontext.go`. The output is
//! what `encoding/json` would have written for the same record, down to the
//! escaping and the spelling of numbers — a stronger claim than "valid JSON",
//! and the one the vectors check, because a test that compared parsed values
//! would not notice a float printed one digit differently.
//!
//! Strings arrive as UTF-8 byte slices onto the message and the escaper works a
//! byte at a time, so nothing here goes through a `str` on the way out. That is
//! not only speed: a round trip through UTF-16 would turn an invalid byte into a
//! replacement character before the escaper could see it, and matching
//! `encoding/json` means deciding that case here.

use alloc::vec::Vec;

/// The other direction: JSON text in, a value tree out. Only the encoder needs
/// it, so only the encoder links it.
#[cfg(feature = "encode")]
pub mod parse;

const HEX_DIGITS: &[u8; 16] = b"0123456789abcdef";

/// The two runes that are a line break in JavaScript and not in JSON. They are
/// escaped for the same reason the HTML-significant bytes are: so the output can
/// be pasted into a script tag and still be the document it was.
const LINE_SEPARATOR: u32 = 0x2028;
const PARAGRAPH_SEPARATOR: u32 = 0x2029;

/// Accumulates JSON text.
#[derive(Debug, Default)]
pub struct JsonSink {
    out: Vec<u8>,
    /// Whether a value has been written at the current depth, so a comma goes in
    /// front of the next one.
    first: Vec<bool>,
    /// Set when a key has just been written, so the value it belongs to does not
    /// also write a comma.
    after_key: bool,
}

impl JsonSink {
    #[must_use]
    pub fn new() -> Self {
        Self::default()
    }

    /// Sized against the message it is decoding, so a document does not grow
    /// through a dozen doublings on the way out. JSON text runs several times
    /// the binary it came from; four is a floor that costs one allocation and
    /// saves the rest.
    #[must_use]
    pub fn with_message_len(len: usize) -> Self {
        Self {
            out: Vec::with_capacity(len.saturating_mul(4).max(256)),
            ..Self::default()
        }
    }

    #[must_use]
    pub fn finish(self) -> Vec<u8> {
        self.out
    }

    #[must_use]
    pub fn as_bytes(&self) -> &[u8] {
        &self.out
    }

    fn before_value(&mut self) {
        if self.after_key {
            self.after_key = false;
            return;
        }
        match self.first.last_mut() {
            None => {}
            Some(first) if *first => *first = false,
            Some(_) => self.out.push(b','),
        }
    }

    pub fn begin_object(&mut self) {
        self.before_value();
        self.out.push(b'{');
        self.first.push(true);
    }

    pub fn end_object(&mut self) {
        self.first.pop();
        self.out.push(b'}');
    }

    pub fn begin_array(&mut self) {
        self.before_value();
        self.out.push(b'[');
        self.first.push(true);
    }

    pub fn end_array(&mut self) {
        self.first.pop();
        self.out.push(b']');
    }

    /// An object key held as UTF-8 bytes.
    pub fn key_bytes(&mut self, name: &[u8]) {
        self.before_value();
        write_json_string(&mut self.out, name);
        self.out.push(b':');
        self.after_key = true;
    }

    /// A key whose quotes, escaping and colon were rendered once, ahead of the
    /// walk — see [`render_key_run`]. A field then costs one `extend_from_slice`
    /// per row rather than an escape pass.
    pub fn key_run(&mut self, run: &[u8]) {
        self.before_value();
        self.out.extend_from_slice(run);
        self.after_key = true;
    }

    pub fn null(&mut self) {
        self.before_value();
        self.out.extend_from_slice(b"null");
    }

    pub fn boolean(&mut self, value: bool) {
        self.before_value();
        self.out
            .extend_from_slice(if value { b"true" } else { b"false" });
    }

    pub fn signed(&mut self, value: i64) {
        self.before_value();
        write_i64(&mut self.out, value);
    }

    pub fn unsigned(&mut self, value: u64) {
        self.before_value();
        write_u64(&mut self.out, value);
    }

    /// A string held as UTF-8 bytes, which is how a walk holds every one.
    pub fn text_bytes(&mut self, value: &[u8]) {
        self.before_value();
        write_json_string(&mut self.out, value);
    }

    /// A `[u8]` as base64 in a string, which is what `encoding/json` does and
    /// therefore what a caller on the other end already has a decoder for.
    /// Worth saying out loud because colbin's `opBytes` and `opStrings` are
    /// different ops and only this one is base64.
    pub fn blob(&mut self, value: &[u8]) {
        const ALPHABET: &[u8; 64] =
            b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
        self.before_value();
        self.out.push(b'"');
        let (groups, left) = value.as_chunks::<3>();
        for group in groups {
            let bits =
                (u32::from(group[0]) << 16) | (u32::from(group[1]) << 8) | u32::from(group[2]);
            self.out.push(ALPHABET[(bits >> 18) as usize & 0x3f]);
            self.out.push(ALPHABET[(bits >> 12) as usize & 0x3f]);
            self.out.push(ALPHABET[(bits >> 6) as usize & 0x3f]);
            self.out.push(ALPHABET[bits as usize & 0x3f]);
        }
        if !left.is_empty() {
            let mut bits = u32::from(left[0]) << 16;
            if left.len() == 2 {
                bits |= u32::from(left[1]) << 8;
            }
            self.out.push(ALPHABET[(bits >> 18) as usize & 0x3f]);
            self.out.push(ALPHABET[(bits >> 12) as usize & 0x3f]);
            if left.len() == 2 {
                self.out.push(ALPHABET[(bits >> 6) as usize & 0x3f]);
            } else {
                self.out.push(b'=');
            }
            self.out.push(b'=');
        }
        self.out.push(b'"');
    }

    /// A float the way `encoding/json` writes one.
    ///
    /// JSON has no spelling for a NaN or an infinity. Writing `null` instead
    /// turns "not a number" into "no value", and the two are not the same thing,
    /// so this refuses and the caller abandons the decode.
    pub fn float(&mut self, value: f64, width: u32) -> bool {
        if value.is_nan() || value.is_infinite() {
            return false;
        }
        self.before_value();
        write_json_float(&mut self.out, value, width);
        true
    }
}

/// One quoted, escaped JSON string, for a caller assembling a small document by
/// hand — a diagnostic envelope, say — rather than through a [`JsonSink`].
pub fn write_json_str(out: &mut Vec<u8>, text: &str) {
    write_json_string(out, text.as_bytes());
}

/// `"name":` — quoted, escaped and punctuated — for [`JsonSink::key_run`].
///
/// Rendered through the same escaper the walker would have used, so a
/// precomputed key cannot drift from the one it replaces.
#[must_use]
pub fn render_key_run(name: &[u8]) -> Vec<u8> {
    let mut out = Vec::with_capacity(name.len() + 3);
    write_json_string(&mut out, name);
    out.push(b':');
    out
}

/// Whether a byte may go into a string as it stands.
#[inline]
const fn json_safe(character: u8) -> bool {
    character >= 0x20
        && character != b'"'
        && character != b'\\'
        && character != b'<'
        && character != b'>'
        && character != b'&'
}

/// A quoted, escaped JSON string, with `encoding/json`'s default escape set.
fn write_json_string(out: &mut Vec<u8>, value: &[u8]) {
    out.push(b'"');
    let mut start = 0;
    let mut at = 0;
    while at < value.len() {
        let character = value[at];
        if character < 0x80 {
            if json_safe(character) {
                at += 1;
                continue;
            }
            out.extend_from_slice(&value[start..at]);
            match character {
                b'\\' | b'"' => {
                    out.push(b'\\');
                    out.push(character);
                }
                b'\n' => out.extend_from_slice(b"\\n"),
                b'\r' => out.extend_from_slice(b"\\r"),
                b'\t' => out.extend_from_slice(b"\\t"),
                // A control byte, or one of the three HTML-significant ones,
                // which go the long way round for the same reason: so the output
                // is safe wherever it is pasted.
                _ => {
                    out.extend_from_slice(b"\\u00");
                    out.push(HEX_DIGITS[(character >> 4) as usize]);
                    out.push(HEX_DIGITS[(character & 0xf) as usize]);
                }
            }
            at += 1;
            start = at;
            continue;
        }
        let width = rune_width(value, at);
        if width == 1 {
            // Invalid UTF-8 is replaced rather than refused, which is what
            // `encoding/json` does and what keeps one bad byte from losing a
            // whole message.
            out.extend_from_slice(&value[start..at]);
            out.extend_from_slice("\u{fffd}".as_bytes());
        } else {
            let rune = decode_rune(value, at, width);
            if rune == LINE_SEPARATOR || rune == PARAGRAPH_SEPARATOR {
                out.extend_from_slice(&value[start..at]);
                out.extend_from_slice(b"\\u202");
                out.push(HEX_DIGITS[(rune & 0xf) as usize]);
            } else {
                at += width;
                continue;
            }
        }
        at += width;
        start = at;
    }
    out.extend_from_slice(&value[start..]);
    out.push(b'"');
}

/// How many bytes the rune at `at` occupies, or 1 when the sequence is not valid
/// UTF-8 — which is how Go's `utf8.DecodeRune` reports one, and the distinction
/// the escaper turns into U+FFFD.
fn rune_width(buf: &[u8], at: usize) -> usize {
    let first = buf[at];
    let (width, lowest) = match first {
        0xc2..=0xdf => (2usize, 0x80u32),
        0xe0..=0xef => (3, 0x800),
        0xf0..=0xf4 => (4, 0x10000),
        _ => return 1,
    };
    if at + width > buf.len() {
        return 1;
    }
    for &b in &buf[at + 1..at + width] {
        if !(0x80..=0xbf).contains(&b) {
            return 1;
        }
    }
    let rune = decode_rune(buf, at, width);
    // Overlong forms, surrogates and anything past U+10FFFF are not valid UTF-8,
    // and Go reports each as a one-byte error rather than decoding it.
    if rune < lowest || rune > 0x10_ffff || (0xd800..=0xdfff).contains(&rune) {
        return 1;
    }
    width
}

#[inline]
fn decode_rune(buf: &[u8], at: usize, width: usize) -> u32 {
    match width {
        2 => ((u32::from(buf[at]) & 0x1f) << 6) | (u32::from(buf[at + 1]) & 0x3f),
        3 => {
            ((u32::from(buf[at]) & 0x0f) << 12)
                | ((u32::from(buf[at + 1]) & 0x3f) << 6)
                | (u32::from(buf[at + 2]) & 0x3f)
        }
        _ => {
            ((u32::from(buf[at]) & 0x07) << 18)
                | ((u32::from(buf[at + 1]) & 0x3f) << 12)
                | ((u32::from(buf[at + 2]) & 0x3f) << 6)
                | (u32::from(buf[at + 3]) & 0x3f)
        }
    }
}

/// `value` in decimal, straight into the buffer.
///
/// Digits come out least significant first, so they go into a fixed scratch from
/// the back and reach the buffer in one copy. Twenty bytes is the widest decimal
/// a `u64` has.
pub fn write_u64(out: &mut Vec<u8>, value: u64) {
    let mut scratch = [0u8; 20];
    let mut at = scratch.len();
    // Most values in a real record fit in 32 bits, and 32-bit division is
    // materially cheaper than 64-bit in WebAssembly, so the wide loop runs only
    // until the value is small enough for the narrow one.
    let mut wide = value;
    while wide > u64::from(u32::MAX) {
        let q = wide / 10;
        at -= 1;
        scratch[at] = b'0' + (wide - q * 10) as u8;
        wide = q;
    }
    let mut narrow = wide as u32;
    loop {
        let q = narrow / 10;
        at -= 1;
        scratch[at] = b'0' + (narrow - q * 10) as u8;
        narrow = q;
        if narrow == 0 {
            break;
        }
    }
    out.extend_from_slice(&scratch[at..]);
}

/// `value` in decimal, with a leading `-` when it is negative.
pub fn write_i64(out: &mut Vec<u8>, value: i64) {
    if value < 0 {
        out.push(b'-');
        // `-i64::MIN` overflows; the unsigned magnitude is what is wanted and
        // `unsigned_abs` is exactly that, so the one value that would need its
        // own branch does not get one.
        write_u64(out, value.unsigned_abs());
        return;
    }
    write_u64(out, value as u64);
}

/// A float the way `encoding/json` writes one: `f` notation in the range a
/// reader expects to see it, `e` outside it, and in either case the shortest
/// form that reads back as the same value.
///
/// Rust's `{}` for floats is already shortest-round-trip, and agrees with Go
/// inside the `f` range. Two differences are real and handled here:
///
///   - **the exponent range.** Go switches to `e` below 1e-6 or at or above
///     1e21; Rust never does, so `1e21` would print as twenty-two digits.
///   - **the exponent's spelling.** Go writes `1e+21`, Rust writes `1e21`.
fn write_json_float(out: &mut Vec<u8>, value: f64, width: u32) {
    use core::fmt::Write as _;

    // A float32 must be formatted at 32 bits, or `1.1` prints as
    // 1.100000023841858. Widening back to f64 afterwards is exact.
    let value = if width == 32 {
        f64::from(value as f32)
    } else {
        value
    };

    // The sign of a zero survives the comparison, so `-0` is not `0`. Go writes
    // both, and losing the sign would lose the difference.
    if value == 0.0 {
        out.extend_from_slice(if value.is_sign_negative() {
            b"-0"
        } else {
            b"0"
        });
        return;
    }

    let magnitude = value.abs();
    let mut text = alloc::string::String::new();
    // The thresholds `encoding/json` switches notation at. Spelled as a range so
    // the two bounds cannot drift apart.
    if !(1e-6..1e21).contains(&magnitude) {
        let _ = write!(text, "{value:e}");
        // Rust spells the exponent `1e21` and `1e-7`; Go spells them `1e+21`
        // and `1e-07`. The mantissa is identical, so only the exponent moves.
        let at = text.find('e').expect("{:e} always writes an exponent");
        let (mantissa, exponent) = text.split_at(at);
        let digits = &exponent[1..];
        let (sign, digits) = digits
            .strip_prefix('-')
            .map_or(("+", digits), |rest| ("-", rest));
        out.extend_from_slice(mantissa.as_bytes());
        out.push(b'e');
        out.extend_from_slice(sign.as_bytes());
        if digits.len() == 1 {
            out.push(b'0');
        }
        out.extend_from_slice(digits.as_bytes());
        return;
    }
    let _ = write!(text, "{value}");
    out.extend_from_slice(text.as_bytes());
}
