//! colbin's wire format: a byte-aligned `[key][descriptor][payload]` layout
//! where a zero-valued field is not written at all. The Go `wire` package is the
//! specification; this module mirrors it.
//!
//! ```
//! use colbin::wire::{Reader, Writer};
//!
//! let mut buf = Vec::new();
//! let mut w = Writer::new(&mut buf);
//! w.u32(0, 7);
//! w.u16(1, 103);
//! w.string(2, "ok");
//!
//! let mut r = Reader::new(&buf);
//! while r.more() {
//!     match r.key() {
//!         0 => assert_eq!(r.u32(), 7),
//!         1 => assert_eq!(r.u16(), 103),
//!         2 => assert_eq!(r.string(), "ok"),
//!         _ => r.skip(),
//!     }
//! }
//! r.err()?;
//! # Ok::<(), colbin::Error>(())
//! ```
//!
//! # Three framings of a key run
//!
//! | | type | key | skip | fields |
//! |---|---|---|---|---|
//! | narrow | [`Writer`] / [`Reader`] | 4 bits, shares the descriptor byte | no | 16 |
//! | wide | [`Writer8`] / [`Reader8`] | 8 bits, own byte | **yes** | 256 |
//! | bitmap | [`BitmapWriter`] / [`BitmapReader`] | a presence bitmap, no key at all | yes | 64 |
//!
//! They are separate types, not a flag, exactly as in Go: a key width the
//! compiler cannot see is a width it cannot fold. The framing is written three
//! times; the value codecs under it — magnitudes, blobs, array elements — are
//! written once and shared by this module.
//!
//! # Wire format
//!
//! A message is a sequence of fields and ends when its buffer ends — the frame
//! that carries it already states its length. Every multi-byte quantity is
//! little-endian. A field whose value is zero, empty or false is omitted.
//!
//! ```text
//! integer        [key:4][positive:1][n:3]                       [magnitude: n bytes]
//!
//!     n          0 → no bytes, the value is 1 (which is what makes a true bool
//!                one byte) · 1..6 → that many bytes · 7 → eight bytes
//!     positive   1 = the bytes are a magnitude; 0 = negative, bytes are |value|
//!
//! unsigned       [key:4][code:4]                                [magnitude]
//!
//!     code       0..7 → the value itself, no payload
//!                8..15 → a magnitude of (code - 7) bytes
//!
//! float          the integer shape, carrying the IEEE-754 bit pattern with its
//!                bytes reversed, so the trim reaches the zero low mantissa bytes
//!
//! string/bytes   [key:4][more:1][size hi:3] [size lo:8]         [bytes: size]
//!                [key:4][1][escape:3]       [size: 2|4|8]       [bytes: size]
//!
//! integer array  [key:4][positive:1][width:2][more:1] [count:8] [count × width]
//!
//! string array   [key:4][more:1][count:11]   then per element
//!                [size: 1 byte, 0xFF → 4 bytes follow] [bytes: size]
//! ```
//!
//! # Nothing has a size ceiling, and nothing is a varint
//!
//! Every size escalates to a width that holds it, named by the header rather
//! than discovered byte by byte. So no read is ever a loop whose trip count is
//! data — which is also why there is no continuation run for an unauthenticated
//! peer to make unbounded — and **a write cannot fail**: neither writer has an
//! error, because anything the format could not express would have to be a value
//! that does not fit in memory.

use alloc::vec::Vec;

mod bitmap;
mod dynamic;
mod narrow;
mod wide;

pub use bitmap::{BitmapReader, BitmapWriter, MAX_BITMAP_KEY};
pub use dynamic::{Kind, kind_of};
pub use narrow::{Reader, Writer};
pub use wide::{Reader8, Writer8};

/// What four key bits buy: keys 0..=15. A record needing a seventeenth field
/// needs a different key width, not a wider key — the key shares a byte with the
/// field's own header bits and there is nothing to take them from.
pub const MAX_FIELDS: usize = 16;

/// What eight key bits buy: keys 0..=255.
pub const MAX_WIDE_FIELDS: usize = 256;

// Integer size codes. Code 0 carries no bytes at all and means the magnitude is
// one, which is what makes a true bool a single byte. Codes 1..=6 are the byte
// count outright; code 7 is eight bytes, so a seven-byte magnitude rounds up to
// eight and every other width is exact.
pub(crate) const SIZE_CODE_ONE: u8 = 0;
pub(crate) const SIZE_CODE_8BYTES: u8 = 7;

/// Maps a size code to its byte count.
pub(crate) const MAGNITUDE_WIDTH: [usize; 8] = [0, 1, 2, 3, 4, 5, 6, 8];

// Array element width codes.
pub(crate) const WIDTH_CODE_1BYTE: u8 = 0;
pub(crate) const WIDTH_CODE_2BYTES: u8 = 1;
pub(crate) const WIDTH_CODE_4BYTES: u8 = 2;
pub(crate) const WIDTH_CODE_8BYTES: u8 = 3;

// A size or count did not fit its inline bits and a wider one follows. It is
// bit 3 in a blob header and bit 0 in an array header, because an array spends
// bits 2..1 on its element width.
pub(crate) const MORE_SIZE_FLAG: u8 = 0b1000;
pub(crate) const MORE_ARRAY_LEN_FLAG: u8 = 0b0001;
pub(crate) const ARRAY_WIDTH_SHIFT: u8 = 1;

// Bit 3 of a *signed* integer header, directly above the three size-code bits,
// and bit 3 of an array header. They are deliberately separate constants: one
// value used for both would land on a size-code bit.
pub(crate) const INT_POSITIVE_FLAG: u8 = 0b1000;
pub(crate) const ARRAY_POSITIVE_FLAG: u8 = 0b1000;

// An unsigned narrow field spends no sign bit, so all sixteen nibble codes carry
// information rather than eight: 0..=7 are the value itself and 8..=15 are a
// magnitude of (code - 7) bytes. A K4 reader has the schema and therefore
// already knows whether the field it is looking at is signed.
pub(crate) const UINT_INLINE_MAX: u64 = 7;
pub(crate) const UINT_WIDTH_BASE: u8 = 8;

// Escape codes, which occupy a blob header's three size bits once `more` is set
// and they no longer carry size.
pub(crate) const ESCAPE_2BYTES: u8 = 0;
pub(crate) const ESCAPE_4BYTES: u8 = 1;
pub(crate) const ESCAPE_8BYTES: u8 = 2;

/// The packed5 escapes. A narrow blob header has no `enc` field — under narrow
/// keys the schema says what a field is, not the wire — so a packed string names
/// itself here, and carries the one bit the schema cannot know: the case mode
/// its unit stream opens in. Spending four codes buys a two-byte header, the
/// same as a raw blob's. Code 7 is still free.
pub(crate) const ESCAPE_PACKED1_LO: u8 = 3;
pub(crate) const ESCAPE_PACKED1_UP: u8 = 4;
pub(crate) const ESCAPE_PACKED4_LO: u8 = 5;
pub(crate) const ESCAPE_PACKED4_UP: u8 = 6;

// What each header carries before an escape is needed.
pub(crate) const INLINE_BLOB_SIZE: usize = (1 << 11) - 1;
pub(crate) const INLINE_ARRAY_COUNT: usize = (1 << 8) - 1;

/// The largest element length a string array writes in one byte; `0xFF` escapes
/// to four bytes.
pub(crate) const INLINE_ELEMENT_SIZE: usize = 0xFE;
pub(crate) const ELEMENT_SIZE_ESCAPE: u8 = 0xFF;

/// Where a composite's length placeholder sits, so `close` can patch it. It is
/// a value, so nesting costs no allocation.
#[derive(Clone, Copy, Debug)]
pub struct Mark {
    at: usize,
}

/// What the reserved byte holds before the body grows past it.
pub(crate) const INLINE_COMPOSITE_LENGTH: usize = 0xFF;

/// The code that describes a magnitude, and the byte count that code implies —
/// which is the magnitude's own byte count except at seven, which rounds to
/// eight.
#[allow(clippy::cast_possible_truncation)]
pub(crate) fn size_code_for(magnitude: u64) -> (u8, usize) {
    let width = bit_length(magnitude).div_ceil(8);
    if width >= 7 {
        return (SIZE_CODE_8BYTES, 8);
    }
    (width as u8, width)
}

/// `bits.Len64`: the number of bits needed to represent `value`.
#[inline]
pub(crate) const fn bit_length(value: u64) -> usize {
    (64 - value.leading_zeros()) as usize
}

/// Writes a magnitude's low `width` bytes, little-endian.
#[inline]
pub(crate) fn append_magnitude(buf: &mut Vec<u8>, magnitude: u64, width: usize) {
    buf.extend_from_slice(&magnitude.to_le_bytes()[..width]);
}

/// Reads a magnitude's `width` bytes as a little-endian integer. The caller has
/// already checked that `buf` holds them.
#[inline]
pub(crate) fn le_uint(buf: &[u8], width: usize) -> u64 {
    if buf.len() >= 8 {
        let window: [u8; 8] = buf[..8].try_into().expect("eight bytes");
        let mask = if width >= 8 {
            u64::MAX
        } else {
            (1_u64 << (8 * width)) - 1
        };
        return u64::from_le_bytes(window) & mask;
    }
    let mut value = 0_u64;
    for index in (0..width).rev() {
        value = (value << 8) | u64::from(buf[index]);
    }
    value
}

/// The narrowest `lw` code that holds `n`, and the byte count it implies.
pub(crate) const fn length_code_for(n: u64) -> (u8, usize) {
    if n <= 0xFF {
        (0, 1)
    } else if n <= 0xFFFF {
        (1, 2)
    } else if n <= 0xFFFF_FFFF {
        (2, 4)
    } else {
        (3, 8)
    }
}

/// `lw` selects the width of the length field that follows: 0 → 1 byte, 1 → 2,
/// 2 → 4, 3 → 8. There is no LEB128 anywhere, so reading a length is a branch
/// and one load, never a loop.
pub(crate) const LENGTH_WIDTH: [usize; 4] = [1, 2, 4, 8];

/// What [`append_count`] will occupy.
pub(crate) const fn count_bytes(count: usize) -> usize {
    if count <= INLINE_ELEMENT_SIZE { 1 } else { 5 }
}

/// Writes an element count the same way an element length is written: one byte,
/// escaping to four. A composite with more than 254 elements is rare enough that
/// the escape costs nothing on average, and the common one is a byte.
#[allow(clippy::cast_possible_truncation)]
pub(crate) fn append_count(buf: &mut Vec<u8>, count: usize) {
    if count <= INLINE_ELEMENT_SIZE {
        buf.push(count as u8);
        return;
    }
    buf.push(ELEMENT_SIZE_ESCAPE);
    buf.extend_from_slice(&(count as u32).to_le_bytes());
}

/// Reverses [`append_count`], returning the count and the offset just past it.
pub(crate) fn read_count(body: &[u8]) -> Option<(usize, usize)> {
    let first = *body.first()?;
    if first != ELEMENT_SIZE_ESCAPE {
        return Some((usize::from(first), 1));
    }
    let bytes: [u8; 4] = body.get(1..5)?.try_into().expect("four bytes");
    Some((u32::from_le_bytes(bytes) as usize, 5))
}

/// Every integer type an array field can hold.
pub trait Integer: Copy {
    /// Whether an element of this type can read as negative, which is what
    /// decides between a magnitude array and a two's complement one.
    ///
    /// It is not quite "is this type signed". Go answers the question with
    /// `isSigned`, which converts −1 to the element type and asks whether the
    /// result is still negative *as an `int64`* — and a `uint64` holding
    /// `1<<64 - 1` is −1 there, so a `[]uint64` travels as two's complement
    /// where a `[]uint32` travels as magnitudes. That is lossless in both
    /// directions, since the narrowing and the sign extension are bit-exact
    /// inverses, and it is usually *smaller*: `u64::MAX` costs one byte as −1
    /// against eight as a magnitude.
    ///
    /// It is spelled out per type here rather than derived, because the wire is
    /// what Go writes and a port that computed a better answer would simply
    /// disagree with it.
    const SIGNED: bool;
    /// Go's `int64(value)`.
    fn as_i64(self) -> i64;
    /// Go's `uint64(value)`: sign-extending for a signed type, zero-extending
    /// for an unsigned one.
    fn as_u64(self) -> u64;
    /// Truncating, like Go's `T(x)`.
    fn from_u64(value: u64) -> Self;
    /// Truncating, like Go's `T(x)`.
    fn from_i64(value: i64) -> Self;
}

macro_rules! impl_integer {
    ($($type:ty => $signed:literal),*) => {$(
        impl Integer for $type {
            const SIGNED: bool = $signed;
            #[inline]
            #[allow(clippy::cast_lossless, clippy::cast_possible_wrap)]
            fn as_i64(self) -> i64 { self as i64 }
            #[inline]
            #[allow(clippy::cast_lossless, clippy::cast_sign_loss)]
            fn as_u64(self) -> u64 { self as u64 }
            #[inline]
            #[allow(clippy::cast_possible_truncation, clippy::cast_possible_wrap)]
            fn from_u64(value: u64) -> Self { value as Self }
            #[inline]
            #[allow(clippy::cast_possible_truncation, clippy::cast_sign_loss)]
            fn from_i64(value: i64) -> Self { value as Self }
        }
    )*};
}

impl_integer!(
    i8 => true, i16 => true, i32 => true, i64 => true,
    u8 => false, u16 => false, u32 => false,
    // Not a slip: see `SIGNED`. `u64::MAX` widens to −1, so Go's `isSigned`
    // answers true here and the array is two's complement.
    u64 => true
);

/// The one pass that decides both the sign flag and the width: a negative
/// anywhere turns the whole array into two's complement, which needs the width
/// that holds the most negative element as well as the largest positive one.
///
/// It returns the pieces rather than a header byte, because the two key widths
/// spend them in different places: K4 ORs them into the byte it shares with the
/// key, K8 into a descriptor of its own.
pub(crate) fn array_plan_of<T: Integer>(values: &[T]) -> (bool, usize, u8) {
    let mut all_positive = true;
    let mut widest = 0_u64;
    for value in values {
        // An unsigned type never has a negative element, however its top bit
        // reads as an i64: the test below is what keeps a u64 past 2^63 from
        // being mistaken for a negative number.
        let as_signed = value.as_i64();
        if T::SIGNED && as_signed < 0 {
            all_positive = false;
            let magnitude = (as_signed as u64).wrapping_neg();
            if magnitude > widest {
                widest = magnitude;
            }
            continue;
        }
        let magnitude = value.as_u64();
        if magnitude > widest {
            widest = magnitude;
        }
    }
    let (width, code) = array_width(widest, all_positive);
    (all_positive, width, code)
}

/// Picks the narrowest element width that holds every element. A two's
/// complement array needs one more bit than its magnitude, which is what the
/// shift below tests.
pub(crate) fn array_width(mut widest: u64, all_positive: bool) -> (usize, u8) {
    if !all_positive {
        // A magnitude that already fills the top bit cannot gain one: shifting it
        // would wrap to zero and pick a one-byte width for i64::MIN.
        if widest > 1 << 62 {
            return (8, WIDTH_CODE_8BYTES);
        }
        widest <<= 1;
    }
    if widest <= 0xFF {
        (1, WIDTH_CODE_1BYTE)
    } else if widest <= 0xFFFF {
        (2, WIDTH_CODE_2BYTES)
    } else if widest <= 0xFFFF_FFFF {
        (4, WIDTH_CODE_4BYTES)
    } else {
        (8, WIDTH_CODE_8BYTES)
    }
}

/// Writes the elements with the width hoisted out of the loop, so each one is a
/// store rather than a switch. That hoist is the difference between a
/// byte-aligned array and a fast one.
#[allow(clippy::cast_possible_truncation)]
pub(crate) fn append_elements<T: Integer>(buf: &mut Vec<u8>, values: &[T], width: usize) {
    match width {
        1 => {
            for value in values {
                buf.push(value.as_u64() as u8);
            }
        }
        2 => {
            for value in values {
                buf.extend_from_slice(&(value.as_u64() as u16).to_le_bytes());
            }
        }
        4 => {
            for value in values {
                buf.extend_from_slice(&(value.as_u64() as u32).to_le_bytes());
            }
        }
        _ => {
            for value in values {
                buf.extend_from_slice(&value.as_u64().to_le_bytes());
            }
        }
    }
}

/// Turns an array field's bytes into elements, the mirror of
/// [`append_elements`] and with the same width hoist.
pub(crate) fn append_array<T: Integer>(
    dst: &mut Vec<T>,
    elements: &[u8],
    width: usize,
    positive: bool,
) {
    if positive {
        append_magnitudes(dst, elements, width);
        return;
    }
    append_twos_complement(dst, elements, width);
}

fn append_magnitudes<T: Integer>(dst: &mut Vec<T>, elements: &[u8], width: usize) {
    match width {
        1 => {
            for raw in elements {
                dst.push(T::from_u64(u64::from(*raw)));
            }
        }
        2 => {
            for bytes in elements.as_chunks::<2>().0 {
                dst.push(T::from_u64(u64::from(u16::from_le_bytes(*bytes))));
            }
        }
        4 => {
            for bytes in elements.as_chunks::<4>().0 {
                dst.push(T::from_u64(u64::from(u32::from_le_bytes(*bytes))));
            }
        }
        _ => {
            for bytes in elements.as_chunks::<8>().0 {
                dst.push(T::from_u64(u64::from_le_bytes(*bytes)));
            }
        }
    }
}

fn append_twos_complement<T: Integer>(dst: &mut Vec<T>, elements: &[u8], width: usize) {
    match width {
        1 => {
            for raw in elements {
                dst.push(T::from_i64(i64::from(*raw as i8)));
            }
        }
        2 => {
            for bytes in elements.as_chunks::<2>().0 {
                dst.push(T::from_i64(i64::from(i16::from_le_bytes(*bytes))));
            }
        }
        4 => {
            for bytes in elements.as_chunks::<4>().0 {
                dst.push(T::from_i64(i64::from(i32::from_le_bytes(*bytes))));
            }
        }
        _ => {
            for bytes in elements.as_chunks::<8>().0 {
                dst.push(T::from_i64(i64::from_le_bytes(*bytes)));
            }
        }
    }
}
