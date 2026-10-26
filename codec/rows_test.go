package codec

import (
	"bytes"
	"math"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unsafe"
)

// The row program must write writeFields' bytes for every field it spells and
// every one it hands back, at every value an integer form or a string length
// changes at.

type rowsEvery struct {
	B   bool             `cb:"1"`
	I8  int8             `cb:"2"`
	I16 int16            `cb:"3"`
	I32 int32            `cb:"4"`
	I64 int64            `cb:"5"`
	U8  uint8            `cb:"6"`
	U16 uint16           `cb:"7"`
	U32 uint32           `cb:"8"`
	U64 uint64           `cb:"9"`
	F32 float32          `cb:"10"`
	F64 float64          `cb:"11"`
	S   string           `cb:"12"`
	M   map[string]int64 `cb:"13"`
	P   *int32           `cb:"14"`
	Sub []rowsEveryInner `cb:"15"`
	One rowsEveryInner   `cb:"16"`
}

type rowsEveryInner struct {
	N     int      `cb:"1"`
	U     uint     `cb:"2"`
	Name  string   `cb:"3"`
	Bytes []byte   `cb:"4"`
	Ints  []int32  `cb:"5"`
	Words []string `cb:"6"`
}

// rowsEdges are the integers the forms change at, and around them: every power
// of two and its neighbours, both signs, which is every width a byte count or a
// varint changes at.
var rowsEdges = func() []int64 {
	edges := []int64{0, 1, 2, 3, 4, 5, -1, -2, -3, 127, 128, 255, 256, 257, 1023, 1024,
		math.MaxInt64, math.MinInt64, math.MinInt64 + 1}
	for shift := range 63 {
		power := int64(1) << shift
		edges = append(edges, power-1, power, power+1, -power+1, -power, -power-1)
	}
	return edges
}()

// rowsLengths are the string lengths the copy and the header change at.
var rowsLengths = []int{0, 1, 2, 3, 4, 5, 7, 8, 9, 15, 16, 17, 31, 239, 240, 253, 254, 255, 300, 511, 512,
	3823, 3824, 65535, 65536, 70000}

func makeRowsEvery(random *rand.Rand) rowsEvery {
	edge := func() int64 {
		if random.IntN(3) == 0 {
			return int64(random.Uint64())
		}
		return rowsEdges[random.IntN(len(rowsEdges))]
	}
	text := func() string {
		size := rowsLengths[random.IntN(len(rowsLengths))]
		if size > 300 && random.IntN(8) != 0 {
			size = random.IntN(40)
		}
		var out strings.Builder
		for range size {
			out.WriteByte(byte('a' + random.IntN(26)))
		}
		return out.String()
	}
	inner := func() rowsEveryInner {
		value := rowsEveryInner{N: int(edge()), U: uint(edge()), Name: text()}
		if random.IntN(2) == 0 {
			value.Bytes = []byte(text())
			value.Ints = []int32{int32(edge()), int32(edge())}
			value.Words = []string{text(), text()}
		}
		return value
	}
	value := rowsEvery{
		B: random.IntN(2) == 0, I8: int8(edge()), I16: int16(edge()), I32: int32(edge()), I64: edge(),
		U8: uint8(edge()), U16: uint16(edge()), U32: uint32(edge()), U64: uint64(edge()),
		F32: float32(random.NormFloat64()), F64: random.NormFloat64(), S: text(), One: inner(),
	}
	if random.IntN(4) == 0 {
		value.F32, value.F64 = 0, 0
	}
	if random.IntN(2) == 0 {
		value.M = map[string]int64{text(): edge(), "k": edge()}
		pointee := int32(edge())
		value.P = &pointee
	}
	for range random.IntN(12) {
		value.Sub = append(value.Sub, inner())
	}
	return value
}

// rowsUntagged is rowsEvery with no tags: its keys come from its field names,
// so its plan, and its inner one's, are wide.
type rowsUntagged struct {
	B   bool
	I8  int8
	I16 int16
	I32 int32
	I64 int64
	U8  uint8
	U16 uint16
	U32 uint32
	U64 uint64
	F32 float32
	F64 float64
	S   string
	M   map[string]int64
	P   *int32
	Sub []rowsUntaggedInner
	One rowsUntaggedInner
	// A narrow list element under a wide parent, and back.
	Tagged []rowsEveryInner
}

type rowsUntaggedInner struct {
	N     int
	U     uint
	Name  string
	Bytes []byte
	Ints  []int32
	Words []string
}

func untagged(value rowsEvery) rowsUntagged {
	inner := func(v rowsEveryInner) rowsUntaggedInner { return rowsUntaggedInner(v) }
	out := rowsUntagged{B: value.B, I8: value.I8, I16: value.I16, I32: value.I32, I64: value.I64,
		U8: value.U8, U16: value.U16, U32: value.U32, U64: value.U64, F32: value.F32, F64: value.F64,
		S: value.S, M: value.M, P: value.P, One: inner(value.One), Tagged: value.Sub}
	for _, element := range value.Sub {
		out.Sub = append(out.Sub, inner(element))
	}
	return out
}

func TestRowProgramWritesWhatWriteFieldsDoes(t *testing.T) {
	if MustCodec[rowsEvery]().plan.rows == nil || MustCodec[rowsEveryInner]().plan.rows == nil {
		t.Fatal("the test types compiled to no program")
	}
	if program := MustCodec[rowsUntagged]().plan.rows; program == nil || !program.wide {
		t.Fatal("the untagged type compiled to no wide program")
	}
	random := rand.New(rand.NewPCG(21, 22))
	for trial := range 3000 {
		value := makeRowsEvery(random)
		useRowPrograms = false
		want, wantErr := Marshal(&value)
		useRowPrograms = true
		for _, kernel := range []bool{false, true} {
			was := setRowKernel(kernel)
			got, gotErr := Marshal(&value)
			setRowKernel(was)
			if (wantErr != nil) != (gotErr != nil) || !bytes.Equal(got, want) {
				t.Fatalf("trial %d, kernel %v: program %d bytes (%v), writeFields %d bytes (%v)\n%+v",
					trial, kernel, len(got), gotErr, len(want), wantErr, value)
			}
		}
		got, _ := Marshal(&value)
		var back rowsEvery
		if err := Unmarshal(got, &back); err != nil {
			t.Fatalf("trial %d: %v", trial, err)
		}
		// And at eight key bits.
		wide := untagged(value)
		useRowPrograms = false
		want, wantErr = Marshal(&wide)
		useRowPrograms = true
		for _, kernel := range []bool{false, true} {
			was := setRowKernel(kernel)
			got, gotErr := Marshal(&wide)
			setRowKernel(was)
			if (wantErr != nil) != (gotErr != nil) || !bytes.Equal(got, want) {
				t.Fatalf("trial %d, wide, kernel %v: program %d bytes (%v), writeFieldsWide %d bytes (%v)\n%+v",
					trial, kernel, len(got), gotErr, len(want), wantErr, wide)
			}
		}
	}
	// And the TPC-H shape the benchmark writes.
	nested := makeRowsNested(500)
	useRowPrograms = false
	want := must(MustCodec[rowsNested]().Encode(nested))
	useRowPrograms = true
	for _, kernel := range []bool{false, true} {
		was := setRowKernel(kernel)
		got := must(MustCodec[rowsNested]().Encode(nested))
		setRowKernel(was)
		if !bytes.Equal(got, want) {
			t.Fatalf("nested, kernel %v: program %d bytes, writeFields %d", kernel, len(got), len(want))
		}
	}
}

// rowsSwapped reads rowsEvery's keys into other types, so that every value the
// program reads is also one it must refuse or hand back: a magnitude too wide
// for the field, a negative into an unsigned one, a string into an integer and
// an integer into a string.
type rowsSwapped struct {
	B   int8             `cb:"1"`
	I8  uint8            `cb:"2"`
	I16 int8             `cb:"3"`
	I32 string           `cb:"4"`
	I64 int16            `cb:"5"`
	U8  bool             `cb:"6"`
	U16 int8             `cb:"7"`
	U32 uint16           `cb:"8"`
	U64 int32            `cb:"9"`
	F32 uint8            `cb:"10"`
	F64 float32          `cb:"11"`
	S   int64            `cb:"12"`
	Sub []rowsEveryInner `cb:"15"`
}

// rowsForms are the narrow headers' nibbles and the wide descriptors around
// every boundary between forms.
var rowsForms = func() []byte {
	forms := []byte{0x7F, 0x80, 0x81, 0x87, 0x88, 0x89, 0x8F, 0x90, 0x91, 0x92, 0x93, 0x94, 0x97,
		0x98, 0x9B, 0x9C, 0xA0, 0xF0, 0xF7, 0xF8, 0xFB, 0xFF, 0xFD, 0xFE}
	for nibble := range byte(16) {
		forms = append(forms, nibble, 0x10|nibble, 0xC0|nibble)
	}
	return forms
}()

// rowsUntaggedSwapped is rowsSwapped at eight key bits: rowsUntagged's names,
// so its keys, with the types changed.
type rowsUntaggedSwapped struct {
	B   int8
	I8  uint8
	I16 int8
	I32 string
	I64 int16
	U8  bool
	U16 int8
	U32 uint16
	U64 int32
	F32 uint8
	F64 float32
	S   int64
	Sub []rowsUntaggedInner
}

// The program must read what readNarrowRun and readRun read, and fail what they
// fail, from the encoder's bytes, from bytes meant for other types, and from
// bytes broken.
func TestRowProgramReadsWhatTheRunsDo(t *testing.T) {
	random := rand.New(rand.NewPCG(23, 24))
	decode := func(data []byte, into any) (any, error) {
		err := Unmarshal(data, into)
		return into, err
	}
	compare := func(trial int, what string, data []byte, fresh func() any) {
		t.Helper()
		useRowPrograms = false
		want, wantErr := decode(data, fresh())
		useRowPrograms = true
		got, gotErr := decode(data, fresh())
		if (wantErr == nil) != (gotErr == nil) || (wantErr != nil && wantErr.Error() != gotErr.Error()) {
			t.Fatalf("trial %d, %s: program error %v, the run's error %v", trial, what, gotErr, wantErr)
		}
		if wantErr == nil && !reflect.DeepEqual(got, want) {
			t.Fatalf("trial %d, %s: program read\n%+v\nthe run read\n%+v", trial, what, got, want)
		}
	}
	// Each message read as its own type and as the swapped one, whole, with a
	// byte flipped and cut short.
	check := func(trial int, what string, data []byte, own, swapped func() any) {
		t.Helper()
		compare(trial, what, data, own)
		compare(trial, what+" swapped", data, swapped)
		broken := bytes.Clone(data)
		broken[random.IntN(len(broken))] ^= byte(1 + random.IntN(255))
		compare(trial, what+" flipped", broken, own)
		compare(trial, what+" flipped swapped", broken, swapped)
		compare(trial, what+" cut", data[:random.IntN(len(data))], own)
		// A byte set to one a header or a descriptor changes form at: the forms
		// the encoder never writes for a field, and the ones of other classes.
		broken = bytes.Clone(data)
		broken[random.IntN(len(broken))] = rowsForms[random.IntN(len(rowsForms))]
		compare(trial, what+" reformed", broken, own)
		compare(trial, what+" reformed swapped", broken, swapped)
	}
	if MustCodec[rowsUntaggedSwapped]().plan.rows == nil {
		t.Fatal("the swapped untagged type compiled to no program")
	}
	for trial := range 3000 {
		value := makeRowsEvery(random)
		check(trial, "narrow", must(Marshal(&value)),
			func() any { return new(rowsEvery) }, func() any { return new(rowsSwapped) })
		wide := untagged(value)
		check(trial, "wide", must(Marshal(&wide)),
			func() any { return new(rowsUntagged) }, func() any { return new(rowsUntaggedSwapped) })
		// Packed strings, which the program hands back.
		was := Packed5()
		SetPacked5(true)
		narrowPacked, widePacked := must(Marshal(&value)), must(Marshal(&wide))
		SetPacked5(was)
		compare(trial, "narrow packed", narrowPacked, func() any { return new(rowsEvery) })
		compare(trial, "wide packed", widePacked, func() any { return new(rowsUntagged) })
	}
	compare(0, "nested", must(MustCodec[rowsNested]().Encode(makeRowsNested(500))),
		func() any { return new(rowsNested) })
	compare(0, "nested wide", must(MustCodec[rowsNestedUntagged]().Encode(makeRowsNestedUntagged(500))),
		func() any { return new(rowsNestedUntagged) })
}

// rowsNumeric is a row the program writes as a whole list (writeRows): every
// field a number or a bool.
type rowsNumeric struct {
	B   bool    `cb:"1"`
	I8  int8    `cb:"2"`
	I16 int16   `cb:"3"`
	I32 int32   `cb:"4"`
	I64 int64   `cb:"5"`
	U8  uint8   `cb:"6"`
	U16 uint16  `cb:"7"`
	U32 uint32  `cb:"8"`
	U64 uint64  `cb:"9"`
	F32 float32 `cb:"10"`
	F64 float64 `cb:"11"`
}

type rowsNumericList struct {
	ID   int64         `cb:"1"`
	Rows []rowsNumeric `cb:"2"`
}

func TestNumericListsWriteTheWalksBytes(t *testing.T) {
	if program := MustCodec[rowsNumeric]().plan.rows; program == nil || !program.numeric {
		t.Fatal("the numeric row compiled to no numeric program")
	}
	random := rand.New(rand.NewPCG(23, 24))
	edge := func() int64 {
		if random.IntN(3) == 0 {
			return int64(random.Uint64())
		}
		return rowsEdges[random.IntN(len(rowsEdges))]
	}
	for trial := range 3000 {
		// Below the table threshold, so the rows are a list.
		value := rowsNumericList{ID: edge(), Rows: make([]rowsNumeric, 1+random.IntN(tableThreshold-1))}
		for index := range value.Rows {
			value.Rows[index] = rowsNumeric{
				B: random.IntN(2) == 0, I8: int8(edge()), I16: int16(edge()), I32: int32(edge()), I64: edge(),
				U8: uint8(edge()), U16: uint16(edge()), U32: uint32(edge()), U64: uint64(edge()),
				F32: math.Float32frombits(uint32(edge())), F64: math.Float64frombits(uint64(edge())),
			}
			if random.IntN(4) == 0 {
				value.Rows[index] = rowsNumeric{}
			}
		}
		useRowPrograms = false
		want := must(Marshal(&value))
		useRowPrograms = true
		got := must(Marshal(&value))
		if !bytes.Equal(got, want) {
			t.Fatalf("trial %d: writeRows %x\nwriteFields %x\n%+v", trial, got, want, value)
		}
		var back rowsNumericList
		if err := Unmarshal(got, &back); err != nil {
			t.Fatalf("trial %d: %v", trial, err)
		}
	}
}

// rowsFlat is a root the kernel writes whole: numbers, strings and int32
// lists, with a bool after them that lands on the last byte.
type rowsFlat struct {
	U64  uint64  `cb:"1"`
	I64  int64   `cb:"2"`
	F64  float64 `cb:"3"`
	S    string  `cb:"4"`
	L    []int32 `cb:"5"`
	I32  int32   `cb:"6"`
	More []int32 `cb:"7"`
	B    bool    `cb:"8"`
}

// rowsListCounts are the list lengths the length byte changes at, at each
// element width — 0xEF bytes is the last one-byte length, 3823 the last
// two-byte one, 0xFFFF the last the kernel writes — and around them.
var rowsListCounts = []int{0, 1, 2, 6, 59, 60, 63, 64, 119, 120, 126, 127, 239, 240, 253, 254, 955, 956,
	1911, 1912, 3823, 3824, 16383, 16384, 32767, 32768, 65535, 65536}

func makeRowsFlat(random *rand.Rand) rowsFlat {
	list := func() []int32 {
		count := rowsListCounts[random.IntN(len(rowsListCounts))]
		if count > 300 && random.IntN(4) != 0 {
			count = random.IntN(20)
		}
		if count == 0 && random.IntN(2) == 0 {
			return nil
		}
		// One width for most of the list, and the widest value at a random place.
		widest := []int64{0, 1, 127, -128, 128, -129, 32767, -32768, 32768, -32769,
			math.MaxInt32, math.MinInt32}[random.IntN(12)]
		out := make([]int32, count)
		for index := range out {
			out[index] = int32(random.Int64N(2*max(1, widest/2+1)) - max(1, widest/2+1))
		}
		if count != 0 {
			out[random.IntN(count)] = int32(widest)
		}
		return out
	}
	edge := func() int64 { return rowsEdges[random.IntN(len(rowsEdges))] }
	text := strings.Repeat("x", rowsLengths[random.IntN(len(rowsLengths))])
	return rowsFlat{U64: uint64(edge()), I64: edge(), F64: random.NormFloat64(), S: text,
		L: list(), I32: int32(edge()), More: list(), B: random.IntN(2) == 0}
}

// A flat root is one kernel entry onto whatever room the caller's buffer has:
// the walk's bytes at every capacity, and a buffer the message fits is written
// in place rather than grown for the kernel's whole-word stores.
func TestFlatRootWritesTheWalksBytes(t *testing.T) {
	if program := MustCodec[rowsFlat]().plan.rows; program == nil || !program.flat {
		t.Fatal("rowsFlat compiled to no flat program")
	}
	codec := MustCodec[rowsFlat]()
	random := rand.New(rand.NewPCG(23, 24))
	for trial := range 600 {
		value := makeRowsFlat(random)
		useRowPrograms = false
		want := must(codec.Encode(&value))
		useRowPrograms = true
		prefix := []byte("prefix")
		capacities := []int{0, len(want), len(want) + 1, len(want) + 8, len(want) + 400}
		if len(want) < 300 {
			for room := range len(want) + 24 {
				capacities = append(capacities, room)
			}
		}
		for _, room := range capacities {
			buffer := append(make([]byte, 0, len(prefix)+room), prefix...)
			// Append, not the codec's, which grows to its size hint up front.
			got, err := Append(buffer, &value)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got[:len(prefix)], prefix) || !bytes.Equal(got[len(prefix):], want) {
				t.Fatalf("trial %d, room %d: %d bytes, the walk's %d\n%+v", trial, room, len(got)-len(prefix), len(want), value)
			}
			// The walk appends a magnitude's whole word and cuts it back, so only
			// the kernel writes in place.
			if useRowKernel && room >= len(want) && &got[0] != &buffer[0] {
				t.Fatalf("trial %d: a buffer with room %d for %d bytes was grown", trial, room, len(want))
			}
		}
		var back rowsFlat
		if err := Unmarshal(want, &back); err != nil {
			t.Fatalf("trial %d: %v", trial, err)
		}
	}
}

// rowsFlatHidden has a field outside its plan, which a decode clears and a
// flat read would not reach: it must not take readFlat.
type rowsFlatHidden struct {
	A int64 `cb:"1"`
	//lint:ignore U1000 read only through reflection: a decode clears it.
	hidden int64
}

// A flat read decodes what the generic walk does, into a used record, for
// every message: the encoder's, and the same cut short, with a byte changed,
// with its fields swapped or repeated, from another type, and random bytes.
// What it refuses the walk decodes again, so the errors must agree too.
func TestFlatReadDecodesWhatTheWalkDoes(t *testing.T) {
	if MustCodec[rowsFlat]().plan.flatReads == nil {
		t.Fatal("rowsFlat does not read flat")
	}
	if MustCodec[rowsFlatHidden]().plan.flatReads != nil {
		t.Fatal("a type with a field outside its plan reads flat")
	}
	random := rand.New(rand.NewPCG(25, 26))
	codec := MustCodec[rowsFlat]()
	decode := func(message []byte, used rowsFlat, flat bool) (rowsFlat, error, error) {
		useRowPrograms = flat
		defer func() { useRowPrograms = true }()
		value := used
		value.L = slices.Clone(used.L)
		viaCodec := codec.Unmarshal(message, &value)
		again := used
		again.L = slices.Clone(used.L)
		viaPackage := Unmarshal(message, &again)
		// A changed byte can make the float a NaN, which DeepEqual never
		// finds equal to itself; its bits are what must agree.
		bothNaN := math.IsNaN(value.F64) && math.Float64bits(value.F64) == math.Float64bits(again.F64)
		if bothNaN {
			value.F64, again.F64 = 0, 0
		}
		if !reflect.DeepEqual(value, again) {
			t.Fatalf("Codec.Unmarshal %+v, Unmarshal %+v", value, again)
		}
		return value, viaCodec, viaPackage
	}
	check := func(trial int, what string, message []byte, used rowsFlat) {
		t.Helper()
		want, wantErr, _ := decode(message, used, false)
		got, gotErr, gotPackageErr := decode(message, used, true)
		if (wantErr != nil) != (gotErr != nil) || (wantErr != nil) != (gotPackageErr != nil) {
			t.Fatalf("trial %d, %s: walk %v, flat %v / %v\n%x", trial, what, wantErr, gotErr, gotPackageErr, message)
		}
		if wantErr == nil && !reflect.DeepEqual(got, want) {
			t.Fatalf("trial %d, %s: walk %+v\nflat %+v\n%x", trial, what, want, got, message)
		}
	}
	for trial := range 3000 {
		value, used := makeRowsFlat(random), makeRowsFlat(random)
		message := must(codec.Encode(&value))
		check(trial, "as written", message, used)
		if into := used; !codec.plan.flatReads.readFlat(narrowBody(message), unsafe.Pointer(&into)) {
			t.Fatalf("trial %d: the encoder's own message was refused\n%+v", trial, value)
		}
		check(trial, "cut short", message[:random.IntN(len(message))], used)
		changed := slices.Clone(message)
		changed[random.IntN(len(changed))] ^= byte(1 << random.IntN(8))
		check(trial, "a byte changed", changed, used)
		// The fields of another record, in another order or twice.
		other := makeRowsFlat(random)
		mixed := must(codec.Encode(&other))
		body := narrowBody(message)
		cut := random.IntN(len(body) + 1)
		check(trial, "two records spliced", append(slices.Clone(mixed), body[:cut]...), used)
		noise := make([]byte, random.IntN(40))
		for index := range noise {
			noise[index] = byte(random.Uint32())
		}
		check(trial, "random", append([]byte{rootStructNarrow}, noise...), used)
		// rowsEvery's keys read as rowsFlat's: every form against every op.
		every := makeRowsEvery(random)
		every.M, every.P, every.Sub = nil, nil, nil
		check(trial, "another type", must(Marshal(&every)), used)
	}
}
