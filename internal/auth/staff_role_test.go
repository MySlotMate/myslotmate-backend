package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The marketing login must reach only routes wrapped in OrStaff; every other
// admin route (all built on ParseAdminToken) must refuse it.
func TestMarketingRoleIsConfinedToStaffRoutes(t *testing.T) {
	const secret = "test-secret"
	mkt, _, err := IssueAdminToken(secret, "mkt@x.com", "Marketing", RoleMarketing, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	adm, _, _ := IssueAdminToken(secret, "admin@x.com", "Administrator", "Super Admin", time.Hour)

	if _, err := ParseAdminToken(secret, mkt); err == nil {
		t.Fatal("ParseAdminToken accepted a marketing token")
	}
	if _, err := ParseStaffToken(secret, mkt); err != nil {
		t.Fatalf("ParseStaffToken rejected a marketing token: %v", err)
	}

	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	call := func(h http.Handler, token string) int {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	adminOnly := RequireAdminToken(secret)(ok)
	staff := OrStaff(secret, RequireAdminToken(secret))(ok)

	if code := call(adminOnly, mkt); code != http.StatusUnauthorized {
		t.Errorf("admin-only route let marketing in: %d", code)
	}
	if code := call(adminOnly, adm); code != http.StatusOK {
		t.Errorf("admin-only route rejected admin: %d", code)
	}
	if code := call(staff, mkt); code != http.StatusOK {
		t.Errorf("staff route rejected marketing: %d", code)
	}
	if code := call(staff, adm); code != http.StatusOK {
		t.Errorf("staff route rejected admin: %d", code)
	}
	if code := call(staff, "garbage"); code != http.StatusUnauthorized {
		t.Errorf("staff route let a bad token in: %d", code)
	}
}
