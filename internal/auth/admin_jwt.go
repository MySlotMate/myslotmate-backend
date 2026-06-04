package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	// ContextKeyAdminUser stores the authenticated admin's username (login identifier).
	ContextKeyAdminUser contextKey = "admin_user"
	// ContextKeyAdminRole stores the authenticated admin's role.
	ContextKeyAdminRole contextKey = "admin_role"

	adminTokenIssuer = "myslotmate-admin"
)

// ErrAdminSecretMissing is returned when no signing secret is configured.
var ErrAdminSecretMissing = errors.New("admin jwt secret is not configured")

// AdminClaims is the JWT payload for a statically-authenticated admin session.
type AdminClaims struct {
	Username string `json:"username"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	jwt.RegisteredClaims
}

// IssueAdminToken signs a new HS256 admin session token valid for ttl.
func IssueAdminToken(secret, username, name, role string, ttl time.Duration) (string, time.Time, error) {
	if secret == "" {
		return "", time.Time{}, ErrAdminSecretMissing
	}

	now := time.Now()
	expiresAt := now.Add(ttl)

	claims := AdminClaims{
		Username: username,
		Name:     name,
		Role:     role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   username,
			Issuer:    adminTokenIssuer,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(secret))
	if err != nil {
		return "", time.Time{}, err
	}
	return signed, expiresAt, nil
}

// ParseAdminToken validates a signed admin token and returns its claims.
func ParseAdminToken(secret, tokenString string) (*AdminClaims, error) {
	if secret == "" {
		return nil, ErrAdminSecretMissing
	}

	claims := &AdminClaims{}
	_, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return []byte(secret), nil
	},
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithIssuer(adminTokenIssuer),
	)
	if err != nil {
		return nil, err
	}
	return claims, nil
}

// RequireAdminToken is HTTP middleware that verifies the static-admin session
// JWT from the Authorization header. On success it stores the admin's username
// and role in the request context.
func RequireAdminToken(secret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				http.Error(w, `{"success":false,"error":"missing Authorization header"}`, http.StatusUnauthorized)
				return
			}
			tokenString := strings.TrimPrefix(authHeader, "Bearer ")
			if tokenString == authHeader { // no "Bearer " prefix found
				http.Error(w, `{"success":false,"error":"invalid Authorization header format"}`, http.StatusUnauthorized)
				return
			}

			claims, err := ParseAdminToken(secret, tokenString)
			if err != nil {
				http.Error(w, `{"success":false,"error":"invalid or expired token"}`, http.StatusUnauthorized)
				return
			}

			ctx := context.WithValue(r.Context(), ContextKeyAdminUser, claims.Username)
			ctx = context.WithValue(ctx, ContextKeyAdminRole, claims.Role)
			// Mirror into the shared email key so handlers that read it work uniformly.
			ctx = context.WithValue(ctx, ContextKeyEmail, claims.Username)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
