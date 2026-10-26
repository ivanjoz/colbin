package codec

import (
	"math/rand/v2"
	"reflect"
	"testing"
	"unsafe"
)

// A decode into a value that held an earlier message must leave what a decode
// into a zero value does, field for field, nil and empty alike; and where a list
// fits in what the value held, it must be decoded into that array.

type resetPage struct {
	Sales []resetSale `cb:"1"`
	Note  string      `cb:"2"`
}

type resetSale struct {
	ID     uint64            `cb:"1"`
	Name   string            `cb:"2"`
	Lines  []resetLine       `cb:"3"`
	Tags   []string          `cb:"4"`
	Blob   []byte            `cb:"5"`
	Ints   []int32           `cb:"6"`
	Inner  resetInner        `cb:"7"`
	Attrs  map[string]string `cb:"8"`
	Parent *resetLine        `cb:"9"`
	hidden []int
}

type resetInner struct {
	Codes []uint16 `cb:"1"`
	Label string   `cb:"2"`
}

// resetLine is a table's row type: past tableThreshold rows, Lines is a table.
type resetLine struct {
	Qty   uint32 `cb:"1"`
	Price int64  `cb:"2"`
	Note  string `cb:"3"`
}

// resetWide's keys come from its field names, so it is read at eight bits.
type resetWide struct {
	Alpha []resetLine
	Beta  []int32
	Gamma string
	Delta []resetSale
}

func resetLength(random *rand.Rand) int {
	// -1 is nil, 0 empty, and the rest cross the table threshold.
	return random.IntN(2*tableThreshold+3) - 1
}

func makeResetLines(random *rand.Rand) []resetLine {
	n := resetLength(random)
	if n < 0 {
		return nil
	}
	lines := make([]resetLine, n)
	for index := range lines {
		lines[index] = resetLine{Qty: random.Uint32N(5), Price: random.Int64N(1000) - 500}
		if random.IntN(3) == 0 {
			lines[index].Note = "note"
		}
	}
	return lines
}

func makeResetSale(random *rand.Rand) resetSale {
	sale := resetSale{ID: random.Uint64N(3), Lines: makeResetLines(random)}
	if random.IntN(2) == 0 {
		sale.Name = "sale"
	}
	if n := resetLength(random); n >= 0 {
		sale.Tags = make([]string, n)
		for index := range sale.Tags {
			sale.Tags[index] = string(rune('a' + index))
		}
	}
	if n := resetLength(random); n >= 0 {
		sale.Blob = make([]byte, n)
		for index := range sale.Blob {
			sale.Blob[index] = byte(index + 1)
		}
	}
	if n := resetLength(random); n >= 0 {
		sale.Ints = make([]int32, n)
		for index := range sale.Ints {
			sale.Ints[index] = int32(index) - 3
		}
	}
	if n := resetLength(random); n > 0 {
		sale.Inner.Codes = make([]uint16, n)
	}
	if random.IntN(3) == 0 {
		sale.Inner.Label = "inner"
	}
	if random.IntN(3) == 0 {
		sale.Attrs = map[string]string{"k": "v"}
	}
	if random.IntN(3) == 0 {
		sale.Parent = &resetLine{Qty: 1}
	}
	return sale
}

func makeResetPage(random *rand.Rand) resetPage {
	page := resetPage{}
	if n := resetLength(random); n >= 0 {
		page.Sales = make([]resetSale, n)
		for index := range page.Sales {
			page.Sales[index] = makeResetSale(random)
		}
	}
	if random.IntN(2) == 0 {
		page.Note = "page"
	}
	return page
}

// testReuse decodes every message into one value, and each into a zero one too,
// and wants the two the same after every decode.
func testReuse[T any](t *testing.T, make func(*rand.Rand) T, dirty func(*T)) {
	t.Helper()
	codec, err := NewCodec[T]()
	if err != nil {
		t.Fatal(err)
	}
	random := rand.New(rand.NewPCG(1, 2))
	var held, heldPackage T
	for round := range 400 {
		value := make(random)
		data, err := codec.Encode(&value)
		if err != nil {
			t.Fatal(err)
		}
		var fresh T
		if err := codec.Unmarshal(data, &fresh); err != nil {
			t.Fatal(err)
		}
		if dirty != nil {
			dirty(&held)
			dirty(&heldPackage)
		}
		// A decode that fails part way leaves the value unspecified, but the next
		// one into it must come out as into a new value.
		if len(data) > 2 && round%3 == 0 {
			broken := append([]byte(nil), data[:1+random.IntN(len(data)-1)]...)
			broken[len(broken)/2] ^= byte(1 + random.IntN(255))
			_ = codec.Unmarshal(broken, &held)
			_ = Unmarshal(broken, &heldPackage)
		}
		if err := codec.Unmarshal(data, &held); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(held, fresh) {
			t.Fatalf("round %d: Codec.Unmarshal into a used value\n got %#v\nwant %#v", round, held, fresh)
		}
		if err := Unmarshal(data, &heldPackage); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(heldPackage, fresh) {
			t.Fatalf("round %d: Unmarshal into a used value\n got %#v\nwant %#v", round, heldPackage, fresh)
		}
	}
}

func TestUnmarshalReuseMatchesFresh(t *testing.T) {
	// An unexported field is cleared as before, slice or not.
	dirtyHidden := func(page *resetPage) {
		for index := range page.Sales {
			page.Sales[index].hidden = []int{1, 2, 3}
		}
	}
	t.Run("narrow", func(t *testing.T) { testReuse(t, makeResetPage, dirtyHidden) })
	t.Run("wide", func(t *testing.T) {
		testReuse(t, func(random *rand.Rand) resetWide {
			page := makeResetPage(random)
			wide := resetWide{Alpha: makeResetLines(random), Delta: page.Sales, Gamma: page.Note}
			if n := resetLength(random); n >= 0 {
				wide.Beta = make([]int32, n)
			}
			return wide
		}, nil)
	})
	t.Run("root slice", func(t *testing.T) {
		testReuse(t, func(random *rand.Rand) []resetSale { return makeResetPage(random).Sales }, nil)
	})
}

func TestUnmarshalReusesArrays(t *testing.T) {
	codec, err := NewCodec[resetPage]()
	if err != nil {
		t.Fatal(err)
	}
	encode := func(sales, lines, ints int) []byte {
		page := resetPage{Sales: make([]resetSale, sales)}
		for index := range page.Sales {
			page.Sales[index] = resetSale{ID: 1, Lines: make([]resetLine, lines), Ints: make([]int32, ints)}
			for line := range page.Sales[index].Lines {
				page.Sales[index].Lines[line].Qty = 1
			}
			for at := range page.Sales[index].Ints {
				page.Sales[index].Ints[at] = 7
			}
		}
		data, err := codec.Encode(&page)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	var page resetPage
	// Lines as a list (3) and as a table (2*tableThreshold).
	for _, lines := range []int{3, 2 * tableThreshold} {
		if err := codec.Unmarshal(encode(4, lines, 5), &page); err != nil {
			t.Fatal(err)
		}
		sales := unsafe.SliceData(page.Sales)
		first, ints := unsafe.SliceData(page.Sales[0].Lines), unsafe.SliceData(page.Sales[0].Ints)
		if err := codec.Unmarshal(encode(3, lines-1, 4), &page); err != nil {
			t.Fatal(err)
		}
		if unsafe.SliceData(page.Sales) != sales || unsafe.SliceData(page.Sales[0].Lines) != first ||
			unsafe.SliceData(page.Sales[0].Ints) != ints {
			t.Fatalf("%d lines: a smaller message was not decoded into the arrays the value held", lines)
		}
		if len(page.Sales) != 3 || len(page.Sales[0].Lines) != lines-1 || len(page.Sales[0].Ints) != 4 {
			t.Fatalf("%d lines: lengths %d, %d, %d", lines, len(page.Sales), len(page.Sales[0].Lines), len(page.Sales[0].Ints))
		}
		// A larger one does not fit and gets arrays of its own.
		if err := codec.Unmarshal(encode(8, lines+1, 6), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Sales) != 8 || len(page.Sales[7].Lines) != lines+1 || page.Sales[7].Ints[5] != 7 {
			t.Fatalf("%d lines: a larger message decoded wrong", lines)
		}
	}
}

func TestResetProgramLayouts(t *testing.T) {
	type plain struct {
		A int64
		B [3]uint16
	}
	type arrays struct {
		A [2]string
		B [maxResetArray + 1]*int
	}
	if program := compileReset(reflect.TypeFor[plain]()); program == nil || program.keeps() ||
		len(program.steps) != 1 || program.steps[0].kind != resetBytes {
		t.Fatalf("a struct without pointers is one run of bytes, got %+v", program)
	}
	if compileReset(reflect.TypeFor[arrays]()) != nil {
		t.Fatal("an array of pointers past maxResetArray is cleared whole")
	}
}
