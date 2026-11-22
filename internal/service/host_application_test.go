package service

import (
	"context"
	"encoding/json"
	"testing"

	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"

	"github.com/google/uuid"
)

type fakeAppHosts struct {
	repository.HostRepository
	existing *models.Host
	created  *models.Host
	updated  *models.Host
}

func (f *fakeAppHosts) GetByUserID(ctx context.Context, userID uuid.UUID) (*models.Host, error) {
	return f.existing, nil
}
func (f *fakeAppHosts) SlugExists(ctx context.Context, slug string) (bool, error) { return false, nil }
func (f *fakeAppHosts) Create(ctx context.Context, h *models.Host) error          { f.created = h; return nil }
func (f *fakeAppHosts) Update(ctx context.Context, h *models.Host) error          { f.updated = h; return nil }

type fakeAppUsers struct {
	repository.UserRepository
}

func (fakeAppUsers) GetByID(ctx context.Context, id uuid.UUID) (*models.User, error) {
	return &models.User{ID: id}, nil
}

func newAppService(hosts *fakeAppHosts) *hostService {
	return &hostService{hostRepo: hosts, userRepo: fakeAppUsers{}}
}

// The exact shape the /become-host form posts (spec §2).
const professionalBody = `{
	"first_name": "Asha", "last_name": "Das", "city": "Guwahati",
	"moods": ["wellness"], "experience_desc": "Path: Verified professional",
	"government_id_url": "https://b.s3.ap-south-1.amazonaws.com/hosts/government-ids/id.png",
	"host_type": "professional",
	"selfie_url": "https://b.s3.ap-south-1.amazonaws.com/hosts/government-ids/me.png",
	"profession": "yoga", "specialization": "Hatha yoga for beginners",
	"years_experience": "3–5 years", "license_number": "LIC-123456",
	"professional_body": "Yoga Alliance", "languages": ["English", "Hindi"],
	"portfolio_photo_urls": ["https://x/1.png"]
}`

func TestSaveApplication_StoresProfessionalFields(t *testing.T) {
	var req HostApplicationRequest
	if err := json.Unmarshal([]byte(professionalBody), &req); err != nil {
		t.Fatal(err)
	}
	hosts := &fakeAppHosts{}
	if _, err := newAppService(hosts).SaveDraft(context.Background(), uuid.New(), req); err != nil {
		t.Fatal(err)
	}
	h := hosts.created
	if h == nil {
		t.Fatal("host not created")
	}
	if h.HostType != "professional" || deref(h.Profession) != "yoga" || deref(h.Specialization) != "Hatha yoga for beginners" {
		t.Fatalf("answers not stored: %+v", h.HostApplicationAnswers)
	}
	if deref(h.YearsExperience) != "3–5 years" || deref(h.LicenseNumber) != "LIC-123456" || deref(h.ProfessionalBody) != "Yoga Alliance" {
		t.Fatalf("answers not stored: %+v", h.HostApplicationAnswers)
	}
	if len(h.Languages) != 2 || h.Languages[1] != "Hindi" || len(h.PortfolioPhotoURLs) != 1 {
		t.Fatalf("arrays not stored: %v %v", h.Languages, h.PortfolioPhotoURLs)
	}
	if deref(h.SelfieURL) == "" || h.IsProfessional {
		t.Fatalf("selfie missing or is_professional taken from body: %+v", h)
	}
}

func TestSaveApplication_PathSwitchClearsOtherPath(t *testing.T) {
	yoga := "yoga"
	old := &models.Host{ID: uuid.New(), ApplicationStatus: models.HostApplicationRejected}
	old.HostType = "professional"
	old.Profession = &yoga
	old.Languages = []string{"English"}
	hosts := &fakeAppHosts{existing: old}

	name := "Guwahati Book Club"
	cat := "social"
	req := HostApplicationRequest{FirstName: "Asha"}
	req.HostType = "community"
	req.CommunityName = &name
	req.CommunityCategory = &cat
	if _, err := newAppService(hosts).SaveDraft(context.Background(), uuid.New(), req); err != nil {
		t.Fatal(err)
	}
	h := hosts.updated
	if h == nil || h.HostType != "community" || deref(h.CommunityName) != name {
		t.Fatalf("community answers not saved: %+v", h)
	}
	if h.Profession != nil || len(h.Languages) != 0 || h.Languages == nil {
		t.Fatalf("professional answers survived the switch: profession=%v languages=%v", h.Profession, h.Languages)
	}
}

func TestSaveApplication_MissingHostTypeDefaultsCommunity(t *testing.T) {
	hosts := &fakeAppHosts{}
	// Old app builds send only the original fields.
	req := HostApplicationRequest{FirstName: "Asha", LastName: "Das", City: "Guwahati"}
	if _, err := newAppService(hosts).SaveDraft(context.Background(), uuid.New(), req); err != nil {
		t.Fatal(err)
	}
	h := hosts.created
	if h.HostType != models.HostTypeCommunity || h.Languages == nil || h.PortfolioPhotoURLs == nil {
		t.Fatalf("defaults not applied: %+v", h.HostApplicationAnswers)
	}
}

func TestSaveApplication_OldProfessionalBuildKeepsTrack(t *testing.T) {
	hosts := &fakeAppHosts{}
	yes := true
	req := HostApplicationRequest{FirstName: "Asha", IsProfessional: &yes}
	if _, err := newAppService(hosts).SaveDraft(context.Background(), uuid.New(), req); err != nil {
		t.Fatal(err)
	}
	if h := hosts.created; h.HostType != models.HostTypeProfessional || h.IsProfessional {
		t.Fatalf("host_type=%q is_professional=%v", h.HostType, h.IsProfessional)
	}
}
