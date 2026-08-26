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
