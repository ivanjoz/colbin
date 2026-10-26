//! A typed narrow message's root.
//!
//! Every other message starts with a root byte in `0xD0..=0xDF` that says what
//! it is. A struct at four key bits, with no schema section and no dictionary,
//! is what nearly every message is, and it spends nothing on one: its key run is
//! the whole message. A reader tells the two apart by the first byte — one in the
//! root range is a root, anything else the key run's first field — and an empty
//! message is the struct with every field zero.
//!
//! A key run can begin in the root range itself: a first field of key 13 has a
//! header of `0xD0..=0xDF`. That message keeps its root byte, `0xD0`, which the
//! reader steps over. Mirrors `wire/root.go`.

use alloc::vec::Vec;

/// The root byte of a typed narrow message, written only when its key run would
/// otherwise begin in the root range.
pub const ROOT_NARROW: u8 = 0xD0;

/// The last byte of the root range, which begins at [`ROOT_NARROW`].
const ROOT_LAST: u8 = 0xDF;

/// Whether `byte` is a root byte rather than a key run's first field.
pub const fn is_root(byte: u8) -> bool {
    byte >= ROOT_NARROW && byte <= ROOT_LAST
}

/// Ends a typed narrow message whose key run was written from `start` on with no
/// root byte in front: it puts the root byte back when the run begins in the root
/// range, which a reader would take for a root.
pub fn finish_narrow(buf: &mut Vec<u8>, start: usize) {
    if buf.get(start).is_some_and(|&first| is_root(first)) {
        buf.insert(start, ROOT_NARROW);
    }
}

/// The key run of a typed narrow message, whether or not it has its root byte,
/// and `None` for a message whose first byte is any other root.
pub fn narrow_body(data: &[u8]) -> Option<&[u8]> {
    match data.first() {
        None => Some(data),
        Some(&first) if !is_root(first) => Some(data),
        Some(&ROOT_NARROW) => Some(&data[1..]),
        Some(_) => None,
    }
}
