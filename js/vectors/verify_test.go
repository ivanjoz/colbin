package main

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/ivanjoz/colbin"
)

// TestVectorsAreCurrent fails when the committed corpus is not what the Go
// codecs produce today.
//
// The browser module asserts against the committed file, so without this a
// change to the Go wire would leave it passing against a corpus that no longer
// describes anything. Run `go run ./js/vectors` to regenerate, and expect the
// module to fail until it is brought over.
func TestVectorsAreCurrent(t *testing.T) {
	committed, err := os.ReadFile("vectors.json")
	if err != nil {
		t.Fatalf("read the corpus: %v", err)
	}
	current, err := json.MarshalIndent(build(), "", "  ")
	if err != nil {
		t.Fatalf("build the corpus: %v", err)
	}
	if string(committed) != string(current)+"\n" {
		t.Fatal("js/vectors/vectors.json is stale: run `go run ./js/vectors`")
	}
}

// held reads the committed file, so that the tests below check what the module
// will actually be handed rather than what build() has in memory.
func held(t *testing.T) vectors {
	t.Helper()
	body, err := os.ReadFile("vectors.json")
	if err != nil {
		t.Fatalf("read the corpus: %v", err)
	}
	var out vectors
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("parse the corpus: %v", err)
	}
	return out
}

// TestEverySectionStandsAlone parses each recorded section back and renders the
// recorded message through it, which is the exact path the module's decoder
// takes: bytes off a wire, no Go type anywhere.
//
// It is the one test here that would catch a section that only works because
// the process that wrote it still had the type in memory. The packed tier goes
// through it unchanged: a reader needs no setting, because each string's own
// header says how it was written.
func TestEverySectionStandsAlone(t *testing.T) {
	all := held(t)
	for _, tier := range []struct {
		name  string
		cases []typeCase
	}{
		{"types", all.Types},
		{"packed", all.Packed},
	} {
		if len(tier.cases) == 0 {
			t.Fatalf("the %s tier is empty", tier.name)
		}
		for _, one := range tier.cases {
			t.Run(tier.name+"/"+one.Name, func(t *testing.T) {
				message := unhex(t, one.Message)
				if len(message) == 0 {
					t.Fatal("the message is empty")
				}
				if wide := message[0]&0x08 != 0; wide != one.Wide {
					t.Fatalf("wide is %v, the root byte says %v", one.Wide, wide)
				}
				schema, err := colbin.ParseSchema(unhex(t, one.Section))
				if err != nil {
					t.Fatalf("parse the section: %v", err)
				}
				text, err := colbin.ToJSON(schema, message)
				if err != nil {
					t.Fatalf("to json: %v", err)
				}
				if string(text) != one.JSON {
					t.Fatalf("json differs\n have %s\n want %s", text, one.JSON)
				}
				if !json.Valid(text) {
					t.Fatalf("not valid json: %s", text)
				}
				// The self-describing form carries its own section, so it must
				// reach the same text with nothing passed in at all.
				inline, err := colbin.ToJSON(nil, unhex(t, one.SelfDescribing))
				if err != nil {
					t.Fatalf("to json, self-describing: %v", err)
				}
				if string(inline) != one.JSON {
					t.Fatalf("the self-describing form differs\n have %s\n want %s", inline, one.JSON)
				}
			})
		}
	}
}

func unhex(t *testing.T, text string) []byte {
	t.Helper()
	out, err := hex.DecodeString(text)
	if err != nil {
		t.Fatalf("not hex: %v", err)
	}
	return out
}
