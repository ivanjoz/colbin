package codec

import (
	"reflect"
	"testing"
)

// Untagged fields, which take their id from their name.

type bare struct {
	SensorID  uint64
	Timestamp int64
	Value     float64
	Unit      string
	Samples   []int32
	Valid     bool
}

// mixed declares some ids and leaves the rest to the hash, which is the case
// assignKeys exists to get right: a declared id is reserved before any hash is
// allowed to land on it.
type mixed struct {
	First  int32  `cb:"0"`
	Second string `cb:"1"`
	Third  uint32
	Fourth bool
}

func TestUntaggedRoundTrips(t *testing.T) {
	original := bare{
		SensorID:  9124,
		Timestamp: 1767225600123,
		Value:     21.5,
		Unit:      "C",
		Samples:   []int32{21, 22, 21},
		Valid:     true,
	}
	data, err := Marshal(&original)
	if err != nil {
		t.Fatal(err)
	}
	if data[0] != rootStructWide {
		t.Fatalf("root is %#02x, want the wide one: a derived id does not fit four bits",
			data[0])
	}
	var back bare
	if err := Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.SensorID != original.SensorID || back.Timestamp != original.Timestamp ||
		back.Value != original.Value || back.Unit != original.Unit ||
		len(back.Samples) != 3 || !back.Valid {
		t.Fatalf("round-tripped as %+v", back)
	}
}

// TestDerivedIDsAreStable is the property that makes the wire readable by
// another language: the same name always gives the same number.
func TestDerivedIDsAreStable(t *testing.T) {
	ids, err := FieldIDs(bare{})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]uint8{
		"SensorID":  fnv8("SensorID"),
		"Timestamp": fnv8("Timestamp"),
		"Value":     fnv8("Value"),
		"Unit":      fnv8("Unit"),
		"Samples":   fnv8("Samples"),
		"Valid":     fnv8("Valid"),
	} {
		if ids[name] != want {
			t.Errorf("%s got id %d, want fnv8 = %d", name, ids[name], want)
		}
	}
}

// TestDeclaredIDsWinOverHashes checks the ordering assignKeys promises.
func TestDeclaredIDsWinOverHashes(t *testing.T) {
	ids, err := FieldIDs(mixed{})
	if err != nil {
		t.Fatal(err)
	}
	if ids["First"] != 0 || ids["Second"] != 1 {
		t.Fatalf("declared ids moved: %+v", ids)
	}
	if ids["Third"] != fnv8("Third") || ids["Fourth"] != fnv8("Fourth") {
		t.Fatalf("derived ids are not their hashes: %+v", ids)
	}
	seen := map[uint8]string{}
	for name, id := range ids {
		if other, clash := seen[id]; clash {
			t.Fatalf("%s and %s both got id %d", name, other, id)
		}
		seen[id] = name
	}

	original := mixed{First: -5, Second: "x", Third: 9, Fourth: true}
	data, err := Marshal(&original)
	if err != nil {
		t.Fatal(err)
	}
	var back mixed
	if err := Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back != original {
		t.Fatalf("round-tripped as %+v, want %+v", back, original)
	}
}

// TestAHashLandingOnATakenIDMovesOneDown is the collision rule itself, driven
// through assignKeys because a struct tag cannot hold a computed number.
func TestAHashLandingOnATakenIDMovesOneDown(t *testing.T) {
	claimed := fnv8("Second") // the slot the hash wants, claimed outright
	plan := &typePlan{fields: make([]planField, 2)}
	err := plan.assignKeys(reflect.TypeOf(struct{}{}),
		[]string{"Taken", "Second"},
		[]string{"Taken", "Second"},
		[]int{int(claimed), -1})
	if err != nil {
		t.Fatal(err)
	}
	if plan.fields[0].key != claimed {
		t.Fatalf("the declared id moved to %d, want %d", plan.fields[0].key, claimed)
	}
	if plan.fields[1].key != claimed+1 {
		t.Fatalf("the colliding hash went to %d, want %d — one down",
			plan.fields[1].key, claimed+1)
	}
	if !plan.derivedKeys {
		t.Fatal("a derived id did not set derivedKeys, so the type would stay narrow")
	}
}

// TestProbeMovesOneDown pins the collision rule itself: the next slot upward.
func TestProbeMovesOneDown(t *testing.T) {
	taken := map[uint8]string{7: "a", 8: "b", 9: "c"}
	if got := probeFieldID(7, taken); got != 10 {
		t.Fatalf("probe from 7 over 7,8,9 gave %d, want 10", got)
	}
	if got := probeFieldID(20, taken); got != 20 {
		t.Fatalf("probe from a free slot gave %d, want 20", got)
	}
}

func TestUntaggedSkipsUnknownFields(t *testing.T) {
	// The wide key is what makes this work: a reader that does not know a name
	// steps over it rather than failing, which a narrow key cannot do.
	type wider struct {
		SensorID uint64
		Unit     string
		Extra    string
	}
	data, err := Marshal(&wider{SensorID: 5, Unit: "C", Extra: "ignored"})
	if err != nil {
		t.Fatal(err)
	}
	var back bare
	if err := Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.SensorID != 5 || back.Unit != "C" {
		t.Fatalf("round-tripped as %+v", back)
	}
}
