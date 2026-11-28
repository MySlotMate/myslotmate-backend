package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/service"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Paid-budget Boost and Premium Hosts routes (PRD 2026-11-26), in the shapes
// MySlotmate-Frontend/src/lib/boostPlan.ts calls.

func (c *BoostController) registerPlanRoutes(r chi.Router, user func(http.Handler) http.Handler) {
	r.With(user).Get("/experiences/{eventID}/boost/estimate", c.Estimate)
	r.Route("/boosts/{boostID}", func(r chi.Router) {
		r.Use(user)
		r.Post("/payments", c.CreatePayment)
		r.Post("/payments/{paymentID}/verify", c.VerifyPayment)
		r.Get("/results", c.Results)
		r.Post("/creatives/upload-url", c.CreativeUploadURL)
		r.Post("/creatives", c.SaveCreative)
		r.Get("/creatives", c.Creatives)
		r.Post("/creatives/{creativeID}/approve", c.ApproveCreative)
		r.Post("/creatives/{creativeID}/request-changes", c.RequestCreativeChanges)
		r.Delete("/creatives/{creativeID}", c.DeleteCreative)
		r.Get("/reports", c.Reports)
		r.Get("/documents", c.Documents)
	})
	// The budget rules and upload limits the admin sets, so the website's
	// planner checks what the backend will.
	r.With(user).Get("/boost/settings", c.GetSettings)
	r.With(user).Get("/host/premium", c.HostPremium)
	r.With(user).Get("/host/boost-activity", c.HostActivity)
	r.With(user).Post("/host/premium/apply", c.ApplyPremium)
}

// registerAdminPlanRoutes adds to /admin/boosts (admin auth already applied).
func (c *BoostController) registerAdminPlanRoutes(r chi.Router) {
	r.Post("/{boostID}/payment-link", c.AdminPaymentLink)
	r.Post("/{boostID}/payments/offline", c.AdminOfflinePayment)
	r.Post("/{boostID}/payments/{paymentID}/reverse", c.AdminReversePayment)
	r.Put("/{boostID}/spend", c.AdminSpend)
	r.Get("/{boostID}/results", c.AdminResults)
	r.Get("/{boostID}/creatives", c.AdminCreatives)
	r.Post("/{boostID}/creatives/upload-url", c.AdminCreativeUploadURL)
	r.Post("/{boostID}/creatives", c.AdminSaveCreative)
	r.Post("/{boostID}/creatives/{creativeID}/status", c.AdminCreativeStatus)
	r.Get("/{boostID}/reports", c.AdminReports)
	r.Post("/{boostID}/reports/upload-url", c.AdminReportUploadURL)
	r.Post("/{boostID}/reports", c.AdminAddReport)
	r.Put("/{boostID}/reports/{reportID}", c.AdminReplaceReport)
	r.Post("/{boostID}/close", c.AdminClose)
}

func (c *BoostController) registerAdminPremiumRoutes(r chi.Router) {
	r.Get("/admin/premium", c.PremiumList)
	r.Get("/admin/premium/{hostID}", c.PremiumGet)
	r.Post("/admin/premium/{hostID}/approve", c.premiumAction(c.boosts.PremiumApprove))
	r.Post("/admin/premium/{hostID}/decline", c.premiumAction(c.boosts.PremiumDecline))
	r.Post("/admin/premium/{hostID}/remove", c.premiumAction(c.boosts.PremiumRemove))
	r.Post("/admin/premium/{hostID}/grant", c.premiumAction(c.boosts.PremiumGrant))
	r.Get("/admin/boost-bands", c.Bands)
	r.Post("/admin/boost-bands", c.PublishBands)
	r.Get("/admin/boost-settings", c.GetSettings)
	r.Put("/admin/boost-settings", c.SaveSettings)
}

/* ---- helpers ---- */

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return false
	}
	return true
}

func reply(w http.ResponseWriter, status int, v any, err error) {
	if err != nil {
		respondBoost(w, err)
		return
	}
	RespondSuccess(w, status, v)
}

// hostAndBoost resolves the caller and the {boostID} path value.
func (c *BoostController) hostAndBoost(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	userID, ok := c.caller(w, r)
	if !ok {
		return uuid.Nil, uuid.Nil, false
	}
	id, ok := pathUUID(w, r, "boostID")
	return userID, id, ok
}

/* ---- host ---- */

func (c *BoostController) Estimate(w http.ResponseWriter, r *http.Request) {
	userID, ok := c.caller(w, r)
	if !ok {
		return
	}
	eventID, ok := pathUUID(w, r, "eventID")
	if !ok {
		return
	}
	q := r.URL.Query()
	var daily *int64
	if v := q.Get("daily_budget_cents"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			RespondError(w, http.StatusBadRequest, "daily_budget_cents must be a number")
			return
		}
		daily = &n
	}
	e, err := c.boosts.Estimate(r.Context(), userID, eventID, daily, q.Get("from"), q.Get("until"))
	reply(w, http.StatusOK, e, err)
}

func (c *BoostController) CreatePayment(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := c.hostAndBoost(w, r)
	if !ok {
		return
	}
	var body service.PaymentRequest
	if !decodeBody(w, r, &body) {
		return
	}
	o, err := c.boosts.CreatePayment(r.Context(), userID, id, body)
	reply(w, http.StatusCreated, o, err)
}

func (c *BoostController) VerifyPayment(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := c.hostAndBoost(w, r)
	if !ok {
		return
	}
	pid, ok := pathUUID(w, r, "paymentID")
	if !ok {
		return
	}
	var body service.VerifyRequest
	if !decodeBody(w, r, &body) {
		return
	}
	b, err := c.boosts.VerifyPayment(r.Context(), userID, id, pid, body)
	reply(w, http.StatusOK, b, err)
}

func (c *BoostController) Results(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := c.hostAndBoost(w, r)
	if !ok {
		return
	}
	res, err := c.boosts.Results(r.Context(), userID, id)
	reply(w, http.StatusOK, res, err)
}

func (c *BoostController) CreativeUploadURL(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := c.hostAndBoost(w, r)
	if !ok {
		return
	}
	var body service.UploadRequest
	if !decodeBody(w, r, &body) {
		return
	}
	u, err := c.boosts.CreativeUploadURL(r.Context(), userID, id, body)
	reply(w, http.StatusOK, u, err)
}

func (c *BoostController) SaveCreative(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := c.hostAndBoost(w, r)
	if !ok {
		return
	}
	var body service.CreativeInput
	if !decodeBody(w, r, &body) {
		return
	}
	cr, err := c.boosts.SaveCreative(r.Context(), userID, id, body)
	reply(w, http.StatusCreated, cr, err)
}

func (c *BoostController) Creatives(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := c.hostAndBoost(w, r)
	if !ok {
		return
	}
	list, err := c.boosts.Creatives(r.Context(), userID, id)
	reply(w, http.StatusOK, list, err)
}

func (c *BoostController) ApproveCreative(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := c.hostAndBoost(w, r)
	if !ok {
		return
	}
	cid, ok := pathUUID(w, r, "creativeID")
	if !ok {
		return
	}
	cr, err := c.boosts.ApproveCreative(r.Context(), userID, id, cid)
	reply(w, http.StatusOK, cr, err)
}

func (c *BoostController) RequestCreativeChanges(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := c.hostAndBoost(w, r)
	if !ok {
		return
	}
	cid, ok := pathUUID(w, r, "creativeID")
	if !ok {
		return
	}
	var body struct {
		Note string `json:"note"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	cr, err := c.boosts.RequestCreativeChanges(r.Context(), userID, id, cid, body.Note)
	reply(w, http.StatusOK, cr, err)
}

func (c *BoostController) DeleteCreative(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := c.hostAndBoost(w, r)
	if !ok {
		return
	}
	cid, ok := pathUUID(w, r, "creativeID")
	if !ok {
		return
	}
	err := c.boosts.DeleteCreative(r.Context(), userID, id, cid)
	reply(w, http.StatusOK, map[string]any{"deleted": cid}, err)
}

func (c *BoostController) Reports(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := c.hostAndBoost(w, r)
	if !ok {
		return
	}
	list, err := c.boosts.Reports(r.Context(), userID, id)
	reply(w, http.StatusOK, list, err)
}

func (c *BoostController) Documents(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := c.hostAndBoost(w, r)
	if !ok {
		return
	}
	docs, err := c.boosts.Documents(r.Context(), userID, id)
	reply(w, http.StatusOK, docs, err)
}

func (c *BoostController) HostPremium(w http.ResponseWriter, r *http.Request) {
	userID, ok := c.caller(w, r)
	if !ok {
		return
	}
	m, err := c.boosts.HostPremium(r.Context(), userID)
	reply(w, http.StatusOK, m, err)
}

// HostActivity is the host's Boost and Premium notifications.
func (c *BoostController) HostActivity(w http.ResponseWriter, r *http.Request) {
	userID, ok := c.caller(w, r)
	if !ok {
		return
	}
	list, err := c.boosts.HostActivity(r.Context(), userID)
	reply(w, http.StatusOK, list, err)
}

func (c *BoostController) ApplyPremium(w http.ResponseWriter, r *http.Request) {
	userID, ok := c.caller(w, r)
	if !ok {
		return
	}
	var body struct {
		Note string `json:"note"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	m, err := c.boosts.ApplyPremium(r.Context(), userID, body.Note)
	reply(w, http.StatusOK, m, err)
}

/* ---- admin ---- */

func (c *BoostController) AdminPaymentLink(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "boostID")
	if !ok {
		return
	}
	var body service.PaymentRequest
	if !decodeBody(w, r, &body) {
		return
	}
	res, err := c.boosts.AdminPaymentLink(r.Context(), adminName(r), id, body)
	reply(w, http.StatusCreated, res, err)
}

func (c *BoostController) AdminOfflinePayment(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "boostID")
	if !ok {
		return
	}
	var body service.OfflinePayment
	if !decodeBody(w, r, &body) {
		return
	}
	b, err := c.boosts.AdminOfflinePayment(r.Context(), adminName(r), id, body)
	reply(w, http.StatusOK, b, err)
}

func (c *BoostController) AdminReversePayment(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "boostID")
	if !ok {
		return
	}
	pid, ok := pathUUID(w, r, "paymentID")
	if !ok {
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	b, err := c.boosts.AdminReversePayment(r.Context(), adminName(r), id, pid, body.Reason)
	reply(w, http.StatusOK, b, err)
}

func (c *BoostController) AdminSpend(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "boostID")
	if !ok {
		return
	}
	var body struct {
		Entries []service.SpendInput `json:"entries"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	b, err := c.boosts.AdminSpend(r.Context(), adminName(r), id, body.Entries)
	reply(w, http.StatusOK, b, err)
}

func (c *BoostController) AdminResults(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "boostID")
	if !ok {
		return
	}
	res, err := c.boosts.AdminResults(r.Context(), id)
	reply(w, http.StatusOK, res, err)
}

func (c *BoostController) AdminCreatives(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "boostID")
	if !ok {
		return
	}
	list, err := c.boosts.AdminCreatives(r.Context(), id)
	reply(w, http.StatusOK, list, err)
}

func (c *BoostController) AdminCreativeUploadURL(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "boostID")
	if !ok {
		return
	}
	var body service.UploadRequest
	if !decodeBody(w, r, &body) {
		return
	}
	u, err := c.boosts.AdminCreativeUploadURL(r.Context(), id, body)
	reply(w, http.StatusOK, u, err)
}

func (c *BoostController) AdminSaveCreative(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "boostID")
	if !ok {
		return
	}
	var body service.CreativeInput
	if !decodeBody(w, r, &body) {
		return
	}
	cr, err := c.boosts.AdminSaveCreative(r.Context(), adminName(r), id, body)
	reply(w, http.StatusCreated, cr, err)
}

func (c *BoostController) AdminCreativeStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "boostID")
	if !ok {
		return
	}
	cid, ok := pathUUID(w, r, "creativeID")
	if !ok {
		return
	}
	var body struct {
		Status string `json:"status"`
		Note   string `json:"note"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	cr, err := c.boosts.AdminCreativeStatus(r.Context(), adminName(r), id, cid, body.Status, body.Note)
	reply(w, http.StatusOK, cr, err)
}

func (c *BoostController) AdminReports(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "boostID")
	if !ok {
		return
	}
	list, err := c.boosts.AdminReports(r.Context(), id)
	reply(w, http.StatusOK, list, err)
}

func (c *BoostController) AdminReportUploadURL(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "boostID")
	if !ok {
		return
	}
	var body service.UploadRequest
	if !decodeBody(w, r, &body) {
		return
	}
	u, err := c.boosts.AdminReportUploadURL(r.Context(), id, body)
	reply(w, http.StatusOK, u, err)
}

func (c *BoostController) AdminAddReport(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "boostID")
	if !ok {
		return
	}
	var body service.ReportInput
	if !decodeBody(w, r, &body) {
		return
	}
	x, err := c.boosts.AdminAddReport(r.Context(), adminName(r), id, body)
	reply(w, http.StatusCreated, x, err)
}

func (c *BoostController) AdminReplaceReport(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "boostID")
	if !ok {
		return
	}
	rid, ok := pathUUID(w, r, "reportID")
	if !ok {
		return
	}
	var body service.ReportInput
	if !decodeBody(w, r, &body) {
		return
	}
	x, err := c.boosts.AdminReplaceReport(r.Context(), adminName(r), id, rid, body)
	reply(w, http.StatusOK, x, err)
}

func (c *BoostController) AdminClose(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "boostID")
	if !ok {
		return
	}
	b, err := c.boosts.AdminClose(r.Context(), adminName(r), id)
	reply(w, http.StatusOK, b, err)
}

func (c *BoostController) PremiumList(w http.ResponseWriter, r *http.Request) {
	list, counts, err := c.boosts.PremiumList(r.Context(), r.URL.Query().Get("status"))
	reply(w, http.StatusOK, map[string]any{"items": list, "counts": counts}, err)
}

func (c *BoostController) PremiumGet(w http.ResponseWriter, r *http.Request) {
	hostID, ok := pathUUID(w, r, "hostID")
	if !ok {
		return
	}
	d, err := c.boosts.PremiumGet(r.Context(), hostID)
	reply(w, http.StatusOK, d, err)
}

type premiumFn func(ctx context.Context, admin string, hostID uuid.UUID, d service.PremiumDecision) (*models.PremiumMembership, error)

func (c *BoostController) premiumAction(fn premiumFn) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hostID, ok := pathUUID(w, r, "hostID")
		if !ok {
			return
		}
		var body service.PremiumDecision
		_ = json.NewDecoder(r.Body).Decode(&body)
		m, err := fn(r.Context(), adminName(r), hostID, body)
		reply(w, http.StatusOK, m, err)
	}
}

func (c *BoostController) Bands(w http.ResponseWriter, r *http.Request) {
	b, err := c.boosts.AllBands(r.Context())
	reply(w, http.StatusOK, b, err)
}

func (c *BoostController) PublishBands(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Bands []models.BoostBand `json:"bands"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	v, err := c.boosts.PublishBands(r.Context(), adminName(r), body.Bands)
	reply(w, http.StatusCreated, map[string]any{"version": v}, err)
}

func (c *BoostController) GetSettings(w http.ResponseWriter, r *http.Request) {
	s, err := c.boosts.Settings(r.Context())
	reply(w, http.StatusOK, s, err)
}

func (c *BoostController) SaveSettings(w http.ResponseWriter, r *http.Request) {
	var body models.BoostSettings
	if !decodeBody(w, r, &body) {
		return
	}
	s, err := c.boosts.SaveSettings(r.Context(), body)
	reply(w, http.StatusOK, s, err)
}
