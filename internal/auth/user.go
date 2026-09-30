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

// OptionalAdmin records that the caller holds a valid admin session, without
// requiring one. A valid admin token puts the admin's username in the context
// (ContextKeyAdminUser); anything else passes straight through untouched.
//
// It exists so a route can be host-scoped for hosts and still serve the admin
// dashboard, which reads other people's hosts by design. Pair it with
// RequireUser or OptionalUser — on its own it gates nothing.
func OptionalAdmin(firebaseAuth *auth.Client, adminEmail, jwtSecret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := r.Header.Get("Authorization")
			token := strings.TrimPrefix(header, "Bearer ")
			if token == "" || token == header {
				next.ServeHTTP(w, r)
				return
			}

			if jwtSecret != "" {
				if claims, err := ParseAdminToken(jwtSecret, token); err == nil {
					ctx := context.WithValue(r.Context(), ContextKeyAdminUser, claims.Username)
					ctx = context.WithValue(ctx, ContextKeyAdminRole, claims.Role)
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			}

			// A Firebase admin is identified by their email being on the
			// allow-list, the same test IsAdmin applies.
			if firebaseAuth != nil && adminEmail != "" {
				if verified, err := firebaseAuth.VerifyIDToken(r.Context(), token); err == nil {
					if email, _ := verified.Claims["email"].(string); email != "" && isAllowedAdminEmail(email, adminEmail) {
						ctx := context.WithValue(r.Context(), ContextKeyAdminUser, email)
						next.ServeHTTP(w, r.WithContext(ctx))
						return
					}
				}
			}

			next.ServeHTTP(w, r)
		})
	}
}

// RequireUserOrAdmin gates a route that both hosts and admins use.
//
// Order matters: the admin session token carries a different issuer to a user
// token, so RequireUser rejects it outright. Trying admin first means an admin
// is recognised rather than turned away at the door; anything that is not an
// admin session falls through to the normal user check.
func RequireUserOrAdmin(firebaseAuth *auth.Client, adminEmail, jwtSecret string) func(http.Handler) http.Handler {
	optionalAdmin := OptionalAdmin(firebaseAuth, adminEmail, jwtSecret)
	requireUser := RequireUser(firebaseAuth, jwtSecret)

	return func(next http.Handler) http.Handler {
		// Admins reach the handler directly; everyone else goes through the
		// user gate first.
		gated := requireUser(next)

		return optionalAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if IsAdminCaller(r) {
				next.ServeHTTP(w, r)
				return
			}
			gated.ServeHTTP(w, r)
		}))
	}
}

// IsAdminCaller reports whether OptionalAdmin (or RequireAdmin) authenticated
// this request as an admin.
func IsAdminCaller(r *http.Request) bool {
	name, _ := r.Context().Value(ContextKeyAdminUser).(string)
	return name != ""
}

// isAllowedAdminEmail matches IsAdmin's test: a case-insensitive hit in the
// comma-separated allow-list.
func isAllowedAdminEmail(email, adminEmail string) bool {
	for _, a := range strings.Split(adminEmail, ",") {
		if strings.EqualFold(email, strings.TrimSpace(a)) {
			return true
		}
	}
	return false
}
