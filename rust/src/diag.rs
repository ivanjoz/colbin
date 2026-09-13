//! Structured failure, for the encode side.
//!
//! Mirrors `web/assembly/diag.ts`. One shape for parse errors, type conflicts,
//! limits and corrupt messages alike, carrying a byte offset and a path into the
//! document so a caller can point at what was wrong rather than only say that
//! something was.
//!
//! # Why not [`crate::Error`]
//!
//! `Error` is the decoder's, and the note at the top of `error.rs` says why
//! every variant of it is a read-side failure: a writer trusts its own program.
//! That stops being true the moment the thing being encoded is a document
//! somebody typed. `{"a":1,"a":"two"}` is not a bug in this crate and not a
//! corrupt message — it is a type conflict at byte 9 — and an enum with no room
//! for the 9 could only report it as an enum variant.
//!
//! Nothing here allocates until something has already failed.

use alloc::string::{String, ToString};
use alloc::vec::Vec;

pub const D_NONE: i32 = 0;
/// The document is not JSON.
pub const D_SYNTAX: i32 = 1;
/// A number the format cannot carry exactly.
pub const D_NUMBER: i32 = 2;
/// A document past one of the encoder's bounds.
pub const D_LIMIT: i32 = 3;
/// One key holding two types, or an array the format has no shape for.
pub const D_CONFLICT: i32 = 4;
/// A message that does not read back as what went in.
pub const D_CORRUPT: i32 = 5;
/// A shape the encoder declines rather than encodes badly.
pub const D_UNSUPPORTED: i32 = 6;

/// What failed, where, and on the way to which field.
#[derive(Debug, Clone, Default)]
pub struct Diag {
    pub code: i32,
    /// Byte offset into the input, or `-1` when the failure has no position.
    pub offset: i32,
    /// 1-based line, filled in by [`Diag::locate`] when reporting.
    pub line: i32,
    /// Dotted path to the value, for the failures that have one.
    pub path: String,
    pub message: String,
    pub warnings: Vec<String>,
}

impl Diag {
    #[must_use]
    pub fn new() -> Self {
        Self {
            code: D_NONE,
            offset: -1,
            line: -1,
            ..Self::default()
        }
    }

    #[must_use]
    pub fn ok(&self) -> bool {
        self.code == D_NONE
    }

    /// First failure wins: later ones are usually consequences of the first, and
    /// the caller wants the cause rather than the last symptom.
    pub fn fail(&mut self, code: i32, offset: i32, path: &str, message: impl Into<String>) {
        if self.code != D_NONE {
            return;
        }
        self.code = code;
        self.offset = offset;
        self.path = path.to_string();
        self.message = message.into();
    }

    pub fn warn(&mut self, message: impl Into<String>) {
        self.warnings.push(message.into());
    }

    pub fn reset(&mut self) {
        *self = Self::new();
    }

    /// Fills in [`Diag::line`] from the source the offset points into. Done here
    /// rather than at the failure, so the scanner carries no line counter
    /// through its hot loop.
    pub fn locate(&mut self, src: &[u8]) {
        self.line = if self.offset < 0 {
            -1
        } else {
            line_of(src, self.offset as usize)
        };
    }

    /// The diagnostic as JSON: code, offset, line, path, message, warnings.
    ///
    /// The same envelope `web/assembly/diag.ts` writes and `rust/wasm` writes
    /// for a decode failure, so one host-side reader serves every case.
    #[must_use]
    pub fn encode(&self) -> Vec<u8> {
        let mut out = Vec::with_capacity(128 + self.message.len());
        out.extend_from_slice(br#"{"code":"#);
        crate::json::write_i64(&mut out, i64::from(self.code));
        out.extend_from_slice(br#","offset":"#);
        crate::json::write_i64(&mut out, i64::from(self.offset));
        out.extend_from_slice(br#","line":"#);
        crate::json::write_i64(&mut out, i64::from(self.line));
        out.extend_from_slice(br#","path":"#);
        crate::json::write_json_str(&mut out, &self.path);
        out.extend_from_slice(br#","message":"#);
        crate::json::write_json_str(&mut out, &self.message);
        out.extend_from_slice(br#","warnings":["#);
        for (at, warning) in self.warnings.iter().enumerate() {
            if at > 0 {
                out.push(b',');
            }
            crate::json::write_json_str(&mut out, warning);
        }
        out.extend_from_slice(b"]}");
        out
    }
}

/// The 1-based line a byte offset falls on.
#[must_use]
pub fn line_of(src: &[u8], offset: usize) -> i32 {
    let end = offset.min(src.len());
    let mut line = 1i32;
    for &byte in &src[..end] {
        if byte == b'\n' {
            line += 1;
        }
    }
    line
}
