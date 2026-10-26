//! Grouped tables: a long table cut into groups of rows (`INTERNALS.md` §6.3).
//!
//! ```text
//! narrow  [key:4][1 1 1 1]  [length] [rows] [groupRows] group*
//! wide    [key][0xEC | lw]  [length] [rows] [groupRows] group*
//! group   [byteLength:u32]  column run
//! ```
//!
//! A group is the column run a table of its rows would hold, keyed at the same
//! width. Every group holds `groupRows` rows but the last, which holds the rest,
//! and there are exactly as many as that takes. Go writes a table field past
//! 4096 rows this way; a dynamic value's table never is.

use super::read_count;
use crate::error::Error;

/// A narrow table's `f1`, set when its rows are in groups.
pub(crate) const NARROW_GROUPED: u8 = 0b10;
/// A wide table's MAP detail bit 2, set when its rows are in groups.
pub(crate) const GROUPED_FLAG: u8 = 0b0100;
/// The bytes in front of a group's column run.
const GROUP_HEADER: usize = 4;

/// Reads a table body's row count and, for a grouped one, its rows per group,
/// returning what follows: the column run, or the groups.
pub(crate) fn table_head(body: &[u8], grouped: bool) -> Result<(usize, usize, &[u8]), Error> {
    let (rows, at) = read_count(body).ok_or(Error::Truncated)?;
    if !grouped {
        return Ok((rows, 0, &body[at..]));
    }
    let (group_rows, used) = read_count(&body[at..]).ok_or(Error::Truncated)?;
    if group_rows == 0 {
        return Err(Error::BadGroup);
    }
    Ok((rows, group_rows, &body[at + used..]))
}

/// The column runs of a table's body, each with the rows it holds: the body
/// itself for a table written whole (`group_rows` 0), and one per group for a
/// grouped one, after which a byte left over is an error.
pub struct Runs<'a> {
    rest: &'a [u8],
    rows: usize,
    group_rows: usize,
    start: usize,
    whole: bool,
}

impl<'a> Runs<'a> {
    /// The runs of a body [`super::Reader::table`] or [`super::Reader8::table`]
    /// returned.
    #[must_use]
    pub const fn new(rows: usize, group_rows: usize, body: &'a [u8]) -> Self {
        Runs {
            rest: body,
            rows,
            group_rows,
            start: 0,
            whole: group_rows == 0,
        }
    }

    fn fail(&mut self) -> Option<Result<(usize, &'a [u8]), Error>> {
        self.start = self.rows;
        self.rest = &[];
        Some(Err(Error::BadGroup))
    }
}

impl<'a> Iterator for Runs<'a> {
    type Item = Result<(usize, &'a [u8]), Error>;

    fn next(&mut self) -> Option<Self::Item> {
        if self.whole {
            self.whole = false;
            self.start = self.rows;
            return Some(Ok((self.rows, core::mem::take(&mut self.rest))));
        }
        if self.start >= self.rows {
            if self.rest.is_empty() {
                return None;
            }
            return self.fail();
        }
        let Some(header) = self.rest.get(..GROUP_HEADER) else {
            return self.fail();
        };
        let size = u32::from_le_bytes(header.try_into().expect("four bytes")) as usize;
        let Some(columns) = self
            .rest
            .get(GROUP_HEADER..GROUP_HEADER.saturating_add(size))
        else {
            return self.fail();
        };
        let rows = self.group_rows.min(self.rows - self.start);
        self.rest = &self.rest[GROUP_HEADER + size..];
        self.start = self.start.saturating_add(self.group_rows);
        Some(Ok((rows, columns)))
    }
}
