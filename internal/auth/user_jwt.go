package auth

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	userTokenIssuer = "myslotmate-user"
)

// ErrUserSecretMissing is returned when no signing secret is configured.
var ErrUserSecretMissing = errors.New("user jwt secret is not configured")

// UserClaims is the JWT payload for an authenticated user session.
type UserClaims struct {
	UID   string `json:"uid"`
	Email string `json:"email"`
	Phone string `json:"phone"`
	jwt.RegisteredClaims
}

// IssueUserToken signs a new HS256 user session token valid for ttl.
func IssueUserToken(secret, uid, email, phone string, ttl time.Duration) (string, time.Time, error) {
	if secret == "" {
		return "", time.Time{}, ErrUserSecretMissing
	}

	now := time.Now()
	expiresAt := now.Add(ttl)

	claims := UserClaims{
		UID:   uid,
		Email: email,
		Phone: phone,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   uid,
			Issuer:    userTokenIssuer,
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

// ParseUserToken validates a signed user token and returns its claims.
func ParseUserToken(secret, tokenString string) (*UserClaims, error) {
	if secret == "" {
		return nil, ErrUserSecretMissing
	}

	claims := &UserClaims{}
	_, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return []byte(secret), nil
	},
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithIssuer(userTokenIssuer),
	)
	if err != nil {
		return nil, err
	}
	return claims, nil
}
