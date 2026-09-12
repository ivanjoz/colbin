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
    assert_eq!(message.len(), 9);

    let mut r = Reader::new(&message);
    let mut seen = Vec::new();
    while r.more() {
        match r.key() {
            0 => seen.push((0, u64::from(r.u32()))),
            1 => seen.push((1, u64::from(r.u32()))),
            2 => seen.push((2, u64::from(r.u32()))),
            3 => seen.push((3, u64::from(r.u8()))),
            6 => seen.push((6, u64::from(r.u16()))),
            _ => r.skip(),
        }
    }
    r.err().expect("a clean read");
    assert_eq!(seen, vec![(0, 7), (1, 42), (2, 103), (3, 5), (6, 0x0139)]);
}

/// Four descriptor bits have no room for a class, so a narrow reader cannot step
/// over a field it does not know — it says so rather than guessing.
#[test]
fn a_narrow_reader_refuses_to_skip() {
    let message = narrow_record();
    let mut r = Reader::new(&message);
    r.skip();
    assert_eq!(r.err(), Err(Error::UnknownKey(0)));
    assert!(!r.more(), "a failure parks the cursor at the end");
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
    // present field. On *this* record the narrow key still wins by one, because
    // its unsigned nibble carries 0..=7 outright and two of these five fields
    // are that small; the bitmap's byte comes back on records whose values do
    // not fit a nibble, which is the trade the two framings make.
    assert_eq!(buf.len(), 10);
    assert_eq!(narrow_record().len(), 9);

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

            let mut r = BitmapReader::new(&message);
            while r.more() && r.skip() {}
        }
    }
}
