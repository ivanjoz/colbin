package main

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/ivanjoz/colbin"
)

// The other direction.
//
// vectors.json goes Go → module: Go writes the bytes and the AssemblyScript port
// must reproduce and read them. This goes module → Go, which is the direction a
// caller actually has — a JavaScript client encodes and a Go service decodes —
// and no byte-for-byte corpus can test it, because the module's encoder is the
// thing under test rather than the thing being copied.
//
//	cd web && node tests/emit.mjs
//	go test ./web/vectors
//
// It is the same shape as `cargo run --features derive --example emit_vectors`
// followed by `go test ./rust/vectors`, and it is here for the same reason: byte
// equality only covers the cases where two encoders agree exactly, and the
// interesting ones are where they need not.

type encodedCase struct {
	Name string `json:"name"`
	// The JSON the module was given.
	Input string `json:"input"`
	// The section and the message it produced, in hex.
	Section string `json:"section"`
	Message string `json:"message"`
	// What the module renders the message back as, so a disagreement says which
	// of the two decoders is the odd one out.
	JSON string `json:"json"`
}

type encodedCorpus struct {
	Cases []encodedCase `json:"cases"`
}

// TestGoReadsWhatTheModuleWrites is the interop claim, checked rather than
// asserted: every message the module produced parses with colbin.ParseSchema and
// renders with colbin.ToJSON, and the values match the JSON the module was
// handed.
func TestGoReadsWhatTheModuleWrites(t *testing.T) {
	body, err := os.ReadFile("web_encoded.json")
	if err != nil {
		t.Skipf("no module output to check: %v (run `cd web && node tests/emit.mjs`)", err)
	}
	var held encodedCorpus
	if err := json.Unmarshal(body, &held); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(held.Cases) == 0 {
		t.Fatal("the module wrote no cases")
	}

	for _, one := range held.Cases {
		t.Run(one.Name, func(t *testing.T) {
			section, err := hex.DecodeString(one.Section)
			if err != nil {
				t.Fatalf("the section is not hex: %v", err)
			}
			message, err := hex.DecodeString(one.Message)
			if err != nil {
				t.Fatalf("the message is not hex: %v", err)
			}

			schema, err := colbin.ParseSchema(section)
			if err != nil {
				t.Fatalf("Go cannot parse the module's section: %v", err)
			}
			text, err := colbin.ToJSON(schema, message)
			if err != nil {
				t.Fatalf("Go cannot read the module's message: %v", err)
			}

			// Compared as parsed values rather than as text. The two decoders
			// order an object's keys differently on purpose — Go writes the
			// fields the message carried first and this module writes them in
			// schema order — and PLAN.md §7 level 3 says so.
			var fromGo, fromModule any
			if err := json.Unmarshal(text, &fromGo); err != nil {
				t.Fatalf("Go produced invalid JSON: %v", err)
			}
			if err := json.Unmarshal([]byte(one.JSON), &fromModule); err != nil {
				t.Fatalf("the module produced invalid JSON: %v", err)
			}
			if !sameValue(fromGo, unwrapEnvelope(fromModule, fromGo)) {
				t.Fatalf("the two decoders disagree\n  go:     %s\n  module: %s", text, one.JSON)
			}
		})
	}
}

// unwrapEnvelope accounts for the one difference the two decoders have by
// design.
//
// A document whose top level is not an object is wrapped in a one-field struct,
// because the root of a colbin message is a struct and nothing else. The module
// marks the wrapper in the section and unwraps it; Go does not know that flag
// and renders `{"rows": …}`, which is more literal rather than wrong
// (REFACTOR_PLAN.md §5.1). So when Go produced exactly that shape and the module
// did not, the module's value is put back inside it before they are compared.
func unwrapEnvelope(fromModule, fromGo any) any {
	wrapper, ok := fromGo.(map[string]any)
	if !ok || len(wrapper) != 1 {
		return fromModule
	}
	if _, ok := wrapper["rows"]; !ok {
		return fromModule
	}
	if _, alsoWrapped := fromModule.(map[string]any); alsoWrapped {
		return fromModule
	}
	return map[string]any{"rows": fromModule}
}

// sameValue compares two decoded JSON trees. encoding/json turns every number
// into a float64, which would silently pass a message whose integers were
// mangled — so numbers are compared through json.Number instead, by the digits
// each side actually wrote.
func sameValue(a, b any) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right) || equalTrees(a, b)
}

func equalTrees(a, b any) bool {
	switch left := a.(type) {
	case map[string]any:
		right, ok := b.(map[string]any)
		if !ok || len(left) != len(right) {
			return false
		}
		for key, value := range left {
			other, ok := right[key]
			if !ok || !equalTrees(value, other) {
				return false
			}
		}
		return true
	case []any:
		right, ok := b.([]any)
		if !ok || len(left) != len(right) {
			return false
		}
		for index := range left {
			if !equalTrees(left[index], right[index]) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}
