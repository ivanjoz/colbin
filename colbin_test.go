package colbin_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ivanjoz/colbin"
)

// The façade, exercised through the public API only — which is the point of this
// file: everything below it has its own tests, and this one is what a caller
// actually touches.

type charge struct {
	CompanyID    int32    `cb:"1"`
	UserID       int32    `cb:"2"`
	RouteID      uint16   `cb:"3"`
	Name         string   `cb:"4"`
	Grants       []uint16 `cb:"5"`
	ExtraAllowed bool     `cb:"6"`
	Ratio        float64  `cb:"7"`
}

var sample = charge{
	CompanyID: 7, UserID: 42, RouteID: 103,
	Name: "responses.go:539", Grants: []uint16{0x0139, 0x008B},
	ExtraAllowed: true, Ratio: 1.5,
}

func TestMarshalRoundTrip(t *testing.T) {
	data, err := colbin.Marshal(&sample)
	if err != nil {
		t.Fatal(err)
	}
	var back charge
	if err := colbin.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.CompanyID != sample.CompanyID || back.Name != sample.Name ||
		back.Ratio != sample.Ratio || !back.ExtraAllowed ||
		len(back.Grants) != 2 || back.Grants[0] != 0x0139 {
		t.Fatalf("round-tripped as %+v", back)
	}
}

// A value, not a pointer, must work the same: the plan reads fields by offset,
// so a non-addressable value is copied once into somewhere it can.
func TestMarshalAcceptsAValue(t *testing.T) {
	fromValue, err := colbin.Marshal(sample)
	if err != nil {
		t.Fatal(err)
	}
	fromPointer, err := colbin.Marshal(&sample)
	if err != nil {
		t.Fatal(err)
	}
	if string(fromValue) != string(fromPointer) {
		t.Fatalf("value %x, pointer %x", fromValue, fromPointer)
	}
}

// An omitted field is a zero field, and the decoder clears the destination
// rather than leaving whatever it held.
func TestOmittedFieldsClearTheDestination(t *testing.T) {
	data, err := colbin.Marshal(&charge{CompanyID: 1})
	if err != nil {
		t.Fatal(err)
	}
	back := sample // deliberately dirty
	if err := colbin.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Name != "" || back.Grants != nil || back.ExtraAllowed || back.Ratio != 0 {
		t.Fatalf("omitted fields left %+v", back)
	}
	if back.CompanyID != 1 {
		t.Fatalf("the one set field read as %d", back.CompanyID)
	}
}

func TestAppendReusesTheBuffer(t *testing.T) {
	buffer := make([]byte, 0, 128)
	for range 3 {
		var err error
		buffer, err = colbin.Append(buffer[:0], &sample)
		if err != nil {
			t.Fatal(err)
		}
	}
	var back charge
	if err := colbin.Unmarshal(buffer, &back); err != nil {
		t.Fatal(err)
	}
	if back.Name != sample.Name {
		t.Fatalf("round-tripped as %+v", back)
	}
}

func TestCodecHandle(t *testing.T) {
	handle := colbin.MustCodec[charge]()
	message := handle.Encode(&sample)
	var back charge
	if err := handle.Unmarshal(message, &back); err != nil {
		t.Fatal(err)
	}
	if back.RouteID != sample.RouteID {
		t.Fatalf("round-tripped as %+v", back)
	}
	// The handle and the façade must agree byte for byte.
	viaFacade, err := colbin.Marshal(&sample)
	if err != nil {
		t.Fatal(err)
	}
	if string(message) != string(viaFacade) {
		t.Fatalf("handle %x, façade %x", message, viaFacade)
	}
}

func TestFieldIDs(t *testing.T) {
	ids, err := colbin.FieldIDs(charge{})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]uint8{
		"CompanyID": 0, "UserID": 1, "RouteID": 2, "Name": 3,
		"Grants": 4, "ExtraAllowed": 5, "Ratio": 6,
	} {
		if ids[name] != want {
			t.Fatalf("%s has id %d, want %d", name, ids[name], want)
		}
	}
}

// The packed5 switch is a writer setting, so a message written with it on must
// read back with it off and the other way round.
func TestPacked5IsAWriterSetting(t *testing.T) {
	if colbin.Packed5() {
		t.Fatal("packed5 should be off by default")
	}
	defer colbin.SetPacked5(false)

	off, err := colbin.Marshal(&sample)
	if err != nil {
		t.Fatal(err)
	}
	colbin.SetPacked5(true)
	if !colbin.Packed5() {
		t.Fatal("SetPacked5(true) did not take")
	}
	on, err := colbin.Marshal(&sample)
	if err != nil {
		t.Fatal(err)
	}

	colbin.SetPacked5(false)
	for label, message := range map[string][]byte{"off": off, "on": on} {
		var back charge
		if err := colbin.Unmarshal(message, &back); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if back.Name != sample.Name {
			t.Fatalf("%s: name round-tripped as %q", label, back.Name)
		}
	}
}

// The out-of-band delivery, end to end through the public API: describe the type
// once, send the section, and turn ordinary messages into JSON on the other side
// with no Go type in sight.
func TestSchemaOutOfBand(t *testing.T) {
	schema, err := colbin.SchemaFor[charge]()
	if err != nil {
		t.Fatal(err)
	}
	// What crosses the wire once, and what crosses it every time.
	section, err := colbin.ParseSchema(schema.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	data, err := colbin.Marshal(&sample)
	if err != nil {
		t.Fatal(err)
	}
	text, err := colbin.ToJSON(section, data)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(&sample)
	if err != nil {
		t.Fatal(err)
	}
	var got, expected any
	if err := json.Unmarshal(text, &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, text)
	}
	if err := json.Unmarshal(want, &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("\n got %s\nwant %s", text, want)
	}

	value, err := colbin.DecodeAny(section, data)
	if err != nil {
		t.Fatal(err)
	}
	if name := value.(map[string]any)["Name"]; name != sample.Name {
		t.Fatalf("DecodeAny gave Name = %v", name)
	}
}

// A self-describing message needs no schema passed in, and still decodes into
// the Go type: the section is additive rather than a second format.
func TestMarshalSelfDescribing(t *testing.T) {
	data, err := colbin.MarshalSelfDescribing(&sample)
	if err != nil {
		t.Fatal(err)
	}
	if !colbin.IsColbin(data) {
		t.Fatalf("byte 0 is %#02x, outside colbin's range", data[0])
	}
	var back charge
	if err := colbin.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, sample) {
		t.Fatalf("round-tripped as %+v", back)
	}
	text, err := colbin.ToJSON(nil, data)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(text) {
		t.Fatalf("not valid JSON: %s", text)
	}
}
