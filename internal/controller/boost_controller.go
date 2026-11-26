package controller

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"myslotmate-backend/internal/auth"
	"myslotmate-backend/internal/repository"
	"myslotmate-backend/internal/service"

	fbauth "firebase.google.com/go/v4/auth"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// BoostController serves Event Boost: the host routes the website already
// calls (docs/backend-spec-boost-and-payouts.md) and the admin queue.
type BoostController struct {
	boosts       *service.BoostService
	userRepo     repository.UserRepository
	firebaseAuth *fbauth.Client
	jwtSecret    string
	adminEmail   string
}

func NewBoostController(b *service.BoostService, ur repository.UserRepository, fa *fbauth.Client, jwtSecret, adminEmail string) *BoostController {
	return &BoostController{boosts: b, userRepo: ur, firebaseAuth: fa, jwtSecret: jwtSecret, adminEmail: adminEmail}
}

func (c *BoostController) RegisterRoutes(r chi.Router) {
	user := auth.RequireUser(c.firebaseAuth, c.jwtSecret)
	r.With(user).Get("/experiences/{eventID}/boost", c.GetEventBoost)
	r.With(user).Post("/experiences/{eventID}/boost", c.RequestBoost)
	r.With(user).Post("/boosts/{boostID}/accept", c.Accept)
	r.With(user).Post("/boosts/{boostID}/stop", c.Stop)
	r.With(user).Get("/host/boosts", c.HostBoosts)

	r.Route("/admin/boosts", func(r chi.Router) {
		r.Use(auth.RequireAdmin(c.firebaseAuth, c.adminEmail, c.jwtSecret))
		r.Get("/", c.AdminList)
		r.Post("/", c.AdminStart)
		r.Get("/{boostID}", c.AdminGet)
		r.Put("/{boostID}/proposal", c.Propose)
		r.Post("/{boostID}/decline", c.Decline)
		r.Post("/{boostID}/accept", c.AdminAccept)
		r.Post("/{boostID}/go-live", c.GoLive)
		r.Post("/{boostID}/end", c.End)
		r.Patch("/{boostID}/campaign", c.Campaign)
	})
	r.With(auth.RequireAdmin(c.firebaseAuth, c.adminEmail, c.jwtSecret)).Put("/admin/boost-invites/{hostID}", c.SetInvite)
	r.With(auth.RequireAdmin(c.firebaseAuth, c.adminEmail, c.jwtSecret)).Get("/admin/boost-invites/{hostID}", c.GetInvite)
}

// RegisterHostRoutes adds /hosts/{hostID}/payout-eligibility to the hosts router.
func (c *BoostController) RegisterHostRoutes(r chi.Router) {
	r.With(auth.RequireUser(c.firebaseAuth, c.jwtSecret)).Get("/{hostID}/payout-eligibility", c.PayoutEligibility)
}

func (c *BoostController) caller(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	uid, _ := r.Context().Value(auth.ContextKeyUID).(string)
	if uid == "" {
		RespondError(w, http.StatusUnauthorized, "unauthorized")
		return uuid.Nil, false
	}
	u, err := c.userRepo.GetByAuthUID(r.Context(), uid)
	if err != nil || u == nil {
		RespondError(w, http.StatusUnauthorized, "unauthorized")
		return uuid.Nil, false
	}
	return u.ID, true
}

func adminName(r *http.Request) string {
	if s, _ := r.Context().Value(auth.ContextKeyAdminUser).(string); s != "" {
		return s
	}
	if s, _ := r.Context().Value(auth.ContextKeyUID).(string); s != "" {
		return s
	}
	return "admin"
}

func pathUUID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid "+name)
		return uuid.Nil, false
	}
	return id, true
}

// respondBoost writes a BoostError as {error: code, message}, else a 500.
func respondBoost(w http.ResponseWriter, err error) {
	var be *service.BoostError
	if errors.As(err, &be) {
		RespondJSON(w, be.Status, APIResponse{Success: false, Error: be.Code, Message: be.Message})
		return
	}
	RespondError(w, http.StatusInternalServerError, err.Error())
}

func (c *BoostController) GetEventBoost(w http.ResponseWriter, r *http.Request) {
	userID, ok := c.caller(w, r)
	if !ok {
		return
	}
	eventID, ok := pathUUID(w, r, "eventID")
	if !ok {
		return
	}
	b, err := c.boosts.EventBoost(r.Context(), userID, eventID)
	if err != nil {
		respondBoost(w, err)
		return
	}
	if b == nil {
		RespondJSON(w, http.StatusNotFound, APIResponse{Success: false, Error: "no_boost", Message: "This event has no Boost yet."})
		return
	}
	RespondSuccess(w, http.StatusOK, b)
}

func (c *BoostController) RequestBoost(w http.ResponseWriter, r *http.Request) {
	userID, ok := c.caller(w, r)
	if !ok {
		return
	}
	eventID, ok := pathUUID(w, r, "eventID")
	if !ok {
		return
	}
	var req service.BoostRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}
	b, err := c.boosts.Request(r.Context(), userID, eventID, req)
	if err != nil {
		respondBoost(w, err)
		return
	}
	RespondSuccess(w, http.StatusCreated, b)
}

func (c *BoostController) Accept(w http.ResponseWriter, r *http.Request) {
	userID, ok := c.caller(w, r)
	if !ok {
		return
	}
	id, ok := pathUUID(w, r, "boostID")
	if !ok {
		return
	}
	ua := r.UserAgent()
	if len(ua) > 500 {
		ua = ua[:500]
	}
	b, err := c.boosts.Accept(r.Context(), userID, id, clientIP(r), ua)
	if err != nil {
		respondBoost(w, err)
		return
	}
	RespondSuccess(w, http.StatusOK, b)
}

func (c *BoostController) Stop(w http.ResponseWriter, r *http.Request) {
	userID, ok := c.caller(w, r)
	if !ok {
		return
	}
	id, ok := pathUUID(w, r, "boostID")
	if !ok {
		return
	}
	b, err := c.boosts.Stop(r.Context(), userID, id)
	if err != nil {
		respondBoost(w, err)
		return
	}
	RespondSuccess(w, http.StatusOK, b)
}

func (c *BoostController) HostBoosts(w http.ResponseWriter, r *http.Request) {
	userID, ok := c.caller(w, r)
	if !ok {
		return
	}
	list, err := c.boosts.HostBoosts(r.Context(), userID)
	if err != nil {
		respondBoost(w, err)
		return
	}
	RespondSuccess(w, http.StatusOK, list)
}

func (c *BoostController) PayoutEligibility(w http.ResponseWriter, r *http.Request) {
	userID, ok := c.caller(w, r)
	if !ok {
		return
	}
	hostID, ok := pathUUID(w, r, "hostID")
	if !ok {
		return
	}
	e, err := c.boosts.PayoutEligibility(r.Context(), userID, hostID)
	if err != nil {
		respondBoost(w, err)
		return
	}
	RespondSuccess(w, http.StatusOK, e)
}

/* ---- admin ---- */

func (c *BoostController) AdminList(w http.ResponseWriter, r *http.Request) {
	list, counts, err := c.boosts.AdminList(r.Context(), r.URL.Query().Get("status"))
	if err != nil {
		respondBoost(w, err)
		return
	}
	RespondSuccess(w, http.StatusOK, map[string]any{"items": list, "counts": counts})
}

// AdminStart opens a Boost on any event from the admin (see BoostService.AdminStart).
func (c *BoostController) AdminStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		EventID string `json:"event_id"`
		service.BoostRequest
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}
	eventID, err := uuid.Parse(body.EventID)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid event_id")
		return
	}
	b, err := c.boosts.AdminStart(r.Context(), adminName(r), eventID, body.BoostRequest)
	if err != nil {
		respondBoost(w, err)
		return
	}
	RespondSuccess(w, http.StatusCreated, b)
}

func (c *BoostController) AdminGet(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "boostID")
	if !ok {
		return
	}
	d, err := c.boosts.AdminGet(r.Context(), id)
	if err != nil {
		respondBoost(w, err)
		return
	}
	RespondSuccess(w, http.StatusOK, d)
}

func (c *BoostController) Propose(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "boostID")
	if !ok {
		return
	}
	var p service.Proposal
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}
	b, err := c.boosts.Propose(r.Context(), adminName(r), id, p)
	if err != nil {
		respondBoost(w, err)
		return
	}
	RespondSuccess(w, http.StatusOK, b)
}

// AdminAccept accepts the proposal for the host (see BoostService.AdminAccept).
func (c *BoostController) AdminAccept(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "boostID")
	if !ok {
		return
	}
	var body struct {
		HowAgreed string `json:"how_agreed"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}
	ua := r.UserAgent()
	if len(ua) > 500 {
		ua = ua[:500]
	}
	b, err := c.boosts.AdminAccept(r.Context(), adminName(r), id, body.HowAgreed, clientIP(r), ua)
	if err != nil {
		respondBoost(w, err)
		return
	}
	RespondSuccess(w, http.StatusOK, b)
}

func (c *BoostController) Decline(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "boostID")
	if !ok {
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	b, err := c.boosts.Decline(r.Context(), adminName(r), id, body.Reason)
	if err != nil {
		respondBoost(w, err)
		return
	}
	RespondSuccess(w, http.StatusOK, b)
}

func (c *BoostController) GoLive(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "boostID")
	if !ok {
		return
	}
	var body struct {
		Until *time.Time `json:"live_until"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	b, err := c.boosts.GoLive(r.Context(), adminName(r), id, body.Until)
	if err != nil {
		respondBoost(w, err)
		return
	}
	RespondSuccess(w, http.StatusOK, b)
}

func (c *BoostController) End(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "boostID")
	if !ok {
		return
	}
	b, err := c.boosts.End(r.Context(), "admin", adminName(r), id, time.Now(), "admin_ended")
	if err != nil {
		respondBoost(w, err)
		return
	}
	RespondSuccess(w, http.StatusOK, b)
}

func (c *BoostController) Campaign(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "boostID")
	if !ok {
		return
	}
	var body struct {
		Reach  int64 `json:"reach"`
		Clicks int64 `json:"clicks"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}
	b, err := c.boosts.Campaign(r.Context(), adminName(r), id, body.Reach, body.Clicks)
	if err != nil {
		respondBoost(w, err)
		return
	}
	RespondSuccess(w, http.StatusOK, b)
}

func (c *BoostController) SetInvite(w http.ResponseWriter, r *http.Request) {
	hostID, ok := pathUUID(w, r, "hostID")
	if !ok {
		return
	}
	var body struct {
		Invited bool `json:"invited"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}
	if err := c.boosts.SetHostInvited(r.Context(), hostID, body.Invited); err != nil {
		respondBoost(w, err)
		return
	}
	RespondSuccess(w, http.StatusOK, map[string]any{"host_id": hostID, "invited": body.Invited})
}

func (c *BoostController) GetInvite(w http.ResponseWriter, r *http.Request) {
	hostID, ok := pathUUID(w, r, "hostID")
	if !ok {
		return
	}
	invited, err := c.boosts.HostInvited(r.Context(), hostID)
	if err != nil {
		respondBoost(w, err)
		return
	}
	RespondSuccess(w, http.StatusOK, map[string]any{"host_id": hostID, "invited": invited})
}
