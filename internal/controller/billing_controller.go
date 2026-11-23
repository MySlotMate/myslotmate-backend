package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"myslotmate-backend/internal/auth"
	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"
	"myslotmate-backend/internal/service"

	fbauth "firebase.google.com/go/v4/auth"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// BillingController serves fee invoices and credit notes: to the guest who
// paid, and to admins (with the billing settings).
type BillingController struct {
	billing      *service.BillingService
	userRepo     repository.UserRepository
	firebaseAuth *fbauth.Client
	jwtSecret    string
	adminEmail   string
}

func NewBillingController(b *service.BillingService, ur repository.UserRepository, fa *fbauth.Client, jwtSecret, adminEmail string) *BillingController {
	return &BillingController{billing: b, userRepo: ur, firebaseAuth: fa, jwtSecret: jwtSecret, adminEmail: adminEmail}
}

func (c *BillingController) RegisterRoutes(r chi.Router) {
	r.Route("/billing", func(r chi.Router) {
		r.Use(auth.RequireUserOrAdmin(c.firebaseAuth, c.adminEmail, c.jwtSecret))
		// ?source_type=booking|pass&source_id=<uuid>: the sale's invoice and credit note.
		r.Get("/documents", c.ListMine)
		r.Get("/documents/mine", c.ListAllMine)
		r.Get("/documents/{invoiceID}/pdf", c.MyPDF)
	})
	r.Route("/admin/billing", func(r chi.Router) {
		r.Use(auth.RequireAdmin(c.firebaseAuth, c.adminEmail, c.jwtSecret))
		r.Get("/settings", c.GetSettings)
		r.Put("/settings", c.SaveSettings)
		r.Get("/invoices", c.List)
		r.Get("/invoices/{invoiceID}", c.Get)
		r.Get("/invoices/{invoiceID}/pdf", c.AdminPDF)
	})
}

// callerUserID resolves the signed-in guest; uuid.Nil for an admin caller.
func (c *BillingController) callerUserID(r *http.Request) (uuid.UUID, error) {
	if auth.IsAdminCaller(r) {
		return uuid.Nil, nil
	}
	uid, _ := r.Context().Value(auth.ContextKeyUID).(string)
	if uid == "" || c.userRepo == nil {
		return uuid.Nil, errors.New("unauthorized")
	}
	u, err := c.userRepo.GetByAuthUID(r.Context(), uid)
	if err != nil || u == nil {
		return uuid.Nil, errors.New("unauthorized")
	}
	return u.ID, nil
}

func (c *BillingController) ListMine(w http.ResponseWriter, r *http.Request) {
	caller, err := c.callerUserID(r)
	if err != nil {
		RespondError(w, http.StatusUnauthorized, err.Error())
		return
	}
	sourceType := r.URL.Query().Get("source_type")
	if sourceType != models.InvoiceSourceBooking && sourceType != models.InvoiceSourcePass {
		RespondError(w, http.StatusBadRequest, "source_type must be booking or pass")
		return
	}
	sourceID, err := uuid.Parse(r.URL.Query().Get("source_id"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid source_id")
		return
	}
	docs, err := c.billing.ForSource(r.Context(), sourceType, sourceID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]*models.Invoice, 0, len(docs))
	for _, d := range docs {
		if caller == uuid.Nil || d.UserID == caller { // someone else's sale reads as "no invoice"
			out = append(out, d)
		}
	}
	RespondSuccess(w, http.StatusOK, out)
}

// ListAllMine returns every invoice and credit note of the signed-in guest
// (My Journey's fee receipts). An admin caller gets an empty list.
func (c *BillingController) ListAllMine(w http.ResponseWriter, r *http.Request) {
	caller, err := c.callerUserID(r)
	if err != nil {
		RespondError(w, http.StatusUnauthorized, err.Error())
		return
	}
	if caller == uuid.Nil {
		RespondSuccess(w, http.StatusOK, []*models.Invoice{})
		return
	}
	docs, err := c.billing.ForUser(r.Context(), caller)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if docs == nil {
		docs = []*models.Invoice{}
	}
	RespondSuccess(w, http.StatusOK, docs)
}

func (c *BillingController) MyPDF(w http.ResponseWriter, r *http.Request) {
	caller, err := c.callerUserID(r)
	if err != nil {
		RespondError(w, http.StatusUnauthorized, err.Error())
		return
	}
	inv := c.load(w, r)
	if inv == nil {
		return
	}
	if caller != uuid.Nil && inv.UserID != caller {
		RespondError(w, http.StatusNotFound, "Invoice not found")
		return
	}
	c.writePDF(w, r, inv)
}

func (c *BillingController) AdminPDF(w http.ResponseWriter, r *http.Request) {
	if inv := c.load(w, r); inv != nil {
		c.writePDF(w, r, inv)
	}
}

func (c *BillingController) Get(w http.ResponseWriter, r *http.Request) {
	if inv := c.load(w, r); inv != nil {
		RespondSuccess(w, http.StatusOK, inv)
	}
}

func (c *BillingController) load(w http.ResponseWriter, r *http.Request) *models.Invoice {
	id, err := uuid.Parse(chi.URLParam(r, "invoiceID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid invoice ID")
		return nil
	}
	inv, err := c.billing.Get(r.Context(), id)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return nil
	}
	if inv == nil {
		RespondError(w, http.StatusNotFound, "Invoice not found")
		return nil
	}
	return inv
}

func (c *BillingController) writePDF(w http.ResponseWriter, r *http.Request, inv *models.Invoice) {
	b, err := c.billing.PDF(r.Context(), inv)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, "Couldn't build the PDF: "+err.Error())
		return
	}
	name := service.InvoiceFilename(inv)
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func (c *BillingController) GetSettings(w http.ResponseWriter, r *http.Request) {
	s, err := c.billing.GetSettings(r.Context())
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, settingsView(s))
}

func (c *BillingController) SaveSettings(w http.ResponseWriter, r *http.Request) {
	var in models.BillingSettings
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}
	s, err := c.billing.SaveSettings(r.Context(), in)
	if err != nil {
		var bad *service.ErrBillingSettings
		if errors.As(err, &bad) {
			RespondError(w, http.StatusBadRequest, bad.Message)
			return
		}
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, settingsView(s))
}

// settingsView adds what the admin form needs to explain itself.
func settingsView(s *models.BillingSettings) map[string]any {
	missing := s.Missing()
	if missing == nil {
		missing = []string{}
	}
	return map[string]any{"settings": s, "issuing": s.Issuing(), "missing": missing}
}

func (c *BillingController) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := models.InvoiceFilter{Query: q.Get("q"), Kind: q.Get("kind")}
	if f.Kind != "" && f.Kind != models.InvoiceKindInvoice && f.Kind != models.InvoiceKindCreditNote {
		RespondError(w, http.StatusBadRequest, "kind must be invoice or credit_note")
		return
	}
	// from/to are dates (YYYY-MM-DD) in IST; to is inclusive.
	ist := time.FixedZone("IST", 5*3600+1800)
	if v := q.Get("from"); v != "" {
		t, err := time.ParseInLocation("2006-01-02", v, ist)
		if err != nil {
			RespondError(w, http.StatusBadRequest, "from must be YYYY-MM-DD")
			return
		}
		f.From = &t
	}
	if v := q.Get("to"); v != "" {
		t, err := time.ParseInLocation("2006-01-02", v, ist)
		if err != nil {
			RespondError(w, http.StatusBadRequest, "to must be YYYY-MM-DD")
			return
		}
		t = t.AddDate(0, 0, 1)
		f.To = &t
	}
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	f.Offset, _ = strconv.Atoi(q.Get("offset"))
	items, total, err := c.billing.List(r.Context(), f)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if items == nil {
		items = []*models.Invoice{}
	}
	RespondSuccess(w, http.StatusOK, map[string]any{"items": items, "total": total})
}
