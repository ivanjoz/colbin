package codec

import (
	"bytes"
	"fmt"
	"reflect"
	"testing"
)

// withDictionary runs f with dictionaries on, and restores the switch after.
func withDictionary(t testing.TB, f func()) {
	t.Helper()
	was := Dictionary()
	SetDictionary(true)
	defer SetDictionary(was)
	f()
}

// dictRow is a narrow row: two low-cardinality string columns, one that is not,
// and integers between them.
type dictRow struct {
	ID       int32  `cb:"1"`
	Status   string `cb:"2"`
	Country  string `cb:"3"`
	Note     string `cb:"4"`
	Quantity int64  `cb:"5"`
}

// dictWideRow is a row whose keys need eight bits.
type dictWideRow struct {
	ID       int32  `cb:"1"`
	Currency string `cb:"20"`
	Status   string `cb:"21"`
}

type dictAddress struct {
	City  string    `cb:"1"`
	Lines []dictRow `cb:"2"`
}

// dictHolder nests tables of the same row type at two depths, which share a
// dictionary per field, and holds a wide-row table and a flat field beside them.
type dictHolder struct {
	Head    uint32        `cb:"1"`
	Rows    []dictRow     `cb:"2"`
	Wide    []dictWideRow `cb:"3"`
	Address dictAddress   `cb:"4"`
	Name    string        `cb:"5"`
}

// dictWideHolder is dictHolder's shape under a wide parent.
type dictWideHolder struct {
	Head uint32        `cb:"1"`
	Rows []dictRow     `cb:"30"`
	Wide []dictWideRow `cb:"31"`
}

var (
	dictStatuses   = []string{"PENDING", "SHIPPED", "DELIVERED", "CANCELLED"}
	dictCountries  = []string{"Peru", "Chile", "Mexico", "España", "United States"}
	dictCurrencies = []string{"USD", "PEN", "CLP", "EUR"}
)

func makeDictRows(n, seed int) []dictRow {
	rows := make([]dictRow, n)
	for index := range rows {
		rows[index] = dictRow{
			ID:       int32(seed + index),
			Status:   dictStatuses[(index*7+seed)%len(dictStatuses)],
			Country:  dictCountries[(index+seed)%len(dictCountries)],
			Note:     fmt.Sprintf("note %d-%d", seed, index),
			Quantity: int64(index * 3),
		}
	}
	return rows
}

func makeDictWideRows(n int) []dictWideRow {
	rows := make([]dictWideRow, n)
	for index := range rows {
		rows[index] = dictWideRow{
			ID:       int32(index),
			Currency: dictCurrencies[index%len(dictCurrencies)],
			Status:   dictStatuses[index%len(dictStatuses)],
		}
	}
	return rows
}

func makeDictHolder(n int) dictHolder {
	return dictHolder{
		Head: 7,
		Rows: makeDictRows(n, 0),
		Wide: makeDictWideRows(n),
		Address: dictAddress{
			City:  "Lima",
			Lines: makeDictRows(n, 3),
		},
		Name: "holder",
	}
}

func TestDictionaryRoundTrip(t *testing.T) {
	for _, n := range []int{0, 1, tableThreshold - 1, tableThreshold, 100, 1000, 70_000} {
		value := makeDictHolder(n)
		plain := must(Marshal(&value))
		var message []byte
		withDictionary(t, func() { message = must(Marshal(&value)) })

		// What the message without dictionaries reads back as: an empty slice
		// reads back nil, with or without them.
		var want dictHolder
		if err := Unmarshal(plain, &want); err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		var back dictHolder
		if err := Unmarshal(message, &back); err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if !reflect.DeepEqual(back, want) {
			t.Fatalf("n=%d: round-tripped wrong", n)
		}
		var handled dictHolder
		if err := MustCodec[dictHolder]().Unmarshal(message, &handled); err != nil {
			t.Fatalf("n=%d: Codec.Unmarshal: %v", n, err)
		}
		if !reflect.DeepEqual(handled, want) {
			t.Fatalf("n=%d: Codec.Unmarshal round-tripped wrong", n)
		}
		if n >= tableThreshold {
			if message[0]&rootDictionary == 0 {
				t.Fatalf("n=%d: no dictionary was used", n)
			}
			if len(message) >= len(plain) {
				t.Fatalf("n=%d: %d bytes with dictionaries, %d without", n, len(message), len(plain))
			}
		} else if !bytes.Equal(message, plain) {
			t.Fatalf("n=%d: no table, yet the message changed", n)
		}
		t.Logf("n=%d: %d bytes → %d with dictionaries", n, len(plain), len(message))
	}
}

func TestDictionaryRoundTripWideParent(t *testing.T) {
	value := dictWideHolder{Head: 1, Rows: makeDictRows(300, 1), Wide: makeDictWideRows(300)}
	plain := must(Marshal(&value))
	var message []byte
	withDictionary(t, func() { message = must(Marshal(&value)) })
	if message[0]&rootWide == 0 || message[0]&rootDictionary == 0 {
		t.Fatalf("root %#02x: want a wide root with dictionaries", message[0])
	}
	var back dictWideHolder
	if err := Unmarshal(message, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, value) {
		t.Fatal("round-tripped wrong")
	}
	t.Logf("%d bytes → %d with dictionaries", len(plain), len(message))
}

// Tables of one row type at two depths use one dictionary per field, not one
// per table.
func TestDictionaryIsPerField(t *testing.T) {
	value := dictHolder{Rows: makeDictRows(50, 0), Address: dictAddress{Lines: makeDictRows(50, 1)}}
	var message []byte
	withDictionary(t, func() { message = must(Marshal(&value)) })
	dicts, _, err := parseDictSection(message[1:])
	if err != nil {
		t.Fatal(err)
	}
	// Status and Country; Note does not repeat.
	if len(dicts) != 2 {
		t.Fatalf("%d dictionaries, want 2: %q", len(dicts), dicts)
	}
	if len(dicts[0]) != len(dictStatuses) || len(dicts[1]) != len(dictCountries) {
		t.Fatalf("dictionaries %q", dicts)
	}
}

// A column whose strings do not repeat goes out as strings, and a message that
// ends up with no dictionary is exactly the one written with them off.
// The pooled dictionaries carry nothing from one message into the next: a
// message comes out the same after a larger one, with other values, used them.
func TestDictionaryPoolCarriesNothing(t *testing.T) {
	small := makeDictHolder(300)
	large := dictHolder{Rows: makeDictRows(5000, 11), Address: dictAddress{Lines: makeDictRows(4000, 5)}}
	for index := range large.Rows {
		large.Rows[index].Status = fmt.Sprintf("other-%d", index%40)
	}
	withDictionary(t, func() {
		want := must(Marshal(&small))
		for range 3 {
			must(Marshal(&large))
			if got := must(Marshal(&small)); !bytes.Equal(got, want) {
				t.Fatal("a message changed after another used the pool")
			}
			var back dictHolder
			if err := Unmarshal(must(Marshal(&large)), &back); err != nil {
				t.Fatal(err)
			}
		}
	})
}

func TestDictionaryUnusedIsByteIdentical(t *testing.T) {
	rows := makeDictRows(200, 0)
	for index := range rows {
		rows[index].Status = fmt.Sprintf("status-%d", index)
		rows[index].Country = fmt.Sprintf("country-%d", index)
	}
	value := tableHolder{Head: 3, Rows: makeTableRows(200)}
	for index := range value.Rows {
		value.Rows[index].Name = fmt.Sprintf("name-%d", index)
	}
	for _, v := range []any{&value, &dictHolder{Rows: rows}} {
		plain := must(Marshal(v))
		var message []byte
		withDictionary(t, func() { message = must(Marshal(v)) })
		if !bytes.Equal(message, plain) {
			t.Fatalf("%T: no column repeats, yet the message changed", v)
		}
	}
}

// Strings that dictHash cannot tell apart — equal in every word it reads —
// collide into one probe run, and past maxDictProbe the column goes out as
// strings rather than probing on: correct, and no slower than a short run.
func TestDictionaryHashCollisionsFallBack(t *testing.T) {
	collide := func(index int) string {
		// 40 bytes: dictHash reads 0..8, 32..40 and 16..24; the index sits in
		// 8..16, which it never reads.
		return "AAAAAAAA" + fmt.Sprintf("%08d", index) + "MMMMMMMMMMMMMMMMZZZZZZZZ"
	}
	if dictHash(collide(1)) != dictHash(collide(2)) {
		t.Fatal("the strings were meant to collide")
	}
	rows := makeDictRows(2000, 0)
	for index := range rows {
		rows[index].Status = collide(index % 100) // 100 values, all colliding
	}
	value := dictHolder{Rows: rows}
	var message []byte
	withDictionary(t, func() { message = must(Marshal(&value)) })
	dicts, _, err := parseDictSection(message[1:])
	if err != nil {
		t.Fatal(err)
	}
	for _, dict := range dicts {
		if len(dict) > 0 && len(dict[0]) == 40 {
			t.Fatalf("the colliding column got a dictionary of %d entries", len(dict))
		}
	}
	var back dictHolder
	if err := Unmarshal(message, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, value) {
		t.Fatal("round-tripped wrong")
	}
}

// The index finds every value it was given, at every size it grows through,
// and a rollback leaves it finding exactly the values kept.
func TestDictionaryIndex(t *testing.T) {
	entries := newDictEntries()
	find := func(value string) (uint32, bool, bool) {
		lo, hi := shortKey(value)
		return entries.find(value, lo, hi)
	}
	var values []string
	for index := range 5000 {
		// Short and long values both, so both tables grow.
		value := fmt.Sprintf("%016x", index*2654435761)[:1+index%11]
		if index%3 == 0 {
			value = fmt.Sprintf("long-%032x", index*2654435761)
		}
		if _, found, ok := find(value); !ok {
			t.Fatalf("probe ran out at %d entries", len(entries.values))
		} else if !found {
			lo, hi := shortKey(value)
			if len(value) > shortKeyBytes {
				lo, hi = 0, 0
			}
			if !entries.insert(value, lo, hi) {
				t.Fatalf("grow failed at %d entries", len(entries.values))
			}
			values = append(values, value)
		}
	}
	check := func(kept int) {
		t.Helper()
		for want, value := range values[:kept] {
			if id, found, _ := find(value); !found || int(id) != want {
				t.Fatalf("%q: id %d found %v, want %d", value, id, found, want)
			}
		}
		for _, value := range values[kept:] {
			if _, found, _ := find(value); found {
				t.Fatalf("%q found after rollback", value)
			}
		}
	}
	check(len(values))
	entries.rollback(len(values) / 3)
	check(len(values) / 3)
}

// referenceKey is shortKey spelled byte by byte: the string's bytes as two
// little-endian words, zero past its end.
func referenceKey(value string) (lo, hi uint64) {
	for at := range len(value) {
		if at < 8 {
			lo |= uint64(value[at]) << (8 * at)
		} else {
			hi |= uint64(value[at]) << (8 * (at - 8))
		}
	}
	return lo, hi
}

// shortKey agrees with the reference at every length and alignment.
func TestDictionaryShortKeys(t *testing.T) {
	source := "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	for n := range shortKeyBytes + 1 {
		for offset := range 8 {
			value := source[offset : offset+n]
			lo, hi := shortKey(value)
			if wantLo, wantHi := referenceKey(value); lo != wantLo || hi != wantHi {
				t.Fatalf("shortKey(%q) = %#x %#x, want %#x %#x", value, lo, hi, wantLo, wantHi)
			}
		}
	}
}

// kernelRows is a table built to make dictKernel stop everywhere it can: every
// length from empty to past sixteen, values new and repeated, in runs and not.
func kernelRows(n int) []dictRow {
	source := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij"
	rows := make([]dictRow, n)
	for index := range rows {
		length := (index * 7) % 21
		offset := (index * 13) % 8
		status := source[offset : offset+length]
		if index%5 == 0 {
			status = dictStatuses[index%len(dictStatuses)]
		}
		rows[index] = dictRow{
			ID:      int32(index),
			Status:  status,
			Country: dictCountries[(index/3)%len(dictCountries)],
			Note:    fmt.Sprintf("n%d", index%40),
		}
	}
	return rows
}

// The kernel writes exactly what the Go loop writes, byte for byte.
func TestDictionaryKernelMatchesGo(t *testing.T) {
	for _, n := range []int{8, 100, 1023, 1024, 1025, 3000} {
		value := dictHolder{Rows: kernelRows(n), Address: dictAddress{Lines: kernelRows(n / 2)}}
		var withKernel, withoutKernel []byte
		withDictionary(t, func() {
			withKernel = must(Marshal(&value))
			was := setDictKernel(false)
			withoutKernel = must(Marshal(&value))
			setDictKernel(was)
		})
		if !bytes.Equal(withKernel, withoutKernel) {
			t.Fatalf("n=%d: the kernel's message differs from the Go loop's", n)
		}
		var back dictHolder
		if err := Unmarshal(withKernel, &back); err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if !reflect.DeepEqual(back.Rows, value.Rows) {
			t.Fatalf("n=%d: round-tripped wrong", n)
		}
	}
}

// Every prefix of a message with dictionaries is refused or reads as the
// fields it holds, as a prefix of any other message is.
func TestDictionaryTruncationIsRefused(t *testing.T) {
	withDictionary(t, func() {
		value := makeDictHolder(40)
		checkPrefixes(t, &value, func(data []byte, back *dictHolder) error { return Unmarshal(data, back) })
	})
}

// No byte of a message with dictionaries, changed to any value, panics a
// decode.
func TestDictionaryCorruptionDoesNotPanic(t *testing.T) {
	value := makeDictHolder(20)
	var message []byte
	withDictionary(t, func() { message = must(Marshal(&value)) })
	corrupt := make([]byte, len(message))
	for at := range message {
		for _, b := range []byte{0x00, 0x01, 0x7F, 0xFE, 0xFF, message[at] ^ 0x01, message[at] ^ 0x80} {
			copy(corrupt, message)
			corrupt[at] = b
			var back dictHolder
			_ = Unmarshal(corrupt, &back)
		}
	}
}

// What does not read a dictionary yet refuses it rather than misreading it.
func TestDictionaryRefusedWhereUnsupported(t *testing.T) {
	value := makeDictHolder(50)
	var message []byte
	withDictionary(t, func() { message = must(Marshal(&value)) })
	schema := must(SchemaFor[dictHolder]())
	if _, err := ToJSON(schema, message); err == nil {
		t.Fatal("ToJSON read a message with dictionaries")
	}
}

func BenchmarkDictionary(b *testing.B) {
	value := makeDictHolder(1000)
	for _, on := range []bool{false, true} {
		var message []byte
		SetDictionary(on)
		message = must(Marshal(&value))
		SetDictionary(false)
		b.Run(fmt.Sprintf("append/dict=%v", on), func(b *testing.B) {
			SetDictionary(on)
			defer SetDictionary(false)
			handle := MustCodec[dictHolder]()
			buffer := make([]byte, 0, 1<<17)
			b.ReportAllocs()
			b.ReportMetric(float64(len(message)), "bytes")
			for b.Loop() {
				buffer, _ = handle.Append(buffer[:0], &value)
			}
		})
		b.Run(fmt.Sprintf("unmarshal/dict=%v", on), func(b *testing.B) {
			handle := MustCodec[dictHolder]()
			var back dictHolder
			b.ReportAllocs()
			for b.Loop() {
				if err := handle.Unmarshal(message, &back); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
