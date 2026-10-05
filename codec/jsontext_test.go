package codec

import (
	"encoding/json"
	"testing"
	"unicode/utf8"
)

// A string is escaped as encoding/json escapes it, byte for byte: every ASCII
// byte and the two JavaScript line breaks. The JSON is meant to be
// interchangeable with what a Go service would have sent.
//
// The one difference is a byte that is not UTF-8: both replace it with U+FFFD,
// which encoding/json writes as the rune and this writes as its escape, so the
// output stays ASCII. They decode to the same string.
func TestStringsAreEscapedAsEncodingJSONEscapesThem(t *testing.T) {
	values := []string{" ", " ", "\xff", "a\xffb", "ñ", "日本"}
	for character := range 128 {
		values = append(values, string(rune(character)), "x"+string(rune(character))+"y")
	}
	for _, value := range values {
		want, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		got := appendJSONString(nil, value)
		if utf8.ValidString(value) {
			if string(got) != string(want) {
				t.Errorf("%q escaped as %s, encoding/json writes %s", value, got, want)
			}
			continue
		}
		var gotValue, wantValue string
		if json.Unmarshal(got, &gotValue) != nil || json.Unmarshal(want, &wantValue) != nil ||
			gotValue != wantValue {
			t.Errorf("%q escaped as %s, which reads back as %q, not %q", value, got, gotValue, wantValue)
		}
	}
}
