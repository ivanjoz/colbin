package wire

import "testing"

// The readers are where untrusted bytes first meet code, so these hand them
// arbitrary input through every exported read. What is checked is the contract
// in doc.go: no panic, and every read either advances or fails — which is what
// makes a decode loop finish.
//
// Which read is tried is taken from the key, so the fuzzer steers it by the
// same byte it mutates.

// fuzzRows caps the rows the harness hands Column. A table's row count is not
// bounded by its bytes — a constant column is nine bytes for any length — so
// wire leaves the bound to codec's budget, and a worker that allocated two
// billion rows would be testing the machine.
const fuzzRows = 1 << 12

// fuzzDepth caps the nesting the harness follows, for the same reason codec
// caps it: wire does not.
const fuzzDepth = 16

func FuzzReader(f *testing.F) {
	writer := Writer{}
	writer.Int(0, -300)
	writer.U16(1, 7)
	writer.String(2, "text")
	writer.PackedString(3, "PACKED TEXT")
	writer.Int32s(4, []int32{1, -2, 3})
	writer.Strings(5, []string{"a", "", "b"})
	inner := writer.OpenStruct(6)
	writer.Bool(0, true)
	writer.Close(inner)
	list := writer.OpenList(7, 2)
	for range 2 {
		element := writer.OpenElement()
		writer.Uint(0, 9)
		writer.CloseElement(element)
	}
	writer.Close(list)
	entries := writer.OpenMap(8, 1)
	writer.ElementString("k")
	writer.ElementInt(-1)
	writer.Close(entries)
	table := writer.OpenTable(9, 3)
	writer.Column(0, []int64{1, 2, 3})
	writer.Strings(1, []string{"x", "y", "z"})
	writer.Close(table)
	writer.F64(10, 1.5)
	writer.String(11, "a long enough string to need its length form")
	writer.Zero(12)
	writer.Int(13, -1)
	writer.Uint64s(14, []uint64{1 << 63, 5})
	f.Add(writer.Buffer)
	f.Add([]byte{0x1C, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})
	f.Add([]byte{0x1C, 0xFE, 0x03, 0x00, 'a', 'b', 'c'})

	f.Fuzz(func(t *testing.T, data []byte) {
		readNarrow(t, NewReader(data), 0)
		// And the same bytes stepped over, which is the path an unknown field
		// takes: every step advances or fails, and a read that succeeds lands
		// where the skip does.
		r := NewReader(data)
		for steps := 0; r.More(); steps++ {
			if steps > len(data) {
				t.Fatal("a skip neither advanced nor failed")
			}
			before := r.at
			if !r.Skip() {
				if r.Err() == nil {
					t.Fatal("a skip failed without an error")
				}
				break
			}
			typed := Reader{buffer: data, at: before}
			if typed.Uint(); typed.Err() == nil && typed.at != r.at {
				t.Fatalf("Uint read to %d where Skip stepped to %d", typed.at, r.at)
			}
			typed = Reader{buffer: data, at: before}
			if _ = typed.String(); typed.Err() == nil && typed.at != r.at {
				t.Fatalf("String read to %d where Skip stepped to %d", typed.at, r.at)
			}
			typed = Reader{buffer: data, at: before}
			if typed.Int(); typed.Err() == nil && typed.at != r.at {
				t.Fatalf("Int read to %d where Skip stepped to %d", typed.at, r.at)
			}
			typed = Reader{buffer: data, at: before}
			if typed.Strings(nil); typed.Err() == nil && typed.at != r.at {
				t.Fatalf("Strings read to %d where Skip stepped to %d", typed.at, r.at)
			}
			typed = Reader{buffer: data, at: before}
			if typed.Int16s(nil); typed.Err() == nil && typed.at != r.at {
				t.Fatalf("Int16s read to %d where Skip stepped to %d", typed.at, r.at)
			}
		}
	})
}

func readNarrow(t *testing.T, r Reader, depth int) {
	for steps := 0; r.More(); steps++ {
		if steps > len(r.buffer) {
			t.Fatalf("%d reads over %d bytes: a read neither advanced nor failed", steps, len(r.buffer))
		}
		switch r.Key() % 24 {
		case 0:
			r.Bool()
		case 1:
			r.Int()
		case 2:
			r.Uint()
		case 3:
			r.I8()
		case 4:
			r.I16()
		case 5:
			r.I32()
		case 6:
			r.U8()
		case 7:
			r.U16()
		case 8:
			r.U32()
		case 9:
			r.F32()
		case 10:
			r.F64()
		case 11:
			_ = r.String()
		case 12:
			r.Bytes()
		case 13:
			r.Int8s(nil)
		case 14:
			r.Int16s(nil)
		case 15:
			r.Int32s(nil)
		case 16:
			r.Ints(nil)
		case 17:
			r.Uint64s(nil)
		case 18:
			r.Strings(nil)
		case 19:
			body, wide, ok := r.StructBody()
			if ok && depth < fuzzDepth {
				readBody(t, body, wide, depth+1)
			}
		case 20:
			count, elements, ok := r.List()
			for index := 0; ok && index < count; index++ {
				body, ok := elements.Element()
				if !ok {
					break
				}
				if depth < fuzzDepth {
					readNarrow(t, NewReader(body), depth+1)
				}
			}
		case 21:
			count, entries, ok := r.Map()
			for index := 0; ok && index < count && entries.Err() == nil; index++ {
				entries.ElementString()
				entries.ElementInt()
			}
		case 22:
			rows, body, ok := r.Table()
			if ok && rows <= fuzzRows {
				columns := NewReader(body)
				for steps := 0; columns.More() && steps <= len(body); steps++ {
					if columns.Key()%2 == 0 {
						columns.Column(rows, nil)
					} else {
						columns.Strings(nil)
					}
				}
			}
		default:
			// A skip has to advance or fail, like every read: it is the path
			// an unknown field takes.
			before := r.at
			if r.Skip() == (r.Err() != nil) || (r.Err() == nil && r.at == before) {
				t.Fatal("a narrow skip neither advanced nor failed, or failed without an error")
			}
		}
	}
}

func readBody(t *testing.T, body []byte, wide bool, depth int) {
	if wide {
		readWide(t, NewReader8(body), depth)
	} else {
		readNarrow(t, NewReader(body), depth)
	}
}

func FuzzReader8(f *testing.F) {
	writer := Writer8{}
	writer.Int(0, -300)
	writer.Strings(1, []string{"a", "", "b"})
	writer.PackedString(2, "PACKED TEXT")
	inner := writer.OpenStruct(3)
	writer.U32(0, 1)
	writer.Close(inner)
	list := writer.OpenList(4, 2)
	for range 2 {
		element := writer.OpenElementStruct()
		writer.String(0, "row")
		writer.Close(element)
	}
	writer.Close(list)
	table := writer.OpenTable(5, 3)
	writer.Column(0, []int64{4, 5, 6})
	writer.StringColumn(1, []string{"x", "y", "z"})
	writer.Close(table)
	writer.Key(6)
	dynamic := writer.OpenElementList(4)
	writer.ElementNull()
	writer.ElementFloat64(-0.5)
	writer.ElementBytes([]byte{1, 2})
	entries := writer.OpenElementMap(1)
	writer.ElementString("k")
	writer.ElementBool(true)
	writer.Close(entries)
	writer.Close(dynamic)
	writer.Key(7)
	writer.ElementTyped(0)
	typed := writer.OpenElementStruct()
	writer.U16(0, 3)
	writer.Close(typed)
	f.Add(writer.Buffer)
	f.Add([]byte{0x00, 0xF3, 0xF3, 0xF3, 0xF3})

	f.Fuzz(func(t *testing.T, data []byte) {
		readWide(t, NewReader8(data), 0)
		// And the same bytes stepped over, which is the path an unknown field
		// takes and the one that used to recurse.
		r := NewReader8(data)
		for steps := 0; r.More(); steps++ {
			if steps > len(data) {
				t.Fatal("a skip neither advanced nor failed")
			}
			if !r.Skip() && r.Err() == nil {
				t.Fatal("a skip failed without an error")
			}
		}
	})
}

func readWide(t *testing.T, r Reader8, depth int) {
	for steps := 0; r.More(); steps++ {
		if steps > len(r.buffer) {
			t.Fatalf("%d reads over %d bytes: a read neither advanced nor failed", steps, len(r.buffer))
		}
		switch r.Key() % 24 {
		case 0:
			r.Bool()
		case 1:
			r.Int()
		case 2:
			r.Uint()
		case 3:
			r.I8()
		case 4:
			r.I16()
		case 5:
			r.I32()
		case 6:
			r.U8()
		case 7:
			r.U16()
		case 8:
			r.U32()
		case 9:
			r.F32()
		case 10:
			r.F64()
		case 11:
			_ = r.String()
		case 12:
			r.Bytes()
		case 13:
			r.Int8s(nil)
		case 14:
			r.Uint16s(nil)
		case 15:
			r.Ints(nil)
		case 16:
			r.Strings(nil)
		case 17:
			r.StringsBytes(nil)
		case 18:
			body, wide, ok := r.StructBody()
			if ok && depth < fuzzDepth {
				readBody(t, body, wide, depth+1)
			}
		case 19:
			count, elements, ok := r.List()
			for index := 0; ok && index < count && elements.More(); index++ {
				body, wide, ok := elements.ElementStructBody()
				if !ok {
					break
				}
				if depth < fuzzDepth {
					readBody(t, body, wide, depth+1)
				}
			}
		case 20:
			count, entries, ok := r.Map()
			for index := 0; ok && index < count && entries.Err() == nil; index++ {
				entries.ElementString()
				entries.ElementInt()
			}
		case 21:
			rows, columns, ok := r.Table()
			if ok && rows <= fuzzRows {
				for steps := 0; columns.More() && steps <= len(columns.buffer); steps++ {
					if columns.Key()%2 == 0 {
						columns.Column(rows, nil)
					} else {
						columns.Strings(nil)
					}
				}
			}
		case 22:
			if r.Payload() {
				readDynamic(t, &r, depth)
			}
		default:
			r.Skip()
		}
	}
}

// readDynamic reads one key-less value the way the `any` decoder does.
func readDynamic(t *testing.T, r *Reader8, depth int) {
	if depth >= fuzzDepth {
		r.SkipElement()
		return
	}
	switch r.ElementKind() {
	case KindNull:
		r.ElementNull()
	case KindBool:
		r.ElementBool()
	case KindInt:
		if r.ElementNegative() {
			r.ElementInt()
		} else {
			r.ElementUint()
		}
	case KindFloat64:
		r.ElementFloat64()
	case KindFloat32:
		r.ElementFloat32()
	case KindString:
		r.ElementString()
	case KindBytes:
		r.ElementBytes()
	case KindList:
		count, elements, ok := r.ElementList()
		readDynamics(t, &elements, ok, count, depth)
	case KindMap:
		count, entries, ok := r.ElementMap()
		readDynamics(t, &entries, ok, 2*count, depth)
	case KindTable:
		rows, columns, ok := r.ElementTable()
		if ok && rows <= fuzzRows {
			readWide(t, columns, depth+1)
		}
	case KindStruct:
		body, wide, ok := r.ElementStructBody()
		if ok {
			readBody(t, body, wide, depth+1)
		}
	case KindTyped:
		if _, ok := r.ElementTypedIndex(); ok && r.More() {
			readDynamic(t, r, depth+1)
		}
	default:
		if !r.SkipElement() && r.Err() == nil {
			r.Fail(ErrBadDescriptor)
		}
	}
}

func readDynamics(t *testing.T, r *Reader8, ok bool, count, depth int) {
	for index := 0; ok && index < count && r.More(); index++ {
		before := r.at
		readDynamic(t, r, depth+1)
		if r.Err() == nil && r.at == before {
			t.Fatal("a dynamic read neither advanced nor failed")
		}
	}
}
