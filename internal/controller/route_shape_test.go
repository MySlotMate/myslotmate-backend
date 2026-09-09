package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TestUsersRouteShape pins the ordering that lets /users/signup-prefill keep its
// auth middleware instead of being swallowed by /users/{userID}.
func TestUsersRouteShape(t *testing.T) {
	r := chi.NewRouter()
	r.Route("/users", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					if req.Header.Get("Authorization") == "" {
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
					next.ServeHTTP(w, req)
				})
			})
			r.Get("/signup-prefill", func(w http.ResponseWriter, req *http.Request) {
				fmt.Fprint(w, "prefill")
			})
		})
		r.Get("/me", func(w http.ResponseWriter, req *http.Request) { fmt.Fprint(w, "me") })
		r.Get("/{userID}", func(w http.ResponseWriter, req *http.Request) {
			fmt.Fprint(w, "byID:"+chi.URLParam(req, "userID"))
		})
	})

	cases := []struct {
		path, auth string
		code       int
		body       string
	}{
		{"/users/signup-prefill", "", http.StatusUnauthorized, ""},
		{"/users/signup-prefill", "Bearer x", http.StatusOK, "prefill"},
		{"/users/abc-123", "", http.StatusOK, "byID:abc-123"},
		{"/users/me", "", http.StatusOK, "me"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		if tc.auth != "" {
			req.Header.Set("Authorization", tc.auth)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != tc.code || w.Body.String() != tc.body {
			t.Errorf("GET %s auth=%q = %d %q; want %d %q", tc.path, tc.auth, w.Code, w.Body.String(), tc.code, tc.body)
		}
	}
}
