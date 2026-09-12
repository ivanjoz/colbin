package corpus

import "strings"

// The word pools.
//
// They are small on purpose. A corpus built from a huge vocabulary of random
// strings measures the allocator and the memcpy, not the format; real data
// repeats, and repetition is what a length-prefixed blob costs the same for and
// a dictionary would not. Keeping the pools short also keeps this file
// reviewable, which a generated name list would not be.
//
// The upper-case pools — brand codes, SKU middles, country codes, levels — are
// there because packed5 only pays on upper-case alphanumerics, and a corpus
// with no such strings would make it look useless.

var firstNames = []string{
	"Ana", "Bruno", "Carla", "Diego", "Elena", "Felipe", "Gabriela", "Hugo",
	"Irene", "Javier", "Karina", "Lucas", "Marta", "Nicolas", "Olivia", "Pablo",
	"Rocio", "Sergio", "Teresa", "Valeria",
}

var lastNames = []string{
	"Alvarez", "Benitez", "Castro", "Dominguez", "Escobar", "Fuentes", "Gomez",
	"Herrera", "Ibarra", "Jimenez", "Lara", "Molina", "Navarro", "Ortiz",
	"Paredes", "Quiroga", "Ramirez", "Salazar", "Torres", "Vargas",
}

var domains = []string{
	"example.com", "mail.example.net", "corp.example.org", "shop.example.io",
}

// ISO 3166 codes, upper case, which is what packed5 wants.
var countries = []string{
	"AR", "BO", "BR", "CL", "CO", "CR", "EC", "ES", "GT", "MX",
	"PA", "PE", "PT", "PY", "US", "UY", "VE",
}

var cities = []string{
	"Lima", "Bogota", "Santiago", "Buenos Aires", "Montevideo", "Quito",
	"Asuncion", "La Paz", "Madrid", "Lisboa", "Mexico", "Panama",
}

var categoryWords = []string{
	"Hardware", "Fasteners", "Industrial", "Garden", "Kitchen", "Outdoor",
	"Electrical", "Plumbing", "Safety", "Tools", "Paint", "Storage",
}

var productAdjectives = []string{
	"Heavy Duty", "Compact", "Stainless", "Galvanized", "Reinforced",
	"Portable", "Insulated", "Adjustable", "Premium", "Standard",
}

var productNouns = []string{
	"Bracket", "Hinge", "Bolt Set", "Drill Bit", "Hose Reel", "Tool Box",
	"Work Bench", "Ladder", "Clamp", "Socket Wrench", "Tarpaulin", "Padlock",
}

var tagWords = []string{
	"NEW", "SALE", "CLEARANCE", "IMPORTED", "CERTIFIED", "BULK", "FRAGILE",
	"HEAVY", "RETURNABLE", "WARRANTY",
}

// Brand and SKU fragments: upper-case alphanumeric, the packed5 case.
var brandCodes = []string{
	"ACME", "NORTEC", "VULCAN", "MERIDIAN", "ATLAS", "ORION", "KESTREL",
}

var skuMiddles = []string{
	"WDG", "BRK", "HNG", "BLT", "DRL", "RL", "TBX", "WBN", "LDR", "CLP",
}

var levels = []string{"DEBUG", "INFO", "WARN", "ERROR"}

var services = []string{
	"billing", "catalog", "checkout", "inventory", "identity", "shipping",
}

var sourceFiles = []string{
	"handler", "responses", "store", "client", "worker", "router",
}

var messages = []string{
	"could not obtain the record",
	"request completed",
	"upstream timed out, retrying",
	"cache miss, falling through to the store",
	"payment authorised",
	"stock reservation released",
	"validation failed on the request body",
}

// lower is strings.ToLower with the spaces dropped, for building an email out
// of a name.
func lower(name string) string {
	return strings.ReplaceAll(strings.ToLower(name), " ", "")
}

// upperPrefix is the first three letters of a city, upper case, for a store
// code. It walks runes rather than bytes so an accented city does not split.
func upperPrefix(city string) string {
	upper := strings.ToUpper(strings.ReplaceAll(city, " ", ""))
	runes := []rune(upper)
	if len(runes) > 3 {
		runes = runes[:3]
	}
	return string(runes)
}
