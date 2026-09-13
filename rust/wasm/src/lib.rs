//! The public ABI: colbin bytes in, JSON text out.
//!
//! Bytes cross the boundary and nothing else. The host never hands over a
//! JavaScript object graph and never receives one, because `JSON.parse` would
//! have already rounded any integer past 2^53 before the codec saw it — which is
//! the failure colbin exists to prevent, and it would be a poor place to
//! reintroduce it.
//!
//! Mirrors `web/assembly/index.ts`, so the same JavaScript drives either module:
//!
//! ```text
//! alloc(n)        -> a pointer to write n bytes of input at
//! set_schema(len) -> parse the section in the input buffer and hold it
//! decode(len)     -> the message in the input buffer, as JSON text
//! result_ptr()    -> where the last successful call left its output
//! last_error()    -> the last failure as a JSON diagnostic, at result_ptr
//! ```
//!
//! # Nothing here traps
//!
//! A trap reaches the host as a `RuntimeError` carrying no path and no offset,
//! which is exactly what a caller cannot act on. So every failure returns a
//! negative length and leaves a message for `last_error` to hand back. The crate
//! forbids `unsafe`, and the library it calls has no `unwrap`, `expect` or
//! `panic!` in it, so the ways to trap are few by construction rather than by
//! inspection.
//!
//! # Where the schema travels
//!
//! Out of band by default, which is the delivery the format advises: send the
//! section once per connection and then send ordinary messages. A message whose
//! root byte says it carries its own section is handled without `set_schema`
//! having been called, which is the self-describing shape a file or a one-shot
//! response wants.

// `deny` rather than `forbid`, with one scoped exception below.
//
// An exported symbol is `unsafe(no_mangle)` in edition 2024 because an
// unmangled name can collide at link time, and that is a real hazard — but it
// is the *only* unsafety here, and it is five attributes on five functions with
// no pointer arithmetic behind any of them. Everything else, including the whole
// of the `colbin` crate this calls, stays `forbid(unsafe_code)`.
#![deny(unsafe_code)]

use std::cell::RefCell;

use colbin::section::{self, Schema};
use colbin::walk;

/// The root bytes this version writes. `0xD0` narrow, `0xD8` wide, and the two
/// with bit 2 set carry their own schema section in front of the body.
const ROOT_STRUCT_NARROW: u8 = 0xd0;
const ROOT_WIDE_BIT: u8 = 0x08;
const ROOT_SCHEMA_BIT: u8 = 0x04;
const ROOT_RANGE_MASK: u8 = 0xf0;

thread_local! {
    /// Held across calls, because the host writes the input in one call and
    /// names its length in the next.
    static INPUT: RefCell<Vec<u8>> = const { RefCell::new(Vec::new()) };
    static RESULT: RefCell<Vec<u8>> = const { RefCell::new(Vec::new()) };
    static SCHEMA: RefCell<Option<Schema>> = const { RefCell::new(None) };
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
        Err(err) => fail(&err),
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
        let wide = root & ROOT_WIDE_BIT != 0;

        if root & ROOT_SCHEMA_BIT != 0 {
            // Self-describing: the section sits between the root byte and the
            // body, and `size` is what steps over it.
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
        Ok(json) => {
            let length = json.len();
            RESULT.with_borrow_mut(|result| *result = json);
            // A message larger than 2 GB cannot be described by the i32 this ABI
            // returns, and is refused rather than reported as a negative length
            // that the host would read as an error code.
            i32::try_from(length).unwrap_or(-1)
        }
        Err(err) => fail(&err),
    }
}

/// The last diagnostic as JSON, at `result_ptr`. Returns its length.
///
/// The same envelope `web/assembly/diag.ts` writes, so one host-side reader
/// serves both modules.
#[allow(unsafe_code, reason = "an exported symbol must not be mangled")]
#[unsafe(no_mangle)]
pub extern "C" fn last_error() -> i32 {
    RESULT.with_borrow(|result| i32::try_from(result.len()).unwrap_or(0))
}

/// Records a failure as the diagnostic envelope and returns the ABI's -1.
fn fail(err: &colbin::Error) -> i32 {
    let mut out = Vec::with_capacity(128);
    out.extend_from_slice(br#"{"code":2,"offset":-1,"line":-1,"path":"","message":"#);
    colbin::json::write_json_str(&mut out, &err.to_string());
    out.extend_from_slice(br#","warnings":[]}"#);
    RESULT.with_borrow_mut(|result| *result = out);
    -1
}
