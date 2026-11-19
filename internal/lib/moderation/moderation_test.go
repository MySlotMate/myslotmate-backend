package moderation

import "testing"

func TestCheck(t *testing.T) {
	cases := []struct {
		text    string
		blocked bool
	}{
		{"Sunrise trek to Tawang", false},
		{"hard climb up the ridge", false},
		{"single-origin coffee tasting", false},
		{"Certificate — B.Sc. Psychology, 2019", false},
		{"looking for partner this weekend", true},
		{"Call 98765 43210 for details", true},
		{"+91-9876543210", true},
		{"pay me at rahul@okaxis", true},
		{"mail rahul.k@gmail.com", true},
		{"cash only, pay directly", true},
		{"horny and kinky night", true},
	}
	for _, c := range cases {
		if got := Check("Caption", c.text) != nil; got != c.blocked {
			t.Errorf("Check(%q) blocked=%v, want %v", c.text, got, c.blocked)
		}
	}
}
