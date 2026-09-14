//! A UTF-8 JSON scanner producing a flat value tree.
//!
//! It exists rather than a host's `JSON.parse` for one reason
//! (`rust/ENCODER.md` §5): `JSON.parse` turns 7295013456321098765 into
//! 7295013456321098800. colbin has an exact int64 column, so the parse has to be
//! exact too, and that means reading the digits here.
//!
//! # The 293 lines that did not come across
//!
//! The AssemblyScript module this replaced carried `decimal.ts`, an
//! arbitrary-precision rational conversion, because AssemblyScript's
//! `parseFloat` is not correctly rounded and "numbers are exact" is the central
//! claim. Rust's `str::parse::<f64>` *is* correctly rounded, so the whole of
//! that file is [`f64::from_str`] here. The float half of
//! `rust/vectors/vectors.json` — 513 literals, Go's `strconv.ParseFloat` for
//! every expected bit pattern — is what says so rather than the documentation
//! does.
//!
//! # The tree is arrays
//!
//! One node is a kind, a 64-bit payload, two `u32` fields whose meaning depends
//! on the kind, and a source offset kept only so a later diagnostic can point at
//! the value. Parallel `Vec`s rather than an enum of owned children: the
//! inference pass walks the same document several times and a flat arena is what
//! keeps that from being a pointer chase, which is the same reason the walk on
//! the decode side holds a plan table rather than a plan graph.

use alloc::vec::Vec;

use crate::diag::{D_LIMIT, D_NUMBER, D_SYNTAX, Diag};

pub const K_NULL: u8 = 0;
pub const K_BOOL: u8 = 1;
pub const K_INT: u8 = 2;
pub const K_UINT: u8 = 3;
pub const K_FLOAT: u8 = 4;
pub const K_STRING: u8 = 5;
pub const K_ARRAY: u8 = 6;
pub const K_OBJECT: u8 = 7;

/// `ENCODER.md` §4 states both, and both are enforced here.
pub const MAX_DEPTH: u32 = 64;
pub const MAX_INPUT: usize = 64 * 1024 * 1024;

/// Keys scanned when resolving a duplicate. Every object becomes a struct, and a
/// struct may hold at most 254 fields, so an object wider than this is refused
/// before its duplicates could matter.
const DEDUP_SCAN_LIMIT: usize = 512;

/// A parsed document: nodes in one arena, strings in another.
#[derive(Debug, Default)]
pub struct Doc {
    kind: Vec<u8>,
    /// Int value, uint bits, bool as 0/1, or f64 bits — read with the kind.
    num: Vec<u64>,
    /// String start in `text`, or the first child index.
    a: Vec<u32>,
    /// String byte length, or the child count.
    b: Vec<u32>,
    /// Source offset of the value, for diagnostics.
    off: Vec<u32>,

    /// Child node indices of arrays and objects, contiguous per parent.
    child: Vec<u32>,
    /// Key start and length in `text`, parallel to `child`; objects only.
    key_a: Vec<u32>,
    key_b: Vec<u32>,

    /// Decoded string bytes: keys and values, escapes already resolved.
    text: Vec<u8>,

    root: u32,
}

impl Doc {
    /// The document's top-level value.
    #[must_use]
    #[inline]
    pub fn root(&self) -> u32 {
        self.root
    }

    #[must_use]
    #[inline]
    pub fn kind_of(&self, node: u32) -> u8 {
        self.kind[node as usize]
    }

    /// Children of an array or object.
    #[must_use]
    #[inline]
    pub fn count(&self, node: u32) -> usize {
        self.b[node as usize] as usize
    }

    #[must_use]
    #[inline]
    pub fn child_at(&self, node: u32, at: usize) -> u32 {
        self.child[self.a[node as usize] as usize + at]
    }

    /// Bytes of a string node, as a view into the text arena.
    #[must_use]
    #[inline]
    pub fn str_of(&self, node: u32) -> &[u8] {
        let from = self.a[node as usize] as usize;
        &self.text[from..from + self.b[node as usize] as usize]
    }

    /// Bytes of the `at`-th key of an object node.
    #[must_use]
    #[inline]
    pub fn key_of(&self, node: u32, at: usize) -> &[u8] {
        let slot = self.a[node as usize] as usize + at;
        let from = self.key_a[slot] as usize;
        &self.text[from..from + self.key_b[slot] as usize]
    }

    /// Where in the source this value began, for a diagnostic.
    #[must_use]
    #[inline]
    pub fn offset_of(&self, node: u32) -> i32 {
        self.off[node as usize] as i32
    }

    #[must_use]
    #[inline]
    pub fn bool_of(&self, node: u32) -> bool {
        self.num[node as usize] != 0
    }

    #[must_use]
    #[inline]
    pub fn int_of(&self, node: u32) -> i64 {
        self.num[node as usize] as i64
    }

    #[must_use]
    #[inline]
    pub fn uint_of(&self, node: u32) -> u64 {
        self.num[node as usize]
    }

    #[must_use]
    #[inline]
    pub fn float_of(&self, node: u32) -> f64 {
        f64::from_bits(self.num[node as usize])
    }

    /// How many nodes the document holds, which bounds every walk over it.
    #[must_use]
    #[inline]
    pub fn len(&self) -> usize {
        self.kind.len()
    }

    #[must_use]
    #[inline]
    pub fn is_empty(&self) -> bool {
        self.kind.is_empty()
    }
}

struct Parser<'a, 'd> {
    src: &'a [u8],
    pos: usize,
    depth: u32,
    doc: Doc,
    diag: &'d mut Diag,
}

impl<'a, 'd> Parser<'a, 'd> {
    fn new(src: &'a [u8], diag: &'d mut Diag) -> Self {
        Self {
            src,
            pos: 0,
            depth: 0,
            doc: Doc::default(),
            diag,
        }
    }

    #[inline]
    fn at_end(&self) -> bool {
        self.pos >= self.src.len()
    }

    #[inline]
    fn peek(&self) -> u8 {
        self.src.get(self.pos).copied().unwrap_or(0)
    }

    fn fail(&mut self, code: i32, offset: usize, message: &str) {
        // The offset is a byte index into an input the parser already refused to
        // hold past 64 MiB, so it fits an i32 by construction.
        self.diag.fail(code, offset as i32, "", message);
    }

    fn skip_space(&mut self) {
        while let Some(&byte) = self.src.get(self.pos) {
            if byte == b' ' || byte == b'\t' || byte == b'\n' || byte == b'\r' {
                self.pos += 1;
            } else {
                break;
            }
        }
    }

    /// Appends a node and returns its index.
    #[inline]
    fn node(&mut self, kind: u8, num: u64, a: u32, b: u32, off: usize) -> u32 {
        let at = self.doc.kind.len() as u32;
        self.doc.kind.push(kind);
        self.doc.num.push(num);
        self.doc.a.push(a);
        self.doc.b.push(b);
        self.doc.off.push(off as u32);
        at
    }

    fn parse(&mut self) -> bool {
        if self.src.len() > MAX_INPUT {
            self.fail(D_LIMIT, 0, "input is larger than the 64 MiB limit");
            return false;
        }
        self.skip_space();
        let Some(root) = self.value() else {
            return false;
        };
        self.skip_space();
        if !self.at_end() {
            self.fail(
                D_SYNTAX,
                self.pos,
                "trailing content after the top-level value",
            );
            return false;
        }
        self.doc.root = root;
        true
    }

    fn value(&mut self) -> Option<u32> {
        if self.at_end() {
            self.fail(D_SYNTAX, self.pos, "unexpected end of input");
            return None;
        }
        let start = self.pos;
        match self.peek() {
            b'{' => self.object(),
            b'[' => self.array(),
            b'"' => {
                let at = self.doc.text.len() as u32;
                let len = self.string_bytes()?;
                Some(self.node(K_STRING, 0, at, len, start))
            }
            b't' => self.literal(b"true", K_BOOL, 1),
            b'f' => self.literal(b"false", K_BOOL, 0),
            b'n' => self.literal(b"null", K_NULL, 0),
            b'-' | b'0'..=b'9' => self.number(),
            _ => {
                self.fail(D_SYNTAX, self.pos, "expected a JSON value");
                None
            }
        }
    }

    fn literal(&mut self, word: &[u8], kind: u8, num: u64) -> Option<u32> {
        let start = self.pos;
        for &want in word {
            if self.src.get(self.pos) != Some(&want) {
                let mut message = alloc::string::String::from("expected ");
                // `word` is one of three ASCII literals written above.
                message.push_str(core::str::from_utf8(word).unwrap_or("a literal"));
                self.diag.fail(D_SYNTAX, start as i32, "", message);
                return None;
            }
            self.pos += 1;
        }
        Some(self.node(kind, num, 0, 0, start))
    }

    fn object(&mut self) -> Option<u32> {
        let start = self.pos;
        self.depth += 1;
        if self.depth > MAX_DEPTH {
            self.fail(D_LIMIT, start, "nesting deeper than 64 levels");
            return None;
        }
        self.pos += 1; // {

        // Children are appended to shared arrays, so a nested value would
        // interleave with ours. They are gathered locally and copied in on the
        // way out.
        let mut keys_a: Vec<u32> = Vec::new();
        let mut keys_b: Vec<u32> = Vec::new();
        let mut kids: Vec<u32> = Vec::new();

        self.skip_space();
        if self.peek() == b'}' {
            self.pos += 1;
            self.depth -= 1;
            let at = self.doc.child.len() as u32;
            return Some(self.node(K_OBJECT, 0, at, 0, start));
        }

        loop {
            self.skip_space();
            if self.peek() != b'"' {
                self.fail(D_SYNTAX, self.pos, "expected a string key");
                return None;
            }
            let key_at = self.doc.text.len() as u32;
            let key_len = self.string_bytes()?;

            self.skip_space();
            if self.peek() != b':' {
                self.fail(D_SYNTAX, self.pos, "expected : after the key");
                return None;
            }
            self.pos += 1;
            self.skip_space();

            let value = self.value()?;

            // A duplicate key keeps its last value, as `JSON.parse` does.
            // Resolving it here rather than in every consumer is what keeps the
            // rule in one place: inference would otherwise see both values and
            // call a changed type a conflict, and the encoder would write
            // whichever it happened to find first. The scan is capped because
            // any object this wide is refused by the 254-field limit before it
            // can be encoded, so its duplicate semantics never reach the wire.
            let mut replaced = false;
            if kids.len() <= DEDUP_SCAN_LIMIT {
                let new_key = &self.doc.text[key_at as usize..key_at as usize + key_len as usize];
                for at in 0..kids.len() {
                    if keys_b[at] != key_len {
                        continue;
                    }
                    let from = keys_a[at] as usize;
                    if &self.doc.text[from..from + key_len as usize] == new_key {
                        kids[at] = value;
                        replaced = true;
                        break;
                    }
                }
            }
            if !replaced {
                keys_a.push(key_at);
                keys_b.push(key_len);
                kids.push(value);
            }

            self.skip_space();
            match self.peek() {
                b',' => self.pos += 1,
                b'}' => {
                    self.pos += 1;
                    break;
                }
                _ => {
                    self.fail(D_SYNTAX, self.pos, "expected , or } in an object");
                    return None;
                }
            }
        }

        let at = self.doc.child.len() as u32;
        self.doc.child.extend_from_slice(&kids);
        self.doc.key_a.extend_from_slice(&keys_a);
        self.doc.key_b.extend_from_slice(&keys_b);
        self.depth -= 1;
        let count = kids.len() as u32;
        Some(self.node(K_OBJECT, 0, at, count, start))
    }

    fn array(&mut self) -> Option<u32> {
        let start = self.pos;
        self.depth += 1;
        if self.depth > MAX_DEPTH {
            self.fail(D_LIMIT, start, "nesting deeper than 64 levels");
            return None;
        }
        self.pos += 1; // [
        let mut kids: Vec<u32> = Vec::new();

        self.skip_space();
        if self.peek() == b']' {
            self.pos += 1;
            self.depth -= 1;
            let at = self.doc.child.len() as u32;
            return Some(self.node(K_ARRAY, 0, at, 0, start));
        }

        loop {
            self.skip_space();
            kids.push(self.value()?);

            self.skip_space();
            match self.peek() {
                b',' => self.pos += 1,
                b']' => {
                    self.pos += 1;
                    break;
                }
                _ => {
                    self.fail(D_SYNTAX, self.pos, "expected , or ] in an array");
                    return None;
                }
            }
        }

        let at = self.doc.child.len() as u32;
        self.doc.child.extend_from_slice(&kids);
        // Keys are parallel to `child`, so array slots need placeholders to keep
        // the two arrays in step for any object that follows.
        self.doc.key_a.resize(self.doc.child.len(), 0);
        self.doc.key_b.resize(self.doc.child.len(), 0);
        self.depth -= 1;
        let count = kids.len() as u32;
        Some(self.node(K_ARRAY, 0, at, count, start))
    }

    /// Reads a string into the text arena and returns its byte length.
    ///
    /// Lone surrogates become U+FFFD, which is what `encoding/json` does; the
    /// bytes handed to packed5 are then always valid UTF-8 (`ENCODER.md` §5).
    fn string_bytes(&mut self) -> Option<u32> {
        let start_len = self.doc.text.len();
        self.pos += 1; // opening quote

        loop {
            let Some(&byte) = self.src.get(self.pos) else {
                self.fail(D_SYNTAX, self.pos, "unterminated string");
                return None;
            };
            match byte {
                b'"' => {
                    self.pos += 1;
                    return Some((self.doc.text.len() - start_len) as u32);
                }
                b'\\' => {
                    self.pos += 1;
                    self.escape()?;
                }
                0x00..=0x1f => {
                    self.fail(
                        D_SYNTAX,
                        self.pos,
                        "control character in a string must be escaped",
                    );
                    return None;
                }
                _ => {
                    self.doc.text.push(byte);
                    self.pos += 1;
                }
            }
        }
    }

    fn escape(&mut self) -> Option<()> {
        let Some(&byte) = self.src.get(self.pos) else {
            self.fail(D_SYNTAX, self.pos, "unterminated escape");
            return None;
        };
        self.pos += 1;
        let simple = match byte {
            b'"' => Some(b'"'),
            b'\\' => Some(b'\\'),
            b'/' => Some(b'/'),
            b'b' => Some(0x08),
            b'f' => Some(0x0c),
            b'n' => Some(b'\n'),
            b'r' => Some(b'\r'),
            b't' => Some(b'\t'),
            _ => None,
        };
        if let Some(resolved) = simple {
            self.doc.text.push(resolved);
            return Some(());
        }
        if byte != b'u' {
            self.fail(D_SYNTAX, self.pos - 1, "unknown escape");
            return None;
        }

        let mut rune = self.hex4()?;
        if (0xd800..=0xdbff).contains(&rune) {
            // A high surrogate is only meaningful paired with a low one.
            if self.src.get(self.pos) == Some(&b'\\') && self.src.get(self.pos + 1) == Some(&b'u') {
                let save = self.pos;
                self.pos += 2;
                let low = self.hex4()?;
                if (0xdc00..=0xdfff).contains(&low) {
                    rune = 0x10000 + ((rune - 0xd800) << 10) + (low - 0xdc00);
                } else {
                    self.pos = save;
                    rune = 0xfffd;
                }
            } else {
                rune = 0xfffd;
            }
        } else if (0xdc00..=0xdfff).contains(&rune) {
            rune = 0xfffd; // a lone low surrogate
        }
        write_utf8(&mut self.doc.text, rune);
        Some(())
    }

    fn hex4(&mut self) -> Option<u32> {
        let mut value = 0u32;
        for _ in 0..4 {
            let Some(&byte) = self.src.get(self.pos) else {
                self.fail(D_SYNTAX, self.pos, "truncated \\u escape");
                return None;
            };
            let digit = match byte {
                b'0'..=b'9' => u32::from(byte - b'0'),
                b'a'..=b'f' => u32::from(byte - b'a') + 10,
                b'A'..=b'F' => u32::from(byte - b'A') + 10,
                _ => {
                    self.fail(D_SYNTAX, self.pos, "invalid hex digit in a \\u escape");
                    return None;
                }
            };
            value = value * 16 + digit;
            self.pos += 1;
        }
        Some(value)
    }

    /// Parses one number exactly.
    ///
    /// An integer literal must land in int64 or uint64 with every digit intact;
    /// a literal carrying `.` or an exponent is a real number and becomes f64,
    /// which must be finite. So `1e30` is a float and
    /// `1000000000000000000000000000000` is an error — the notation is what says
    /// which is meant, as it does in JSON itself.
    fn number(&mut self) -> Option<u32> {
        let start = self.pos;
        let negative = self.peek() == b'-';
        if negative {
            self.pos += 1;
        }

        let int_start = self.pos;
        if self.at_end() {
            self.fail(D_SYNTAX, start, "truncated number");
            return None;
        }
        match self.peek() {
            // A leading zero stands alone: `01` is not a JSON number.
            b'0' => self.pos += 1,
            b'1'..=b'9' => {
                while !self.at_end() && self.peek().is_ascii_digit() {
                    self.pos += 1;
                }
            }
            _ => {
                self.fail(D_SYNTAX, start, "expected a digit");
                return None;
            }
        }
        let int_end = self.pos;

        let mut is_float = false;
        if !self.at_end() && self.peek() == b'.' {
            is_float = true;
            self.pos += 1;
            if self.at_end() || !self.peek().is_ascii_digit() {
                self.fail(
                    D_SYNTAX,
                    self.pos,
                    "expected a digit after the decimal point",
                );
                return None;
            }
            while !self.at_end() && self.peek().is_ascii_digit() {
                self.pos += 1;
            }
        }

        if !self.at_end() && (self.peek() == b'e' || self.peek() == b'E') {
            is_float = true;
            self.pos += 1;
            if !self.at_end() && (self.peek() == b'+' || self.peek() == b'-') {
                self.pos += 1;
            }
            if self.at_end() || !self.peek().is_ascii_digit() {
                self.fail(D_SYNTAX, self.pos, "expected a digit in the exponent");
                return None;
            }
            while !self.at_end() && self.peek().is_ascii_digit() {
                self.pos += 1;
            }
        }

        if !is_float {
            return self.integer(start, int_start, int_end, negative);
        }

        // The literal is ASCII by construction — the scan above accepted only
        // digits, a sign, a point and an exponent marker — so this is the whole
        // of the decimal-to-binary conversion. `from_str` is correctly rounded,
        // which is the property `decimal.ts` spends 293 lines buying.
        let literal = core::str::from_utf8(&self.src[start..self.pos]).unwrap_or("");
        let value: f64 = literal.parse().unwrap_or(f64::NAN);
        if !value.is_finite() {
            self.fail(
                D_NUMBER,
                start,
                "number overflows float64; JSON has no infinity and colbin would store it as null",
            );
            return None;
        }
        Some(self.node(K_FLOAT, value.to_bits(), 0, 0, start))
    }

    fn integer(&mut self, start: usize, from: usize, to: usize, negative: bool) -> Option<u32> {
        let mut magnitude: u64 = 0;
        for at in from..to {
            let digit = u64::from(self.src[at] - b'0');
            // 1844674407370955161 = (2^64-1)/10, so anything above it overflows.
            if magnitude > 1_844_674_407_370_955_161
                || (magnitude == 1_844_674_407_370_955_161 && digit > 5)
            {
                self.fail(
                    D_NUMBER,
                    start,
                    "integer does not fit in int64 or uint64; \
                     write it as a float if that is what it is",
                );
                return None;
            }
            magnitude = magnitude * 10 + digit;
        }

        if negative {
            if magnitude > 1 << 63 {
                self.fail(D_NUMBER, start, "integer is below the int64 minimum");
                return None;
            }
            // `-(1<<63)` is i64::MIN, whose magnitude has no i64 of its own.
            let value = (magnitude as i64).wrapping_neg();
            return Some(self.node(K_INT, value as u64, 0, 0, start));
        }
        if magnitude <= i64::MAX as u64 {
            return Some(self.node(K_INT, magnitude, 0, 0, start));
        }
        Some(self.node(K_UINT, magnitude, 0, 0, start))
    }
}

/// UTF-8 encodes one code point.
fn write_utf8(out: &mut Vec<u8>, rune: u32) {
    if rune < 0x80 {
        out.push(rune as u8);
    } else if rune < 0x800 {
        out.push(0xc0 | (rune >> 6) as u8);
        out.push(0x80 | (rune & 0x3f) as u8);
    } else if rune < 0x10000 {
        out.push(0xe0 | (rune >> 12) as u8);
        out.push(0x80 | ((rune >> 6) & 0x3f) as u8);
        out.push(0x80 | (rune & 0x3f) as u8);
    } else {
        out.push(0xf0 | (rune >> 18) as u8);
        out.push(0x80 | ((rune >> 12) & 0x3f) as u8);
        out.push(0x80 | ((rune >> 6) & 0x3f) as u8);
        out.push(0x80 | (rune & 0x3f) as u8);
    }
}

/// Parses `src`, returning the document or `None` with `diag` set.
#[must_use]
pub fn parse(src: &[u8], diag: &mut Diag) -> Option<Doc> {
    let mut parser = Parser::new(src, diag);
    if !parser.parse() {
        return None;
    }
    Some(parser.doc)
}
