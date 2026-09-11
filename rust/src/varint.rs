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

use crate::bitstream::{BitReader, BitWriter};
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
    while more && !reader.failed() {
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

/// Maps signed to unsigned so small magnitudes stay short in both directions.
/// Used when `ALL_POSITIVE` is clear.
pub(crate) fn zigzag(value: i64) -> u64 {
    ((value << 1) ^ (value >> 63)) as u64
}

/// The encoded bit length of a payload needing `b` significant bits, with the
/// selector that achieves it. Both ladders are walked because neither dominates:
/// the nibble wins on `b` in 7..10, 14..17, 21..24 and so on, and loses by four
/// bits exactly where LEB128's first unit was already full.
const fn varint_size(b: u32) -> (u32, bool) {
    let mut plain = 8;
    let mut cap = UNIT0_BITS as u32;
    while b > cap {
        plain += 8;
        cap += UNIT_BITS as u32;
    }
    let mut nib = 12;
    let mut cap = (UNIT0_BITS + NIBBLE_BITS) as u32;
    while b > cap {
        nib += 8;
        cap += UNIT_BITS as u32;
    }
    if nib < plain {
        (nib, true)
    } else {
        (plain, false)
    }
}

/// `VARINT_SELECTOR[b]` is the ladder `varint_size` picks for a payload of `b`
/// bits. The choice depends on nothing but `b`, and `b` has 65 possible values,
/// so it is a table rather than two loops walked on every integer written.
const VARINT_SELECTOR: [bool; 65] = {
    let mut table = [false; 65];
    let mut b = 0;
    while b < 65 {
        table[b] = varint_size(b as u32).1;
        b += 1;
    }
    table
};

/// Writes `value`, choosing the smaller of the two forms. The inverse of
/// [`get_varint`].
///
/// Each unit goes out as one call: unit 0 is its selector, its continuation bit
/// and its payload assembled into a single 8- or 12-bit word, and each
/// continuation unit is one byte-wide word.
pub(crate) fn put_varint(writer: &mut BitWriter, value: u64) {
    let b = 64 - value.leading_zeros();
    let selector = VARINT_SELECTOR[b as usize];

    let mut shift = u32::from(UNIT0_BITS);
    let mut width = 1 + 1 + UNIT0_BITS;
    if selector {
        shift = u32::from(UNIT0_BITS + NIBBLE_BITS);
        width += NIBBLE_BITS;
    }
    let mut more = b > shift;

    // unit 0: [selector:1][cont:1][payload:6] (+[payload:4] if selector)
    let mut unit = u64::from(selector) | u64::from(more) << 1 | (value & mask(UNIT0_BITS)) << 2;
    if selector {
        unit |= ((value >> UNIT0_BITS) & mask(NIBBLE_BITS)) << 8;
    }
    writer.chunk(unit, width);

    while more {
        more = b > shift + u32::from(UNIT_BITS);
        // unit i: [cont:1][payload:7]
        writer.chunk(
            u64::from(more) | ((value >> shift) & mask(UNIT_BITS)) << 1,
            1 + UNIT_BITS,
        );
        shift += u32::from(UNIT_BITS);
    }
}

fn mask(width: u8) -> u64 {
    (1_u64 << width) - 1
}

// --- the columnar array codec ------------------------------------------------

const TR_RAW: u8 = 0;
const TR_DELTA: u8 = 1;
const TR_FOR: u8 = 2;
const TR_FIXED: u8 = 3;

const MIN_K: u8 = 1;
const MAX_K: u8 = 4;

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

// --- the array encoder -------------------------------------------------------
//
// A byte-exact port of `varint/array.go`'s AppendArray. The input is mapped to
// unsigned residuals by one of four transforms and the residuals are written
// with the (k, M) varint; the encoder scores every transform against every
// parameter pair and keeps the smallest, which is why this is a search rather
// than a straight port. Getting the search right is what makes a Rust-written
// integer array identical to a Go-written one.

/// How many payload bits a value occupying exactly `l` bytes can hold under
/// `(k, m)`. Requires `k <= l <= m`.
fn cap_bits(l: u8, k: u8, m: u8) -> u8 {
    if l == m {
        return 7 * m + k;
    }
    8 * (k - 1) + 7 * (l - k + 1)
}

/// The byte length a value of `nbits` bits occupies under `(k, m)`, or 0 if it
/// does not fit in `m` bytes.
fn enc_len(nbits: u8, k: u8, m: u8) -> u8 {
    for l in k..=m {
        if nbits <= cap_bits(l, k, m) {
            return l;
        }
    }
    0
}

/// Writes `value` under `(k, m)`. The caller has established that it fits, which
/// the parameter search guarantees by rejecting pairs that cannot.
fn write_km(writer: &mut BitWriter<'_>, mut value: u64, k: u8, m: u8) {
    let l = enc_len((64 - value.leading_zeros()) as u8, k, m);
    for unit in 1..=l {
        if unit < k || unit == m {
            // Flag-free: a forced continuation, or a forced terminal.
            writer.chunk(value & 0xFF, 8);
            value >>= 8;
            continue;
        }
        let mut byte = value & 0x7F;
        value >>= 7;
        if unit < l {
            byte |= 0x80;
        }
        writer.chunk(byte, 8);
    }
}

/// One candidate parameter pair plus the payload size it produces.
#[derive(Clone, Copy, Default)]
struct KmParams {
    k: u8,
    code: u8,
    m: u8,
    /// Payload bytes, excluding the header.
    size: usize,
}

/// Picks the `(k, d-code)` pair minimising total payload size for the residual
/// bit-length histogram, rejecting pairs that cannot represent the widest
/// residual.
///
/// `enc_len` is monotonic in the residual bit length, so a cumulative histogram
/// lets each candidate score the ranges assigned to its `k..M` byte lengths
/// instead of calling `enc_len` for all 65 buckets.
fn search(hist: &[u32; 65]) -> Option<KmParams> {
    let mut cumulative = [0_u32; 65];
    let mut total = 0_u32;
    for (b, count) in hist.iter().enumerate() {
        total += count;
        cumulative[b] = total;
    }

    let mut best: Option<KmParams> = None;
    for k in MIN_K..=MAX_K {
        for code in 0..8_u8 {
            let m = k + D_CODES[usize::from(code)];
            let max_bits = cap_bits(m, k, m);
            if max_bits < 64 && cumulative[usize::from(max_bits)] != total {
                continue;
            }

            let mut size = 0_usize;
            let mut previous: Option<usize> = None;
            for l in k..=m {
                let hi = usize::from(cap_bits(l, k, m)).min(64);
                let mut n = cumulative[hi];
                if let Some(previous) = previous {
                    n -= cumulative[previous];
                }
                size += usize::from(l) * n as usize;
                previous = Some(hi);
                if hi == 64 {
                    break;
                }
            }
            if best.is_none_or(|best| size < best.size) {
                best = Some(KmParams { k, code, m, size });
            }
        }
    }
    best
}

/// How many residuals a transform emits for `n` values. `TR_FOR` emits one
/// extra: the frame-of-reference base at index 0.
fn residual_count(transform: u8, n: usize) -> usize {
    if transform == TR_FOR { n + 1 } else { n }
}

/// Residual `i` for the given transform. `base` is the column minimum and is
/// used only by `TR_FOR`, whose deltas are non-negative by construction and so
/// are never zigzagged; its base at index 0 always is, since it alone may be
/// negative.
fn residual(vals: &[i64], i: usize, transform: u8, zz: bool, base: i64) -> u64 {
    match transform {
        TR_FOR => {
            if i == 0 {
                return zigzag(base);
            }
            // Wraps correctly for spans above 2^63, matching the Go encoder.
            (vals[i - 1] as u64).wrapping_sub(base as u64)
        }
        TR_DELTA => {
            if i == 0 {
                enc(vals[0], zz)
            } else {
                enc(vals[i].wrapping_sub(vals[i - 1]), zz)
            }
        }
        _ => enc(vals[i], zz),
    }
}

fn enc(value: i64, zz: bool) -> u64 {
    if zz { zigzag(value) } else { value as u64 }
}

/// Whether `a - b` overflows `i64`.
fn sub_overflows(a: i64, b: i64) -> bool {
    a.checked_sub(b).is_none()
}

/// A fully evaluated encoding candidate.
#[derive(Clone, Copy)]
struct Plan {
    transform: u8,
    zz: bool,
    base: i64,
    params: KmParams,
    /// Header plus payload.
    total: usize,
}

/// Scores one transform by histogramming its residual bit lengths. `None` when
/// the transform is unusable: `TR_DELTA` is rejected if any consecutive
/// difference overflows `i64`, which needs a span above 2^63 and so is only
/// reachable for 64-bit input.
fn evaluate(vals: &[i64], width: u8, transform: u8, zz: bool, base: i64) -> Option<Plan> {
    let mut hist = [0_u32; 65];
    let count = residual_count(transform, vals.len());
    if transform == TR_DELTA && width == 64 {
        for i in 1..vals.len() {
            if sub_overflows(vals[i], vals[i - 1]) {
                return None;
            }
        }
    }
    for i in 0..count {
        let value = residual(vals, i, transform, zz, base);
        hist[(64 - value.leading_zeros()) as usize] += 1;
    }
    let params = search(&hist)?;
    Some(Plan {
        transform,
        zz,
        base,
        params,
        total: 1 + params.size,
    })
}

/// Encodes `vals` as `width`-bit elements, straight through the bitstream.
///
/// The element width is not stored — it comes from the Go type on one side and
/// the schema on the other — so [`decode_array`] must be given the same width.
///
/// The payload is written unit by unit rather than assembled into a scratch
/// buffer and copied, so an array field costs no allocation. Only the transform
/// search needs the values twice, and it reads them without writing anything.
pub(crate) fn encode_array(writer: &mut BitWriter<'_>, vals: &[i64], width: u8) {
    let n = vals.len();
    if n == 0 {
        writer.chunk(u64::from(TR_RAW), 8); // k=1, code=0, no payload
        return;
    }

    // Candidate 1: raw values, zigzagged only if the column has negatives.
    let mut has_negative = false;
    let mut minimum = vals[0];
    for value in vals {
        if *value < 0 {
            has_negative = true;
        }
        if *value < minimum {
            minimum = *value;
        }
    }
    let mut best = evaluate(vals, width, TR_RAW, has_negative, 0);

    // Candidate 2: delta of previous. Zigzag whenever any step goes backwards.
    let mut negative_step = false;
    for i in 1..n {
        if !sub_overflows(vals[i], vals[i - 1]) && vals[i] < vals[i - 1] {
            negative_step = true;
            break;
        }
    }
    if let Some(plan) = evaluate(vals, width, TR_DELTA, negative_step, 0) {
        if best.is_none_or(|best| plan.total < best.total) {
            best = Some(plan);
        }
    }

    // Candidate 3: frame of reference against the column minimum.
    if let Some(plan) = evaluate(vals, width, TR_FOR, false, minimum) {
        if best.is_none_or(|best| plan.total < best.total) {
            best = Some(plan);
        }
    }

    // Candidate 4: uncompressed native words. Wins on full-width random data,
    // where no varint scheme can beat the element width. Always available, since
    // every value fits its own type; this is what bounds output at 1 + width*n.
    let bytes = usize::from(width / 8);
    let fixed_total = 1 + n * bytes;
    let best = match best {
        Some(best) if best.total <= fixed_total => best,
        _ => Plan {
            transform: TR_FIXED,
            zz: false,
            base: 0,
            params: KmParams {
                k: 1,
                code: 0,
                m: 1,
                size: 0,
            },
            total: fixed_total,
        },
    };

    let zz_bit = if best.zz { 1 << 2 } else { 0 };
    let header = best.transform | zz_bit | (best.params.k - 1) << 3 | best.params.code << 5;
    writer.chunk(u64::from(header), 8);
    if best.transform == TR_FIXED {
        for value in vals {
            let mut raw = *value as u64;
            for _ in 0..bytes {
                writer.chunk(raw & 0xFF, 8);
                raw >>= 8;
            }
        }
        return;
    }
    for i in 0..residual_count(best.transform, n) {
        write_km(
            writer,
            residual(vals, i, best.transform, best.zz, best.base),
            best.params.k,
            best.params.m,
        );
    }
}
