package auth

import (
	"context"
	"net/http"
	"strings"

	"firebase.google.com/go/v4/auth"
)

// contextKey is a private type to avoid collisions in context values.
type contextKey string

const (
	// ContextKeyEmail stores the authenticated user's email in request context.
	ContextKeyEmail contextKey = "auth_email"
	// ContextKeyUID stores the authenticated user's Firebase UID in request context.
	ContextKeyUID contextKey = "auth_uid"
)

// IsAdmin is an HTTP middleware that verifies the Firebase ID token from the
// Authorization header and ensures the caller's email matches the configured
// admin email. The admin email is read from the ADMIN_EMAIL environment
// variable (via config).
//
// Usage (chi router):
//
//	r.With(auth.IsAdmin(firebaseAuthClient, cfg.AdminEmail)).Post("/admin/hosts/{id}/approve", ...)
func IsAdmin(firebaseAuth *auth.Client, adminEmail string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 1. Extract bearer token
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

			// 2. Verify Firebase ID token
			token, err := firebaseAuth.VerifyIDToken(r.Context(), idToken)
			if err != nil {
				http.Error(w, `{"success":false,"error":"invalid or expired token"}`, http.StatusUnauthorized)
				return
			}

			// 3. Extract email from token claims
			email, _ := token.Claims["email"].(string)
			if email == "" {
				http.Error(w, `{"success":false,"error":"token does not contain an email claim"}`, http.StatusForbidden)
				return
			}

			// 4. Check admin (supports comma-separated list)
			isAdmin := false
			admins := strings.Split(adminEmail, ",")
			for _, a := range admins {
				if strings.EqualFold(email, strings.TrimSpace(a)) {
					isAdmin = true
					break
				}
			}

			if !isAdmin {
				http.Error(w, `{"success":false,"error":"forbidden: admin access required"}`, http.StatusForbidden)
				return
			}

			// 5. Store in context and proceed
			ctx := context.WithValue(r.Context(), ContextKeyEmail, email)
			ctx = context.WithValue(ctx, ContextKeyUID, token.UID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
