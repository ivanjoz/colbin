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

    /// The root byte and then the run.
    pub fn build(&mut self, out: &mut Vec<u8>, node: u32, with_schema: bool) {
        let mut root = if self.schema.root().is_wide {
            ROOT_STRUCT_WIDE
        } else {
            ROOT_STRUCT_NARROW
        };
        if with_schema {
            root |= ROOT_SCHEMA;
        }
        out.push(root);
        self.run(out, 0, node);
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
            OP_INT64S | OP_UINT64S => {
                let values = self.gather_ints(node);
                // Always through the signed shape, for both ops: a `[]uint64`
                // past 2^63 reads as negative through an i64 and must still
                // travel as two's complement, which is lossless and is what Go
                // does.
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
                    w.zero_signed(key);
                } else {
                    w.i64(key, value);
                }
            }
            OP_FLOAT64 | OP_FLOAT32 => {
                let value = self.as_float(node);
                // A float rides the unsigned shape, so its explicit zero is the
                // unsigned one: reading it back goes through `u64` either way.
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

    fn narrow_structs(&mut self, w: &mut wire::Writer<'_>, field: &PlanField, node: u32) {
        let Some(sub) = field.sub else { return };
        let rows = self.doc.count(node);
        if rows == 0 {
            return;
        }
        let element = self.schema.plan(sub);
        if element.is_some_and(|plan| plan.can_table) && rows >= TABLE_THRESHOLD {
            let mark = w.open_table(field.key, rows);
            self.narrow_columns(w, sub, node, rows);
            w.close(mark);
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
        // The last occurrence wins, as `JSON.parse` does. Taking the first was a
        // real bug the self-check caught the first time it ran, on a document
        // with a duplicate key.
        let mut found = None;
        for at in 0..self.doc.count(node) {
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
    let self_describing = flags & SELF_DESCRIBING != 0;

    let mut message = Vec::with_capacity(text.len());
    let mut builder = Builder::new(&doc, &inferred.schema, diag);
    builder.pack_strings = flags & PACK_STRINGS != 0;
    if self_describing {
        message.push(if inferred.schema.root().is_wide {
            ROOT_STRUCT_WIDE | ROOT_SCHEMA
        } else {
            ROOT_STRUCT_NARROW | ROOT_SCHEMA
        });
        message.extend_from_slice(&section);
        builder.run(&mut message, 0, inferred.root);
    } else {
        builder.build(&mut message, inferred.root, false);
    }
    if !diag.ok() {
        diag.locate(text);
        return None;
    }

    if flags & VERIFY != 0 {
        // Through the same decoder any other reader would use, and against the
        // section that travelled with it — so a section that describes the wrong
        // body fails here rather than at whoever reads it next.
        let (schema, body, wide) = if self_describing {
            match crate::section::parse(&message[1..]) {
                Ok(schema) => {
                    let body = 1 + schema.size;
                    let wide = message[0] & 0x08 != 0;
                    (schema, body, wide)
                }
                Err(err) => {
                    diag.fail(
                        crate::diag::D_CORRUPT,
                        -1,
                        "",
                        format!("the encoder wrote a section it cannot read back: {err}"),
                    );
                    return None;
                }
            }
        } else {
            (inferred.schema, 1, message[0] & 0x08 != 0)
        };

        match crate::walk::to_json(&schema, &message[body..], wide) {
            Ok(json) => {
                if !crate::verify::verify(&doc, inferred.root, &json, diag) {
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
