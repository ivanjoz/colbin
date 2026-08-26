package comparison

import (
	"fmt"
	"math/rand"

	"github.com/brianvoe/gofakeit/v7"
)

// ExampleTypeCount is the number of concrete domain models in examples.proto.
// BenchmarkCorpus is only the repeated-field wrapper around these models.
const ExampleTypeCount = 21

// GenerateCorpus builds count values of every example type. Both faker and the
// numeric source are locally seeded, making comparisons reproducible and safe to
// run in parallel with unrelated tests that use package-level randomness.
func GenerateCorpus(seed uint64, count int) *BenchmarkCorpus {
	if count < 0 {
		panic("comparison: negative corpus count")
	}
	g := &corpusGenerator{
		fake: gofakeit.New(seed),
		rng:  rand.New(rand.NewSource(int64(seed))),
	}
	c := &BenchmarkCorpus{}
	for i := range count {
		c.Addresses = append(c.Addresses, g.address(i))
		c.People = append(c.People, g.person(i))
		c.Companies = append(c.Companies, g.company(i))
		c.Products = append(c.Products, g.product(i))
		c.OrderLines = append(c.OrderLines, g.orderLine(i))
		c.Orders = append(c.Orders, g.order(i))
		c.Invoices = append(c.Invoices, g.invoice(i))
		c.SensorReadings = append(c.SensorReadings, g.sensorReading(i))
		c.WeatherSamples = append(c.WeatherSamples, g.weatherSample(i))
		c.GeoPoints = append(c.GeoPoints, g.geoPoint(i))
		c.Routes = append(c.Routes, g.route(i))
		c.BlogPosts = append(c.BlogPosts, g.blogPost(i))
		c.Comments = append(c.Comments, g.comment(i))
		c.UserProfiles = append(c.UserProfiles, g.userProfile(i))
		c.GamePlayers = append(c.GamePlayers, g.gamePlayer(i))
		c.GameMatches = append(c.GameMatches, g.gameMatch(i))
		c.MetricPoints = append(c.MetricPoints, g.metricPoint(i, 0))
		c.MetricSeries = append(c.MetricSeries, g.metricSeries(i))
		c.LogEvents = append(c.LogEvents, g.logEvent(i))
		c.PortfolioPositions = append(c.PortfolioPositions, g.portfolioPosition(i, 0))
		c.Portfolios = append(c.Portfolios, g.portfolio(i))
	}
	return c
}

type corpusGenerator struct {
	fake *gofakeit.Faker
	rng  *rand.Rand
}

func (g *corpusGenerator) address(i int) *Address {
	return &Address{
		Street:     g.fake.Street(),
		City:       g.fake.City(),
		Country:    g.fake.Country(),
		PostalCode: fmt.Sprintf("%05d", 10000+(i*37)%90000),
		Latitude:   -80 + g.rng.Float64()*160,
		Longitude:  -170 + g.rng.Float64()*340,
	}
}

func (g *corpusGenerator) person(i int) *Person {
	var address *Address
	if i%7 != 0 { // exercise nullable nested messages as well as populated ones
		address = g.address(i)
	}
	emails := make([]string, 1+i%3)
	for j := range emails {
		emails[j] = g.fake.Email()
	}
	var avatar []byte
	if i%5 != 0 {
		avatar = g.bytes(16)
	}
	return &Person{
		Id:         100_000 + uint64(i),
		Name:       g.fake.Name(),
		Age:        uint32(18 + i%73),
		Active:     i%4 != 0,
		Emails:     emails,
		Address:    address,
		AvatarHash: avatar,
	}
}

func (g *corpusGenerator) company(i int) *Company {
	employees := make([]*Person, 2+i%4)
	for j := range employees {
		employees[j] = g.person(i*10 + j)
	}
	return &Company{
		Id:        20_000 + uint64(i),
		Name:      g.fake.Company(),
		Industry:  g.fake.JobTitle(),
		Employees: employees,
		Labels: map[string]string{
			"region": g.fake.Country(),
			"tier":   []string{"startup", "growth", "enterprise"}[i%3],
		},
	}
}

func (g *corpusGenerator) product(i int) *Product {
	return &Product{
		Id:         500_000 + uint64(i),
		Sku:        fmt.Sprintf("SKU-%06d", i),
		Name:       g.fake.ProductName(),
		PriceCents: int64(199 + (i*7919)%250_000),
		Stock:      uint32((i * 17) % 500),
		Categories: []string{g.fake.Word(), g.fake.Word()},
		Attributes: map[string]string{
			"color": g.fake.Color(),
			"size":  []string{"small", "medium", "large"}[i%3],
		},
	}
}

func (g *corpusGenerator) orderLine(i int) *OrderLine {
	price := int64(499 + (i*1543)%100_000)
	return &OrderLine{
		Product:        g.product(i),
		Quantity:       uint32(1 + i%8),
		UnitPriceCents: price,
		DiscountBps:    []int32{0, int32((i % 5) * 100), -int32((i % 3) * 25)},
	}
}

func (g *corpusGenerator) order(i int) *Order {
	lines := make([]*OrderLine, 1+i%5)
	for j := range lines {
		lines[j] = g.orderLine(i*10 + j)
	}
	return &Order{
		Id:              900_000 + uint64(i),
		Customer:        g.person(i),
		Lines:           lines,
		ShippingAddress: g.address(i + 1000),
		Status:          []string{"pending", "paid", "shipped", "delivered"}[i%4],
		CreatedUnix:     1_735_689_600 + int64(i*61),
	}
}

func (g *corpusGenerator) invoice(i int) *Invoice {
	order := g.order(i)
	var subtotal int64
	for _, line := range order.Lines {
		subtotal += int64(line.Quantity) * line.UnitPriceCents
	}
	tax := subtotal * 18 / 100
	return &Invoice{
		Number:        fmt.Sprintf("INV-%08d", i),
		Order:         order,
		SubtotalCents: subtotal,
		TaxCents:      tax,
		TotalCents:    subtotal + tax,
		Paid:          i%3 != 0,
		Notes:         []string{g.fake.Sentence(), g.fake.Sentence()},
	}
}

func (g *corpusGenerator) sensorReading(i int) *SensorReading {
	samples := make([]int32, 8+i%9)
	for j := range samples {
		samples[j] = int32((i*13+j*7)%2000 - 1000)
	}
	return &SensorReading{
		SensorId:        70_000 + uint64(i%64),
		TimestampUnixMs: 1_735_689_600_000 + int64(i*250),
		Value:           -20 + g.rng.Float64()*120,
		Unit:            []string{"celsius", "percent", "kPa", "rpm"}[i%4],
		Samples:         samples,
		Valid:           i%11 != 0,
	}
}

func (g *corpusGenerator) weatherSample(i int) *WeatherSample {
	sensors := make([]*SensorReading, 2+i%3)
	for j := range sensors {
		sensors[j] = g.sensorReading(i*10 + j)
	}
	return &WeatherSample{
		Station:         fmt.Sprintf("station-%03d", i%40),
		TimestampUnix:   1_735_689_600 + int64(i*300),
		TemperatureC:    float32(-10 + g.rng.Float64()*45),
		HumidityPercent: float32(20 + g.rng.Float64()*80),
		WindKph:         float32(g.rng.Float64() * 90),
		Sensors:         sensors,
	}
}

func (g *corpusGenerator) geoPoint(i int) *GeoPoint {
	return &GeoPoint{
		Latitude:   -16.4 + float64(i%100)*0.001,
		Longitude:  -71.5 + float64(i%100)*0.001,
		ElevationM: int32(2000 + i%3000),
		Label:      g.fake.City(),
	}
}

func (g *corpusGenerator) route(i int) *Route {
	points := make([]*GeoPoint, 4+i%8)
	durations := make([]uint32, len(points)-1)
	for j := range points {
		points[j] = g.geoPoint(i*20 + j)
		if j < len(durations) {
			durations[j] = uint32(30 + (i+j)*17%600)
		}
	}
	return &Route{
		Id:                  300_000 + uint64(i),
		Name:                g.fake.City() + " route",
		Points:              points,
		SegmentDurationsSec: durations,
		Toll:                i%5 == 0,
	}
}

func (g *corpusGenerator) blogPost(i int) *BlogPost {
	return &BlogPost{
		Id:            400_000 + uint64(i),
		Author:        g.person(i),
		Title:         g.fake.Sentence(),
		Body:          g.fake.Paragraph(),
		Tags:          []string{g.fake.Word(), g.fake.Word(), g.fake.Word()},
		PublishedUnix: 1_735_689_600 + int64(i*3600),
	}
}

func (g *corpusGenerator) comment(i int) *Comment {
	return &Comment{
		Id:        600_000 + uint64(i),
		PostId:    400_000 + uint64(i%100),
		Author:    g.person(i),
		Body:      g.fake.Sentence(),
		ReplyIds:  []uint64{600_001 + uint64(i), 600_002 + uint64(i)},
		Moderated: i%13 == 0,
	}
}

func (g *corpusGenerator) userProfile(i int) *UserProfile {
	return &UserProfile{
		Person:    g.person(i),
		Biography: g.fake.Paragraph(),
		Interests: []string{g.fake.Word(), g.fake.Word(), g.fake.Word()},
		Preferences: map[string]string{
			"language": []string{"en", "es", "pt"}[i%3],
			"theme":    []string{"light", "dark"}[i%2],
		},
		PreviousAddresses: []*Address{g.address(i + 2000), g.address(i + 3000)},
	}
}

func (g *corpusGenerator) gamePlayer(i int) *GamePlayer {
	return &GamePlayer{
		Id:           800_000 + uint64(i),
		Handle:       g.fake.Username(),
		Rating:       int32(800 + i%2200),
		RecentScores: []int32{int32(i % 100), int32((i + 7) % 100), -int32(i % 5)},
		Inventory: map[string]uint32{
			"coins":  uint32(i * 31 % 10_000),
			"boosts": uint32(i % 12),
		},
	}
}

func (g *corpusGenerator) gameMatch(i int) *GameMatch {
	players := make([]*GamePlayer, 2+i%4)
	for j := range players {
		players[j] = g.gamePlayer(i*10 + j)
	}
	return &GameMatch{
		Id:               850_000 + uint64(i),
		Players:          players,
		RoundDurationsMs: []uint32{61_000 + uint32(i%1000), 58_000, 64_500},
		WinnerId:         players[i%len(players)].Id,
		MapName:          g.fake.City(),
	}
}

func (g *corpusGenerator) metricPoint(i, offset int) *MetricPoint {
	return &MetricPoint{
		TimestampUnixMs: 1_735_689_600_000 + int64(i*10_000+offset*1000),
		Value:           20 + g.rng.Float64()*80,
		Annotations:     []string{[]string{"normal", "warning", "recovered"}[(i+offset)%3]},
	}
}

func (g *corpusGenerator) metricSeries(i int) *MetricSeries {
	points := make([]*MetricPoint, 6+i%7)
	for j := range points {
		points[j] = g.metricPoint(i, j)
	}
	return &MetricSeries{
		Name: g.fake.Word() + ".latency",
		Dimensions: map[string]string{
			"host":   fmt.Sprintf("node-%03d", i%64),
			"region": []string{"us-east", "eu-west", "sa-east"}[i%3],
		},
		Points: points,
		Unit:   "milliseconds",
	}
}

func (g *corpusGenerator) logEvent(i int) *LogEvent {
	return &LogEvent{
		TimestampUnixMs: 1_735_689_600_000 + int64(i*25),
		Level:           []string{"debug", "info", "warning", "error"}[i%4],
		Message:         g.fake.Sentence(),
		Fields: map[string]string{
			"service": g.fake.Word(),
			"host":    fmt.Sprintf("node-%03d", i%64),
		},
		TraceId:     g.bytes(16),
		StackFrames: []string{"main.run", "worker.process", "storage.write"},
	}
}

func (g *corpusGenerator) portfolioPosition(i, offset int) *PortfolioPosition {
	n := i*7 + offset
	return &PortfolioPosition{
		Symbol:          []string{"AAPL", "AMZN", "GOOG", "MSFT", "NVDA", "TSLA"}[n%6],
		QuantityMicros:  int64(1_000_000 + n*125_000),
		CostBasisMicros: int64(50_000_000 + n*75_000),
		MarketPrice:     50 + g.rng.Float64()*450,
		DailyPnlMicros:  []int64{int64(n * 1000), -int64(n * 370), int64(n * 125)},
	}
}

func (g *corpusGenerator) portfolio(i int) *Portfolio {
	positions := make([]*PortfolioPosition, 3+i%5)
	for j := range positions {
		positions[j] = g.portfolioPosition(i, j)
	}
	return &Portfolio{
		AccountId: 1_000_000 + uint64(i),
		Owner:     g.person(i),
		Positions: positions,
		CashMicros: map[string]int64{
			"USD": int64(1_000_000_000 + i*50_000),
			"EUR": int64(500_000_000 + i*25_000),
		},
		UpdatedUnix: 1_735_689_600 + int64(i*60),
	}
}

func (g *corpusGenerator) bytes(n int) []byte {
	b := make([]byte, n)
	_, _ = g.rng.Read(b)
	return b
}
