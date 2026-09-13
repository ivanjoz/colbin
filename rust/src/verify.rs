//! The encode self-check (`web/PLAN.md` §4.5).
//!
//! Mirrors `web/assembly/verify.ts`. The structural rules in [`crate::infer`]
//! and [`crate::build`] are an argument that a message says what its input said.
//! This is the proof: the encoder decodes what it just wrote and walks it
//! against the parsed input, so the caller hears about a disagreement as an
//! error rather than as data.
//!
//! It is on by default and costs roughly one decode. §4.3 established why that
//! is worth paying, and the corruption sweep in `web/tests/fuzz.test.mjs` puts a
//! number on it: five sixths of all single-byte corruptions of a colbin message
//! decode to well-formed, *wrong* JSON. A decoder cannot tell. An encoder can,
//! because it still has the input.
//!
//! # It compares values, not bytes
//!
//! "It decoded without error" is far too weak a check, for exactly that reason.
//! And comparing the two *texts* would be too strong: the decoder writes every
//! field of the schema, in the order the message carried them, where the input
//! wrote only what it had in the order the author typed it.
//!
//! Three differences are expected rather than failures, and every one of them is
//! a documented property of a dense columnar layout (`web/PLAN.md` §6):
//!
//!   - a key absent from a record comes back as its zero;
//!   - an empty array comes back as null;
//!   - an integer in a column one float promoted comes back as that float.
//!
//! Anything else is a bug in the encoder, and the point of this file is that it
//! is caught here rather than by whoever reads the message next.

use alloc::format;
use alloc::string::String;
use alloc::vec::Vec;

use crate::diag::{D_CORRUPT, Diag};
use crate::json::parse::{
    self, Doc, K_ARRAY, K_BOOL, K_FLOAT, K_INT, K_NULL, K_OBJECT, K_STRING, K_UINT,
};

/// Compares the JSON the decoder produced against the document the encoder was
/// given. Returns whether they agree, and sets `diag` when they do not.
#[must_use]
pub fn verify(input: &Doc, input_root: u32, decoded: &[u8], diag: &mut Diag) -> bool {
    let mut quiet = Diag::new();
    let Some(back) = parse::parse(decoded, &mut quiet) else {
        diag.fail(
            D_CORRUPT,
            -1,
            "",
            format!(
                "the encoder wrote a message whose decoding is not valid JSON: {}",
                quiet.message,
            ),
        );
        return false;
    };
    let root = back.root();
    let mut checker = Checker {
        input,
        back: &back,
        diag,
        trail: Vec::new(),
    };
    checker.compare(Some(input_root), root);
    diag.ok()
}

/// One step of the path to the value being compared.
///
/// A trail rather than text: building the string eagerly would put a UTF-8
/// decode on every key of every record, for a path that is read only when
/// something differs. A key step holds the *decoded* node and the slot it sits
/// in, so the name is looked up once, at the failure.
enum Step {
    Index(usize),
    Key { node: u32, slot: usize },
}

struct Checker<'a, 'd> {
    input: &'a Doc,
    back: &'a Doc,
    diag: &'d mut Diag,
    trail: Vec<Step>,
}

impl Checker<'_, '_> {
    /// The trail as text, built only when there is a failure to describe.
    fn path_string(&self) -> String {
        let mut out = String::new();
        for step in &self.trail {
            match step {
                Step::Index(at) => out.push_str(&format!("[{at}]")),
                Step::Key { node, slot } => {
                    let key = self.back.key_of(*node, *slot);
                    out.push('.');
                    out.push_str(&String::from_utf8_lossy(key));
                }
            }
        }
        if out.is_empty() {
            String::from("(root)")
        } else {
            out
        }
    }

    fn differs(&mut self, what: &str) {
        let path = self.path_string();
        self.diag.fail(
            D_CORRUPT,
            -1,
            &path,
            format!("the encoder wrote a message that does not read back as its input: {what}"),
        );
    }

    /// One value against another. `mine` may be `None`, which is a key the input
    /// did not carry — the decoder will still have written it, and its zero is
    /// what it must have written.
    fn compare(&mut self, mine: Option<u32>, theirs: u32) {
        if !self.diag.ok() {
            return;
        }
        let Some(mine) = mine else {
            // An absent key comes back as its zero. That is the encoding of a
            // zero value, not a lost field.
            if !self.is_zeroish(theirs) {
                self.differs(
                    "a key the input did not carry came back as something other than a zero",
                );
            }
            return;
        };

        match self.input.kind_of(mine) {
            K_NULL => {
                // A null is omitted, so it comes back as null where the format
                // has a pointer for it and as the zero where it does not.
                if !self.is_zeroish(theirs) {
                    self.differs("a null came back as a value");
                }
            }
            K_BOOL => {
                if self.back.kind_of(theirs) != K_BOOL
                    || self.back.bool_of(theirs) != self.input.bool_of(mine)
                {
                    self.differs("a boolean came back differently");
                }
            }
            K_INT | K_UINT | K_FLOAT => self.compare_number(mine, theirs),
            K_STRING => {
                if self.back.kind_of(theirs) != K_STRING
                    || self.back.str_of(theirs) != self.input.str_of(mine)
                {
                    self.differs("a string came back differently");
                }
            }
            K_ARRAY => self.compare_array(mine, theirs),
            _ => self.compare_object(mine, theirs),
        }
    }

    fn compare_number(&mut self, mine: u32, theirs: u32) {
        let their_kind = self.back.kind_of(theirs);
        if !matches!(their_kind, K_INT | K_UINT | K_FLOAT) {
            self.differs("a number came back as something else");
            return;
        }
        let my_kind = self.input.kind_of(mine);
        if my_kind == K_FLOAT || their_kind == K_FLOAT {
            // One float anywhere in a column promotes the whole column, so an
            // integer legitimately comes back as that float. Comparing as f64 is
            // what makes that a tolerance rather than a hole: a value that does
            // not survive the promotion still differs.
            if as_float(self.input, mine) != as_float(self.back, theirs) {
                self.differs("a number came back with a different value");
            }
            return;
        }
        // Both integers: compare the bits *and* the signedness, so an int64 and
        // a uint64 holding the same bit pattern are not mistaken for each other.
        if self.input.uint_of(mine) != self.back.uint_of(theirs) || my_kind != their_kind {
            self.differs("an integer came back with a different value");
        }
    }

    fn compare_array(&mut self, mine: u32, theirs: u32) {
        let count = self.input.count(mine);
        if self.back.kind_of(theirs) == K_NULL {
            // An empty array and a nil one are the same on the wire.
            if count != 0 {
                self.differs("an array came back as null");
            }
            return;
        }
        if self.back.kind_of(theirs) != K_ARRAY {
            self.differs("an array came back as something else");
            return;
        }
        let got = self.back.count(theirs);
        if got != count {
            self.differs(&format!("an array of {count} came back with {got}"));
            return;
        }
        for index in 0..count {
            if !self.diag.ok() {
                break;
            }
            self.trail.push(Step::Index(index));
            self.compare(
                Some(self.input.child_at(mine, index)),
                self.back.child_at(theirs, index),
            );
            self.trail.pop();
        }
    }

    fn compare_object(&mut self, mine: u32, theirs: u32) {
        if self.back.kind_of(theirs) != K_OBJECT {
            self.differs("an object came back as something else");
            return;
        }
        // Walk the *decoder's* keys, because it writes every field of the schema
        // and the input may have written only some. A key the input did not
        // carry is compared against `None`, which is the absent case above.
        for index in 0..self.back.count(theirs) {
            if !self.diag.ok() {
                break;
            }
            let want = self.back.key_of(theirs, index);
            let found = find_key(self.input, Some(mine), want);
            self.trail.push(Step::Key {
                node: theirs,
                slot: index,
            });
            self.compare(found, self.back.child_at(theirs, index));
            self.trail.pop();
        }
        // And the other way: a key the input carried that the decoder did not
        // write is a field the schema lost, which is the failure this whole file
        // exists to catch.
        for index in 0..self.input.count(mine) {
            if !self.diag.ok() {
                break;
            }
            let key = self.input.key_of(mine, index);
            if find_key(self.back, Some(theirs), key).is_none() {
                let name = String::from_utf8_lossy(key).into_owned();
                self.differs(&format!(
                    "the input carried a key the message does not: {name}"
                ));
            }
        }
    }

    /// Whether a decoded value is the zero an omitted field comes back as.
    fn is_zeroish(&self, node: u32) -> bool {
        match self.back.kind_of(node) {
            K_NULL => true,
            K_BOOL | K_INT | K_UINT => self.back.uint_of(node) == 0,
            K_FLOAT => self.back.float_of(node) == 0.0,
            K_STRING => self.back.str_of(node).is_empty(),
            K_ARRAY => self.back.count(node) == 0,
            K_OBJECT => {
                // A nested struct is always written, so an omitted one comes
                // back as an object whose every field is itself a zero.
                (0..self.back.count(node)).all(|at| self.is_zeroish(self.back.child_at(node, at)))
            }
            _ => false,
        }
    }
}

/// The child of `node` under `want`, or `None`. Last rather than first, because
/// a duplicate key overwrites — which is what `JSON.parse` does and what
/// [`crate::build`] writes.
fn find_key(doc: &Doc, node: Option<u32>, want: &[u8]) -> Option<u32> {
    let node = node?;
    if doc.kind_of(node) != K_OBJECT {
        return None;
    }
    let mut found = None;
    for index in 0..doc.count(node) {
        if doc.key_of(node, index) == want {
            found = Some(doc.child_at(node, index));
        }
    }
    found
}

#[allow(clippy::cast_precision_loss)]
fn as_float(doc: &Doc, node: u32) -> f64 {
    match doc.kind_of(node) {
        K_FLOAT => doc.float_of(node),
        K_UINT => doc.uint_of(node) as f64,
        _ => doc.int_of(node) as f64,
    }
}
