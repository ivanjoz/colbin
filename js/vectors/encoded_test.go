package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/ivanjoz/colbin"
)

// The other direction.
//
// vectors.json goes Go → module: Go writes the bytes and the browser module must
// read them. This goes module → Go, which is the direction a caller actually
// has — a JavaScript client encodes and a Go service decodes — and no
// byte-for-byte corpus can test it, because the module's encoder is the thing
// under test rather than the thing being copied. Byte equality only covers the
// cases where two encoders agree exactly, and the interesting ones are where
// they need not.
//
// web_encoded.json is written by js/tests/emit.mjs, which runs the documents in
// js/tests/documents.mjs through the wasm module built from rust/wasm:
//
//	cd js && bun run wasm && bun run emit
//	go test ./js/vectors

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
// renders with colbin.ToJSON, and the values match what the module itself
// renders the message as.
func TestGoReadsWhatTheModuleWrites(t *testing.T) {
	body, err := os.ReadFile("web_encoded.json")
	if err != nil {
		t.Skipf("no module output to check: %v (run `cd js && bun run wasm && bun run emit`)", err)
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

			// Compared as parsed values rather than as text, because the two
			// decoders may order an object's keys differently: Go writes the
			// fields the message carried first, then the ones it omitted.
			// Nothing else is compensated for.
			fromGo, err := decodeJSON(text)
			if err != nil {
				t.Fatalf("Go produced invalid JSON: %v", err)
			}
			fromModule, err := decodeJSON([]byte(one.JSON))
			if err != nil {
				t.Fatalf("the module produced invalid JSON: %v", err)
			}
			if !sameValue(fromGo, fromModule) {
				t.Fatalf("the two decoders disagree\n  go:     %s\n  module: %s", text, one.JSON)
			}
		})
	}
}

// decodeJSON parses one JSON document with every number kept as the literal it
// was written as, rather than as the float64 encoding/json would round it to.
func decodeJSON(text []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(text))
	decoder.UseNumber()
	var out any
	if err := decoder.Decode(&out); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("data after the top-level value")
	}
	return out, nil
}

// sameValue compares two trees from decodeJSON: objects by key regardless of
// order, arrays element by element, numbers by sameNumber, everything else
// exactly.
func sameValue(a, b any) bool {
	switch left := a.(type) {
	case map[string]any:
		right, ok := b.(map[string]any)
		if !ok || len(left) != len(right) {
			return false
		}
		for key, value := range left {
			other, ok := right[key]
			if !ok || !sameValue(value, other) {
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
			if !sameValue(left[index], right[index]) {
				return false
			}
		}
		return true
	case json.Number:
		right, ok := b.(json.Number)
		return ok && sameNumber(left, right)
	default:
		return a == b
	}
}

// sameNumber compares two JSON number literals.
//
// Two integer literals must be the same integer, digit for digit: an int64
// that went through a float64 on either side is a different number, and the
// whole point of this check is to catch it. Two real literals are compared as
// the float64 each denotes, because one value has more than one correct
// spelling (1e21 and 1e+21). An integer beside a real — 1 against 1.0 — is the
// same number only if the real is exactly that integer, so a float rendering of
// a mangled int64 still fails.
func sameNumber(a, b json.Number) bool {
	aInteger, bInteger := isIntegerLiteral(a), isIntegerLiteral(b)
	switch {
	case aInteger && bInteger:
		left, okLeft := new(big.Int).SetString(string(a), 10)
		right, okRight := new(big.Int).SetString(string(b), 10)
		return okLeft && okRight && left.Cmp(right) == 0
	case !aInteger && !bInteger:
		left, errLeft := strconv.ParseFloat(string(a), 64)
		right, errRight := strconv.ParseFloat(string(b), 64)
		return errLeft == nil && errRight == nil && left == right
	default:
		integerText, realText := a, b
		if !aInteger {
			integerText, realText = b, a
		}
		exact, ok := new(big.Int).SetString(string(integerText), 10)
		if !ok {
			return false
		}
		value, err := strconv.ParseFloat(string(realText), 64)
		if err != nil {
			return false
		}
		return new(big.Float).SetInt(exact).Cmp(big.NewFloat(value)) == 0
	}
}

// isIntegerLiteral is the rule rust/ENCODER.md §2 states for the encoder's own
// input: a number with no '.' and no exponent is an integer.
func isIntegerLiteral(number json.Number) bool {
	return !strings.ContainsAny(string(number), ".eE")
}

// TestSameValueKeepsEveryDigit pins the comparator itself: it is the only check
// of the module → Go direction, so a comparator that rounds would let a mangled
// int64 through the whole suite.
func TestSameValueKeepsEveryDigit(t *testing.T) {
	for _, one := range []struct {
		left, right string
		same        bool
	}{
		// float64 cannot tell these apart; the comparator must.
		{`{"id":7295013456321098765}`, `{"id":7295013456321098770}`, false},
		// int64 max against the float64 it rounds to, spelled as an integer and
		// as a real.
		{`9223372036854775807`, `9223372036854775808`, false},
		{`9223372036854775807`, `9.223372036854776e18`, false},
		{`[-9223372036854775808]`, `[-9223372036854775807]`, false},
		{`18446744073709551615`, `18446744073709551614`, false},

		// One value, spelled more than one correct way.
		{`{"id":7295013456321098765}`, `{"id":7295013456321098765}`, true},
		{`1`, `1.0`, true},
		{`1e21`, `1e+21`, true},
		{`0.1`, `0.10000000000000001`, true},
		{`9223372036854775808`, `9.223372036854775808e18`, true},

		// Shapes, and a number is not its own string.
		{`{"a":1,"b":[true,null,"x"]}`, `{"b":[true,null,"x"],"a":1}`, true},
		{`{"a":1}`, `{"a":1,"b":2}`, false},
		{`[1,2]`, `[2,1]`, false},
		{`1`, `"1"`, false},
	} {
		left, err := decodeJSON([]byte(one.left))
		if err != nil {
			t.Fatalf("%s: %v", one.left, err)
		}
		right, err := decodeJSON([]byte(one.right))
		if err != nil {
			t.Fatalf("%s: %v", one.right, err)
		}
		if got := sameValue(left, right); got != one.same {
			t.Errorf("sameValue(%s, %s) = %v, want %v", one.left, one.right, got, one.same)
		}
		if got := sameValue(right, left); got != one.same {
			t.Errorf("sameValue(%s, %s) = %v, want %v", one.right, one.left, got, one.same)
		}
	}
	if _, err := decodeJSON([]byte(`{} {}`)); err == nil {
		t.Error("decodeJSON accepted two top-level values")
	}
}
