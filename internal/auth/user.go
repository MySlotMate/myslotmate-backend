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
func RequireUser(firebaseAuth *auth.Client, jwtSecret string) func(http.Handler) http.Handler {
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

			// Try to verify as custom user JWT first if secret is configured
			if jwtSecret != "" {
				if claims, err := ParseUserToken(jwtSecret, idToken); err == nil {
					ctx := context.WithValue(r.Context(), ContextKeyEmail, claims.Email)
					ctx = context.WithValue(ctx, ContextKeyUID, claims.UID)
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
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

// OptionalUser verifies the Authorization header the same way RequireUser does,
// but lets an unauthenticated request through instead of rejecting it. A valid
// token puts the caller's email and UID in the context; a missing or bad one
// simply leaves them absent.
//
// It exists for endpoints that are mid-migration to authenticated identity: a
// handler can trust the context when it is there, and fall back to whatever the
// body claims when it is not. Use RequireUser for anything that must be signed
// in — this middleware is deliberately not a gate.
func OptionalUser(firebaseAuth *auth.Client, jwtSecret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			idToken := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if idToken == "" || idToken == r.Header.Get("Authorization") {
				next.ServeHTTP(w, r)
				return
			}

			if jwtSecret != "" {
				if claims, err := ParseUserToken(jwtSecret, idToken); err == nil {
					ctx := context.WithValue(r.Context(), ContextKeyEmail, claims.Email)
					ctx = context.WithValue(ctx, ContextKeyUID, claims.UID)
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			}

			if firebaseAuth != nil {
				if token, err := firebaseAuth.VerifyIDToken(r.Context(), idToken); err == nil {
					email, _ := token.Claims["email"].(string)
					ctx := context.WithValue(r.Context(), ContextKeyEmail, email)
					ctx = context.WithValue(ctx, ContextKeyUID, token.UID)
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			}

			next.ServeHTTP(w, r)
		})
	}
}
