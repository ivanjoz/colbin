//! The compact form: dynamic values inside a dynamic container.
//!
//! Mirrors `wire/compact.go`. A dynamic value at a field says what it is in an
//! ordinary descriptor, because a reader that does not know the field has to
//! step over it. What is *inside* a dynamic container is read only by something
//! that reads every value, so the values there are written in a code table of
//! their own:
//!
//! ```text
//! 00–7F  an unsigned integer, the code itself
//! 80–8F  a map of 0..15 entries, each a key and a value
//! 90–9F  a list of 0..15 values
//! A0–BF  a string of 0..31 bytes, which follow
//! C0     null
//! C1 C2  false, true
//! C3–C5  a string, its length in 1, 2 or 4 bytes
//! C6–C8  bytes, their length in 1, 2 or 4 bytes
//! C9 CA  a map, a list, its count after (one byte up to 254, or FF and four)
//! CB     TYPED: a struct index, then a STRUCT, LIST or TABLE element as anywhere
//! CC     a float64 holding an integer, the compact integer after
//! D0–D7  an unsigned integer of 1..8 bytes
//! D8–DF  a negative integer, its magnitude in 1..8 bytes
//! E0–E8  a float64, its byte-reversed bits trimmed to 0..8 bytes
//! E9–ED  a float32, the same in 0..4 bytes
//! F0–FF  a float64 holding 0..15
//! ```
//!
//! `CD`–`CF` and `EE`–`EF` are unassigned and refused. A compact reader sits on
//! the code: `self.at` is the code's offset, as an element reader's is its
//! descriptor's. This is a reader only; the crate writes no dynamic values.

use super::{Kind, Reader8, le_uint, read_count};
use crate::Error;

const MAP: u8 = 0x80;
const LIST: u8 = 0x90;
const STRING: u8 = 0xA0;
const NULL: u8 = 0xC0;
const TRUE: u8 = 0xC2;
const STRING8: u8 = 0xC3;
const BYTES8: u8 = 0xC6;
const MAP_N: u8 = 0xC9;
const LIST_N: u8 = 0xCA;
const TYPED: u8 = 0xCB;
const WHOLE: u8 = 0xCC;
const UINT: u8 = 0xD0;
const NEGATIVE: u8 = 0xD8;
const FLOAT64: u8 = 0xE0;
const FLOAT32: u8 = 0xE9;
const SMALL: u8 = 0xF0;

const FIX_MAX: usize = 15;
const FIX_STRING: u8 = 31;

/// What a compact code is, for a reader with no schema to ask.
const fn compact_kind_of(code: u8) -> Kind {
    match code {
        0x00..=0x7F | 0xD0..=0xDF => Kind::Int,
        0x80..=0x8F | MAP_N => Kind::Map,
        0x90..=0x9F | LIST_N => Kind::List,
        0xA0..=0xBF | 0xC3..=0xC5 => Kind::String,
        NULL => Kind::Null,
        0xC1 | TRUE => Kind::Bool,
        0xC6..=0xC8 => Kind::Bytes,
        TYPED => Kind::Typed,
        WHOLE | 0xE0..=0xE8 | SMALL..=0xFF => Kind::Float64,
        0xE9..=0xED => Kind::Float32,
        _ => Kind::Invalid,
    }
}

impl<'a> Reader8<'a> {
    /// What the compact value at the cursor is, without advancing, and
    /// [`Kind::Invalid`] for a code this version does not assign or a message
    /// that has run out.
    #[must_use]
    pub fn compact_kind(&self) -> Kind {
        if self.err.is_some() {
            return Kind::Invalid;
        }
        self.buf
            .get(self.at)
            .map_or(Kind::Invalid, |&code| compact_kind_of(code))
    }

    /// More compact values to read, which is what a dynamic root's entries run
    /// on to.
    #[must_use]
    pub fn compact_more(&self) -> bool {
        self.err.is_none() && self.at < self.buf.len()
    }

    /// Steps over a null.
    pub fn compact_null(&mut self) {
        if self.compact_kind() != Kind::Null {
            self.fail(self.compact_error());
            return;
        }
        self.at += 1;
    }

    /// Reads false or true.
    pub fn compact_bool(&mut self) -> bool {
        if self.compact_kind() != Kind::Bool {
            self.fail(self.compact_error());
            return false;
        }
        self.at += 1;
        self.buf[self.at - 1] == TRUE
    }

    /// Reads an integer as its magnitude and its sign.
    pub fn compact_integer(&mut self) -> (u64, bool) {
        let Some(&code) = self.buf.get(self.at) else {
            self.fail(Error::Truncated);
            return (0, false);
        };
        if code < MAP {
            self.at += 1;
            return (u64::from(code), false);
        }
        if !(UINT..=NEGATIVE | 7).contains(&code) {
            self.fail(Error::BadDescriptor);
            return (0, false);
        }
        let magnitude = self.compact_magnitude(usize::from(code & 7) + 1);
        let negative = code >= NEGATIVE;
        if negative && magnitude > 1 << 63 {
            // Past the most negative int64, which nothing writes.
            self.fail(Error::BadDescriptor);
            return (0, false);
        }
        (magnitude, negative)
    }

    /// Reads the `width` bytes after the code.
    fn compact_magnitude(&mut self, width: usize) -> u64 {
        let start = self.at + 1;
        if width > self.buf.len() - start {
            self.fail(Error::Truncated);
            return 0;
        }
        self.at = start + width;
        le_uint(&self.buf[start..start + width], width)
    }

    /// Reads a float64 or a float32, and says which width it was.
    pub fn compact_float(&mut self) -> (f64, u32) {
        let Some(&code) = self.buf.get(self.at) else {
            self.fail(Error::Truncated);
            return (0.0, 64);
        };
        match code {
            SMALL..=0xFF => {
                self.at += 1;
                (f64::from(code - SMALL), 64)
            }
            WHOLE => {
                self.at += 1;
                let (magnitude, negative) = self.compact_integer();
                let value = magnitude as f64;
                (if negative { -value } else { value }, 64)
            }
            FLOAT64..=0xE8 => {
                let reversed = self.compact_magnitude(usize::from(code - FLOAT64));
                (f64::from_bits(reversed.swap_bytes()), 64)
            }
            FLOAT32..=0xED => {
                let reversed = self.compact_magnitude(usize::from(code - FLOAT32));
                (
                    f64::from(f32::from_bits((reversed as u32).swap_bytes())),
                    32,
                )
            }
            _ => {
                self.fail(Error::BadDescriptor);
                (0.0, 64)
            }
        }
    }

    /// Returns a string's bytes as a sub-slice of the message.
    pub fn compact_string(&mut self) -> &'a [u8] {
        let Some(&code) = self.buf.get(self.at) else {
            self.fail(Error::Truncated);
            return &[];
        };
        if code & 0xE0 == STRING {
            return self.compact_body(self.at + 1, usize::from(code & FIX_STRING));
        }
        self.compact_sized(STRING8)
    }

    /// Returns opaque bytes as a sub-slice of the message.
    pub fn compact_bytes(&mut self) -> &'a [u8] {
        self.compact_sized(BYTES8)
    }

    /// Reads a value whose code is `first` + 0, 1 or 2 for a length of one, two
    /// or four bytes.
    fn compact_sized(&mut self, first: u8) -> &'a [u8] {
        let Some(&code) = self.buf.get(self.at) else {
            self.fail(Error::Truncated);
            return &[];
        };
        if code < first || code > first + 2 {
            self.fail(Error::BadDescriptor);
            return &[];
        }
        let width = 1_usize << (code - first);
        let start = self.at + 1;
        if width > self.buf.len() - start {
            self.fail(Error::Truncated);
            return &[];
        }
        let Ok(size) = usize::try_from(le_uint(&self.buf[start..start + width], width)) else {
            self.fail(Error::Truncated);
            return &[];
        };
        self.compact_body(start + width, size)
    }

    fn compact_body(&mut self, start: usize, size: usize) -> &'a [u8] {
        if size > self.buf.len() - start {
            self.fail(Error::Truncated);
            return &[];
        }
        self.at = start + size;
        &self.buf[start..start + size]
    }

    /// Reads a map's entry count and leaves the cursor on its first key. A count
    /// the bytes left cannot hold, at two codes an entry, is refused, which is
    /// what makes it safe to allocate for.
    pub fn compact_map(&mut self) -> Option<usize> {
        self.compact_count(MAP, MAP_N, 2)
    }

    /// Reads a list's value count and leaves the cursor on its first value.
    pub fn compact_list(&mut self) -> Option<usize> {
        self.compact_count(LIST, LIST_N, 1)
    }

    fn compact_count(&mut self, fix: u8, counted: u8, min_size: usize) -> Option<usize> {
        let Some(&code) = self.buf.get(self.at) else {
            self.fail(Error::Truncated);
            return None;
        };
        let count;
        if code & 0xF0 == fix {
            count = usize::from(code) & FIX_MAX;
            self.at += 1;
        } else if code == counted {
            let Some((value, width)) = read_count(&self.buf[self.at + 1..]) else {
                self.fail(Error::Truncated);
                return None;
            };
            count = value;
            self.at += 1 + width;
        } else {
            self.fail(Error::BadDescriptor);
            return None;
        }
        if count > (self.buf.len() - self.at) / min_size {
            self.fail(Error::Truncated);
            return None;
        }
        Some(count)
    }

    /// Reads the tag that names a struct def and leaves the cursor on the
    /// element behind it, which is in the descriptor form.
    pub fn compact_typed(&mut self) -> Option<u32> {
        if self.compact_kind() != Kind::Typed {
            self.fail(self.compact_error());
            return None;
        }
        let Some((index, width)) = read_count(&self.buf[self.at + 1..]) else {
            self.fail(Error::Truncated);
            return None;
        };
        self.at += 1 + width;
        u32::try_from(index).ok().or_else(|| {
            self.fail(Error::BadSection);
            None
        })
    }

    /// Why the value at the cursor is not the one asked for.
    fn compact_error(&self) -> Error {
        if let Some(err) = &self.err {
            return err.clone();
        }
        if self.at >= self.buf.len() {
            return Error::Truncated;
        }
        Error::BadDescriptor
    }
}
