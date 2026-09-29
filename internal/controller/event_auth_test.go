package controller

import (
	"context"
	"net/http/httptest"
	"testing"

	"myslotmate-backend/internal/auth"
	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"

	"github.com/google/uuid"
)

// Fakes embed the real interfaces so only the two lookups hostIDFor makes need
// implementing; anything else would panic loudly.
type fakeUsers struct {
	repository.UserRepository
	user *models.User
}

func (f *fakeUsers) GetByAuthUID(ctx context.Context, uid string) (*models.User, error) {
	return f.user, nil
}

type fakeHosts struct {
	repository.HostRepository
	host *models.Host
}

func (f *fakeHosts) GetByUserID(ctx context.Context, userID uuid.UUID) (*models.Host, error) {
	return f.host, nil
}

func signedIn(hostID uuid.UUID) *EventController {
	userID := uuid.New()
	c := &EventController{}
	return c.WithAuth(
		&fakeUsers{user: &models.User{ID: userID}},
		&fakeHosts{host: &models.Host{ID: hostID, UserID: userID}},
		nil, "",
	)
}

func TestHostIDForPrefersTheSignedInHost(t *testing.T) {
	mine := uuid.New()
	c := signedIn(mine)

	r := httptest.NewRequest("POST", "/events/", nil)
	r = r.WithContext(context.WithValue(r.Context(), auth.ContextKeyUID, "firebase-uid"))

	// No host_id in the body at all — the token decides.
	got, err := c.hostIDFor(r, uuid.Nil)
	if err != nil || got != mine {
		t.Fatalf("expected the signed-in host %s, got %s (err %v)", mine, got, err)
	}

	// A body host_id that agrees is fine.
	got, err = c.hostIDFor(r, mine)
	if err != nil || got != mine {
		t.Fatalf("matching host_id must be accepted, got %s (err %v)", got, err)
	}

	// Someone else's host_id is refused — this is the spoof that used to work.
	if _, err := c.hostIDFor(r, uuid.New()); err == nil {
		t.Fatal("a host_id belonging to another host must be refused")
	}
}

func TestHostIDForRefusesAnUnauthenticatedCaller(t *testing.T) {
	c := signedIn(uuid.New())

	// No UID in context. The mutating routes sit behind auth.RequireUser so this
	// should be unreachable — if it ever happens, a body host_id must NOT be
	// trusted, which is exactly the spoof this endpoint used to allow.
	r := httptest.NewRequest("POST", "/events/", nil)
	if _, err := c.hostIDFor(r, uuid.New()); err == nil {
		t.Fatal("a body host_id must never be honoured without a token")
	}
	if _, err := c.hostIDFor(r, uuid.Nil); err == nil {
		t.Fatal("an unauthenticated call must be refused")
	}
}
