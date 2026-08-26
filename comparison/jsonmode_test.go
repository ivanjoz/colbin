package comparison

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"github.com/ivanjoz/colbin"
)

// modelSlices returns every corpus model as a []*T value, keyed by field name.
func modelSlices(c *BenchmarkCorpus) map[string]any {
	out := map[string]any{}
	cv := reflect.ValueOf(c).Elem()
	for i := 0; i < cv.NumField(); i++ {
		f := cv.Type().Field(i)
		if f.PkgPath != "" || cv.Field(i).Kind() != reflect.Slice {
			continue
		}
		out[f.Name] = cv.Field(i).Interface()
	}
	return out
}

// TestJSONModeMatchesBinaryDecode checks the JSON mode against the typed decoder
// over all 21 models: reading the emitted JSON back into the Go type must land on
// the same value the binary path produces. This is the equivalence that matters,
// since the JSON reader works from the message's schema alone.
//
// It is not compared to encoding/json's output directly because these models tag
// their fields `omitempty`, which a columnar layout cannot honour — every record
// carries a value for every column, so zero values come back as 0 / "" / null
// rather than being absent.
func TestJSONModeMatchesBinaryDecode(t *testing.T) {
	for name, v := range modelSlices(GenerateCorpus(7, 5)) {
		msg, err := colbin.MarshalJSON(v)
		if err != nil {
			t.Errorf("%s: MarshalJSON: %v", name, err)
			continue
		}
		text, err := colbin.DecodeJSON(msg)
		if err != nil {
			t.Errorf("%s: DecodeJSON: %v", name, err)
			continue
		}
		viaJSON := reflect.New(reflect.TypeOf(v))
		if err := json.Unmarshal(text, viaJSON.Interface()); err != nil {
			t.Errorf("%s: re-reading the emitted JSON: %v", name, err)
			continue
		}
		viaBinary := reflect.New(reflect.TypeOf(v))
		if err := colbin.Unmarshal(msg, viaBinary.Interface()); err != nil {
			t.Errorf("%s: Unmarshal: %v", name, err)
			continue
		}
		// Proto models carry unexported bookkeeping fields, so the two decoded
		// values are compared through their JSON projection rather than directly.
		fromJSON, _ := json.Marshal(viaJSON.Interface())
		fromBinary, _ := json.Marshal(viaBinary.Interface())
		if string(fromJSON) != string(fromBinary) {
			t.Errorf("%s: the JSON path and the binary path disagree\n json %s\n bin  %s",
				name, fromJSON, fromBinary)
		}
	}
}

// TestJSONModeSchemaOverhead pins the cost of the mode — the schema section is
// the only thing MarshalJSON adds, and it is a per-message constant that does
// not grow with the record count — and prints it per model with -v.
func TestJSONModeSchemaOverhead(t *testing.T) {
	small, large := modelSlices(GenerateCorpus(7, 10)), modelSlices(GenerateCorpus(7, 200))
	names := make([]string, 0, len(small))
	for name := range small {
		names = append(names, name)
	}
	sort.Strings(names)

	t.Logf("%-20s %10s %10s %8s %8s", "model", "10 recs", "200 recs", "schema", "of 10")
	for _, name := range names {
		schema10, body10 := schemaAndBody(t, small[name])
		schema200, body200 := schemaAndBody(t, large[name])
		if schema10 != schema200 {
			t.Errorf("%s: schema section is %d bytes at 10 records and %d at 200: it must not scale with the data",
				name, schema10, schema200)
		}
		if schema10 <= 0 {
			t.Errorf("%s: schema section is %d bytes", name, schema10)
		}
		t.Logf("%-20s %10d %10d %8d %7.1f%%", name, body10, body200, schema10,
			100*float64(schema10)/float64(body10))
	}
}

// schemaAndBody marshals v both ways and returns the size of the schema section
// and of the plain binary payload. Their sum being the JSON-mode size is what
// keeps the binary mode's size exactly where it was.
func schemaAndBody(t *testing.T, v any) (schema, body int) {
	t.Helper()
	plain, err := colbin.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	withSchema, err := colbin.MarshalJSON(v)
	if err != nil {
		t.Fatal(err)
	}
	return len(withSchema) - len(plain), len(plain)
}
