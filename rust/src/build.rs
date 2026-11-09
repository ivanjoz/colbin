//! The body: a schema and a parsed document in, a message out.
//!
//! The body is generated from the **schema**, never from the raw JSON values
//! (`rust/ENCODER.md` §5, rule 2). One source of truth means a value that does not
//! fit its column cannot be written — it is a conflict at fill time instead of a
//! byte that decodes as something else. So every function here takes the field
//! it is writing and looks the value up, rather than taking a value and deciding
//! what it is.
//!
//! # Two widths, written twice
//!
//! Exactly as the readers are, and for the format's reason rather than the
//! compiler's: [`wire::Writer`] and [`wire::Writer8`] produce different bytes for
//! the same field, and sharing them behind a trait object would cost a virtual
//! call per field to save a match that is already resolved per run.
//!
//! # A slice of structs picks its own layout
//!
//! Past [`TABLE_THRESHOLD`] rows a `[]struct` is transposed into a table — one
//! key per column rather than one per field per row — provided every field of
//! the element is a column the codec carries. The threshold is Go's, and it has
//! to be: two encoders must pick the same shape for the same data or a message
//! that is otherwise identical comes out different.

use alloc::format;
use alloc::vec::Vec;

use crate::codec::{ROOT_STRUCT_NARROW, ROOT_STRUCT_WIDE, TABLE_THRESHOLD};
use crate::diag::{D_LIMIT, D_UNSUPPORTED, Diag};
use crate::json::parse::{Doc, K_BOOL, K_FLOAT, K_INT, K_NULL, K_OBJECT, K_STRING, K_UINT};
use crate::plan::{
    OP_BOOL, OP_FLOAT32, OP_FLOAT64, OP_INT64, OP_INT64S, OP_POINTER, OP_STRING, OP_STRINGS,
    OP_STRUCT, OP_STRUCTS, OP_UINT64, OP_UINT64S, Plan, PlanField, width_of_op,
};
use crate::section::Schema;
use crate::wire;

/// The root byte's bit saying a schema section rides in front of the body.
const ROOT_SCHEMA: u8 = 0x04;

/// How deep the encoder will recurse. At or below the decoder's bound, so what
/// this writes it can always read back (`ENCODER.md` §5, rule 4).
const MAX_DEPTH: u32 = 64;

/// Where a value lives in the document, or that the record does not carry it.
type Node = Option<u32>;

/// Turns a document into a message against a schema inferred from it.
pub struct Builder<'a, 'd> {
    doc: &'a Doc,
    schema: &'a Schema,
    diag: &'d mut Diag,
    /// Whether to offer each string to the packed encoding.
    ///
    /// A writer setting, exactly as Go's `SetPacked5` is, and off for the same
    /// reason: a raw blob is a sub-slice of the message on the way out and a
    /// `memcpy` on the way in, where a packed one is a pass over every character
    /// on both sides. The trade is a third of a short token against that pass,
    /// which is worth taking on a wire that is size-bound and not on one that is
    /// not.
    ///
    /// It is never a correctness question. The encoding is recorded in each
    /// string's own descriptor, so a decoder reads either form without being
    /// told — and the encoder tries the packed form and keeps it only when it is
    /// smaller, so turning this on cannot make a message larger.
    pub pack_strings: bool,
    depth: u32,
}

impl<'a, 'd> Builder<'a, 'd> {
    #[must_use]
    pub fn new(doc: &'a Doc, schema: &'a Schema, diag: &'d mut Diag) -> Self {
        Self {
            doc,
            schema,
            diag,
            pack_strings: false,
            depth: 0,
        }
    }

    fn ok(&self) -> bool {
        self.diag.ok()
    }

    fn enter(&mut self) -> bool {
        if self.depth >= MAX_DEPTH {
            self.diag.fail(
                D_LIMIT,
                -1,
                "",
                "the document nests deeper than the encoder writes",
            );
            return false;
        }
        self.depth += 1;
        true
    }

    /// The run, behind the root byte a wide or self-describing message has: a
    /// typed narrow message has none, and gets `0xD0` only when its key run
    /// begins in the root range.
    pub fn build(&mut self, out: &mut Vec<u8>, node: u32, with_schema: bool) {
        let wide = self.schema.root().is_wide;
        if with_schema || wide {
            let mut root = if wide {
                ROOT_STRUCT_WIDE
            } else {
                ROOT_STRUCT_NARROW
            };
            if with_schema {
                root |= ROOT_SCHEMA;
            }
            out.push(root);
            self.run(out, 0, node);
            return;
        }
        let start = out.len();
        self.run(out, 0, node);
        wire::root::finish_narrow(out, start);
    }

    /// A key run at whichever width the plan declared.
    pub fn run(&mut self, out: &mut Vec<u8>, at: u32, node: u32) {
        let Some(plan) = self.schema.plan(at) else {
            return;
        };
        if plan.is_wide {
            self.wide_run(out, plan, node);
        } else {
            self.narrow_run(out, plan, node);
        }
    }

    // ---- the narrow path ----------------------------------------------------

    fn narrow_run(&mut self, out: &mut Vec<u8>, plan: &'a Plan, node: u32) {
        if !self.enter() {
            return;
        }
        let mut writer = wire::Writer::new(out);
        for (index, field) in plan.fields.iter().enumerate() {
            if !self.ok() {
                break;
            }
            // An envelope's single field *is* the document, so there is no
            // object to look a name up in.
            let value = if plan.is_envelope {
                Some(node)
            } else {
                self.value_of(plan, index, Some(node))
            };
            self.narrow_field(&mut writer, field, value);
        }
        self.depth -= 1;
    }

    fn narrow_field(&mut self, w: &mut wire::Writer<'_>, field: &PlanField, node: Node) {
        // An absent key and an explicit null are the same thing on the wire: the
        // field is simply not written, and a pointer field decodes back as null.
        let Some(node) = node else { return };
        if self.doc.kind_of(node) == K_NULL {
            return;
        }
        match field.op {
            // A non-nil pointer to a zero writes an explicit zero — two bytes —
            // because otherwise it would be indistinguishable from nil. That is
            // the one value in the format written solely to say it is there.
            OP_POINTER => self.narrow_scalar(w, field.key, field.elem_op, node, true),
            OP_STRUCT => {
                let Some(sub) = field.sub else { return };
                let wide = self.schema.plan(sub).is_some_and(|plan| plan.is_wide);
                let mark = if wide {
                    w.open_struct_wide(field.key)
                } else {
                    w.open_struct(field.key)
                };
                self.run(w.buf, sub, node);
                w.close(mark);
            }
            OP_STRUCTS => self.narrow_structs(w, field, node),
            OP_STRINGS => {
                let values = self.gather_strings(node);
                w.strings(field.key, &values);
            }
            OP_INT64S => {
                let values = self.gather_ints(node);
                w.ints(field.key, &values);
            }
            OP_UINT64S => {
                // A narrow array's form is its element type's, so a `[]uint64`
                // travels as magnitudes — and the gathered `i64`s go back to the
                // `u64`s they came from, past 2^63 included, before it does.
                let values: Vec<u64> = self
                    .gather_ints(node)
                    .into_iter()
                    .map(i64::cast_unsigned)
                    .collect();
                w.ints(field.key, &values);
            }
            op => self.narrow_scalar(w, field.key, op, node, false),
        }
    }

    fn narrow_scalar(
        &mut self,
        w: &mut wire::Writer<'_>,
        key: u8,
        op: u8,
        node: u32,
        explicit_zero: bool,
    ) {
        // Every type spells its explicit zero the same way under narrow keys —
        // the length form with nothing in it — so one `zero` serves them all.
        match op {
            OP_BOOL => {
                let value = self.doc.uint_of(node) != 0;
                if !value && explicit_zero {
                    w.zero(key);
                } else {
                    w.bool(key, value);
                }
            }
            OP_UINT64 => {
                let value = self.doc.uint_of(node);
                if value == 0 && explicit_zero {
                    w.zero(key);
                } else {
                    w.u64(key, value);
                }
            }
            OP_INT64 => {
                let value = self.as_int(node);
                if value == 0 && explicit_zero {
                    w.zero(key);
                } else {
                    w.i64(key, value);
                }
            }
            OP_FLOAT64 | OP_FLOAT32 => {
                let value = self.as_float(node);
                if value == 0.0 && explicit_zero {
                    w.zero(key);
                } else {
                    w.f64(key, value);
                }
            }
            OP_STRING => {
                let bytes = self.text_of(node);
                if bytes.is_empty() && explicit_zero {
                    w.zero(key);
                } else if self.pack_strings {
                    w.packed_bytes(key, bytes);
                } else {
                    w.bytes(key, bytes);
                }
            }
            _ => self.unwritable(op),
        }
    }

    fn narrow_structs(&mut self, w: &mut wire::Writer<'_>, field: &PlanField, node: u32) {
        let Some(sub) = field.sub else { return };
        let rows = self.doc.count(node);
        if rows == 0 {
            return;
        }
        let element = self.schema.plan(sub);
        if element.is_some_and(|plan| plan.can_table) && rows >= TABLE_THRESHOLD {
            let mark = w.open_table(field.key, rows);
            // The columns are keyed at the row type's width, which for wide
            // rows is not this run's. See `codec::write_table`.
            if element.is_some_and(|plan| plan.is_wide) {
                self.wide_columns(&mut wire::Writer8::new(w.buf), sub, node, rows);
            } else {
                self.narrow_columns(w, sub, node, rows);
            }
            w.close(mark);
            return;
        }
        if rows == 1 {
            let single = w.open_single(field.key);
            let child = self.doc.child_at(node, 0);
            self.run(w.buf, sub, child);
            w.close(single);
            return;
        }
        let list = w.open_list(field.key, rows);
        for index in 0..rows {
            if !self.ok() {
                break;
            }
            let mark = w.open_element();
            let child = self.doc.child_at(node, index);
            self.run(w.buf, sub, child);
            w.close_element(mark);
        }
        w.close(list);
    }

    fn narrow_columns(&mut self, w: &mut wire::Writer<'_>, sub: u32, node: u32, rows: usize) {
        let Some(plan) = self.schema.plan(sub) else {
            return;
        };
        for (index, field) in plan.fields.iter().enumerate() {
            if !self.ok() {
                break;
            }
            if field.op == OP_STRING {
                let values = self.gather_string_column(plan, index, node, rows);
                w.string_column(field.key, &values);
                continue;
            }
            let values = self.gather_column(plan, index, node, rows);
            if field.op == OP_FLOAT64 {
                w.float_column(field.key, &values);
                continue;
            }
            write_column_narrow(w, field.key, &values, width_of_op(field.op));
        }
    }

    // ---- the wide path ------------------------------------------------------

    fn wide_run(&mut self, out: &mut Vec<u8>, plan: &'a Plan, node: u32) {
        if !self.enter() {
            return;
        }
        let mut writer = wire::Writer8::new(out);
        for (index, field) in plan.fields.iter().enumerate() {
            if !self.ok() {
                break;
            }
            let value = if plan.is_envelope {
                Some(node)
            } else {
                self.value_of(plan, index, Some(node))
            };
            self.wide_field(&mut writer, field, value);
        }
        self.depth -= 1;
    }

    fn wide_field(&mut self, w: &mut wire::Writer8<'_>, field: &PlanField, node: Node) {
        let Some(node) = node else { return };
        if self.doc.kind_of(node) == K_NULL {
            return;
        }
        match field.op {
            OP_POINTER => self.wide_scalar(w, field.key, field.elem_op, node, true),
            OP_STRUCT => {
                let Some(sub) = field.sub else { return };
                let wide = self.schema.plan(sub).is_some_and(|plan| plan.is_wide);
                let mark = if wide {
                    w.open_struct_wide(field.key)
                } else {
                    w.open_struct(field.key)
                };
                self.run(w.buf, sub, node);
                w.close(mark);
            }
            OP_STRUCTS => self.wide_structs(w, field, node),
            OP_STRINGS => {
                let values = self.gather_strings(node);
                w.strings(field.key, &values);
            }
            OP_INT64S | OP_UINT64S => {
                let values = self.gather_ints(node);
                w.ints(field.key, &values);
            }
            op => self.wide_scalar(w, field.key, op, node, false),
        }
    }

    fn wide_scalar(
        &mut self,
        w: &mut wire::Writer8<'_>,
        key: u8,
        op: u8,
        node: u32,
        explicit_zero: bool,
    ) {
        match op {
            OP_BOOL => {
                let value = self.doc.uint_of(node) != 0;
                if !value && explicit_zero {
                    w.zero(key);
                } else {
                    w.bool(key, value);
                }
            }
            OP_UINT64 => {
                let value = self.doc.uint_of(node);
                if value == 0 && explicit_zero {
                    w.zero(key);
                } else {
                    w.u64(key, value);
                }
            }
            OP_INT64 => {
                let value = self.as_int(node);
                if value == 0 && explicit_zero {
                    // The wide writer has one explicit zero: its signed and
                    // unsigned forms are the same descriptor.
                    w.zero(key);
                } else {
                    w.i64(key, value);
                }
            }
            OP_FLOAT64 | OP_FLOAT32 => {
                let value = self.as_float(node);
                if value == 0.0 && explicit_zero {
                    w.zero(key);
                } else {
                    w.f64(key, value);
                }
            }
            OP_STRING => {
                let bytes = self.text_of(node);
                if bytes.is_empty() && explicit_zero {
                    w.empty_string(key);
                } else if self.pack_strings {
                    w.packed_bytes(key, bytes);
                } else {
                    w.bytes(key, bytes);
                }
            }
            _ => self.unwritable(op),
        }
    }

    fn wide_structs(&mut self, w: &mut wire::Writer8<'_>, field: &PlanField, node: u32) {
        let Some(sub) = field.sub else { return };
        let rows = self.doc.count(node);
        if rows == 0 {
            return;
        }
        let element = self.schema.plan(sub);
        if element.is_some_and(|plan| plan.can_table) && rows >= TABLE_THRESHOLD {
            let mark = w.open_table(field.key, rows);
            self.wide_columns(w, sub, node, rows);
            w.close(mark);
            return;
        }
        let wide = element.is_some_and(|plan| plan.is_wide);
        let list = w.open_list(field.key, rows);
        for index in 0..rows {
            if !self.ok() {
                break;
            }
            let mark = if wide {
                w.open_element_struct_wide()
            } else {
                w.open_element_struct()
            };
            let child = self.doc.child_at(node, index);
            self.run(w.buf, sub, child);
            w.close(mark);
        }
        w.close(list);
    }

    fn wide_columns(&mut self, w: &mut wire::Writer8<'_>, sub: u32, node: u32, rows: usize) {
        let Some(plan) = self.schema.plan(sub) else {
            return;
        };
        for (index, field) in plan.fields.iter().enumerate() {
            if !self.ok() {
                break;
            }
            if field.op == OP_STRING {
                let values = self.gather_string_column(plan, index, node, rows);
                w.string_column(field.key, &values);
                continue;
            }
            let values = self.gather_column(plan, index, node, rows);
            if field.op == OP_FLOAT64 {
                w.float_column(field.key, &values);
                continue;
            }
            write_column_wide(w, field.key, &values, width_of_op(field.op));
        }
    }

    // ---- gathering ----------------------------------------------------------

    /// The document node for one field of one record, or `None` when the record
    /// does not carry it.
    fn value_of(&self, plan: &Plan, index: usize, node: Node) -> Node {
        let node = node?;
        if self.doc.kind_of(node) != K_OBJECT {
            return None;
        }
        let name = plan.names.get(index)?.as_bytes();
        // Records of one shape carry their keys in the order the schema numbered
        // them, so the field's own position is where to look first. A document
        // holds a key once — the scan resolves a duplicate when it parses, and
        // the browser's walk has none to write — so a hit there is *the* value,
        // and the scan below is only for a record that is missing something.
        let count = self.doc.count(node);
        if index < count && self.doc.key_of(node, index) == name {
            return Some(self.doc.child_at(node, index));
        }
        // The last occurrence wins, as `JSON.parse` does. Taking the first was a
        // real bug the self-check caught the first time it ran, on a document
        // with a duplicate key.
        let mut found = None;
        for at in 0..count {
            if self.doc.key_of(node, at) == name {
                found = Some(self.doc.child_at(node, at));
            }
        }
        found
    }

    fn gather_strings(&self, node: u32) -> Vec<&'a [u8]> {
        (0..self.doc.count(node))
            .map(|index| self.text_of(self.doc.child_at(node, index)))
            .collect()
    }

    fn gather_ints(&self, node: u32) -> Vec<i64> {
        (0..self.doc.count(node))
            .map(|index| self.as_int(self.doc.child_at(node, index)))
            .collect()
    }

    /// One column of a table, transposed out of the rows.
    ///
    /// A float column carries its **plain** bit pattern, where a scalar field
    /// carries the byte-reversed one: the reversal exists to move a float's zero
    /// bytes where the integer trim can reach them, and a column has no such
    /// trim to feed.
    fn gather_column(&self, plan: &Plan, index: usize, node: u32, rows: usize) -> Vec<i64> {
        let op = plan.fields[index].op;
        (0..rows)
            .map(|row| {
                let record = self.doc.child_at(node, row);
                let Some(cell) = self.value_of(plan, index, Some(record)) else {
                    return 0;
                };
                if self.doc.kind_of(cell) == K_NULL {
                    return 0;
                }
                match op {
                    OP_FLOAT64 => self.as_float(cell).to_bits() as i64,
                    #[allow(clippy::cast_possible_truncation)]
                    OP_FLOAT32 => i64::from((self.as_float(cell) as f32).to_bits()),
                    _ => self.as_int(cell),
                }
            })
            .collect()
    }

    fn gather_string_column(
        &self,
        plan: &Plan,
        index: usize,
        node: u32,
        rows: usize,
    ) -> Vec<&'a [u8]> {
        (0..rows)
            .map(|row| {
                let record = self.doc.child_at(node, row);
                match self.value_of(plan, index, Some(record)) {
                    Some(cell) => self.text_of(cell),
                    None => &[][..],
                }
            })
            .collect()
    }

    /// A node's string bytes, or nothing when it is not a string. A field whose
    /// op says string and whose value is not one is written as the empty string
    /// rather than refused: the schema is the source of truth, and inference
    /// would have refused the document already if the two really disagreed.
    fn text_of(&self, node: u32) -> &'a [u8] {
        if self.doc.kind_of(node) == K_STRING {
            self.doc.str_of(node)
        } else {
            &[]
        }
    }

    /// A node as an i64, whatever number kind it parsed as.
    #[allow(clippy::cast_possible_truncation)]
    fn as_int(&self, node: u32) -> i64 {
        match self.doc.kind_of(node) {
            K_INT | K_UINT | K_BOOL => self.doc.int_of(node),
            K_FLOAT => self.doc.float_of(node) as i64,
            _ => 0,
        }
    }

    #[allow(clippy::cast_precision_loss)]
    fn as_float(&self, node: u32) -> f64 {
        match self.doc.kind_of(node) {
            K_FLOAT => self.doc.float_of(node),
            K_UINT => self.doc.uint_of(node) as f64,
            K_INT => self.doc.int_of(node) as f64,
            _ => 0.0,
        }
    }

    fn unwritable(&mut self, op: u8) {
        self.diag.fail(
            D_UNSUPPORTED,
            -1,
            "",
            format!("field type {op} is one this encoder does not write"),
        );
    }
}

/// The column codec takes its element width from the Rust type, and the schema
/// states it as a number — so the one place the two meet is this match. An
/// inferred schema only ever reaches widths 1 and 8; the other two are here
/// because a schema that came from Go can name them.
fn write_column_narrow(w: &mut wire::Writer<'_>, key: u8, values: &[i64], width: usize) {
    match width {
        1 => w.column(key, &narrowed::<i8>(values)),
        2 => w.column(key, &narrowed::<i16>(values)),
        4 => w.column(key, &narrowed::<i32>(values)),
        _ => w.column(key, values),
    }
}

fn write_column_wide(w: &mut wire::Writer8<'_>, key: u8, values: &[i64], width: usize) {
    match width {
        1 => w.column(key, &narrowed::<i8>(values)),
        2 => w.column(key, &narrowed::<i16>(values)),
        4 => w.column(key, &narrowed::<i32>(values)),
        _ => w.column(key, values),
    }
}

fn narrowed<T: crate::column::Signed>(values: &[i64]) -> Vec<T> {
    values.iter().map(|&value| T::from_i64(value)).collect()
}

// ---- the whole encode -------------------------------------------------------

/// Wrap the message in its own schema section, so it stands alone. Without it
/// the section is returned beside the message, to be sent once per connection.
pub const SELF_DESCRIBING: u32 = 1;
/// Decode the message and walk it against the input before returning it. See
/// [`crate::verify`] for why this is on by default in every caller that has a
/// choice.
pub const VERIFY: u32 = 2;
/// Offer each string to the packed5 encoding, keeping it where it is smaller.
pub const PACK_STRINGS: u32 = 4;

/// A message and the schema that describes it.
#[derive(Debug, Clone, Default)]
pub struct Encoded {
    /// The message: a root byte, the section when [`SELF_DESCRIBING`] was asked
    /// for, and then the body.
    pub message: Vec<u8>,
    /// The section on its own, for the delivery that sends it once per
    /// connection. Present either way, because a caller that asked for a
    /// standalone message may still want to show the schema.
    pub section: Vec<u8>,
}

/// JSON text in, a colbin message out.
///
/// The whole encode in one call: scan the text exactly, infer a schema from
/// every record before committing to one, write the body from that schema, and —
/// under [`VERIFY`] — read the message back and walk it against the input.
///
/// This is the thing colbin does that needs no type at compile time, and the
/// reason the `encode` feature exists. A Rust service accepting a document from
/// a client wants it for the same reason a browser does.
///
/// Returns `None` on a refusal, with `diag` filled in: the byte offset and the
/// path to whatever was wrong — JSON that does not parse, a number the format
/// cannot hold exactly, one key holding two types, or a shape the wire has no
/// form for. `diag.warnings` is filled in on success too, for the guesses
/// inference had to make.
///
/// `Option` rather than `Result<_, Diag>` because a caller wants those warnings
/// either way, and because it is the shape every other entry point on this side
/// already has.
#[must_use]
pub fn encode(text: &[u8], flags: u32, diag: &mut Diag) -> Option<Encoded> {
    let Some(doc) = crate::json::parse::parse(text, diag) else {
        diag.locate(text);
        return None;
    };
    let Some(inferred) = crate::infer::infer(&doc, diag) else {
        diag.locate(text);
        return None;
    };
    let section = crate::section::build(&inferred.schema);
    write_message(
        &doc,
        inferred.schema,
        inferred.root,
        section,
        text,
        flags,
        diag,
    )
}

/// A document the caller already has as a [`Doc`], and the section it inferred
/// for it, to a colbin message: the encode with the scan and the inference taken
/// out. This is what the browser module's fast path calls, after JavaScript has
/// walked its own objects once, inferred the schema on the way and written the
/// arrays — so no JSON text is built and none is parsed.
///
/// The section is *trusted to describe the document* in the way inference's own
/// output is: the body is written from it, and a field whose value is not what
/// the section says is written as that type's zero rather than refused. What is
/// not trusted is its syntax, which [`crate::section::parse`] checks.
///
/// The caller owns the oracle: the text path is what a document this refuses, or
/// one it is unsure of, goes through, and its output is what this must equal.
#[must_use]
pub fn encode_doc(doc: &Doc, section: Vec<u8>, flags: u32, diag: &mut Diag) -> Option<Encoded> {
    let schema = match crate::section::parse(&section) {
        Ok(schema) => schema,
        Err(err) => {
            diag.fail(
                crate::diag::D_CORRUPT,
                -1,
                "",
                format!("the fast path wrote a section it cannot read: {err}"),
            );
            return None;
        }
    };
    write_message(doc, schema, doc.root(), section, &[], flags, diag)
}

/// What [`encode_blob`] reads: eight little-endian `u32`s —
/// `nodes, children, text bytes, section bytes, root, 0, 0, 0` — and then, packed
/// with no padding between them, the arrays of a [`Doc`] in this order: `kind`
/// (1 byte a node), `num` (8), `a` (4), `b` (4), `child` (4 a child), `key_a` (4),
/// `key_b` (4), the text, and the section.
pub const BLOB_HEADER: usize = 32;

/// [`encode_doc`] over one buffer, which is what crosses the wasm boundary: the
/// caller writes the arrays where it has room for them and the module reads them
/// out, checking every length against the buffer and every index against the
/// arrays ([`Doc::from_parts`]) before it walks anything.
#[must_use]
pub fn encode_blob(blob: &[u8], flags: u32, diag: &mut Diag) -> Option<Encoded> {
    let refuse = |diag: &mut Diag, why: &str| {
        diag.fail(
            crate::diag::D_CORRUPT,
            -1,
            "",
            format!("the fast path's document is malformed: {why}"),
        );
        None
    };
    let word = |at: usize| -> Option<usize> {
        let bytes = blob.get(at..at + 4)?;
        Some(u32::from_le_bytes([bytes[0], bytes[1], bytes[2], bytes[3]]) as usize)
    };
    let (Some(nodes), Some(children), Some(text_len), Some(section_len), Some(root)) =
        (word(0), word(4), word(8), word(12), word(16))
    else {
        return refuse(diag, "the header is short");
    };
    // The arrays' total, in 64 bits so a header claiming four billion nodes cannot
    // wrap a 32-bit sum into something the buffer holds.
    let need = (BLOB_HEADER as u64)
        + nodes as u64 * (1 + 8 + 4 + 4)
        + children as u64 * 12
        + text_len as u64
        + section_len as u64;
    if need != blob.len() as u64 {
        return refuse(diag, "the lengths do not add up to the buffer");
    }
    let mut at = BLOB_HEADER;
    let mut take = |len: usize| {
        let part = &blob[at..at + len];
        at += len;
        part
    };
    let kind = take(nodes).to_vec();
    let num = take(nodes * 8)
        .as_chunks::<8>()
        .0
        .iter()
        .map(|c| u64::from_le_bytes(*c))
        .collect();
    let mut words = |len: usize| -> Vec<u32> {
        take(len * 4)
            .as_chunks::<4>()
            .0
            .iter()
            .map(|c| u32::from_le_bytes(*c))
            .collect()
    };
    let a = words(nodes);
    let b = words(nodes);
    let child = words(children);
    let key_a = words(children);
    let key_b = words(children);
    let text = take(text_len).to_vec();
    let section = take(section_len).to_vec();
    let Some(doc) = Doc::from_parts(crate::json::parse::DocParts {
        kind,
        num,
        a,
        b,
        child,
        key_a,
        key_b,
        text,
        root: root as u32,
    }) else {
        return refuse(diag, "an index points outside it");
    };
    encode_doc(&doc, section, flags, diag)
}

/// The body, behind the root byte and the section when asked for, and the
/// self-check under [`VERIFY`]: what both entry points end in.
fn write_message(
    doc: &Doc,
    schema: Schema,
    root: u32,
    section: Vec<u8>,
    text: &[u8],
    flags: u32,
    diag: &mut Diag,
) -> Option<Encoded> {
    let self_describing = flags & SELF_DESCRIBING != 0;

    let mut message = Vec::with_capacity(text.len().max(1024));
    let mut builder = Builder::new(doc, &schema, diag);
    builder.pack_strings = flags & PACK_STRINGS != 0;
    if self_describing {
        message.push(if schema.root().is_wide {
            ROOT_STRUCT_WIDE | ROOT_SCHEMA
        } else {
            ROOT_STRUCT_NARROW | ROOT_SCHEMA
        });
        message.extend_from_slice(&section);
        builder.run(&mut message, 0, root);
    } else {
        builder.build(&mut message, root, false);
    }
    if !diag.ok() {
        diag.locate(text);
        return None;
    }

    if flags & VERIFY != 0 {
        // Through the same decoder any other reader would use, and against the
        // section that travelled with it — so a section that describes the wrong
        // body fails here rather than at whoever reads it next.
        let split = match crate::walk::split_root(&message) {
            Ok(split) => split,
            Err(err) => {
                diag.fail(
                    crate::diag::D_CORRUPT,
                    -1,
                    "",
                    format!("the encoder wrote a section it cannot read back: {err}"),
                );
                return None;
            }
        };
        let wide = split.root & 0x08 != 0;
        let schema = split.schema.unwrap_or(schema);
        let body = split.body;

        match crate::walk::to_json(&schema, body, wide) {
            Ok(json) => {
                if !crate::verify::verify(doc, root, &json, diag) {
                    return None;
                }
            }
            Err(err) => {
                diag.fail(
                    crate::diag::D_CORRUPT,
                    -1,
                    "",
                    format!("the encoder wrote a message it cannot read back: {err}"),
                );
                return None;
            }
        }
    }

    Some(Encoded { message, section })
}
