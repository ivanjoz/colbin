package packed5

// A measurement, not a guard: what three case schemes would cost on TPC-H and
// on hand-written real-world names, priced by the optimum each format allows so
// the encoder's heuristics are out of the comparison.
//
//	current   27 toggles the next letter, 28 toggles the mode
//	spacetog  27 is a space plus a toggle of the next letter; no bare toggle
//	title-sp  current, plus 28 as the first unit opens title mode: a letter
//	          after a space (or at the start) is inverted for free
//	title-sym title-sp, where a word also starts after - ( / . & " '
//
// Run with
//
//	PACKED5_LINEITEM=/tmp/tp/lineitem.tbl go test ./packed5 -run TestCaseVariants -v

import (
	"bufio"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"testing"
)

type caseVariant int

const (
	varCurrent caseVariant = iota
	varSpaceToggle
	varTitleSpace
	varTitleSym
	numVariants
)

var variantNames = [numVariants]string{"current", "spacetog", "title-sp", "title-sym"}

// variantUnits is the fewest units variant v can spend on s, over both opening
// modes. For the title variants it is the cheaper of plain and title mode, so it
// is never worse than current.
func variantUnits(s string, v caseVariant) int {
	switch v {
	case varTitleSpace, varTitleSym:
		return min(caseDP(s, varCurrent, nil), 1+caseDP(s, v, wordStarts(s, v)))
	}
	return caseDP(s, v, nil)
}

func wordStarts(s string, v caseVariant) []bool {
	seps := " "
	if v == varTitleSym {
		seps = " -(/.&\"'"
	}
	ws := make([]bool, len(s))
	for i := range s {
		ws[i] = i == 0 || strings.IndexByte(seps, s[i-1]) >= 0
	}
	return ws
}

// caseDP is a shortest path over (offset, mode, pending toggle). ws, when set,
// marks the letters title mode inverts.
func caseDP(s string, v caseVariant, ws []bool) int {
	const inf = 1 << 30
	type st = [2][2]int // [mode][pending]
	dist := make([]st, len(s)+1)
	for i := range dist {
		dist[i] = st{{inf, inf}, {inf, inf}}
	}
	dist[0] = st{{0, inf}, {0, inf}}

	for i := range len(s) {
		// Edges within an offset: the long toggle flips the mode, and the bare
		// simple toggle sets pending. Three rounds settle four states.
		bare := v != varSpaceToggle || i == 0
		for range 3 {
			for p := range 2 {
				dist[i][1][p] = min(dist[i][1][p], dist[i][0][p]+1)
				dist[i][0][p] = min(dist[i][0][p], dist[i][1][p]+1)
			}
			if bare {
				for m := range 2 {
					dist[i][m][1] = min(dist[i][m][1], dist[i][m][0]+1)
				}
			}
		}
		for m := range 2 {
			for p := range 2 {
				base := dist[i][m][p]
				if base >= inf {
					continue
				}
				relax := func(to, mode, pend, cost int) {
					dist[to][mode][pend] = min(dist[to][mode][pend], base+cost)
				}
				c := s[i]
				if isLetter(c) {
					want := m == 1
					if ws != nil && ws[i] {
						want = !want
					}
					if p == 1 {
						want = !want
					}
					if isUpper(c) == want {
						relax(i+1, m, 0, unitsLetter)
					}
					continue
				}
				if c == ' ' {
					relax(i+1, m, p, unitsSpace)
					if v == varSpaceToggle && p == 0 {
						relax(i+1, m, 1, unitsSpace)
					}
					continue
				}
				// Everything else is case-blind and leaves pending alone.
				if isDigit(c) {
					n := 0
					for l := 1; l <= numberMaxDigits && i+l <= len(s); l++ {
						if !isDigit(s[i+l-1]) || (l > 1 && s[i] == '0') {
							break
						}
						n = n*10 + int(s[i+l-1]-'0')
						if n > numberMax {
							break
						}
						if l == 1 {
							relax(i+1, m, p, unitsSymbol)
						} else {
							relax(i+l, m, p, unitsNumber)
						}
					}
				} else if c < 0x80 {
					if asciiSym[c] >= 0 {
						relax(i+1, m, p, unitsSymbol)
					}
					if asciiExt[c] >= 0 {
						relax(i+1, m, p, unitsExt)
					}
				} else if k, w := extMulti(s, i); k >= 0 {
					relax(i+w, m, p, unitsExt)
				}
				for n := 1; n <= maxEscapeRun && i+n <= len(s); n++ {
					if !escapes(s, i+n-1) {
						break
					}
					relax(i+n, m, p, unitsEscapeBase+unitsEscapeByte*n)
				}
			}
		}
	}
	e := dist[len(s)]
	return min(e[0][0], e[0][1], e[1][0], e[1][1])
}

// tpchColumns synthesises the string columns of the TPC-H tables other than
// lineitem, following the formats in the spec (section 4.2.2).
func tpchColumns(rows int) map[string][]string {
	rng := rand.New(rand.NewPCG(7, 8))
	pick := func(xs []string) string { return xs[rng.IntN(len(xs))] }
	const alnum = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ,"
	vstr := func(lo, hi int) string {
		b := make([]byte, lo+rng.IntN(hi-lo+1))
		for i := range b {
			b[i] = alnum[rng.IntN(len(alnum))]
		}
		return string(b)
	}
	colors := strings.Fields("almond antique aquamarine azure beige bisque black blanched blue blush brown burlywood burnished chartreuse chiffon chocolate coral cornflower cornsilk cream cyan dark deep dim dodger drab firebrick floral forest frosted gainsboro ghost goldenrod green grey honeydew hot indian ivory khaki lace lavender lawn lemon light lime linen magenta maroon medium metallic midnight mint misty moccasin navajo navy olive orange orchid pale papaya peach peru pink plum powder puff purple red rose rosy royal saddle salmon sandy seashell sienna sky slate smoke snow spring steel tan thistle tomato turquoise violet wheat white yellow")
	nations := strings.Fields("ALGERIA ARGENTINA BRAZIL CANADA EGYPT ETHIOPIA FRANCE GERMANY INDIA INDONESIA IRAN IRAQ JAPAN JORDAN KENYA MOROCCO MOZAMBIQUE PERU CHINA ROMANIA SAUDI_ARABIA VIETNAM RUSSIA UNITED_KINGDOM UNITED_STATES")
	for i, n := range nations {
		nations[i] = strings.ReplaceAll(n, "_", " ")
	}
	t1 := strings.Fields("STANDARD SMALL MEDIUM LARGE ECONOMY PROMO")
	t2 := strings.Fields("ANODIZED BURNISHED PLATED POLISHED BRUSHED")
	t3 := strings.Fields("TIN NICKEL BRASS STEEL COPPER")
	c1 := strings.Fields("SM LG MED JUMBO WRAP")
	c2 := strings.Fields("CASE BOX BAG JAR PKG PACK CAN DRUM")
	seg := strings.Fields("AUTOMOBILE BUILDING FURNITURE MACHINERY HOUSEHOLD")
	prio := []string{"1-URGENT", "2-HIGH", "3-MEDIUM", "4-NOT SPECIFIED", "5-LOW"}

	out := map[string][]string{}
	add := func(col, s string) { out[col] = append(out[col], s) }
	for i := range rows {
		add("c_name", fmt.Sprintf("Customer#%09d", i+1))
		add("s_name", fmt.Sprintf("Supplier#%09d", i+1))
		add("o_clerk", fmt.Sprintf("Clerk#%09d", 1+rng.IntN(1000)))
		add("c_address", vstr(10, 40))
		add("c_phone", fmt.Sprintf("%d-%03d-%03d-%04d", 10+rng.IntN(25), 100+rng.IntN(900), 100+rng.IntN(900), 1000+rng.IntN(9000)))
		add("c_mktsegment", pick(seg))
		add("o_orderpriority", pick(prio))
		m := 1 + rng.IntN(5)
		add("p_mfgr", fmt.Sprintf("Manufacturer#%d", m))
		add("p_brand", fmt.Sprintf("Brand#%d%d", m, 1+rng.IntN(5)))
		add("p_type", pick(t1)+" "+pick(t2)+" "+pick(t3))
		add("p_container", pick(c1)+" "+pick(c2))
		add("p_name", strings.Join([]string{pick(colors), pick(colors), pick(colors), pick(colors), pick(colors)}, " "))
		add("n_name", pick(nations))
	}
	return out
}

// lineitemColumns reads the string columns of a lineitem.tbl.
func lineitemColumns(t *testing.T, path string) map[string][]string {
	f, err := os.Open(path)
	if err != nil {
		t.Skipf("no lineitem: %v", err)
	}
	defer f.Close()
	cols := map[int]string{8: "l_returnflag", 9: "l_linestatus", 13: "l_shipinstruct", 14: "l_shipmode", 15: "l_comment"}
	out := map[string][]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Split(sc.Text(), "|")
		for i, name := range cols {
			out[name] = append(out[name], fs[i])
		}
	}
	return out
}

// realWorld is hand-written: people, companies, brands, places, addresses and
// product descriptions as they appear in ERP and CRM tables.
var realWorld = map[string][]string{
	"people": {
		"Pedro Juan", "María José Pérez", "Juan Carlos Rodríguez", "Ana Lucía Torres",
		"José Luis Gonzales Vargas", "Rosa Elena Quispe", "Luis Alberto Mendoza", "Carmen Rosa Flores",
		"Ivan Angulo", "Pedro de la Cruz", "Juan Pablo del Castillo", "Sofía Ramírez",
		"Diego Fernández", "Valentina Gómez", "Andrés Felipe Martínez", "Camila Herrera",
		"Patrick O'Brien", "Ronald McDonald", "Mary-Jane Watson", "Jean-Luc Picard",
		"Ana María Fuentes-Rivas", "Carlos Eduardo Silva", "Lucía Morales", "Jorge Chávez",
		"John Smith", "Robert Johnson", "Michael Williams", "Elizabeth Brown", "William Davis",
		"Jennifer Garcia", "David Miller", "Susan Wilson", "Thomas Moore", "Karen Taylor",
	},
	"companies": {
		"Hortifrut S.A.", "Hortifrut Chile S.A.", "Agrícola Santa Rosa S.A.C.", "Inversiones La Pampa SRL",
		"Banco de Crédito del Perú", "Supermercados Peruanos S.A.", "Cencosud Retail",
		"Walmart Inc.", "Costco Wholesale", "Amazon Web Services", "Microsoft Corporation",
		"Procter & Gamble", "Johnson & Johnson", "The Coca-Cola Company", "PepsiCo Inc.",
		"Driscoll's", "Naturipe Farms", "Sun Belle Inc.", "Transportes Del Norte EIRL",
		"Distribuidora Andina", "Grupo Romero", "Alicorp SAA", "Backus y Johnston",
		"IBM", "SAP SE", "AT&T Inc.", "3M Company", "eBay Inc.", "iRobot",
	},
	"brands": {
		"Coca Cola", "Inca Kola", "Gloria", "Nestlé", "Sublime", "Doritos", "Lay's",
		"Red Bull", "Head & Shoulders", "Colgate Total", "Nike Air Max", "Adidas Originals",
		"Samsung Galaxy S24", "iPhone 15 Pro", "MacBook Air", "PlayStation 5", "Xbox Series X",
		"Toyota Hilux", "Hyundai Tucson", "Mercedes-Benz", "Louis Vuitton", "Calvin Klein",
	},
	"places": {
		"Lima", "San Isidro", "Miraflores", "Santiago de Chile", "Buenos Aires", "Ciudad de México",
		"Bogotá", "Trujillo", "La Libertad", "Arequipa", "Piura", "San Francisco", "New York",
		"Los Angeles", "Rio de Janeiro", "Sao Paulo", "Puerto Montt", "Valle del Elqui",
		"Viña del Mar", "Chiclayo", "Ica", "Huaral", "Rancagua", "Michoacán",
	},
	"addresses": {
		"Av. Javier Prado Este 1234", "Calle Los Pinos 456", "Jr. de la Unión 789",
		"Av. Larco 1301 Of. 402", "Av. Apoquindo 3000 Piso 12", "Calle Las Begonias 415",
		"1600 Pennsylvania Avenue NW", "350 Fifth Avenue", "221B Baker Street",
		"Carretera Panamericana Norte Km 512", "Fundo Santa Rosa s/n", "Mz. B Lt. 14 Urb. Los Álamos",
	},
	"products": {
		"Arándano Fresco 125g", "Blueberry Clamshell 6oz", "Frambuesa Orgánica", "Mora Congelada IQF",
		"Caja Exportación 12x125g", "Leche Evaporada Gloria", "Aceite Vegetal Primor 1L",
		"Detergente Ariel Power", "Papel Higiénico Elite Doble Hoja", "Arroz Costeño Extra",
		"Galletas Soda Field", "Cerveza Pilsen Callao", "Agua San Luis Sin Gas 625ml",
	},
}

func TestCaseVariants(t *testing.T) {
	type group struct {
		name string
		cols map[string][]string
	}
	groups := []group{{"tpch-synth", tpchColumns(20000)}, {"real-world", realWorld}}
	if path := os.Getenv("PACKED5_LINEITEM"); path != "" {
		groups = append([]group{{"lineitem", lineitemColumns(t, path)}}, groups...)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "\n%-12s %-16s %8s %9s", "group", "column", "strings", "raw")
	for _, n := range variantNames {
		fmt.Fprintf(&sb, " %10s", n)
	}
	sb.WriteString("   (stored bytes; +x% vs current)\n")
	for _, g := range groups {
		var gRaw int
		var gTot [numVariants]int
		names := make([]string, 0, len(g.cols))
		for n := range g.cols {
			names = append(names, n)
		}
		slices.Sort(names)
		for _, col := range names {
			var raw int
			var tot [numVariants]int
			for _, s := range g.cols[col] {
				raw += len(s)
				for v := range numVariants {
					tot[v] += min(payloadBytes(variantUnits(s, v)), len(s))
				}
			}
			row(&sb, g.name, col, len(g.cols[col]), raw, tot)
			gRaw += raw
			for v := range numVariants {
				gTot[v] += tot[v]
			}
		}
		row(&sb, g.name, "TOTAL", 0, gRaw, gTot)
		sb.WriteString("\n")
	}
	t.Log(sb.String())

	// The current-variant DP must agree with the oracle encode_test.go keeps.
	for _, g := range groups {
		for _, ss := range g.cols {
			for _, s := range ss[:min(len(ss), 2000)] {
				if a, b := variantUnits(s, varCurrent), optimalUnits(s); a != b {
					t.Fatalf("%q: dp %d, oracle %d", s, a, b)
				}
			}
		}
	}

	// Strings where a variant differs from current, to see what drives it.
	for _, s := range realWorld["people"][:8] {
		t.Logf("%-28q cur=%d spacetog=%d title-sp=%d title-sym=%d", s,
			variantUnits(s, varCurrent), variantUnits(s, varSpaceToggle),
			variantUnits(s, varTitleSpace), variantUnits(s, varTitleSym))
	}
}

func row(sb *strings.Builder, g, col string, n, raw int, tot [numVariants]int) {
	fmt.Fprintf(sb, "%-12s %-16s %8d %9d", g, col, n, raw)
	for v := range numVariants {
		if v == 0 {
			fmt.Fprintf(sb, " %10d", tot[v])
			continue
		}
		fmt.Fprintf(sb, " %+9.2f%%", 100*float64(tot[v]-tot[0])/float64(tot[0]))
	}
	sb.WriteString("\n")
}
