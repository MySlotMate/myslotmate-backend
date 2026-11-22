package repository

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"myslotmate-backend/internal/models"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

// Round-trips the two-path application answers through Create, Update and
// scanHost against a real Postgres. Skipped unless TEST_DATABASE_URL points at
// a migrated, throwaway database.
func TestHostRepository_ApplicationAnswersRoundTrip(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	db, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	userID := uuid.New()
	if _, err := db.ExecContext(ctx, `INSERT INTO users (id, auth_uid) VALUES ($1, $2)`, userID, "test-"+userID.String()); err != nil {
		t.Fatal(err)
	}
	repo := NewHostRepository(db)
	yoga, spec, selfie, desc := "yoga", "Hatha", "https://b/hosts/government-ids/me.png", "x"
	h := &models.Host{ID: uuid.New(), UserID: userID, FirstName: "A", LastName: "D", City: "G",
		ApplicationStatus: models.HostApplicationDraft, Slug: "t-" + userID.String(),
		ExperienceDesc: &desc, Description: &desc}
	h.HostType = models.HostTypeProfessional
	h.Profession, h.Specialization, h.SelfieURL = &yoga, &spec, &selfie
	h.Languages = []string{"English", "Hindi"}
	if err := repo.Create(ctx, h); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetByID(ctx, h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.HostType != "professional" || *got.Profession != "yoga" || *got.SelfieURL != selfie || len(got.Languages) != 2 || got.PortfolioPhotoURLs == nil {
		t.Fatalf("create round trip: %+v", got.HostApplicationAnswers)
	}

	name, reason := "Book Club", "blurry ID"
	got.HostApplicationAnswers = models.HostApplicationAnswers{HostType: "community", CommunityName: &name}
	got.RejectionReason = &reason
	if err := repo.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	got, _ = repo.GetByID(ctx, h.ID)
	if got.HostType != "community" || *got.CommunityName != name || got.Profession != nil || len(got.Languages) != 0 || *got.RejectionReason != reason {
		t.Fatalf("update round trip: %+v reason=%v", got.HostApplicationAnswers, got.RejectionReason)
	}
	if got.FirstName != "A" || got.City != "G" {
		t.Fatalf("existing columns shifted: %+v", got)
	}
}
