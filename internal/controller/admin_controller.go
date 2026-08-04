package controller

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"myslotmate-backend/internal/auth"
	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"
	"myslotmate-backend/internal/service"

	fbauth "firebase.google.com/go/v4/auth"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// AdminController handles admin-only host-application management and platform payouts.
type AdminController struct {
	hostService   service.HostService
	payoutService service.PayoutService
	userService   service.UserService
	dirRepo       *repository.AdminDirectoryRepository
	firebaseAuth  *fbauth.Client
	adminEmail    string
	jwtSecret     string
}

func NewAdminController(hs service.HostService, ps service.PayoutService, us service.UserService, dir *repository.AdminDirectoryRepository, fa *fbauth.Client, adminEmail, jwtSecret string) *AdminController {
	return &AdminController{
		hostService:   hs,
		payoutService: ps,
		userService:   us,
		dirRepo:       dir,
		firebaseAuth:  fa,
		adminEmail:    adminEmail,
		jwtSecret:     jwtSecret,
	}
}

func (c *AdminController) RegisterRoutes(r chi.Router) {
	r.Route("/admin/hosts", func(r chi.Router) {
		// Accepts the static admin-dashboard token OR a Firebase admin token.
		r.Use(auth.RequireAdmin(c.firebaseAuth, c.adminEmail, c.jwtSecret))

		r.Get("/applications", c.ListPendingApplications)
		r.Post("/", c.CreateHost)
		r.Post("/{hostID}/approve", c.ApproveApplication)
		r.Post("/{hostID}/reject", c.RejectApplication)
		r.Put("/{hostID}/application-status", c.UpdateApplicationStatus)
		r.Put("/{hostID}/platform-fee", c.SetHostPlatformFee)
		r.Put("/{hostID}/profile", c.UpdateHostProfile)
		r.Put("/{hostID}/active", c.SetHostActive)
		r.Get("/{hostID}/earnings", c.GetHostEarnings)
	})

	r.Route("/admin/platform", func(r chi.Router) {
		// Accepts the static admin-dashboard token OR a Firebase admin token.
		r.Use(auth.RequireAdmin(c.firebaseAuth, c.adminEmail, c.jwtSecret))

		r.Get("/balance", c.GetPlatformBalance)
		r.Get("/fee-config", c.GetPlatformFeeConfig)
		r.Post("/payout-methods", c.AddAdminPayoutMethod)
		r.Get("/payout-methods", c.ListAdminPayoutMethods)
		r.Put("/payout-methods/{methodID}/primary", c.SetAdminPrimaryMethod)
		r.Delete("/payout-methods/{methodID}", c.DeleteAdminPayoutMethod)
		r.Post("/withdraw", c.RequestAdminWithdrawal)
	})

	r.Route("/admin/payments", func(r chi.Router) {
		// Admin-only "refund to source" — sends a top-up's money back to the
		// customer's original card/UPI via the Razorpay Refunds API. The default
		// refund (cancellation → wallet) is unchanged; this is the escape hatch
		// for special cases (disputes, chargebacks, regulatory).
		r.Use(auth.RequireAdmin(c.firebaseAuth, c.adminEmail, c.jwtSecret))
		r.Post("/{paymentID}/source-refund", c.RequestSourceRefund)

		// Manual "Sync payout status" — polls Cashfree for every payout stuck in
		// 'processing' and finalizes those that reached a terminal state.
		r.Post("/reconcile-payouts", c.ReconcilePayouts)

		// Read-only admin Payments dashboard views.
		r.Get("/list", c.ListPayments)
		r.Get("/ledger", c.ListLedger)
		r.Get("/balances", c.ListBalances)
		r.Get("/summary", c.GetPaymentsSummary)
	})

	r.Get("/platform-settings/{key}", c.GetPlatformSetting)

	r.Route("/admin/platform-settings", func(r chi.Router) {
		r.Use(auth.RequireAdmin(c.firebaseAuth, c.adminEmail, c.jwtSecret))
		r.Put("/{key}", c.SavePlatformSetting)
	})
}

// ── Request types ───────────────────────────────────────────────────────────

type RejectApplicationRequestBody struct {
	Reason string `json:"reason"`
}

// ── Handlers ────────────────────────────────────────────────────────────────

func (c *AdminController) ListPendingApplications(w http.ResponseWriter, r *http.Request) {
	hosts, err := c.hostService.ListPendingApplications(r.Context())
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, hosts)
}

// CreateHost onboards an existing user as a host in one step: it records the
// same application the user would have submitted themselves, then approves it,
// so the host is live immediately without a trip through the review queue.
// The user must already exist — hosts.user_id is a NOT NULL UNIQUE FK — and
// their phone number is taken from their user record, as on self-signup.
func (c *AdminController) CreateHost(w http.ResponseWriter, r *http.Request) {
	var req HostApplicationRequestBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}
	if req.UserID == uuid.Nil {
		RespondError(w, http.StatusBadRequest, "user_id is required")
		return
	}

	// Submit first, then approve: approval is what verifies the user, stamps
	// approved_at and fires the host-approved event, so it can't be skipped by
	// writing the approved status directly.
	host, err := c.hostService.SubmitApplication(r.Context(), req.UserID, toHostApplicationRequest(req))
	if err != nil {
		RespondError(w, http.StatusBadRequest, err.Error())
		return
	}

	approved, err := c.hostService.ApproveApplication(r.Context(), host.ID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusCreated, approved)
}

func (c *AdminController) ApproveApplication(w http.ResponseWriter, r *http.Request) {
	hostIDStr := chi.URLParam(r, "hostID")
	hostID, err := uuid.Parse(hostIDStr)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid host ID")
		return
	}

	host, err := c.hostService.ApproveApplication(r.Context(), hostID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, host)
}

func (c *AdminController) RejectApplication(w http.ResponseWriter, r *http.Request) {
	hostIDStr := chi.URLParam(r, "hostID")
	hostID, err := uuid.Parse(hostIDStr)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid host ID")
		return
	}

	var req RejectApplicationRequestBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	host, err := c.hostService.RejectApplication(r.Context(), hostID, req.Reason)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, host)
}

// UpdateApplicationStatusRequestBody is the payload for setting a host's
// application status directly.
type UpdateApplicationStatusRequestBody struct {
	Status string `json:"status"`
}

// validHostApplicationStatuses maps accepted request values to enum values.
var validHostApplicationStatuses = map[string]models.HostApplicationStatus{
	"draft":        models.HostApplicationDraft,
	"pending":      models.HostApplicationPending,
	"under_review": models.HostApplicationUnderReview,
	"approved":     models.HostApplicationApproved,
	"rejected":     models.HostApplicationRejected,
}

// UpdateApplicationStatus sets a host's application status to any valid value
// (admin override from the dashboard's Hosts directory).
func (c *AdminController) UpdateApplicationStatus(w http.ResponseWriter, r *http.Request) {
	hostID, err := uuid.Parse(chi.URLParam(r, "hostID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid host ID")
		return
	}

	var req UpdateApplicationStatusRequestBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	status, ok := validHostApplicationStatuses[strings.ToLower(strings.TrimSpace(req.Status))]
	if !ok {
		RespondError(w, http.StatusBadRequest, "Invalid application status")
		return
	}

	host, err := c.hostService.SetApplicationStatus(r.Context(), hostID, status)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, host)
}

// SetHostPlatformFeeRequestBody is the payload for overriding a host's
// commission split. PlatformPercentage is the platform's cut (host keeps the
// remainder); pass null to clear the override and fall back to the global
// platform_settings default (currently 70/30).
type SetHostPlatformFeeRequestBody struct {
	PlatformPercentage *int `json:"platform_percentage"`
}

// SetHostPlatformFee lets an admin set a per-host commission override, e.g.
// an 80/20 host-favorable split instead of the platform-wide default.
func (c *AdminController) SetHostPlatformFee(w http.ResponseWriter, r *http.Request) {
	hostID, err := uuid.Parse(chi.URLParam(r, "hostID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid host ID")
		return
	}

	var req SetHostPlatformFeeRequestBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	host, err := c.hostService.SetPlatformFeePercentage(r.Context(), hostID, req.PlatformPercentage)
	if err != nil {
		RespondError(w, http.StatusBadRequest, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, host)
}

// UpdateHostProfile lets an admin edit a host's profile data (image, bio,
// description, contact, tags, badges, government ID, etc.) from the dashboard.
// It reuses the service's partial-update semantics: only fields present in the
// body are changed. Unlike the host self-edit path it does not re-scrape
// Instagram. Application status and commission split have their own endpoints.
func (c *AdminController) UpdateHostProfile(w http.ResponseWriter, r *http.Request) {
	hostID, err := uuid.Parse(chi.URLParam(r, "hostID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid host ID")
		return
	}

	var req service.HostProfileUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	host, err := c.hostService.AdminUpdateProfile(r.Context(), hostID, req)
	if err != nil {
		RespondError(w, http.StatusBadRequest, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, host)
}

// GetHostEarnings returns a host's live earnings + balance breakdown — the same
// figures the host sees on their own dashboard (total, available, pending
// clearance, in-flight payouts, current balance) — for the admin Host profile.
func (c *AdminController) GetHostEarnings(w http.ResponseWriter, r *http.Request) {
	hostID, err := uuid.Parse(chi.URLParam(r, "hostID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid host ID")
		return
	}

	summary, err := c.payoutService.GetEarningsSummary(r.Context(), hostID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, summary)
}

// ReconcilePayouts polls Cashfree for every payout stuck in 'processing' and
// finalizes the ones that reached a terminal state — the manual "Sync payout
// status" action. Returns a summary (checked / finalized / skipped / errors).
func (c *AdminController) ReconcilePayouts(w http.ResponseWriter, r *http.Request) {
	res, err := c.payoutService.ReconcilePendingPayouts(r.Context())
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, res)
}

// SetHostActive deactivates (active=false) or reactivates (active=true) a host.
// Deactivation hides the host + their events from the public site; data is kept.
func (c *AdminController) SetHostActive(w http.ResponseWriter, r *http.Request) {
	hostID, err := uuid.Parse(chi.URLParam(r, "hostID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid host ID")
		return
	}

	var body struct {
		Active bool `json:"active"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	if err := c.hostService.SetHostActive(r.Context(), hostID, body.Active); err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, map[string]bool{"is_active": body.Active})
}

// ── Admin Platform Payout Handlers ──────────────────────────────────────────

func (c *AdminController) GetPlatformBalance(w http.ResponseWriter, r *http.Request) {
	balance, err := c.payoutService.GetPlatformBalance(r.Context())
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, balance)
}

// GetPlatformFeeConfig returns the effective global commission split (the
// fallback applied to any host without a per-host override) so the admin
// dashboard can show what "default" actually means.
func (c *AdminController) GetPlatformFeeConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := c.payoutService.GetPlatformFeeConfig(r.Context())
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, cfg)
}

func (c *AdminController) AddAdminPayoutMethod(w http.ResponseWriter, r *http.Request) {
	var reqBody struct {
		Type            string  `json:"type"`
		BankName        *string `json:"bank_name"`
		AccountType     *string `json:"account_type"`
		AccountNumber   *string `json:"account_number"`
		IFSC            *string `json:"ifsc"`
		BeneficiaryName *string `json:"beneficiary_name"`
		UPIID           *string `json:"upi_id"`
	}

	if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	methodType := ""
	switch reqBody.Type {
	case "bank":
		methodType = "bank"
	case "upi":
		methodType = "upi"
	default:
		RespondError(w, http.StatusBadRequest, "Invalid payout method type")
		return
	}

	req := service.AddPayoutMethodRequest{
		Type:            models.PayoutMethodType(methodType),
		BankName:        reqBody.BankName,
		AccountType:     reqBody.AccountType,
		AccountNumber:   reqBody.AccountNumber,
		IFSC:            reqBody.IFSC,
		BeneficiaryName: reqBody.BeneficiaryName,
		UPIID:           reqBody.UPIID,
	}

	method, err := c.payoutService.AddAdminPayoutMethod(r.Context(), req)
	if err != nil {
		RespondError(w, http.StatusBadRequest, err.Error())
		return
	}

	RespondSuccess(w, http.StatusCreated, method)
}

func (c *AdminController) ListAdminPayoutMethods(w http.ResponseWriter, r *http.Request) {
	methods, err := c.payoutService.ListAdminPayoutMethods(r.Context())
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, methods)
}

func (c *AdminController) SetAdminPrimaryMethod(w http.ResponseWriter, r *http.Request) {
	methodIDStr := chi.URLParam(r, "methodID")
	methodID, err := uuid.Parse(methodIDStr)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid method ID")
		return
	}

	if err := c.payoutService.SetAdminPrimaryMethod(r.Context(), methodID); err != nil {
		RespondError(w, http.StatusBadRequest, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, map[string]string{"message": "Primary method updated"})
}

func (c *AdminController) DeleteAdminPayoutMethod(w http.ResponseWriter, r *http.Request) {
	methodIDStr := chi.URLParam(r, "methodID")
	methodID, err := uuid.Parse(methodIDStr)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid method ID")
		return
	}

	if err := c.payoutService.DeleteAdminPayoutMethod(r.Context(), methodID); err != nil {
		RespondError(w, http.StatusBadRequest, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, map[string]string{"message": "Method deleted"})
}

func (c *AdminController) RequestAdminWithdrawal(w http.ResponseWriter, r *http.Request) {
	var reqBody struct {
		AmountCents    int64   `json:"amount_cents"`
		PayoutMethodID *string `json:"payout_method_id"`
		IdempotencyKey string  `json:"idempotency_key"`
	}

	if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	var methodID *uuid.UUID
	if reqBody.PayoutMethodID != nil {
		parsed, err := uuid.Parse(*reqBody.PayoutMethodID)
		if err != nil {
			RespondError(w, http.StatusBadRequest, "Invalid payout method ID")
			return
		}
		methodID = &parsed
	}

	req := service.WithdrawalRequest{
		AmountCents:    reqBody.AmountCents,
		PayoutMethodID: methodID,
		IdempotencyKey: reqBody.IdempotencyKey,
	}

	payment, err := c.payoutService.RequestAdminWithdrawal(r.Context(), req)
	if err != nil {
		RespondError(w, http.StatusBadRequest, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, payment)
}

// ── Admin "refund to source" ────────────────────────────────────────────────

type SourceRefundRequestBody struct {
	AmountCents    int64  `json:"amount_cents"`
	Reason         string `json:"reason"`
	IdempotencyKey string `json:"idempotency_key"`
}

// RequestSourceRefund initiates a Razorpay refund of the {paymentID} top-up
// back to the customer's original payment instrument. Admin-only.
func (c *AdminController) RequestSourceRefund(w http.ResponseWriter, r *http.Request) {
	topupID, err := uuid.Parse(chi.URLParam(r, "paymentID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid payment ID")
		return
	}

	var body SourceRefundRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	refund, err := c.userService.RefundTopUpToSource(r.Context(), topupID, service.SourceRefundRequest{
		AmountCents:    body.AmountCents,
		Reason:         body.Reason,
		IdempotencyKey: body.IdempotencyKey,
	})
	if err != nil {
		RespondError(w, http.StatusBadRequest, err.Error())
		return
	}

	RespondSuccess(w, http.StatusCreated, refund)
}

func (c *AdminController) GetPlatformSetting(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	if key == "" {
		RespondError(w, http.StatusBadRequest, "Missing key parameter")
		return
	}

	setting, err := c.payoutService.GetPlatformSetting(r.Context(), key)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if setting == nil {
		RespondSuccess(w, http.StatusOK, json.RawMessage("{}"))
		return
	}

	RespondSuccess(w, http.StatusOK, setting)
}

func (c *AdminController) SavePlatformSetting(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	if key == "" {
		RespondError(w, http.StatusBadRequest, "Missing key parameter")
		return
	}

	var value json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&value); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid JSON payload")
		return
	}

	err := c.payoutService.SavePlatformSetting(r.Context(), key, value)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, map[string]string{"message": "Setting saved successfully"})
}

// ── Payments dashboard (read-only views) ──────────────────────────────────────

type adminPaymentDTO struct {
	ID               string `json:"id"`
	Type             string `json:"type"`
	AmountCents      int64  `json:"amount_cents"`
	Status           string `json:"status"`
	DisplayReference string `json:"display_reference"`
	ReferenceID      string `json:"reference_id"`
	OwnerType        string `json:"owner_type"`
	OwnerName        string `json:"owner_name"`
	OwnerEmail       string `json:"owner_email"`
	CreatedAt        string `json:"created_at"`
}

// ListPayments returns a filtered, paginated feed of every payment. The `search`
// filter (owner name/email) doubles as the per-user view.
func (c *AdminController) ListPayments(w http.ResponseWriter, r *http.Request) {
	page, pageSize, offset := parsePagination(r)
	q := r.URL.Query()
	rows, total, err := c.dirRepo.ListPayments(r.Context(), repository.ListPaymentsParams{
		Limit: pageSize, Offset: offset,
		Search: q.Get("search"), Type: q.Get("type"), Status: q.Get("status"),
		From: q.Get("from"), To: q.Get("to"),
	})
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	items := make([]adminPaymentDTO, 0, len(rows))
	for _, p := range rows {
		items = append(items, adminPaymentDTO{
			ID: p.ID.String(), Type: p.Type, AmountCents: p.AmountCents, Status: p.Status,
			DisplayReference: p.DisplayReference.String, ReferenceID: p.ReferenceID.String,
			OwnerType: p.OwnerType, OwnerName: p.OwnerName.String, OwnerEmail: p.OwnerEmail.String,
			CreatedAt: p.CreatedAt.Format(time.RFC3339),
		})
	}
	RespondSuccess(w, http.StatusOK, paginatedResponse{Items: items, Total: total, Page: page, PageSize: pageSize})
}

type adminLedgerDTO struct {
	ID                string `json:"id"`
	Type              string `json:"type"`
	AmountCents       int64  `json:"amount_cents"`
	BalanceAfterCents int64  `json:"balance_after_cents"`
	ReferenceType     string `json:"reference_type"`
	Description       string `json:"description"`
	Status            string `json:"status"`
	OwnerType         string `json:"owner_type"`
	OwnerName         string `json:"owner_name"`
	CreatedAt         string `json:"created_at"`
}

// ListLedger returns a filtered, paginated feed of transaction_ledger entries.
func (c *AdminController) ListLedger(w http.ResponseWriter, r *http.Request) {
	page, pageSize, offset := parsePagination(r)
	q := r.URL.Query()
	rows, total, err := c.dirRepo.ListLedgerEntries(r.Context(), repository.ListLedgerParams{
		Limit: pageSize, Offset: offset,
		Search: q.Get("search"), Type: q.Get("type"),
		From: q.Get("from"), To: q.Get("to"),
	})
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	items := make([]adminLedgerDTO, 0, len(rows))
	for _, l := range rows {
		items = append(items, adminLedgerDTO{
			ID: l.ID.String(), Type: l.Type, AmountCents: l.AmountCents, BalanceAfterCents: l.BalanceAfterCents,
			ReferenceType: l.ReferenceType.String, Description: l.Description.String, Status: l.Status,
			OwnerType: l.OwnerType, OwnerName: l.OwnerName.String, CreatedAt: l.CreatedAt.Format(time.RFC3339),
		})
	}
	RespondSuccess(w, http.StatusOK, paginatedResponse{Items: items, Total: total, Page: page, PageSize: pageSize})
}

type adminBalanceDTO struct {
	AccountID    string `json:"account_id"`
	OwnerType    string `json:"owner_type"`
	OwnerName    string `json:"owner_name"`
	BalanceCents int64  `json:"balance_cents"`
	LedgerCents  int64  `json:"ledger_cents"`
	DriftCents   int64  `json:"drift_cents"`
}

// ListBalances returns a paginated view of account balances vs ledger sums.
func (c *AdminController) ListBalances(w http.ResponseWriter, r *http.Request) {
	page, pageSize, offset := parsePagination(r)
	q := r.URL.Query()
	rows, total, err := c.dirRepo.ListAccountBalances(r.Context(), repository.ListBalancesParams{
		Limit: pageSize, Offset: offset,
		Search: q.Get("search"), OwnerType: q.Get("owner_type"),
	})
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	items := make([]adminBalanceDTO, 0, len(rows))
	for _, b := range rows {
		items = append(items, adminBalanceDTO{
			AccountID: b.AccountID.String(), OwnerType: b.OwnerType, OwnerName: b.OwnerName.String,
			BalanceCents: b.BalanceCents, LedgerCents: b.LedgerCents, DriftCents: b.DriftCents,
		})
	}
	RespondSuccess(w, http.StatusOK, paginatedResponse{Items: items, Total: total, Page: page, PageSize: pageSize})
}

// GetPaymentsSummary returns reconciliation/health aggregates for the tab header.
func (c *AdminController) GetPaymentsSummary(w http.ResponseWriter, r *http.Request) {
	summary, err := c.dirRepo.GetPaymentsSummary(r.Context())
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, summary)
}
