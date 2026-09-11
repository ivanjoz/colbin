package codec

import (
	"reflect"
	"testing"
	"unsafe"

	"github.com/ivanjoz/colbin/compact"
)

// The plan is the type's cache, so it must describe the type exactly: one op per
// field, in declaration order, each with the field's own offset and id.
func TestCompactPlanMatchesFields(t *testing.T) {
	ti, err := getTypeInfo(reflect.TypeFor[cmWideNumbered]())
	if err != nil {
		t.Fatal(err)
	}
	pl := compactPlanFor(ti)
	if !pl.usable {
		t.Fatal("cmWideNumbered is not usable in compact mode")
	}
	if len(pl.ops) != len(ti.fields) {
		t.Fatalf("%d ops for %d fields", len(pl.ops), len(ti.fields))
	}
	for i := range pl.ops {
		op, fm := &pl.ops[i], &ti.fields[i]
		if op.id != fm.id || op.offset != fm.offset {
			t.Errorf("op %d: id %d offset %d, want id %d offset %d",
				i, op.id, op.offset, fm.id, fm.offset)
		}
		if op.kind == opNone {
			t.Errorf("op %d (%s): no opcode", i, fm.name)
		}
		if got := pl.byID[fm.id]; got != uint8(i+1) {
			t.Errorf("byID[%d] = %d, want %d", fm.id, got, i+1)
		}
	}
	// Every op form in the table above must be reachable from a real type, or
	// the switch arms that handle it are never exercised.
	seen := map[uint8]bool{}
	for i := range pl.ops {
		seen[pl.ops[i].kind] = true
	}
	if len(seen) < 8 {
		t.Errorf("cmWideNumbered exercises only %d op kinds", len(seen))
	}
}

// Only signed integers are scanned for ALL_POSITIVE: unsigned values are never
// zigzagged and a float is not a varint.
func TestCompactPlanSignedList(t *testing.T) {
	type rec struct {
		A int32   `cb:"1"`
		B uint32  `cb:"2"`
		C float64 `cb:"3"`
		D int64   `cb:"4"`
		E string  `cb:"5"`
		F bool    `cb:"6"`
	}
	ti, _ := getTypeInfo(reflect.TypeFor[rec]())
	pl := compactPlanFor(ti)
	if want := []uint8{0, 3}; !reflect.DeepEqual(pl.signed, want) {
		t.Fatalf("signed = %v, want %v", pl.signed, want)
	}

	v := rec{A: 1, B: 2, D: 4}
	p := unsafe.Pointer(&v)
	if !compactAllPositivePlan(pl, []unsafe.Pointer{p}) {
		t.Error("all-positive record reported negative")
	}
	v.D = -1
	if compactAllPositivePlan(pl, []unsafe.Pointer{p}) {
		t.Error("negative record reported all-positive")
	}
	// An unsigned value above MaxInt64 must not read as negative.
	v.D = 0
	v.B = 1 << 31
	if !compactAllPositivePlan(pl, []unsafe.Pointer{p}) {
		t.Error("a large unsigned value was scanned as signed")
	}
}

// A type with no signed integer at all skips the scan entirely.
func TestCompactPlanNoSignedFields(t *testing.T) {
	type rec struct {
		A uint32 `cb:"1"`
		B string `cb:"2"`
	}
	ti, _ := getTypeInfo(reflect.TypeFor[rec]())
	if pl := compactPlanFor(ti); len(pl.signed) != 0 {
		t.Fatalf("signed = %v, want empty", pl.signed)
	}
}

// One field without a compact form disqualifies the whole type, and the plan is
// what records that.
func TestCompactPlanIneligible(t *testing.T) {
	for _, v := range []any{cmAny{}, cmCyclic{}, cmPointer{}, cmPlatformInt{}, struct{}{}} {
		ti, err := getTypeInfo(reflect.TypeOf(v))
		if err != nil {
			t.Fatalf("%T: %v", v, err)
		}
		pl := compactPlanFor(ti)
		if pl.usable {
			t.Errorf("%T: usable", v)
		}
		if len(pl.ops) != 0 {
			t.Errorf("%T: %d ops on an unusable plan", v, len(pl.ops))
		}
	}
}

// The plan is built once and handed out thereafter; asking twice must return the
// identical value, since everything downstream caches pointers into it.
func TestCompactPlanMemoised(t *testing.T) {
	ti, _ := getTypeInfo(reflect.TypeFor[cmNumbered]())
	first := compactPlanFor(ti)
	if second := compactPlanFor(ti); second != first {
		t.Fatal("compactPlanFor built a second plan")
	}
	if first.keys != compact.Keys4 {
		t.Fatalf("keys %d, want %d", first.keys, compact.Keys4)
	}
}
