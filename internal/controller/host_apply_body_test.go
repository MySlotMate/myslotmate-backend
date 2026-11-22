package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/service"

	"github.com/google/uuid"
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

type rejectingHosts struct {
	service.HostService
}

func (rejectingHosts) SubmitApplication(ctx context.Context, userID uuid.UUID, req service.HostApplicationRequest) (*models.Host, error) {
	return nil, service.ValidateHostApplication(req, "")
}

// A rule failure reaches the client as 400 {error: code, message} (spec §3).
func TestSubmitApplication_RuleFailureIs400WithCode(t *testing.T) {
	c := NewHostController(rejectingHosts{})
	r := httptest.NewRequest(http.MethodPost, "/hosts/apply", strings.NewReader(`{"first_name":"A","city":"B","host_type":"professional"}`))
	w := httptest.NewRecorder()
	c.SubmitApplication(w, r)
	var body struct{ Error, Message string }
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != http.StatusBadRequest || body.Error != "government_id_required" || body.Message != "Please upload a government ID." {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}
