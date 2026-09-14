//! A schema-driven walk of a message, into JSON text.
//!
//! Mirrors `codec/json.go`. The walk is what a reader
//! does when it has bytes and a section and no Rust type: the section says what
//! each key holds, and this steps the message against it.
//!
//! # An absent key is a zero value
//!
//! colbin writes nothing for a field holding its zero, so a record that wrote
//! three of its nine fields still has nine. Putting them back is not an
//! afterthought — a reader that printed three would be printing a different
//! record.
//!
//! Those recovered fields are written **after** the ones the message carried,
//! which is what `codec/json.go` does and therefore what this does. It means
//! colbin's JSON is not byte-identical to the JSON a document came from when a
//! zero-valued field was not last. JSON objects are unordered, all three
//! implementations agree, and the cross-language test compares parsed values for
//! exactly this reason.

use alloc::vec::Vec;

use crate::Error;
use crate::json::{JsonSink, render_key_run};
use crate::plan::{
    self, MAP_ANY, MAP_BOOL, MAP_FLOAT32, MAP_FLOAT64, MAP_INT, MAP_STRING, MAP_UINT, OP_ANY,
    OP_ANYS, OP_BOOL, OP_BYTES, OP_FLOAT32, OP_FLOAT64, OP_INT8, OP_INT64, OP_MAP, OP_POINTER,
    OP_STRING, OP_STRINGS, OP_STRUCT, OP_STRUCTS, OP_UINT8, OP_UINT64, Plan, PlanField,
};
use crate::section::Schema;
use crate::wire::{Kind, Reader, Reader8};

/// Whether a map kind is one the format accepts as a key.
///
/// A float and a bool are refused at plan time, so only a section from somewhere
/// else can name one — and a key that does not consume its bytes would misread
/// every entry after it, so this is checked once before the loop rather than
/// defaulted inside it.
const fn map_key_is_renderable(kind: u8) -> bool {
    matches!(kind, MAP_STRING | MAP_INT | MAP_UINT)
}

const MAX_DEPTH: u32 = 128;

/// The most rows a table may claim.
///
/// This is the one place a message's own number decides an allocation. It cannot
/// be derived from the bytes left, and that is worth stating rather than hiding:
/// a constant column is nine bytes for any length, so a legitimate table of a
/// million identical rows really does fit in a handful of bytes. The bound is a
/// budget, not a proof — four million rows is 32 MB per integer column, already
/// past what a browser tab should be handed.
pub(crate) const MAX_ROWS: usize = 1 << 22;

/// Renders a message as the JSON `encoding/json` would have written for it.
///
/// # Errors
///
/// Any malformed message, and any message the schema does not describe.
pub fn to_json(schema: &Schema, message: &[u8], wide: bool) -> Result<Vec<u8>, Error> {
    let mut walker = Walker::new(schema, message.len());
    walker.root(0, message, wide)?;
    Ok(walker.to.finish())
}

/// The walk, and the one buffer it renders into.
pub struct Walker<'s> {
    schema: &'s Schema,
    /// `"name":` per field per plan, rendered once. A field then costs one copy
    /// per row rather than an escape pass — which on a thousand-record table is
    /// the difference between one escape and seven thousand.
    key_runs: Vec<Vec<Vec<u8>>>,
    to: JsonSink,
    depth: u32,
}

impl<'s> Walker<'s> {
    #[must_use]
    pub fn new(schema: &'s Schema, message_len: usize) -> Self {
        let key_runs = schema
            .plans
            .iter()
            .map(|plan| {
                plan.names
                    .iter()
                    .map(|name| render_key_run(name.as_bytes()))
                    .collect()
            })
            .collect();
        Self {
            schema,
            key_runs,
            to: JsonSink::with_message_len(message_len),
            depth: 0,
        }
    }

    /// The rendered bytes, once a walk has finished.
    #[must_use]
    pub fn finish(self) -> Vec<u8> {
        self.to.finish()
    }

    fn enter(&mut self) -> Result<(), Error> {
        if self.depth >= MAX_DEPTH {
            return Err(Error::TooDeep);
        }
        self.depth += 1;
        Ok(())
    }

    fn leave(&mut self) {
        self.depth -= 1;
    }

    fn plan_at(&self, at: u32) -> Result<&'s Plan, Error> {
        self.schema.plan(at).ok_or(Error::BadSection)
    }

    /// Writes a precomputed `"name":`.
    ///
    /// The two fields are reached directly rather than through a `key_run`
    /// accessor, because a method call borrows the whole of `self` and the sink
    /// is about to be borrowed mutably. Disjoint field borrows inside one body
    /// are fine; a getter would not be.
    #[inline]
    fn write_key(&mut self, plan_at: u32, field: usize) {
        let run = &self.key_runs[plan_at as usize][field];
        self.to.key_run(run);
    }

    /// The root run, which is the only place an envelope can be.
    ///
    /// A document whose top level was not an object was wrapped in a one-field
    /// struct, because the root of a colbin message is a struct and nothing
    /// else. The section says so, so unwrapping it is reading a fact rather than
    /// guessing at a field name. Go builds the same wrapper round a slice or map
    /// root and unwraps it the same way; a reader that does not know the flag
    /// renders `{"rows":[…]}` and is not wrong, only more literal.
    ///
    /// # Errors
    ///
    /// Any malformed message.
    pub fn root(&mut self, plan_at: u32, body: &[u8], wide: bool) -> Result<(), Error> {
        let plan = self.plan_at(plan_at)?;
        if !plan.is_envelope || plan.fields.len() != 1 {
            return self.message(plan_at, body, wide);
        }
        self.enter()?;
        let field = plan.fields[0].clone();
        if wide {
            let mut reader = Reader8::new(body);
            if reader.more() && reader.key() == field.key {
                self.wide_value(&mut reader, &field)?;
            } else {
                self.zero(&field)?;
            }
            reader.err()?;
        } else {
            let mut reader = Reader::new(body);
            if reader.more() && reader.key() == field.key {
                self.narrow_value(&mut reader, &field)?;
            } else {
                self.zero(&field)?;
            }
            reader.err()?;
        }
        self.leave();
        Ok(())
    }

    /// A whole record, at whichever key width the run declared.
    ///
    /// # Errors
    ///
    /// Any malformed message.
    pub fn message(&mut self, plan_at: u32, body: &[u8], wide: bool) -> Result<(), Error> {
        if wide {
            self.wide_run(plan_at, body)
        } else {
            self.narrow_run(plan_at, body)
        }
    }

    // ---- runs ---------------------------------------------------------------

    fn narrow_run(&mut self, plan_at: u32, body: &[u8]) -> Result<(), Error> {
        self.enter()?;
        let plan = self.plan_at(plan_at)?;
        let mut reader = Reader::new(body);
        let mut seen = alloc::vec![false; plan.fields.len()];
        self.to.begin_object();
        while reader.more() {
            let key = reader.key();
            // Four descriptor bits have no room for a class, so nothing can size
            // a field it cannot classify: an unknown narrow key ends the decode
            // rather than being stepped over.
            let index = plan.field_of(key).ok_or(Error::UnknownKey(key))?;
            seen[index] = true;
            self.write_key(plan_at, index);
            let field = plan.fields[index].clone();
            self.narrow_value(&mut reader, &field)?;
        }
        reader.err()?;
        self.absent(plan_at, &seen)?;
        self.to.end_object();
        self.leave();
        Ok(())
    }

    fn wide_run(&mut self, plan_at: u32, body: &[u8]) -> Result<(), Error> {
        self.enter()?;
        let plan = self.plan_at(plan_at)?;
        let mut reader = Reader8::new(body);
        let mut seen = alloc::vec![false; plan.fields.len()];
        self.to.begin_object();
        while reader.more() {
            let Some(index) = plan.field_of(reader.key()) else {
                // A key the schema does not list is stepped over, which is what
                // the wide width is for.
                reader.skip();
                reader.err()?;
                continue;
            };
            seen[index] = true;
            self.write_key(plan_at, index);
            let field = plan.fields[index].clone();
            self.wide_value(&mut reader, &field)?;
        }
        reader.err()?;
        self.absent(plan_at, &seen)?;
        self.to.end_object();
        self.leave();
        Ok(())
    }

    /// The fields the message left out. See the module comment on ordering.
    fn absent(&mut self, plan_at: u32, seen: &[bool]) -> Result<(), Error> {
        let plan = self.plan_at(plan_at)?;
        for (index, field) in plan.fields.iter().enumerate() {
            if seen.get(index).copied().unwrap_or(false) {
                continue;
            }
            self.write_key(plan_at, index);
            let field = field.clone();
            self.zero(&field)?;
        }
        Ok(())
    }

    /// What an absent key means, per op — which is what `Unmarshal` would have
    /// left in the record, because it zeroes it first.
    fn zero(&mut self, field: &PlanField) -> Result<(), Error> {
        match field.op {
            OP_BOOL => self.to.boolean(false),
            op if (OP_INT8..=OP_INT64).contains(&op) => self.to.signed(0),
            op if (OP_UINT8..=OP_UINT64).contains(&op) => self.to.unsigned(0),
            OP_FLOAT32 => {
                self.to.float(0.0, 32);
            }
            OP_FLOAT64 => {
                self.to.float(0.0, 64);
            }
            OP_STRING => self.to.text_bytes(&[]),
            OP_STRUCT => {
                // A nested struct is always written, empty body or not, so this
                // is unreachable from anything colbin produces. A section from
                // somewhere else may still say it, and an object of zeros is
                // what the field would hold.
                return self.zero_struct(field.sub);
            }
            // A blob, an array, a slice of structs, a map and a pointer are all
            // nil when the key is absent, and a nil one of those is null.
            _ => self.to.null(),
        }
        Ok(())
    }

    fn zero_struct(&mut self, sub: Option<u32>) -> Result<(), Error> {
        let Some(at) = sub else {
            self.to.null();
            return Ok(());
        };
        self.enter()?;
        let plan = self.plan_at(at)?;
        let none = alloc::vec![false; plan.fields.len()];
        self.to.begin_object();
        self.absent(at, &none)?;
        self.to.end_object();
        self.leave();
        Ok(())
    }

    // ---- one field, each width ----------------------------------------------
    //
    // The composites are here and the values are in *_scalar, because a pointer
    // field's payload *is* a value — the op it points at — and the two matches
    // would otherwise be one match written twice.

    fn narrow_value(&mut self, reader: &mut Reader<'_>, field: &PlanField) -> Result<(), Error> {
        match field.op {
            OP_STRUCT => {
                let (body, wide) = reader.struct_body().ok_or(Error::Truncated)?;
                let at = field.sub.ok_or(Error::BadSection)?;
                self.message(at, body, wide)
            }
            OP_STRUCTS => {
                if reader.is_table() {
                    self.narrow_table(reader, field)
                } else {
                    self.narrow_list(reader, field)
                }
            }
            OP_MAP => Err(Error::Unsupported),
            OP_POINTER => self.narrow_scalar(reader, field.elem_op),
            op => self.narrow_scalar(reader, op),
        }
    }

    fn narrow_scalar(&mut self, reader: &mut Reader<'_>, op: u8) -> Result<(), Error> {
        match op {
            OP_BOOL => self.to.boolean(reader.bool()),
            o if (OP_INT8..=OP_INT64).contains(&o) => self.to.signed(reader.i64()),
            o if (OP_UINT8..=OP_UINT64).contains(&o) => self.to.unsigned(reader.u64()),
            OP_FLOAT32 => {
                let value = f64::from(reader.f32());
                if !self.to.float(value, 32) {
                    return Err(Error::NotJson);
                }
            }
            OP_FLOAT64 => {
                let value = reader.f64();
                if !self.to.float(value, 64) {
                    return Err(Error::NotJson);
                }
            }
            OP_STRING => {
                let text = reader.packed_string();
                reader.err()?;
                self.to.text_bytes(text.as_bytes());
            }
            OP_BYTES => {
                let bytes = reader.bytes();
                self.to.blob(bytes);
            }
            OP_STRINGS => {
                let values = reader.strings_bytes();
                self.text_array(&values);
            }
            o if plan::array_element_op(o) != plan::OP_COUNT => {
                let mut values: Vec<i64> = Vec::new();
                reader.ints_into(&mut values);
                self.int_array(&values, o);
            }
            o => return Err(Error::UnwalkableOp(o)),
        }
        reader.err()
    }

    fn wide_value(&mut self, reader: &mut Reader8<'_>, field: &PlanField) -> Result<(), Error> {
        match field.op {
            OP_STRUCT => {
                let (body, wide) = reader.struct_body().ok_or(Error::Truncated)?;
                let at = field.sub.ok_or(Error::BadSection)?;
                self.message(at, body, wide)
            }
            OP_STRUCTS => {
                if reader.is_table() {
                    self.wide_table(reader, field)
                } else {
                    self.wide_list(reader, field)
                }
            }
            OP_MAP => self.wide_map(reader, field),
            OP_POINTER => self.wide_scalar(reader, field.elem_op),
            OP_ANY | OP_ANYS => {
                // The key is a field's and everything behind it is a key-less
                // value's, so the cursor steps once and the dynamic walk takes
                // it from there.
                if !reader.payload() {
                    return Err(Error::Truncated);
                }
                self.dynamic_value(reader)
            }
            op => self.wide_scalar(reader, op),
        }
    }

    fn wide_scalar(&mut self, reader: &mut Reader8<'_>, op: u8) -> Result<(), Error> {
        match op {
            OP_BOOL => self.to.boolean(reader.bool()),
            o if (OP_INT8..=OP_INT64).contains(&o) => self.to.signed(reader.i64()),
            o if (OP_UINT8..=OP_UINT64).contains(&o) => self.to.unsigned(reader.u64()),
            OP_FLOAT32 => {
                let value = f64::from(reader.f32());
                if !self.to.float(value, 32) {
                    return Err(Error::NotJson);
                }
            }
            OP_FLOAT64 => {
                let value = reader.f64();
                if !self.to.float(value, 64) {
                    return Err(Error::NotJson);
                }
            }
            OP_STRING => {
                let text = reader.packed_string();
                reader.err()?;
                self.to.text_bytes(text.as_bytes());
            }
            OP_BYTES => {
                let bytes = reader.bytes();
                self.to.blob(bytes);
            }
            OP_STRINGS => {
                let values = reader.strings_bytes();
                self.text_array(&values);
            }
            o if plan::array_element_op(o) != plan::OP_COUNT => {
                let mut values: Vec<i64> = Vec::new();
                reader.ints_into(&mut values);
                self.int_array(&values, o);
            }
            o => return Err(Error::UnwalkableOp(o)),
        }
        reader.err()
    }

    // ---- maps ---------------------------------------------------------------

    /// A map, whose entries are pairs of key-less values: no field ids, no
    /// schema lookup, just the kinds the section named once for all of them.
    ///
    /// There is no narrow twin. A four-bit descriptor has no room for a class,
    /// so a narrow map's entries take their type from the schema — readable, but
    /// a second element codec, and `narrow_value` refuses it until there is a
    /// message that needs it.
    fn wide_map(&mut self, reader: &mut Reader8<'_>, field: &PlanField) -> Result<(), Error> {
        let (count, mut entries) = reader.map().ok_or(Error::Truncated)?;
        if !map_key_is_renderable(field.key_kind) {
            return Err(Error::UnwalkableOp(field.op));
        }
        self.enter()?;
        self.to.begin_object();
        for _ in 0..count {
            match field.key_kind {
                MAP_STRING => {
                    let name = entries.element_blob();
                    entries.err()?;
                    self.to.key_bytes(name);
                }
                MAP_INT => self.number_key(entries.element_int(), true),
                _ => self.number_key(entries.element_uint() as i64, false),
            }
            self.map_value(&mut entries, field.value_kind)?;
            entries.err()?;
        }
        self.to.end_object();
        self.leave();
        Ok(())
    }

    /// One map value, by the kind the section declared for all of them.
    fn map_value(&mut self, entries: &mut Reader8<'_>, kind: u8) -> Result<(), Error> {
        match kind {
            MAP_STRING => {
                let text = entries.element_blob();
                entries.err()?;
                self.to.text_bytes(text);
            }
            MAP_INT => self.to.signed(entries.element_int()),
            MAP_UINT => self.to.unsigned(entries.element_uint()),
            // A map value carries a float the way a scalar field does: the bit
            // pattern with its bytes reversed, so that a round value trims.
            MAP_FLOAT32 => {
                let bits = (entries.element_uint() as u32).swap_bytes();
                if !self.to.float(f64::from(f32::from_bits(bits)), 32) {
                    return Err(Error::NotJson);
                }
            }
            MAP_FLOAT64 => {
                let bits = entries.element_uint().swap_bytes();
                if !self.to.float(f64::from_bits(bits), 64) {
                    return Err(Error::NotJson);
                }
            }
            MAP_BOOL => self.to.boolean(entries.element_uint() == 1),
            // The entries of a `map[string]any`, which say what they are one at a
            // time rather than once in the schema.
            MAP_ANY => return self.dynamic_value(entries),
            other => return Err(Error::UnwalkableOp(other)),
        }
        Ok(())
    }

    /// Renders an integer key as the name JSON needs it to be.
    fn number_key(&mut self, value: i64, signed: bool) {
        let mut digits = Vec::new();
        if signed {
            crate::json::write_i64(&mut digits, value);
        } else {
            crate::json::write_u64(&mut digits, value as u64);
        }
        self.to.key_bytes(&digits);
    }

    // ---- dynamic values -----------------------------------------------------

    /// One value that says what it is.
    ///
    /// The cursor is on the descriptor, which is where a key-less element always
    /// sits. See `wire/dynamic.rs` for what each kind is on the wire.
    fn dynamic_value(&mut self, reader: &mut Reader8<'_>) -> Result<(), Error> {
        match reader.element_kind() {
            Kind::Null => {
                reader.element_null();
                self.to.null();
            }
            Kind::Bool => {
                let value = reader.element_bool();
                self.to.boolean(value);
            }
            Kind::Int => self.dynamic_int(reader),
            Kind::Float64 => {
                let value = reader.element_f64();
                if !self.to.float(value, 64) {
                    return Err(Error::NotJson);
                }
            }
            Kind::Float32 => {
                let value = f64::from(reader.element_f32());
                if !self.to.float(value, 32) {
                    return Err(Error::NotJson);
                }
            }
            Kind::String => {
                let text = reader.element_blob();
                reader.err()?;
                self.to.text_bytes(text);
            }
            Kind::Bytes => {
                let bytes = reader.element_bytes();
                reader.err()?;
                self.to.blob(bytes);
            }
            Kind::List => return self.dynamic_list(reader),
            Kind::Map => return self.dynamic_map(reader),
            Kind::Typed => return self.dynamic_typed(reader),
            // Both are reachable only behind a TYPED tag, which is what says
            // which struct they are. Bare, there is nothing to name their fields
            // with.
            Kind::Struct | Kind::Table => return Err(Error::Unsupported),
            // The reader answers Invalid for a truncated message as well as for
            // a descriptor it does not know, so its own error comes first.
            Kind::Invalid => {
                reader.err()?;
                return Err(Error::BadDescriptor);
            }
        }
        reader.err()
    }

    /// The Go shape an integer comes back as: signed where it fits, unsigned
    /// only past 2^63, which is a value the format carries and `i64` is not
    /// where it fits. JSON spells both the same way.
    fn dynamic_int(&mut self, reader: &mut Reader8<'_>) {
        if reader.element_negative() {
            let value = reader.element_int();
            self.to.signed(value);
            return;
        }
        let value = reader.element_uint();
        if let Ok(signed) = i64::try_from(value) {
            self.to.signed(signed);
        } else {
            self.to.unsigned(value);
        }
    }

    fn dynamic_list(&mut self, reader: &mut Reader8<'_>) -> Result<(), Error> {
        let (count, mut elements) = reader.element_list().ok_or(Error::Truncated)?;
        self.enter()?;
        self.to.begin_array();
        for _ in 0..count {
            self.dynamic_value(&mut elements)?;
        }
        elements.err()?;
        self.to.end_array();
        self.leave();
        Ok(())
    }

    fn dynamic_map(&mut self, reader: &mut Reader8<'_>) -> Result<(), Error> {
        let (count, mut entries) = reader.element_map().ok_or(Error::Truncated)?;
        self.enter()?;
        self.to.begin_object();
        for _ in 0..count {
            self.dynamic_key(&mut entries)?;
            self.dynamic_value(&mut entries)?;
        }
        entries.err()?;
        self.to.end_object();
        self.leave();
        Ok(())
    }

    /// An entry's key, which has to be a name.
    fn dynamic_key(&mut self, entries: &mut Reader8<'_>) -> Result<(), Error> {
        match entries.element_kind() {
            Kind::String => {
                let name = entries.element_blob();
                entries.err()?;
                self.to.key_bytes(name);
            }
            Kind::Int => {
                if entries.element_negative() {
                    let value = entries.element_int();
                    self.number_key(value, true);
                } else {
                    let value = entries.element_uint();
                    self.number_key(value as i64, false);
                }
            }
            _ => return Err(Error::BadDescriptor),
        }
        entries.err()
    }

    /// A value whose type the section names: the rows of an array of records,
    /// which is the shape the dynamic form exists for.
    fn dynamic_typed(&mut self, reader: &mut Reader8<'_>) -> Result<(), Error> {
        let at = reader.element_typed_index().ok_or(Error::Truncated)?;
        // Resolved before the value is read, so a tag naming a struct this
        // section does not hold fails as a bad section rather than as whatever
        // the bytes behind it happened to look like.
        self.plan_at(at)?;
        match reader.element_kind() {
            Kind::Struct => {
                let (body, wide) = reader.element_struct_body().ok_or(Error::Truncated)?;
                self.message(at, body, wide)
            }
            Kind::List => {
                let (count, mut elements) = reader.element_list().ok_or(Error::Truncated)?;
                self.struct_list(at, count, &mut elements)
            }
            Kind::Table => {
                let (rows, mut columns) = reader.element_table().ok_or(Error::Truncated)?;
                self.struct_table(at, rows, &mut columns)
            }
            _ => Err(Error::BadDescriptor),
        }
    }

    // ---- arrays -------------------------------------------------------------

    fn int_array(&mut self, values: &[i64], op: u8) {
        let signed = plan::is_signed_op(plan::array_element_op(op));
        self.to.begin_array();
        for &value in values {
            if signed {
                self.to.signed(value);
            } else {
                self.to.unsigned(value as u64);
            }
        }
        self.to.end_array();
    }

    fn text_array(&mut self, values: &[&[u8]]) {
        self.to.begin_array();
        for value in values {
            self.to.text_bytes(value);
        }
        self.to.end_array();
    }

    // ---- lists of structs, the row-wise shape -------------------------------

    /// A narrow list, whose element is a length and a key run with no descriptor
    /// between them. That missing descriptor is a byte saved per element and the
    /// reason a structDef states its key width: this is the one run on the wire
    /// whose width the wire does not say.
    fn narrow_list(&mut self, reader: &mut Reader<'_>, field: &PlanField) -> Result<(), Error> {
        let at = field.sub.ok_or(Error::BadSection)?;
        let sub_wide = self.plan_at(at)?.is_wide;
        let (count, mut elements) = reader.counted().ok_or(Error::Truncated)?;
        self.enter()?;
        self.to.begin_array();
        for _ in 0..count {
            let element = elements.element().ok_or(Error::Truncated)?;
            self.message(at, element, sub_wide)?;
        }
        elements.err()?;
        self.to.end_array();
        self.leave();
        Ok(())
    }

    /// A wide list, whose elements carry a descriptor of their own — which is
    /// what lets a wide reader step over one it does not understand.
    fn wide_list(&mut self, reader: &mut Reader8<'_>, field: &PlanField) -> Result<(), Error> {
        let at = field.sub.ok_or(Error::BadSection)?;
        let (count, mut elements) = reader.list().ok_or(Error::Truncated)?;
        self.struct_list(at, count, &mut elements)
    }

    /// An opened list of struct elements.
    ///
    /// Split from [`Walker::wide_list`] because a dynamic value reaches the same
    /// run by a different door — a TYPED tag names the plan, where a field's
    /// descriptor comes with one — and the rows behind both doors are byte for
    /// byte the same.
    fn struct_list(
        &mut self,
        at: u32,
        count: usize,
        elements: &mut Reader8<'_>,
    ) -> Result<(), Error> {
        self.enter()?;
        self.to.begin_array();
        for _ in 0..count {
            let (body, wide) = elements.element_struct_body().ok_or(Error::Truncated)?;
            self.message(at, body, wide)?;
        }
        elements.err()?;
        self.to.end_array();
        self.leave();
        Ok(())
    }

    // ---- tables, the transposed shape ---------------------------------------

    fn narrow_table(&mut self, reader: &mut Reader<'_>, field: &PlanField) -> Result<(), Error> {
        let at = field.sub.ok_or(Error::BadSection)?;
        let (rows, mut columns) = reader.counted().ok_or(Error::Truncated)?;
        if rows > MAX_ROWS {
            return Err(Error::TooManyRows);
        }
        let plan = self.plan_at(at)?;
        let mut gathered = Gathered::new(plan.fields.len());
        while columns.more() {
            let key = columns.key();
            let index = plan.field_of(key).ok_or(Error::UnknownKey(key))?;
            let op = plan.fields[index].op;
            gathered.take(&mut columns, index, op, rows);
            columns.err()?;
        }
        columns.err()?;
        self.table_rows(at, &gathered, rows)
    }

    fn wide_table(&mut self, reader: &mut Reader8<'_>, field: &PlanField) -> Result<(), Error> {
        let at = field.sub.ok_or(Error::BadSection)?;
        let (rows, mut columns) = reader.table().ok_or(Error::Truncated)?;
        self.struct_table(at, rows, &mut columns)
    }

    /// An opened table's columns, for the reason [`Walker::struct_list`] is
    /// split out: a dynamic value reaches the same columns through a TYPED tag.
    fn struct_table(
        &mut self,
        at: u32,
        rows: usize,
        columns: &mut Reader8<'_>,
    ) -> Result<(), Error> {
        if rows > MAX_ROWS {
            return Err(Error::TooManyRows);
        }
        let plan = self.plan_at(at)?;
        let mut gathered = Gathered::new(plan.fields.len());
        while columns.more() {
            let Some(index) = plan.field_of(columns.key()) else {
                columns.skip();
                columns.err()?;
                continue;
            };
            let op = plan.fields[index].op;
            gathered.take8(columns, index, op, rows);
            columns.err()?;
        }
        columns.err()?;
        self.table_rows(at, &gathered, rows)
    }

    /// The transpose itself.
    fn table_rows(&mut self, plan_at: u32, gathered: &Gathered, rows: usize) -> Result<(), Error> {
        self.enter()?;
        self.to.begin_array();
        let plan = self.plan_at(plan_at)?;
        for row in 0..rows {
            self.to.begin_object();
            for (index, field) in plan.fields.iter().enumerate() {
                self.write_key(plan_at, index);
                if !gathered.present[index] {
                    // A column whose every value was zero is not written at all,
                    // and its absence is the whole of what says so.
                    let field = field.clone();
                    self.zero(&field)?;
                } else if field.op == OP_STRING {
                    let value = gathered.strings[index].get(row).copied().unwrap_or(&[]);
                    self.to.text_bytes(value);
                } else {
                    // A column shorter than the table's row count is a message
                    // that disagrees with itself; the row still has to be
                    // written, and a zero is what an absent column means
                    // everywhere else here.
                    let raw = gathered.ints[index].get(row).copied().unwrap_or(0);
                    self.column_value(field.op, raw)?;
                }
            }
            self.to.end_object();
        }
        self.to.end_array();
        self.leave();
        Ok(())
    }

    /// One gathered column value, back to its type.
    ///
    /// A column carries a float as its **plain** bit pattern where a scalar
    /// field carries the byte-reversed one: the reversal exists to move a
    /// float's zero bytes where the integer trim can reach them, and a column
    /// has no such trim to feed. Reading either as the other yields a number
    /// rather than an error, which is why they do not share a line of code.
    fn column_value(&mut self, op: u8, raw: i64) -> Result<(), Error> {
        match op {
            OP_BOOL => self.to.boolean(raw == 1),
            o if (OP_INT8..=OP_INT64).contains(&o) => self.to.signed(raw),
            o if (OP_UINT8..=OP_UINT64).contains(&o) => {
                // The column codec stores signed residuals, so a column narrower
                // than 64 bits comes back sign-extended. Truncating to the op's
                // width is what turns that back into the unsigned value the
                // encoder held — the same `uint8(int8(v))` the Go decoder does.
                let width = plan::width_of_op(o);
                let value = if width == 8 {
                    raw as u64
                } else {
                    (raw as u64) & (u64::MAX >> (64 - width * 8))
                };
                self.to.unsigned(value);
            }
            OP_FLOAT32 => {
                let value = f64::from(f32::from_bits(raw as u32));
                if !self.to.float(value, 32) {
                    return Err(Error::NotJson);
                }
            }
            OP_FLOAT64 => {
                let value = f64::from_bits(raw as u64);
                if !self.to.float(value, 64) {
                    return Err(Error::NotJson);
                }
            }
            // Nothing else is columnable, so nothing else can be here.
            o => return Err(Error::UnwalkableOp(o)),
        }
        Ok(())
    }
}

/// The columns of one table, gathered before the transpose.
///
/// `pub(crate)` rather than private: [`crate::materialize`] reuses this
/// gather step verbatim — a table row is only ever a columnable op (see
/// [`plan::columnable_op`]), so the same per-column read serves both sinks.
pub(crate) struct Gathered<'a> {
    pub(crate) present: Vec<bool>,
    pub(crate) ints: Vec<Vec<i64>>,
    pub(crate) strings: Vec<Vec<&'a [u8]>>,
}

impl<'a> Gathered<'a> {
    pub(crate) fn new(fields: usize) -> Self {
        Self {
            present: alloc::vec![false; fields],
            ints: (0..fields).map(|_| Vec::new()).collect(),
            strings: (0..fields).map(|_| Vec::new()).collect(),
        }
    }

    pub(crate) fn take(&mut self, columns: &mut Reader<'a>, index: usize, op: u8, rows: usize) {
        if op == OP_STRING {
            self.strings[index] = columns.strings_bytes();
        } else {
            self.ints[index] = read_column(columns, op, rows);
        }
        self.present[index] = true;
    }

    pub(crate) fn take8(&mut self, columns: &mut Reader8<'a>, index: usize, op: u8, rows: usize) {
        if op == OP_STRING {
            self.strings[index] = columns.strings_bytes();
        } else {
            self.ints[index] = read_column8(columns, op, rows);
        }
        self.present[index] = true;
    }
}

/// A column at the width its op was encoded at, widened to `i64`.
///
/// The width is derived from the type on both sides and never goes on the wire,
/// so reading a one-byte column as eight would not fail — it would silently
/// produce different numbers. Hence the dispatch.
macro_rules! read_column_at {
    ($name:ident, $reader:ty) => {
        pub(crate) fn $name(columns: &mut $reader, op: u8, rows: usize) -> Vec<i64> {
            match plan::width_of_op(op) {
                1 => {
                    let mut values: Vec<i8> = Vec::new();
                    columns.column(rows, &mut values);
                    values.into_iter().map(i64::from).collect()
                }
                2 => {
                    let mut values: Vec<i16> = Vec::new();
                    columns.column(rows, &mut values);
                    values.into_iter().map(i64::from).collect()
                }
                4 => {
                    let mut values: Vec<i32> = Vec::new();
                    columns.column(rows, &mut values);
                    values.into_iter().map(i64::from).collect()
                }
                _ => {
                    let mut values: Vec<i64> = Vec::new();
                    columns.column(rows, &mut values);
                    values
                }
            }
        }
    };
}

read_column_at!(read_column, Reader<'_>);
read_column_at!(read_column8, Reader8<'_>);
