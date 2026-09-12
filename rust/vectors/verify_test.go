package main

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/ivanjoz/colbin"
)

// TestCorpusIsCurrent fails when the committed corpus is not what the Go
// codecs produce today.
//
// The Rust port asserts against the committed file, so without this a change to
// the Go wire would leave Rust passing against a corpus that no longer describes
// anything — which is exactly how the two ports diverged in silence before. Run
// `go run ./rust/vectors` to regenerate, and expect the Rust side to fail until
// it is brought over.
func TestCorpusIsCurrent(t *testing.T) {
	committed, err := os.ReadFile("vectors.json")
	if err != nil {
		t.Fatalf("read the corpus: %v", err)
	}
	current, err := json.MarshalIndent(build(), "", "  ")
	if err != nil {
		t.Fatalf("build the corpus: %v", err)
	}
	if string(committed) != string(current)+"\n" {
		t.Fatal("rust/vectors/vectors.json is stale: run `go run ./rust/vectors`")
	}
}

// TestEveryCaseRoundTrips checks the corpus against the decoder as well as the
// encoder, so a message no Go reader would accept cannot be handed to Rust as a
// specification. Decoding and re-encoding must also return the same bytes: a
// message that is not a fixed point of its own codec is one the Rust side could
// only match by accident.
func TestEveryCaseRoundTrips(t *testing.T) {
	body, err := os.ReadFile("vectors.json")
	if err != nil {
		t.Fatalf("read the corpus: %v", err)
	}
	var held corpus
	if err := json.Unmarshal(body, &held); err != nil {
		t.Fatalf("parse the corpus: %v", err)
	}
	if len(held.Cases) == 0 {
		t.Fatal("the corpus holds no cases")
	}

	// One fresh destination per corpus type, by the name the generator wrote.
	fresh := map[string]func() any{
		"main.Charge":      func() any { return &Charge{} },
		"main.Scalars":     func() any { return &Scalars{} },
		"main.Arrays":      func() any { return &Arrays{} },
		"main.Optionals":   func() any { return &Optionals{} },
		"main.Line":        func() any { return &Line{} },
		"main.Order":       func() any { return &Order{} },
		"main.Maps":        func() any { return &Maps{} },
		"main.WideEvolved": func() any { return &WideEvolved{} },
		"main.Wide":        func() any { return &Wide{} },
		"main.WideWidths":  func() any { return &WideWidths{} },
		"main.Hashed":      func() any { return &Hashed{} },
		"main.PackedText":  func() any { return &PackedText{} },
	}

	for _, one := range held.Cases {
		t.Run(one.Name, func(t *testing.T) {
			make, ok := fresh[one.Type]
			if !ok {
				t.Fatalf("no destination for %s", one.Type)
			}
			message, err := hex.DecodeString(one.Message)
			if err != nil {
				t.Fatalf("the message is not hex: %v", err)
			}
			into := make()
			if err := colbin.Unmarshal(message, into); err != nil {
				t.Fatalf("decode: %v", err)
			}
			// packed5 is a writer setting, so a message written with it on
			// re-encodes raw unless it is turned back on for the comparison.
			packed := strings.HasPrefix(one.Name, "packed5.")
			colbin.SetPacked5(packed)
			again, err := colbin.Marshal(into)
			colbin.SetPacked5(false)
			if err != nil {
				t.Fatalf("re-encode: %v", err)
			}
			if got := hex.EncodeToString(again); got != one.Message {
				t.Fatalf("re-encoded differently\n have %s\n want %s", got, one.Message)
			}
		})
	}
}
