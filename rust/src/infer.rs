//! Inference (`rust/ENCODER.md` §1–§2) and enforcement (§4), producing a schema.
//!
//! Two passes, and the split is load-bearing. The first observes every record
//! without deciding anything; the second resolves each observation into an op
//! and refuses the ones that contradict themselves. A schema decided from record
//! 0 that record 500 contradicts is precisely how an encoder emits a body its
//! own schema does not describe (§5, rule 1), so nothing here commits to a type
//! until every record has been seen.
//!
//! # Field ids are sequential, in first-seen order
//!
//! `ENCODER.md` §3. Go derives an untagged field's id from `fnv8` of
//! its name and probes past collisions, which lands anywhere in 0..255 and so
//! puts every message on eight-bit keys. Numbering 0, 1, 2 instead puts any
//! object of sixteen fields or fewer on the four-bit fast path, which is a byte
//! per field smaller. The section carries the ids, so nothing that reads the
//! section can be confused by it.
//!
//! First-seen order stays exactly as load-bearing as it was, for a different
//! reason: it is no longer the input to a hash, it *is* the id.
//!
//! # The root is a struct, and JSON's top level often is not
//!
//! `ENCODER.md` §1. A colbin message is a struct and nothing else,
//! so an array or a bare scalar at the top level is wrapped in a one-field
//! envelope — the same one `codec/envelope.go` puts round a Go slice, down to
//! the field being called `rows`. It is marked in the section rather than
//! guessed at on the way back.
//!
//! # What the format does not carry
//!
//! An array of floats, an array of bools and an array of arrays have no op. That
//! is the format's limit rather than this module's: `sliceOp` in
//! `codec/codec.go` resolves integers, strings and structs and refuses the rest.
//! They are refused here with a message that says so, because the alternative is
//! a message that does not decode.

use alloc::format;
use alloc::string::{String, ToString};
use alloc::vec::Vec;

use crate::diag::{D_CONFLICT, D_LIMIT, D_UNSUPPORTED, Diag};
use crate::json::parse::{
    Doc, K_ARRAY, K_BOOL, K_FLOAT, K_INT, K_NULL, K_OBJECT, K_STRING, K_UINT,
};
use crate::plan::{
    OP_BOOL, OP_FLOAT64, OP_INT64, OP_INT64S, OP_POINTER, OP_STRING, OP_STRINGS, OP_STRUCT,
    OP_STRUCTS, OP_UINT64, OP_UINT64S, Plan, PlanField, columnable_op,
};
use crate::section::Schema;

/// What eight key bits buy, and therefore the most fields one object may hold.
const MAX_FIELDS: usize = 256;

/// The name the envelope's single field takes. It is only ever built when the
/// top level is *not* an object, so it can never collide with a caller's key —
/// and it is the name `codec/envelope.go` uses, because two ports writing the
/// same document have no business writing different bytes for it.
pub const ENVELOPE_FIELD: &str = "rows";

// Value categories. Two different categories in one position is a conflict; the
// numeric one absorbs int, uint and float internally because those unify.
const C_BOOL: usize = 0;
const C_NUM: usize = 1;
const C_STR: usize = 2;
const C_ARR: usize = 3;
const C_OBJ: usize = 4;
const C_COUNT: usize = 5;

const CATEGORY_NAMES: [&str; C_COUNT] = ["bool", "number", "string", "array", "object"];

/// The outcome of [`Inferrer::category_of`]: the one category this position was
/// observed to hold, or why there is not one.
enum Category {
    /// Exactly one category, and which.
    One(usize),
    /// The position was only ever null, so nothing says what it is.
    OnlyNull,
    /// Two categories in one position. The diagnostic is already set.
    Conflict,
}

/// What one position in the shape was observed to hold, across every record.
///
/// Held in an arena rather than as an owning tree: a field's observation can
/// hold another, and a flat table is what keeps that expressible without
/// `Box`-per-node and re-borrow trouble in the two passes that walk it.
#[derive(Debug, Default)]
struct Obs {
    saw_null: bool,
    saw_neg: bool,
    saw_uint: bool,
    saw_float: bool,

    /// First record and source offset each category appeared at, or `-1`.
    cat_rec: [i32; C_COUNT],
    cat_off: [i32; C_COUNT],

    /// The observation for this array's elements, once there has been one.
    elem: Option<u32>,
    /// Field observations, in first-seen order. Index into the arena, with the
    /// name beside it.
    fields: Vec<FieldObs>,
    /// Objects merged into this position; a field short of it is nullable.
    objects: u32,
}

impl Obs {
    fn new() -> Self {
        Self {
            cat_rec: [-1; C_COUNT],
            cat_off: [-1; C_COUNT],
            ..Self::default()
        }
    }

    #[inline]
    fn note(&mut self, category: usize, record: i32, offset: i32) {
        if self.cat_rec[category] < 0 {
            self.cat_rec[category] = record;
            self.cat_off[category] = offset;
        }
    }
}

#[derive(Debug)]
struct FieldObs {
    name: Vec<u8>,
    obs: u32,
    present: u32,
}

/// An inferred schema: the struct table a section is written from, and the
/// document node the root describes.
#[derive(Debug)]
pub struct Inferred {
    pub schema: Schema,
    /// The document node the root plan describes. For an envelope this is the
    /// top-level value; otherwise it is the object itself.
    pub root: u32,
}

struct Inferrer<'a, 'd> {
    doc: &'a Doc,
    diag: &'d mut Diag,
    /// The observation arena. Index 0 is never special; children hold indices.
    obs: Vec<Obs>,
    /// The struct table being built, in the order a section writes it.
    plans: Vec<Plan>,
    path: Vec<String>,
    record: i32,
}

impl<'a, 'd> Inferrer<'a, 'd> {
    fn new(doc: &'a Doc, diag: &'d mut Diag) -> Self {
        Self {
            doc,
            diag,
            obs: Vec::new(),
            plans: Vec::new(),
            path: Vec::new(),
            record: 0,
        }
    }

    fn new_obs(&mut self) -> u32 {
        self.obs.push(Obs::new());
        (self.obs.len() - 1) as u32
    }

    fn path_string(&self) -> String {
        self.path.concat()
    }

    fn infer(mut self) -> Option<Inferred> {
        let root = self.doc.root();
        let kind = self.doc.kind_of(root);

        if kind == K_NULL {
            let at = self.doc.offset_of(root);
            self.diag
                .fail(D_CONFLICT, at, "", "null has no shape to infer");
            return None;
        }
        if kind == K_ARRAY && self.doc.count(root) == 0 {
            let at = self.doc.offset_of(root);
            self.diag
                .fail(D_CONFLICT, at, "", "an empty array has no shape to infer");
            return None;
        }

        if kind == K_OBJECT {
            // The ordinary case: the top level is the record, and the root plan
            // is its struct. No envelope, and nothing to unwrap on the way back.
            let obs = self.new_obs();
            self.record = 0;
            self.path.clear();
            self.observe(obs, root);
            if !self.diag.ok() {
                return None;
            }
            self.path.clear();
            self.resolve_struct(obs)?;
            return Some(Inferred {
                schema: Schema {
                    plans: self.plans,
                    size: 0,
                },
                root,
            });
        }

        // Everything else goes in an envelope, because the root of a colbin
        // message is a struct and nothing else.
        let obs = self.new_obs();
        if kind == K_ARRAY {
            // Each element is a record, so a conflict names the record it
            // appeared in rather than "inside the array".
            let count = self.doc.count(root);
            for index in 0..count {
                self.record = index as i32;
                self.path.clear();
                self.path.push(format!("[{index}]"));
                let elem = match self.obs[obs as usize].elem {
                    Some(at) => at,
                    None => self.make_elem(obs),
                };
                let child = self.doc.child_at(root, index);
                self.observe(elem, child);
                if !self.diag.ok() {
                    return None;
                }
            }
            let at = self.doc.offset_of(root);
            self.obs[obs as usize].note(C_ARR, 0, at);
        } else {
            self.record = 0;
            self.path.clear();
            self.observe(obs, root);
            if !self.diag.ok() {
                return None;
            }
        }

        self.path.clear();
        // The envelope takes index 0, reserved before its one field is resolved
        // so that a struct behind it lands at index 1 — which is the order a
        // reader's own hoisting assigns and therefore the order the bytes are in.
        self.plans.push(Plan::default());
        let mut field = PlanField {
            key: 0,
            ..PlanField::default()
        };
        let op = self.resolve_op(obs, &mut field)?;
        field.op = op;

        self.plans[0] = Plan::assembled(
            alloc::vec![field],
            alloc::vec![ENVELOPE_FIELD.to_string()],
            true,
        );

        Some(Inferred {
            schema: Schema {
                plans: self.plans,
                size: 0,
            },
            root,
        })
    }

    fn make_elem(&mut self, obs: u32) -> u32 {
        let elem = self.new_obs();
        self.obs[obs as usize].elem = Some(elem);
        elem
    }

    // ---- pass one: observe --------------------------------------------------

    fn observe(&mut self, obs: u32, node: u32) {
        if !self.diag.ok() {
            return;
        }
        let kind = self.doc.kind_of(node);
        let offset = self.doc.offset_of(node);
        let record = self.record;

        match kind {
            K_NULL => self.obs[obs as usize].saw_null = true,
            K_BOOL => self.obs[obs as usize].note(C_BOOL, record, offset),
            K_INT => {
                let held = &mut self.obs[obs as usize];
                held.note(C_NUM, record, offset);
                if self.doc.int_of(node) < 0 {
                    held.saw_neg = true;
                }
            }
            K_UINT => {
                let held = &mut self.obs[obs as usize];
                held.note(C_NUM, record, offset);
                held.saw_uint = true;
            }
            K_FLOAT => {
                let held = &mut self.obs[obs as usize];
                held.note(C_NUM, record, offset);
                held.saw_float = true;
            }
            K_STRING => self.obs[obs as usize].note(C_STR, record, offset),
            K_ARRAY => self.observe_array(obs, node, record, offset),
            _ => self.observe_object(obs, node, record, offset),
        }
    }

    fn observe_array(&mut self, obs: u32, node: u32, record: i32, offset: i32) {
        self.obs[obs as usize].note(C_ARR, record, offset);
        let count = self.doc.count(node);
        // Created only when there is an element to observe: a field that is
        // always an empty array must stay distinguishable from one that is
        // always null.
        if count > 0 && self.obs[obs as usize].elem.is_none() {
            self.make_elem(obs);
        }
        let Some(elem) = self.obs[obs as usize].elem else {
            return;
        };
        for index in 0..count {
            self.path.push(format!("[{index}]"));
            let child = self.doc.child_at(node, index);
            self.observe(elem, child);
            self.path.pop();
            if !self.diag.ok() {
                return;
            }
        }
    }

    fn observe_object(&mut self, obs: u32, node: u32, record: i32, offset: i32) {
        {
            let held = &mut self.obs[obs as usize];
            held.note(C_OBJ, record, offset);
            held.objects += 1;
        }
        let count = self.doc.count(node);
        for index in 0..count {
            let key = self.doc.key_of(node, index);
            let mut slot = self.obs[obs as usize]
                .fields
                .iter()
                .position(|field| field.name == key);
            if slot.is_none() {
                if self.obs[obs as usize].fields.len() >= MAX_FIELDS {
                    let path = self.path_string();
                    self.diag.fail(
                        D_LIMIT,
                        offset,
                        &path,
                        format!(
                            "an object with more than {MAX_FIELDS} fields cannot be encoded: \
                             a key is one byte"
                        ),
                    );
                    return;
                }
                let name = key.to_vec();
                let child_obs = self.new_obs();
                let held = &mut self.obs[obs as usize];
                held.fields.push(FieldObs {
                    name,
                    obs: child_obs,
                    present: 0,
                });
                slot = Some(held.fields.len() - 1);
            }
            let slot = slot.unwrap_or(0);

            let (field_obs, name) = {
                let held = &mut self.obs[obs as usize];
                let objects = held.objects;
                let field = &mut held.fields[slot];
                // A duplicate key overwrites, as `JSON.parse` does, so the count
                // still reflects records rather than occurrences.
                if field.present < objects {
                    field.present = objects;
                }
                (field.obs, field.name.clone())
            };
            self.path
                .push(format!(".{}", String::from_utf8_lossy(&name)));
            let child = self.doc.child_at(node, index);
            self.observe(field_obs, child);
            self.path.pop();
            if !self.diag.ok() {
                return;
            }
        }
    }

    // ---- pass two: resolve --------------------------------------------------

    /// The incumbent category, or why there is not one.
    fn category_of(&mut self, obs: u32) -> Category {
        let seen = self.obs[obs as usize]
            .cat_rec
            .iter()
            .filter(|&&at| at >= 0)
            .count();
        if seen > 1 {
            self.report_conflict(obs);
            return Category::Conflict;
        }
        if seen == 0 {
            return Category::OnlyNull;
        }
        let cat_rec = self.obs[obs as usize].cat_rec;
        let mut winner = 0;
        let mut best = i32::MAX;
        for (category, &at) in cat_rec.iter().enumerate() {
            if at >= 0 && at < best {
                best = at;
                winner = category;
            }
        }
        Category::One(winner)
    }

    /// One observation into an op, filling `field` with whatever the op does not
    /// say by itself.
    fn resolve_op(&mut self, obs: u32, field: &mut PlanField) -> Option<u8> {
        let category = match self.category_of(obs) {
            Category::Conflict => return None,
            Category::OnlyNull => {
                let path = self.path_string();
                self.diag.warn(format!(
                    "{path} was only ever null; encoded as a nullable string"
                ));
                field.elem_op = OP_STRING;
                return Some(OP_POINTER);
            }
            Category::One(category) => category,
        };

        if category == C_OBJ {
            let sub = self.resolve_struct(obs)?;
            field.sub = Some(sub);
            // A null where an object belongs cannot be a pointer — the format
            // refuses a pointer to a composite — so it decodes back as an object
            // of zeros.
            if self.obs[obs as usize].saw_null {
                let path = self.path_string();
                self.diag.warn(format!(
                    "{path} is sometimes null; a null object decodes as an object of zeros"
                ));
            }
            return Some(OP_STRUCT);
        }

        if category == C_ARR {
            return self.resolve_array(obs, field);
        }

        let op = match category {
            C_BOOL => OP_BOOL,
            C_NUM => self.resolve_number(obs)?,
            _ => OP_STRING,
        };

        // A null, or a key absent from some record, makes the column a pointer:
        // one that is nil is omitted and costs nothing, and one pointing at a
        // zero writes an explicit zero so the two stay distinguishable.
        if self.obs[obs as usize].saw_null {
            field.elem_op = op;
            return Some(OP_POINTER);
        }
        Some(op)
    }

    fn resolve_number(&mut self, obs: u32) -> Option<u8> {
        let held = &self.obs[obs as usize];
        let (saw_float, saw_uint, saw_neg) = (held.saw_float, held.saw_uint, held.saw_neg);
        if saw_float {
            if saw_uint {
                let path = self.path_string();
                self.diag.warn(format!(
                    "{path} mixes integers above 2^63 with floats; \
                     the column is float64 and those integers lose precision"
                ));
            }
            return Some(OP_FLOAT64);
        }
        if saw_uint {
            if saw_neg {
                let held = &self.obs[obs as usize];
                let (offset, record) = (held.cat_off[C_NUM], held.cat_rec[C_NUM]);
                let path = format!("[{record}]{}", self.path_string());
                self.diag.fail(
                    D_CONFLICT,
                    offset,
                    &path,
                    "values span both below zero and above the int64 maximum, \
                     so no single integer column holds them exactly",
                );
                return None;
            }
            return Some(OP_UINT64);
        }
        Some(OP_INT64)
    }

    fn resolve_array(&mut self, obs: u32, field: &mut PlanField) -> Option<u8> {
        let Some(elem) = self.obs[obs as usize].elem else {
            let path = self.path_string();
            self.diag.warn(format!(
                "{path} was always an empty array; the element type is taken as string"
            ));
            return Some(OP_STRINGS);
        };
        let category = match self.category_of(elem) {
            Category::Conflict => return None,
            Category::OnlyNull => {
                let path = self.path_string();
                self.diag.warn(format!(
                    "{path} holds only nulls; the element type is taken as string"
                ));
                return Some(OP_STRINGS);
            }
            Category::One(category) => category,
        };
        match category {
            C_STR => Some(OP_STRINGS),
            C_OBJ => {
                let sub = self.resolve_struct(elem)?;
                field.sub = Some(sub);
                Some(OP_STRUCTS)
            }
            C_NUM => {
                let numeric = self.resolve_number(elem)?;
                match numeric {
                    OP_FLOAT64 => self.unsupported("an array of floats"),
                    OP_UINT64 => Some(OP_UINT64S),
                    _ => Some(OP_INT64S),
                }
            }
            C_BOOL => self.unsupported("an array of booleans"),
            _ => self.unsupported("an array of arrays"),
        }
    }

    /// Every op the format carries as an array element is an integer, a string
    /// or a struct. The rest are refused here rather than encoded into something
    /// that does not decode.
    fn unsupported(&mut self, what: &str) -> Option<u8> {
        let path = self.path_string();
        self.diag.fail(
            D_UNSUPPORTED,
            -1,
            &path,
            format!(
                "{what} has no form on the wire: the format carries arrays of integers, \
                 of strings and of objects"
            ),
        );
        None
    }

    /// Resolves an observed object into a plan, appends it to the struct table
    /// and returns its index.
    ///
    /// The index is taken *before* the fields are walked, so a struct reached
    /// from a field lands after the one that reaches it. That pre-order is what
    /// a reader's own hoisting assigns, and therefore what makes a section
    /// written here the same bytes as one written for the same shape elsewhere.
    fn resolve_struct(&mut self, obs: u32) -> Option<u32> {
        let me = self.plans.len() as u32;
        self.plans.push(Plan::default());

        let count = self.obs[obs as usize].fields.len();
        let objects = self.obs[obs as usize].objects;
        let mut fields = Vec::with_capacity(count);
        let mut names = Vec::with_capacity(count);

        for index in 0..count {
            let (field_obs, present, name) = {
                let observed = &self.obs[obs as usize].fields[index];
                (observed.obs, observed.present, observed.name.clone())
            };
            self.path
                .push(format!(".{}", String::from_utf8_lossy(&name)));
            let mut field = PlanField {
                // Sequential, in first-seen order. See the header.
                key: index as u8,
                ..PlanField::default()
            };
            let op = self.resolve_op(field_obs, &mut field);
            self.path.pop();
            field.op = op?;

            // A key absent from some record is indistinguishable from an
            // explicit null on the wire (`ENCODER.md` §6), and both make the
            // column a pointer — where the format allows one.
            if present < objects && field.op != OP_POINTER && pointable(field.op) {
                field.elem_op = field.op;
                field.op = OP_POINTER;
            }
            fields.push(field);
            names.push(String::from_utf8_lossy(&name).into_owned());
        }

        if fields.is_empty() {
            let path = self.path_string();
            self.diag.fail(
                D_CONFLICT,
                -1,
                &path,
                "an object with no fields has nothing to encode",
            );
            return None;
        }

        // The key width is exactly "more than sixteen fields". packed5 used to
        // be the other thing that forced eight-bit keys and it is a per-field
        // code in the descriptor now, so an object of sixteen keys or fewer
        // always takes the four-bit fast path.
        self.plans[me as usize] = Plan::assembled(fields, names, false);
        Some(me)
    }

    fn report_conflict(&mut self, obs: u32) {
        // The incumbent is whatever appeared first; the offender is the newcomer.
        let cat_rec = self.obs[obs as usize].cat_rec;
        let cat_off = self.obs[obs as usize].cat_off;
        let mut first = 0usize;
        let mut last = 0usize;
        let (mut lowest, mut highest) = (i32::MAX, i32::MIN);
        for (category, &at) in cat_rec.iter().enumerate() {
            if at < 0 {
                continue;
            }
            if at < lowest {
                lowest = at;
                first = category;
            }
            if at > highest {
                highest = at;
                last = category;
            }
        }
        let (first_record, last_record) = (cat_rec[first], cat_rec[last]);
        let where_ = if last_record - first_record > 1 {
            format!("records {first_record}-{}", last_record - 1)
        } else {
            format!("record {first_record}")
        };
        let path = format!("[{last_record}]{}", self.path_string());
        self.diag.fail(
            D_CONFLICT,
            cat_off[last],
            &path,
            format!(
                "type conflict: {}, but {where_} had {}",
                CATEGORY_NAMES[last], CATEGORY_NAMES[first],
            ),
        );
    }
}

/// Whether an op can sit behind a pointer. The format refuses a pointer to a
/// composite: those carry a length already, and what a nil one should mean is
/// not settled.
#[inline]
const fn pointable(op: u8) -> bool {
    columnable_op(op)
}

/// Infers a schema for a parsed document, or returns `None` with `diag` set.
#[must_use]
pub fn infer(doc: &Doc, diag: &mut Diag) -> Option<Inferred> {
    Inferrer::new(doc, diag).infer()
}
