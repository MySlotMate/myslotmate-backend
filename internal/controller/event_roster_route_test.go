package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"myslotmate-backend/internal/auth"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// The helper tests prove assertHostScope decides correctly. This one proves the
// router actually puts it in the way: a route registered in the wrong block
// would pass every helper test and still be wide open.
func TestHostScopedRoutesRejectAnonymousCallers(t *testing.T) {
	c := signedIn(uuid.New())
	// Route registration is what is under test, so the real one is used.
	r := chi.NewRouter()
	c.RegisterRoutes(r)

	eventID, hostID := uuid.New().String(), uuid.New().String()

	for _, path := range []string{
		"/events/" + eventID + "/attendees",
		"/events/host/" + hostID,
		"/events/host/" + hostID + "/filtered",
		"/events/calendar/" + hostID,
		"/events/today/" + hostID,
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", path, nil))

		if w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s returned %d, want 401 — this route is reachable without a token", path, w.Code)
		}
	}
}

// Reads that are public on purpose must stay public: the event page and the
// booking flow depend on them, and locking them would break discovery.
//
// This controller has no event service behind it, so a public route reaches the
// handler and panics on the nil service. That panic is the assertion: it can
// only happen if the request got past the middleware. A 401 would mean it never
// did.
func TestPublicEventReadsStayPublic(t *testing.T) {
	c := signedIn(uuid.New())
	r := chi.NewRouter()
	c.RegisterRoutes(r)

	for _, path := range []string{
		"/events/",
		"/events/" + uuid.New().String() + "/availability",
	} {
		reached := func() (reached bool) {
			defer func() {
				if recover() != nil {
					reached = true // got to the handler, which is the point
				}
			}()
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
			return w.Code != http.StatusUnauthorized
		}()

		if !reached {
			t.Errorf("GET %s was refused with 401 — this read is public by design", path)
		}
	}
}

var _ = auth.IsAdminCaller
