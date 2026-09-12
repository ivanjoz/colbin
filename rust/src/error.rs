//! The one error type, mirroring the sentinel errors the Go packages return.
//!
//! A reader defends against the network and a writer trusts its own program, so
//! every variant here is a read-side failure: there is nothing a caller can hand
//! a writer that it cannot express. See `wire/README.md`.

use core::fmt;

/// What a decode refused, and why.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Error {
    /// The message ends inside a field.
    Truncated,
    /// A declared size is larger than this platform can address.
    SizeTooLarge,
    /// A peer wrote a field wider than the type this side declares — a schema
    /// disagreement, refused rather than truncated into a different,
    /// valid-looking value.
    FieldTooWide,
    /// A size escape code this version does not assign.
    BadEscape,
    /// A descriptor this version does not assign, or one whose class is not the
    /// one the caller asked to read.
    BadDescriptor,
    /// A presence bitmap wider than the keys a run can hold, or one the buffer
    /// does not contain.
    BadBitmap,
    /// A column block declares a width above 64 bits.
    BadWidth,
    /// A column decode was handed an output slice shorter than the row count.
    ShortBuffer,
    /// Byte 0 is not a root descriptor this version writes.
    BadRoot(u8),
    /// A four-bit key run holds a field this type does not declare. Four
    /// descriptor bits have no room for a class, so nothing can size a field it
    /// cannot classify — the wide key width is the one that can skip.
    UnknownKey(u8),
    /// A string field holds bytes that are not UTF-8. The Go codec is byte
    /// exact and permits it; a Rust `String` cannot be.
    NotUtf8,
    /// A packed5 frame the decoder refused.
    Packed5(Packed5Error),
}

/// The packed5 failures, kept apart so the frame codec reads as its own thing.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Packed5Error {
    /// A frame ends before its declared payload, or a token's operand runs past
    /// the end of the bitstream.
    Truncated,
    /// Symbol indices 30 and 31 of opcode 29, which the current table leaves
    /// unassigned.
    ReservedSymbol,
    /// The length prefix is malformed, overlong, or describes a payload larger
    /// than the buffer.
    BadLength,
    /// The pad count exceeds the bits actually present in the payload.
    BadPadding,
}

impl fmt::Display for Error {
    fn fmt(&self, out: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Truncated => out.write_str("colbin: the message ends inside a field"),
            Self::SizeTooLarge => {
                out.write_str("colbin: a declared size is larger than this platform can address")
            }
            Self::FieldTooWide => {
                out.write_str("colbin: a field is wider than its declared type")
            }
            Self::BadEscape => out.write_str("colbin: unassigned size escape code"),
            Self::BadDescriptor => {
                out.write_str("colbin: unassigned or mismatched descriptor")
            }
            Self::BadBitmap => out.write_str("colbin: bad presence bitmap"),
            Self::BadWidth => out.write_str("colbin: column block width above 64 bits"),
            Self::ShortBuffer => out.write_str("colbin: column output slice too short"),
            Self::BadRoot(byte) => write!(
                out,
                "colbin: byte 0 is {byte:#04x}, which is not a root descriptor this version writes"
            ),
            Self::UnknownKey(key) => write!(
                out,
                "colbin: the message holds field id {key}, which this type does not declare, \
                 and a narrow key cannot be skipped"
            ),
            Self::NotUtf8 => out.write_str("colbin: a string field is not valid UTF-8"),
            Self::Packed5(inner) => write!(out, "colbin: packed5 {inner}"),
        }
    }
}

impl fmt::Display for Packed5Error {
    fn fmt(&self, out: &mut fmt::Formatter<'_>) -> fmt::Result {
        out.write_str(match self {
            Self::Truncated => "string truncated",
            Self::ReservedSymbol => "reserved symbol index",
            Self::BadLength => "bad length prefix",
            Self::BadPadding => "bad stream padding",
        })
    }
}

impl std::error::Error for Error {}

impl From<Packed5Error> for Error {
    fn from(inner: Packed5Error) -> Self {
        Self::Packed5(inner)
    }
}
