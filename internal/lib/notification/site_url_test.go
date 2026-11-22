package notification

import "testing"

func TestSiteURL(t *testing.T) {
	for in, want := range map[string]string{
		"":                             "https://myslotmate.com",
		"https://staging.example.com/": "https://staging.example.com",
		"http://localhost:3000":        "http://localhost:3000",
	} {
		if got := (&TwilioNotificationService{FrontendBaseURL: in}).siteURL(); got != want {
			t.Errorf("siteURL(%q) = %q, want %q", in, got, want)
		}
	}
}
