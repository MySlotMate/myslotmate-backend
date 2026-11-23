package controller

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"myslotmate-backend/internal/auth"

	"github.com/go-chi/chi/v5"
)

// AdminAuthController issues and validates session tokens for the admin
// dashboard. Authentication is against a single static credential pair
// (ADMIN_USERNAME / ADMIN_PASSWORD) and, on success, returns a signed JWT.
type AdminAuthController struct {
	username  string
	password  string
	jwtSecret string
	tokenTTL  time.Duration

	// displayName / role are returned to the dashboard for UI rendering.
	displayName string
	role        string

	// Optional restricted marketing login (auth.RoleMarketing); empty = off.
	marketingUsername string
	marketingPassword string
}

// WithMarketingLogin enables the restricted marketing login. Both values must
// be non-empty, otherwise it stays disabled.
func (c *AdminAuthController) WithMarketingLogin(username, password string) *AdminAuthController {
	if strings.TrimSpace(username) != "" && password != "" {
		c.marketingUsername, c.marketingPassword = strings.TrimSpace(username), password
	}
	return c
}

func credentialsMatch(gotUser, gotPass, wantUser, wantPass string) bool {
	if wantUser == "" || wantPass == "" {
		return false
	}
	userMatch := subtle.ConstantTimeCompare([]byte(strings.ToLower(gotUser)), []byte(strings.ToLower(wantUser))) == 1
	passMatch := subtle.ConstantTimeCompare([]byte(gotPass), []byte(wantPass)) == 1
	return userMatch && passMatch
}

// NewAdminAuthController builds the controller from the configured static
// credentials and signing settings.
func NewAdminAuthController(username, password, jwtSecret string, tokenTTL time.Duration) *AdminAuthController {
	if tokenTTL <= 0 {
		tokenTTL = 12 * time.Hour
	}
	return &AdminAuthController{
		username:    username,
		password:    password,
		jwtSecret:   jwtSecret,
		tokenTTL:    tokenTTL,
		displayName: "Administrator",
		role:        "Super Admin",
	}
}

func (c *AdminAuthController) RegisterRoutes(r chi.Router) {
	r.Route("/admin/auth", func(r chi.Router) {
		r.Post("/login", c.Login)

		// Token introspection — used by the dashboard to validate a stored
		// session on load.
		// Any staff session, so the marketing login can restore itself too.
		r.With(auth.OrStaff(c.jwtSecret, auth.RequireAdminToken(c.jwtSecret))).Get("/me", c.Me)
	})
}

// ── Request / response types ─────────────────────────────────────────────────

type adminLoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type adminUserPayload struct {
	Username string `json:"username"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	Scope    string `json:"scope"`
}

type adminLoginResponse struct {
	Token     string           `json:"token"`
	ExpiresAt time.Time        `json:"expires_at"`
	User      adminUserPayload `json:"user"`
}

// ── Handlers ─────────────────────────────────────────────────────────────────

// Login validates the static admin credentials and returns a signed session
// token. Credentials are compared in constant time to avoid leaking length or
// content via timing.
func (c *AdminAuthController) Login(w http.ResponseWriter, r *http.Request) {
	if c.jwtSecret == "" {
		RespondError(w, http.StatusInternalServerError, "Admin authentication is not configured")
		return
	}

	var req adminLoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	username := strings.TrimSpace(req.Username)
	isAdmin := credentialsMatch(username, req.Password, c.username, c.password)
	isMarketing := !isAdmin && credentialsMatch(username, req.Password, c.marketingUsername, c.marketingPassword)

	if !isAdmin && !isMarketing {
		RespondError(w, http.StatusUnauthorized, "Invalid administrator credentials")
		return
	}

	user := c.userPayload()
	if isMarketing {
		user = adminUserPayload{Username: c.marketingUsername, Name: "Marketing", Role: auth.RoleMarketing, Scope: "Event marketing"}
	}

	token, expiresAt, err := auth.IssueAdminToken(c.jwtSecret, user.Username, user.Name, user.Role, c.tokenTTL)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, "Failed to issue session token")
		return
	}

	RespondSuccess(w, http.StatusOK, adminLoginResponse{
		Token:     token,
		ExpiresAt: expiresAt,
		User:      user,
	})
}

// Me returns the current admin identity for a valid session token.
func (c *AdminAuthController) Me(w http.ResponseWriter, r *http.Request) {
	if role, _ := r.Context().Value(auth.ContextKeyAdminRole).(string); role == auth.RoleMarketing {
		username, _ := r.Context().Value(auth.ContextKeyAdminUser).(string)
		RespondSuccess(w, http.StatusOK, adminUserPayload{Username: username, Name: "Marketing", Role: auth.RoleMarketing, Scope: "Event marketing"})
		return
	}
	RespondSuccess(w, http.StatusOK, c.userPayload())
}

func (c *AdminAuthController) userPayload() adminUserPayload {
	return adminUserPayload{
		Username: c.username,
		Name:     c.displayName,
		Role:     c.role,
		Scope:    "Global",
	}
}
