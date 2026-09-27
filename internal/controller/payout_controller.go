package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	fbauth "firebase.google.com/go/v4/auth"

	"myslotmate-backend/internal/auth"
	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"
	"myslotmate-backend/internal/service"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type PayoutController struct {
	payoutService service.PayoutService
	userRepo      repository.UserRepository
	hostRepo      repository.HostRepository
	firebaseAuth  *fbauth.Client
	jwtSecret     string
}

func NewPayoutController(
	s service.PayoutService,
	ur repository.UserRepository,
	hr repository.HostRepository,
	fa *fbauth.Client,
	jwtSecret string,
) *PayoutController {
	return &PayoutController{
		payoutService: s,
		userRepo:      ur,
		hostRepo:      hr,
		firebaseAuth:  fa,
		jwtSecret:     jwtSecret,
	}
}

func (c *PayoutController) RegisterRoutes(r chi.Router) {
	r.Route("/payouts", func(r chi.Router) {
		// All /payouts/* endpoints require an authenticated user. The host
		// identity is derived from the auth context (resolveHostID) — body
		// fields and URL params named host_id are IGNORED for ownership.
		// Closes bug C1 (unauthenticated payout drain).
		r.Use(auth.RequireUser(c.firebaseAuth, c.jwtSecret))

		// Payout Methods
		r.Post("/methods", c.AddPayoutMethod)
		r.Get("/methods/{hostID}", c.ListPayoutMethods)
		r.Put("/methods/{methodID}/primary", c.SetPrimaryMethod)
		r.Delete("/methods/{methodID}", c.DeletePayoutMethod)

		// Withdrawals
		r.Post("/withdraw", c.RequestWithdrawal)

		// Earnings
		r.Get("/earnings/{hostID}", c.GetEarningsSummary)

		// Sales — list of bookings on the host's events (buyer + event + amount)
		r.Get("/sales", c.GetHostSales)

		// Payout History
		r.Get("/history/{hostID}", c.GetPayoutHistory)
	})

	// Co-hosting — sharing ONE event's earnings with another host.
	r.Route("/cohosts", func(r chi.Router) {
		r.Use(auth.RequireUser(c.firebaseAuth, c.jwtSecret))

		// Owner side
		r.Post("/invite", c.InviteCoHost)
		r.Get("/event/{eventID}", c.ListEventCoHosts)
		r.Patch("/{cohostID}/withdraw-permission", c.SetCoHostCanWithdraw)
		r.Delete("/{cohostID}", c.RevokeCoHost)
		r.Post("/{cohostID}/resend", c.ResendCoHostInvite)

		// Co-host side
		r.Get("/shared", c.ListSharedEvents)
		r.Get("/summary", c.GetCoHostSummary)
		r.Post("/{cohostID}/respond", c.RespondToCoHostInvite)
		r.Post("/{cohostID}/withdraw", c.RequestCoHostWithdrawal)
	})
}

// resolveHostID derives the caller's host UUID from the auth context. Returns
// an error if the caller is not authenticated or is not registered as a host.
// This is the SINGLE source of truth for "which host is acting" on /payouts/*.
// Body fields and URL params named host_id MUST be ignored — closes C1 / H5.
func (c *PayoutController) resolveHostID(ctx context.Context) (uuid.UUID, error) {
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
	host, err := c.hostRepo.GetByUserID(ctx, user.ID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("host lookup failed: %w", err)
	}
	if host == nil {
		return uuid.Nil, errors.New("caller is not a host")
	}
	return host.ID, nil
}

// ── Payout Methods ──────────────────────────────────────────────────────────

type AddPayoutMethodReq struct {
	HostID          uuid.UUID               `json:"host_id"`
	Type            models.PayoutMethodType `json:"type"`
	BankName        *string                 `json:"bank_name,omitempty"`
	AccountType     *string                 `json:"account_type,omitempty"`
	AccountNumber   *string                 `json:"account_number,omitempty"`
	IFSC            *string                 `json:"ifsc,omitempty"`
	BeneficiaryName *string                 `json:"beneficiary_name,omitempty"`
	UPIID           *string                 `json:"upi_id,omitempty"`
}

func (c *PayoutController) AddPayoutMethod(w http.ResponseWriter, r *http.Request) {
	hostID, err := c.resolveHostID(r.Context())
	if err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}
	var req AddPayoutMethodReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	svcReq := service.AddPayoutMethodRequest{
		Type:            req.Type,
		BankName:        req.BankName,
		AccountType:     req.AccountType,
		AccountNumber:   req.AccountNumber,
		IFSC:            req.IFSC,
		BeneficiaryName: req.BeneficiaryName,
		UPIID:           req.UPIID,
	}

	pm, err := c.payoutService.AddPayoutMethod(r.Context(), hostID, svcReq)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusCreated, pm)
}

func (c *PayoutController) ListPayoutMethods(w http.ResponseWriter, r *http.Request) {
	hostID, err := c.resolveHostID(r.Context())
	if err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}

	methods, err := c.payoutService.ListPayoutMethods(r.Context(), hostID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, methods)
}

func (c *PayoutController) SetPrimaryMethod(w http.ResponseWriter, r *http.Request) {
	hostID, err := c.resolveHostID(r.Context())
	if err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}
	methodID, err := uuid.Parse(chi.URLParam(r, "methodID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid method ID")
		return
	}

	if err := c.payoutService.SetPrimaryMethod(r.Context(), hostID, methodID); err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, map[string]string{"message": "primary method updated"})
}

func (c *PayoutController) DeletePayoutMethod(w http.ResponseWriter, r *http.Request) {
	hostID, err := c.resolveHostID(r.Context())
	if err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}
	methodID, err := uuid.Parse(chi.URLParam(r, "methodID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid method ID")
		return
	}

	if err := c.payoutService.DeletePayoutMethod(r.Context(), hostID, methodID); err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, map[string]string{"message": "payout method deleted"})
}

// ── Withdrawal ──────────────────────────────────────────────────────────────

type WithdrawalReq struct {
	HostID         uuid.UUID  `json:"host_id"`
	AmountCents    int64      `json:"amount_cents"`
	PayoutMethodID *uuid.UUID `json:"payout_method_id,omitempty"`
	IdempotencyKey string     `json:"idempotency_key"`
}

func (c *PayoutController) RequestWithdrawal(w http.ResponseWriter, r *http.Request) {
	hostID, err := c.resolveHostID(r.Context())
	if err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}
	var req WithdrawalReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}
	// req.HostID is intentionally ignored — closes C1 (anyone-can-withdraw-as-any-host).

	svcReq := service.WithdrawalRequest{
		AmountCents:    req.AmountCents,
		PayoutMethodID: req.PayoutMethodID,
		IdempotencyKey: req.IdempotencyKey,
	}

	payment, err := c.payoutService.RequestWithdrawal(r.Context(), hostID, svcReq)
	if err != nil {
		RespondError(w, http.StatusBadRequest, err.Error())
		return
	}

	RespondSuccess(w, http.StatusCreated, payment)
}

// ── Earnings ────────────────────────────────────────────────────────────────

func (c *PayoutController) GetEarningsSummary(w http.ResponseWriter, r *http.Request) {
	hostID, err := c.resolveHostID(r.Context())
	if err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}

	summary, err := c.payoutService.GetEarningsSummary(r.Context(), hostID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, summary)
}

// ── Sales (incoming bookings on host's events) ──────────────────────────────

func (c *PayoutController) GetHostSales(w http.ResponseWriter, r *http.Request) {
	hostID, err := c.resolveHostID(r.Context())
	if err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}
	limit := 50
	offset := 0
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	if o := r.URL.Query().Get("offset"); o != "" {
		if parsed, err := strconv.Atoi(o); err == nil && parsed >= 0 {
			offset = parsed
		}
	}
	// Optional `from_date` (RFC 3339) — default range for the UI is last 90d.
	var fromDate *time.Time
	if s := r.URL.Query().Get("from_date"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			fromDate = &t
		}
	}
	sales, err := c.payoutService.GetHostSales(r.Context(), hostID, limit, offset, fromDate)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, sales)
}

// ── Payout History ──────────────────────────────────────────────────────────

func (c *PayoutController) GetPayoutHistory(w http.ResponseWriter, r *http.Request) {
	hostID, err := c.resolveHostID(r.Context())
	if err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}

	limit := 20
	offset := 0
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	if o := r.URL.Query().Get("offset"); o != "" {
		if parsed, err := strconv.Atoi(o); err == nil && parsed >= 0 {
			offset = parsed
		}
	}

	payments, err := c.payoutService.GetPayoutHistory(r.Context(), hostID, limit, offset)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, payments)
}

// ── Co-hosting ──────────────────────────────────────────────────────────────

type InviteCoHostReq struct {
	EventID uuid.UUID `json:"event_id"`
	Email   string    `json:"email"`
	// CanWithdraw grants earnings access up front, so the owner does not have to
	// come back and flip the toggle after the invite is accepted.
	CanWithdraw bool `json:"can_withdraw"`
}

func (c *PayoutController) InviteCoHost(w http.ResponseWriter, r *http.Request) {
	hostID, err := c.resolveHostID(r.Context())
	if err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}
	var req InviteCoHostReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	ch, err := c.payoutService.InviteCoHost(r.Context(), hostID, req.EventID, req.Email, req.CanWithdraw)
	if err != nil {
		RespondError(w, http.StatusBadRequest, err.Error())
		return
	}
	RespondSuccess(w, http.StatusCreated, ch)
}

func (c *PayoutController) ListEventCoHosts(w http.ResponseWriter, r *http.Request) {
	hostID, err := c.resolveHostID(r.Context())
	if err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}
	eventID, err := uuid.Parse(chi.URLParam(r, "eventID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "invalid event id")
		return
	}
	rows, err := c.payoutService.ListEventCoHosts(r.Context(), hostID, eventID)
	if err != nil {
		RespondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if rows == nil {
		rows = []*repository.CoHostRow{}
	}
	RespondSuccess(w, http.StatusOK, rows)
}

type SetCoHostCanWithdrawReq struct {
	CanWithdraw bool `json:"can_withdraw"`
}

func (c *PayoutController) SetCoHostCanWithdraw(w http.ResponseWriter, r *http.Request) {
	hostID, err := c.resolveHostID(r.Context())
	if err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}
	cohostID, err := uuid.Parse(chi.URLParam(r, "cohostID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "invalid co-host id")
		return
	}
	var req SetCoHostCanWithdrawReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := c.payoutService.SetCoHostCanWithdraw(r.Context(), hostID, cohostID, req.CanWithdraw); err != nil {
		RespondError(w, http.StatusBadRequest, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, map[string]bool{"can_withdraw": req.CanWithdraw})
}

func (c *PayoutController) RevokeCoHost(w http.ResponseWriter, r *http.Request) {
	hostID, err := c.resolveHostID(r.Context())
	if err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}
	cohostID, err := uuid.Parse(chi.URLParam(r, "cohostID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "invalid co-host id")
		return
	}
	if err := c.payoutService.RevokeCoHost(r.Context(), hostID, cohostID); err != nil {
		RespondError(w, http.StatusBadRequest, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, map[string]string{"status": "revoked"})
}

func (c *PayoutController) ResendCoHostInvite(w http.ResponseWriter, r *http.Request) {
	hostID, err := c.resolveHostID(r.Context())
	if err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}
	cohostID, err := uuid.Parse(chi.URLParam(r, "cohostID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "invalid co-host id")
		return
	}
	if err := c.payoutService.ResendCoHostInvite(r.Context(), hostID, cohostID); err != nil {
		RespondError(w, http.StatusBadRequest, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, map[string]string{"status": "sent"})
}

func (c *PayoutController) GetCoHostSummary(w http.ResponseWriter, r *http.Request) {
	hostID, err := c.resolveHostID(r.Context())
	if err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}
	summary, err := c.payoutService.CoHostSummary(r.Context(), hostID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, summary)
}

func (c *PayoutController) ListSharedEvents(w http.ResponseWriter, r *http.Request) {
	hostID, err := c.resolveHostID(r.Context())
	if err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}
	rows, err := c.payoutService.ListSharedEvents(r.Context(), hostID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if rows == nil {
		rows = []*repository.SharedEventRow{}
	}
	RespondSuccess(w, http.StatusOK, rows)
}

type RespondCoHostReq struct {
	Accept bool `json:"accept"`
}

func (c *PayoutController) RespondToCoHostInvite(w http.ResponseWriter, r *http.Request) {
	hostID, err := c.resolveHostID(r.Context())
	if err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}
	cohostID, err := uuid.Parse(chi.URLParam(r, "cohostID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "invalid co-host id")
		return
	}
	var req RespondCoHostReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := c.payoutService.RespondToCoHostInvite(r.Context(), hostID, cohostID, req.Accept); err != nil {
		RespondError(w, http.StatusBadRequest, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, map[string]bool{"accepted": req.Accept})
}

type CoHostWithdrawReq struct {
	AmountCents    int64      `json:"amount_cents"` // 0 = whole available pool
	PayoutMethodID *uuid.UUID `json:"payout_method_id,omitempty"`
	IdempotencyKey string     `json:"idempotency_key,omitempty"`
}

func (c *PayoutController) RequestCoHostWithdrawal(w http.ResponseWriter, r *http.Request) {
	hostID, err := c.resolveHostID(r.Context())
	if err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}
	cohostID, err := uuid.Parse(chi.URLParam(r, "cohostID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "invalid co-host id")
		return
	}
	var req CoHostWithdrawReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	payment, err := c.payoutService.RequestCoHostWithdrawal(r.Context(), hostID, cohostID, service.WithdrawalRequest{
		AmountCents:    req.AmountCents,
		PayoutMethodID: req.PayoutMethodID,
		IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		RespondError(w, http.StatusBadRequest, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, payment)
}
