//! The materializer against the corpus, and a hand-built case for the shape
//! only a table reaches: a column whose value is past 2^53, which is exactly
//! the failure `JSON.parse` would have introduced and the fast path exists to
//! avoid re-introducing.
//!
//! The corpus comparison is against **Go's own JSON**, not against
//! `walk::to_json` — the two sinks share a walk but not a codec path, so
//! comparing them to each other would miss a bug the two happened to share.
//! Only cases where the root is a shape the materializer covers (an envelope,
//! or a root struct whose every field is a table) are checked here; everything
//! else returns `Ok(None)` and is `walk.rs`'s test's responsibility.

use colbin::materialize::message_to_buffer;
use colbin::section;

fn unhex(text: &str) -> Vec<u8> {
    assert!(text.len().is_multiple_of(2), "odd-length hex: {text}");
    (0..text.len())
        .step_by(2)
        .map(|at| u8::from_str_radix(&text[at..at + 2], 16).expect("hex"))
        .collect()
}

fn corpus() -> serde_json::Value {
    let path = concat!(env!("CARGO_MANIFEST_DIR"), "/vectors/vectors.json");
    let text = std::fs::read_to_string(path).expect("vectors.json — run `go run ./rust/vectors`");
    serde_json::from_str(&text).expect("vectors.json is not JSON")
}

fn documents(corpus: &serde_json::Value) -> Vec<&serde_json::Value> {
    corpus["walks"]
        .as_array()
        .expect("walks")
        .iter()
        .filter(|case| case["name"].as_str().is_some_and(|n| n.starts_with("doc.")))
        .collect()
}

fn delivery(case: &serde_json::Value) -> (section::Schema, Vec<u8>) {
    let name = case["name"].as_str().expect("name");
    let schema = section::parse(&unhex(case["section"].as_str().expect("section")))
        .unwrap_or_else(|e| panic!("{name}: {e}"));
    let message = unhex(case["message"].as_str().expect("message"));
    (schema, message)
}

/// One materialized value, still carrying the kind that says how to compare
/// it — comparing through `serde_json::Value` directly is a trap here, since
/// `Number`'s equality distinguishes an integer-token from a float-token
/// representation, and a whole-number float column (`3.0` written as `"3"` by
/// Go) would otherwise look unequal to itself.
enum Cell {
    Bool(bool),
    Signed(i64),
    Unsigned(u64),
    Float(f64),
    Str(String),
}

fn cell_matches(got: &Cell, want: &serde_json::Value, path: &str) {
    let ok = match got {
        Cell::Bool(v) => want.as_bool() == Some(*v),
        Cell::Signed(v) => want.as_i64() == Some(*v),
        Cell::Unsigned(v) => want.as_u64() == Some(*v),
        Cell::Float(v) => want.as_f64() == Some(*v),
        Cell::Str(v) => want.as_str() == Some(v.as_str()),
    };
    assert!(
        ok,
        "{path}: materialized value disagrees with Go's JSON ({want})"
    );
}

fn read_u16(buf: &[u8], at: usize) -> u16 {
    u16::from_le_bytes(buf[at..at + 2].try_into().unwrap())
}

fn read_u32(buf: &[u8], at: usize) -> u32 {
    u32::from_le_bytes(buf[at..at + 4].try_into().unwrap())
}

type Rows = Vec<Vec<(String, Option<Cell>)>>;

/// What a buffer decodes to: the one table's rows for an envelope, or the root
/// object's tables by name, in the order the buffer holds them.
enum Decoded {
    Array(Rows),
    Object(Vec<(String, Rows)>),
}

impl Decoded {
    fn rows(self) -> Rows {
        match self {
            Decoded::Array(rows) => rows,
            Decoded::Object(mut tables) => {
                assert_eq!(tables.len(), 1, "expected a single table");
                tables.remove(0).1
            }
        }
    }
}

/// A second, independent reader of the buffer `materialize::to_buffer` writes
/// — deliberately not sharing code with it, so a bug in the writer has to
/// also be a bug in this reader to slip past the comparison below.
fn decode_buffer(buf: &[u8]) -> Decoded {
    assert_eq!(read_u32(buf, 0), 2, "buffer version");
    let shape = read_u32(buf, 4);
    let table_count = read_u32(buf, 8) as usize;
    let mut at = 12;
    let mut tables = Vec::with_capacity(table_count);
    for _ in 0..table_count {
        let name_len = read_u16(buf, at) as usize;
        let name = String::from_utf8(buf[at + 2..at + 2 + name_len].to_vec()).expect("UTF-8 name");
        at = (at + 2 + name_len).next_multiple_of(8);
        let (rows, next) = decode_table(buf, at);
        at = next;
        tables.push((name, rows));
    }
    match shape {
        0 => {
            assert_eq!(tables.len(), 1, "an envelope holds one table");
            assert_eq!(tables[0].0, "", "an envelope's table has no name");
            Decoded::Array(tables.remove(0).1)
        }
        1 => Decoded::Object(tables),
        other => panic!("unknown shape {other}"),
    }
}

fn decode_table(buf: &[u8], start: usize) -> (Rows, usize) {
    let rows = read_u32(buf, start) as usize;
    let field_count = read_u32(buf, start + 4) as usize;

    struct Header {
        kind: u8,
        flags: u8,
        name: String,
    }

    let mut at = start + 8;
    let mut headers = Vec::with_capacity(field_count);
    for _ in 0..field_count {
        let kind = buf[at];
        let flags = buf[at + 1];
        let name_len = read_u16(buf, at + 2) as usize;
        let name = String::from_utf8(buf[at + 4..at + 4 + name_len].to_vec()).expect("UTF-8 name");
        at += 4 + name_len;
        headers.push(Header { kind, flags, name });
    }
    at = at.next_multiple_of(8);

    let mut rows_out: Rows = (0..rows).map(|_| Vec::with_capacity(field_count)).collect();
    for header in &headers {
        let valid: Option<Vec<bool>> = if header.flags & 0b1000 != 0 {
            let bits = (0..rows)
                .map(|row| (buf[at + (row >> 3)] >> (row & 7)) & 1 == 1)
                .collect();
            at = (at + rows.div_ceil(8)).next_multiple_of(8);
            Some(bits)
        } else {
            None
        };
        let mut cells: Vec<Cell> = Vec::with_capacity(rows);
        match header.kind {
            0 => {
                for row in 0..rows {
                    let bit = (buf[at + (row >> 3)] >> (row & 7)) & 1;
                    cells.push(Cell::Bool(bit == 1));
                }
                at += rows.div_ceil(8);
            }
            1 => {
                let signed = header.flags & 0b001 != 0;
                for row in 0..rows {
                    let bits =
                        u64::from_le_bytes(buf[at + row * 8..at + row * 8 + 8].try_into().unwrap());
                    cells.push(if signed {
                        Cell::Signed(bits as i64)
                    } else {
                        Cell::Unsigned(bits)
                    });
                }
                at += rows * 8;
            }
            2 => {
                for row in 0..rows {
                    let value =
                        f64::from_le_bytes(buf[at + row * 8..at + row * 8 + 8].try_into().unwrap());
                    cells.push(Cell::Float(value));
                }
                at += rows * 8;
            }
            3 => {
                let offsets_at = at;
                let mut offsets = Vec::with_capacity(rows + 1);
                for i in 0..=rows {
                    offsets.push(read_u32(buf, offsets_at + i * 4) as usize);
                }
                let blob_at = offsets_at + (rows + 1) * 4;
                let blob_len = offsets[rows];
                let blob = &buf[blob_at..blob_at + blob_len];
                for row in 0..rows {
                    let text = std::str::from_utf8(&blob[offsets[row]..offsets[row + 1]])
                        .expect("UTF-8 blob");
                    cells.push(Cell::Str(text.to_string()));
                }
                at = blob_at + blob_len;
            }
            other => panic!("unknown column kind {other}"),
        }
        at = at.next_multiple_of(8);
        for (row, cell) in cells.into_iter().enumerate() {
            let present = valid.as_ref().is_none_or(|valid| valid[row]);
            rows_out[row].push((header.name.clone(), present.then_some(cell)));
        }
    }
    (rows_out, at)
}

fn rows_match(name: &str, got_rows: &Rows, want_rows: &[serde_json::Value]) {
    assert_eq!(got_rows.len(), want_rows.len(), "{name}: row count");
    for (row, (got_row, want_row)) in got_rows.iter().zip(want_rows).enumerate() {
        let want_object = want_row.as_object().expect("a row is an object");
        assert_eq!(
            got_row.len(),
            want_object.len(),
            "{name}: row {row}: field count"
        );
        for (field, cell) in got_row {
            let want_value = want_object
                .get(field)
                .unwrap_or_else(|| panic!("{name}: row {row}: Go's object has no {field:?}"));
            let path = format!("{name}: row {row}.{field}");
            match cell {
                Some(cell) => cell_matches(cell, want_value, &path),
                None => assert!(
                    want_value.is_null(),
                    "{path}: null, but Go wrote {want_value}"
                ),
            }
        }
    }
}

#[test]
fn table_shaped_documents_materialize_to_what_go_wrote() {
    let corpus = corpus();
    let cases = documents(&corpus);
    assert!(cases.len() >= 40, "corpus shrank: {} cases", cases.len());

    let mut covered = Vec::new();
    for case in &cases {
        let name = case["name"].as_str().expect("name");
        let want_text = case["json"].as_str().expect("json");
        let (schema, body) = delivery(case);

        let buf = match message_to_buffer(Some(&schema), &body) {
            Ok(None) => continue,
            Ok(Some(buf)) => buf,
            Err(e) => panic!("{name}: {e}"),
        };
        covered.push(name);

        let want: serde_json::Value = serde_json::from_str(want_text)
            .unwrap_or_else(|e| panic!("{name}: Go's own JSON: {e}"));
        match decode_buffer(&buf) {
            Decoded::Array(rows) => {
                let want_rows = want.as_array().unwrap_or_else(|| {
                    panic!("{name}: materialized an array but Go's JSON root is not one")
                });
                rows_match(name, &rows, want_rows);
            }
            Decoded::Object(tables) => {
                let want_object = want.as_object().unwrap_or_else(|| {
                    panic!("{name}: materialized an object but Go's JSON root is not one")
                });
                assert_eq!(tables.len(), want_object.len(), "{name}: root field count");
                for (field, rows) in &tables {
                    let want_rows = want_object[field]
                        .as_array()
                        .unwrap_or_else(|| panic!("{name}: {field} is not an array in Go's JSON"));
                    rows_match(&format!("{name}.{field}"), rows, want_rows);
                }
            }
        }
    }

    // The shapes the fast path grew to cover, so a regression to the JSON path
    // is a failure here rather than a silent slowdown.
    for must in [
        "doc.records",
        "doc.nullable-columns",
        "doc.nullable-columns-all-present",
        "doc.dictionary-columns",
        "doc.grouped-records",
        "doc.nulls-and-missing-keys",
        "doc.paged-table",
    ] {
        assert!(
            covered.contains(&must),
            "{must} no longer materializes: {covered:?}"
        );
    }
    assert!(
        !covered.is_empty(),
        "no case in the corpus exercised the table fast path — check the corpus still has an \
         enveloped (bare-array root) table case"
    );
}

#[test]
fn a_truncated_table_message_is_refused_rather_than_panicking() {
    let corpus = corpus();
    for case in documents(&corpus) {
        let (schema, body) = delivery(case);
        if !matches!(message_to_buffer(Some(&schema), &body), Ok(Some(_))) {
            continue;
        }
        for cut in 0..body.len() {
            let _ = message_to_buffer(Some(&schema), &body[..cut]);
        }
    }
}

/// INTERNALS.md §16.3's case: an integer past 2^53, which `JSON.parse` would
/// have rounded before a JS caller ever saw it. Built by hand because the
/// corpus's own table cases do not happen to carry one.
#[test]
fn a_table_column_past_2_53_is_flagged_and_exact() {
    use colbin::build;
    use colbin::diag::Diag;

    const SNOWFLAKE: i64 = 9_007_199_254_740_993; // 2^53 + 1
    let rows: Vec<String> = (0..12u32)
        .map(|i| format!(r#"{{"id":{i},"big":{SNOWFLAKE}}}"#))
        .collect();
    let json = format!("[{}]", rows.join(","));

    let mut diag = Diag::new();
    let encoded = build::encode(json.as_bytes(), 0, &mut diag)
        .unwrap_or_else(|| panic!("refused to encode: {:?}", diag.encode()));

    let schema = section::parse(&encoded.section).expect("section parses");
    let buf = message_to_buffer(Some(&schema), &encoded.message)
        .expect("a well-formed message")
        .expect("a bare-array root past the table threshold is this fast path's shape");
    let rows = decode_buffer(&buf).rows();
    assert_eq!(rows.len(), 12);
    for (i, row) in rows.iter().enumerate() {
        let (_, big) = row
            .iter()
            .find(|(name, _)| name == "big")
            .expect("big column");
        match big {
            Some(Cell::Signed(v)) => assert_eq!(*v, SNOWFLAKE, "row {i}"),
            _ => panic!("row {i}: expected a signed column"),
        }
    }
}
