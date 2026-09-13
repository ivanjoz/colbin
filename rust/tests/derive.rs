//! `#[derive(Colbin)]` end to end: every shape the derive carries, both key
//! widths, and the composites.

use std::collections::{BTreeMap, HashMap};

use colbin::{Colbin, Error};

/// The ten-field benchmark record the Go side measures, narrow-keyed.
#[derive(Colbin, Debug, PartialEq)]
struct Charge {
    #[cb(1)]
    company_id: u32,
    #[cb(2)]
    user_id: u32,
    #[cb(3)]
    route_id: u32,
    #[cb(4)]
    cpu: u8,
    #[cb(5)]
    memory: u16,
    #[cb(6)]
    duration: i64,
    #[cb(7)]
    access_1: u16,
    #[cb(8)]
    access_2: u16,
    #[cb(9)]
    created: i64,
    #[cb(10)]
    updated: i64,
}

#[test]
fn the_benchmark_record_is_the_bytes_the_plan_documents() {
    let charge = Charge {
        company_id: 7,
        user_id: 42,
        route_id: 103,
        cpu: 5,
        access_1: 0x0139,
        ..Charge::colbin_zero()
    };
    let message = charge.encode();
    // BYTE_ALIGNED_PLAN.md §2.6's worked example, ten bytes rather than its
    // twelve: an unsigned field spends no sign bit, so all sixteen nibble codes
    // carry information and 0..=7 are the value itself. 7 and 5 are therefore a
    // whole field in one byte each. The same bytes are pinned against Go in
    // tests/vectors.rs under `charge.benchmark`.
    assert_eq!(
        message,
        vec![
            0xD0, // root: STRUCT, narrow keys — the frame bounds it
            0x07, // key 0, the value 7 inline
            0x18, 0x2A, // key 1, one magnitude byte: 42
            0x28, 0x67, // key 2, one magnitude byte: 103
            0x35, // key 3, the value 5 inline
            0x69, 0x39, 0x01, // key 6, two magnitude bytes: 0x0139, low byte first
        ]
    );
    assert_eq!(Charge::decode(&message).expect("decode"), charge);
}

#[test]
fn an_omitted_field_reads_back_as_zero() {
    let message = Charge::colbin_zero().encode();
    assert_eq!(
        message,
        vec![0xD0],
        "every field is zero, so nothing is written"
    );
    assert_eq!(
        Charge::decode(&message).expect("decode"),
        Charge::colbin_zero()
    );
}

#[derive(Colbin, Debug, PartialEq)]
struct Scalars {
    #[cb(1)]
    flag: bool,
    #[cb(2)]
    tiny: i8,
    #[cb(3)]
    small: i16,
    #[cb(4)]
    medium: i32,
    #[cb(5)]
    large: i64,
    #[cb(6)]
    byte: u8,
    #[cb(7)]
    half: u16,
    #[cb(8)]
    word: u32,
    #[cb(9)]
    giant: u64,
    #[cb(10)]
    single: f32,
    #[cb(11)]
    double: f64,
    #[cb(12)]
    text: String,
}

#[test]
fn every_scalar_round_trips_at_its_extremes() {
    for value in [
        Scalars {
            flag: true,
            tiny: i8::MIN,
            small: i16::MIN,
            medium: i32::MIN,
            large: i64::MIN,
            byte: u8::MAX,
            half: u16::MAX,
            word: u32::MAX,
            giant: u64::MAX,
            single: f32::MIN,
            double: f64::MIN,
            text: "el niño comió jamón".into(),
        },
        Scalars {
            flag: false,
            tiny: i8::MAX,
            small: i16::MAX,
            medium: i32::MAX,
            large: i64::MAX,
            byte: 1,
            half: 300,
            word: 70_000,
            giant: 1 << 63,
            single: 1.0,
            double: -0.5,
            text: String::new(),
        },
        Scalars::colbin_zero(),
    ] {
        let message = value.encode();
        assert_eq!(Scalars::decode(&message).expect("decode"), value);
    }
}

/// A float trims its low end where an integer trims its high one, which is the
/// whole of what the byte reversal buys.
#[test]
fn a_float_trims_its_low_end() {
    // The magnitude bytes of the one field set: the message is the root
    // descriptor, the field header, and those.
    let magnitude = |value: Scalars| value.encode().len() - 2;

    // A round value is nearly all zero mantissa, so it costs two bytes.
    assert_eq!(
        magnitude(Scalars {
            single: 1.0,
            ..Scalars::colbin_zero()
        }),
        2
    );
    assert_eq!(
        magnitude(Scalars {
            double: 1.0,
            ..Scalars::colbin_zero()
        }),
        2
    );
    assert_eq!(
        magnitude(Scalars {
            double: 0.5,
            ..Scalars::colbin_zero()
        }),
        2
    );

    // An f64 holding a value that is exactly an f32 has twenty-nine zero low
    // bits — three whole bytes and five over, and only whole bytes trim.
    assert_eq!(
        magnitude(Scalars {
            double: f64::from(0.1_f32),
            ..Scalars::colbin_zero()
        }),
        5
    );

    // And nothing is claimed for a value with a full mantissa.
    assert_eq!(
        magnitude(Scalars {
            double: core::f64::consts::PI,
            ..Scalars::colbin_zero()
        }),
        8
    );
}

#[derive(Colbin, Debug, PartialEq)]
struct Arrays {
    #[cb(1)]
    blob: Vec<u8>,
    #[cb(2)]
    tiny: Vec<i8>,
    #[cb(3)]
    small: Vec<i16>,
    #[cb(4)]
    medium: Vec<i32>,
    #[cb(5)]
    large: Vec<i64>,
    #[cb(6)]
    halves: Vec<u16>,
    #[cb(7)]
    words: Vec<u32>,
    #[cb(8)]
    giants: Vec<u64>,
    #[cb(9)]
    texts: Vec<String>,
}

#[test]
fn every_array_round_trips() {
    let value = Arrays {
        blob: vec![0, 1, 2, 255],
        tiny: vec![i8::MIN, 0, i8::MAX],
        small: vec![i16::MIN, 0, i16::MAX],
        medium: vec![i32::MIN, 0, i32::MAX],
        large: vec![i64::MIN, 0, i64::MAX],
        halves: vec![0, u16::MAX],
        words: vec![0, u32::MAX],
        giants: vec![0, u64::MAX],
        texts: vec![String::new(), "a".repeat(300), "ok".into()],
    };
    let message = value.encode();
    assert_eq!(Arrays::decode(&message).expect("decode"), value);
    assert_eq!(
        Arrays::decode(&Arrays::colbin_zero().encode()).expect("decode"),
        Arrays::colbin_zero()
    );
}

/// A blob past 2047 bytes escapes to a wider size field rather than to a
/// continuation run.
#[test]
fn a_long_blob_escapes_its_size() {
    for length in [0, 1, 2047, 2048, 70_000] {
        let value = Arrays {
            blob: vec![7; length],
            ..Arrays::colbin_zero()
        };
        let back = Arrays::decode(&value.encode()).expect("decode");
        assert_eq!(back.blob.len(), length);
    }
}

#[derive(Colbin, Debug, PartialEq)]
struct Optionals {
    #[cb(1)]
    maybe_int: Option<i32>,
    #[cb(2)]
    maybe_uint: Option<u32>,
    #[cb(3)]
    maybe_text: Option<String>,
    #[cb(4)]
    maybe_flag: Option<bool>,
    #[cb(5)]
    maybe_float: Option<f64>,
}

/// An absent key means `None`, so a `Some` holding a zero has to say so out
/// loud — the one place this format writes a zero rather than omitting it.
#[test]
fn a_present_zero_is_not_an_absent_field() {
    let none = Optionals::colbin_zero();
    assert_eq!(none.encode(), vec![0xD0]);
    assert_eq!(Optionals::decode(&none.encode()).expect("decode"), none);

    let zero = Optionals {
        maybe_int: Some(0),
        maybe_uint: Some(0),
        maybe_text: Some(String::new()),
        maybe_flag: Some(false),
        maybe_float: Some(0.0),
    };
    assert_eq!(Optionals::decode(&zero.encode()).expect("decode"), zero);

    let set = Optionals {
        maybe_int: Some(-5),
        maybe_uint: Some(70_000),
        maybe_text: Some("x".into()),
        maybe_flag: Some(true),
        maybe_float: Some(2.5),
    };
    assert_eq!(Optionals::decode(&set.encode()).expect("decode"), set);
}

#[derive(Colbin, Debug, PartialEq, Clone)]
struct Line {
    #[cb(1)]
    sku: String,
    #[cb(2)]
    quantity: i32,
    #[cb(3)]
    price: f64,
}

#[derive(Colbin, Debug, PartialEq)]
struct Order {
    #[cb(1)]
    id: u32,
    #[cb(2)]
    customer: Line,
    #[cb(3)]
    lines: Vec<Line>,
    #[cb(4)]
    note: String,
}

#[test]
fn composites_round_trip() {
    let order = Order {
        id: 90210,
        customer: Line {
            sku: "ACME".into(),
            quantity: 1,
            price: 0.0,
        },
        lines: (0..3)
            .map(|index| Line {
                sku: format!("SKU-{index}"),
                quantity: index,
                price: f64::from(index) * 1.5,
            })
            .collect(),
        note: "ship soon".into(),
    };
    let message = order.encode();
    assert_eq!(Order::decode(&message).expect("decode"), order);
}

/// Past the threshold the writer transposes, which is a different class on the
/// wire — so the reader dispatches on what it finds rather than on what it was
/// told.
#[test]
fn a_long_slice_of_structs_becomes_a_table() {
    for rows in [1, 7, colbin::TABLE_THRESHOLD, 200] {
        let order = Order {
            id: 1,
            customer: Line {
                sku: String::new(),
                quantity: 0,
                price: 0.0,
            },
            lines: (0..rows)
                .map(|index| Line {
                    sku: format!("SKU-{index}"),
                    quantity: index as i32,
                    price: index as f64,
                })
                .collect(),
            note: String::new(),
        };
        let message = order.encode();
        assert_eq!(
            Order::decode(&message).expect("decode"),
            order,
            "{rows} rows"
        );
        // A list and a table are different classes, so what the writer chose is
        // on the wire rather than in the reader's expectations. Walk to the
        // lines and ask the field what it is. (A nested struct is written even
        // when it is empty — it is a length and nothing else — so the customer
        // is there to be stepped past.)
        let mut reader = colbin::wire::Reader::new(&message[1..]);
        assert_eq!(reader.key(), 0);
        let _ = reader.u32();
        assert_eq!(reader.key(), 1);
        let _ = reader.struct_body();
        assert_eq!(reader.key(), 2);
        assert_eq!(
            reader.is_table(),
            rows >= colbin::TABLE_THRESHOLD,
            "{rows} rows took the wrong shape"
        );
    }
}

#[derive(Colbin, Debug, PartialEq)]
struct Maps {
    #[cb(1)]
    labels: BTreeMap<String, String>,
    #[cb(2)]
    counts: BTreeMap<i64, f64>,
    #[cb(3)]
    flags: HashMap<String, bool>,
}

#[test]
fn maps_round_trip() {
    let mut value = Maps::colbin_zero();
    value.labels.insert("a".into(), "one".into());
    value.labels.insert("b".into(), String::new());
    value.counts.insert(-1, 2.5);
    value.counts.insert(0, 0.0);
    value.flags.insert("on".into(), true);
    let message = value.encode();
    assert_eq!(Maps::decode(&message).expect("decode"), value);
    assert_eq!(
        Maps::decode(&Maps::colbin_zero().encode()).expect("decode"),
        Maps::colbin_zero()
    );
}

/// A field id above fifteen puts the whole run on the wide path, which is what
/// buys 256 ids and the ability to step over an unknown field.
#[derive(Colbin, Debug, PartialEq)]
struct Wide {
    #[cb(1)]
    first: u32,
    #[cb(201)]
    last: String,
}

#[derive(Colbin, Debug, PartialEq)]
struct WideEvolved {
    #[cb(1)]
    first: u32,
    #[cb(201)]
    last: String,
    #[cb(202)]
    added: Vec<i32>,
    #[cb(203)]
    also: f64,
}

/// Checked at compile time, which is where a key width belongs.
const _: () = assert!(Wide::WIDE_KEYS);
const _: () = assert!(ForcedWide::WIDE_KEYS);
const _: () = assert!(Hashed::WIDE_KEYS);
const _: () = assert!(!Charge::WIDE_KEYS);

#[test]
fn a_wide_reader_steps_over_a_field_it_does_not_know() {
    let evolved = WideEvolved {
        first: 9,
        last: "tail".into(),
        added: vec![1, 2, 3],
        also: 1.5,
    };
    let message = evolved.encode();
    assert_eq!(message[0], colbin::ROOT_STRUCT_WIDE);
    let old = Wide::decode(&message).expect("an unknown wide field is skipped");
    assert_eq!(
        old,
        Wide {
            first: 9,
            last: "tail".into()
        }
    );
}

/// The narrow width cannot: four descriptor bits have no room for a class, so
/// nothing can size a field it cannot classify.
#[derive(Colbin, Debug, PartialEq)]
struct NarrowOne {
    #[cb(1)]
    first: u32,
}

#[test]
fn a_narrow_reader_refuses_a_field_it_does_not_know() {
    let two = Charge {
        company_id: 1,
        user_id: 2,
        ..Charge::colbin_zero()
    };
    assert_eq!(
        NarrowOne::decode(&two.encode()),
        Err(Error::UnknownKey(1)),
        "an unknown narrow key ends the run"
    );
}

/// An id that is not declared comes from the hash of the field's name, which
/// lands anywhere in 0..=255 — so such a type is wide whatever its ids turn out
/// to be.
#[derive(Colbin, Debug, PartialEq)]
struct Hashed {
    #[cb(name = "CompanyID")]
    company_id: i32,
    #[cb(name = "User")]
    user: String,
    #[cb(8)]
    pinned: u32,
    #[cb(skip)]
    ignored: u64,
}

#[test]
fn hashed_ids_round_trip_and_a_skipped_field_is_not_written() {
    let value = Hashed {
        company_id: -3,
        user: "ana".into(),
        pinned: 1000,
        ignored: 7,
    };
    let message = value.encode();
    let back = Hashed::decode(&message).expect("decode");
    assert_eq!(back.company_id, -3);
    assert_eq!(back.user, "ana");
    assert_eq!(back.pinned, 1000);
    assert_eq!(back.ignored, 0, "a skipped field is not carried");
}

/// A struct may ask for the wide width even when four bits would do, which is
/// what a wire that has to evolve wants.
#[derive(Colbin, Debug, PartialEq)]
#[cb(wide)]
struct ForcedWide {
    #[cb(1)]
    value: u32,
}

/// `packed5` is a per-field encoding code rather than a mode, so a message
/// written with it on reads back with it off.
#[derive(Colbin, Debug, PartialEq)]
#[cb(packed5)]
struct Packed {
    #[cb(1)]
    text: String,
}

#[derive(Colbin, Debug, PartialEq)]
#[cb(wide)]
struct Unpacked {
    #[cb(1)]
    text: String,
}

#[test]
fn a_packed_string_reads_back_through_the_plain_reader() {
    let packed = Packed {
        text: "el niño comió jamón".into(),
    };
    let plain = Unpacked {
        text: packed.text.clone(),
    };
    let message = packed.encode();
    assert!(
        message.len() < plain.encode().len(),
        "packing did not save anything"
    );
    assert_eq!(Packed::decode(&message).expect("decode"), packed);
    // The descriptor says which encoding it is, so the reader needs no telling.
    assert_eq!(
        Unpacked::decode(&message).expect("decode").text,
        packed.text
    );
    assert_eq!(
        Packed::decode(&plain.encode()).expect("decode").text,
        packed.text
    );
}

/// Every prefix of a valid message must produce an error rather than a panic.
#[test]
fn a_truncated_message_is_refused_rather_than_panicked_on() {
    let order = Order {
        id: 70_000,
        customer: Line {
            sku: "ACME".into(),
            quantity: -1,
            price: 2.5,
        },
        lines: (0..20)
            .map(|index| Line {
                sku: format!("SKU-{index}"),
                quantity: index,
                price: f64::from(index),
            })
            .collect(),
        note: "a".repeat(400),
    };
    let message = order.encode();
    for cut in 0..message.len() {
        let _ = Order::decode(&message[..cut]);
    }
    let evolved = WideEvolved {
        first: 70_000,
        last: "a".repeat(400),
        added: vec![-1; 40],
        also: 0.25,
    };
    let message = evolved.encode();
    for cut in 0..message.len() {
        let _ = WideEvolved::decode(&message[..cut]);
    }
}

/// A byte 0 that is not a root descriptor this version writes is refused, which
/// is what keeps an old message from being misparsed as a new one.
#[test]
fn a_message_that_is_not_one_is_refused() {
    assert_eq!(Charge::decode(&[]), Err(Error::Truncated));
    for root in [0x00_u8, 0x02, 0x04, 0x06, 0x08, 0xD1, 0xFF] {
        assert_eq!(Charge::decode(&[root]), Err(Error::BadRoot(root)));
    }
}

/// A list element is the one composite with no descriptor in front of it, so it cannot widen the
/// way every other one does — there is nothing to put an `lw` code in. Closing it like a keyed
/// composite wrote a bare four-byte length where the reader expects the 0xFF escape, and OR-ed 2
/// into whatever byte preceded the placeholder: a message this crate produced and refused, for any
/// element body reaching 255 bytes.
///
/// That is an ordinary record — a `Vec<T>` under the table threshold holding a couple of hundred
/// characters of text — not a corner. Go's `TestNarrowListElementWidths` pins the same sizes.
#[test]
fn a_list_element_past_the_inline_length_round_trips() {
    #[derive(Colbin, Clone, Debug, Default, PartialEq)]
    struct Row {
        #[cb(1)]
        id: i32,
        #[cb(2)]
        text: String,
    }

    #[derive(Colbin, Clone, Debug, Default, PartialEq)]
    struct Document {
        #[cb(1)]
        rows: Vec<Row>,
    }

    for size in [0_usize, 1, 200, 250, 251, 253, 254, 255, 256, 1_000, 70_000] {
        let document = Document {
            rows: vec![Row {
                id: 1,
                text: "x".repeat(size),
            }],
        };
        let encoded = document.encode();
        assert_eq!(
            Document::decode(&encoded).unwrap(),
            document,
            "an element body of {size} bytes did not survive the round trip"
        );
    }
}
