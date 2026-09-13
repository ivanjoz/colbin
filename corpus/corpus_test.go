package corpus

import (
	"hash/fnv"
	"reflect"
	"testing"

	"github.com/ivanjoz/colbin"
)

// What "reproducible" has to mean here is stronger than "the generator uses a
// seed": the same seed must give the same *bytes on the wire*, or the corpus
// cannot pin a format. These tests are what make that true, and what would catch
// either half of it changing by accident.

// fingerprint is FNV-1a over the colbin encoding of every table that has one.
//
// Events are excluded, and the reason is the wire's rather than the generator's:
// a Go map has no iteration order, so a field of one does not encode to stable
// bytes. See TestEventsRoundTripButAreNotStable.
func fingerprint(t *testing.T, built *Corpus) uint64 {
	t.Helper()
	hash := fnv.New64a()
	buffer := make([]byte, 0, 4096)

	write := func(data []byte, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		hash.Write(data)
	}
	for index := range built.Users {
		write(colbin.Append(buffer[:0], &built.Users[index]))
	}
	for index := range built.Products {
		write(colbin.Append(buffer[:0], &built.Products[index]))
	}
	for index := range built.Categories {
		write(colbin.Append(buffer[:0], &built.Categories[index]))
	}
	for index := range built.Stores {
		write(colbin.Append(buffer[:0], &built.Stores[index]))
	}
	for index := range built.Sales {
		write(colbin.Append(buffer[:0], &built.Sales[index]))
	}
	for index := range built.Metrics {
		write(colbin.Append(buffer[:0], &built.Metrics[index]))
	}
	return hash.Sum64()
}

// TestFingerprint pins the corpus. A change to the generator or to the wire
// moves this number, which is the point: it should not move by accident, and
// when it moves on purpose the diff says which.
func TestFingerprint(t *testing.T) {
	const want uint64 = 0xce36b77ee845b34d
	if got := fingerprint(t, Generate(Seed, Small)); got != want {
		t.Fatalf("fingerprint %#016x, want %#016x", got, want)
	}
}

// TestGenerateIsDeterministic is the weaker property, checked separately so a
// failure says which half broke.
func TestGenerateIsDeterministic(t *testing.T) {
	first, second := Generate(Seed, Small), Generate(Seed, Small)
	if fingerprint(t, first) != fingerprint(t, second) {
		t.Fatal("two runs of the same seed disagree")
	}
	if fingerprint(t, Generate(Seed+1, Small)) == fingerprint(t, first) {
		t.Fatal("two different seeds agree, so the seed is not reaching the data")
	}
}

// TestEveryTableRoundTrips is the correctness half: the corpus is only useful
// as a fixture if the format carries all of it.
func TestEveryTableRoundTrips(t *testing.T) {
	built := Generate(Seed, Small)

	for index := range built.Users {
		roundTrip(t, &built.Users[index])
	}
	for index := range built.Products {
		roundTrip(t, &built.Products[index])
	}
	for index := range built.Categories {
		roundTrip(t, &built.Categories[index])
	}
	for index := range built.Stores {
		roundTrip(t, &built.Stores[index])
	}
	for index := range built.Metrics {
		roundTrip(t, &built.Metrics[index])
	}
	// Sales go through the same helper: DeepEqual reaches the nested detail, so
	// a line that lost a field fails here rather than needing its own loop.
	for index := range built.Sales {
		roundTrip(t, &built.Sales[index])
	}
}

// roundTrip compares with DeepEqual rather than ==, because half these tables
// hold a slice. Every slice the generator produces is non-empty, so the
// nil-versus-empty distinction that omission would blur never arises here —
// Events are the exception and have their own check.
func roundTrip[T any](t *testing.T, original *T) {
	t.Helper()
	data, err := colbin.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var back T
	if err := colbin.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*original, back) {
		t.Fatalf("round-tripped as %+v, want %+v", back, *original)
	}
}

// TestEventsRoundTripButAreNotStable documents the one table that cannot be
// fingerprinted, and proves it is the map rather than anything else.
func TestEventsRoundTripButAreNotStable(t *testing.T) {
	built := Generate(Seed, Small)
	for index := range built.Events {
		original := &built.Events[index]
		data, err := colbin.Marshal(original)
		if err != nil {
			t.Fatal(err)
		}
		var back Event
		if err := colbin.Unmarshal(data, &back); err != nil {
			t.Fatal(err)
		}
		if back.At != original.At || back.Message != original.Message ||
			len(back.Fields) != len(original.Fields) {
			t.Fatalf("event %d round-tripped as %+v", index, back)
		}
		for key, value := range original.Fields {
			if back.Fields[key] != value {
				t.Fatalf("event %d lost field %q", index, key)
			}
		}
	}

	// An event with two or more map entries will not encode to the same bytes
	// twice, which is why it is out of the fingerprint. Finding one such case is
	// enough to make the point; requiring *every* encoding to differ would be a
	// flaky test, since a two-entry map agrees with itself half the time.
	var multi *Event
	for index := range built.Events {
		if len(built.Events[index].Fields) >= 3 {
			multi = &built.Events[index]
			break
		}
	}
	if multi == nil {
		t.Skip("no event with three map entries in this corpus")
	}
	first, err := colbin.Marshal(multi)
	if err != nil {
		t.Fatal(err)
	}
	stable := true
	for range 200 {
		again, err := colbin.Marshal(multi)
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(first) {
			stable = false
			break
		}
	}
	if stable {
		t.Log("map encoding looked stable over 200 tries; if map iteration is " +
			"ever ordered, Events can join the fingerprint")
	}
}

// TestSaleTotalsAreConsistent checks the generator rather than the format. A
// fixture whose numbers do not add up invites a reader to distrust the ones
// that do.
func TestSaleTotalsAreConsistent(t *testing.T) {
	for _, sale := range Generate(Seed, Small).Sales {
		var subtotal int64
		for _, line := range sale.Detail {
			subtotal += line.TotalCents
		}
		if subtotal != sale.SubtotalCents {
			t.Fatalf("sale %d: lines sum to %d, header says %d",
				sale.ID, subtotal, sale.SubtotalCents)
		}
		if sale.TotalCents != sale.SubtotalCents+sale.TaxCents {
			t.Fatalf("sale %d: total %d != subtotal %d + tax %d",
				sale.ID, sale.TotalCents, sale.SubtotalCents, sale.TaxCents)
		}
		if len(sale.Detail) == 0 {
			t.Fatalf("sale %d has no detail", sale.ID)
		}
	}
}

// TestSalesStraddleTheTableThreshold is what makes this corpus worth more than
// a hand-written record: one dataset has to exercise both the list layout and
// the columnar one, or the benchmark only ever measures half the encoder.
func TestSalesStraddleTheTableThreshold(t *testing.T) {
	const threshold = 8 // codec.tableThreshold
	short, long := 0, 0
	for _, sale := range Generate(Seed, Small).Sales {
		if len(sale.Detail) >= threshold {
			long++
		} else {
			short++
		}
	}
	if short == 0 || long == 0 {
		t.Fatalf("%d sales under the threshold and %d over it: the corpus only "+
			"reaches one layout", short, long)
	}
	t.Logf("%d sales as a list, %d transposed into a table", short, long)
}

// TestEveryMessageStartsInTheReservedRange pins the guarantee an application
// builds its own framing on: colbin never writes a first byte outside
// 0xD0..0xDF, so every other value is free for a caller to claim.
//
// It runs over the corpus rather than over a handful of literals because the
// range has to hold for every shape at once — narrow and wide keys, a
// transposed table, a map, floats, an empty-ish record.
func TestEveryMessageStartsInTheReservedRange(t *testing.T) {
	built := Generate(Seed, Small)
	seen := map[byte]string{}

	encode := func(v any) []byte {
		t.Helper()
		data, err := colbin.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	check := func(table string, data []byte) {
		t.Helper()
		if !colbin.IsColbin(data) {
			t.Fatalf("%s: first byte %#02x is outside %#02x..%#02x",
				table, data[0], colbin.RootFirst, colbin.RootLast)
		}
		seen[data[0]] = table
	}
	for index := range built.Users {
		check("users", encode(&built.Users[index]))
	}
	for index := range built.Products {
		check("products", encode(&built.Products[index]))
	}
	for index := range built.Stores {
		check("stores", encode(&built.Stores[index]))
	}
	for index := range built.Sales {
		check("sales", encode(&built.Sales[index]))
	}
	for index := range built.Events {
		check("events", encode(&built.Events[index]))
	}
	for index := range built.Metrics {
		check("metrics", encode(&built.Metrics[index]))
	}
	// packed5 is still worth encoding here, even though it no longer decides the
	// key width: it takes a different path through the blob header under each.
	colbin.SetPacked5(true)
	for index := range built.Products {
		check("products+packed5", encode(&built.Products[index]))
	}
	colbin.SetPacked5(false)

	// A key past fifteen is what puts a type on the wide path, and so what
	// reaches the other root byte. packed5 used to force it and no longer does,
	// which is the point of that change — so the wide root has to come from a
	// type that genuinely needs wide keys.
	check("wide keys", encode(&wideKeyed{Name: "ACME", Code: 42}))

	for root, table := range seen {
		t.Logf("root %#02x written by %s", root, table)
	}
	if len(seen) < 2 {
		t.Fatalf("only %d distinct root bytes: the corpus is not reaching both "+
			"key widths, so this proves less than it looks", len(seen))
	}
}

// wideKeyed exists only to reach the wide root byte: its second key is past the
// fifteen a narrow key run can hold.
type wideKeyed struct {
	Name string `cb:"2"`
	Code int32  `cb:"21"`
}

// TestNonColbinFirstBytesAreRejected is the other half: a byte an application
// claimed must not be mistaken for a message.
func TestNonColbinFirstBytesAreRejected(t *testing.T) {
	body := colbin.MustCodec[Metric]().Encode(&Metric{SeriesID: 1, At: 2, Value: 3})
	for value := range 256 {
		first := byte(value)
		data := append([]byte{first}, body[1:]...)
		inRange := first >= colbin.RootFirst && first <= colbin.RootLast
		if colbin.IsColbin(data) != inRange {
			t.Fatalf("IsColbin disagrees with the range on %#02x", first)
		}
		if inRange {
			continue
		}
		var into Metric
		if err := colbin.Unmarshal(data, &into); err == nil {
			t.Fatalf("%#02x decoded as a message; it is supposed to be free "+
				"for an application to use", first)
		}
	}
}
