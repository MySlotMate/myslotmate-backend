package controller

import (
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// The paid-budget Boost and Premium routes mount next to the existing Boost
// routes without a chi conflict, at the paths the website calls.
func TestBoostPlanRoutesRegister(t *testing.T) {
	r := chi.NewRouter()
	c := NewBoostController(nil, nil, nil, "secret", "admin@example.com")
	c.RegisterRoutes(r)
	want := map[string]bool{
		"GET /experiences/{eventID}/boost/estimate":          false,
		"POST /boosts/{boostID}/payments/{paymentID}/verify": false,
		"DELETE /boosts/{boostID}/creatives/{creativeID}":    false,
		"GET /host/premium":        false,
		"GET /boost/settings":      false,
		"GET /host/boost-activity": false,
		"POST /admin/boosts/{boostID}/payments/{paymentID}/reverse": false,
		"GET /admin/boosts/{boostID}/creatives/download":            false,
		"PUT /admin/boosts/{boostID}/spend":                         false,
		"POST /admin/premium/{hostID}/grant":                        false,
		"PUT /admin/boost-settings":                                 false,
	}
	_ = chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		key := method + " " + strings.TrimSuffix(strings.ReplaceAll(route, "/*/", "/"), "/")
		if _, ok := want[key]; ok {
			want[key] = true
		}
		return nil
	})
	for k, seen := range want {
		if !seen {
			t.Errorf("route %s not registered", k)
		}
	}
}
