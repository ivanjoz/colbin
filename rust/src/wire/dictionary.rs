//! The dictionary column: a table's string column carried as ids into a
//! dictionary the message holds once, ahead of its body. Mirrors
//! `wire/dictionary.go`.
//!
//! ```text
//! wide    [key] [0xB0 | lw] [length] [dict] [column]
//! narrow  [key | 1101]      [length] [dict] [column]
//!
//! wide    [key] [0xB4 | lw] [length] [dict] [presence] [column]     nullable
//! narrow  [key | 1111]      [length] [dict] [presence] [column]
//! ```
//!
//! `dict` is a count naming which of the message's dictionaries the ids index,
//! and `column` is the blocked column codec over the ids, one per row — or one
//! per row *present* for the nullable form. What the ids mean is the walk's to
//! resolve: this module only frames them.

use super::narrow::{DICT_FLAGS, NULLABLE_DICT_FLAGS, Reader};
use super::nullable::{NULLABLE_DETAIL, nullable_column};
use super::read_count;
use super::wide::{CLASS_COL, Reader8};
use crate::{Error, column};
use alloc::vec::Vec;

/// The COL descriptor's high bits, `1 100 ....`.
const COL_DESCRIPTOR: u8 = 0b1000 | CLASS_COL;

fn dict_ids(body: &[u8], rows: usize) -> Result<(usize, Vec<i64>), Error> {
    let (dict, at) = read_count(body).ok_or(Error::Truncated)?;
    let mut ids = alloc::vec![0_i64; rows];
    column::decode_array(&body[at..], rows, &mut ids)?;
    Ok((dict, ids))
}

fn nullable_dict_ids(body: &[u8], rows: usize) -> Result<(usize, &[u8], Vec<i64>), Error> {
    let (dict, at) = read_count(body).ok_or(Error::Truncated)?;
    let (presence, ids) = nullable_column(&body[at..], rows)?;
    Ok((dict, presence, ids))
}

impl<'a> Reader8<'a> {
    /// Whether the field at the cursor is a COL, which under a string column's
    /// key is a dictionary column.
    #[must_use]
    pub fn is_dict_column(&self) -> bool {
        self.buf
            .get(self.at + 1)
            .is_some_and(|&desc| desc >> 4 == COL_DESCRIPTOR && desc & NULLABLE_DETAIL == 0)
    }

    /// Whether the field at the cursor is a COL with the nullable bit, which
    /// under a string column's key is a nullable dictionary column.
    #[must_use]
    pub fn is_nullable_dict_column(&self) -> bool {
        self.buf
            .get(self.at + 1)
            .is_some_and(|&desc| desc >> 4 == COL_DESCRIPTOR && desc & NULLABLE_DETAIL != 0)
    }

    /// Reads a dictionary column of `rows` rows: which dictionary, and the ids.
    pub fn dict_column(&mut self, rows: usize) -> Option<(usize, Vec<i64>)> {
        let body = self.composite_body(CLASS_COL)?;
        self.fail_on(dict_ids(body, rows))
    }

    /// Reads a nullable dictionary column of `rows` rows: which dictionary, the
    /// presence, and the ids of the rows present.
    pub fn nullable_dict_column(&mut self, rows: usize) -> Option<(usize, &'a [u8], Vec<i64>)> {
        let body = self.composite_body(CLASS_COL)?;
        self.fail_on(nullable_dict_ids(body, rows))
    }

    fn fail_on<T>(&mut self, result: Result<T, Error>) -> Option<T> {
        match result {
            Ok(found) => Some(found),
            Err(err) => {
                self.fail(err);
                None
            }
        }
    }
}

impl<'a> Reader<'a> {
    /// Whether the field at the cursor is the length form with the dictionary
    /// flag.
    #[must_use]
    pub fn is_dict_column(&self) -> bool {
        self.buf
            .get(self.at)
            .is_some_and(|&header| header & 0b1111 == DICT_FLAGS)
    }

    /// Whether the field at the cursor is the length form with the nullable
    /// dictionary flags.
    #[must_use]
    pub fn is_nullable_dict_column(&self) -> bool {
        self.buf
            .get(self.at)
            .is_some_and(|&header| header & 0b1111 == NULLABLE_DICT_FLAGS)
    }

    /// Reads a dictionary column of `rows` rows: which dictionary, and the ids.
    pub fn dict_column(&mut self, rows: usize) -> Option<(usize, Vec<i64>)> {
        let body = self.nullable_body(DICT_FLAGS)?;
        match dict_ids(body, rows) {
            Ok(found) => Some(found),
            Err(err) => {
                self.fail(err);
                None
            }
        }
    }

    /// Reads a nullable dictionary column of `rows` rows: which dictionary, the
    /// presence, and the ids of the rows present.
    pub fn nullable_dict_column(&mut self, rows: usize) -> Option<(usize, &'a [u8], Vec<i64>)> {
        let body = self.nullable_body(NULLABLE_DICT_FLAGS)?;
        match nullable_dict_ids(body, rows) {
            Ok(found) => Some(found),
            Err(err) => {
                self.fail(err);
                None
            }
        }
    }
}
