package codec

import (
	"encoding/json"
	"testing"
)

// flatProduct is foreignProduct's columns without the nested struct and the
// array, so 500 of them are a table rather than a list of structs.
type flatProduct struct {
	Qty    int16   `cb:"1" json:"qty"`
	Price  float32 `cb:"2" json:"price"`
	ID     int32   `cb:"3" json:"id"`
	Weight float32 `cb:"4" json:"weight"`
	Name   string  `cb:"6" json:"name"`
	Note   string  `cb:"8" json:"note"`
}

func TestForeignFlatTable(t *testing.T) {
	var got []flatProduct
	if err := Unmarshal(foreignFile(t, "flat500.standalone.bin"), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 500 {
		t.Fatalf("%d rows", len(got))
	}
	for i, row := range got {
		want := wantForeignProduct(i)
		if row != (flatProduct{Qty: want.Qty, Price: want.Price, ID: want.ID, Weight: want.Weight, Name: want.Name, Note: "note " + itoa(i)}) {
			t.Fatalf("row %d = %+v", i, row)
		}
	}
}

// The three ways a Go backend can take 500 products from the browser. Foreign is
// the decode through the message's own section; JSON is what was the only
// working path (ToJSON, then encoding/json); native is a message Go wrote for the
// same type, which is what the foreign decode is measured against.

func benchForeign[T any](b *testing.B, file string, goRows func() []T) {
	standalone := foreignFile(b, file)
	b.Run("foreign", func(b *testing.B) {
		b.SetBytes(int64(len(standalone)))
		b.ReportAllocs()
		var rows []T
		for b.Loop() {
			rows = rows[:0]
			if err := Unmarshal(standalone, &rows); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("json", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			text, err := ToJSON(nil, standalone)
			if err != nil {
				b.Fatal(err)
			}
			var rows []T
			if err := json.Unmarshal(text, &rows); err != nil {
				b.Fatal(err)
			}
		}
	})
	native, err := Marshal(goRows())
	if err != nil {
		b.Fatal(err)
	}
	b.Run("native", func(b *testing.B) {
		b.SetBytes(int64(len(native)))
		b.ReportAllocs()
		var rows []T
		for b.Loop() {
			rows = rows[:0]
			if err := Unmarshal(native, &rows); err != nil {
				b.Fatal(err)
			}
		}
	})
	selfDescribing, err := MarshalSelfDescribing(goRows())
	if err != nil {
		b.Fatal(err)
	}
	b.Run("native-self-describing", func(b *testing.B) {
		b.ReportAllocs()
		var rows []T
		for b.Loop() {
			rows = rows[:0]
			if err := Unmarshal(selfDescribing, &rows); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkForeignProducts500(b *testing.B) {
	benchForeign(b, "products500.standalone.bin", func() []foreignProduct {
		rows := make([]foreignProduct, 500)
		for i := range rows {
			rows[i] = wantForeignProduct(i)
		}
		return rows
	})
}

func BenchmarkForeignFlat500(b *testing.B) {
	benchForeign(b, "flat500.standalone.bin", func() []flatProduct {
		rows := make([]flatProduct, 500)
		for i := range rows {
			want := wantForeignProduct(i)
			rows[i] = flatProduct{Qty: want.Qty, Price: want.Price, ID: want.ID, Weight: want.Weight, Name: want.Name, Note: "note " + itoa(i)}
		}
		return rows
	})
}
