package codec

import (
	"fmt"
	"reflect"
	"runtime"
	"testing"
	"unsafe"

	"github.com/ivanjoz/colbin/wire"
)

// A struct at the root of a browser message, with a nested struct and a list
// that becomes a table: the shape of foreign_vectors/order*.bin.

type foreignCustomer struct {
	Tier int8   `cb:"3" json:"tier"`
	Name string `cb:"1" json:"name"`
}

type foreignLine struct {
	Price float32 `cb:"3" json:"price"`
	SKU   string  `cb:"1" json:"sku"`
	Qty   int16   `cb:"2" json:"qty"`
}

type foreignOrder struct {
	Total    float64         `cb:"4" json:"total"`
	Number   uint32          `cb:"1" json:"number"`
	Customer foreignCustomer `cb:"2" json:"customer"`
	Lines    []foreignLine   `cb:"3" json:"lines"`
}

func TestForeignOrders(t *testing.T) {
	for file, lines := range map[string]int{"order.standalone.bin": 4, "order600.standalone.bin": 600} {
		t.Run(file, func(t *testing.T) {
			var got foreignOrder
			if err := Unmarshal(foreignFile(t, file), &got); err != nil {
				t.Fatal(err)
			}
			if got.Number != 1234 || got.Total != 99.5 || got.Customer != (foreignCustomer{Tier: 2, Name: "ada"}) {
				t.Fatalf("got %+v", got)
			}
			if len(got.Lines) != lines {
				t.Fatalf("%d lines, want %d", len(got.Lines), lines)
			}
			for i, line := range got.Lines {
				if want := (foreignLine{Price: float32(i) + 0.5, SKU: "sku-" + itoa(i), Qty: int16(i + 1)}); line != want {
					t.Fatalf("line %d = %+v, want %+v", i, line, want)
				}
			}
		})
	}
}

// foreignTargets are the fixed types a hostile message is decoded into: a list of
// narrow records, a list of wide ones, and a struct root.
func foreignTargets() []func(data []byte) error {
	return []func([]byte) error{
		func(data []byte) error { return Unmarshal(data, new([]foreignProduct)) },
		func(data []byte) error { return Unmarshal(data, new([]foreignProductWide)) },
		func(data []byte) error { return Unmarshal(data, new(foreignOrder)) },
	}
}

// TestForeignSurvivesEveryCorruptedByte changes each byte of each browser message
// to a few other values and decodes it three ways. Most come back as an error and
// some as other data; the rule is that none panics, hangs or writes outside the
// value, which the race detector and -d=checkptr watch for.
func TestForeignSurvivesEveryCorruptedByte(t *testing.T) {
	for _, file := range []string{"products3.standalone.bin", "order.standalone.bin"} {
		original := foreignFile(t, file)
		t.Run(file, func(t *testing.T) {
			for at := range original {
				for _, replace := range []func(byte) byte{
					func(byte) byte { return 0 },
					func(byte) byte { return 0xFF },
					func(b byte) byte { return b + 1 },
					func(b byte) byte { return b ^ 0x80 },
				} {
					data := append([]byte(nil), original...)
					data[at] = replace(data[at])
					returns(t, fmt.Sprintf("%s byte %d", file, at), func() {
						for _, decode := range foreignTargets() {
							_ = decode(data)
						}
					})
				}
			}
		})
	}
}

// TestForeignSurvivesTruncation cuts every message short at every length.
func TestForeignSurvivesTruncation(t *testing.T) {
	for _, file := range []string{"products3.standalone.bin", "order.standalone.bin"} {
		original := foreignFile(t, file)
		for length := range original {
			for _, decode := range foreignTargets() {
				_ = decode(original[:length])
			}
		}
	}
}

// section builds a section by hand: one struct, narrow, with the named int64
// fields, keys 0, 1, ... and the message that carries it, whose body is empty.
func handSection(names ...string) []byte {
	body := wire.AppendLength(nil, 1) // one struct
	def := wire.AppendLength([]byte{0}, len(names))
	for key, name := range names {
		def = append(def, byte(key))
		def = wire.AppendLength(def, len(name))
		def = append(def, name...)
		def = append(def, byte(opInt64))
	}
	body = append(body, def...)
	section := wire.AppendLength(nil, len(body))
	return append(section, body...)
}

// TestForeignCacheIsBounded is the memory test: a client sending a new, valid
// section with every message builds a translation each time and keeps almost none
// of them.
func TestForeignCacheIsBounded(t *testing.T) {
	type target struct {
		V int32 `json:"v"`
	}
	foreignCache.reset()
	var before runtime.MemStats
	measure := func() uint64 {
		runtime.GC()
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		return stats.HeapAlloc
	}
	run := func(from, to int) {
		for i := from; i < to; i++ {
			// The first field is the one the type has; the second is distinct per message.
			message := append([]byte{rootStructNarrowSchema}, handSection("v", fmt.Sprintf("extra%d", i))...)
			var got target
			if err := Unmarshal(message, &got); err != nil {
				t.Fatalf("message %d: %v", i, err)
			}
		}
	}
	run(0, 200)
	_ = before
	base := measure()
	run(200, 10200)
	entries, bytes := foreignCache.size()
	if entries > maxForeignPerType {
		t.Fatalf("%d translations held, want at most %d", entries, maxForeignPerType)
	}
	if bytes > maxForeignPerType*maxCachedSection {
		t.Fatalf("%d section bytes held", bytes)
	}
	if grown := int64(measure()) - int64(base); grown > 1<<20 {
		t.Fatalf("heap grew by %d bytes over 10000 distinct sections", grown)
	}
}

// A section too large to be a schema anyone wrote decodes but is not kept.
func TestForeignDoesNotCacheAHugeSection(t *testing.T) {
	type target struct {
		V int32 `json:"v"`
	}
	names := make([]string, 0, 3000)
	names = append(names, "v")
	for i := 1; i < 3000; i++ {
		names = append(names, fmt.Sprintf("a_field_with_a_long_name_%d_padding_padding", i))
	}
	section := handSection(names...)
	if len(section) <= maxCachedSection {
		t.Fatalf("section is %d bytes, want more than %d", len(section), maxCachedSection)
	}
	foreignCache.reset()
	// 3000 fields do not fit a four-bit key, so it is refused on parse; the point
	// here is only that nothing is kept for it.
	_ = Unmarshal(append([]byte{rootStructNarrowSchema}, section...), new(target))
	if entries, _ := foreignCache.size(); entries != 0 {
		t.Fatalf("%d translations kept", entries)
	}
}

// A translation is shared between goroutines: the cache is hit and evicted at
// once and every decode still comes back right.
func TestForeignCacheUnderConcurrency(t *testing.T) {
	type target struct {
		V int32 `json:"v"`
	}
	foreignCache.reset()
	errs := make(chan error, 8)
	for worker := range 8 {
		go func() {
			for i := range 500 {
				names := []string{"v", fmt.Sprintf("extra%d", (worker*7+i)%20)}
				message := append([]byte{rootStructNarrowSchema}, handSection(names...)...)
				var got target
				if err := Unmarshal(message, &got); err != nil {
					errs <- err
					return
				}
			}
			errs <- nil
		}()
	}
	for range 8 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if entries, _ := foreignCache.size(); entries > maxForeignPerType {
		t.Fatalf("%d translations held", entries)
	}
}

func FuzzForeignUnmarshal(f *testing.F) {
	for _, name := range []string{
		"products3.standalone.bin", "products500.standalone.bin", "overflow500.standalone.bin",
		"order.standalone.bin", "order600.standalone.bin", "products500.message.bin",
	} {
		f.Add(foreignFile(f, name))
	}
	// Sections a Go peer wrote for other types, so the keys and widths vary.
	for _, value := range []any{
		[]foreignProductWide{{Qty: 1}}, foreignOuterFrom{Name: "n", Many: []foreignInner{{Count: 1}}},
		[]narrowOne[int64]{{1}}, wideOne[[]uint64]{[]uint64{1}}, narrowOne[*float64]{ptr(1.5)},
	} {
		message, err := MarshalSelfDescribing(value)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(message)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, decode := range foreignTargets() {
			_ = decode(data)
		}
		var outer foreignOuterTo
		var rows []wideOne[int16]
		var ints []narrowOne[[]int32]
		_ = Unmarshal(data, &outer)
		if Unmarshal(data, &rows) == nil {
			if _, err := Marshal(&rows); err != nil {
				t.Fatalf("a decoded value does not encode: %v", err)
			}
		}
		_ = Unmarshal(data, &ints)
	})
}

// The translation never reads a plan that has no layout: a parsed section's own
// plans carry zero offsets, and writing through one stores over the record's head.
func TestForeignPlansAreNotTheSectionsPlans(t *testing.T) {
	var got []foreignProduct
	if err := Unmarshal(foreignFile(t, "products3.standalone.bin"), &got); err != nil {
		t.Fatal(err)
	}
	foreign, err := foreignFor(reflect.TypeFor[[]foreignProduct](), mustRootPlan(t, reflect.TypeFor[[]foreignProduct]()),
		foreignFile(t, "products3.standalone.bin")[1:])
	if err != nil || foreign == nil {
		t.Fatalf("no translation: %v", err)
	}
	var check func(plan *typePlan, seen map[*typePlan]bool)
	check = func(plan *typePlan, seen map[*typePlan]bool) {
		if seen[plan] {
			return
		}
		seen[plan] = true
		if plan.fromSchema {
			t.Fatal("a translated plan is a parsed one")
		}
		for _, field := range plan.fields {
			if field.op == opConvert && (field.conv == 0 || int(field.conv) > len(foreign.convs)) {
				t.Fatalf("conversion %d of %d", field.conv, len(foreign.convs))
			}
			if field.sub != nil {
				check(field.sub, seen)
			}
		}
	}
	check(foreign.root, map[*typePlan]bool{})
}

func mustRootPlan(t *testing.T, rootType reflect.Type) *typePlan {
	t.Helper()
	plan, err := planForRoot(rootType)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

// planField is read once per field per message, so what the foreign decode added
// to it has to live in the padding it already had.
func TestPlanFieldDidNotGrow(t *testing.T) {
	// 80 on 64 bits with the three bools and five bytes of padding; 44 where a
	// pointer, uintptr and interface word are four bytes.
	want := uintptr(80)
	if unsafe.Sizeof(uintptr(0)) == 4 {
		want = 44
	}
	if got := unsafeSizeofPlanField(); got != want {
		t.Fatalf("planField is %d bytes, was %d before the foreign decode", got, want)
	}
}

func unsafeSizeofPlanField() uintptr { return unsafe.Sizeof(planField{}) }
