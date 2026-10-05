package codec

import (
	"bytes"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/ivanjoz/colbin/wire"
)

// Each test here is an input that once hung the decoder, crashed the process or
// came back as different data without an error. The rule they pin: a decode
// returns, in time and memory proportional to its input, and either gives back
// what was written or says why not.

// returns fails the test when run does not, which is what a hang looks like
// from inside a test; the goroutine is leaked, which is the lesser problem.
func returns(t *testing.T, what string, run func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		run()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not return", what)
	}
}

// refusedEverywhere decodes data into a fresh T and through both schema walks,
// and wants every one of them to fail with want — or with any error, for a nil
// want.
func refusedEverywhere[T any](t *testing.T, data []byte, want error) {
	t.Helper()
	check := func(path string, err error) {
		t.Helper()
		if err == nil || (want != nil && !errors.Is(err, want)) {
			t.Errorf("%s: got %v, want %v", path, err, want)
		}
	}
	var zero T
	schema, err := SchemaOf(zero)
	if err != nil {
		t.Fatal(err)
	}
	returns(t, "Unmarshal", func() {
		var into T
		check("Unmarshal", Unmarshal(data, &into))
	})
	returns(t, "ToJSON", func() {
		_, err := ToJSON(schema, data)
		check("ToJSON", err)
	})
	returns(t, "DecodeAny", func() {
		_, err := DecodeAny(schema, data)
		check("DecodeAny", err)
	})
}

// A struct with eight-bit keys, in a list long enough to be a table, under a
// struct with four-bit keys. The narrow writer used to shift the columns' keys
// into a nibble, so key 17 landed on key 1: {X:100 Y:900} came back {X:900 Y:0}
// with no error.
func TestANarrowTableOfWideRowsRoundTrips(t *testing.T) {
	type row struct {
		X int64  `cb:"2"`
		Y int64  `cb:"18"`
		S string `cb:"19"`
	}
	type holder struct {
		Rows []row `cb:"1"`
	}
	for _, n := range []int{1, tableThreshold - 1, tableThreshold, 20, 300} {
		value := holder{}
		for index := range n {
			value.Rows = append(value.Rows, row{X: 100 + int64(index), Y: 900, S: "s"})
		}
		message, err := Marshal(&value)
		if err != nil {
			t.Fatal(err)
		}
		if message[0] != rootStructNarrow {
			t.Fatalf("the holder went wide, so this tests nothing: %#02x", message[0])
		}
		var back holder
		if err := Unmarshal(message, &back); err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if !reflect.DeepEqual(back, value) {
			t.Fatalf("n=%d: round-tripped as %+v", n, back.Rows[0])
		}
		text, err := ToJSON(mustSchema(t, value), message)
		if err != nil {
			t.Fatalf("n=%d: ToJSON: %v", n, err)
		}
		if !bytes.Contains(text, []byte(`"Y":900`)) {
			t.Fatalf("n=%d: ToJSON lost the wide column: %s", n, text)
		}
	}
}

// Two bytes: a narrow root and one field, on a type whose only field is `any`.
// The narrow decode had no arm for it and no default, so it never advanced.
func TestAnAnyUnderANarrowRootIsRefused(t *testing.T) {
	type holder struct {
		A any `cb:"1"`
	}
	refusedEverywhere[holder](t, []byte{rootStructNarrow, 0x01}, nil)
}

// Three bytes: a key and an inline value where a []any belongs, which panicked
// slicing at -1. The typed decode refuses it — a []any cannot hold 48 — and the
// schema walks render what the message holds, as they do any dynamic value.
func TestAScalarWhereAListOfAnyBelongs(t *testing.T) {
	type holder struct {
		Vals []any `cb:"1"`
	}
	data := []byte{rootStructWide, 0x00, 0x30}
	returns(t, "Unmarshal", func() {
		var back holder
		if err := Unmarshal(data, &back); err == nil {
			t.Errorf("a scalar was decoded into a []any: %v", back.Vals)
		}
	})
	returns(t, "ToJSON", func() {
		if _, err := ToJSON(mustSchema(t, holder{}), data); err != nil {
			t.Errorf("ToJSON: %v", err)
		}
	})
}

// A count is a number the peer chose. A list of four billion structs in nine
// bytes was allocated before any element was read.
func TestACountPastTheMessageIsRefused(t *testing.T) {
	writer := wire.Writer{Buffer: []byte{rootStructNarrow}}
	writer.Close(writer.OpenList(1, math.MaxInt32))
	refusedEverywhere[basket](t, writer.Buffer, nil)
}

// A table's rows are not bounded by its bytes — a constant column is nine bytes
// for any length — so they are bounded by a budget, and the budget is per
// message: four tables each under the per-table cap are over it together.
func TestTheRowBudgetIsPerMessage(t *testing.T) {
	type cell struct {
		A uint8 `cb:"1"`
	}
	type sheet struct {
		Cells []cell `cb:"1"`
	}
	type book struct {
		Sheets []sheet `cb:"1"`
	}
	writer := wire.Writer{Buffer: []byte{rootStructNarrow}}
	list := writer.OpenList(0, 4)
	for range 4 {
		element := writer.OpenElement()
		writer.Close(writer.OpenTable(0, maxTableRows/2))
		writer.CloseElement(element)
	}
	writer.Close(list)
	if len(writer.Buffer) > 64 {
		t.Fatalf("the message is %d bytes, which is not the attack", len(writer.Buffer))
	}
	refusedEverywhere[book](t, writer.Buffer, errTooManyRows)

	// One table of the same size is within the budget, so the cap is not
	// simply refusing big tables.
	writer = wire.Writer{Buffer: []byte{rootStructNarrow}}
	list = writer.OpenList(0, 1)
	element := writer.OpenElement()
	writer.Close(writer.OpenTable(0, maxTableRows/2))
	writer.CloseElement(element)
	writer.Close(list)
	var back book
	if err := Unmarshal(writer.Buffer, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Sheets) != 1 || len(back.Sheets[0].Cells) != maxTableRows/2 {
		t.Fatalf("decoded %d sheets", len(back.Sheets))
	}
}

// Nesting is bounded on every path: a decode refuses past maxSchemaDepth rather
// than recursing until the stack runs out, and a skip steps over a nested value
// of any depth by its length, without descending.
func TestDeepDynamicNestingIsRefusedAndSkippable(t *testing.T) {
	writer := wire.Writer8{Buffer: []byte{rootStructWide}}
	writer.Key(0)
	marks := make([]wire.Mark, 0, 1000)
	for range 1000 {
		marks = append(marks, writer.OpenElementList(1))
	}
	writer.ElementNull()
	for index := len(marks) - 1; index >= 0; index-- {
		writer.Close(marks[index])
	}

	type holder struct {
		A any `cb:"1"`
	}
	refusedEverywhere[holder](t, writer.Buffer, errTooDeep)

	// A type that does not declare key 0 steps over all of it.
	type other struct {
		B int32 `cb:"20"`
	}
	var back other
	returns(t, "the skip", func() {
		if err := Unmarshal(writer.Buffer, &back); err != nil {
			t.Errorf("skipping a deep value: %v", err)
		}
	})
}

type chain struct {
	Next  *chain `cb:"1"`
	Value int32  `cb:"2"`
}

// A pointer to a struct is the one typed recursion with no bound in the type,
// so it is bounded in the walk: in a message nested past the limit, and in a
// value that points at itself.
func TestDeepTypedNestingIsRefused(t *testing.T) {
	writer := wire.Writer{Buffer: []byte{rootStructNarrow}}
	marks := make([]wire.Mark, 0, 1000)
	for range 1000 {
		marks = append(marks, writer.OpenStruct(0))
	}
	for index := len(marks) - 1; index >= 0; index-- {
		writer.Close(marks[index])
	}
	refusedEverywhere[chain](t, writer.Buffer, errTooDeep)

	loop := chain{Value: 1}
	loop.Next = &loop
	returns(t, "Marshal", func() {
		if _, err := Marshal(&loop); !errors.Is(err, errTooDeep) {
			t.Errorf("a value pointing at itself encoded with %v", err)
		}
	})

	// And a chain within the limit still goes both ways.
	var deep *chain
	for index := range maxSchemaDepth / 2 {
		deep = &chain{Next: deep, Value: int32(index)}
	}
	message, err := Marshal(deep)
	if err != nil {
		t.Fatal(err)
	}
	var back chain
	if err := Unmarshal(message, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(&back, deep) {
		t.Fatal("a chain within the limit did not round-trip")
	}
}

// A map that holds itself twice: the error was recorded at the first sibling and
// the walk carried on into the second, forever.
func TestAMapHoldingItselfTwiceIsRefused(t *testing.T) {
	cycle := map[string]any{}
	cycle["a"] = cycle
	cycle["b"] = cycle
	returns(t, "Marshal", func() {
		if _, err := Marshal(cycle); err == nil {
			t.Error("a map holding itself was encoded")
		}
	})
}

// A section is read from a peer. One whose struct holds itself by value
// describes an infinite value: walking it was 2^128 work for fifteen bytes.
func TestASectionWhoseStructHoldsItselfByValueIsRefused(t *testing.T) {
	def := wire.AppendLength([]byte{0}, 2)
	for _, name := range []string{"a", "b"} {
		def = append(def, name[0]-'a') // the key
		def = wire.AppendLength(def, 1)
		def = append(def, name...)
		def = wire.AppendLength(append(def, uint8(opStruct)), 0)
	}
	body := append(wire.AppendLength(nil, 1), def...)
	section := append(wire.AppendLength(nil, len(body)), body...)
	if _, err := ParseSchema(section); err == nil {
		t.Fatal("a struct holding itself by value was accepted")
	}
	message := append(append([]byte{rootStructNarrowSchema}, section...), 0x00)
	returns(t, "ToJSON", func() {
		if _, err := ToJSON(nil, message); err == nil {
			t.Error("a message describing an infinite value was rendered")
		}
	})
}

// A message that carries its own section is read with it, whatever schema the
// caller passes: the caller's describes some other message.
func TestAMessageSectionWinsOverTheCallers(t *testing.T) {
	type mine struct {
		Name string `cb:"1"`
	}
	type theirs struct {
		Count int64 `cb:"1"`
	}
	message, err := MarshalSelfDescribing(mine{Name: "x"})
	if err != nil {
		t.Fatal(err)
	}
	text, err := ToJSON(mustSchema(t, theirs{}), message)
	if err != nil {
		t.Fatal(err)
	}
	if string(text) != `{"Name":"x"}` {
		t.Fatalf("rendered with the caller's schema: %s", text)
	}
}
