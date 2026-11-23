// Package gststate maps an Indian address to its GST state code, so TCS can be
// split into CGST+SGST (venue in the host's GSTIN state) or IGST (elsewhere).
// Mirrors Myslotmate-backend-node/src/lib/tax/gst-state.ts.
package gststate

import (
	"regexp"
	"strings"
)

// names maps state / UT names (current and common older spellings) to GST codes.
var names = map[string]string{
	"jammu and kashmir": "01", "jammu & kashmir": "01",
	"himachal pradesh": "02",
	"punjab":           "03",
	"chandigarh":       "04",
	"uttarakhand":      "05", "uttaranchal": "05",
	"haryana": "06",
	"delhi":   "07", "national capital territory of delhi": "07",
	"rajasthan":         "08",
	"uttar pradesh":     "09",
	"bihar":             "10",
	"sikkim":            "11",
	"arunachal pradesh": "12",
	"nagaland":          "13",
	"manipur":           "14",
	"mizoram":           "15",
	"tripura":           "16",
	"meghalaya":         "17",
	"assam":             "18",
	"west bengal":       "19",
	"jharkhand":         "20",
	"odisha":            "21", "orissa": "21",
	"chhattisgarh":   "22",
	"madhya pradesh": "23",
	"gujarat":        "24",
	"dadra and nagar haveli and daman and diu": "26", "dadra and nagar haveli": "26", "daman and diu": "26",
	"maharashtra": "27",
	"karnataka":   "29",
	"goa":         "30",
	"lakshadweep": "31",
	"kerala":      "32",
	"tamil nadu":  "33",
	"puducherry":  "34", "pondicherry": "34",
	"andaman and nicobar islands": "35", "andaman and nicobar": "35",
	"telangana":      "36",
	"andhra pradesh": "37",
	"ladakh":         "38",
}

var pattern = func() *regexp.Regexp {
	keys := make([]string, 0, len(names))
	for k := range names {
		keys = append(keys, regexp.QuoteMeta(k))
	}
	// Longest first, so "dadra and nagar haveli and daman and diu" wins over
	// its parts at the same position.
	for i := range keys {
		for j := i + 1; j < len(keys); j++ {
			if len(keys[j]) > len(keys[i]) {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return regexp.MustCompile(`(?i)\b(` + strings.Join(keys, "|") + `)\b`)
}()

// FromAddress returns the GST state code of the last state named in the
// address (addresses end with the state, so "Delhi Gate, Agra, Uttar Pradesh"
// is Uttar Pradesh), or "" when none is found.
func FromAddress(addr string) string {
	matches := pattern.FindAllString(addr, -1)
	if len(matches) == 0 {
		if m := cityPattern.FindString(addr); m != "" {
			return cities[strings.ToLower(m)]
		}
		return ""
	}
	return names[strings.ToLower(matches[len(matches)-1])]
}

// cities backs up addresses that name a city but no state. Only used when no
// state name is found. ponytail: Guwahati/Dispur only (where hosts are); add
// cities as venues spread.
var cities = map[string]string{"guwahati": "18", "dispur": "18"}

var cityPattern = regexp.MustCompile(`(?i)\b(guwahati|dispur)\b`)

// canonical is the printed name of each GST state code.
var canonical = map[string]string{
	"01": "Jammu and Kashmir", "02": "Himachal Pradesh", "03": "Punjab", "04": "Chandigarh",
	"05": "Uttarakhand", "06": "Haryana", "07": "Delhi", "08": "Rajasthan", "09": "Uttar Pradesh",
	"10": "Bihar", "11": "Sikkim", "12": "Arunachal Pradesh", "13": "Nagaland", "14": "Manipur",
	"15": "Mizoram", "16": "Tripura", "17": "Meghalaya", "18": "Assam", "19": "West Bengal",
	"20": "Jharkhand", "21": "Odisha", "22": "Chhattisgarh", "23": "Madhya Pradesh", "24": "Gujarat",
	"26": "Dadra and Nagar Haveli and Daman and Diu", "27": "Maharashtra", "29": "Karnataka", "30": "Goa",
	"31": "Lakshadweep", "32": "Kerala", "33": "Tamil Nadu", "34": "Puducherry",
	"35": "Andaman and Nicobar Islands", "36": "Telangana", "37": "Andhra Pradesh", "38": "Ladakh",
	"97": "Other Territory",
}

// Name returns the state's name for a GST state code, or "" when unknown.
func Name(code string) string { return canonical[code] }
