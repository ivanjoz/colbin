//! colbin's column codec, mirroring the Go `column` package: a transform, then
//! blocks of 128 residuals packed at an exact bit width chosen per block.
//!
//! ```
//! use colbin::column;
//!
//! let mut buf = Vec::new();
//! column::append_array(&mut buf, &[100_i32, 240, 250, 380]);
//! let mut out = [0_i32; 4];
//! let read = column::decode_array(&buf, 4, &mut out)?;
//! assert_eq!(out, [100, 240, 250, 380]);
//! assert_eq!(read, buf.len());
//! # Ok::<(), colbin::Error>(())
//! ```
//!
//! # Why blocks of 128
//!
//! 128 values at `w` bits occupy `128w/8 = 16w` bytes — a whole number of bytes
//! for every `w`, and a whole number of 64-bit words too. So a block is byte
//! aligned at both ends, no padding is ever wasted, and no state crosses a block
//! boundary. The width leaves the element loop, which is what keeps the read
//! fast, and the ladder is still one bit fine, which is what keeps it small.
//!
//! # Reading a value is one load
//!
//! Values are packed least-significant-bit first with no gap, so value `i`
//! starts at bit `i*w` and
//!
//! ```text
//! v = (u64::from_le_bytes(buf[pos>>3..]) >> (pos & 7)) & mask
//! ```
//!
//! holds for every `w <= 57`, with no dependency on the value before it. The
//! cost is that the read touches up to eight bytes past the value it wants;
//! [`decode_array`] takes a slower gather for the tail of a buffer rather than
//! requiring the caller to pad it.
//!
//! # Layout
//!
//! ```text
//! column := [header:1] [base: 8 bytes]? block*
//!
//! header  bits 0-1  transform: raw / delta / frame-of-reference / constant
//!         bit  2    zigzag applied to residuals: set for delta, set for raw
//!                   when the column has a negative, clear otherwise
//!         bits 3-7  reserved, zero
//!
//!         An empty column is the header 0 alone. The decoder refuses any
//!         other header the encoder does not write.
//!
//! base    the delta's first value or the frame's minimum, zigzagged, in the
//!         clear at full width
//!
//! block  := [width: 1 byte, in bits 0..64] [16 × width bytes]
//! ```
//!
//! Width 0 is a block of nothing but zeros and occupies one byte, which is what
//! makes a sparse or constant run nearly free. A constant column has no blocks
//! at all: the header is followed by the one value.
//!
//! The element count is not stored; it comes from the record count, and the
//! element *width* comes from `T` and never reaches the wire — which is why
//! [`decode_array`] must be instantiated with the type that encoded.

use crate::Error;
use alloc::vec::Vec;

/// How many residuals share one width byte. 128 is what makes every width land
/// on a byte boundary, and it puts the width header at 0.8% overhead while
/// letting a single outlier widen its own block rather than the column.
pub const BLOCK_SIZE: usize = 128;

/// The largest width the one-load read covers. A value that starts anywhere in
/// a byte and is at most 57 bits wide always finishes inside the eight bytes
/// that load spans; past it the read needs a ninth byte and takes the
/// out-of-line path.
const WIDE_WIDTH: usize = 57;

const TR_RAW: u8 = 0; // residual[i] = enc(vals[i])
const TR_DELTA: u8 = 1; // base = vals[0]; residual[i] = zigzag(vals[i+1]-vals[i])
const TR_FOR: u8 = 2; // base = min; residual[i] = vals[i] - min
const TR_CONSTANT: u8 = 3; // no blocks at all: every value is the base

const ZIGZAG_FLAG: u8 = 1 << 2;

/// What a constant column's payload occupies: the one value, in the clear. Its
/// header byte is counted separately, like every other transform's.
const CONSTANT_COST: usize = 8;

/// The element types the codec handles natively. Values are widened to `i64`
/// internally, which is free for every member.
///
/// `isize` is deliberately absent. Its width is platform-dependent, so a column
/// encoded on a 64-bit host would not decode on a 32-bit one; the width is
/// derived from the type rather than stored, so that mismatch would be silent.
pub trait Signed: Copy {
    /// The element width in bytes, which encoder and decoder derive from the
    /// same type and never put on the wire.
    const WIDTH: usize;
    /// Widened to `i64`, which every member does exactly.
    fn as_i64(self) -> i64;
    /// Narrowed back, truncating — the inverse of the widening above for every
    /// value the encoder saw.
    fn from_i64(value: i64) -> Self;
}

macro_rules! impl_signed {
    ($($type:ty),*) => {$(
        impl Signed for $type {
            const WIDTH: usize = size_of::<$type>();
            #[inline]
            fn as_i64(self) -> i64 { i64::from(self) }
            #[inline]
            #[allow(clippy::cast_possible_truncation)]
            fn from_i64(value: i64) -> Self { value as Self }
        }
    )*};
}

impl_signed!(i8, i16, i32);

impl Signed for i64 {
    const WIDTH: usize = 8;
    #[inline]
    fn as_i64(self) -> i64 {
        self
    }
    #[inline]
    fn from_i64(value: i64) -> Self {
        value
    }
}

/// Maps signed to unsigned so that small magnitudes stay short:
/// `0,-1,1,-2,2 -> 0,1,2,3,4`.
#[inline]
#[allow(clippy::cast_sign_loss)]
pub(crate) const fn zigzag(value: i64) -> u64 {
    ((value << 1) ^ (value >> 63)) as u64
}

/// Inverts [`zigzag`].
#[inline]
#[allow(clippy::cast_possible_wrap)]
pub(crate) const fn unzigzag(value: u64) -> i64 {
    ((value >> 1) as i64) ^ -((value & 1) as i64)
}

/// What a run of `count` residuals occupies at `w` bits.
const fn block_bytes(count: usize, w: usize) -> usize {
    (count * w).div_ceil(8)
}

/// The width a block of residuals needs: the widest one's bit length, with no
/// rounding. OR-ing them together and taking one bit length is the same answer
/// as a running maximum, without the compare.
fn width_of(block: &[u64]) -> usize {
    let mut set = 0_u64;
    for residual in block {
        set |= residual;
    }
    (64 - set.leading_zeros()) as usize
}

/// Writes `block.len()` residuals at `w` bits each, least significant bit
/// first, with no gap between them and none at the end beyond the final partial
/// byte.
///
/// A full block is `2w` whole 64-bit words, so the accumulator flushes exactly
/// and only the last block of a column can end mid-word.
fn pack_run(out: &mut Vec<u8>, block: &[u64], w: usize) {
    if w == 0 {
        return;
    }
    let mut accumulator = 0_u64;
    let mut accumulated = 0_usize;
    for &residual in block {
        if accumulated + w < 64 {
            accumulator |= residual << accumulated;
            accumulated += w;
            continue;
        }
        // This residual straddles the word boundary, or lands exactly on it.
        let low = 64 - accumulated;
        accumulator |= residual << accumulated;
        out.extend_from_slice(&accumulator.to_le_bytes());
        accumulator = 0;
        accumulated = 0;
        if low < w {
            accumulator = residual >> low;
            accumulated = w - low;
        }
    }
    while accumulated > 0 {
        #[allow(clippy::cast_possible_truncation)]
        out.push(accumulator as u8);
        accumulator >>= 8;
        accumulated = accumulated.saturating_sub(8);
    }
}

/// Reads `dst.len()` residuals at `w` bits each. It is the read the whole
/// layout is for: one load, one shift and one mask per value, with the width
/// hoisted out of the loop and no carry between iterations.
fn unpack_run(buf: &[u8], dst: &mut [u64], w: usize) {
    if w == 0 {
        dst.fill(0);
        return;
    }
    if w > WIDE_WIDTH {
        unpack_wide(buf, dst, w);
        return;
    }
    let mask = (1_u64 << w) - 1;
    if buf.len() < block_bytes(dst.len(), w) + 8 {
        unpack_tail(buf, dst, w, mask);
        return;
    }
    let mut position = 0;
    for slot in dst.iter_mut() {
        let at = position >> 3;
        let window: [u8; 8] = buf[at..at + 8].try_into().expect("eight bytes");
        *slot = (u64::from_le_bytes(window) >> (position & 7)) & mask;
        position += w;
    }
}

/// [`unpack_run`] where the eight-byte load could read past the buffer, which
/// is only ever the last block of a column. It gathers each value from the
/// bytes that are actually there.
fn unpack_tail(buf: &[u8], dst: &mut [u64], w: usize, mask: u64) {
    let mut position = 0;
    for slot in dst.iter_mut() {
        // w is at most 57 here, so the value ends within eight bytes of where it
        // starts and gathering those eight is enough.
        *slot = (gather8(buf, position >> 3) >> (position & 7)) & mask;
        position += w;
    }
}

/// Covers `w` in 58..=64, where one eight-byte load can no longer be guaranteed
/// to hold a whole value: starting at bit 7 of a byte, a 64-bit value reaches
/// into a ninth. A column that reaches these widths is incompressible anyway.
fn unpack_wide(buf: &[u8], dst: &mut [u64], w: usize) {
    // The mask is built in two shifts so that w == 64 does not shift by 64,
    // which is undefined for a u64.
    let mask = ((1_u64 << (w - 1)) << 1).wrapping_sub(1);
    let mut position = 0;
    for slot in dst.iter_mut() {
        let at = position >> 3;
        let shift = position & 7;
        let mut value = gather8(buf, at) >> shift;
        if shift != 0 && at + 8 < buf.len() {
            value |= u64::from(buf[at + 8]) << (64 - shift);
        }
        *slot = value & mask;
        position += w;
    }
}

/// Reads eight little-endian bytes from `at`, stopping at the end of the buffer
/// rather than past it.
fn gather8(buf: &[u8], at: usize) -> u64 {
    if at + 8 <= buf.len() {
        let window: [u8; 8] = buf[at..at + 8].try_into().expect("eight bytes");
        return u64::from_le_bytes(window);
    }
    let mut value = 0_u64;
    for index in 0..8 {
        match buf.get(at + index) {
            Some(byte) => value |= u64::from(*byte) << (8 * index),
            None => break,
        }
    }
    value
}

/// Reports whether `a - b` overflows an `i64`.
const fn sub_overflows(a: i64, b: i64) -> bool {
    let difference = a.wrapping_sub(b);
    ((a ^ b) & (a ^ difference)) < 0
}

/// How many residuals a transform emits for `n` values. Delta writes its first
/// value in the clear as a base, so it has one fewer.
const fn residuals_under(transform: u8, n: usize) -> usize {
    if transform == TR_DELTA { n - 1 } else { n }
}

/// Applies the header's zigzag bit.
#[inline]
#[allow(clippy::cast_sign_loss)]
fn encode(x: i64, zz: bool) -> u64 {
    if zz { zigzag(x) } else { x as u64 }
}

/// Inverts [`encode`].
#[inline]
#[allow(clippy::cast_possible_wrap)]
fn decode(value: u64, zz: bool) -> i64 {
    if zz { unzigzag(value) } else { value as i64 }
}

/// The payload size a transform would produce, block widths and all — which is
/// what the transform has to be scored against. Scoring it by the column's
/// widest residual instead picks frame-of-reference for a column of small ids
/// and loses 76%, where scoring it this way picks delta and loses 14%.
///
/// One pass over the data per candidate, with the residual inlined into the
/// loop and the block's width taken by OR-ing its residuals together, so there
/// is not even a compare inside.
#[allow(clippy::cast_sign_loss)]
fn blocked_cost<T: Signed>(vals: &[T], transform: u8, base: i64, zz: bool) -> usize {
    let n = residuals_under(transform, vals.len());
    let mut total = 0;
    if transform != TR_RAW {
        total += 8; // the base, in the clear
    }
    let mut start = 0;
    while start < n {
        let end = (start + BLOCK_SIZE).min(n);
        let mut set = 0_u64;
        match transform {
            TR_FOR => {
                for value in &vals[start..end] {
                    set |= (value.as_i64() as u64).wrapping_sub(base as u64);
                }
            }
            TR_DELTA => {
                for index in start..end {
                    set |= zigzag(vals[index + 1].as_i64().wrapping_sub(vals[index].as_i64()));
                }
            }
            _ => {
                for value in &vals[start..end] {
                    set |= encode(value.as_i64(), zz);
                }
            }
        }
        total += 1 + block_bytes(end - start, (64 - set.leading_zeros()) as usize);
        start = end;
    }
    total
}

/// Encodes `vals` onto `out`.
///
/// The element width comes from `T` and is not stored, so [`decode_array`] must
/// be instantiated with the same type.
#[allow(clippy::cast_sign_loss)]
pub fn append_array<T: Signed>(out: &mut Vec<u8>, vals: &[T]) {
    if vals.is_empty() {
        out.push(TR_RAW);
        return;
    }

    // One pass for everything the transforms need to be scored: the minimum for
    // the frame of reference, whether anything is negative, whether every value
    // is the same, and whether a delta could overflow.
    let first = vals[0].as_i64();
    let mut minimum = first;
    let mut constant = true;
    let mut delta_overflows = false;
    for (index, value) in vals.iter().enumerate() {
        let x = value.as_i64();
        if x < minimum {
            minimum = x;
        }
        if x != first {
            constant = false;
        }
        // A delta can only overflow an i64 for i64 input, and only across a span
        // above 2^63.
        if index > 0 && T::WIDTH == 8 && sub_overflows(x, vals[index - 1].as_i64()) {
            delta_overflows = true;
        }
    }
    // Raw is zigzagged whenever the column has a negative, which is what keeps
    // -1 from becoming a 64-bit residual.
    let zz = minimum < 0;
    let mut transform = TR_RAW;
    let mut zigzagged = zz;
    let mut best = blocked_cost(vals, TR_RAW, 0, zz);
    let cost = blocked_cost(vals, TR_FOR, minimum, false);
    if cost < best {
        transform = TR_FOR;
        zigzagged = false;
        best = cost;
    }
    if !delta_overflows && vals.len() > 1 {
        let cost = blocked_cost(vals, TR_DELTA, 0, true);
        if cost < best {
            transform = TR_DELTA;
            zigzagged = true;
            best = cost;
        }
    }
    // Constant is scored rather than short-circuited. It is unbeatable on a long
    // column and beaten on a short one: three small values are three bytes raw
    // against the eight a base costs, so taking it on sight would make the
    // smallest columns the most expensive.
    if constant && CONSTANT_COST < best {
        out.push(TR_CONSTANT);
        out.extend_from_slice(&zigzag(minimum).to_le_bytes());
        return;
    }

    let mut header = transform;
    if zigzagged {
        header |= ZIGZAG_FLAG;
    }
    out.push(header);
    match transform {
        TR_FOR => out.extend_from_slice(&zigzag(minimum).to_le_bytes()),
        TR_DELTA => out.extend_from_slice(&zigzag(first).to_le_bytes()),
        _ => {}
    }
    append_blocks(out, vals, transform, minimum, zigzagged);
}

/// Materialises each block's residuals into a scratch array on the stack and
/// packs it. 128 `u64` is a kilobyte, which is worth not allocating and worth
/// not walking the input twice for.
#[allow(clippy::cast_sign_loss)]
fn append_blocks<T: Signed>(out: &mut Vec<u8>, vals: &[T], transform: u8, base: i64, zz: bool) {
    let mut scratch = [0_u64; BLOCK_SIZE];
    let n = residuals_under(transform, vals.len());
    let mut start = 0;
    while start < n {
        let end = (start + BLOCK_SIZE).min(n);
        let block = &mut scratch[..end - start];
        match transform {
            TR_FOR => {
                for (index, slot) in block.iter_mut().enumerate() {
                    *slot = (vals[start + index].as_i64() as u64).wrapping_sub(base as u64);
                }
            }
            TR_DELTA => {
                for (index, slot) in block.iter_mut().enumerate() {
                    let at = start + index;
                    *slot = zigzag(vals[at + 1].as_i64().wrapping_sub(vals[at].as_i64()));
                }
            }
            _ => {
                for (index, slot) in block.iter_mut().enumerate() {
                    *slot = encode(vals[start + index].as_i64(), zz);
                }
            }
        }
        let w = width_of(block);
        #[allow(clippy::cast_possible_truncation)]
        out.push(w as u8);
        pack_run(out, block, w);
        start = end;
    }
}

/// Reports whether [`append_array`] writes `header` for a column of `n`
/// values: raw with or without zigzag, delta with it, frame of reference and
/// constant without, and raw alone for an empty column. Everything else —
/// a reserved bit, a zigzag the transform never takes — is refused, as the Go
/// decoder refuses it.
fn header_written(header: u8, n: usize) -> bool {
    if n == 0 {
        return header == TR_RAW;
    }
    matches!(header, TR_RAW | TR_FOR | TR_CONSTANT)
        || header == TR_RAW | ZIGZAG_FLAG
        || header == TR_DELTA | ZIGZAG_FLAG
}

/// Reads `n` values written by [`append_array`] into `out` and returns the
/// number of bytes consumed from `buf`.
///
/// It must be instantiated with the element type used to encode: the width is
/// derived from `T` rather than stored.
#[allow(clippy::cast_sign_loss)]
pub fn decode_array<T: Signed>(buf: &[u8], n: usize, out: &mut [T]) -> Result<usize, Error> {
    if out.len() < n {
        return Err(Error::ShortBuffer);
    }
    let header = *buf.first().ok_or(Error::Truncated)?;
    let mut position = 1;
    if header == HEADER_DECIMAL && n > 0 {
        return decode_decimal(buf, n, out);
    }
    if !header_written(header, n) {
        return Err(Error::BadColumnHeader);
    }
    let transform = header & 0x03;
    let zz = header & ZIGZAG_FLAG != 0;
    if n == 0 {
        return Ok(position);
    }

    let mut base = 0_i64;
    if transform != TR_RAW {
        let bytes = buf
            .get(position..position + 8)
            .ok_or(Error::Truncated)?
            .try_into()
            .expect("eight bytes");
        base = unzigzag(u64::from_le_bytes(bytes));
        position += 8;
    }
    if transform == TR_CONSTANT {
        out[..n].fill(T::from_i64(base));
        return Ok(position);
    }

    // Delta's first value is the base itself; the blocks carry the steps.
    let mut offset = 0;
    if transform == TR_DELTA {
        out[0] = T::from_i64(base);
        offset = 1;
    }
    let target = &mut out[offset..n];

    // Delta accumulates in an i64 rather than in out[i], because a step can
    // exceed T's range even when every value fits it: for i8, 127-(-128) is 255.
    // Storing the step first and summing afterwards would truncate it.
    let mut accumulator = base;

    let mut scratch = [0_u64; BLOCK_SIZE];
    let mut start = 0;
    while start < target.len() {
        let end = (start + BLOCK_SIZE).min(target.len());
        let w = usize::from(*buf.get(position).ok_or(Error::Truncated)?);
        position += 1;
        if w > 64 {
            return Err(Error::BadWidth);
        }
        let bytes = block_bytes(end - start, w);
        if buf.len() - position < bytes {
            return Err(Error::Truncated);
        }
        let block = &mut scratch[..end - start];
        unpack_run(&buf[position..], block, w);
        position += bytes;
        match transform {
            TR_FOR => {
                for (index, residual) in block.iter().enumerate() {
                    target[start + index] =
                        T::from_i64((base as u64).wrapping_add(*residual) as i64);
                }
            }
            TR_DELTA => {
                for (index, residual) in block.iter().enumerate() {
                    accumulator = accumulator.wrapping_add(unzigzag(*residual));
                    target[start + index] = T::from_i64(accumulator);
                }
            }
            _ => {
                for (index, residual) in block.iter().enumerate() {
                    target[start + index] = T::from_i64(decode(*residual, zz));
                }
            }
        }
        start = end;
    }
    Ok(position)
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Every width, at every partial-block length, read back both with slack
    /// after the run and with the buffer ending exactly at it — so the one-load
    /// path and the gather path must agree.
    #[test]
    fn pack_run_round_trips_at_every_width() {
        for w in 0..=64_usize {
            for count in [1, 2, 7, 63, 64, 65, 127, 128] {
                let mask = if w == 0 {
                    0
                } else {
                    ((1_u64 << (w - 1)) << 1).wrapping_sub(1)
                };
                let block: Vec<u64> = (0..count)
                    .map(|index| (0x9E37_79B9_7F4A_7C15_u64.wrapping_mul(index as u64 + 1)) & mask)
                    .collect();
                let mut packed = Vec::new();
                pack_run(&mut packed, &block, w);
                assert_eq!(
                    packed.len(),
                    block_bytes(count, w),
                    "width {w}, count {count}"
                );

                let mut exact = alloc::vec![0_u64; count];
                unpack_run(&packed, &mut exact, w);
                assert_eq!(exact, block, "exact buffer, width {w}, count {count}");

                let mut slack = packed.clone();
                slack.extend_from_slice(&[0_u8; 8]);
                let mut loaded = alloc::vec![0_u64; count];
                unpack_run(&slack, &mut loaded, w);
                assert_eq!(loaded, block, "slack buffer, width {w}, count {count}");
            }
        }
    }

    /// The arithmetic the whole layout rests on.
    #[test]
    fn a_full_block_is_a_whole_number_of_bytes() {
        for w in 0..=64 {
            assert_eq!(BLOCK_SIZE * w % 8, 0);
            assert_eq!(block_bytes(BLOCK_SIZE, w), 16 * w);
        }
    }

    fn round_trip<T: Signed + PartialEq + core::fmt::Debug>(vals: &[T]) -> usize {
        let mut buf = Vec::new();
        append_array(&mut buf, vals);
        let mut out = alloc::vec![T::from_i64(0); vals.len()];
        let read = decode_array(&buf, vals.len(), &mut out).expect("decode");
        assert_eq!(out, vals);
        assert_eq!(read, buf.len(), "the decode consumed the whole column");
        buf.len()
    }

    #[test]
    fn round_trips_every_shape() {
        round_trip::<i64>(&[]);
        round_trip(&[0_i64]);
        round_trip(&[i64::MIN, i64::MAX, 0, -1, 1]);
        round_trip(&(0..1000_i64).collect::<Vec<_>>()); // delta
        round_trip(&[7_i64; 500]); // constant
        round_trip(&alloc::vec![0_i64; 256]); // width 0
        round_trip(
            &(0..300)
                .map(|i| 1_700_000_000_000 + i * 37)
                .collect::<Vec<i64>>(),
        );
        round_trip(&[i8::MIN, i8::MAX, 0, -1]);
        round_trip(&[i16::MIN, i16::MAX, 0, -1]);
        round_trip(&[i32::MIN, i32::MAX, 0, -1]);
    }

    /// No other transform may produce a shorter encoding than the one chosen.
    #[test]
    fn the_transform_choice_is_optimal() {
        let shapes: Vec<Vec<i64>> = alloc::vec![
            (1..900).collect(),
            (0..500).map(|i| 1_700_000_000 + i).collect(),
            (0..500).map(|i| (i * 7919) % 50000).collect(),
            alloc::vec![42; 300],
            alloc::vec![0; 300],
            (0..300).map(|i| -i).collect(),
        ];
        for shape in shapes {
            let mut chosen = Vec::new();
            append_array(&mut chosen, &shape);
            let minimum = *shape.iter().min().expect("non-empty");
            for (transform, base, zz) in [
                (TR_RAW, 0, false),
                (TR_RAW, 0, true),
                (TR_FOR, minimum, false),
                (TR_DELTA, 0, true),
            ] {
                if transform == TR_DELTA && shape.len() < 2 {
                    continue;
                }
                let candidate = 1 + blocked_cost(&shape, transform, base, zz);
                assert!(
                    chosen.len() <= candidate,
                    "chose {} bytes where transform {transform} would take {candidate}",
                    chosen.len()
                );
            }
        }
    }

    /// The +0.1% bound: one byte per block over the raw element width.
    #[test]
    fn never_exceeds_the_element_width_by_more_than_a_block_header() {
        let random: Vec<i64> = (0..1024)
            .map(|index: u64| {
                (0x2545_F491_4F6C_DD1D_u64.wrapping_mul(index.wrapping_add(1)) >> 1) as i64
            })
            .collect();
        let mut buf = Vec::new();
        append_array(&mut buf, &random);
        let blocks = random.len().div_ceil(BLOCK_SIZE);
        assert!(buf.len() <= random.len() * 8 + blocks + 9);
    }

    /// Arbitrary and truncated input must be refused rather than panicked on.
    #[test]
    fn garbage_never_panics() {
        let mut buf = Vec::new();
        append_array(&mut buf, &(0..400_i64).collect::<Vec<_>>());
        for cut in 0..buf.len() {
            let mut out = alloc::vec![0_i64; 400];
            let _ = decode_array(&buf[..cut], 400, &mut out);
        }
        for byte in 0..=255_u8 {
            let mut out = alloc::vec![0_i64; 8];
            let _ = decode_array(&[byte; 12], 8, &mut out);
        }
    }
}

// --- decimal columns --------------------------------------------------------
//
// A float64 column whose values are decimals, as nearly every double that came
// out of a CSV, a form or a database is. Mirrors `column/float.go`:
//
//     [0x08] [e] [exceptions:u32] [integers] ( [positions] [bits] )
//
// `integers` is an ordinary column of n values, each in [−2^51, 2^51), with
// v = float64(i) / 10^e exactly; `positions` and `bits`, present when there are
// exceptions, are ordinary columns of the rows that are not, and their bit
// patterns. An exception's own integer is the one before it.

/// The header byte of a decimal column.
const HEADER_DECIMAL: u8 = 0x08;

/// The largest exponent a decimal column takes: 10^18 is the largest power of ten
/// an `i64` holds.
const MAX_DECIMAL_EXPONENT: usize = 18;

/// The divisors. Every one is exact in an `f64`.
const POWERS_OF_10: [f64; MAX_DECIMAL_EXPONENT + 1] = [
    1e0, 1e1, 1e2, 1e3, 1e4, 1e5, 1e6, 1e7, 1e8, 1e9, 1e10, 1e11, 1e12, 1e13, 1e14, 1e15, 1e16,
    1e17, 1e18,
];

/// 2^52 + 2^51. Added to an `f64` with |s| < 2^51 it lands in [2^52, 2^53), where
/// the step is exactly one, so the sum is `s` rounded to the nearest integer.
const DECIMAL_MAGIC: f64 = (1_u64 << 52 | 1 << 51) as f64;

/// A decimal column's integers each lie in [−DECIMAL_SPAN, DECIMAL_SPAN).
const DECIMAL_SPAN: i64 = 1 << 51;

/// How many values the exponent is chosen from.
const DECIMAL_SAMPLE: usize = 32;

/// The longest column the scalar scoring treats as one short block.
const SHORT_COLUMN: usize = 32;

/// The integer of the bit pattern `bits` at exponent `e`, and whether it is one:
/// whether `float64(i) / 10^e` gives back the value bit for bit, which rules out
/// −0, NaN and the infinities as well as the decimals `e` does not reach.
#[allow(clippy::cast_possible_wrap, clippy::cast_sign_loss)]
fn decimal_of(bits: i64, e: usize) -> (i64, bool) {
    let power = POWERS_OF_10[e];
    let product = f64::from_bits(bits as u64) * power;
    let sum = product + DECIMAL_MAGIC;
    let i = (sum.to_bits() as i64).wrapping_sub(DECIMAL_MAGIC.to_bits() as i64);
    let in_span = (i.wrapping_add(DECIMAL_SPAN) as u64) < 2 * DECIMAL_SPAN as u64;
    (
        i,
        in_span && ((sum - DECIMAL_MAGIC) / power).to_bits() as i64 == bits,
    )
}

/// The smallest exponent that reaches as many of a sample of the values as any
/// does, and how many of the sample that is.
fn choose_exponent(vals: &[i64]) -> (usize, usize, usize) {
    let step = (vals.len() / DECIMAL_SAMPLE).max(1);
    let (mut exponent, mut reached, mut sampled) = (0, 0, 0);
    for e in 0..=MAX_DECIMAL_EXPONENT {
        let (mut count, mut seen) = (0, 0);
        let mut index = 0;
        while index < vals.len() && seen < DECIMAL_SAMPLE {
            if decimal_of(vals[index], e).1 {
                count += 1;
            }
            seen += 1;
            index += step;
        }
        sampled = seen;
        if count > reached {
            exponent = e;
            reached = count;
        }
        if reached == seen {
            break;
        }
    }
    (exponent, reached, sampled)
}

/// Whether the sample says a decimal column could be smaller than the bits.
#[allow(clippy::cast_sign_loss)]
fn worth_converting(vals: &[i64], exponent: usize, reached: usize, sampled: usize) -> bool {
    if reached * 2 < sampled {
        return false;
    }
    let step = (vals.len() / DECIMAL_SAMPLE).max(1);
    let (mut low_int, mut high_int) = (i64::MAX, i64::MIN);
    let (mut low_bits, mut high_bits) = (i64::MAX, i64::MIN);
    let (mut index, mut seen) = (0, 0);
    while index < vals.len() && seen < DECIMAL_SAMPLE {
        low_bits = low_bits.min(vals[index]);
        high_bits = high_bits.max(vals[index]);
        let (i, ok) = decimal_of(vals[index], exponent);
        if ok {
            low_int = low_int.min(i);
            high_int = high_int.max(i);
        }
        index += step;
        seen += 1;
    }
    // In bits per value: a frame of reference over each, and an exception at 64
    // bits and a position's dozen or so.
    let int_bits = bit_len(high_int.wrapping_sub(low_int) as u64);
    let float_bits = bit_len(high_bits.wrapping_sub(low_bits) as u64);
    let misses = sampled - reached;
    int_bits * sampled + 76 * misses <= float_bits * sampled
}

const fn bit_len(value: u64) -> usize {
    (64 - value.leading_zeros()) as usize
}

/// `len(append_array(vals))`: what a caller choosing between two encodings of a
/// column compares.
fn array_cost(vals: &[i64]) -> usize {
    let mut scratch = Vec::new();
    append_array(&mut scratch, vals);
    scratch.len()
}

/// [`append_array`] for a column of `f64` bit patterns: a decimal column when
/// that is smaller, and `append_array`'s own bytes otherwise, so a float column
/// never costs more than it did before decimal columns.
#[allow(clippy::cast_possible_truncation)]
pub fn append_float64(out: &mut Vec<u8>, vals: &[i64]) {
    if vals.is_empty() {
        return append_array(out, vals);
    }
    let (exponent, reached, sampled) = choose_exponent(vals);
    if !worth_converting(vals, exponent, reached, sampled) {
        return append_array(out, vals);
    }
    let mut ints = alloc::vec![0_i64; vals.len()];
    let mut positions: Vec<i64> = Vec::new();
    let mut bits: Vec<i64> = Vec::new();
    let mut previous = 0_i64;
    for (index, &value) in vals.iter().enumerate() {
        let (i, ok) = decimal_of(value, exponent);
        if ok {
            ints[index] = i;
            previous = i;
        } else {
            ints[index] = previous;
            positions.push(index as i64);
            bits.push(value);
        }
    }

    // Written whichever way is smaller, scored as the encoder would write it —
    // except for a long column with no exceptions, whose integers no transform
    // can make cost more than its bits unless the bits are a constant.
    if positions.is_empty() && vals.len() > SHORT_COLUMN {
        if ints.iter().all(|&value| value == ints[0]) {
            return append_array(out, vals);
        }
    } else {
        let mut decimal = 6 + array_cost(&ints);
        if !positions.is_empty() {
            decimal += array_cost(&positions) + array_cost(&bits);
        }
        if decimal >= array_cost(vals) {
            return append_array(out, vals);
        }
    }
    out.push(HEADER_DECIMAL);
    out.push(exponent as u8);
    out.extend_from_slice(&(positions.len() as u32).to_le_bytes());
    append_array(out, &ints);
    if !positions.is_empty() {
        append_array(out, &positions);
        append_array(out, &bits);
    }
}

/// Reads the decimal column of `n` values that `buf` opens with into `out`, as
/// their bit patterns, and returns the bytes it used.
#[allow(clippy::cast_sign_loss, clippy::cast_possible_wrap)]
fn decode_decimal<T: Signed>(buf: &[u8], n: usize, out: &mut [T]) -> Result<usize, Error> {
    if buf.len() < 6 {
        return Err(Error::Truncated);
    }
    let exponent = usize::from(buf[1]);
    let exceptions = u32::from_le_bytes([buf[2], buf[3], buf[4], buf[5]]) as usize;
    if exponent > MAX_DECIMAL_EXPONENT || exceptions > n {
        return Err(Error::BadColumnHeader);
    }
    let mut position = 6;
    // Decoded as int64, then narrowed when the column is a narrower type, as
    // every column is.
    let mut target = alloc::vec![0_i64; n];
    position += decode_inner(&buf[position..], n, &mut target)?;

    let power = POWERS_OF_10[exponent];
    let mut outside = 0_u64;
    for value in &mut target {
        outside |= value.wrapping_add(DECIMAL_SPAN) as u64;
        *value = ((*value as f64) / power).to_bits() as i64;
    }
    if bit_len(outside) > 52 {
        return Err(Error::ColumnOutOfRange);
    }
    if exceptions > 0 {
        let mut positions = alloc::vec![0_i64; exceptions];
        let mut bits = alloc::vec![0_i64; exceptions];
        position += decode_inner(&buf[position..], exceptions, &mut positions)?;
        position += decode_inner(&buf[position..], exceptions, &mut bits)?;
        let mut last = -1_i64;
        for (index, &at) in positions.iter().enumerate() {
            // In order, each once, and inside the column.
            if at <= last || at >= n as i64 {
                return Err(Error::BadColumnHeader);
            }
            target[at as usize] = bits[index];
            last = at;
        }
    }
    for (slot, &value) in out.iter_mut().zip(&target) {
        let narrowed = T::from_i64(value);
        if narrowed.as_i64() != value {
            return Err(Error::ColumnOutOfRange);
        }
        *slot = narrowed;
    }
    Ok(position)
}

/// [`decode_array`] for a column inside a decimal one, which is never decimal
/// itself: one could nest another as deep as the message is long.
fn decode_inner(buf: &[u8], n: usize, out: &mut [i64]) -> Result<usize, Error> {
    if buf.first() == Some(&HEADER_DECIMAL) {
        return Err(Error::BadColumnHeader);
    }
    decode_array(buf, n, out)
}
