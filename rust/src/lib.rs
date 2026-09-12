//! A Rust port of [colbin](https://github.com/ivanjoz/colbin), a byte-aligned
//! binary format for records. The Go packages in the parent directory are the
//! specification; this crate mirrors them and adds nothing.
//!
//! # One format
//!
//! A message is a root descriptor byte and then a sequence of fields:
//!
//! ```text
//! [key][descriptor][payload]
//! ```
//!
//! Nothing is packed across a byte boundary. No size is a varint — a header
//! carries the common size and, when it does not fit, names the width of the one
//! that follows, so no read is ever a loop whose trip count is data. A field
//! holding its zero value is not written at all, which is where most of the
//! saving comes from.
//!
//! # Layers
//!
//! | module | what it is |
//! |---|---|
//! | [`wire`] | the format: field framing, both key widths, composites, tables |
//! | [`column`] | the column codec: blocks of 128 residuals at a chosen bit width |
//! | [`packed5`] | an opt-in string packing, off by default |
//! | [`codec`] | the [`Colbin`] trait and the root descriptor, which `#[derive(Colbin)]` writes against |
//!
//! # Reading and writing a record
//!
//! ```
//! # #[cfg(feature = "derive")]
//! # fn main() -> Result<(), colbin::Error> {
//! use colbin::Colbin;
//!
//! #[derive(Colbin, Debug, Default, PartialEq)]
//! struct Charge {
//!     #[cb(0)] company_id: u32,
//!     #[cb(1)] user_id: u32,
//!     #[cb(2)] note: String,
//! }
//!
//! let charge = Charge { company_id: 7, user_id: 42, note: "ok".into() };
//! let message = charge.encode();
//! assert_eq!(Charge::decode(&message)?, charge);
//! # Ok(())
//! # }
//! # #[cfg(not(feature = "derive"))]
//! # fn main() {}
//! ```
//!
//! A caller that would rather not derive drives [`wire::Writer`] and
//! [`wire::Reader`] directly, which is what the generated code does.

#![forbid(unsafe_code)]

pub mod codec;
pub mod column;
pub mod packed5;
pub mod wire;

mod error;

pub use codec::{
    Colbin, ColumnKind, ColumnSpec, MapValue, ROOT_STRUCT_NARROW, ROOT_STRUCT_WIDE,
    TABLE_THRESHOLD, assign_ids, fnv8,
};
pub use error::Error;

/// `#[derive(Colbin)]`, which generates the straight-line encode and decode a
/// hand-written one would be.
#[cfg(feature = "derive")]
pub use colbin_derive::Colbin;
