//! The public ABI: UTF-8 bytes in, UTF-8 bytes out.
//!
//! Bytes cross the boundary and nothing else. The host never hands over a
//! JavaScript object graph and never receives one, because `JSON.parse` would
//! have already rounded any integer past 2^53 before the codec saw it — which is
//! the failure colbin exists to prevent, and it would be a poor place to
//! reintroduce it.
//!
//! ```text
//! alloc(n)              -> a pointer to write n bytes of input at
//! encode(len, flags)    -> JSON text in, a colbin message out
//! section()             -> the schema the last encode resolved
//! set_schema(len)       -> parse the section in the input buffer and hold it
//! decode(len)           -> the message in the input buffer, as JSON text
//! materialize(len)      -> the message's table, as a flat typed buffer; 0 when
//!                           the root is not that shape (fall back to decode)
//! inspect_message(len)  -> the field tree, with a byte span on every node
//!                           (feature `inspect`, off by default)
//! result_ptr()          -> where the last successful call left its output
//! last_error()          -> the last diagnostic as JSON, at result_ptr
//! ```
//!
//! Which of those exist depends on the features the module was built with, and
//! the host is expected to look rather than assume: `encode`/`materialize` are
//! on by default and `inspect` is not, so a caller that wants the field tree
//! asks for a build that has it.
//!
//! # Nothing here traps
//!
//! A trap reaches the host as a `RuntimeError` carrying no path and no offset,
//! which is exactly what a caller cannot act on. So every failure returns a
//! negative length and leaves a message for `last_error` to hand back. The crate
//! forbids `unsafe`, and the library it calls has no `unwrap`, `expect` or
//! `panic!` in it, so the ways to trap are few by construction rather than by
//! inspection.

// `deny` rather than `forbid`, with one scoped exception below.
//
// An exported symbol is `unsafe(no_mangle)` in edition 2024 because an
// unmangled name can collide at link time, and that is a real hazard — but it
// is the *only* unsafety here, and it is the attributes on the exported
// functions with no pointer arithmetic behind any of them. Everything else,
// including the whole of the `colbin` crate this calls, stays `forbid(unsafe_code)`.
#![deny(unsafe_code)]

use std::cell::RefCell;

#[cfg(feature = "encode")]
use colbin::build;
#[cfg(feature = "encode")]
use colbin::diag::Diag;
use colbin::section::{self, Schema};
use colbin::walk;

/// The root bytes this version writes. `0xD0` narrow, `0xD8` wide, and the two
/// with bit 2 set carry their own schema section in front of the body.
const ROOT_STRUCT_NARROW: u8 = 0xd0;
const ROOT_WIDE_BIT: u8 = 0x08;
const ROOT_SCHEMA_BIT: u8 = 0x04;
const ROOT_RANGE_MASK: u8 = 0xf0;
const ROOT_ASSIGNED: u8 = ROOT_WIDE_BIT | ROOT_SCHEMA_BIT;

thread_local! {
    /// Held across calls, because the host writes the input in one call and
    /// names its length in the next.
    static INPUT: RefCell<Vec<u8>> = const { RefCell::new(Vec::new()) };
    static RESULT: RefCell<Vec<u8>> = const { RefCell::new(Vec::new()) };
    static SCHEMA: RefCell<Option<Schema>> = const { RefCell::new(None) };
    static LAST_DIAG: RefCell<Vec<u8>> = const { RefCell::new(Vec::new()) };
}

#[cfg(feature = "encode")]
thread_local! {
    static LAST_SECTION: RefCell<Vec<u8>> = const { RefCell::new(Vec::new()) };
}

/// Reserves `n` bytes for the caller to write the input into.
#[allow(unsafe_code, reason = "an exported symbol must not be mangled")]
#[unsafe(no_mangle)]
pub extern "C" fn alloc(n: usize) -> *const u8 {
    INPUT.with_borrow_mut(|input| {
        input.clear();
        input.resize(n, 0);
        input.as_ptr()
    })
}

/// Where the last successful call left its output.
#[allow(unsafe_code, reason = "an exported symbol must not be mangled")]
#[unsafe(no_mangle)]
pub extern "C" fn result_ptr() -> *const u8 {
    RESULT.with_borrow(|result| result.as_ptr())
}

/// JSON text to a colbin message.
///
/// Returns the message length, or -1 with a diagnostic in `last_error`. The
/// section for it is at `section()`; with `SELF_DESCRIBING` it is in front of
/// the body as well, and the root byte says so.
#[cfg(feature = "encode")]
#[allow(unsafe_code, reason = "an exported symbol must not be mangled")]
#[unsafe(no_mangle)]
pub extern "C" fn encode(len: usize, flags: u32) -> i32 {
    let mut diag = Diag::new();
    // Encoded out of the input buffer rather than out of a copy of it. The
    // document is the largest thing this module is ever handed, `build::encode`
    // only reads it, and nothing it calls reaches back through the ABI to touch
    // `INPUT` — which is the same reason `decode` below works inside the borrow.
    let built = INPUT.with_borrow(|input| {
        input
            .get(..len)
            .map(|text| build::encode(text, flags, &mut diag))
    });
    let Some(built) = built else {
        return fail_err(&colbin::Error::Truncated);
    };
    match built {
        Some(encoded) => {
            LAST_SECTION.with_borrow_mut(|held| *held = encoded.section);
            remember_diag(&diag.encode());
            ok(encoded.message)
        }
        None => fail_bytes(diag.encode()),
    }
}

/// The schema section for the last encode, to send once per connection.
#[cfg(feature = "encode")]
#[allow(unsafe_code, reason = "an exported symbol must not be mangled")]
#[unsafe(no_mangle)]
pub extern "C" fn section() -> i32 {
    let bytes = LAST_SECTION.with_borrow(Vec::clone);
    ok(bytes)
}

/// Parses a schema section from the input buffer and holds it for the decodes
/// that follow. A length of zero clears it.
///
/// Returns 0, or -1 with a diagnostic at `last_error`.
#[allow(unsafe_code, reason = "an exported symbol must not be mangled")]
#[unsafe(no_mangle)]
pub extern "C" fn set_schema(len: usize) -> i32 {
    if len == 0 {
        SCHEMA.with_borrow_mut(|held| *held = None);
        return 0;
    }
    let parsed = INPUT.with_borrow(|input| match input.get(..len) {
        None => Err(colbin::Error::Truncated),
        Some(bytes) => section::parse(bytes),
    });
    match parsed {
        Ok(schema) => {
            SCHEMA.with_borrow_mut(|held| *held = Some(schema));
            0
        }
        Err(err) => fail_err(&err),
    }
}

/// The message in the input buffer, as JSON text.
///
/// Returns the length, or -1 with a diagnostic at `last_error`. Uses the held
/// schema, or — when byte 0 says so — the message's own.
#[allow(unsafe_code, reason = "an exported symbol must not be mangled")]
#[unsafe(no_mangle)]
pub extern "C" fn decode(len: usize) -> i32 {
    let outcome = INPUT.with_borrow(|input| {
        let message = input.get(..len).ok_or(colbin::Error::Truncated)?;
        let (root, rest) = message.split_first().ok_or(colbin::Error::Truncated)?;
        if root & ROOT_RANGE_MASK != ROOT_STRUCT_NARROW {
            return Err(colbin::Error::BadRoot(*root));
        }
        // Four of sixteen detail values are assigned. The rest must be refused,
        // which is what leaves room for the format to grow.
        if root & !ROOT_ASSIGNED != ROOT_STRUCT_NARROW {
            return Err(colbin::Error::BadRoot(*root));
        }
        let wide = root & ROOT_WIDE_BIT != 0;

        if root & ROOT_SCHEMA_BIT != 0 {
            let schema = section::parse(rest)?;
            let body = rest.get(schema.size..).ok_or(colbin::Error::Truncated)?;
            return walk::to_json(&schema, body, wide);
        }
        SCHEMA.with_borrow(|held| match held.as_ref() {
            None => Err(colbin::Error::NoSchema),
            Some(schema) => walk::to_json(schema, rest, wide),
        })
    });

    match outcome {
        Ok(json) => ok(json),
        Err(err) => fail_err(&err),
    }
}

/// The message's table, as a flat typed buffer — `colbin::materialize::to_buffer`,
/// behind the wasm ABI `decode` already uses.
///
/// Returns the buffer's length, `0` when the root is not the table shape the
/// materializer covers (the caller falls back to `decode`), or -1 with a
/// diagnostic at `last_error`. A real buffer is never empty — its header alone
/// is 12 bytes — so `0` is not ambiguous with a genuine success.
#[cfg(feature = "materialize")]
#[allow(unsafe_code, reason = "an exported symbol must not be mangled")]
#[unsafe(no_mangle)]
pub extern "C" fn materialize(len: usize) -> i32 {
    let outcome = INPUT.with_borrow(|input| {
        let message = input.get(..len).ok_or(colbin::Error::Truncated)?;
        let (root, rest) = message.split_first().ok_or(colbin::Error::Truncated)?;
        if root & ROOT_RANGE_MASK != ROOT_STRUCT_NARROW {
            return Err(colbin::Error::BadRoot(*root));
        }
        if root & !ROOT_ASSIGNED != ROOT_STRUCT_NARROW {
            return Err(colbin::Error::BadRoot(*root));
        }
        let wide = root & ROOT_WIDE_BIT != 0;

        if root & ROOT_SCHEMA_BIT != 0 {
            let schema = section::parse(rest)?;
            let body = rest.get(schema.size..).ok_or(colbin::Error::Truncated)?;
            return colbin::materialize::to_buffer(&schema, body, wide);
        }
        SCHEMA.with_borrow(|held| match held.as_ref() {
            None => Err(colbin::Error::NoSchema),
            Some(schema) => colbin::materialize::to_buffer(schema, rest, wide),
        })
    });

    match outcome {
        Ok(Some(buf)) => ok(buf),
        Ok(None) => ok(Vec::new()),
        Err(err) => fail_err(&err),
    }
}

/// The field tree of a message, with a byte span on every node.
///
/// The spans are absolute offsets into the message handed in, so a caller can
/// highlight them without knowing where the body starts behind the section.
///
/// Behind its own feature, **off by default**. It used to sit behind `encode`,
/// on the reasoning that a decode-only client has no hex view to feed — which
/// was right about who wants it and wrong about who was paying for it: every
/// consumer of the npm package linked the span walk, 14.8 KB of the module's
/// code, to never call it. The demo page is the one caller, and it builds the
/// module itself.
#[cfg(feature = "inspect")]
#[allow(unsafe_code, reason = "an exported symbol must not be mangled")]
#[unsafe(no_mangle)]
pub extern "C" fn inspect_message(len: usize) -> i32 {
    let outcome = INPUT.with_borrow(|input| {
        let message = input.get(..len).ok_or(colbin::Error::Truncated)?;
        SCHEMA.with_borrow(|held| colbin::inspect::inspect(message, held.as_ref()))
    });
    match outcome {
        Ok(json) => ok(json),
        Err(err) => fail_err(&err),
    }
}

/// The last diagnostic as JSON, at `result_ptr`. Returns its length.
///
/// Overwrites the last successful output: a caller that still wants the message
/// copies it before asking why the next call failed — and a successful encode
/// still has warnings to hand back.
#[allow(unsafe_code, reason = "an exported symbol must not be mangled")]
#[unsafe(no_mangle)]
pub extern "C" fn last_error() -> i32 {
    let bytes = LAST_DIAG.with_borrow(Vec::clone);
    ok(bytes)
}

fn ok(bytes: Vec<u8>) -> i32 {
    let length = bytes.len();
    RESULT.with_borrow_mut(|result| *result = bytes);
    match i32::try_from(length) {
        Ok(length) => length,
        // Out of reach at wasm32's four gibibytes, and still not a bare `-1`: a
        // negative return sends the host to `last_error`, which would otherwise
        // hand back whatever the previous call left there.
        Err(_) => fail_err(&colbin::Error::SizeTooLarge),
    }
}

fn fail_err(err: &colbin::Error) -> i32 {
    fail_bytes(error_envelope(err))
}

fn fail_bytes(bytes: Vec<u8>) -> i32 {
    remember_diag(&bytes);
    RESULT.with_borrow_mut(|result| *result = bytes);
    -1
}

fn remember_diag(bytes: &[u8]) {
    LAST_DIAG.with_borrow_mut(|held| {
        held.clear();
        held.extend_from_slice(bytes);
    });
}

/// The same envelope `colbin::diag::Diag::encode` writes, so one host-side
/// reader serves every failure — and the decode-only build, which does not
/// compile `Diag` at all, still speaks it.
fn error_envelope(err: &colbin::Error) -> Vec<u8> {
    let mut out = Vec::with_capacity(128);
    out.extend_from_slice(br#"{"code":2,"offset":-1,"line":-1,"path":"","message":"#);
    colbin::json::write_json_str(&mut out, &err.to_string());
    out.extend_from_slice(br#","warnings":[]}"#);
    out
}
