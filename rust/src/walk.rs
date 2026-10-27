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

use alloc::boxed::Box;
use alloc::vec::Vec;

use crate::Error;
use crate::json::{JsonSink, render_key_run};
use crate::plan::{
    self, MAP_ANY, MAP_BOOL, MAP_FLOAT32, MAP_FLOAT64, MAP_INT, MAP_LIST, MAP_STRING, MAP_STRUCT,
    MAP_STRUCTS, MAP_UINT, OP_ANY, OP_ANYS, OP_BOOL, OP_BYTES, OP_FLOAT32, OP_FLOAT64, OP_INT8,
    OP_INT16, OP_INT32, OP_INT64, OP_MAP, OP_POINTER, OP_POINTER_STRUCT, OP_STRING, OP_STRINGS,
    OP_STRUCT, OP_STRUCTS, OP_UINT8, OP_UINT16, OP_UINT32, OP_UINT64, Plan, PlanField,
};
use crate::section::{self, Schema};
use crate::wire::{self, Kind, Reader, Reader8, Runs};

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

/// Renders a whole message — root byte, section and body — as JSON, the way Go's
/// `ToJSON` does: the message's own section takes precedence over `held`, and a
/// dynamic document needs neither unless a TYPED value names a struct.
///
/// # Errors
///
/// Any malformed message, a root this version does not read (a dictionary
/// section among them), and [`Error::NoSchema`] for a typed message with no
/// section anywhere.
pub fn message_to_json(held: Option<&Schema>, message: &[u8]) -> Result<Vec<u8>, Error> {
    let split = split_root(message)?;
    let schema = split.schema.as_ref().or(held);
    match split.root {
        ROOT_NARROW | ROOT_WIDE => {
            let schema = schema.ok_or(Error::NoSchema)?;
            to_json(schema, split.body, split.root == ROOT_WIDE)
        }
        ROOT_DICT_NARROW | ROOT_DICT_WIDE => {
            // A message with dictionaries carries no schema section, and its
            // dictionaries come ahead of the body for the walk to resolve its id
            // columns against.
            let schema = schema.ok_or(Error::NoSchema)?;
            let (dicts, body) = parse_dictionaries(split.body)?;
            let mut walker = Walker::new(schema, message.len()).with_dictionaries(dicts);
            walker.root(0, body, split.root == ROOT_DICT_WIDE)?;
            Ok(walker.to.finish())
        }
        ROOT_DYNAMIC_MAP | ROOT_DYNAMIC_LIST => {
            let none = Schema::default();
            let mut walker = Walker::new(schema.unwrap_or(&none), message.len());
            walker.dynamic_root(split.body, split.root == ROOT_DYNAMIC_LIST)?;
            Ok(walker.to.finish())
        }
        _ => Err(Error::BadRoot(split.root)),
    }
}

/// A message's dictionaries: per dictionary, its entries.
type Dictionaries = Vec<Vec<Vec<u8>>>;

/// Reads the dictionary section at the front of `data` and returns its
/// dictionaries and the body after it:
///
/// ```text
/// section := [byteLength] [dictCount] ( [entryCount] ( [size] [bytes] )* )*
/// ```
fn parse_dictionaries(data: &[u8]) -> Result<(Dictionaries, &[u8]), Error> {
    let section_error = Error::BadDictionary;
    let (size, width) = wire::read_count(data).ok_or(Error::BadDictionary)?;
    if size > data.len() - width {
        return Err(section_error);
    }
    let (section, body) = (&data[width..width + size], &data[width + size..]);
    let (count, mut at) = wire::read_count(section).ok_or(Error::BadDictionary)?;
    // Every dictionary is at least its count byte.
    if count > section.len() - at {
        return Err(Error::BadDictionary);
    }
    let mut dicts = Vec::with_capacity(count);
    for _ in 0..count {
        let (entries, used) = wire::read_count(&section[at..]).ok_or(Error::BadDictionary)?;
        at += used;
        // And every entry at least its size byte.
        if entries > section.len() - at {
            return Err(Error::BadDictionary);
        }
        let mut values = Vec::with_capacity(entries);
        for _ in 0..entries {
            let (length, used) = wire::read_count(&section[at..]).ok_or(Error::BadDictionary)?;
            at += used;
            if length > section.len() - at {
                return Err(Error::BadDictionary);
            }
            values.push(section[at..at + length].to_vec());
            at += length;
        }
        dicts.push(values);
    }
    if at != section.len() {
        return Err(Error::BadDictionary);
    }
    Ok((dicts, body))
}

/// A message cut at its root: the root byte without the schema bit (`ROOT_NARROW`
/// for a message that has none), the section it carries, and the body behind
/// both.
pub(crate) struct Split<'m> {
    pub(crate) root: u8,
    pub(crate) schema: Option<Schema>,
    pub(crate) body: &'m [u8],
}

/// Reads a message's root byte and steps over its section, as Go's `rootParts`
/// does. A first byte outside `0xD0..=0xDF`, or an empty message, is a typed
/// narrow message's key run.
pub(crate) fn split_root(message: &[u8]) -> Result<Split<'_>, Error> {
    let Some(&root) = message.first().filter(|&&byte| wire::root::is_root(byte)) else {
        return Ok(Split {
            root: ROOT_NARROW,
            schema: None,
            body: message,
        });
    };
    if root & ROOT_SCHEMA == 0 {
        return Ok(Split {
            root,
            schema: None,
            body: &message[1..],
        });
    }
    let parsed = section::parse(&message[1..])?;
    let body = message[1..].get(parsed.size..).ok_or(Error::Truncated)?;
    Ok(Split {
        root: root & !ROOT_SCHEMA,
        schema: Some(parsed),
        body,
    })
}

/// The root bytes this module dispatches on, without the schema bit.
const ROOT_NARROW: u8 = 0xD0;
const ROOT_WIDE: u8 = 0xD8;
const ROOT_SCHEMA: u8 = 0x04;
const ROOT_DICT_NARROW: u8 = 0xD2;
const ROOT_DICT_WIDE: u8 = 0xDA;
const ROOT_DYNAMIC_MAP: u8 = 0xD1;
const ROOT_DYNAMIC_LIST: u8 = 0xD3;

/// The walk, and the one buffer it renders into.
pub struct Walker<'s> {
    schema: &'s Schema,
    /// `"name":` per field per plan, rendered once. A field then costs one copy
    /// per row rather than an escape pass — which on a thousand-record table is
    /// the difference between one escape and seven thousand.
    key_runs: Vec<Vec<Vec<u8>>>,
    to: JsonSink,
    depth: u32,
    /// The message's dictionaries, for a root that declared them: the strings
    /// an id column's ids index. Empty for every other message.
    dicts: Vec<Vec<Vec<u8>>>,
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
            dicts: Vec::new(),
        }
    }

    /// Hands the walk the dictionaries of a message that has them.
    pub fn with_dictionaries(mut self, dicts: Vec<Vec<Vec<u8>>>) -> Self {
        self.dicts = dicts;
        self
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
        self.to.begin_object();
        self.narrow_fields(plan_at, body)?;
        self.to.end_object();
        self.leave();
        Ok(())
    }

    /// A four-bit-keyed run's fields, into the object already open: the run's
    /// own or, for a page, the one that links it.
    fn narrow_fields(&mut self, plan_at: u32, body: &[u8]) -> Result<(), Error> {
        let plan = self.plan_at(plan_at)?;
        let mut reader = Reader::new(body);
        let mut seen = alloc::vec![false; plan.fields.len()];
        while reader.more() {
            let Some(index) = plan.field_of(reader.key()) else {
                // The nibble sizes the field whatever its type, so a key the
                // schema does not list is stepped over exactly as a wide one is:
                // a type that dropped a field still reads its old rows.
                reader.skip();
                reader.err()?;
                continue;
            };
            seen[index] = true;
            self.write_key(plan_at, index);
            let field = plan.fields[index].clone();
            self.narrow_value(&mut reader, &field)?;
        }
        reader.err()?;
        self.absent(plan_at, &seen)
    }

    fn wide_run(&mut self, plan_at: u32, body: &[u8]) -> Result<(), Error> {
        self.enter()?;
        self.to.begin_object();
        self.wide_fields(plan_at, body)?;
        self.to.end_object();
        self.leave();
        Ok(())
    }

    /// [`Walker::narrow_fields`] at eight key bits, which is the only width a
    /// page link can be under.
    fn wide_fields(&mut self, plan_at: u32, body: &[u8]) -> Result<(), Error> {
        let plan = self.plan_at(plan_at)?;
        let mut reader = Reader8::new(body);
        let mut seen = alloc::vec![false; plan.fields.len()];
        while reader.more() {
            let Some(index) = plan.field_of(reader.key()) else {
                // A key the schema does not list is stepped over, which is what
                // the wide width is for.
                reader.skip();
                reader.err()?;
                continue;
            };
            seen[index] = true;
            let field = plan.fields[index].clone();
            if self.links_page(&field) {
                self.page(&mut reader, &field)?;
                continue;
            }
            self.write_key(plan_at, index);
            self.wide_value(&mut reader, &field)?;
        }
        reader.err()?;
        self.absent(plan_at, &seen)
    }

    /// Whether a field is the link to the next page of a paged type, which
    /// `section::parse` only accepts as a struct under key 255 of a wide run.
    fn links_page(&self, field: &PlanField) -> bool {
        field.op == OP_STRUCT
            && field
                .sub
                .and_then(|at| self.schema.plan(at))
                .is_some_and(|plan| plan.is_page)
    }

    /// The next page of a paged type, into the object its first page opened:
    /// the fields are the type's, and the page is only where they were put.
    fn page(&mut self, reader: &mut Reader8<'_>, field: &PlanField) -> Result<(), Error> {
        let (body, wide) = reader.struct_body().ok_or(Error::Truncated)?;
        let at = field.sub.ok_or(Error::BadSection)?;
        self.enter()?;
        if wide {
            self.wide_fields(at, body)?;
        } else {
            self.narrow_fields(at, body)?;
        }
        self.leave();
        Ok(())
    }

    /// The fields the message left out. See the module comment on ordering. A
    /// page left out is the same thing at a larger scale: its fields go into
    /// this object, as zeros.
    fn absent(&mut self, plan_at: u32, seen: &[bool]) -> Result<(), Error> {
        let plan = self.plan_at(plan_at)?;
        for (index, field) in plan.fields.iter().enumerate() {
            if seen.get(index).copied().unwrap_or(false) {
                continue;
            }
            let field = field.clone();
            if self.links_page(&field) {
                self.absent_page(field.sub)?;
                continue;
            }
            self.write_key(plan_at, index);
            self.zero(&field)?;
        }
        Ok(())
    }

    fn absent_page(&mut self, sub: Option<u32>) -> Result<(), Error> {
        let at = sub.ok_or(Error::BadSection)?;
        self.enter()?;
        let none = alloc::vec![false; self.plan_at(at)?.fields.len()];
        self.absent(at, &none)?;
        self.leave();
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
            OP_STRUCT | OP_POINTER_STRUCT => {
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
            OP_MAP => self.narrow_map(reader, field),
            OP_POINTER => self.narrow_scalar(reader, field.elem_op),
            op => self.narrow_scalar(reader, op),
        }
    }

    fn narrow_scalar(&mut self, reader: &mut Reader<'_>, op: u8) -> Result<(), Error> {
        match op {
            OP_BOOL => self.to.boolean(reader.bool()),
            // Each width is read as itself, so a value past its type is refused
            // rather than rendered: the schema is what says what fits.
            OP_INT8 => self.to.signed(i64::from(reader.i8())),
            OP_INT16 => self.to.signed(i64::from(reader.i16())),
            OP_INT32 => self.to.signed(i64::from(reader.i32())),
            OP_INT64 => self.to.signed(reader.i64()),
            OP_UINT8 => self.to.unsigned(u64::from(reader.u8())),
            OP_UINT16 => self.to.unsigned(u64::from(reader.u16())),
            OP_UINT32 => self.to.unsigned(u64::from(reader.u32())),
            OP_UINT64 => self.to.unsigned(reader.u64()),
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
                // A narrow array's form is its element type's, and the op is the
                // only element type a walk has: it says whether the elements are
                // two's complement and how wide one may be.
                let element = plan::array_element_op(o);
                let mut values: Vec<i64> = Vec::new();
                reader.ints_into_as(
                    &mut values,
                    plan::is_signed_op(element),
                    plan::width_of_op(element),
                );
                self.int_array(&values, o);
            }
            o => return Err(Error::UnwalkableOp(o)),
        }
        reader.err()
    }

    fn wide_value(&mut self, reader: &mut Reader8<'_>, field: &PlanField) -> Result<(), Error> {
        match field.op {
            OP_STRUCT | OP_POINTER_STRUCT => {
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
            // Each width is read as itself, so a value past its type is refused
            // rather than rendered: the schema is what says what fits.
            OP_INT8 => self.to.signed(i64::from(reader.i8())),
            OP_INT16 => self.to.signed(i64::from(reader.i16())),
            OP_INT32 => self.to.signed(i64::from(reader.i32())),
            OP_INT64 => self.to.signed(reader.i64()),
            OP_UINT8 => self.to.unsigned(u64::from(reader.u8())),
            OP_UINT16 => self.to.unsigned(u64::from(reader.u16())),
            OP_UINT32 => self.to.unsigned(u64::from(reader.u32())),
            OP_UINT64 => self.to.unsigned(reader.u64()),
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

    /// A map in a four-bit run. Its entries carry no class — a four-bit
    /// descriptor has no room for one — so each takes its type from the kinds
    /// the section named, through the narrow element readers. A dynamic value
    /// and a list value are key-less values with a class of their own, which is
    /// why a map of either is always wide: a section saying otherwise is wrong.
    fn narrow_map(&mut self, reader: &mut Reader<'_>, field: &PlanField) -> Result<(), Error> {
        let (count, mut entries) = reader.map().ok_or(Error::Truncated)?;
        if !map_key_is_renderable(field.key_kind)
            || field.value_kind == MAP_ANY
            || plan::list_valued(field.value_kind)
        {
            return Err(Error::UnwalkableOp(field.op));
        }
        self.enter()?;
        self.to.begin_object();
        for _ in 0..count {
            match field.key_kind {
                MAP_STRING => {
                    let name = entries.element_string();
                    entries.err()?;
                    self.to.key_bytes(name.as_bytes());
                }
                MAP_INT => self.number_key(entries.element_int(), true),
                _ => self.number_key(entries.element_uint() as i64, false),
            }
            self.narrow_map_value(&mut entries, field.value_kind, field.sub)?;
            entries.err()?;
        }
        self.to.end_object();
        self.leave();
        Ok(())
    }

    /// [`Walker::map_value`] at four key bits. A struct value is a narrow
    /// list's element — a length and a body — whose key width the section says,
    /// because nothing on the wire does.
    fn narrow_map_value(
        &mut self,
        entries: &mut Reader<'_>,
        kind: u8,
        sub: Option<u32>,
    ) -> Result<(), Error> {
        match kind {
            MAP_STRING => {
                let text = entries.element_string();
                entries.err()?;
                self.to.text_bytes(text.as_bytes());
            }
            MAP_INT => self.to.signed(entries.element_int()),
            MAP_UINT => self.to.unsigned(entries.element_uint()),
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
            MAP_STRUCT => {
                let at = sub.ok_or(Error::BadSection)?;
                let wide = self.plan_at(at)?.is_wide;
                let body = entries.element().ok_or(Error::Truncated)?;
                return self.message(at, body, wide);
            }
            other => return Err(Error::UnwalkableOp(other)),
        }
        Ok(())
    }

    /// A map at eight key bits, whose entries are pairs of key-less values: no
    /// field ids, no schema lookup, just the kinds the section named once for
    /// all of them.
    fn wide_map(&mut self, reader: &mut Reader8<'_>, field: &PlanField) -> Result<(), Error> {
        let (count, mut entries) = reader.map().ok_or(Error::Truncated)?;
        if !map_key_is_renderable(field.key_kind) {
            return Err(Error::UnwalkableOp(field.op));
        }
        if field.value_kind == MAP_ANY {
            // The entries of a `map[string]any`, which say what they are one at a
            // time rather than once in the schema: a dynamic container, compact
            // inside. See `wire/compact.rs`.
            self.compact_entries(&mut entries, count)?;
            return entries.err();
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
            match field.value_kind {
                MAP_STRUCTS => self.map_structs(&mut entries, field.sub)?,
                MAP_LIST => self.map_list(&mut entries, field.list_kind)?,
                kind => self.map_value(&mut entries, kind, field.sub)?,
            }
            entries.err()?;
        }
        self.to.end_object();
        self.leave();
        Ok(())
    }

    /// One map value, by the kind the section declared for all of them — and,
    /// for a struct value, the plan it named.
    fn map_value(
        &mut self,
        entries: &mut Reader8<'_>,
        kind: u8,
        sub: Option<u32>,
    ) -> Result<(), Error> {
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
            // A struct value is a list's element: a descriptor that says its key
            // width, a length and a key run.
            MAP_STRUCT => {
                let at = sub.ok_or(Error::BadSection)?;
                let (body, wide) = entries.element_struct_body().ok_or(Error::Truncated)?;
                return self.message(at, body, wide);
            }
            other => return Err(Error::UnwalkableOp(other)),
        }
        Ok(())
    }

    /// A [`MAP_STRUCTS`] value: the key-less list or table a slice of structs
    /// inside a dynamic value is, behind the struct the section names rather
    /// than a type tag.
    fn map_structs(&mut self, entries: &mut Reader8<'_>, sub: Option<u32>) -> Result<(), Error> {
        let at = sub.ok_or(Error::BadSection)?;
        self.plan_at(at)?;
        match entries.element_kind() {
            Kind::List => {
                let (count, mut elements) = entries.element_list().ok_or(Error::Truncated)?;
                self.struct_list(at, count, &mut elements)
            }
            Kind::Table => {
                let (rows, mut columns) = entries.element_table().ok_or(Error::Truncated)?;
                self.struct_table(at, rows, &mut columns)
            }
            _ => Err(Error::BadDescriptor),
        }
    }

    /// A [`MAP_LIST`] value: a key-less list of elements of one scalar kind,
    /// each rendered as a map value of that kind is.
    fn map_list(&mut self, entries: &mut Reader8<'_>, kind: u8) -> Result<(), Error> {
        if entries.element_kind() != Kind::List {
            return Err(Error::BadDescriptor);
        }
        let (count, mut elements) = entries.element_list().ok_or(Error::Truncated)?;
        self.enter()?;
        self.to.begin_array();
        for _ in 0..count {
            self.map_value(&mut elements, kind, None)?;
            elements.err()?;
        }
        self.to.end_array();
        self.leave();
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
            self.compact_value(&mut elements)?;
        }
        elements.err()?;
        self.to.end_array();
        self.leave();
        Ok(())
    }

    fn dynamic_map(&mut self, reader: &mut Reader8<'_>) -> Result<(), Error> {
        let (count, mut entries) = reader.element_map().ok_or(Error::Truncated)?;
        self.compact_entries(&mut entries, count)?;
        entries.err()
    }

    /// A value whose type the section names: the rows of an array of records,
    /// which is the shape the dynamic form exists for.
    fn dynamic_typed(&mut self, reader: &mut Reader8<'_>) -> Result<(), Error> {
        let at = reader.element_typed_index().ok_or(Error::Truncated)?;
        self.typed_value(reader, at)
    }

    /// The record or the rows behind a type tag naming `at`.
    fn typed_value(&mut self, reader: &mut Reader8<'_>, at: u32) -> Result<(), Error> {
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

    // ---- compact values -----------------------------------------------------

    /// One compact value: [`Walker::dynamic_value`], inside a container.
    fn compact_value(&mut self, reader: &mut Reader8<'_>) -> Result<(), Error> {
        match reader.compact_kind() {
            Kind::Null => {
                reader.compact_null();
                self.to.null();
            }
            Kind::Bool => {
                let value = reader.compact_bool();
                self.to.boolean(value);
            }
            Kind::Int => {
                let (magnitude, negative) = reader.compact_integer();
                if negative {
                    self.to.signed((magnitude as i64).wrapping_neg());
                } else if let Ok(signed) = i64::try_from(magnitude) {
                    self.to.signed(signed);
                } else {
                    self.to.unsigned(magnitude);
                }
            }
            Kind::Float64 | Kind::Float32 => {
                let (value, width) = reader.compact_float();
                if !self.to.float(value, width) {
                    return Err(Error::NotJson);
                }
            }
            Kind::String => {
                let text = reader.compact_string();
                reader.err()?;
                self.to.text_bytes(text);
            }
            Kind::Bytes => {
                let bytes = reader.compact_bytes();
                reader.err()?;
                self.to.blob(bytes);
            }
            Kind::List => {
                let count = reader.compact_list().ok_or(Error::Truncated)?;
                self.enter()?;
                self.to.begin_array();
                for _ in 0..count {
                    self.compact_value(reader)?;
                }
                self.to.end_array();
                self.leave();
            }
            Kind::Map => {
                let count = reader.compact_map().ok_or(Error::Truncated)?;
                self.compact_entries(reader, count)?;
            }
            Kind::Typed => {
                let at = reader.compact_typed().ok_or(Error::Truncated)?;
                self.typed_value(reader, at)?;
            }
            _ => {
                reader.err()?;
                return Err(Error::BadDescriptor);
            }
        }
        reader.err()
    }

    /// `count` compact entries, as an object.
    fn compact_entries(&mut self, reader: &mut Reader8<'_>, count: usize) -> Result<(), Error> {
        self.enter()?;
        self.to.begin_object();
        for _ in 0..count {
            self.compact_key(reader)?;
            self.compact_value(reader)?;
        }
        self.to.end_object();
        self.leave();
        Ok(())
    }

    /// An entry's key, which has to be a name.
    fn compact_key(&mut self, reader: &mut Reader8<'_>) -> Result<(), Error> {
        match reader.compact_kind() {
            Kind::String => {
                let name = reader.compact_string();
                reader.err()?;
                self.to.key_bytes(name);
            }
            Kind::Int => {
                let (magnitude, negative) = reader.compact_integer();
                if negative {
                    self.number_key((magnitude as i64).wrapping_neg(), true);
                } else {
                    self.number_key(magnitude as i64, false);
                }
            }
            _ => {
                reader.err()?;
                return Err(Error::BadDescriptor);
            }
        }
        reader.err()
    }

    /// A message whose root is a dynamic document: a `map[string]any`'s entries
    /// to the end of the message, or one compact value for a `[]any`.
    ///
    /// # Errors
    ///
    /// Any malformed message.
    pub fn dynamic_root(&mut self, body: &[u8], list: bool) -> Result<(), Error> {
        let mut reader = Reader8::new(body);
        if list {
            self.compact_value(&mut reader)?;
            if reader.compact_more() {
                return Err(Error::Trailing);
            }
            return Ok(());
        }
        self.enter()?;
        self.to.begin_object();
        while reader.compact_more() {
            self.compact_key(&mut reader)?;
            self.compact_value(&mut reader)?;
        }
        self.to.end_object();
        self.leave();
        Ok(())
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
        let (count, mut elements) = reader.list().ok_or(Error::Truncated)?;
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
        let (rows, group_rows, body) = reader.table().ok_or(Error::Truncated)?;
        // The columns are keyed at the row type's width, which for wide rows is
        // not this run's. See `codec::write_table`.
        let wide = self.plan_at(at)?.is_wide;
        self.table(at, rows, group_rows, body, wide)
    }

    fn wide_table(&mut self, reader: &mut Reader8<'_>, field: &PlanField) -> Result<(), Error> {
        let at = field.sub.ok_or(Error::BadSection)?;
        let (rows, group_rows, body) = reader.table().ok_or(Error::Truncated)?;
        self.table(at, rows, group_rows, body, true)
    }

    /// A table's body as one array: its columns, keyed at eight bits if `wide`,
    /// or for a grouped table each group's in turn, so that a group's columns
    /// are all it holds at once.
    fn table(
        &mut self,
        at: u32,
        rows: usize,
        group_rows: usize,
        body: &[u8],
        wide: bool,
    ) -> Result<(), Error> {
        if rows > MAX_ROWS {
            return Err(Error::TooManyRows);
        }
        self.enter()?;
        self.to.begin_array();
        for run in Runs::new(rows, group_rows, body) {
            let (rows, columns) = run?;
            let mut gathered = Gathered::new(self.plan_at(at)?.fields.len());
            gathered.read(self.schema, at, columns, rows, wide, self.dicts_on(), 0)?;
            self.table_rows(at, &mut gathered, rows)?;
        }
        self.to.end_array();
        self.leave();
        Ok(())
    }

    /// A table a dynamic value reached through a TYPED tag, for the reason
    /// [`Walker::struct_list`] is split out. It is never grouped.
    fn struct_table(
        &mut self,
        at: u32,
        rows: usize,
        columns: &mut Reader8<'_>,
    ) -> Result<(), Error> {
        if rows > MAX_ROWS {
            return Err(Error::TooManyRows);
        }
        let mut gathered = Gathered::new(self.plan_at(at)?.fields.len());
        gathered.read_run8(self.schema, at, columns, rows, self.dicts_on(), 0)?;
        self.enter()?;
        self.to.begin_array();
        self.table_rows(at, &mut gathered, rows)?;
        self.to.end_array();
        self.leave();
        Ok(())
    }

    /// Whether this message declared dictionaries, which is what makes a string
    /// column worth asking whether it holds ids.
    fn dicts_on(&self) -> bool {
        !self.dicts.is_empty()
    }

    /// The transpose itself: `rows` objects, without the array around them,
    /// which a grouped table writes once for all its groups.
    fn table_rows(
        &mut self,
        plan_at: u32,
        gathered: &mut Gathered<'_>,
        rows: usize,
    ) -> Result<(), Error> {
        gathered.restart();
        for row in 0..rows {
            self.to.begin_object();
            self.row_fields(plan_at, gathered, row)?;
            self.to.end_object();
        }
        Ok(())
    }

    /// One row's fields into the object open for it, a paged row type's next
    /// pages merged in as a record's are ([`Walker::page`]).
    fn row_fields(
        &mut self,
        plan_at: u32,
        gathered: &mut Gathered<'_>,
        row: usize,
    ) -> Result<(), Error> {
        let plan = self.plan_at(plan_at)?;
        for (index, field) in plan.fields.iter().enumerate() {
            if self.links_page(field) {
                let at = field.sub.ok_or(Error::BadSection)?;
                match gathered.pages[index].as_deref_mut() {
                    Some(page) if gathered.present[index] => self.row_fields(at, page, row)?,
                    _ => self.absent_page(field.sub)?,
                }
                continue;
            }
            self.write_key(plan_at, index);
            if field.op == OP_STRUCTS && gathered.present[index] {
                self.list_rows(field, gathered, index, row)?;
                continue;
            }
            let (mut op, mut at) = (field.op, row);
            if op == OP_POINTER && gathered.present[index] {
                op = field.elem_op;
                if let Some(presence) = gathered.presence[index] {
                    if !wire::present(presence, row) {
                        self.to.null();
                        continue;
                    }
                    at = gathered.next[index];
                    gathered.next[index] += 1;
                }
            }
            if !gathered.present[index] {
                // A column whose every value was zero is not written at all, and
                // its absence is the whole of what says so — or, for a pointer
                // column, that every row was nil.
                self.zero(field)?;
            } else if op == OP_STRING {
                if let Some(number) = gathered.dict[index] {
                    let id = gathered.ints[index].get(at).copied().unwrap_or(0);
                    let value = usize::try_from(id)
                        .ok()
                        .and_then(|id| self.dicts.get(number)?.get(id))
                        .ok_or(Error::BadDictionary)?;
                    self.to.text_bytes(value);
                } else {
                    let value = gathered.strings[index].get(at).copied().unwrap_or(&[]);
                    self.to.text_bytes(value);
                }
            } else {
                // A column shorter than the table's row count is a message that
                // disagrees with itself; the row still has to be written, and a
                // zero is what an absent column means everywhere else here.
                let raw = gathered.ints[index].get(at).copied().unwrap_or(0);
                self.column_value(op, raw)?;
            }
        }
        Ok(())
    }

    /// One row's elements of a list column: the next of them in its element
    /// table, as many as the row's length says. A row with none is what an empty
    /// slice is in a record, which leaves it out.
    fn list_rows(
        &mut self,
        field: &PlanField,
        gathered: &mut Gathered<'_>,
        index: usize,
        row: usize,
    ) -> Result<(), Error> {
        let n = gathered.lengths[index].get(row).copied().unwrap_or(0);
        if n == 0 {
            return self.zero(field);
        }
        let at = field.sub.ok_or(Error::BadSection)?;
        let n = usize::try_from(n).map_err(|_| Error::BadLengths)?;
        self.enter()?;
        let start = gathered.cursor[index];
        gathered.cursor[index] += n;
        let elements = gathered.lists[index]
            .as_deref_mut()
            .ok_or(Error::BadLengths)?;
        self.to.begin_array();
        for element in start..start + n {
            self.to.begin_object();
            self.row_fields(at, elements, element)?;
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
/// gather step verbatim for the plain columns of a table.
pub(crate) struct Gathered<'a> {
    pub(crate) present: Vec<bool>,
    pub(crate) ints: Vec<Vec<i64>>,
    pub(crate) strings: Vec<Vec<&'a [u8]>>,
    /// A nullable column's presence, `None` for a column every row holds a
    /// value of; `next` is where the row being written reads its value from.
    presence: Vec<Option<&'a [u8]>>,
    next: Vec<usize>,
    /// Which of the message's dictionaries a string column's ids index. The ids
    /// are in `ints`, and resolved as the rows are written.
    dict: Vec<Option<usize>>,
    /// A paged row type's next pages, by the index of their link: each a table
    /// of the same rows.
    pages: Vec<Option<Box<Gathered<'a>>>>,
    /// The element tables of list columns by field index, `lengths` each row's
    /// count of elements and `cursor` the element the row being written starts at.
    lists: Vec<Option<Box<Gathered<'a>>>>,
    lengths: Vec<Vec<i64>>,
    cursor: Vec<usize>,
}

/// How many list columns deep a table may go, which no writer takes past.
const MAX_LIST_DEPTH: u32 = 4;

/// A column run's reader at either key width.
trait ColumnReader<'a> {
    fn is_nullable(&self) -> bool;
    fn is_dict_column(&self) -> bool;
    fn is_nullable_dict_column(&self) -> bool;
    fn int_column(&mut self, op: u8, rows: usize) -> Vec<i64>;
    fn string_column(&mut self) -> Vec<&'a [u8]>;
    fn nullable_ints(&mut self, rows: usize) -> (&'a [u8], Vec<i64>);
    fn nullable_strings(&mut self, rows: usize) -> (&'a [u8], Vec<&'a [u8]>);
    fn dict_ids(&mut self, rows: usize) -> Option<(usize, Vec<i64>)>;
    fn nullable_dict_ids(&mut self, rows: usize) -> Option<(usize, &'a [u8], Vec<i64>)>;
}

macro_rules! column_reader {
    ($reader:ident, $int_column:ident, $strings:ident) => {
        impl<'a> ColumnReader<'a> for $reader<'a> {
            fn is_nullable(&self) -> bool {
                $reader::is_nullable(self)
            }
            fn is_dict_column(&self) -> bool {
                $reader::is_dict_column(self)
            }
            fn is_nullable_dict_column(&self) -> bool {
                $reader::is_nullable_dict_column(self)
            }
            fn int_column(&mut self, op: u8, rows: usize) -> Vec<i64> {
                $int_column(self, op, rows)
            }
            fn string_column(&mut self) -> Vec<&'a [u8]> {
                $reader::$strings(self)
            }
            fn nullable_ints(&mut self, rows: usize) -> (&'a [u8], Vec<i64>) {
                $reader::nullable_column(self, rows)
            }
            fn nullable_strings(&mut self, rows: usize) -> (&'a [u8], Vec<&'a [u8]>) {
                $reader::nullable_strings_bytes(self, rows)
            }
            fn dict_ids(&mut self, rows: usize) -> Option<(usize, Vec<i64>)> {
                $reader::dict_column(self, rows)
            }
            fn nullable_dict_ids(&mut self, rows: usize) -> Option<(usize, &'a [u8], Vec<i64>)> {
                $reader::nullable_dict_column(self, rows)
            }
        }
    };
}

column_reader!(Reader, read_column, column_strings_bytes);
column_reader!(Reader8, read_column8, strings_bytes);

impl<'a> Gathered<'a> {
    pub(crate) fn new(fields: usize) -> Self {
        Self {
            present: alloc::vec![false; fields],
            ints: (0..fields).map(|_| Vec::new()).collect(),
            strings: (0..fields).map(|_| Vec::new()).collect(),
            presence: alloc::vec![None; fields],
            next: alloc::vec![0; fields],
            dict: alloc::vec![None; fields],
            pages: (0..fields).map(|_| None).collect(),
            lists: (0..fields).map(|_| None).collect(),
            lengths: (0..fields).map(|_| Vec::new()).collect(),
            cursor: alloc::vec![0; fields],
        }
    }

    /// Whether any column is something the plain gather does not hold: a
    /// nullable, dictionary, page or list column.
    #[cfg(feature = "materialize")]
    pub(crate) fn is_plain(&self) -> bool {
        self.presence.iter().all(Option::is_none)
            && self.dict.iter().all(Option::is_none)
            && self.pages.iter().all(Option::is_none)
            && self.lists.iter().all(Option::is_none)
    }

    /// Rewinds where each nullable column's next value is read from, and each
    /// list column's next element, its pages' and element tables' included.
    fn restart(&mut self) {
        self.next.fill(0);
        self.cursor.fill(0);
        for index in 0..self.present.len() {
            if !self.present[index] {
                continue;
            }
            if let Some(page) = self.pages[index].as_deref_mut() {
                page.restart();
            }
            if let Some(elements) = self.lists[index].as_deref_mut() {
                elements.restart();
            }
        }
    }

    /// Reads the column at the cursor into field `index`.
    fn take_column<R: ColumnReader<'a>>(
        &mut self,
        columns: &mut R,
        field: &PlanField,
        index: usize,
        rows: usize,
        dicts_on: bool,
    ) {
        let mut op = field.op;
        if op == OP_POINTER {
            op = field.elem_op;
        }
        self.presence[index] = None;
        self.dict[index] = None;
        if op == OP_STRING && dicts_on && self.take_dict_column(columns, index, rows) {
            return;
        }
        let nullable = field.op == OP_POINTER && columns.is_nullable();
        if op == OP_STRING {
            if nullable {
                let (presence, strings) = columns.nullable_strings(rows);
                self.presence[index] = Some(presence);
                self.strings[index] = strings;
            } else {
                self.strings[index] = columns.string_column();
            }
        } else if nullable {
            let (presence, ints) = columns.nullable_ints(rows);
            self.presence[index] = Some(presence);
            self.ints[index] = ints;
        } else {
            self.ints[index] = columns.int_column(op, rows);
        }
        self.present[index] = true;
    }

    /// Reads a string column written as ids, in either form. It reads nothing and
    /// reports `false` when the column at the cursor is not one.
    fn take_dict_column<R: ColumnReader<'a>>(
        &mut self,
        columns: &mut R,
        index: usize,
        rows: usize,
    ) -> bool {
        if columns.is_dict_column() {
            if let Some((number, ids)) = columns.dict_ids(rows) {
                self.dict[index] = Some(number);
                self.ints[index] = ids;
                self.present[index] = true;
            }
            return true;
        }
        if columns.is_nullable_dict_column() {
            if let Some((number, presence, ids)) = columns.nullable_dict_ids(rows) {
                self.dict[index] = Some(number);
                self.presence[index] = Some(presence);
                self.ints[index] = ids;
                self.present[index] = true;
            }
            return true;
        }
        false
    }

    /// Reads the list column at the cursor into field `index`: its lengths, and
    /// its element table, keyed at eight bits if `wide`.
    #[allow(clippy::too_many_arguments)]
    fn take_list<R: ListColumns<'a>>(
        &mut self,
        schema: &Schema,
        columns: &mut R,
        field: &PlanField,
        index: usize,
        rows: usize,
        wide: bool,
        dicts_on: bool,
        depth: u32,
    ) -> Result<(), Error> {
        let at = field.sub.ok_or(Error::BadSection)?;
        let sub = schema.plan(at).ok_or(Error::BadSection)?;
        let Some(list) = columns.list_column(rows) else {
            return Ok(());
        };
        if list.children > MAX_ROWS {
            return Err(Error::TooManyRows);
        }
        if depth >= MAX_LIST_DEPTH {
            return Err(Error::TooDeep);
        }
        self.lengths[index] = list.lengths;
        let elements =
            self.lists[index].get_or_insert_with(|| Box::new(Gathered::new(sub.fields.len())));
        elements.read(
            schema,
            at,
            list.run,
            list.children,
            wide || sub.is_wide,
            dicts_on,
            depth + 1,
        )?;
        self.present[index] = true;
        Ok(())
    }

    /// Reads the table at the cursor as field `index`'s page, a table of the same
    /// rows.
    #[allow(clippy::too_many_arguments)]
    fn take_page(
        &mut self,
        schema: &Schema,
        columns: &mut Reader8<'a>,
        field: &PlanField,
        index: usize,
        rows: usize,
        dicts_on: bool,
        depth: u32,
    ) -> Result<(), Error> {
        let at = field.sub.ok_or(Error::BadSection)?;
        let sub = schema.plan(at).ok_or(Error::BadSection)?;
        let (page_rows, group_rows, body) = columns.table().ok_or(Error::Truncated)?;
        if page_rows != rows || group_rows != 0 {
            return Err(Error::BadGroup);
        }
        let page =
            self.pages[index].get_or_insert_with(|| Box::new(Gathered::new(sub.fields.len())));
        page.read(schema, at, body, rows, true, dicts_on, depth)?;
        self.present[index] = true;
        Ok(())
    }

    /// Gathers one column run of `rows` rows of plan `at`, keyed at eight bits if
    /// `wide`, stepping over a column the plan does not declare.
    #[allow(clippy::too_many_arguments)]
    pub(crate) fn read(
        &mut self,
        schema: &Schema,
        at: u32,
        body: &'a [u8],
        rows: usize,
        wide: bool,
        dicts_on: bool,
        depth: u32,
    ) -> Result<(), Error> {
        if wide {
            let mut columns = Reader8::new(body);
            return self.read_run8(schema, at, &mut columns, rows, dicts_on, depth);
        }
        let plan = schema.plan(at).ok_or(Error::BadSection)?;
        self.present.fill(false);
        let mut columns = Reader::new(body);
        while columns.more() {
            let Some(index) = plan.field_of(columns.key()) else {
                columns.skip();
                columns.err()?;
                continue;
            };
            let field = &plan.fields[index];
            if field.op == OP_STRUCTS {
                self.take_list(
                    schema,
                    &mut columns,
                    field,
                    index,
                    rows,
                    false,
                    dicts_on,
                    depth,
                )?;
            } else {
                self.take_column(&mut columns, field, index, rows, dicts_on);
            }
            columns.err()?;
        }
        columns.err()
    }

    /// [`Gathered::read`] for a column run keyed at eight bits, already opened.
    pub(crate) fn read_run8(
        &mut self,
        schema: &Schema,
        at: u32,
        columns: &mut Reader8<'a>,
        rows: usize,
        dicts_on: bool,
        depth: u32,
    ) -> Result<(), Error> {
        let plan = schema.plan(at).ok_or(Error::BadSection)?;
        self.present.fill(false);
        while columns.more() {
            let Some(index) = plan.field_of(columns.key()) else {
                columns.skip();
                columns.err()?;
                continue;
            };
            let field = &plan.fields[index];
            let links_page = field.op == OP_STRUCT
                && field
                    .sub
                    .and_then(|sub| schema.plan(sub))
                    .is_some_and(|sub| sub.is_page);
            if links_page {
                self.take_page(schema, columns, field, index, rows, dicts_on, depth)?;
            } else if field.op == OP_STRUCTS {
                self.take_list(schema, columns, field, index, rows, true, dicts_on, depth)?;
            } else {
                self.take_column(columns, field, index, rows, dicts_on);
            }
            columns.err()?;
        }
        columns.err()
    }

    /// Gathers a whole table body, each group's rows after the one before's, so
    /// that a grouped table reads as the table it was cut from. A column a
    /// group leaves out is zeros, or empty strings, for that group's rows.
    #[cfg(feature = "materialize")]
    pub(crate) fn read_table(
        schema: &Schema,
        at: u32,
        rows: usize,
        group_rows: usize,
        body: &'a [u8],
        wide: bool,
    ) -> Result<Self, Error> {
        let plan = schema.plan(at).ok_or(Error::BadSection)?;
        if group_rows == 0 {
            let mut whole = Self::new(plan.fields.len());
            whole.read(schema, at, body, rows, wide, false, 0)?;
            return Ok(whole);
        }
        let mut whole = Self::new(plan.fields.len());
        let mut start = 0;
        for run in Runs::new(rows, group_rows, body) {
            let (size, columns) = run?;
            let mut group = Self::new(plan.fields.len());
            group.read(schema, at, columns, size, wide, false, 0)?;
            for (index, field) in plan.fields.iter().enumerate() {
                if !group.present[index] && !whole.present[index] {
                    continue;
                }
                whole.present[index] = true;
                if field.op == OP_STRING {
                    let strings = &mut whole.strings[index];
                    strings.resize(start, &[]);
                    strings.extend(group.strings[index].iter().take(size));
                } else {
                    let ints = &mut whole.ints[index];
                    ints.resize(start, 0);
                    ints.extend(group.ints[index].iter().take(size));
                }
            }
            start += size;
        }
        Ok(whole)
    }
}

/// A column run's reader, for the list column.
trait ListColumns<'a> {
    fn list_column(&mut self, rows: usize) -> Option<wire::ListColumn<'a>>;
}

impl<'a> ListColumns<'a> for Reader<'a> {
    fn list_column(&mut self, rows: usize) -> Option<wire::ListColumn<'a>> {
        Reader::list_column(self, rows)
    }
}

impl<'a> ListColumns<'a> for Reader8<'a> {
    fn list_column(&mut self, rows: usize) -> Option<wire::ListColumn<'a>> {
        Reader8::list_column(self, rows)
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
