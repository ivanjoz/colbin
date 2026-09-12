package codec

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestGenerateMinimalProducesCompilableSource(t *testing.T) {
	source, err := GenerateString("billing", benchRecord{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"func (v *benchRecord) AppendColbin(dst []byte) []byte",
		"w.I32(0, v.CompanyID)",
		"w.U16(2, v.RouteID)",
		"w.Bool(5, v.ExtraAllowed)",
		"case 6:",
		"v.Access1 = r.U16()",
		"const _ = uint(15 - 9)",
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("generated source is missing %q:\n%s", want, source)
		}
	}
}

// The checked-in generated codec must match what the generator produces now, so
// that a change to the format or to the plan cannot leave stale generated code
// compiling against it.
func TestGeneratedCodecIsUpToDate(t *testing.T) {
	source, err := GenerateString("codec", GenCharge{})
	if err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile("gencharge_colbin_test.go")
	if err != nil {
		t.Fatal(err)
	}
	// The checked-in file carries the type declaration and a regenerate hint
	// that the generator does not emit; compare the bodies it does.
	for _, want := range strings.Split(strings.TrimSpace(source), "\n") {
		if strings.TrimSpace(want) == "" || strings.HasPrefix(want, "//") {
			continue
		}
		if !strings.Contains(string(onDisk), want) {
			t.Fatalf("gencharge_colbin_test.go is stale: it is missing\n\t%s", want)
		}
	}
}

// The generated codec must agree with the reflective one byte for byte, and
// round-trip through it. Nothing else guarantees the generator emits the same
// width-typed call the plan walk would take.
func TestGeneratedCodecMatchesReflection(t *testing.T) {
	charge := GenCharge{CompanyID: 7, UserID: 42, RouteID: 103, CPU: 5, Access1: 0x0139}
	generated := charge.AppendColbin(nil)

	reflected, err := Append(nil, &charge)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(generated, reflected) {
		t.Fatalf("generated %x, reflective %x", generated, reflected)
	}

	var back GenCharge
	if err := back.UnmarshalColbin(generated); err != nil {
		t.Fatal(err)
	}
	if back != charge {
		t.Fatalf("round-tripped as %+v", back)
	}
	// And the reflective decoder reads what the generated encoder wrote.
	back = GenCharge{}
	if err := Unmarshal(generated, &back); err != nil {
		t.Fatal(err)
	}
	if back != charge {
		t.Fatalf("reflective decode gave %+v", back)
	}
}

var genCharge = GenCharge{CompanyID: 7, UserID: 42, RouteID: 103, CPU: 5, Access1: 0x0139}

func BenchmarkGeneratedAppend(b *testing.B) {
	buffer := make([]byte, 0, 64)
	b.ReportAllocs()
	for b.Loop() {
		buffer = genCharge.AppendColbin(buffer[:0])
	}
}

func BenchmarkGeneratedUnmarshal(b *testing.B) {
	message := genCharge.AppendColbin(nil)
	var back GenCharge
	b.ReportAllocs()
	for b.Loop() {
		_ = back.UnmarshalColbin(message)
	}
}
