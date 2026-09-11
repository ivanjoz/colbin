package main

// The Rust encoder, verified by the Go decoder.
//
// `cargo run --example emit_vectors` re-encodes every compact case in both
// corpora with the Rust encoder and writes rust_encoded.json. This test replays
// those bytes through the real Go reader against the same schema the corpus
// records, and compares the values to the ones the corpus says are in there.
//
// It is the other half of the arrangement that pins the port, and the two halves
// fail differently. vectors.rs catches Rust misreading what Go wrote. Byte
// equality in tests/encode.rs catches Rust writing different bytes from Go --
// but only for the cases where the two agree exactly, and a string that packs
// smaller than raw is deliberately not one of them. Those messages are precisely
// the ones nothing else ever hands to Go, and a raw packed5 frame landing at an
// odd bit offset is exactly the sort of thing that would survive every other
// test here.

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/ivanjoz/colbin/compact"
)

func TestGoReadsWhatRustWrote(t *testing.T) {
	var rust struct {
		Messages map[string]string `json:"messages"`
	}
	readJSON(t, "rust_encoded.json", &rust)
	if len(rust.Messages) == 0 {
		t.Fatal("rust_encoded.json holds no messages; run: cargo run --example emit_vectors")
	}

	// Both corpora, so the expected values come from the same place the Rust
	// encoder's input did.
	var flat corpus
	readJSON(t, "vectors.json", &flat)
	var composites ccCorpus
	readJSON(t, "composites.json", &composites)

	seen := 0
	divergent := 0
	for _, c := range flat.Cases {
		encoded, ok := rust.Messages[c.Name]
		if !ok {
			continue // a standard-mode case, which the Rust encoder does not write
		}
		seen++
		if !c.RustByteExact {
			divergent++
		}
		verifyFlat(t, c, decodeBase64(t, encoded))
	}
	for _, c := range composites.Cases {
		encoded, ok := rust.Messages[c.Name]
		if !ok {
			t.Errorf("%s: the Rust encoder wrote no message for a compact case", c.Name)
			continue
		}
		seen++
		if !c.RustByteExact {
			divergent++
		}
		verifyComposite(t, c, decodeBase64(t, encoded))
	}

	if seen != len(rust.Messages) {
		t.Errorf("verified %d of %d Rust messages", seen, len(rust.Messages))
	}
	// The point of this test is the cases byte equality cannot cover. If the
	// corpus ever stopped having any, it would still pass and prove much less.
	if divergent == 0 {
		t.Error("no string-divergent case was verified; this test's reason for existing is gone")
	}
	t.Logf("Go read %d Rust-written messages, %d of them string-divergent", seen, divergent)
}

// verifyFlat walks the message with the Go compact reader driven by the case's
// flat schema, which is what readCompact does for the generator.
func verifyFlat(t *testing.T, c caseOut, data []byte) {
	t.Helper()
	if !compact.IsCompact(data) {
		t.Errorf("%s: Rust wrote a non-compact message", c.Name)
		return
	}
	r, err := compact.NewReader(data)
	if err != nil {
		t.Errorf("%s: %v", c.Name, err)
		return
	}
	// The header facts the Go writer recorded must survive: these are decisions
	// the Rust encoder makes independently and they have to land the same way.
	if got := int(r.Shape()); got != c.Shape {
		t.Errorf("%s: shape %d, want %d", c.Name, got, c.Shape)
	}
	if r.AllPositive() != c.AllPositive {
		t.Errorf("%s: ALL_POSITIVE %v, want %v", c.Name, r.AllPositive(), c.AllPositive)
	}
	if narrow := r.Keys() == compact.Keys4; narrow != c.NarrowKeys {
		t.Errorf("%s: narrow keys %v, want %v", c.Name, narrow, c.NarrowKeys)
	}

	byID := map[int]fieldOut{}
	for _, f := range c.Fields {
		byID[f.ID] = f
	}
	for index, want := range c.Records {
		got := []fieldValue{}
		for {
			key := r.Key()
			if err := r.Err(); err != nil {
				t.Errorf("%s record %d: %v", c.Name, index, err)
				return
			}
			if key == compact.TerminatorKey {
				break
			}
			f, ok := byID[int(key)]
			if !ok {
				t.Errorf("%s record %d: Rust named unknown field id %d", c.Name, index, key)
				return
			}
			got = append(got, fieldValue{ID: f.ID, V: readCompact(r, f.Kind)})
		}
		compareFields(t, c.Name, index, got, want)
	}
	if err := r.Err(); err != nil {
		t.Errorf("%s: %v", c.Name, err)
	}
}

func verifyComposite(t *testing.T, c ccCaseOut, data []byte) {
	t.Helper()
	if !compact.IsCompact(data) {
		t.Errorf("%s: Rust wrote a non-compact message", c.Name)
		return
	}
	r, err := compact.NewReader(data)
	if err != nil {
		t.Errorf("%s: %v", c.Name, err)
		return
	}
	if got := int(r.Shape()); got != c.Shape {
		t.Errorf("%s: shape %d, want %d", c.Name, got, c.Shape)
	}
	if r.AllPositive() != c.AllPositive {
		t.Errorf("%s: ALL_POSITIVE %v, want %v", c.Name, r.AllPositive(), c.AllPositive)
	}
	if narrow := r.Keys() == compact.Keys4; narrow != c.NarrowKeys {
		t.Errorf("%s: narrow keys %v, want %v", c.Name, narrow, c.NarrowKeys)
	}
	for index, want := range c.Records {
		got := readCompositeRecord(c.Name, r, c.Fields)
		compareFields(t, c.Name, index, got, want)
	}
	if err := r.Err(); err != nil {
		t.Errorf("%s: %v", c.Name, err)
	}
}

// compareFields compares through JSON, because the corpus's expected values are
// what json.Unmarshal produced from the file while the read-back values are the
// generator's own shapes -- decimal strings, hex float bits and nested slices,
// which agree once both go through the same encoder.
func compareFields(t *testing.T, name string, index int, got, want any) {
	t.Helper()
	if a, b := toJSON(t, got), toJSON(t, want); !reflect.DeepEqual(a, b) {
		t.Errorf("%s record %d:\n got %s\nwant %s", name, index, a, b)
	}
}

func toJSON(t *testing.T, v any) string {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func readJSON(t *testing.T, name string, dst any) {
	t.Helper()
	body, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("%s: %v (run: go run ./rust/vectors)", name, err)
	}
	if err := json.Unmarshal(body, dst); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

func decodeBase64(t *testing.T, text string) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
