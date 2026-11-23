package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	htmlpkg "html"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"myslotmate-backend/internal/auth"
	"myslotmate-backend/internal/lib/notification"
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
	sendEmail    func(to, subject, htmlBody string, files ...notification.Attachment) error
	hostRepo     repository.HostRepository
	userRepo     repository.UserRepository
	firebaseAuth *fbauth.Client
	jwtSecret    string
	adminEmail   string
}

func NewBillingController(b *service.BillingService, ur repository.UserRepository, fa *fbauth.Client, jwtSecret, adminEmail string) *BillingController {
	return &BillingController{billing: b, userRepo: ur, firebaseAuth: fa, jwtSecret: jwtSecret, adminEmail: adminEmail}
}

// WithHosts lets a host read their own payout statement.
func (c *BillingController) WithHosts(hr repository.HostRepository) *BillingController {
	c.hostRepo = hr
	return c
}

// WithEmail lets admins resend a receipt by email.
func (c *BillingController) WithEmail(send func(to, subject, htmlBody string, files ...notification.Attachment) error) *BillingController {
	c.sendEmail = send
	return c
}

func (c *BillingController) RegisterRoutes(r chi.Router) {
	r.Route("/billing", func(r chi.Router) {
		r.Use(auth.RequireUserOrAdmin(c.firebaseAuth, c.adminEmail, c.jwtSecret))
		// ?source_type=booking|pass&source_id=<uuid>: the sale's invoice and credit note.
		r.Get("/documents", c.ListMine)
		r.Get("/documents/mine", c.ListAllMine)
		r.Get("/documents/{invoiceID}/pdf", c.MyPDF)
		// ?month=2026-11[&format=pdf]. A host gets their own; an admin passes host_id.
		r.Get("/host-statement", c.HostStatement)
	})
	r.Route("/admin/billing", func(r chi.Router) {
		r.Use(auth.RequireAdmin(c.firebaseAuth, c.adminEmail, c.jwtSecret))
		r.Get("/settings", c.GetSettings)
		r.Put("/settings", c.SaveSettings)
		r.Get("/invoices", c.List)
		r.Get("/invoices/{invoiceID}", c.Get)
		r.Get("/invoices/{invoiceID}/pdf", c.AdminPDF)
		r.Post("/invoices/{invoiceID}/resend", c.Resend)
		// ?month=2026-11 (IST); &format=csv&table=b2cs|hsn|docs|register for a file.
		r.Get("/reports/gst", c.GSTReport)
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

// Resend emails the document's PDF to the buyer, or to "email" in the body.
func (c *BillingController) Resend(w http.ResponseWriter, r *http.Request) {
	inv := c.load(w, r)
	if inv == nil {
		return
	}
	var body struct {
		Email string `json:"email"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	to := strings.TrimSpace(body.Email)
	if to == "" {
		to = inv.BuyerEmail
	}
	if _, err := mail.ParseAddress(to); err != nil || strings.ContainsAny(to, "\r\n") {
		RespondError(w, http.StatusBadRequest, "That email address doesn't look right.")
		return
	}
	if c.sendEmail == nil {
		RespondError(w, http.StatusServiceUnavailable, "Email isn't set up on this server.")
		return
	}
	pdf, err := c.billing.PDF(r.Context(), inv)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, "Couldn't build the PDF: "+err.Error())
		return
	}
	what := "fee receipt"
	if inv.Kind == models.InvoiceKindCreditNote {
		what = "refund note"
	}
	name := inv.BuyerName
	if name == "" {
		name = "there"
	}
	html := fmt.Sprintf(`<html><body style="font-family: Arial, sans-serif; line-height: 1.6; color: #333;">
<p>Hi %s,</p>
<p>Here is your MySlotMate platform %s <strong>%s</strong>%s, attached as a PDF.</p>
<p>Thank you for using MySlotMate!</p></body></html>`,
		htmlpkg.EscapeString(name), what, htmlpkg.EscapeString(inv.Number), forEvent(inv.EventTitle))
	err = c.sendEmail(to, fmt.Sprintf("Your MySlotMate %s %s", what, inv.Number), html,
		notification.Attachment{Filename: service.InvoiceFilename(inv), ContentType: "application/pdf", Data: pdf})
	if err != nil {
		if strings.Contains(err.Error(), "SMTP configuration incomplete") {
			RespondError(w, http.StatusServiceUnavailable, "Email isn't set up on this server.")
			return
		}
		RespondError(w, http.StatusBadGateway, "Couldn't send the email: "+err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, map[string]string{"sent_to": to})
}

func forEvent(title string) string {
	if title == "" {
		return ""
	}
	return " for " + htmlpkg.EscapeString(title)
}

func (c *BillingController) GSTReport(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	month := q.Get("month")
	if q.Get("format") == "csv" {
		table := q.Get("table")
		b, err := c.billing.GSTReportCSV(r.Context(), month, table)
		if err != nil {
			var bad *service.ErrBillingSettings
			if errors.As(err, &bad) {
				RespondError(w, http.StatusBadRequest, bad.Message)
				return
			}
			RespondError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="gst-%s-%s.csv"`, table, month))
		w.Header().Set("Cache-Control", "private, no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b)
		return
	}
	rep, err := c.billing.GSTReport(r.Context(), month)
	if err != nil {
		var bad *service.ErrBillingSettings
		if errors.As(err, &bad) {
			RespondError(w, http.StatusBadRequest, bad.Message)
			return
		}
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, rep)
}

// HostStatement serves a host's monthly payout statement (JSON, or the PDF
// with format=pdf). Hosts read their own only; admins name the host.
func (c *BillingController) HostStatement(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var hostID uuid.UUID
	if auth.IsAdminCaller(r) {
		id, err := uuid.Parse(q.Get("host_id"))
		if err != nil {
			RespondError(w, http.StatusBadRequest, "host_id is required")
			return
		}
		hostID = id
	} else {
		caller, err := c.callerUserID(r)
		if err != nil {
			RespondError(w, http.StatusUnauthorized, err.Error())
			return
		}
		if c.hostRepo == nil {
			RespondError(w, http.StatusForbidden, "Not a host")
			return
		}
		h, err := c.hostRepo.GetByUserID(r.Context(), caller)
		if err != nil || h == nil {
			RespondError(w, http.StatusForbidden, "Not a host")
			return
		}
		hostID = h.ID // host_id in the query is ignored for hosts
	}
	st, err := c.billing.HostStatement(r.Context(), hostID, q.Get("month"))
	if err != nil {
		var bad *service.ErrBillingSettings
		if errors.As(err, &bad) {
			status := http.StatusBadRequest
			if err == service.ErrHostNotFound {
				status = http.StatusNotFound
			}
			RespondError(w, status, bad.Message)
			return
		}
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if q.Get("format") != "pdf" {
		RespondSuccess(w, http.StatusOK, st)
		return
	}
	b, err := service.RenderHostStatementPDF(st)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, "Couldn't build the PDF: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, service.StatementFilename(st)))
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}
