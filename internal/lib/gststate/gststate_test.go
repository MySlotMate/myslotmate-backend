package gststate

import "testing"

// Same cases as Myslotmate-backend-node/tests/gst-state.test.ts.
func TestFromAddress(t *testing.T) {
	cases := map[string]string{
		"Zoo Road, Guwahati, Kamrup Metropolitan, Assam, 781024, India":          "18",
		"Bandra West, Mumbai, Maharashtra 400050, India":                        "27",
		"Indiranagar, Bengaluru, Karnataka":                                     "29",
		"Connaught Place, New Delhi, Delhi 110001":                              "07",
		"Delhi Gate, Agra, Uttar Pradesh, India":                                "09", // last state named wins
		"Goalpara, Assam":                                                       "18", // "Goa" only as a whole word
		"MG Road, Kochi, kerala":                                                "32", // case-insensitive
		"Bhubaneswar, Orissa":                                                   "21", // old name
		"Srinagar, Jammu & Kashmir":                                             "01",
		"Leh, Ladakh":                                                           "38",
		"Visakhapatnam, Andhra Pradesh":                                         "37",
		"Itanagar, Arunachal Pradesh":                                           "12",
		"Hyderabad, Telangana 500001":                                           "36",
		"Pondicherry":                                                           "34",
		"Shoreditch, London, United Kingdom":                                    "",
		"":                                                                      "",
	}
	for addr, want := range cases {
		if got := FromAddress(addr); got != want {
			t.Errorf("FromAddress(%q) = %q, want %q", addr, got, want)
		}
	}
}
