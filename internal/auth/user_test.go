package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// An admin session token must pass RequireUserOrAdmin. It carries a different
// issuer to a user token, so a plain RequireUser rejects it — which is what
// broke admin avatar uploads and admin event creation.
func TestRequireUserOrAdminAcceptsAdminToken(t *testing.T) {
	const secret = "test-secret"

	token, _, err := IssueAdminToken(secret, "admin", "Admin", "superadmin", time.Hour)
	if err != nil {
		t.Fatalf("IssueAdminToken: %v", err)
	}

	mw := RequireUserOrAdmin(nil, "admin@example.com", secret)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !IsAdminCaller(r) {
			t.Error("expected the request to be flagged as an admin caller")
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/upload", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("admin token rejected: got %d, body %s", rec.Code, rec.Body.String())
	}
}

func TestRequireUserOrAdminRejectsMissingHeader(t *testing.T) {
	mw := RequireUserOrAdmin(nil, "admin@example.com", "test-secret")
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler must not run without credentials")
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/upload", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}
