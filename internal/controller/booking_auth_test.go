package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"myslotmate-backend/internal/auth"
	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Until this change every route below was public, and CreateBooking took the
// buyer's id from the request body — so one account could book as another and
// spend that person's wallet. These tests are the regression guard.

type fakeBookingUsers struct {
	repository.UserRepository
	user *models.User
}

func (f *fakeBookingUsers) GetByAuthUID(ctx context.Context, uid string) (*models.User, error) {
	return f.user, nil
}

func bookingControllerFor(userID uuid.UUID) *BookingController {
	c := &BookingController{}
	return c.WithAuth(&fakeBookingUsers{user: &models.User{ID: userID}}, nil, nil, nil, "", "")
}

func bookingRouter(c *BookingController) chi.Router {
	r := chi.NewRouter()
	c.RegisterRoutes(r)
	return r
}

func TestBookingRoutesRejectAnonymousCallers(t *testing.T) {
	r := bookingRouter(bookingControllerFor(uuid.New()))
	bookingID, userID := uuid.New().String(), uuid.New().String()

	for _, tc := range []struct{ method, path string }{
		{"POST", "/bookings/"},
		{"POST", "/bookings/" + bookingID + "/confirm"},
		{"POST", "/bookings/" + bookingID + "/cancel"},
		{"GET", "/bookings/" + bookingID},
		{"GET", "/bookings/user/" + userID},
		{"POST", "/bookings/" + bookingID + "/ticket-notification"},
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
		r.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s returned %d, want 401 — this route spends or exposes someone's money",
				tc.method, tc.path, w.Code)
		}
	}
}

func TestCallerUserIDFailsClosedWithoutLookups(t *testing.T) {
	// A controller wired without WithAuth cannot identify anyone, so it must
	// refuse rather than fall through to a zero uuid.
	c := &BookingController{}
	r := httptest.NewRequest("POST", "/bookings/", nil)
	r = r.WithContext(context.WithValue(r.Context(), auth.ContextKeyUID, "firebase-uid"))

	if _, err := c.callerUserID(r); err == nil {
		t.Fatal("a controller with no identity lookups resolved a caller")
	}
}

func TestCallerUserIDRequiresAToken(t *testing.T) {
	c := bookingControllerFor(uuid.New())
	// No UID in context: the request never authenticated.
	if _, err := c.callerUserID(httptest.NewRequest("POST", "/bookings/", nil)); err == nil {
		t.Fatal("an unauthenticated caller was resolved to a user")
	}
}

func TestCallerUserIDComesFromTheToken(t *testing.T) {
	mine := uuid.New()
	c := bookingControllerFor(mine)

	r := httptest.NewRequest("POST", "/bookings/", nil)
	r = r.WithContext(context.WithValue(r.Context(), auth.ContextKeyUID, "firebase-uid"))

	got, err := c.callerUserID(r)
	if err != nil || got != mine {
		t.Fatalf("expected the signed-in user %s, got %s (err %v)", mine, got, err)
	}
}
