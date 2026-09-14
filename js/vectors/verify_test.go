package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"strconv"
	"testing"

	"github.com/ivanjoz/colbin"
	"github.com/ivanjoz/colbin/column"
	"github.com/ivanjoz/colbin/wire"
)

// TestVectorsAreCurrent fails when the committed corpus is not what the Go
// codecs produce today.
//
// The browser module asserts against the committed file, so without this a
// change to the Go wire would leave the port passing against a corpus that no
// longer describes anything — which is how the previous port went stale in
// silence. Run `go run ./js/vectors` to regenerate, and expect the module to
// fail until it is brought over.
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

// TestStringFramesFrameTheirPayload checks that a recorded header really is the
// whole of the framing: put the payload back behind it and a reader must hand
// back exactly the payload.
//
// The tier records the header alone, so that a 64 KB case costs two fields
// rather than 128 KB of hex. That is only sound if the header and the payload
// compose, which is what this asserts — and it is the tier the move of the
// packed encoding into a per-field descriptor code went through, so it should
// be the one that is checked hardest.
func TestStringFramesFrameTheirPayload(t *testing.T) {
	for _, one := range held(t).Strings {
		t.Run(one.Name, func(t *testing.T) {
			fill, err := hex.DecodeString(one.Fill)
			if err != nil {
				t.Fatalf("the fill is not hex: %v", err)
			}
			payload := repeatBytes(fill, one.Size)

			// An empty blob is not written at all — omission is unconditional
			// and there is no flag for it — so both frames are empty and there
			// is nothing to read back.
			if one.Size == 0 {
				if one.Narrow != "" || one.Wide != "" {
					t.Fatalf("an empty blob wrote %q / %q", one.Narrow, one.Wide)
				}
				return
			}

			if one.Narrow != "" {
				frame := append(unhex(t, one.Narrow), payload...)
				reader := wire.NewReader(frame)
				if !reader.More() {
					t.Fatal("the narrow frame holds no field")
				}
				if got := int(reader.Key()); got != one.Key {
					t.Fatalf("narrow key %d, want %d", got, one.Key)
				}
				if got := reader.Bytes(); !bytes.Equal(got, payload) {
					t.Fatalf("narrow payload of %d bytes, want %d", len(got), len(payload))
				}
				if err := reader.Err(); err != nil {
					t.Fatalf("narrow: %v", err)
				}
			} else if one.Key < wire.MaxFields {
				t.Fatalf("key %d fits four bits but has no narrow frame", one.Key)
			}

			frame := append(unhex(t, one.Wide), payload...)
			reader := wire.NewReader8(frame)
			if !reader.More() {
				t.Fatal("the wide frame holds no field")
			}
			if got := int(reader.Key()); got != one.Key {
				t.Fatalf("wide key %d, want %d", got, one.Key)
			}
			if got := reader.Bytes(); !bytes.Equal(got, payload) {
				t.Fatalf("wide payload of %d bytes, want %d", len(got), len(payload))
			}
			if err := reader.Err(); err != nil {
				t.Fatalf("wide: %v", err)
			}
		})
	}
}

// TestEveryColumnDecodes reads each recorded column back at the width the case
// declares. A frame the Go codec cannot read is not a specification for anyone.
func TestEveryColumnDecodes(t *testing.T) {
	for _, one := range held(t).Columns {
		t.Run(one.Name, func(t *testing.T) {
			encoded := unhex(t, one.Encoded)
			count := len(one.Values)
			want := make([]int64, count)
			for index, text := range one.Values {
				value, err := strconv.ParseInt(text, 10, 64)
				if err != nil {
					t.Fatalf("value %d is not an int64: %v", index, err)
				}
				want[index] = value
			}
			got := make([]int64, count)
			switch one.Width {
			case 1:
				out := make([]int8, count)
				decodeInto(t, encoded, count, out)
				widen(got, out)
			case 2:
				out := make([]int16, count)
				decodeInto(t, encoded, count, out)
				widen(got, out)
			case 4:
				out := make([]int32, count)
				decodeInto(t, encoded, count, out)
				widen(got, out)
			default:
				decodeInto(t, encoded, count, got)
			}
			for index := range got {
				if got[index] != want[index] {
					t.Fatalf("value %d is %d, want %d", index, got[index], want[index])
				}
			}
		})
	}
}

// TestEverySectionStandsAlone parses each recorded section back and renders the
// recorded message through it, which is the exact path the module's decoder
// takes: bytes off a wire, no Go type anywhere.
//
// It is the one test here that would catch a section that only works because
// the process that wrote it still had the type in memory.
func TestEverySectionStandsAlone(t *testing.T) {
	for _, one := range held(t).Types {
		t.Run(one.Name, func(t *testing.T) {
			schema, err := colbin.ParseSchema(unhex(t, one.Section))
			if err != nil {
				t.Fatalf("parse the section: %v", err)
			}
			text, err := colbin.ToJSON(schema, unhex(t, one.Message))
			if err != nil {
				t.Fatalf("to json: %v", err)
			}
			if string(text) != one.JSON {
				t.Fatalf("json differs\n have %s\n want %s", text, one.JSON)
			}
			if !json.Valid(text) {
				t.Fatalf("not valid json: %s", text)
			}
			// The self-describing form carries its own section, so it must reach
			// the same text with nothing passed in at all.
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

func unhex(t *testing.T, text string) []byte {
	t.Helper()
	out, err := hex.DecodeString(text)
	if err != nil {
		t.Fatalf("not hex: %v", err)
	}
	return out
}

func decodeInto[T column.Signed](t *testing.T, buf []byte, count int, out []T) {
	t.Helper()
	if _, err := column.DecodeArray(buf, count, out); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

func widen[T column.Signed](dst []int64, src []T) {
	for index, value := range src {
		dst[index] = int64(value)
	}
}
