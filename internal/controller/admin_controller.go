package controller

import (
	"encoding/json"
	"net/http"
	"strings"

	"myslotmate-backend/internal/auth"
	"myslotmate-backend/internal/models"
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
	firebaseAuth  *fbauth.Client
	adminEmail    string
	jwtSecret     string
}

func NewAdminController(hs service.HostService, ps service.PayoutService, us service.UserService, fa *fbauth.Client, adminEmail, jwtSecret string) *AdminController {
	return &AdminController{
		hostService:   hs,
		payoutService: ps,
		userService:   us,
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
		r.Post("/{hostID}/approve", c.ApproveApplication)
		r.Post("/{hostID}/reject", c.RejectApplication)
		r.Put("/{hostID}/application-status", c.UpdateApplicationStatus)
	})

	r.Route("/admin/platform", func(r chi.Router) {
		// Accepts the static admin-dashboard token OR a Firebase admin token.
		r.Use(auth.RequireAdmin(c.firebaseAuth, c.adminEmail, c.jwtSecret))

		r.Get("/balance", c.GetPlatformBalance)
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

// ── Admin Platform Payout Handlers ──────────────────────────────────────────

func (c *AdminController) GetPlatformBalance(w http.ResponseWriter, r *http.Request) {
	balance, err := c.payoutService.GetPlatformBalance(r.Context())
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, balance)
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
