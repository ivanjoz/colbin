//! The nullable column: a table column of a pointer field (`Option<i32>`,
//! `Option<String>`, …) in which some rows are `None`. Mirrors `wire/nullable.go`.
//!
//! ```text
//! wide integer  [key] [0xB4 | lw] [length] [presence] [column]
//! wide string   [key] [0xCC | lw] [length] [presence] [elements]
//! narrow        [key | 1110]      [length] [presence] [column | elements]
//! ```
//!
//! `presence` is a bit per row of the table (or group), ⌈rows/8⌉ bytes, bit
//! `row % 8` of byte `row / 8` set where the row holds a value; bits past the
//! last row are clear. Only the rows that hold one follow: `column` is the
//! blocked column codec over them, `elements` their strings each behind its
//! size — so the count of either is the presence's population, and is never
//! stored.
//!
//! It is the third form of a pointer column, and the writer's to choose: no row
//! holds a value and the column is absent; every row does and it is the ordinary
//! column; some do and it is this. This module reads; [`Reader`] and
//! [`Reader8`] are the two key widths.

use super::narrow::Reader;
use super::wide::{CLASS_COL, CLASS_LIST, DESC_EXPLICIT, Reader8};
use crate::{Error, column};
use alloc::vec::Vec;

/// The wide COL and LIST detail bit of a nullable column.
pub(crate) const NULLABLE_DETAIL: u8 = 0b0100;

/// A presence bitmap's size over `rows` rows.
#[must_use]
pub const fn presence_bytes(rows: usize) -> usize {
    rows.div_ceil(8)
}

/// Whether `row` holds a value in `presence`.
#[must_use]
pub fn present(presence: &[u8], row: usize) -> bool {
    presence
        .get(row >> 3)
        .is_some_and(|byte| byte & (1 << (row & 7)) != 0)
}

/// Reads a presence bitmap of `rows` rows at the head of `body` and counts the
/// rows it says are present. It refuses a bitmap the body cannot hold, a bit set
/// past the last row, and a column with no row present, which the writer leaves
/// out.
pub(crate) fn read_presence(body: &[u8], rows: usize) -> Result<(&[u8], usize), Error> {
    let size = presence_bytes(rows);
    if size > body.len() {
        return Err(Error::Truncated);
    }
    let presence = &body[..size];
    if rows & 7 != 0 && presence[size - 1] >> (rows & 7) != 0 {
        return Err(Error::BadPresence);
    }
    let present: usize = presence.iter().map(|byte| byte.count_ones() as usize).sum();
    if present == 0 {
        return Err(Error::BadPresence);
    }
    Ok((presence, present))
}

/// Splits a nullable integer column, which is all of `body`, into its presence
/// and its values.
pub(crate) fn nullable_column(body: &[u8], rows: usize) -> Result<(&[u8], Vec<i64>), Error> {
    let (presence, present) = read_presence(body, rows)?;
    let mut values = alloc::vec![0_i64; present];
    column::decode_array(&body[presence.len()..], present, &mut values)?;
    Ok((presence, values))
}

impl<'a> Reader8<'a> {
    /// Whether the field at the cursor is a nullable column.
    #[must_use]
    pub fn is_nullable(&self) -> bool {
        self.buf.get(self.at + 1).is_some_and(|&desc| {
            let class = (desc >> 4) & 0b111;
            desc >= DESC_EXPLICIT
                && (class == CLASS_COL || class == CLASS_LIST)
                && desc & NULLABLE_DETAIL != 0
        })
    }

    /// Reads a nullable integer column of a table of `rows` rows: the presence,
    /// a sub-slice of the message, and the values of the rows present.
    pub fn nullable_column(&mut self, rows: usize) -> (&'a [u8], Vec<i64>) {
        let Some(body) = self.composite_body(CLASS_COL) else {
            return (&[], Vec::new());
        };
        match nullable_column(body, rows) {
            Ok(found) => found,
            Err(err) => {
                self.fail(err);
                (&[], Vec::new())
            }
        }
    }

    /// Reads a nullable string column of a table of `rows` rows: the presence and
    /// the strings of the rows present, as sub-slices of the message.
    pub fn nullable_strings_bytes(&mut self, rows: usize) -> (&'a [u8], Vec<&'a [u8]>) {
        let Some(body) = self.composite_body(CLASS_LIST) else {
            return (&[], Vec::new());
        };
        match nullable_elements(body, rows) {
            Ok(found) => found,
            Err(err) => {
                self.fail(err);
                (&[], Vec::new())
            }
        }
    }
}

impl<'a> Reader<'a> {
    /// Whether the field at the cursor is a nullable column.
    #[must_use]
    pub fn is_nullable(&self) -> bool {
        self.buf
            .get(self.at)
            .is_some_and(|&header| header & 0b1111 == super::narrow::NULLABLE_FLAGS)
    }

    /// [`Reader8::nullable_column`] with four key bits.
    pub fn nullable_column(&mut self, rows: usize) -> (&'a [u8], Vec<i64>) {
        let Some(body) = self.nullable_body(super::narrow::NULLABLE_FLAGS) else {
            return (&[], Vec::new());
        };
        match nullable_column(body, rows) {
            Ok(found) => found,
            Err(err) => {
                self.fail(err);
                (&[], Vec::new())
            }
        }
    }

    /// [`Reader8::nullable_strings_bytes`] with four key bits.
    pub fn nullable_strings_bytes(&mut self, rows: usize) -> (&'a [u8], Vec<&'a [u8]>) {
        let Some(body) = self.nullable_body(super::narrow::NULLABLE_FLAGS) else {
            return (&[], Vec::new());
        };
        match nullable_elements(body, rows) {
            Ok(found) => found,
            Err(err) => {
                self.fail(err);
                (&[], Vec::new())
            }
        }
    }

    /// A narrow column's framing, which has to be the length form with `flags`,
    /// and its body, advancing past the whole field.
    pub(super) fn nullable_body(&mut self, flags: u8) -> Option<&'a [u8]> {
        let (nibble, body) = self.field()?;
        if nibble != flags {
            self.fail(Error::BadEscape);
            return None;
        }
        Some(body)
    }
}

/// A nullable string column's body: the presence, then exactly as many elements
/// as it says are present, each behind its size, and nothing after.
fn nullable_elements(body: &[u8], rows: usize) -> Result<(&[u8], Vec<&[u8]>), Error> {
    let (presence, present) = read_presence(body, rows)?;
    let mut elements = Reader::new(&body[presence.len()..]);
    let mut out = Vec::with_capacity(present.min(body.len()));
    for _ in 0..present {
        match elements.element() {
            Some(element) => out.push(element),
            None => {
                elements.err()?;
                return Err(Error::BadPresence);
            }
        }
    }
    elements.err()?;
    if elements.more() {
        return Err(Error::BadPresence);
    }
    Ok((presence, out))
}
