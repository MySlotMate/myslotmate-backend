package auth

import (
	"context"
	"net/http"
	"strings"

	"firebase.google.com/go/v4/auth"
)

// RequireUser is an HTTP middleware that verifies the Firebase ID token from the
// Authorization header and rejects unauthenticated callers. On success it stores
// the caller's email and Firebase UID in the request context (ContextKeyEmail,
// ContextKeyUID).
//
// Unlike IsAdmin, it allows any authenticated user through — it does not check
// the email against the admin allow-list.
//
// Usage (chi router):
//
//	r.Route("/payouts", func(r chi.Router) {
//	    r.Use(auth.RequireUser(firebaseAuth))
//	    r.Post("/withdraw", c.RequestWithdrawal)
//	    // ...
//	})
func RequireUser(firebaseAuth *auth.Client) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				http.Error(w, `{"success":false,"error":"missing Authorization header"}`, http.StatusUnauthorized)
				return
			}
			idToken := strings.TrimPrefix(authHeader, "Bearer ")
			if idToken == authHeader { // no "Bearer " prefix found
				http.Error(w, `{"success":false,"error":"invalid Authorization header format"}`, http.StatusUnauthorized)
				return
			}

			token, err := firebaseAuth.VerifyIDToken(r.Context(), idToken)
			if err != nil {
				http.Error(w, `{"success":false,"error":"invalid or expired token"}`, http.StatusUnauthorized)
				return
			}

			email, _ := token.Claims["email"].(string)

			ctx := context.WithValue(r.Context(), ContextKeyEmail, email)
			ctx = context.WithValue(ctx, ContextKeyUID, token.UID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
