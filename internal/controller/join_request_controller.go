package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	firebaseauth "firebase.google.com/go/v4/auth"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"myslotmate-backend/internal/auth"
	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"
	"myslotmate-backend/internal/service"
)

// JoinRequestController exposes the RSVP flow.
//
// Every route here is authenticated, and every identity — guest, host, admin —
// comes from the auth context. Nothing reads user_id or host_id from a body or
// URL param for authorisation: on this endpoint that would let anyone approve
// requests on anyone's event.
type JoinRequestController struct {
	svc          service.JoinRequestService
	userRepo     repository.UserRepository
	hostRepo     repository.HostRepository
	firebaseAuth *firebaseauth.Client
	adminEmail   string
	jwtSecret    string
}

func NewJoinRequestController(
	svc service.JoinRequestService,
	userRepo repository.UserRepository,
	hostRepo repository.HostRepository,
	firebaseAuth *firebaseauth.Client,
	adminEmail string,
	jwtSecret string,
) *JoinRequestController {
	return &JoinRequestController{
		svc: svc, userRepo: userRepo, hostRepo: hostRepo,
		firebaseAuth: firebaseAuth, adminEmail: adminEmail, jwtSecret: jwtSecret,
	}
}

func (c *JoinRequestController) RegisterRoutes(r chi.Router) {
	// Guest-facing: ask to join, check my own status, withdraw.
	r.Route("/events/{eventID}/join-requests", func(r chi.Router) {
		r.Use(auth.RequireUser(c.firebaseAuth, c.jwtSecret))
		r.Post("/", c.Submit)
		r.Get("/me", c.GetMine)
	})

	// Host-facing queue + decisions. The host is resolved from the auth context
	// and checked against the event's owner inside the service.
	r.Route("/join-requests", func(r chi.Router) {
		r.Use(auth.RequireUser(c.firebaseAuth, c.jwtSecret))
		r.Get("/host", c.ListForHost)
		r.Get("/host/pending-count", c.PendingCount)
		r.Get("/event/{eventID}", c.ListForEvent)
		r.Post("/{requestID}/approve", c.Approve)
		r.Post("/{requestID}/reject", c.Reject)
		r.Post("/{requestID}/withdraw", c.WithdrawMine)
	})
}

// RegisterAdminRoutes mounts the platform-admin queue behind the admin guard —
// the same one the rest of /admin/* uses, which accepts the static dashboard
// token or a Firebase admin token.
func (c *JoinRequestController) RegisterAdminRoutes(r chi.Router) {
	// Registered flat rather than via r.Route so the paths are exactly what the
	// client calls. A subrouter would register "/admin/join-requests/" (with the
	// trailing slash), and this router has no RedirectSlashes middleware — so the
	// slashless form the client sends with query params would 404.
	guard := auth.RequireAdmin(c.firebaseAuth, c.adminEmail, c.jwtSecret)
	r.With(guard).Get("/admin/join-requests", c.AdminList)
	r.With(guard).Post("/admin/join-requests/{requestID}/approve", c.AdminApprove)
	r.With(guard).Post("/admin/join-requests/{requestID}/reject", c.AdminReject)
}

// ── Identity helpers ────────────────────────────────────────────────────────

// resolveUserID derives the calling guest from the auth context.
func (c *JoinRequestController) resolveUserID(ctx context.Context) (uuid.UUID, error) {
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

// resolveHostID derives the calling host. The SINGLE source of truth for "which
// host is acting" on these routes — body/param host_id must never be used.
func (c *JoinRequestController) resolveHostID(ctx context.Context) (uuid.UUID, error) {
	userID, err := c.resolveUserID(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	host, err := c.hostRepo.GetByUserID(ctx, userID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("host lookup failed: %w", err)
	}
	if host == nil {
		return uuid.Nil, errors.New("caller is not a host")
	}
	return host.ID, nil
}

// ── Guest endpoints ─────────────────────────────────────────────────────────

type submitJoinRequestBody struct {
	Message string                  `json:"message"`
	Answers *models.AttendeeProfile `json:"answers,omitempty"`
}

func (c *JoinRequestController) Submit(w http.ResponseWriter, r *http.Request) {
	userID, err := c.resolveUserID(r.Context())
	if err != nil {
		RespondError(w, http.StatusUnauthorized, err.Error())
		return
	}
	eventID, err := uuid.Parse(chi.URLParam(r, "eventID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "invalid event id")
		return
	}
	var body submitJoinRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	req, err := c.svc.Submit(r.Context(), eventID, userID, service.JoinRequestInput{
		Message: body.Message,
		Answers: body.Answers,
	})
	if err != nil {
		RespondError(w, joinRequestStatusCode(err), err.Error())
		return
	}
	RespondSuccess(w, http.StatusCreated, req)
}

func (c *JoinRequestController) GetMine(w http.ResponseWriter, r *http.Request) {
	userID, err := c.resolveUserID(r.Context())
	if err != nil {
		RespondError(w, http.StatusUnauthorized, err.Error())
		return
	}
	eventID, err := uuid.Parse(chi.URLParam(r, "eventID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "invalid event id")
		return
	}
	// A guest who has never asked gets null, not a 404 — "not requested" is a
	// normal state the page renders a button for.
	req, err := c.svc.GetForUser(r.Context(), eventID, userID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, req)
}

func (c *JoinRequestController) WithdrawMine(w http.ResponseWriter, r *http.Request) {
	userID, err := c.resolveUserID(r.Context())
	if err != nil {
		RespondError(w, http.StatusUnauthorized, err.Error())
		return
	}
	requestID, err := uuid.Parse(chi.URLParam(r, "requestID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "invalid request id")
		return
	}
	if err := c.svc.Withdraw(r.Context(), requestID, userID); err != nil {
		RespondError(w, joinRequestStatusCode(err), err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, nil)
}

// ── Host endpoints ──────────────────────────────────────────────────────────

func (c *JoinRequestController) ListForHost(w http.ResponseWriter, r *http.Request) {
	hostID, err := c.resolveHostID(r.Context())
	if err != nil {
		RespondError(w, http.StatusUnauthorized, err.Error())
		return
	}
	limit, offset := pageParams(r)
	list, err := c.svc.ListForHost(r.Context(), hostID, r.URL.Query().Get("status"), limit, offset)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, list)
}

func (c *JoinRequestController) PendingCount(w http.ResponseWriter, r *http.Request) {
	hostID, err := c.resolveHostID(r.Context())
	if err != nil {
		RespondError(w, http.StatusUnauthorized, err.Error())
		return
	}
	n, err := c.svc.CountPendingForHost(r.Context(), hostID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, map[string]int{"pending": n})
}

func (c *JoinRequestController) ListForEvent(w http.ResponseWriter, r *http.Request) {
	hostID, err := c.resolveHostID(r.Context())
	if err != nil {
		RespondError(w, http.StatusUnauthorized, err.Error())
		return
	}
	eventID, err := uuid.Parse(chi.URLParam(r, "eventID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "invalid event id")
		return
	}
	list, err := c.svc.ListForEvent(r.Context(), eventID, hostID, r.URL.Query().Get("status"))
	if err != nil {
		RespondError(w, joinRequestStatusCode(err), err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, list)
}

type reviewBody struct {
	Note *string `json:"note,omitempty"`
}

func (c *JoinRequestController) Approve(w http.ResponseWriter, r *http.Request) {
	c.hostReview(w, r, true)
}

func (c *JoinRequestController) Reject(w http.ResponseWriter, r *http.Request) {
	c.hostReview(w, r, false)
}

func (c *JoinRequestController) hostReview(w http.ResponseWriter, r *http.Request, approve bool) {
	hostID, err := c.resolveHostID(r.Context())
	if err != nil {
		RespondError(w, http.StatusUnauthorized, err.Error())
		return
	}
	requestID, err := uuid.Parse(chi.URLParam(r, "requestID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "invalid request id")
		return
	}
	var body reviewBody
	_ = json.NewDecoder(r.Body).Decode(&body) // note is optional

	req, err := c.svc.ReviewAsHost(r.Context(), requestID, hostID, approve, body.Note)
	if err != nil {
		RespondError(w, joinRequestStatusCode(err), err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, req)
}

// ── Admin endpoints ─────────────────────────────────────────────────────────

func (c *JoinRequestController) AdminList(w http.ResponseWriter, r *http.Request) {
	limit, offset := pageParams(r)
	list, err := c.svc.ListAll(r.Context(), r.URL.Query().Get("status"), limit, offset)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, list)
}

func (c *JoinRequestController) AdminApprove(w http.ResponseWriter, r *http.Request) {
	c.adminReview(w, r, true)
}

func (c *JoinRequestController) AdminReject(w http.ResponseWriter, r *http.Request) {
	c.adminReview(w, r, false)
}

func (c *JoinRequestController) adminReview(w http.ResponseWriter, r *http.Request, approve bool) {
	requestID, err := uuid.Parse(chi.URLParam(r, "requestID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "invalid request id")
		return
	}
	var body reviewBody
	_ = json.NewDecoder(r.Body).Decode(&body)

	// The admin route group already authenticated the caller. The username is
	// recorded for the audit trail only; authorisation is the middleware's job.
	req, err := c.svc.ReviewAsAdmin(r.Context(), requestID, adminLabelFromContext(r.Context()), approve, body.Note)
	if err != nil {
		RespondError(w, joinRequestStatusCode(err), err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, req)
}

// ── Helpers ─────────────────────────────────────────────────────────────────

// adminLabelFromContext reads the admin's username. Admin JWTs carry no UUID,
// so the username is the only stable identifier for the audit trail.
func adminLabelFromContext(ctx context.Context) string {
	if raw, ok := ctx.Value(auth.ContextKeyAdminUser).(string); ok {
		return raw
	}
	return ""
}

// joinRequestStatusCode maps the service's sentinel errors onto HTTP so the
// client can tell "already decided" from "not yours" from "broken".
func joinRequestStatusCode(err error) int {
	switch {
	case errors.Is(err, service.ErrJoinRequestForbidden):
		return http.StatusForbidden
	case errors.Is(err, service.ErrJoinRequestNotFound):
		return http.StatusNotFound
	case errors.Is(err, service.ErrJoinRequestExists),
		errors.Is(err, service.ErrJoinRequestNotPending):
		return http.StatusConflict
	case errors.Is(err, service.ErrJoinRequestNotApplicable):
		return http.StatusBadRequest
	default:
		return http.StatusBadRequest
	}
}

func pageParams(r *http.Request) (int, int) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	return limit, offset
}
