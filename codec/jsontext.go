package codec

// The sink that writes JSON text.
//
// It matches `encoding/json` byte for byte on everything colbin can carry, which
// is not vanity: the tests compare the two directly, and a decoder that produced
// merely *equivalent* JSON would need a JSON parser in every one of them to say
// so. Matching means escaping the three HTML-significant bytes as encoding/json
// does by default, spelling numbers the way it does, and replacing invalid UTF-8
// with the replacement character.
//
// The one place the two disagree is a NaN or an infinity, which encoding/json
// refuses outright. So does this — see AppendJSON.

import (
	"encoding/base64"
	"fmt"
	"math"
	"strconv"
	"unicode/utf8"
)

// jsonSink appends JSON onto a buffer the caller owns.
type jsonSink struct {
	buffer []byte
	// stack is one entry per open object or array. `first` is what decides the
	// comma, and it has to be per level rather than one flag, because closing a
	// nested value leaves its parent mid-list.
	stack []jsonLevel
}

type jsonLevel struct {
	object bool
	first  bool
}

func (s *jsonSink) push(object bool) {
	s.stack = append(s.stack, jsonLevel{object: object, first: true})
}

func (s *jsonSink) pop() {
	if len(s.stack) > 0 {
		s.stack = s.stack[:len(s.stack)-1]
	}
}

// beforeValue writes the comma an array element needs. An object member needs
// none: its key wrote it.
func (s *jsonSink) beforeValue() {
	top := len(s.stack) - 1
	if top < 0 || s.stack[top].object {
		return
	}
	if !s.stack[top].first {
		s.buffer = append(s.buffer, ',')
	}
	s.stack[top].first = false
}

func (s *jsonSink) beginObject() {
	s.beforeValue()
	s.buffer = append(s.buffer, '{')
	s.push(true)
}

func (s *jsonSink) endObject() {
	s.buffer = append(s.buffer, '}')
	s.pop()
}

func (s *jsonSink) beginArray() {
	s.beforeValue()
	s.buffer = append(s.buffer, '[')
	s.push(false)
}

func (s *jsonSink) endArray() {
	s.buffer = append(s.buffer, ']')
	s.pop()
}

func (s *jsonSink) key(name string) {
	s.beforeKey()
	s.buffer = append(appendJSONString(s.buffer, name), ':')
}

// keyBytes is key without the string conversion, through the same elision the
// compiler gives textBytes: Go recognises string(b) in a call argument.
func (s *jsonSink) keyBytes(name []byte) {
	s.beforeKey()
	s.buffer = append(appendJSONString(s.buffer, string(name)), ':')
}

func (s *jsonSink) beforeKey() {
	if top := len(s.stack) - 1; top >= 0 {
		if !s.stack[top].first {
			s.buffer = append(s.buffer, ',')
		}
		s.stack[top].first = false
	}
}

func (s *jsonSink) null() {
	s.beforeValue()
	s.buffer = append(s.buffer, "null"...)
}

func (s *jsonSink) boolean(value bool) {
	s.beforeValue()
	if value {
		s.buffer = append(s.buffer, "true"...)
		return
	}
	s.buffer = append(s.buffer, "false"...)
}

func (s *jsonSink) signed(value int64) {
	s.beforeValue()
	s.buffer = strconv.AppendInt(s.buffer, value, 10)
}

func (s *jsonSink) unsigned(value uint64) {
	s.beforeValue()
	s.buffer = strconv.AppendUint(s.buffer, value, 10)
}

// float refuses before it writes, so a message with a NaN three fields in leaves
// the caller's buffer as it found it rather than half a document.
func (s *jsonSink) float(value float64, width int) error {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return fmt.Errorf(
			"colbin: %v has no JSON spelling; decode with DecodeAny to keep it", value)
	}
	s.beforeValue()
	s.buffer = appendJSONFloat(s.buffer, value, width)
	return nil
}

func (s *jsonSink) text(value string) {
	s.beforeValue()
	s.buffer = appendJSONString(s.buffer, value)
}

// textBytes is text without the copy, for the strings the walk holds as a
// sub-slice of the message. The escaper works a byte at a time either way, so
// the two share it through a string conversion the compiler does not allocate
// for: Go recognises string(b) in a call argument and elides it.
func (s *jsonSink) textBytes(value []byte) {
	s.beforeValue()
	s.buffer = appendJSONString(s.buffer, string(value))
}

// blob writes a []byte as base64 in a string, which is what encoding/json does
// and therefore what a caller on the other end already has a decoder for. It is
// worth saying out loud because colbin's opBytes and opStrings are different ops
// and only this one is base64.
func (s *jsonSink) blob(value []byte) {
	s.beforeValue()
	s.buffer = append(s.buffer, '"')
	s.buffer = base64.StdEncoding.AppendEncode(s.buffer, value)
	s.buffer = append(s.buffer, '"')
}

// appendJSONFloat writes a float the way encoding/json writes one: 'f' notation
// in the range a reader expects to see it, 'e' outside it, and in either case
// the shortest form that reads back as the same value.
//
// A non-finite value must be refused before reaching here. JSON has no spelling
// for one; writing null instead — which is what the last version of this did —
// turns "not a number" into "no value", and the two are not the same thing.
func appendJSONFloat(dst []byte, value float64, width int) []byte {
	// Where 'f' gives way to 'e' is encoding/json's rule, so that the two agree
	// on the point at which a large number stops being written out in full.
	format := byte('f')
	if absolute := math.Abs(value); absolute != 0 &&
		(width == 64 && (absolute < 1e-6 || absolute >= 1e21) ||
			width == 32 && (float32(absolute) < 1e-6 || float32(absolute) >= 1e21)) {
		format = 'e'
	}
	dst = strconv.AppendFloat(dst, value, format, -1, width)
	if format == 'e' {
		// strconv writes a two-digit exponent where JSON conventionally has one:
		// 1e-09 becomes 1e-9.
		if end := len(dst); end >= 4 && dst[end-4] == 'e' && dst[end-3] == '-' && dst[end-2] == '0' {
			dst[end-2] = dst[end-1]
			dst = dst[:end-1]
		}
	}
	return dst
}

// The two runes that are a line break in JavaScript and not in JSON. They are
// escaped for the same reason the HTML-significant bytes are: so that the output
// can be pasted into a script tag and still be the document it was.
const (
	lineSeparator      rune = 0x2028
	paragraphSeparator rune = 0x2029
)

// appendJSONString writes a quoted, escaped JSON string, with encoding/json's
// default escape set.
func appendJSONString(dst []byte, value string) []byte {
	dst = append(dst, '"')
	start := 0
	for at := 0; at < len(value); {
		if character := value[at]; character < utf8.RuneSelf {
			if jsonSafe(character) {
				at++
				continue
			}
			dst = append(dst, value[start:at]...)
			switch character {
			case '\\', '"':
				dst = append(dst, '\\', character)
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			default:
				// A control byte, or one of the three HTML-significant ones,
				// which go the long way round for the same reason: so the output
				// is safe wherever it is pasted.
				dst = append(dst, '\\', 'u', '0', '0',
					hexDigits[character>>4], hexDigits[character&0xF])
			}
			at++
			start = at
			continue
		}
		character, width := utf8.DecodeRuneInString(value[at:])
		switch {
		case character == utf8.RuneError && width == 1:
			// Invalid UTF-8 is replaced rather than refused, which is what
			// encoding/json does and what keeps one bad byte from losing a
			// whole message.
			dst = append(append(dst, value[start:at]...), '\\', 'u', 'f', 'f', 'f', 'd')
		case character == lineSeparator || character == paragraphSeparator:
			dst = append(append(dst, value[start:at]...),
				'\\', 'u', '2', '0', '2', hexDigits[character&0xF])
		default:
			at += width
			continue
		}
		at += width
		start = at
	}
	return append(append(dst, value[start:]...), '"')
}

const hexDigits = "0123456789abcdef"

// jsonSafe reports whether a byte may go into a string as it stands.
func jsonSafe(character byte) bool {
	return character >= 0x20 &&
		character != '"' && character != '\\' &&
		character != '<' && character != '>' && character != '&'
}
