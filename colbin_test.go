package colbin_test

import (
	"reflect"
	"testing"

	"github.com/ivanjoz/colbin"
)

func TestPublicFacadeRoundTrip(t *testing.T) {
	type row struct {
		ID   int64
		Name string
	}
	in := []row{{ID: 1, Name: "uno"}, {ID: 2, Name: "dos"}}

	data, err := colbin.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out []row
	if err := colbin.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("got %#v want %#v", out, in)
	}
}

func TestPublicFacadeJSONMode(t *testing.T) {
	type row struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		Tags []string
	}
	in := []row{{ID: 1, Name: "uno", Tags: []string{"a"}}, {ID: 2, Name: "dos"}}

	data, err := colbin.MarshalJSON(in)
	if err != nil {
		t.Fatal(err)
	}
	text, err := colbin.DecodeJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	const want = `[{"Tags":["a"],"id":1,"name":"uno"},{"Tags":null,"id":2,"name":"dos"}]`
	if string(text) != want {
		t.Fatalf("got  %s\nwant %s", text, want)
	}
	// The typed decoder still reads the same message.
	var out []row
	if err := colbin.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("got %#v want %#v", out, in)
	}
	// And the untyped one needs no Go type at all.
	v, err := colbin.DecodeAny(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.([]any)[0].(map[string]any)["name"]; got != "uno" {
		t.Fatalf("name = %v, want uno", got)
	}
}

// A struct numbered `cb:"1"`.. and driven through a cached Codec: the shape this
// is built for, one record per message and many of them.
type stats struct {
	Quantity                int32 `cb:"1,quantity"`
	QuantityPendingDelivery int32 `cb:"2,quantityPendingDelivery"`
	SubQuantity             int16 `cb:"3,subQuantity"`
	TotalAmount             int32 `cb:"4,totalAmount"`
}

var statsCodec = colbin.MustCodec[stats]()

func TestPublicFacadeCodec(t *testing.T) {
	records := []stats{
		{Quantity: 480, QuantityPendingDelivery: 120, SubQuantity: 12, TotalAmount: 145900},
		{Quantity: 1},
		{},
	}

	buf := make([]byte, 0, 64)
	for i, rec := range records {
		var err error
		if buf, err = statsCodec.Append(buf[:0], &rec); err != nil {
			t.Fatal(err)
		}

		var out stats
		if err := statsCodec.Unmarshal(buf, &out); err != nil {
			t.Fatal(err)
		}
		if out != rec {
			t.Fatalf("record %d: got %#v, want %#v", i, out, rec)
		}
		// The same bytes the package functions would have written and read.
		want, err := colbin.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		if string(buf) != string(want) {
			t.Fatalf("record %d: codec wrote %x, Marshal wrote %x", i, buf, want)
		}
		out = stats{}
		if err := colbin.Unmarshal(buf, &out); err != nil {
			t.Fatal(err)
		}
		if out != rec {
			t.Fatalf("record %d via Unmarshal: got %#v, want %#v", i, out, rec)
		}
	}

	if _, err := colbin.NewCodec[[]stats](); err == nil {
		t.Error("NewCodec on a non-struct returned no error")
	}
}
