package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"myslotmate-backend/internal/auth"
	"myslotmate-backend/internal/models"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// The /coupons write and list routes were once fully public: anyone holding a
// host id could mint free-booking codes for that host or read all of them.

func TestCouponRoutesRequireSignIn(t *testing.T) {
	c := NewCouponController(nil, nil, nil, nil, nil, nil, "", "")
	router := chi.NewRouter()
	c.RegisterRoutes(router)

	host := uuid.New().String()
	for _, tc := range []struct{ method, path string }{
		{"POST", "/coupons/"},
		{"POST", "/coupons/batch"},
		{"GET", "/coupons/host/" + host},
		{"PUT", "/coupons/" + uuid.New().String()},
		{"DELETE", "/coupons/" + uuid.New().String() + "?host_id=" + host},
	} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"host_id":"`+host+`","code":"X"}`)))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a token: got %d, want 401", tc.method, tc.path, rec.Code)
		}
	}
}

func TestCouponHostCannotActForAnotherHost(t *testing.T) {
	mine, theirs := uuid.New(), uuid.New()
	userID := uuid.New()
	c := NewCouponController(nil, nil, nil,
		&fakeUsers{user: &models.User{ID: userID}},
		&fakeHosts{host: &models.Host{ID: mine, UserID: userID}},
		nil, "", "")

	withUID := func(r *http.Request, id string) *http.Request {
		rc := chi.NewRouteContext()
		rc.URLParams.Add("hostID", theirs.String())
		rc.URLParams.Add("couponID", id)
		ctx := context.WithValue(r.Context(), chi.RouteCtxKey, rc)
		return r.WithContext(context.WithValue(ctx, auth.ContextKeyUID, "firebase-uid"))
	}
	body := `{"host_id":"` + theirs.String() + `","code":"FREE"}`

	for name, run := range map[string]func(w http.ResponseWriter){
		"create": func(w http.ResponseWriter) {
			c.CreateCoupon(w, withUID(httptest.NewRequest("POST", "/coupons/", strings.NewReader(body)), ""))
		},
		"batch": func(w http.ResponseWriter) {
			c.BatchCreateCoupons(w, withUID(httptest.NewRequest("POST", "/coupons/batch", strings.NewReader(`{"host_id":"`+theirs.String()+`","count":5}`)), ""))
		},
		"list": func(w http.ResponseWriter) {
			c.ListHostCoupons(w, withUID(httptest.NewRequest("GET", "/coupons/host/x", nil), ""))
		},
		"update": func(w http.ResponseWriter) {
			c.UpdateCoupon(w, withUID(httptest.NewRequest("PUT", "/coupons/x", strings.NewReader(body)), uuid.New().String()))
		},
		"delete": func(w http.ResponseWriter) {
			c.DeleteCoupon(w, withUID(httptest.NewRequest("DELETE", "/coupons/x?host_id="+theirs.String(), nil), uuid.New().String()))
		},
	} {
		rec := httptest.NewRecorder()
		run(rec)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s on another host's coupons: got %d, want 403", name, rec.Code)
		}
	}
}
