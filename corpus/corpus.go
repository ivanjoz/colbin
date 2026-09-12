// Package corpus generates a reproducible, real-shaped dataset for benchmarks,
// size comparisons and cross-language vectors.
//
// # Why it exists
//
// The benchmarks used to run on three hand-written literals: one sensor
// reading, one order, one product. That is enough to compare framing overhead
// and nothing else. It cannot show what a slice of structs costs once it
// crosses into the columnar layout, what a realistic mix of small and large
// integers does to the varint, or what a million rows weigh — which are the
// questions the format was designed around.
//
// # Reproducible
//
// Everything comes from `rand.New(rand.NewSource(seed))`, whose sequence is
// fixed for a given seed. The same seed and scale give byte-identical tables on
// any platform, which is what lets a golden fingerprint catch an accidental
// change to the generator or to the wire.
//
// One exception, and it is the wire's rather than the generator's: a Go map has
// no iteration order, so a type with a map field does not encode to stable
// bytes. Event is the only such table and it is excluded from the fingerprint.
// See TestFingerprint.
//
// # Real-shaped
//
// The point is not plausible-looking strings, it is plausible *distributions* —
// the things that decide how many bytes a field takes:
//
//   - money is integer cents, never a float, and clusters on prices that end in
//     99 or 00 rather than spreading uniformly;
//   - quantities are small and long-tailed: mostly one or two, occasionally a
//     pallet;
//   - ids are dense and ascending, which is what the column codec's delta
//     transform is for;
//   - timestamps sit inside one year and lean on working hours;
//   - a sale's line count straddles the table threshold deliberately, so both
//     the list and the columnar layout are exercised by one dataset.
//
// # The tables
//
//	Users       strings and small integers — the ordinary flat record
//	Products    strings, a string array, uppercase SKUs — the packed5 case
//	Categories  a tiny lookup table
//	Stores      floats, which nothing else here has
//	Sales       integers only, with Detail []SaleLine nested inside
//	Events      strings and a map — the awkward case, and the unstable one
//	Metrics     three integers and nothing else — the columnar case
package corpus

import (
	"fmt"
	"math/rand"
)

// User is the ordinary flat record: a few small integers and a few short
// strings, which is the shape most messages on a wire actually are.
type User struct {
	ID        uint32 `cb:"1"`
	Name      string `cb:"2"`
	Email     string `cb:"3"`
	Country   string `cb:"4"` // an ISO code, upper case, so packed5 can reach it
	Age       uint8  `cb:"5"`
	Active    bool   `cb:"6"`
	CreatedAt int64  `cb:"7"` // unix seconds
}

// Product carries the strings. The SKU is upper-case alphanumeric on purpose:
// it is what packed5 is for, and what decides whether packed5 is worth its key
// width on a real record.
type Product struct {
	ID         uint32   `cb:"1"`
	SKU        string   `cb:"2"`
	Name       string   `cb:"3"`
	PriceCents int64    `cb:"4"`
	Stock      uint32   `cb:"5"`
	CategoryID uint16   `cb:"6"`
	Tags       []string `cb:"7"`
}

type Category struct {
	ID       uint16 `cb:"1"`
	Name     string `cb:"2"`
	ParentID uint16 `cb:"3"`
}

// Store is the only table with floats in it, so that the float trim is not
// measured only by its unit test.
type Store struct {
	ID   uint16  `cb:"1"`
	Code string  `cb:"2"`
	City string  `cb:"3"`
	Lat  float64 `cb:"4"`
	Lon  float64 `cb:"5"`
}

// Sale holds no string and no float: every field is an integer, and every
// amount is cents. That is deliberate and it is what makes this the interesting
// table — the detail lines are columnable, so a sale long enough to cross
// tableThreshold is transposed into columns rather than written row by row, and
// the same dataset exercises both layouts.
type Sale struct {
	ID            uint64     `cb:"1"`
	UserID        uint32     `cb:"2"`
	StoreID       uint16     `cb:"3"`
	CreatedAt     int64      `cb:"4"`
	SubtotalCents int64      `cb:"5"`
	TaxCents      int64      `cb:"6"`
	TotalCents    int64      `cb:"7"`
	PaidCents     int64      `cb:"8"`
	Detail        []SaleLine `cb:"9"`
}

// SaleLine is every field an integer, which is what makes a slice of them a
// candidate for the columnar layout. A single string here would disqualify the
// whole table.
type SaleLine struct {
	ProductID  uint32 `cb:"1"`
	Quantity   uint32 `cb:"2"`
	UnitCents  int64  `cb:"3"`
	DiscountBP uint16 `cb:"4"` // basis points, 0..10000
	TaxBP      uint16 `cb:"5"`
	TotalCents int64  `cb:"6"`
}

// Event is the awkward one: long strings, a string array and a map. It is the
// table whose encoding is *not* byte-stable, because a Go map has no iteration
// order.
type Event struct {
	At      int64             `cb:"1"`
	Level   string            `cb:"2"`
	Actor   string            `cb:"3"`
	Message string            `cb:"4"`
	Frames  []string          `cb:"5"`
	Fields  map[string]string `cb:"6"`
}

// Metric is three integers, which is the shape the column codec was written
// for: dense ascending ids, clustered timestamps, and values with a narrow
// range.
type Metric struct {
	SeriesID uint32 `cb:"1"`
	At       int64  `cb:"2"`
	Value    int64  `cb:"3"`
}

// Corpus is one generated dataset.
type Corpus struct {
	Users      []User
	Products   []Product
	Categories []Category
	Stores     []Store
	Sales      []Sale
	Events     []Event
	Metrics    []Metric
}

// Scale is how many rows of each table to generate.
type Scale struct {
	Users, Products, Categories, Stores, Sales, Events, Metrics int
	// MaxLines bounds a sale's detail. The distribution below puts most sales
	// well under the table threshold and a minority well over it, so both
	// layouts appear whatever this is set to.
	MaxLines int
}

// Three sizes, because the interesting costs are not linear. Small fits in a
// unit test, Medium is what a benchmark should use, Large is where the columnar
// layout and the block codec actually matter.
var (
	Small = Scale{
		Users: 100, Products: 200, Categories: 12, Stores: 5,
		Sales: 300, Events: 100, Metrics: 2000, MaxLines: 24,
	}
	Medium = Scale{
		Users: 2_000, Products: 5_000, Categories: 40, Stores: 25,
		Sales: 10_000, Events: 2_000, Metrics: 100_000, MaxLines: 32,
	}
	Large = Scale{
		Users: 50_000, Products: 100_000, Categories: 200, Stores: 400,
		Sales: 200_000, Events: 50_000, Metrics: 2_000_000, MaxLines: 40,
	}
)

// Seed is the seed every fixture in this repo uses, so that "the corpus" means
// one dataset rather than a family of them.
const Seed int64 = 20260101

// epoch is the start of the year every timestamp sits in: 2026-01-01 UTC.
const epoch int64 = 1767225600

// Generate builds a corpus. The same seed and scale always give the same rows.
func Generate(seed int64, scale Scale) *Corpus {
	random := rand.New(rand.NewSource(seed))
	built := &Corpus{}
	built.Categories = categories(random, scale.Categories)
	built.Stores = stores(random, scale.Stores)
	built.Users = users(random, scale.Users)
	built.Products = products(random, scale.Products, scale.Categories)
	built.Sales = sales(random, scale, built.Products)
	built.Events = events(random, scale.Events)
	built.Metrics = metrics(random, scale.Metrics)
	return built
}

func categories(random *rand.Rand, count int) []Category {
	out := make([]Category, count)
	for index := range out {
		parent := uint16(0)
		if index > 3 {
			parent = uint16(random.Intn(4) + 1)
		}
		out[index] = Category{
			ID:       uint16(index + 1),
			Name:     pick(random, categoryWords) + " " + pick(random, categoryWords),
			ParentID: parent,
		}
	}
	return out
}

func stores(random *rand.Rand, count int) []Store {
	out := make([]Store, count)
	for index := range out {
		city := pick(random, cities)
		out[index] = Store{
			ID:   uint16(index + 1),
			Code: fmt.Sprintf("%s%03d", upperPrefix(city), index+1),
			City: city,
			// Rounded to five decimals, which is what a real coordinate carries
			// and what leaves the float trim something to do.
			Lat: float64(random.Intn(18000000)-9000000) / 100000,
			Lon: float64(random.Intn(36000000)-18000000) / 100000,
		}
	}
	return out
}

func users(random *rand.Rand, count int) []User {
	out := make([]User, count)
	for index := range out {
		first, last := pick(random, firstNames), pick(random, lastNames)
		out[index] = User{
			ID:      uint32(index + 1),
			Name:    first + " " + last,
			Email:   fmt.Sprintf("%s.%s%d@%s", lower(first), lower(last), index%100, pick(random, domains)),
			Country: pick(random, countries),
			// A real age distribution is not uniform over 0..255.
			Age:    uint8(18 + random.Intn(52)),
			Active: random.Intn(100) < 82,
			// Signups spread over the year, denser later.
			CreatedAt: epoch + int64(random.Intn(365*24*3600)),
		}
	}
	return out
}

func products(random *rand.Rand, count, categoryCount int) []Product {
	out := make([]Product, count)
	for index := range out {
		tags := make([]string, 1+random.Intn(3))
		for tag := range tags {
			tags[tag] = pick(random, tagWords)
		}
		out[index] = Product{
			ID:         uint32(index + 1),
			SKU:        sku(random, index),
			Name:       pick(random, productAdjectives) + " " + pick(random, productNouns),
			PriceCents: price(random),
			// Stock is long-tailed: most lines carry a little, a few carry a lot.
			Stock:      uint32(random.Intn(40) + random.Intn(random.Intn(900)+1)),
			CategoryID: uint16(random.Intn(max(categoryCount, 1)) + 1),
			Tags:       tags,
		}
	}
	return out
}

// sku builds an upper-case alphanumeric code, which is what packed5 is for.
func sku(random *rand.Rand, index int) string {
	return fmt.Sprintf("%s-%s-%04d", pick(random, brandCodes), pick(random, skuMiddles), index%10000)
}

// price clusters where real prices cluster rather than spreading uniformly,
// because the digit pattern is what decides how many bytes the integer takes.
func price(random *rand.Rand) int64 {
	base := []int64{199, 499, 999, 1499, 1999, 2499, 2999, 4999, 7999, 9999,
		12999, 19999, 24999, 49999, 99999}
	cents := base[random.Intn(len(base))]
	if random.Intn(8) == 0 { // the occasional odd price
		cents += int64(random.Intn(400))
	}
	return cents
}

func sales(random *rand.Rand, scale Scale, catalogue []Product) []Sale {
	out := make([]Sale, scale.Sales)
	for index := range out {
		lines := lineCount(random, scale.MaxLines)
		detail := make([]SaleLine, lines)
		var subtotal, tax int64
		for line := range detail {
			product := catalogue[random.Intn(len(catalogue))]
			quantity := quantity(random)
			discount := uint16(0)
			if random.Intn(5) == 0 {
				discount = uint16(random.Intn(4)+1) * 500 // 5%, 10%, 15%, 20%
			}
			taxBP := uint16(2100)
			if random.Intn(6) == 0 {
				taxBP = 1000 // a reduced rate on some goods
			}
			gross := product.PriceCents * int64(quantity)
			net := gross - gross*int64(discount)/10000
			lineTax := net * int64(taxBP) / 10000
			detail[line] = SaleLine{
				ProductID:  product.ID,
				Quantity:   quantity,
				UnitCents:  product.PriceCents,
				DiscountBP: discount,
				TaxBP:      taxBP,
				TotalCents: net,
			}
			subtotal += net
			tax += lineTax
		}
		paid := subtotal + tax
		if random.Intn(50) == 0 { // a few part-paid sales, so PaidCents differs
			paid = paid * int64(20+random.Intn(70)) / 100
		}
		out[index] = Sale{
			ID:            uint64(index + 1),
			UserID:        uint32(random.Intn(max(scale.Users, 1)) + 1),
			StoreID:       uint16(random.Intn(max(scale.Stores, 1)) + 1),
			CreatedAt:     epoch + businessSecond(random),
			SubtotalCents: subtotal,
			TaxCents:      tax,
			TotalCents:    subtotal + tax,
			PaidCents:     paid,
			Detail:        detail,
		}
	}
	return out
}

// lineCount straddles the table threshold on purpose: most sales are a handful
// of lines and stay a list, and a minority are long enough to be transposed.
func lineCount(random *rand.Rand, most int) int {
	switch roll := random.Intn(100); {
	case roll < 62:
		return 1 + random.Intn(4) // 1..4, comfortably a list
	case roll < 88:
		return 5 + random.Intn(4) // 5..8, either side of the threshold
	default:
		return 9 + random.Intn(max(most-8, 1)) // long enough to be a table
	}
}

// quantity is long-tailed: almost always one or two.
func quantity(random *rand.Rand) uint32 {
	switch roll := random.Intn(100); {
	case roll < 64:
		return 1
	case roll < 86:
		return 2
	case roll < 96:
		return uint32(3 + random.Intn(3))
	default:
		return uint32(10 + random.Intn(90))
	}
}

// businessSecond leans on working hours rather than spreading a timestamp
// uniformly over the day, which is what makes a delta-encoded timestamp column
// realistic.
func businessSecond(random *rand.Rand) int64 {
	day := int64(random.Intn(365))
	hour := int64(9 + random.Intn(10))
	if random.Intn(9) == 0 {
		hour = int64(random.Intn(24))
	}
	return day*24*3600 + hour*3600 + int64(random.Intn(3600))
}

func events(random *rand.Rand, count int) []Event {
	out := make([]Event, count)
	for index := range out {
		frames := make([]string, random.Intn(4))
		for frame := range frames {
			frames[frame] = fmt.Sprintf("%s.go:%d", pick(random, sourceFiles), random.Intn(900)+10)
		}
		fields := map[string]string{
			"request_id": fmt.Sprintf("%s-%06d", pick(random, brandCodes), random.Intn(1000000)),
			"service":    pick(random, services),
		}
		if random.Intn(3) == 0 {
			fields["tenant"] = pick(random, countries)
		}
		out[index] = Event{
			At:      epoch + int64(random.Intn(365*24*3600)),
			Level:   pick(random, levels),
			Actor:   pick(random, services),
			Message: pick(random, messages),
			Frames:  frames,
			Fields:  fields,
		}
	}
	return out
}

// metrics is the columnar table: ids repeat in runs, timestamps ascend on a
// fixed step, values wander. Between them they cover the three transforms the
// column codec chooses from.
func metrics(random *rand.Rand, count int) []Metric {
	out := make([]Metric, count)
	at := epoch
	series := uint32(1)
	value := int64(1000)
	for index := range out {
		if index%64 == 0 { // a new series every so often, so the id column is a run
			series = uint32(random.Intn(64) + 1)
		}
		at += int64(10 + random.Intn(4)) // a roughly fixed sampling interval
		value += int64(random.Intn(21) - 10)
		out[index] = Metric{SeriesID: series, At: at, Value: value}
	}
	return out
}

func pick(random *rand.Rand, from []string) string { return from[random.Intn(len(from))] }
