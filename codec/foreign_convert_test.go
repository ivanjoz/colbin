package codec

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

// Each row of the conversion table in plan/FOREIGN_SECTION_DECODE_PLAN.md is a case
// here, written against a Go source type whose section differs from the target's.
// Every case runs as a lone struct, as a short list and as a table, at both key
// widths on both sides, because those are four different readers.

type narrowOne[T any] struct {
	V T `json:"v"`
}

type wideOne[T any] struct {
	V T `cb:"20" json:"v"`
}

type shape struct {
	name string
	// encode writes values as one message of this shape, and decode reads it
	// into the destination type and returns the destination's values.
	rows int
}

var conversionShapes = []shape{{"struct", 0}, {"list", 3}, {"table", 600}, {"grouped", 20000}}

// convertAll marshals vals as S (narrow or wide) in every shape, unmarshals each
// into D (narrow or wide) and compares the result with want.
func convertAll[S, D any](t *testing.T, vals []S, want []D) {
	t.Helper()
	for _, sourceWide := range []bool{false, true} {
		for _, targetWide := range []bool{false, true} {
			for _, shape := range conversionShapes {
				name := shape.name + "/source=" + width(sourceWide) + "/target=" + width(targetWide)
				t.Run(name, func(t *testing.T) {
					convertShape(t, vals, want, sourceWide, targetWide, shape)
				})
			}
		}
	}
}

func width(wide bool) string {
	if wide {
		return "wide"
	}
	return "narrow"
}

func sourceMessage[S any](t *testing.T, vals []S, wide bool, shape shape) []byte {
	t.Helper()
	var data []byte
	var err error
	switch {
	case shape.rows == 0 && wide:
		data, err = MarshalSelfDescribing(wideOne[S]{vals[0]})
	case shape.rows == 0:
		data, err = MarshalSelfDescribing(narrowOne[S]{vals[0]})
	case wide:
		rows := make([]wideOne[S], shape.rows)
		for i := range rows {
			rows[i].V = vals[i%len(vals)]
		}
		data, err = MarshalSelfDescribing(rows)
	default:
		rows := make([]narrowOne[S], shape.rows)
		for i := range rows {
			rows[i].V = vals[i%len(vals)]
		}
		data, err = MarshalSelfDescribing(rows)
	}
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func convertShape[S, D any](t *testing.T, vals []S, want []D, sourceWide, targetWide bool, shape shape) {
	t.Helper()
	data := sourceMessage(t, vals, sourceWide, shape)
	if shape.rows == 0 {
		var got D
		var err error
		if targetWide {
			var one wideOne[D]
			err = Unmarshal(data, &one)
			got = one.V
		} else {
			var one narrowOne[D]
			err = Unmarshal(data, &one)
			got = one.V
		}
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want[0]) {
			t.Fatalf("got %#v, want %#v", got, want[0])
		}
		return
	}
	var got []D
	if targetWide {
		var rows []wideOne[D]
		if err := Unmarshal(data, &rows); err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			got = append(got, row.V)
		}
	} else {
		var rows []narrowOne[D]
		if err := Unmarshal(data, &rows); err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			got = append(got, row.V)
		}
	}
	if len(got) != shape.rows {
		t.Fatalf("%d rows, want %d", len(got), shape.rows)
	}
	for i := range got {
		if !reflect.DeepEqual(got[i], want[i%len(want)]) {
			t.Fatalf("row %d = %#v, want %#v", i, got[i], want[i%len(want)])
		}
	}
}

// refuseAll is convertAll for a message that must not decode: it checks the error
// names every fragment, in every shape and width. A value-dependent refusal needs
// the offending value in the vals; a plan-time one refuses whatever they are.
func refuseAll[S, D any](t *testing.T, vals []S, fragments ...string) {
	t.Helper()
	for _, sourceWide := range []bool{false, true} {
		for _, targetWide := range []bool{false, true} {
			for _, shape := range conversionShapes {
				name := shape.name + "/source=" + width(sourceWide) + "/target=" + width(targetWide)
				t.Run(name, func(t *testing.T) {
					data := sourceMessage(t, vals, sourceWide, shape)
					var err error
					switch {
					case shape.rows == 0 && targetWide:
						err = Unmarshal(data, new(wideOne[D]))
					case shape.rows == 0:
						err = Unmarshal(data, new(narrowOne[D]))
					case targetWide:
						err = Unmarshal(data, new([]wideOne[D]))
					default:
						err = Unmarshal(data, new([]narrowOne[D]))
					}
					if err == nil {
						t.Fatal("decoded")
					}
					for _, fragment := range fragments {
						if !strings.Contains(err.Error(), fragment) {
							t.Fatalf("error %q does not say %q", err, fragment)
						}
					}
				})
			}
		}
	}
}

// The values of a refusal are placed where the table's rows reach them: in the
// shapes with several rows the bad one is the third, and the struct shape takes
// the first, so the bad value leads and the rest are fine.

func TestForeignIntegerNarrowing(t *testing.T) {
	t.Run("int64 to int8", func(t *testing.T) {
		convertAll(t, []int64{5, -128, 127, 0, -1}, []int8{5, -128, 127, 0, -1})
	})
	t.Run("int64 to int16", func(t *testing.T) {
		convertAll(t, []int64{5, -32768, 32767, 4, -1, 3}, []int16{5, -32768, 32767, 4, -1, 3})
	})
	t.Run("int64 to int32", func(t *testing.T) {
		convertAll(t, []int64{5, math.MinInt32, math.MaxInt32, 70000, -4}, []int32{5, math.MinInt32, math.MaxInt32, 70000, -4})
	})
	t.Run("int64 to uint8", func(t *testing.T) {
		convertAll(t, []int64{5, 255, 0, 4}, []uint8{5, 255, 0, 4})
	})
	t.Run("int64 to uint16", func(t *testing.T) {
		convertAll(t, []int64{5, 65535, 0, 4}, []uint16{5, 65535, 0, 4})
	})
	t.Run("int64 to uint32", func(t *testing.T) {
		convertAll(t, []int64{5, math.MaxUint32, 0, 4}, []uint32{5, math.MaxUint32, 0, 4})
	})
	t.Run("int64 to uint64", func(t *testing.T) {
		convertAll(t, []int64{5, math.MaxInt64, 0, 4}, []uint64{5, math.MaxInt64, 0, 4})
	})
	t.Run("int64 to int64", func(t *testing.T) {
		convertAll(t, []int64{5, math.MinInt64, math.MaxInt64, -1}, []int64{5, math.MinInt64, math.MaxInt64, -1})
	})
	t.Run("uint64 to int32", func(t *testing.T) {
		convertAll(t, []uint64{5, math.MaxInt32, 4}, []int32{5, math.MaxInt32, 4})
	})
	t.Run("uint64 to int64", func(t *testing.T) {
		convertAll(t, []uint64{5, math.MaxInt64, 4}, []int64{5, math.MaxInt64, 4})
	})
	t.Run("int32 to int64", func(t *testing.T) {
		convertAll(t, []int32{5, math.MinInt32, math.MaxInt32, -1}, []int64{5, math.MinInt32, math.MaxInt32, -1})
	})
}

func TestForeignIntegerRefusals(t *testing.T) {
	t.Run("int64 past int16", func(t *testing.T) {
		refuseAll[int64, int16](t, []int64{70000, 1, 2}, "70000", "int16")
	})
	t.Run("int64 past int8 below", func(t *testing.T) {
		refuseAll[int64, int8](t, []int64{-129}, "-129", "int8")
	})
	t.Run("int64 past int32", func(t *testing.T) {
		refuseAll[int64, int32](t, []int64{math.MaxInt32 + 1}, "int32")
	})
	t.Run("negative into uint8", func(t *testing.T) {
		refuseAll[int64, uint8](t, []int64{-1}, "-1", "uint8")
	})
	t.Run("negative into uint64", func(t *testing.T) {
		refuseAll[int64, uint64](t, []int64{-1}, "-1", "uint64")
	})
	t.Run("past uint32", func(t *testing.T) {
		refuseAll[int64, uint32](t, []int64{math.MaxUint32 + 1}, "uint32")
	})
	t.Run("uint64 past int64", func(t *testing.T) {
		refuseAll[uint64, int64](t, []uint64{math.MaxInt64 + 1}, "int64")
	})
	t.Run("uint64 past int32", func(t *testing.T) {
		refuseAll[uint64, int32](t, []uint64{math.MaxUint32}, "uint64 value 4294967295", "int32")
	})
}

func TestForeignIntegerIntoFloat(t *testing.T) {
	t.Run("int64 to float64", func(t *testing.T) {
		convertAll(t, []int64{5, -3, 1 << 40, 0, 12}, []float64{5, -3, 1 << 40, 0, 12})
	})
	t.Run("int64 to float32", func(t *testing.T) {
		convertAll(t, []int64{5, -3, 1 << 20, 0, 12}, []float32{5, -3, 1 << 20, 0, 12})
	})
	t.Run("uint64 to float64", func(t *testing.T) {
		convertAll(t, []uint64{5, 1 << 40, 12}, []float64{5, 1 << 40, 12})
	})
}

func TestForeignFloat(t *testing.T) {
	t.Run("float64 to float32", func(t *testing.T) {
		convertAll(t, []float64{1.5, -0.25, 3.14159, 1e30, 0, 7}, []float32{1.5, -0.25, float32(3.14159), 1e30, 0, 7})
	})
	t.Run("float32 to float64", func(t *testing.T) {
		convertAll(t, []float32{1.5, -0.25, 3.5, 0, 7}, []float64{1.5, -0.25, 3.5, 0, 7})
	})
	t.Run("float64 to float64", func(t *testing.T) {
		convertAll(t, []float64{1.5, math.Pi, 0, math.Inf(1)}, []float64{1.5, math.Pi, 0, math.Inf(1)})
	})
	t.Run("float64 too large for a float32", func(t *testing.T) {
		refuseAll[float64, float32](t, []float64{1e300, 1, 2}, "float32")
	})
	for name, test := range map[string]func(*testing.T){
		"int8":   func(t *testing.T) { refuseAll[float64, int8](t, []float64{1.5}, "never stored in an integer") },
		"int32":  func(t *testing.T) { refuseAll[float64, int32](t, []float64{2}, "never stored in an integer") },
		"int64":  func(t *testing.T) { refuseAll[float64, int64](t, []float64{2}, "float64", "int64") },
		"uint16": func(t *testing.T) { refuseAll[float64, uint16](t, []float64{2}, "never stored in an integer") },
		"f32 to int32": func(t *testing.T) {
			refuseAll[float32, int32](t, []float32{2}, "never stored in an integer")
		},
	} {
		t.Run("float64 into "+name, test)
	}
}

func TestForeignIntegerArrays(t *testing.T) {
	t.Run("int64s to int8s", func(t *testing.T) {
		convertAll(t, [][]int64{{1, -2, 127}, {-128, 0, 5}}, [][]int8{{1, -2, 127}, {-128, 0, 5}})
	})
	t.Run("int64s to int16s", func(t *testing.T) {
		convertAll(t, [][]int64{{1, -2, 300}, {-32768, 7}}, [][]int16{{1, -2, 300}, {-32768, 7}})
	})
	t.Run("int64s to int32s", func(t *testing.T) {
		convertAll(t, [][]int64{{1, -2, 70000}, {math.MinInt32, 4}}, [][]int32{{1, -2, 70000}, {math.MinInt32, 4}})
	})
	t.Run("int64s to uint16s", func(t *testing.T) {
		convertAll(t, [][]int64{{1, 2, 65535}, {0}}, [][]uint16{{1, 2, 65535}, {0}})
	})
	t.Run("int64s to uint32s", func(t *testing.T) {
		convertAll(t, [][]int64{{1, 2, math.MaxUint32}, {0}}, [][]uint32{{1, 2, math.MaxUint32}, {0}})
	})
	t.Run("int64s to uint64s", func(t *testing.T) {
		convertAll(t, [][]int64{{1, 2, math.MaxInt64}, {0}}, [][]uint64{{1, 2, math.MaxInt64}, {0}})
	})
	t.Run("uint64s to int32s", func(t *testing.T) {
		convertAll(t, [][]uint64{{1, 2, math.MaxInt32}, {4}}, [][]int32{{1, 2, math.MaxInt32}, {4}})
	})
	t.Run("uint64s to int64s", func(t *testing.T) {
		convertAll(t, [][]uint64{{1, 2, math.MaxInt64}, {4}}, [][]int64{{1, 2, math.MaxInt64}, {4}})
	})
	t.Run("int32s to int64s", func(t *testing.T) {
		convertAll(t, [][]int32{{1, -2, math.MaxInt32}, {4}}, [][]int64{{1, -2, math.MaxInt32}, {4}})
	})
	t.Run("same on both sides", func(t *testing.T) {
		convertAll(t, [][]int64{{1, -2, math.MaxInt64}, {4}}, [][]int64{{1, -2, math.MaxInt64}, {4}})
	})
	t.Run("element past int8", func(t *testing.T) {
		refuseAll[[]int64, []int8](t, [][]int64{{1, 200, 3}}, "element 1", "200", "[]int8")
	})
	t.Run("negative element into uint32s", func(t *testing.T) {
		refuseAll[[]int64, []uint32](t, [][]int64{{1, -2}}, "element 1", "-2", "[]uint32")
	})
	t.Run("uint64s past int64s", func(t *testing.T) {
		refuseAll[[]uint64, []int64](t, [][]uint64{{1, math.MaxInt64 + 1}}, "[]int64")
	})
}

func TestForeignPointers(t *testing.T) {
	// A pointer in the section, a value in the Go type: null is zero.
	t.Run("*int64 to int32", func(t *testing.T) {
		convertAll(t, []*int64{ptr[int64](5), nil, ptr[int64](-7), ptr[int64](0), nil},
			[]int32{5, 0, -7, 0, 0})
	})
	t.Run("*int64 to float64", func(t *testing.T) {
		convertAll(t, []*int64{ptr[int64](5), nil, ptr[int64](-7)}, []float64{5, 0, -7})
	})
	t.Run("*int64 to uint16", func(t *testing.T) {
		convertAll(t, []*int64{ptr[int64](5), nil, ptr[int64](65535)}, []uint16{5, 0, 65535})
	})
	t.Run("*float64 to float32", func(t *testing.T) {
		convertAll(t, []*float64{ptr(1.5), nil, ptr(-0.25)}, []float32{1.5, 0, -0.25})
	})
	t.Run("*string to string", func(t *testing.T) {
		convertAll(t, []*string{ptr("a"), nil, ptr("bc"), ptr("")}, []string{"a", "", "bc", ""})
	})
	t.Run("*bool to bool", func(t *testing.T) {
		convertAll(t, []*bool{ptr(true), nil, ptr(false)}, []bool{true, false, false})
	})
	// A pointer on both sides, with the op differing.
	t.Run("*int64 to *int32", func(t *testing.T) {
		convertAll(t, []*int64{ptr[int64](5), nil, ptr[int64](-7), ptr[int64](0)},
			[]*int32{ptr[int32](5), nil, ptr[int32](-7), ptr[int32](0)})
	})
	t.Run("*float64 to *float32", func(t *testing.T) {
		convertAll(t, []*float64{ptr(1.5), nil, ptr(-0.25)}, []*float32{ptr[float32](1.5), nil, ptr[float32](-0.25)})
	})
	t.Run("*int64 to *int64", func(t *testing.T) {
		convertAll(t, []*int64{ptr[int64](5), nil, ptr[int64](-7)}, []*int64{ptr[int64](5), nil, ptr[int64](-7)})
	})
	// A value in the section, a pointer in the Go type.
	t.Run("int64 to *int32", func(t *testing.T) {
		convertAll(t, []int64{5, -3, 9}, []*int32{ptr[int32](5), ptr[int32](-3), ptr[int32](9)})
	})
	t.Run("string to *string", func(t *testing.T) {
		convertAll(t, []string{"a", "bc", "d"}, []*string{ptr("a"), ptr("bc"), ptr("d")})
	})
	t.Run("float64 to *float32", func(t *testing.T) {
		convertAll(t, []float64{1.5, -0.25}, []*float32{ptr[float32](1.5), ptr[float32](-0.25)})
	})
	t.Run("*int64 past an int16", func(t *testing.T) {
		refuseAll[*int64, int16](t, []*int64{ptr[int64](70000)}, "70000", "int16")
	})
	t.Run("*int64 past a *int16", func(t *testing.T) {
		refuseAll[*int64, *int16](t, []*int64{ptr[int64](70000)}, "70000", "int16")
	})
}

// A field the section says is one thing and the Go type another, in a way no
// conversion covers.
func TestForeignRefusesWhatDoesNotConvert(t *testing.T) {
	t.Run("string to int", func(t *testing.T) {
		refuseAll[string, int32](t, []string{"x"}, `"v"`, "string", "int32")
	})
	t.Run("int to string", func(t *testing.T) {
		refuseAll[int64, string](t, []int64{1}, "int64", "string")
	})
	t.Run("bool to int", func(t *testing.T) {
		refuseAll[bool, int32](t, []bool{true}, "bool", "int32")
	})
	t.Run("int to bool", func(t *testing.T) {
		refuseAll[int64, bool](t, []int64{1}, "int64", "bool")
	})
	t.Run("string to bytes", func(t *testing.T) {
		refuseAll[string, []byte](t, []string{"x"}, "string", "[]uint8")
	})
	t.Run("int to array", func(t *testing.T) {
		refuseAll[int64, []int32](t, []int64{1}, "int64", "[]int32")
	})
	t.Run("array to int", func(t *testing.T) {
		refuseAll[[]int64, int32](t, [][]int64{{1}}, "[]int64", "int32")
	})
	t.Run("strings to int32s", func(t *testing.T) {
		refuseAll[[]string, []int32](t, [][]string{{"a"}}, "[]string", "[]int32")
	})
	t.Run("struct to int", func(t *testing.T) {
		refuseAll[narrowOne[int64], int32](t, []narrowOne[int64]{{1}}, "struct", "int32")
	})
	t.Run("int to struct", func(t *testing.T) {
		refuseAll[int64, narrowOne[int64]](t, []int64{1}, "int64", "narrowOne")
	})
	t.Run("struct to list of structs", func(t *testing.T) {
		refuseAll[narrowOne[int64], []narrowOne[int64]](t, []narrowOne[int64]{{1}}, "struct")
	})
}

type foreignInner struct {
	Count int64  `json:"count"`
	Label string `json:"label"`
}

type foreignInnerNarrow struct {
	Count int16  `cb:"4" json:"count"`
	Label string `cb:"2" json:"label"`
}

type foreignOuterFrom struct {
	Name  string              `json:"name"`
	Inner foreignInner        `json:"inner"`
	Many  []foreignInner      `json:"many"`
	Ptr   *foreignInner       `json:"ptr"`
	Extra string              `json:"extra"`
	Value int64               `json:"value"`
	Tail  []foreignInner      `json:"tail"`
	Keep  map[string]int64    `json:"keep"`
	Maps  map[string][]string `json:"maps"`
}

type foreignOuterTo struct {
	Value int32                `cb:"3" json:"value"`
	Name  string               `cb:"1" json:"name"`
	Inner foreignInnerNarrow   `cb:"2" json:"inner"`
	Many  []foreignInnerNarrow `cb:"5" json:"many"`
	Ptr   *foreignInnerNarrow  `cb:"6" json:"ptr"`
	Tail  []foreignInnerNarrow `cb:"7" json:"tail"`
	Keep  map[string]int64     `cb:"8" json:"keep"`
	// Absent from the message.
	Absent string `cb:"9" json:"absent"`
}

func TestForeignNestedStructs(t *testing.T) {
	many := func(n int) []foreignInner {
		out := make([]foreignInner, n)
		for i := range out {
			out[i] = foreignInner{Count: int64(i), Label: "l" + itoa(i)}
		}
		return out
	}
	for _, rows := range []int{0, 2, 700} {
		t.Run(itoa(rows), func(t *testing.T) {
			from := foreignOuterFrom{
				Name: "outer", Inner: foreignInner{Count: 7, Label: "in"}, Many: many(rows),
				Ptr: &foreignInner{Count: 9, Label: "p"}, Extra: "ignored", Value: 123456,
				Tail: many(rows / 2), Keep: map[string]int64{"a": 1, "b": -2},
				Maps: map[string][]string{"x": {"y"}},
			}
			data, err := MarshalSelfDescribing(from)
			if err != nil {
				t.Fatal(err)
			}
			var got foreignOuterTo
			if err := Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			want := foreignOuterTo{
				Value: 123456, Name: "outer", Inner: foreignInnerNarrow{Count: 7, Label: "in"},
				Ptr: &foreignInnerNarrow{Count: 9, Label: "p"}, Keep: map[string]int64{"a": 1, "b": -2},
			}
			for _, inner := range many(rows) {
				want.Many = append(want.Many, foreignInnerNarrow{Count: int16(inner.Count), Label: inner.Label})
			}
			for _, inner := range many(rows / 2) {
				want.Tail = append(want.Tail, foreignInnerNarrow{Count: int16(inner.Count), Label: inner.Label})
			}
			if rows == 0 {
				want.Many, want.Tail = nil, nil
				if got.Many == nil {
					got.Many = nil
				}
				if got.Tail == nil {
					got.Tail = nil
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %+v\nwant %+v", got, want)
			}
		})
	}
}

func TestForeignNestedOverflowNamesTheNestedPath(t *testing.T) {
	from := foreignOuterFrom{Name: "o", Inner: foreignInner{Count: 70000}}
	data, err := MarshalSelfDescribing(from)
	if err != nil {
		t.Fatal(err)
	}
	err = Unmarshal(data, new(foreignOuterTo))
	if err == nil || !strings.Contains(err.Error(), "foreignInnerNarrow.Count") {
		t.Fatalf("error %v does not name foreignOuterTo.Inner.Count", err)
	}
	// And inside a list, where the rows go through a table.
	from = foreignOuterFrom{Name: "o", Many: make([]foreignInner, 40)}
	from.Many[30].Count = 70000
	data, err = MarshalSelfDescribing(from)
	if err != nil {
		t.Fatal(err)
	}
	err = Unmarshal(data, new(foreignOuterTo))
	if err == nil || !strings.Contains(err.Error(), "foreignInnerNarrow.Count") ||
		!strings.Contains(err.Error(), "row 30") {
		t.Fatalf("error %v does not name foreignInnerNarrow.Count at row 30", err)
	}
}

func TestForeignNoFieldInCommon(t *testing.T) {
	data, err := MarshalSelfDescribing(foreignInner{Count: 1, Label: "x"})
	if err != nil {
		t.Fatal(err)
	}
	var wrong struct {
		Other int32 `json:"other"`
		More  int32 `json:"more"`
	}
	err = Unmarshal(data, &wrong)
	if err == nil || !strings.Contains(err.Error(), "count") || !strings.Contains(err.Error(), "matches") {
		t.Fatalf("error %v", err)
	}
}

func TestForeignEnvelopeMismatch(t *testing.T) {
	list, err := MarshalSelfDescribing([]foreignInner{{Count: 1}})
	if err != nil {
		t.Fatal(err)
	}
	var one foreignInner
	if err := Unmarshal(list, &one); err == nil || !strings.Contains(err.Error(), "slice or a map") {
		t.Fatalf("a list decoded into a struct: %v", err)
	}
	object, err := MarshalSelfDescribing(foreignInner{Count: 1})
	if err != nil {
		t.Fatal(err)
	}
	var many []foreignInnerNarrow
	if err := Unmarshal(object, &many); err == nil || !strings.Contains(err.Error(), "struct") {
		t.Fatalf("a struct decoded into a list: %v", err)
	}
}

// Names are matched the way the section writer names a field: the json tag first,
// then the cb tag's name, then the Go name.
func TestForeignMatchesByName(t *testing.T) {
	type from struct {
		A int64  `json:"alpha"`
		B string `cb:"beta,5"`
		C int64
	}
	type to struct {
		Z int32  `cb:"1" json:"alpha"`
		Y string `cb:"2,beta"`
		X int16  `cb:"3" json:"C"`
	}
	data, err := MarshalSelfDescribing(from{A: 5, B: "b", C: 9})
	if err != nil {
		t.Fatal(err)
	}
	var got to
	if err := Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got != (to{Z: 5, Y: "b", X: 9}) {
		t.Fatalf("got %+v", got)
	}
}

// Decoding into a value that already holds something clears it: a field the
// message omits is zero, foreign or not.
func TestForeignClearsTheDestination(t *testing.T) {
	data, err := MarshalSelfDescribing(narrowOne[int64]{V: 0})
	if err != nil {
		t.Fatal(err)
	}
	got := wideOne[int32]{V: 99}
	if err := Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.V != 0 {
		t.Fatalf("got %d", got.V)
	}
}

func TestForeignThroughACodec(t *testing.T) {
	data, err := MarshalSelfDescribing([]narrowOne[int64]{{1}, {2}, {3}})
	if err != nil {
		t.Fatal(err)
	}
	codec := MustCodec[[]wideOne[int32]]()
	var got []wideOne[int32]
	if err := codec.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].V != 1 || got[2].V != 3 {
		t.Fatalf("got %+v", got)
	}
}

// The Go type's own section is not foreign, and a plain Go-to-Go message still
// takes the path it always did.
func TestForeignLeavesOwnSectionAlone(t *testing.T) {
	data, err := MarshalSelfDescribing([]foreignProduct{wantForeignProduct(1), wantForeignProduct(2)})
	if err != nil {
		t.Fatal(err)
	}
	foreignCache.reset()
	var got []foreignProduct
	if err := Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if entries, _ := foreignCache.size(); entries != 0 {
		t.Fatalf("the type's own section built %d translations", entries)
	}
	if len(got) != 2 || got[1].ID != 3 {
		t.Fatalf("got %+v", got)
	}
}
