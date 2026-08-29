//! The two integer codecs a colbin message uses.
//!
//! * [`get_varint`] is compact mode's selector varint (`compact/varint.go`): a
//!   bit-level form whose first unit is one bit narrower than LEB128's, spending
//!   that bit on a selector that adds a one-time four-bit payload nibble.
//! * [`decode_array`] is the columnar array codec (the `varint` package): a
//!   header byte naming a transform and a `(k, M)` varint width, followed by one
//!   residual per element. It backs every integer column in standard mode and
//!   every integer slice in compact mode, which is why the two modes cost the
//!   same for an array.

use crate::bitstream::BitReader;
use crate::{Error, try_vec};

// --- compact mode's selector varint ------------------------------------------

/// Payload bits in unit 0 without the nibble.
const UNIT0_BITS: u8 = 6;
/// The selector's one-time offset.
const NIBBLE_BITS: u8 = 4;
/// Payload bits per continuation unit.
const UNIT_BITS: u8 = 7;

/// Reads one selector varint:
///
/// ```text
/// unit 0    [selector:1] [cont:1] [payload:6]   (+ [payload:4] if selector)
/// unit i    [cont:1] [payload:7]
/// ```
///
/// A shift of 64 or more saturates rather than panicking, so a corrupt run of
/// continuation units yields a wrong value instead of a crash; it can still run
/// off the end, which the reader reports.
pub(crate) fn get_varint(reader: &mut BitReader<'_>) -> u64 {
    let unit = reader.get(1 + 1 + UNIT0_BITS);
    let selector = unit & 1 == 1;
    let mut more = unit & 2 == 2;
    let mut value = unit >> 2;
    let mut shift = UNIT0_BITS;
    if selector {
        value |= reader.get(NIBBLE_BITS) << UNIT0_BITS;
        shift = UNIT0_BITS + NIBBLE_BITS;
    }
    while more && reader.err().is_none() {
        let unit = reader.get(1 + UNIT_BITS);
        more = unit & 1 == 1;
        value |= shifted(unit >> 1, shift);
        shift = shift.saturating_add(UNIT_BITS);
    }
    value
}

/// `v << shift`, saturating to zero at 64 bits the way Go's shift does.
fn shifted(value: u64, shift: u8) -> u64 {
    if shift >= 64 { 0 } else { value << shift }
}

/// Maps an unsigned payload back onto the signed value it came from, used when
/// ALL_POSITIVE is clear.
pub(crate) fn unzigzag(value: u64) -> i64 {
    ((value >> 1) as i64) ^ -((value & 1) as i64)
}

// --- the columnar array codec ------------------------------------------------

const TR_DELTA: u8 = 1;
const TR_FOR: u8 = 2;
const TR_FIXED: u8 = 3;

/// Maps the header's 3-bit code to `d = M - k`. Code 7 is 8 rather than 7 so
/// that `k = 1` can still reach `M = 9`, which full-width 64-bit columns need.
const D_CODES: [u8; 8] = [0, 1, 2, 3, 4, 5, 6, 8];

/// Interprets the low `width` bits of `value` as two's complement and widens to
/// `i64`. This is also how a decoded value is truncated to its element width:
/// the Go codec writes `out[i] = T(v)`, and `T` is what the wire never carries.
pub(crate) fn sign_extend(value: u64, width: u8) -> i64 {
    if width < 64 && value & (1_u64 << (width - 1)) != 0 {
        return (value | !((1_u64 << width) - 1)) as i64;
    }
    value as i64
}

/// Reads one value written under `(k, m)` starting at `pos`, returning it with
/// the position just past its last byte.
fn get_km(buf: &[u8], mut pos: usize, k: u8, m: u8) -> Result<(u64, usize), Error> {
    let mut value = 0_u64;
    let mut shift = 0_u8;
    let mut unit = 1_u8;
    loop {
        let byte = *buf.get(pos).ok_or(Error::Truncated)?;
        pos += 1;
        if unit < k {
            // Guaranteed continuation: the whole byte is payload.
            value |= shifted(u64::from(byte), shift);
            shift = shift.saturating_add(8);
        } else if unit == m {
            // Guaranteed terminal: the whole byte is payload.
            return Ok((value | shifted(u64::from(byte), shift), pos));
        } else {
            value |= shifted(u64::from(byte & 0x7F), shift);
            shift = shift.saturating_add(7);
            if byte & 0x80 == 0 {
                return Ok((value, pos));
            }
        }
        unit = unit.saturating_add(1);
    }
}

/// Decodes `n` values of `width` bits from a frame written by the Go codec's
/// `AppendArray`, returning them with the number of bytes the frame occupied.
///
/// The element width never reaches the wire — it comes from the Go type on one
/// side and from the caller's schema on the other — so every value is truncated
/// to `width` and sign extended back, exactly as `out[i] = T(v)` does in Go.
pub(crate) fn decode_array(buf: &[u8], n: usize, width: u8) -> Result<(Vec<i64>, usize), Error> {
    let header = *buf.first().ok_or(Error::Truncated)?;
    let mut pos = 1;
    let transform = header & 0x03;
    let zigzag = header & 0x04 != 0;
    let k = ((header >> 3) & 0x03) + 1;
    let m = k + D_CODES[usize::from(header >> 5)];
    if n == 0 {
        return Ok((Vec::new(), pos));
    }

    let mut out = try_vec(n)?;
    if transform == TR_FIXED {
        let bytes = usize::from(width / 8);
        if buf.len() - pos < n * bytes {
            return Err(Error::Truncated);
        }
        for _ in 0..n {
            let mut raw = 0_u64;
            for byte in 0..bytes {
                raw |= u64::from(buf[pos]) << (8 * byte);
                pos += 1;
            }
            out.push(sign_extend(raw, width));
        }
        return Ok((out, pos));
    }

    match transform {
        TR_FOR => {
            let (raw, next) = get_km(buf, pos, k, m)?;
            pos = next;
            let base = unzigzag(raw);
            for _ in 0..n {
                let (delta, next) = get_km(buf, pos, k, m)?;
                pos = next;
                // Wraps to match the encoder, which subtracted the base the same way.
                out.push(sign_extend((base as u64).wrapping_add(delta), width));
            }
        }
        TR_DELTA => {
            let mut previous = 0_i64;
            for index in 0..n {
                let (raw, next) = get_km(buf, pos, k, m)?;
                pos = next;
                let step = decode_residual(raw, zigzag);
                previous = if index == 0 {
                    step
                } else {
                    previous.wrapping_add(step)
                };
                out.push(sign_extend(previous as u64, width));
            }
        }
        // TR_RAW, and any header the two bits cannot otherwise name.
        _ => {
            for _ in 0..n {
                let (raw, next) = get_km(buf, pos, k, m)?;
                pos = next;
                out.push(sign_extend(decode_residual(raw, zigzag) as u64, width));
            }
        }
    }
    Ok((out, pos))
}

/// Applies the header's zigzag bit to one residual.
fn decode_residual(value: u64, zigzag: bool) -> i64 {
    if zigzag {
        unzigzag(value)
    } else {
        value as i64
    }
}
