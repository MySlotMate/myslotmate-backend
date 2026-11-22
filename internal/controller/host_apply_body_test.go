package controller

import (
	"encoding/json"
	"testing"
)

// The form's two-path answers must survive the controller's body → service
// request conversion, or /hosts/apply silently drops them.
func TestHostApplicationBody_CarriesAnswers(t *testing.T) {
	body := `{"first_name":"Asha","city":"Guwahati","host_type":"professional","profession":"yoga",
		"selfie_url":"https://b/hosts/government-ids/me.png","languages":["English","Hindi"],
		"portfolio_photo_urls":["https://x/1.png"],"community_name":"Club"}`
	var req HostApplicationRequestBody
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	got := toHostApplicationRequest(req)
	if got.HostType != "professional" || got.Profession == nil || *got.Profession != "yoga" ||
		got.SelfieURL == nil || len(got.Languages) != 2 || len(got.PortfolioPhotoURLs) != 1 || got.CommunityName == nil {
		t.Fatalf("answers dropped: %+v", got.HostApplicationAnswers)
	}
}
