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
