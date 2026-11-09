package codec

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The messages under foreign_vectors are what the browser's encoder writes
// (gen.mjs): ids numbered 0, 1, 2 in first-seen order, every integer an int64,
// every fractional column a float64. The Go types below number their fields their
// own way and narrow their integers, which is what a backend does.

func foreignFile(t testing.TB, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("foreign_vectors", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

type foreignSeller struct {
	Name   string `json:"name"`
	Rating int8   `json:"rating"`
}

// foreignProduct is a Go type for the JS products: tagged, narrowed, with a field
// the message does not have (Missing), and without one it does (_updatedFields).
type foreignProduct struct {
	Qty     int16         `cb:"1" json:"qty"`
	Price   float32       `cb:"2" json:"price"`
	ID      int32         `cb:"3" json:"id"`
	Weight  float32       `cb:"4" json:"weight"` // a float column that was all whole numbers
	Tags    []int32       `cb:"5" json:"tags"`
	Name    string        `cb:"6" json:"name"`
	Seller  foreignSeller `cb:"7" json:"seller"`
	Note    string        `cb:"8" json:"note"` // a pointer in the section
	Missing int32         `cb:"9" json:"missing"`
}

// foreignProductWide is the same with a field id past sixteen, so Go's own key
// width is eight bits while the message's is four.
type foreignProductWide struct {
	Qty    int16         `cb:"30" json:"qty"`
	Price  float64       `cb:"2" json:"price"`
	ID     uint32        `cb:"31" json:"id"`
	Weight int64         `cb:"4" json:"weight"`
	Tags   []int64       `cb:"5" json:"tags"`
	Name   string        `json:"name"` // untagged: derived from its name
	Seller foreignSeller `cb:"40" json:"seller"`
	Note   *string       `cb:"41" json:"note"`
}

func wantForeignProduct(i int) foreignProduct {
	product := foreignProduct{
		Qty:    int16(i % 200),
		Price:  float32(float64(i)*0.25 + 0.1),
		ID:     int32(i + 1),
		Weight: float32((i % 40) * 3),
		Tags:   []int32{int32(i), int32(i + 1), -int32(i % 7)},
		Name:   "product " + itoa(i),
		Seller: foreignSeller{Name: "seller " + itoa(i%13), Rating: int8(i % 5)},
	}
	if i%3 != 0 {
		product.Note = "note " + itoa(i)
	}
	return product
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for ; i > 0; i /= 10 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
	}
	return string(digits)
}

// TestForeignProducts is the repro: before the foreign decode, every one of these
// failed with "wire: unassigned or mismatched descriptor".
func TestForeignProducts(t *testing.T) {
	for _, test := range []struct {
		file string
		rows int
	}{
		{"products3.standalone.bin", 3},
		{"products500.standalone.bin", 500},
	} {
		t.Run(test.file, func(t *testing.T) {
			var got []foreignProduct
			if err := Unmarshal(foreignFile(t, test.file), &got); err != nil {
				t.Fatal(err)
			}
			if len(got) != test.rows {
				t.Fatalf("rows = %d, want %d", len(got), test.rows)
			}
			for i, product := range got {
				want := wantForeignProduct(i)
				if product.Qty != want.Qty || product.ID != want.ID || product.Name != want.Name ||
					product.Seller != want.Seller || product.Note != want.Note || product.Missing != 0 ||
					product.Weight != want.Weight || product.Price != want.Price ||
					len(product.Tags) != 3 || product.Tags[0] != want.Tags[0] ||
					product.Tags[1] != want.Tags[1] || product.Tags[2] != want.Tags[2] {
					t.Fatalf("row %d = %+v, want %+v", i, product, want)
				}
			}
		})
	}
}

func TestForeignProductsWide(t *testing.T) {
	var got []foreignProductWide
	if err := Unmarshal(foreignFile(t, "products500.standalone.bin"), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 500 {
		t.Fatalf("rows = %d, want 500", len(got))
	}
	for i, product := range got {
		want := wantForeignProduct(i)
		if product.Qty != want.Qty || product.ID != uint32(want.ID) || product.Name != want.Name ||
			product.Seller != want.Seller || product.Weight != int64(want.Weight) ||
			math.Abs(product.Price-(float64(i)*0.25+0.1)) > 1e-9 || len(product.Tags) != 3 ||
			product.Tags[2] != int64(want.Tags[2]) {
			t.Fatalf("row %d = %+v", i, product)
		}
		if (i%3 == 0) != (product.Note == nil) || (product.Note != nil && *product.Note != want.Note) {
			t.Fatalf("row %d note = %v, want %q", i, product.Note, want.Note)
		}
	}
}

// A bare message has no section to read, so it fails as it always did.
func TestForeignBareMessageStillRefused(t *testing.T) {
	var got []foreignProduct
	if err := Unmarshal(foreignFile(t, "products500.message.bin"), &got); err == nil {
		t.Fatal("a message with no section decoded")
	}
}

func TestForeignOverflowNamesThePath(t *testing.T) {
	var got []foreignProduct
	err := Unmarshal(foreignFile(t, "overflow500.standalone.bin"), &got)
	if err == nil {
		t.Fatal("70000 went into an int16")
	}
	for _, want := range []string{"foreignProduct.Qty", "70000", "int16"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err, want)
		}
	}
}

// reset empties the cache, for a test that counts what a decode puts in it.
func (store *foreignStore) reset() {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.byType = nil
}
