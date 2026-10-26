//! The list column: a table's `Vec<Struct>` field, as columns. Mirrors
//! `wire/listcolumn.go`.
//!
//! ```text
//! wide    [key] [COL | 1000 | lw] [length] [children] [lengths] [columns]
//! narrow  [key | 1100]            [length] [children] [lengths] [columns]
//! ```
//!
//! `children` is a count, the elements in all; `lengths` the blocked column
//! codec over the rows' element counts, one per row; and `columns` the element
//! table's column run, to the end of the field, keyed at the width a table of the
//! element type takes there. The counts add up to `children`, which a reader
//! checks before it allocates.

use super::narrow::Reader;
use super::read_count;
use super::wide::{CLASS_COL, Reader8};
use crate::{Error, column};
use alloc::vec::Vec;

/// The wide COL detail bit of a list column.
const LIST_COLUMN_DETAIL: u8 = 0b1000;

/// A list column's body, split.
pub struct ListColumn<'a> {
    /// The elements in all.
    pub children: usize,
    /// Each row's count of them.
    pub lengths: Vec<i64>,
    /// The element table's column run.
    pub run: &'a [u8],
}

fn list_column(body: &[u8], rows: usize) -> Result<ListColumn<'_>, Error> {
    let (children, at) = read_count(body).ok_or(Error::Truncated)?;
    let mut lengths = alloc::vec![0_i64; rows];
    let used = column::decode_array(&body[at..], rows, &mut lengths)?;
    if used > body.len() - at {
        return Err(Error::Truncated);
    }
    let mut left = children;
    for &length in &lengths {
        if length < 0 || length as u64 > left as u64 {
            return Err(Error::BadLengths);
        }
        left -= length as usize;
    }
    if left != 0 {
        return Err(Error::BadLengths);
    }
    Ok(ListColumn {
        children,
        lengths,
        run: &body[at + used..],
    })
}

impl<'a> Reader8<'a> {
    /// Reads a list column of a table of `rows` rows.
    pub fn list_column(&mut self, rows: usize) -> Option<ListColumn<'a>> {
        let Some(&desc) = self.buf.get(self.at + 1) else {
            self.fail(Error::Truncated);
            return None;
        };
        if desc >> 4 != 0b1000 | CLASS_COL || desc & LIST_COLUMN_DETAIL == 0 {
            self.fail(Error::BadDescriptor);
            return None;
        }
        let body = self.composite_body(CLASS_COL)?;
        match list_column(body, rows) {
            Ok(found) => Some(found),
            Err(err) => {
                self.fail(err);
                None
            }
        }
    }
}

impl<'a> Reader<'a> {
    /// [`Reader8::list_column`] with four key bits.
    pub fn list_column(&mut self, rows: usize) -> Option<ListColumn<'a>> {
        let (nibble, body) = self.field()?;
        if nibble != super::narrow::NIBBLE_LENGTH {
            self.fail(Error::BadEscape);
            return None;
        }
        match list_column(body, rows) {
            Ok(found) => Some(found),
            Err(err) => {
                self.fail(err);
                None
            }
        }
    }
}
