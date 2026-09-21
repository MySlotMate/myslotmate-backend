package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	fbauth "firebase.google.com/go/v4/auth"

	"myslotmate-backend/internal/auth"
	"myslotmate-backend/internal/repository"
	"myslotmate-backend/internal/service"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type PassController struct {
	passService  service.PassService
	userRepo     repository.UserRepository
	hostRepo     repository.HostRepository
	firebaseAuth *fbauth.Client
	jwtSecret    string
}

func NewPassController(
	s service.PassService,
	ur repository.UserRepository,
	hr repository.HostRepository,
	fa *fbauth.Client,
	jwtSecret string,
) *PassController {
	return &PassController{passService: s, userRepo: ur, hostRepo: hr, firebaseAuth: fa, jwtSecret: jwtSecret}
}

func (c *PassController) RegisterRoutes(r chi.Router) {
	r.Route("/passes", func(r chi.Router) {
		// A pass purchase moves real money, so the buyer is taken from the
		// verified token — never from the request body (skill golden rule 2).
		r.Use(auth.RequireUser(c.firebaseAuth, c.jwtSecret))

		r.Post("/", c.PurchasePass)
		r.Get("/me", c.GetMyPasses)
		r.Get("/event/{eventID}", c.GetPassForEvent)
		r.Post("/{passID}/cancel", c.CancelPass)
		r.Post("/{passID}/reserve", c.ReserveSession)

		// Host roster of everyone holding a pass on their own experience.
		r.Get("/host/event/{eventID}", c.ListHolders)
	})
}

// resolveUserID derives the caller's user UUID from the auth context. This is
// the SINGLE source of truth for "who is buying" on /passes/*.
func (c *PassController) resolveUserID(ctx context.Context) (uuid.UUID, error) {
	uid, ok := ctx.Value(auth.ContextKeyUID).(string)
	if !ok || uid == "" {
		return uuid.Nil, errors.New("unauthenticated")
	}
	user, err := c.userRepo.GetByAuthUID(ctx, uid)
	if err != nil {
		return uuid.Nil, fmt.Errorf("user lookup failed: %w", err)
	}
	if user == nil {
		return uuid.Nil, errors.New("user not found")
	}
	return user.ID, nil
}

type purchasePassRequest struct {
	EventID        uuid.UUID `json:"event_id"`
	IdempotencyKey string    `json:"idempotency_key,omitempty"`
}

func (c *PassController) PurchasePass(w http.ResponseWriter, r *http.Request) {
	userID, err := c.resolveUserID(r.Context())
	if err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}
	var req purchasePassRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}
	if req.EventID == uuid.Nil {
		RespondError(w, http.StatusBadRequest, "event_id is required")
		return
	}

	pass, err := c.passService.PurchasePass(r.Context(), userID, req.EventID, req.IdempotencyKey)
	if err != nil {
		RespondError(w, passErrorStatus(err), err.Error())
		return
	}
	RespondSuccess(w, http.StatusCreated, pass)
}

func (c *PassController) CancelPass(w http.ResponseWriter, r *http.Request) {
	userID, err := c.resolveUserID(r.Context())
	if err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}
	passID, err := uuid.Parse(chi.URLParam(r, "passID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid pass ID")
		return
	}

	pass, err := c.passService.CancelPass(r.Context(), userID, passID)
	if err != nil {
		RespondError(w, passErrorStatus(err), err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, pass)
}

type reserveSessionRequest struct {
	OccurrenceDate string `json:"occurrence_date"` // RFC3339
}

// ReserveSession holds a seat for a pass holder on one covered date. Free — the
// pass was paid for up front — but it still takes a real seat, so the host's
// capacity and roster stay honest.
func (c *PassController) ReserveSession(w http.ResponseWriter, r *http.Request) {
	userID, err := c.resolveUserID(r.Context())
	if err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}
	passID, err := uuid.Parse(chi.URLParam(r, "passID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid pass ID")
		return
	}
	var req reserveSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}
	occurrence, err := time.Parse(time.RFC3339, req.OccurrenceDate)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid occurrence_date format; expected RFC3339")
		return
	}

	booking, err := c.passService.ReserveSession(r.Context(), userID, passID, occurrence)
	if err != nil {
		RespondError(w, passErrorStatus(err), err.Error())
		return
	}
	RespondSuccess(w, http.StatusCreated, booking)
}

func (c *PassController) GetMyPasses(w http.ResponseWriter, r *http.Request) {
	userID, err := c.resolveUserID(r.Context())
	if err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}
	passes, err := c.passService.GetMyPasses(r.Context(), userID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, passes)
}

func (c *PassController) GetPassForEvent(w http.ResponseWriter, r *http.Request) {
	userID, err := c.resolveUserID(r.Context())
	if err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}
	eventID, err := uuid.Parse(chi.URLParam(r, "eventID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid event ID")
		return
	}

	pass, passesLeft, err := c.passService.GetPassForEvent(r.Context(), userID, eventID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, map[string]interface{}{
		"pass":        pass,       // null when the guest holds none
		"passes_left": passesLeft, // null when the event caps nothing
	})
}

func (c *PassController) ListHolders(w http.ResponseWriter, r *http.Request) {
	userID, err := c.resolveUserID(r.Context())
	if err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}
	host, err := c.hostRepo.GetByUserID(r.Context(), userID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if host == nil {
		RespondError(w, http.StatusForbidden, "caller is not a host")
		return
	}
	eventID, err := uuid.Parse(chi.URLParam(r, "eventID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid event ID")
		return
	}

	holders, err := c.passService.ListHolders(r.Context(), host.ID, eventID)
	if err != nil {
		RespondError(w, passErrorStatus(err), err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, holders)
}

// passErrorStatus maps the service's user-facing errors onto HTTP codes, so the
// client can tell "top up your wallet" from "this sold out".
func passErrorStatus(err error) int {
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "insufficient wallet balance"):
		return http.StatusPaymentRequired
	case msg == "event not found", msg == "pass not found", msg == "user account not found":
		return http.StatusNotFound
	case msg == "you already have an active pass for this experience",
		msg == "no passes left for this experience",
		msg == "event capacity exceeded",
		msg == "this pass has already been refunded":
		return http.StatusConflict
	case msg == "your account is blocked due to suspicious activity",
		msg == "not authorized to cancel this pass",
		msg == "not authorized to use this pass",
		msg == "not your experience":
		return http.StatusForbidden
	case msg == "your pass does not cover this session",
		msg == "a monthly pass covers one guest per session",
		msg == "this experience does not offer a monthly pass",
		msg == "this experience is not taking bookings right now",
		msg == "this pass can no longer be cancelled for a refund":
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}
