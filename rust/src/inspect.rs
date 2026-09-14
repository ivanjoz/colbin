//! Where the bytes went: the field tree, with a byte span on every node.
//!
//! This is the page's whole point. Anyone can be told "three times smaller";
//! watching `name` eat 38% of the payload is what
//! makes the format legible. It is also the decoder's own test — if the spans do
//! not tile the buffer exactly, something in the walk is reading a field
//! differently from the way it was written, and that shows up here as a gap
//! rather than as a wrong value.
//!
//! # Why this is its own walk
//!
//! [`crate::walk`] answers "what does this message say"; this answers "where is
//! it". They visit different things. A rendering walk descends into every row of
//! a table and every element of a list, because JSON needs each value. A span
//! walk stops at the column: a table of four thousand rows is six nodes, not
//! twenty-four thousand.
//!
//! The duplication is held honest by the strongest invariant there is: **the
//! spans tile the body exactly**. A key run has no padding, an omitted field
//! writes nothing, so the fields that *are* written are contiguous from the
//! first byte of the body to the last. If this walk consumed a field differently
//! from the decoder, the total would not match and the test says so.
//!
//! # Every node has a real span
//!
//! Including a list's, which is the one shape where that took a decision. A list
//! is row-wise, so there is no per-field span inside it — the bytes of `sku` are
//! scattered through every element. So a list's children are its *elements*,
//! each with a real span, and an element's children are its fields. That keeps
//! the tiling exact where per-field totals would have made `start` a fiction and
//! the hex highlight a lie.
//!
//! A list past [`ELEMENT_LIMIT`] elements collapses the tail into one node,
//! which keeps the tree bounded without putting a hole in it. It rarely happens:
//! a list of columnable structs becomes a table at eight rows, so a long list
//! means an element the column codec cannot carry.

use alloc::format;
use alloc::string::String;
use alloc::vec::Vec;

use crate::Error;
use crate::json::JsonSink;
use crate::plan::{
    self, OP_ANY, OP_ANYS, OP_BOOL, OP_BYTES, OP_FLOAT32, OP_FLOAT64, OP_INT8, OP_INT8S, OP_INT16,
    OP_INT16S, OP_INT32, OP_INT32S, OP_INT64, OP_INT64S, OP_MAP, OP_POINTER, OP_STRING, OP_STRINGS,
    OP_STRUCT, OP_STRUCTS, OP_UINT8, OP_UINT16, OP_UINT16S, OP_UINT32, OP_UINT32S, OP_UINT64,
    OP_UINT64S, Plan, PlanField,
};
use crate::section::{self, Schema};
use crate::wire::{Reader, Reader8};

/// How many of a list's elements get a node of their own.
const ELEMENT_LIMIT: usize = 64;

/// How deep the tree goes, matching the decoder's bound.
const MAX_DEPTH: u32 = 128;

const ROOT_FIRST: u8 = 0xd0;
const ROOT_WIDE: u8 = 0x08;
const ROOT_SCHEMA: u8 = 0x04;

/// One node of the tree, built before it is written so a parent can hold its
/// children's total.
struct Node {
    name: String,
    key: i32,
    type_name: String,
    /// Whether the field is a pointer, which is the format's "absent, not zero".
    optional: bool,
    /// Absolute offsets into the whole message, so the hex view can highlight
    /// without knowing where the body starts.
    start: usize,
    end: usize,
    children: Vec<Node>,
}

impl Node {
    fn bytes(&self) -> usize {
        self.end.saturating_sub(self.start)
    }
}

/// The field tree of a message, as JSON, with a byte span on every node.
///
/// `schema` is the out-of-band section. Pass `None` for a message that carries
/// its own — byte 0 with the schema bit — which is the self-describing delivery.
///
/// # Errors
///
/// Any malformed message, a root this version does not assign, and a message
/// that carries no section when none was passed.
pub fn inspect(data: &[u8], schema: Option<&Schema>) -> Result<Vec<u8>, Error> {
    if data.is_empty() {
        return Err(Error::Truncated);
    }
    let root = data[0];
    if root & 0xf0 != ROOT_FIRST {
        return Err(Error::BadRoot(root));
    }
    // Four of sixteen detail values are assigned (wide, schema, both, neither).
    // The rest must be refused, which is what leaves room for the format to grow.
    if root & !(ROOT_WIDE | ROOT_SCHEMA) != ROOT_FIRST {
        return Err(Error::BadRoot(root));
    }

    let mut held: Option<Schema> = None;
    let mut at = 1;
    let mut section_bytes = 0;
    if root & ROOT_SCHEMA != 0 {
        let parsed = section::parse(&data[1..])?;
        section_bytes = parsed.size;
        at = 1 + section_bytes;
        held = Some(parsed);
    } else if schema.is_none() {
        return Err(Error::NoSchema);
    }
    // A message that carries its own section is described by *that* section, and
    // a held one is ignored — which is what [`crate::walk`] does for the same
    // message under `decode`. The two disagreeing would put this walk's spans on
    // one reading of the body and the rendered JSON on another, and the hex view
    // would highlight bytes the values did not come from.
    let plan_schema = match (&held, schema) {
        (Some(parsed), _) => parsed,
        (None, Some(passed)) => passed,
        (None, None) => return Err(Error::NoSchema),
    };
    let body = data.get(at..).ok_or(Error::Truncated)?;
    let wide = root & ROOT_WIDE != 0;

    let mut walker = SpanWalker { depth: 0, rows: 1 };
    let fields = walker.run(plan_schema, body, wide, at)?;

    let mut sink = JsonSink::new();
    sink.begin_object();
    sink.key_bytes(b"totalBytes");
    sink.signed(data.len() as i64);
    sink.key_bytes(b"schemaBytes");
    sink.signed(section_bytes as i64);
    sink.key_bytes(b"bodyBytes");
    sink.signed((data.len() - at) as i64);
    sink.key_bytes(b"rootBytes");
    sink.signed(1);
    sink.key_bytes(b"wide");
    sink.boolean(wide);
    sink.key_bytes(b"rows");
    sink.signed(i64::from(walker.rows));
    sink.key_bytes(b"envelope");
    sink.boolean(plan_schema.root().is_envelope);
    sink.key_bytes(b"fields");
    write_nodes(&mut sink, &fields);
    sink.end_object();
    Ok(sink.finish())
}

fn write_nodes(sink: &mut JsonSink, nodes: &[Node]) {
    sink.begin_array();
    for node in nodes {
        sink.begin_object();
        sink.key_bytes(b"name");
        sink.text_bytes(node.name.as_bytes());
        sink.key_bytes(b"key");
        sink.signed(i64::from(node.key));
        sink.key_bytes(b"type");
        sink.text_bytes(node.type_name.as_bytes());
        sink.key_bytes(b"optional");
        sink.boolean(node.optional);
        sink.key_bytes(b"start");
        sink.signed(node.start as i64);
        sink.key_bytes(b"end");
        sink.signed(node.end as i64);
        sink.key_bytes(b"bytes");
        sink.signed(node.bytes() as i64);
        sink.key_bytes(b"children");
        write_nodes(sink, &node.children);
        sink.end_object();
    }
    sink.end_array();
}

struct SpanWalker {
    depth: u32,
    /// How many records the document holds: the element count of the root's list
    /// or table, or one for a lone record.
    ///
    /// Taken at depth one only. A nested list deeper in is a field of a record,
    /// not a count of them, and letting it win would have made an invoice with
    /// three lines report three records.
    rows: i32,
}

impl SpanWalker {
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

    fn run(
        &mut self,
        schema: &Schema,
        body: &[u8],
        wide: bool,
        offset: usize,
    ) -> Result<Vec<Node>, Error> {
        let plan = schema.root();
        self.run_plan(schema, plan, body, wide, offset)
    }

    fn run_plan(
        &mut self,
        schema: &Schema,
        plan: &Plan,
        body: &[u8],
        wide: bool,
        offset: usize,
    ) -> Result<Vec<Node>, Error> {
        if wide {
            self.wide_run(schema, plan, body, offset)
        } else {
            self.narrow_run(schema, plan, body, offset)
        }
    }

    fn sub_plan<'s>(&self, schema: &'s Schema, field: &PlanField) -> Result<&'s Plan, Error> {
        let at = field.sub.ok_or(Error::BadSection)?;
        schema.plan(at).ok_or(Error::BadSection)
    }

    fn narrow_run(
        &mut self,
        schema: &Schema,
        plan: &Plan,
        body: &[u8],
        offset: usize,
    ) -> Result<Vec<Node>, Error> {
        self.enter()?;
        let mut reader = Reader::new(body);
        let mut out = Vec::new();
        while reader.more() {
            let start = reader.cursor();
            let key = reader.key();
            let index = plan.field_of(key).ok_or(Error::UnknownKey(key))?;
            let field = &plan.fields[index];
            let mut node = Node {
                name: plan.names.get(index).cloned().unwrap_or_default(),
                key: i32::from(field.key),
                type_name: type_name(field),
                optional: field.op == OP_POINTER,
                start: 0,
                end: 0,
                children: Vec::new(),
            };
            self.narrow_value(&mut reader, schema, field, &mut node, offset)?;
            node.start = offset + start;
            node.end = offset + reader.cursor();
            out.push(node);
        }
        reader.err()?;
        self.leave();
        Ok(out)
    }

    fn narrow_value(
        &mut self,
        reader: &mut Reader<'_>,
        schema: &Schema,
        field: &PlanField,
        node: &mut Node,
        offset: usize,
    ) -> Result<(), Error> {
        match field.op {
            OP_STRUCT => {
                let (body, wide) = reader.struct_body().ok_or(Error::Truncated)?;
                let inner = offset + reader.cursor() - body.len();
                let sub = self.sub_plan(schema, field)?;
                node.children = self.run_plan(schema, sub, body, wide, inner)?;
            }
            OP_STRUCTS => {
                if reader.is_table() {
                    self.narrow_table(reader, schema, field, node, offset)?;
                } else {
                    self.narrow_list(reader, schema, field, node, offset)?;
                }
            }
            OP_MAP => {
                // A span walk does not descend into a map: there is no per-entry
                // field id, and the tiling is exact at the map's own span.
                let _ = reader.composite_body().ok_or(Error::Truncated)?;
            }
            op => self.narrow_scalar(reader, if op == OP_POINTER { field.elem_op } else { op })?,
        }
        reader.err()
    }

    fn narrow_scalar(&mut self, reader: &mut Reader<'_>, op: u8) -> Result<(), Error> {
        match op {
            OP_BOOL | OP_UINT8..=OP_UINT64 | OP_FLOAT32 | OP_FLOAT64 => {
                let _ = reader.u64();
            }
            OP_INT8..=OP_INT64 => {
                let _ = reader.i64();
            }
            OP_STRING => reader.skip_string(),
            OP_BYTES => {
                let _ = reader.bytes();
            }
            OP_STRINGS => {
                let _ = reader.strings_bytes();
            }
            op if plan::array_element_op(op) != plan::OP_COUNT => {
                let mut dump: Vec<i64> = Vec::new();
                reader.ints_into(&mut dump);
            }
            op => return Err(Error::UnwalkableOp(op)),
        }
        reader.err()
    }

    fn narrow_list(
        &mut self,
        reader: &mut Reader<'_>,
        schema: &Schema,
        field: &PlanField,
        node: &mut Node,
        offset: usize,
    ) -> Result<(), Error> {
        let before = reader.cursor();
        let (count, mut elements) = reader.counted().ok_or(Error::Truncated)?;
        let body_end = offset + reader.cursor();
        let body_at = body_end - elements.buf_len();
        let sub = self.sub_plan(schema, field)?;
        if self.depth == 1 {
            self.rows = i32::try_from(count).unwrap_or(i32::MAX);
        }
        self.enter()?;
        let sub_wide = sub.is_wide;
        for index in 0..count {
            let start = elements.cursor();
            let element = elements.element().ok_or(Error::Truncated)?;
            if index < ELEMENT_LIMIT {
                let inner = body_at + elements.cursor() - element.len();
                let child = Node {
                    name: format!("[{index}]"),
                    key: i32::try_from(index).unwrap_or(i32::MAX),
                    type_name: String::from("struct"),
                    optional: false,
                    start: body_at + start,
                    end: body_at + elements.cursor(),
                    children: self.run_plan(schema, sub, element, sub_wide, inner)?,
                };
                node.children.push(child);
            } else if index == ELEMENT_LIMIT {
                node.children.push(Node {
                    name: format!("… {} more", count - ELEMENT_LIMIT),
                    key: i32::try_from(index).unwrap_or(i32::MAX),
                    type_name: String::from("struct"),
                    optional: false,
                    start: body_at + start,
                    end: body_at + elements.buf_len(),
                    children: Vec::new(),
                });
            }
        }
        elements.err()?;
        self.leave();
        if count > ELEMENT_LIMIT
            && let Some(tail) = node.children.last_mut()
        {
            tail.end = body_at + elements.buf_len();
        }
        if before == reader.cursor() {
            return Err(Error::Truncated);
        }
        Ok(())
    }

    fn narrow_table(
        &mut self,
        reader: &mut Reader<'_>,
        schema: &Schema,
        field: &PlanField,
        node: &mut Node,
        offset: usize,
    ) -> Result<(), Error> {
        let (rows, mut columns) = reader.counted().ok_or(Error::Truncated)?;
        let body_end = offset + reader.cursor();
        let body_at = body_end - columns.buf_len();
        let sub = self.sub_plan(schema, field)?;
        if self.depth == 1 {
            self.rows = i32::try_from(rows).unwrap_or(i32::MAX);
        }
        self.enter()?;
        while columns.more() {
            let start = columns.cursor();
            let key = columns.key();
            let index = sub.field_of(key).ok_or(Error::UnknownKey(key))?;
            let column = &sub.fields[index];
            if column.op == OP_STRING {
                let _ = columns.strings_bytes();
            } else {
                let _ = columns.composite_body().ok_or(Error::Truncated)?;
            }
            columns.err()?;
            node.children.push(Node {
                name: sub.names.get(index).cloned().unwrap_or_default(),
                key: i32::from(column.key),
                type_name: format!("{} column", type_name(column)),
                optional: false,
                start: body_at + start,
                end: body_at + columns.cursor(),
                children: Vec::new(),
            });
        }
        columns.err()?;
        self.leave();
        Ok(())
    }

    fn wide_run(
        &mut self,
        schema: &Schema,
        plan: &Plan,
        body: &[u8],
        offset: usize,
    ) -> Result<Vec<Node>, Error> {
        self.enter()?;
        let mut reader = Reader8::new(body);
        let mut out = Vec::new();
        while reader.more() {
            let start = reader.cursor();
            let key = reader.key();
            let Some(index) = plan.field_of(key) else {
                if !reader.skip() {
                    reader.err()?;
                    break;
                }
                out.push(Node {
                    name: format!("field {key}"),
                    key: i32::from(key),
                    type_name: String::from("unknown"),
                    optional: false,
                    start: offset + start,
                    end: offset + reader.cursor(),
                    children: Vec::new(),
                });
                continue;
            };
            let field = &plan.fields[index];
            let mut node = Node {
                name: plan.names.get(index).cloned().unwrap_or_default(),
                key: i32::from(field.key),
                type_name: type_name(field),
                optional: field.op == OP_POINTER,
                start: 0,
                end: 0,
                children: Vec::new(),
            };
            self.wide_value(&mut reader, schema, field, &mut node, offset)?;
            node.start = offset + start;
            node.end = offset + reader.cursor();
            out.push(node);
        }
        reader.err()?;
        self.leave();
        Ok(out)
    }

    fn wide_value(
        &mut self,
        reader: &mut Reader8<'_>,
        schema: &Schema,
        field: &PlanField,
        node: &mut Node,
        offset: usize,
    ) -> Result<(), Error> {
        match field.op {
            OP_STRUCT => {
                let (body, wide) = reader.struct_body().ok_or(Error::Truncated)?;
                let inner = offset + reader.cursor() - body.len();
                let sub = self.sub_plan(schema, field)?;
                node.children = self.run_plan(schema, sub, body, wide, inner)?;
            }
            OP_STRUCTS => {
                if reader.is_table() {
                    self.wide_table(reader, schema, field, node, offset)?;
                } else {
                    self.wide_list(reader, schema, field, node, offset)?;
                }
            }
            _ => {
                if !reader.skip() {
                    return reader.err();
                }
            }
        }
        reader.err()
    }

    fn wide_list(
        &mut self,
        reader: &mut Reader8<'_>,
        schema: &Schema,
        field: &PlanField,
        node: &mut Node,
        offset: usize,
    ) -> Result<(), Error> {
        let before = reader.cursor();
        let (count, mut inner) = reader.list().ok_or(Error::Truncated)?;
        let body_end = offset + reader.cursor();
        let body_at = body_end - inner.buf_len();
        let sub = self.sub_plan(schema, field)?;
        if self.depth == 1 {
            self.rows = i32::try_from(count).unwrap_or(i32::MAX);
        }
        self.enter()?;
        for index in 0..count {
            let start = inner.cursor();
            let (element, wide) = inner.element_struct_body().ok_or(Error::Truncated)?;
            if index < ELEMENT_LIMIT {
                let at = body_at + inner.cursor() - element.len();
                let child = Node {
                    name: format!("[{index}]"),
                    key: i32::try_from(index).unwrap_or(i32::MAX),
                    type_name: String::from("struct"),
                    optional: false,
                    start: body_at + start,
                    end: body_at + inner.cursor(),
                    children: self.run_plan(schema, sub, element, wide, at)?,
                };
                node.children.push(child);
            } else if index == ELEMENT_LIMIT {
                node.children.push(Node {
                    name: format!("… {} more", count - ELEMENT_LIMIT),
                    key: i32::try_from(index).unwrap_or(i32::MAX),
                    type_name: String::from("struct"),
                    optional: false,
                    start: body_at + start,
                    end: body_at + inner.buf_len(),
                    children: Vec::new(),
                });
            }
        }
        inner.err()?;
        self.leave();
        if count > ELEMENT_LIMIT
            && let Some(tail) = node.children.last_mut()
        {
            tail.end = body_at + inner.buf_len();
        }
        if before == reader.cursor() {
            return Err(Error::Truncated);
        }
        Ok(())
    }

    fn wide_table(
        &mut self,
        reader: &mut Reader8<'_>,
        schema: &Schema,
        field: &PlanField,
        node: &mut Node,
        offset: usize,
    ) -> Result<(), Error> {
        let (rows, mut columns) = reader.table().ok_or(Error::Truncated)?;
        let body_end = offset + reader.cursor();
        let body_at = body_end - columns.buf_len();
        let sub = self.sub_plan(schema, field)?;
        if self.depth == 1 {
            self.rows = i32::try_from(rows).unwrap_or(i32::MAX);
        }
        self.enter()?;
        while columns.more() {
            let start = columns.cursor();
            let key = columns.key();
            let Some(index) = sub.field_of(key) else {
                if !columns.skip() {
                    break;
                }
                continue;
            };
            let column = &sub.fields[index];
            if !columns.skip() {
                columns.err()?;
                break;
            }
            columns.err()?;
            node.children.push(Node {
                name: sub.names.get(index).cloned().unwrap_or_default(),
                key: i32::from(column.key),
                type_name: format!("{} column", type_name(column)),
                optional: false,
                start: body_at + start,
                end: body_at + columns.cursor(),
                children: Vec::new(),
            });
        }
        columns.err()?;
        self.leave();
        Ok(())
    }
}

/// What a field is called in the tree. Names the *wire* shape rather than the
/// Go type, because that is what the spans beside it are showing.
fn type_name(field: &PlanField) -> String {
    let op = if field.op == OP_POINTER {
        field.elem_op
    } else {
        field.op
    };
    String::from(match op {
        OP_BOOL => "bool",
        OP_INT8 => "int8",
        OP_INT16 => "int16",
        OP_INT32 => "int32",
        OP_INT64 => "int64",
        OP_UINT8 => "uint8",
        OP_UINT16 => "uint16",
        OP_UINT32 => "uint32",
        OP_UINT64 => "uint64",
        OP_FLOAT32 => "float32",
        OP_FLOAT64 => "float64",
        OP_STRING => "string",
        OP_BYTES => "bytes",
        OP_INT8S => "[]int8",
        OP_INT16S => "[]int16",
        OP_INT32S => "[]int32",
        OP_INT64S => "[]int64",
        OP_UINT16S => "[]uint16",
        OP_UINT32S => "[]uint32",
        OP_UINT64S => "[]uint64",
        OP_STRINGS => "[]string",
        OP_STRUCT => "struct",
        OP_STRUCTS => "[]struct",
        OP_MAP => "map",
        OP_ANY => "any",
        OP_ANYS => "[]any",
        _ => "unknown",
    })
}
