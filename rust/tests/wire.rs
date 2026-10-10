//! The three framings of a key run, driven directly — which is what a caller
//! that already knows its type does, and what the derive generates.

use colbin::Error;
use colbin::wire::{BitmapReader, BitmapWriter, Reader, Reader8, Writer, Writer8};

/// The ten-field record both key widths are measured on, five fields set.
fn narrow_record() -> Vec<u8> {
    let mut buf = Vec::new();
    let mut w = Writer::new(&mut buf);
    w.u32(0, 7);
    w.u32(1, 42);
    w.u32(2, 103);
    w.u8(3, 5);
    w.u16(6, 0x0139);
    buf
}

#[test]
fn a_narrow_run_reads_back_what_it_wrote() {
    let message = narrow_record();
    // 7, 42, 103 and 5 are past the inline 1..=4 and take a byte each; 0x0139
    // takes two.
    assert_eq!(
        message,
        [0x04, 7, 0x14, 42, 0x24, 103, 0x34, 5, 0x65, 0x39, 0x01]
    );

    let mut r = Reader::new(&message);
    let mut seen = Vec::new();
    while r.more() {
        match r.key() {
            0 => seen.push((0, u64::from(r.u32()))),
            1 => seen.push((1, u64::from(r.u32()))),
            2 => seen.push((2, u64::from(r.u32()))),
            3 => seen.push((3, u64::from(r.u8()))),
            6 => seen.push((6, u64::from(r.u16()))),
            _ => assert!(r.skip()),
        }
    }
    r.err().expect("a clean read");
    assert_eq!(seen, vec![(0, 7), (1, 42), (2, 103), (3, 5), (6, 0x0139)]);
}

/// The nibble sizes the field, so a narrow reader that has heard of nothing at
/// all still walks the whole message — and lands on every key a reader that
/// knows them would.
#[test]
fn a_narrow_run_skips_what_it_does_not_know() {
    let mut buf = Vec::new();
    let mut w = Writer::new(&mut buf);
    w.u32(0, 7);
    w.string(1, "hello");
    w.string(2, &"long ".repeat(60));
    w.ints(3, &[1_i32, -2, 3]);
    w.f64(4, 1.5);
    w.bool(5, true);
    w.i64(6, -300);
    w.u64(7, 300);
    w.strings(8, &["a", "bb"]);
    w.zero(9);
    w.i64(10, -1);
    let mark = w.open_struct(11);
    {
        let mut inner = Writer::new(w.buf);
        inner.u32(0, 1);
    }
    w.close(mark);

    let mut r = Reader::new(&buf);
    let mut keys = Vec::new();
    while r.more() {
        keys.push(r.key());
        assert!(r.skip(), "every field sizes itself");
    }
    r.err().expect("a clean walk");
    assert_eq!(keys, (0..=11).collect::<Vec<_>>());

    let mut r = Reader::new(&buf);
    assert_eq!(r.u32(), 7);
    assert_eq!(r.string(), "hello");
    assert_eq!(r.string(), "long ".repeat(60));
    assert_eq!(r.ints::<i32>(), vec![1, -2, 3]);
    assert_eq!(r.f64(), 1.5);
    assert!(r.bool());
    assert_eq!(r.i64(), -300);
    assert_eq!(r.u64(), 300);
    assert_eq!(r.strings(), vec!["a".to_owned(), "bb".to_owned()]);
    assert_eq!(r.u64(), 0, "an explicit zero");
    assert_eq!(r.i64(), -1);
    let (body, wide) = r.struct_body().expect("a nested run");
    assert!(!wide);
    assert_eq!(Reader::new(body).u32(), 1);
    assert!(!r.more());
    r.err().expect("a clean read");
}

/// A write a layout case makes, and a read a refusal case attempts.
type Write = Box<dyn Fn(&mut Writer<'_>)>;
type Read = fn(&mut Reader<'_>);

/// The narrow layout byte by byte, against `INTERNALS.md` §3: the nibble is
/// `0..3` inline, `4..11` sized, `1 1 f1 f0` a length.
#[test]
fn the_narrow_nibble_sizes_every_field() {
    let cases: Vec<(&str, Write, Vec<u8>)> = vec![
        ("uint 1", Box::new(|w| w.u64(1, 1)), vec![0x10]),
        ("uint 4", Box::new(|w| w.u64(1, 4)), vec![0x13]),
        ("uint 5", Box::new(|w| w.u64(1, 5)), vec![0x14, 5]),
        ("uint 256", Box::new(|w| w.u64(1, 256)), vec![0x15, 0, 1]),
        (
            "uint max",
            Box::new(|w| w.u64(1, u64::MAX)),
            [vec![0x1B], vec![0xFF; 8]].concat(),
        ),
        ("bool", Box::new(|w| w.bool(2, true)), vec![0x20]),
        ("int 3", Box::new(|w| w.i64(3, 3)), vec![0x32]),
        ("int 4", Box::new(|w| w.i64(3, 4)), vec![0x34, 4]),
        ("int -1", Box::new(|w| w.i64(3, -1)), vec![0x33]),
        ("int -2", Box::new(|w| w.i64(3, -2)), vec![0x3C, 1, 2]),
        (
            "int -300",
            Box::new(|w| w.i64(3, -300)),
            vec![0x3C, 2, 0x2C, 1],
        ),
        (
            "int min",
            Box::new(|w| w.i64(3, i64::MIN)),
            vec![0x3C, 8, 0, 0, 0, 0, 0, 0, 0, 0x80],
        ),
        // 2.0 is 0x4000_0000_0000_0000; reversed, its one non-zero byte is low.
        ("float 2", Box::new(|w| w.f64(4, 2.0)), vec![0x44, 0x40]),
        ("string 1", Box::new(|w| w.string(5, "a")), vec![0x54, b'a']),
        (
            "string 8",
            Box::new(|w| w.string(5, "abcdefgh")),
            [vec![0x5B], b"abcdefgh".to_vec()].concat(),
        ),
        (
            "string 9",
            Box::new(|w| w.string(5, "abcdefghi")),
            [vec![0x5C, 9], b"abcdefghi".to_vec()].concat(),
        ),
        ("zero", Box::new(|w| w.zero(6)), vec![0x6C, 0]),
        (
            "u8 array",
            Box::new(|w| w.ints(7, &[1_u8, 255])),
            vec![0x7C, 2, 1, 255],
        ),
        (
            "u16 array of small values",
            Box::new(|w| w.ints(7, &[1_u16, 200])),
            vec![0x7C, 2, 1, 200],
        ),
        (
            "i16 array, two's complement",
            Box::new(|w| w.ints(7, &[1_i16, 200])),
            vec![0x7D, 4, 1, 0, 200, 0],
        ),
        (
            "i32 array of -1 and -128",
            Box::new(|w| w.ints(7, &[-1_i32, -128])),
            vec![0x7C, 2, 0xFF, 0x80],
        ),
        (
            "u64 array past 2^63, magnitudes",
            Box::new(|w| w.ints(7, &[u64::MAX])),
            [vec![0x7F, 8], vec![0xFF; 8]].concat(),
        ),
        (
            "string array",
            Box::new(|w| w.strings(8, &["a", "", "bc"])),
            vec![0x8C, 6, 1, b'a', 0, 2, b'b', b'c'],
        ),
        (
            "empty narrow struct",
            Box::new(|w| {
                let mark = w.open_struct(9);
                w.close(mark);
            }),
            vec![0x9C, 0],
        ),
        (
            "wide struct",
            Box::new(|w| {
                let mark = w.open_struct_wide(9);
                w.close(mark);
            }),
            vec![0x9D, 0],
        ),
        (
            "table",
            Box::new(|w| {
                let mark = w.open_table(10, 8);
                w.close(mark);
            }),
            vec![0xAD, 1, 8],
        ),
        (
            "list",
            Box::new(|w| {
                let mark = w.open_list(10, 0);
                w.close(mark);
            }),
            vec![0xAC, 1, 0],
        ),
    ];
    for (name, write, want) in cases {
        let mut buf = Vec::new();
        write(&mut Writer::new(&mut buf));
        assert_eq!(buf, want, "{name}");
        let mut r = Reader::new(&buf);
        assert!(r.skip(), "{name}");
        assert!(!r.more(), "{name}: skip lands on the end");
    }
}

/// Signed and unsigned arrays at every boundary of every width read back as
/// what was written: the width is the narrowest that holds every element.
#[test]
fn narrow_integer_arrays_round_trip_at_every_width() {
    fn check<T: colbin::wire::Integer + PartialEq + core::fmt::Debug>(values: &[T], width: usize) {
        let mut buf = Vec::new();
        Writer::new(&mut buf).ints(0, values);
        assert_eq!(1 << (buf[0] & 0b11), width, "{values:?}");
        let mut r = Reader::new(&buf);
        assert_eq!(r.ints::<T>(), values);
        r.err().expect("a clean read");
    }
    check(&[i64::from(i8::MIN), i64::from(i8::MAX)], 1);
    check(&[i64::from(i8::MIN) - 1], 2);
    check(&[i64::from(i8::MAX) + 1], 2);
    check(&[i64::from(i16::MIN), i64::from(i16::MAX)], 2);
    check(&[i64::from(i16::MAX) + 1], 4);
    check(&[i64::from(i32::MIN)], 4);
    check(&[i64::from(i32::MIN) - 1], 8);
    check(&[i64::MIN, i64::MAX], 8);
    check(&[u64::from(u8::MAX)], 1);
    check(&[u64::from(u16::MAX)], 2);
    check(&[u64::from(u16::MAX) + 1], 4);
    check(&[u64::from(u32::MAX) + 1, 0], 8);
    check(&[0_u8, 0], 1);
}

/// A narrow composite's length is one byte up to 253 and then escapes to a u16
/// and a u32 — never touching the header in front of it, which is why the
/// reader cannot tell a widened length from one written wide to begin with.
#[test]
fn a_long_narrow_composite_widens_its_length() {
    for (size, length_bytes) in [
        (10_usize, 1),
        (253, 1),
        (254, 3),
        (0xFFFF, 3),
        (0x1_0000, 5),
    ] {
        let mut buf = Vec::new();
        {
            let mut w = Writer::new(&mut buf);
            let mark = w.open_struct(0);
            w.buf.extend(core::iter::repeat_n(0x10_u8, size)); // key 1, value 1
            w.close(mark);
            w.u32(2, 9);
        }
        assert_eq!(buf.len(), 1 + length_bytes + size + 2, "{size}");
        let mut r = Reader::new(&buf);
        let (body, _) = r.struct_body().expect("a nested run");
        assert_eq!(body.len(), size);
        assert_eq!(r.u32(), 9, "{size}: the field after a widened length");
        r.err().expect("a clean read");
    }
}

/// The forms §3.2 marks refused are refused, not read as something plausible.
#[test]
fn a_narrow_reader_refuses_what_the_layout_does_not_assign() {
    let refused: [(&str, &[u8], Read); 9] = [
        ("inline string", &[0x00], |r| {
            let _ = r.string();
        }),
        ("string flag 11", &[0x0F, 1, b'a'], |r| {
            let _ = r.packed_string();
        }),
        ("packed string as bytes", &[0x0D, 1, 0], |r| {
            let _ = r.bytes();
        }),
        ("sized integer array", &[0x04, 1], |r| {
            let _ = r.ints::<i64>();
        }),
        ("array length off its width", &[0x0D, 3, 1, 2, 3], |r| {
            let _ = r.ints::<i64>();
        }),
        ("array wider than its type", &[0x0D, 2, 1, 0], |r| {
            let _ = r.ints::<u8>();
        }),
        ("string array with a flag", &[0x0D, 1, 0], |r| {
            let _ = r.strings();
        }),
        ("struct with f1 set", &[0x0E, 0], |r| {
            let _ = r.struct_body();
        }),
        ("unsigned in the negative form", &[0x0C, 1, 5], |r| {
            let _ = r.u64();
        }),
    ];
    for (name, message, read) in refused {
        let mut r = Reader::new(message);
        read(&mut r);
        assert!(r.err().is_err(), "{name}");
    }

    // And a value wider than the field's type.
    let mut r = Reader::new(&[0x05, 0, 1]);
    let _ = r.u8();
    assert_eq!(r.err(), Err(Error::FieldTooWide));
    let mut r = Reader::new(&[0x0C, 9, 1, 0, 0, 0, 0, 0, 0, 0, 0]);
    let _ = r.i64();
    assert_eq!(r.err(), Err(Error::FieldTooWide));
    let mut r = Reader::new(&[0x0C, 2, 0x81, 0]);
    let _ = r.i8();
    assert_eq!(r.err(), Err(Error::FieldTooWide), "−129 is past an i8");

    // A length's long forms are accepted even where a shorter one would do.
    let mut r = Reader::new(&[0x0C, 0xFE, 1, 0, b'a']);
    assert_eq!(r.string(), "a");
    let mut r = Reader::new(&[0x0C, 0xFF, 1, 0, 0, 0, b'a']);
    assert_eq!(r.string(), "a");
    r.err().expect("a reader accepts any length form");
}

#[test]
fn a_wide_run_skips_what_it_does_not_know() {
    let mut buf = Vec::new();
    let mut w = Writer8::new(&mut buf);
    w.u32(0, 7);
    w.string(1, "hello");
    w.ints(2, &[1_i32, -2, 3]);
    w.f64(3, 1.5);
    w.bool(4, true);
    w.i64(5, -300);
    w.u64(6, 300);
    w.strings(7, &["a", "bb"]);

    // A reader that has heard of nothing at all still walks the whole message,
    // because every class either carries a byte length or has one derivable from
    // its descriptor.
    let mut r = Reader8::new(&buf);
    let mut keys = Vec::new();
    while r.more() {
        keys.push(r.key());
        assert!(r.skip(), "every field sizes itself");
    }
    r.err().expect("a clean walk");
    assert_eq!(keys, vec![0, 1, 2, 3, 4, 5, 6, 7]);

    // And one that has heard of them reads them.
    let mut r = Reader8::new(&buf);
    assert_eq!(r.key(), 0);
    assert_eq!(r.u32(), 7);
    assert_eq!(r.string(), "hello");
    assert_eq!(r.ints::<i32>(), vec![1, -2, 3]);
    assert_eq!(r.f64(), 1.5);
    assert!(r.bool());
    assert_eq!(r.i64(), -300);
    assert_eq!(r.u64(), 300);
    assert_eq!(r.strings(), vec!["a".to_owned(), "bb".to_owned()]);
    assert!(!r.more());
    r.err().expect("a clean read");
}

/// The wide integer has two forms and the writer picks the shorter, so no value
/// is ever larger than the byte-count form would have made it.
#[test]
fn the_wide_integer_never_grows() {
    for value in [
        0_u64,
        1,
        127,
        128,
        255,
        300,
        65_535,
        70_000,
        1 << 40,
        u64::MAX,
    ] {
        let mut buf = Vec::new();
        let mut w = Writer8::new(&mut buf);
        w.u64(0, value);
        if value == 0 {
            assert!(buf.is_empty(), "a zero is not written");
            continue;
        }
        let mut r = Reader8::new(&buf);
        assert_eq!(r.u64(), value, "{value}");
        r.err().expect("a clean read");

        // Key, descriptor and at most eight magnitude bytes.
        assert!(buf.len() <= 10, "{value} took {} bytes", buf.len());
    }
    for value in [i64::MIN, -70_000, -300, -1, 1, 300, i64::MAX] {
        let mut buf = Vec::new();
        let mut w = Writer8::new(&mut buf);
        w.i64(0, value);
        let mut r = Reader8::new(&buf);
        assert_eq!(r.i64(), value, "{value}");
        r.err().expect("a clean read");
    }
}

/// The bitmap framing: the keys are an ordered subset of a known set, so they
/// are a bitmap rather than a sequence of numbers.
#[test]
fn a_bitmap_run_reads_back_what_it_wrote() {
    let mut buf = Vec::new();
    let mut w = BitmapWriter::new(&mut buf, 9);
    w.u32(0, 7);
    w.u32(1, 42);
    w.u32(2, 103);
    w.u8(3, 5);
    w.u16(6, 0x0139);
    w.finish();

    // A length byte and a two-byte bitmap, then a descriptor and a payload per
    // present field. On *this* record the bitmap wins by one: every value is
    // past the narrow nibble's inline 1..=4, so the narrow key costs a byte of
    // header per field where the bitmap's three bytes cover all five.
    assert_eq!(buf.len(), 10);
    assert_eq!(narrow_record().len(), 11);

    let mut r = BitmapReader::new(&buf);
    let mut seen = Vec::new();
    while r.more() {
        match r.key() {
            0 => seen.push((0, u64::from(r.u32()))),
            1 => seen.push((1, u64::from(r.u32()))),
            2 => seen.push((2, u64::from(r.u32()))),
            3 => seen.push((3, u64::from(r.u8()))),
            6 => seen.push((6, u64::from(r.u16()))),
            _ => assert!(r.skip()),
        }
    }
    r.err().expect("a clean read");
    assert_eq!(seen, vec![(0, 7), (1, 42), (2, 103), (3, 5), (6, 0x0139)]);
}

#[test]
fn a_bitmap_run_carries_every_kind_and_skips_the_unknown() {
    let mut buf = Vec::new();
    let mut w = BitmapWriter::new(&mut buf, 63);
    w.bool(0, true);
    w.i64(1, -300);
    w.f32(2, 1.0);
    w.string(3, "hello");
    w.bytes(4, &[1, 2, 3]);
    w.u64(63, u64::MAX);
    w.finish();

    let mut r = BitmapReader::new(&buf);
    let mut keys = Vec::new();
    while r.more() {
        keys.push(r.key());
        assert!(r.skip(), "a bitmap field sizes itself, like a wide one");
    }
    r.err().expect("a clean walk");
    assert_eq!(keys, vec![0, 1, 2, 3, 4, 63]);

    let mut r = BitmapReader::new(&buf);
    assert!(r.bool());
    assert_eq!(r.i64(), -300);
    assert!((r.f32() - 1.0).abs() < f32::EPSILON);
    assert_eq!(r.string(), "hello");
    assert_eq!(r.bytes(), &[1, 2, 3]);
    assert_eq!(r.u64(), u64::MAX);
    assert!(!r.more());
    r.err().expect("a clean read");
}

#[test]
fn a_bitmap_that_is_not_one_is_refused() {
    assert_eq!(BitmapReader::new(&[]).err(), Err(Error::Truncated));
    assert_eq!(BitmapReader::new(&[0]).err(), Err(Error::BadBitmap));
    assert_eq!(BitmapReader::new(&[9, 0]).err(), Err(Error::BadBitmap));
    assert_eq!(BitmapReader::new(&[2, 0]).err(), Err(Error::BadBitmap));
}

/// Composites carry a byte length, which is what makes one skippable without its
/// sub-schema — and what a nested run at the other key width rides inside.
#[test]
fn composites_nest_at_either_key_width() {
    let mut buf = Vec::new();
    {
        let mut w = Writer8::new(&mut buf);
        w.u32(0, 1);

        let outer = w.open_struct(1); // narrow keys inside a wide run
        {
            let mut inner = Writer::new(w.buf);
            inner.string(0, "narrow");
            inner.u16(1, 500);
        }
        w.close(outer);

        let list = w.open_list(2, 2);
        for name in ["first", "second"] {
            let element = w.open_element_struct();
            {
                let mut inner = Writer::new(w.buf);
                inner.string(0, name);
            }
            w.close(element);
        }
        w.close(list);

        let map = w.open_map(3, 1);
        w.element_string("key");
        w.element_int(-7);
        w.close(map);
    }

    let mut r = Reader8::new(&buf);
    assert_eq!(r.u32(), 1);

    let (body, wide_keys) = r.struct_body().expect("a nested run");
    assert!(
        !wide_keys,
        "the descriptor names the width of the run inside"
    );
    let mut inner = Reader::new(body);
    assert_eq!(inner.string(), "narrow");
    assert_eq!(inner.u16(), 500);
    inner.err().expect("a clean nested read");

    let (count, mut elements) = r.list().expect("a list");
    assert_eq!(count, 2);
    for name in ["first", "second"] {
        let (body, _) = elements.element_struct_body().expect("an element");
        let mut inner = Reader::new(body);
        assert_eq!(inner.string(), name);
    }

    let (entries, mut pairs) = r.map().expect("a map");
    assert_eq!(entries, 1);
    assert_eq!(pairs.element_string(), "key");
    assert_eq!(pairs.element_int(), -7);
    r.err().expect("a clean read");
}

/// A composite whose body outgrows the reserved byte widens its length in
/// place, which the reader cannot tell from one that never did.
#[test]
fn a_long_composite_widens_its_length() {
    for size in [1_usize, 254, 255, 256, 70_000] {
        let payload = "a".repeat(size);
        let mut buf = Vec::new();
        {
            let mut w = Writer8::new(&mut buf);
            let mark = w.open_struct(0);
            {
                let mut inner = Writer::new(w.buf);
                inner.string(0, &payload);
            }
            w.close(mark);
            w.u32(1, 9);
        }
        let mut r = Reader8::new(&buf);
        let (body, _) = r.struct_body().expect("a nested run");
        let mut inner = Reader::new(body);
        assert_eq!(inner.string().len(), size);
        assert_eq!(
            r.u32(),
            9,
            "the field after a widened length is still found"
        );
        r.err().expect("a clean read");
    }
}

/// Every prefix of a valid message must produce an error rather than a panic,
/// which is the property a reader facing a network needs.
#[test]
fn every_truncation_is_refused_rather_than_panicked_on() {
    let mut buf = Vec::new();
    {
        let mut w = Writer8::new(&mut buf);
        w.u32(0, 70_000);
        w.string(1, &"a".repeat(400));
        w.ints(2, &[-1_i64; 40]);
        let mark = w.open_struct_wide(3);
        {
            let mut inner = Writer8::new(w.buf);
            inner.u64(0, u64::MAX);
        }
        w.close(mark);
        w.column(4, &(0..300_i64).collect::<Vec<_>>());
    }
    for cut in 0..buf.len() {
        let mut r = Reader8::new(&buf[..cut]);
        while r.more() && r.skip() {}
        let mut r = Reader8::new(&buf[..cut]);
        while r.more() {
            let _ = r.u64();
            let _ = r.string();
            let _ = r.ints::<i64>();
            let _ = r.struct_body();
            let mut dst: Vec<i64> = Vec::new();
            r.column(300, &mut dst);
        }
    }
}

/// The same, for a narrow run of every shape.
#[test]
fn every_narrow_truncation_is_refused_rather_than_panicked_on() {
    let mut buf = Vec::new();
    {
        let mut w = Writer::new(&mut buf);
        w.u32(0, 70_000);
        w.i64(1, -70_000);
        w.string(2, &"a".repeat(400));
        w.packed_string(3, "hello world");
        w.ints(4, &[-1_i64; 40]);
        w.strings(5, &["a", "bb"]);
        let mark = w.open_struct(6);
        {
            let mut inner = Writer::new(w.buf);
            inner.u64(0, u64::MAX);
        }
        w.close(mark);
        w.column(7, &(0..300_i64).collect::<Vec<_>>());
    }
    for cut in 0..buf.len() {
        let mut r = Reader::new(&buf[..cut]);
        while r.more() && r.skip() {}
        let mut r = Reader::new(&buf[..cut]);
        while r.more() {
            let _ = r.u64();
            let _ = r.i64();
            let _ = r.string();
            let _ = r.packed_string();
            let _ = r.ints::<i64>();
            let _ = r.strings();
            let _ = r.struct_body();
            let mut dst: Vec<i64> = Vec::new();
            r.column(300, &mut dst);
        }
    }
}

/// Arbitrary bytes must not panic either: a reader is handed whatever the peer
/// sent, not only what a writer produced.
#[test]
fn arbitrary_bytes_are_refused_rather_than_panicked_on() {
    for first in 0..=255_u8 {
        for second in 0..=255_u8 {
            let message = [first, second, 0x41, 0x00, 0xFF, 0x7F, 0x01];
            let mut r = Reader8::new(&message);
            while r.more() && r.skip() {}

            let mut r = Reader::new(&message);
            let _ = r.u64();
            let mut r = Reader::new(&message);
            let _ = r.i64();
            let mut r = Reader::new(&message);
            let _ = r.bytes();
            let mut r = Reader::new(&message);
            let _ = r.ints::<i64>();
            let mut r = Reader::new(&message);
            let _ = r.strings();
            let mut r = Reader::new(&message);
            let _ = r.struct_body();
            let mut r = Reader::new(&message);
            let _ = r.counted();
            let mut r = Reader::new(&message);
            let _ = r.table();
            let mut r = Reader::new(&message);
            let _ = r.packed_string();
            let mut r = Reader::new(&message);
            while r.more() && r.skip() {}

            let mut r = BitmapReader::new(&message);
            while r.more() && r.skip() {}
        }
    }
}
