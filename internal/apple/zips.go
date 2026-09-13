package apple

// SeedZips are the anchor points for a nationwide sweep.
//
// Apple's pickup endpoint only answers "the ~12 nearest stores to this postal
// code", so covering every US Apple Store means querying from enough anchors
// that their 12-store neighbourhoods overlap.
//
// The order matters as much as the contents. Apple will often cut a sweep
// short, so the anchors are listed in rough order of how many stores each one
// reaches: the densest metros first, secondary cities next, thinly served
// regions last. A sweep that only gets thirty requests through still comes back
// with a fair picture of the country rather than a very detailed picture of
// California.
var SeedZips = []string{
	// The largest store clusters: one truncated sweep should still cover these.
	"10001", // New York City
	"90210", // Los Angeles — west
	"60601", // Chicago
	"94105", // San Francisco
	"75201", // Dallas
	"77002", // Houston
	"33101", // Miami
	"20001", // Washington DC
	"02108", // Boston
	"19103", // Philadelphia
	"30303", // Atlanta
	"98101", // Seattle
	"85251", // Phoenix — Scottsdale
	"92660", // Orange County
	"95129", // San Jose
	"92101", // San Diego
	"80202", // Denver
	"48226", // Detroit
	"55402", // Minneapolis
	"07030", // Northern New Jersey
	"33301", // Fort Lauderdale
	"89109", // Las Vegas
	"78701", // Austin
	"28202", // Charlotte
	"97201", // Portland
	"84101", // Salt Lake City
	"63101", // St. Louis
	"44114", // Cleveland
	"43215", // Columbus
	"37203", // Nashville

	// Secondary metros.
	"11530", // Long Island
	"10601", // Westchester
	"91311", // Los Angeles — San Fernando Valley
	"90802", // Long Beach
	"91101", // Pasadena
	"94301", // Palo Alto
	"94596", // East Bay
	"92507", // Inland Empire
	"78205", // San Antonio
	"76102", // Fort Worth
	"32801", // Orlando
	"33602", // Tampa
	"21201", // Baltimore
	"23219", // Richmond
	"15222", // Pittsburgh
	"27601", // Raleigh
	"53202", // Milwaukee
	"46204", // Indianapolis
	"45202", // Cincinnati
	"64111", // Kansas City
	"85712", // Tucson
	"95814", // Sacramento
	"98684", // Vancouver WA
	"06103", // Hartford
	"08540", // Central New Jersey
	"32218", // Jacksonville
	"38103", // Memphis
	"35203", // Birmingham
	"70112", // New Orleans
	"73102", // Oklahoma City

	// Smaller markets and thinly served regions.
	"29601", // Greenville SC
	"29401", // Charleston SC
	"40202", // Louisville
	"72201", // Little Rock
	"74103", // Tulsa
	"68102", // Omaha
	"50309", // Des Moines
	"67202", // Wichita
	"87110", // Albuquerque
	"89501", // Reno
	"83702", // Boise
	"99201", // Spokane
	"80906", // Colorado Springs
	"93701", // Fresno
	"93101", // Santa Barbara
	"93536", // Antelope Valley
	"92262", // Palm Springs
	"79901", // El Paso
	"79424", // Lubbock
	"34102", // Naples
	"12207", // Albany
	"14202", // Buffalo
	"13202", // Syracuse
	"19801", // Wilmington DE
	"03060", // Southern New Hampshire
	"04101", // Portland ME
	"59101", // Billings
	"96814", // Honolulu
}
