package service

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"myslotmate-backend/internal/lib/payment"
	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"

	"github.com/google/uuid"
)

// Premium Hosts and paid Boost marketing (PRD 2026-11-26). A budget Boost
// keeps the request → proposal → accept → go-live steps and adds paying the
// ad budget before go-live, a daily spend log the team keeps, creatives and
// reports spaces, and closing with a final report. The budget is never
// refunded; the marketing fee works as before (boostCommissionFor).

// BoostPayments is the Razorpay part the budget needs.
type BoostPayments interface {
	CreateOrder(ctx context.Context, req payment.OrderRequest) (*payment.OrderResponse, error)
	VerifyPaymentSignature(req payment.VerifyRequest) bool
	GetKeyID() string
	CreatePaymentLink(ctx context.Context, req payment.PaymentLinkRequest) (*payment.PaymentLinkResponse, error)
}

// BoostWhatsApp sends a budget payment link on WhatsApp. Optional: the
// notifier may implement it (a no-op when no template is configured).
type BoostWhatsApp interface {
	BoostPaymentWhatsApp(ctx context.Context, phone string, params map[string]string) error
}

// BoostFiles is the storage the creatives and reports use (storage.UploadService).
type BoostFiles interface {
	PresignBoostUpload(ctx context.Context, key, contentType string, size int64) (string, error)
	SignBoostDownload(key, name string) string
	SignBoostAttachment(key, name string) string
	OpenBoostObject(ctx context.Context, key string) (io.ReadCloser, error)
	HeadBoostObject(ctx context.Context, key string) (int64, string, error)
}

// WithPlan wires what the paid-budget Boost needs. Any of them may be nil:
// the routes that need one then answer 503.
func (s *BoostService) WithPlan(p BoostPayments, f BoostFiles, b *BillingService) *BoostService {
	s.payments, s.files, s.billing = p, f, b
	return s
}

const (
	mb              = int64(1024 * 1024)
	boostNoteMax    = 1000
	boostTitleMax   = 200
	spendEntriesMax = 200
	budgetWarnBps   = 8000 // email the host once 80% of the budget is spent
)

/* ------------------------------------------------------------------ */
/*  Small helpers                                                      */
/* ------------------------------------------------------------------ */

func istDay(t time.Time) string { return t.In(istZone).Format("2006-01-02") }

func parseDay(s string) (time.Time, bool) {
	t, err := time.ParseInLocation("2006-01-02", s, istZone)
	return t, err == nil
}

// daysIncl counts the days from..until, both included (0 if until is before from).
func daysIncl(from, until string) int {
	f, ok1 := parseDay(from)
	u, ok2 := parseDay(until)
	if !ok1 || !ok2 || u.Before(f) {
		return 0
	}
	return int(u.Sub(f).Hours()/24+0.5) + 1
}

func addDay(day string, n int) string {
	t, ok := parseDay(day)
	if !ok {
		return day
	}
	return t.AddDate(0, 0, n).Format("2006-01-02")
}

// budgetGST is the GST added on top of a budget, half-up to the paisa.
func budgetGST(cents int64, bps int) int64 { return (cents*int64(bps) + 5000) / 10000 }

func rupeesText(cents int64) string {
	if cents%100 == 0 {
		return fmt.Sprintf("₹%d", cents/100)
	}
	return fmt.Sprintf("₹%d.%02d", cents/100, cents%100)
}

// budgetEnd is when a budget Boost's last day ends: midnight IST after budget_until.
func budgetEnd(b *models.Boost) *time.Time {
	if b.BudgetUntil == nil {
		return nil
	}
	t, ok := parseDay(*b.BudgetUntil)
	if !ok {
		return nil
	}
	t = t.AddDate(0, 0, 1)
	return &t
}

func budgetLine(b *models.Boost) string {
	if b.PricingModel != models.BoostPricingBudget || b.DailyBudgetCents == nil || b.BudgetFrom == nil || b.BudgetUntil == nil {
		return ""
	}
	return fmt.Sprintf(" Ad budget: %s a day from %s to %s, %s plus %s GST, paid upfront and not refundable.",
		rupeesText(*b.DailyBudgetCents), *b.BudgetFrom, *b.BudgetUntil, rupeesText(b.BudgetTotalCents), rupeesText(b.BudgetGSTCents))
}

func liveBudgetLine(b *models.Boost) string {
	if b.PricingModel != models.BoostPricingBudget || b.DailyBudgetCents == nil || b.BudgetUntil == nil {
		return ""
	}
	return fmt.Sprintf(" Daily budget: %s, running until %s.", rupeesText(*b.DailyBudgetCents), *b.BudgetUntil)
}

func safeName(name string) string {
	name = path.Base(strings.TrimSpace(name))
	name = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, name)
	if name == "" || name == "." || name == "_" {
		name = "file"
	}
	if len(name) > 100 {
		name = name[len(name)-100:]
	}
	return name
}

func trimmedPtr(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return &s
}

func tooLong(s string, n int) bool { return len([]rune(s)) > n }

var errPlanOff = boostErr(http.StatusServiceUnavailable, "not_configured", "This isn't set up on the server yet.")

/* ------------------------------------------------------------------ */
/*  Premium gate and acceptance                                        */
/* ------------------------------------------------------------------ */

func (s *BoostService) requirePremium(ctx context.Context, r *repository.BoostRepository, hostID uuid.UUID) error {
	ok, err := r.PremiumActive(ctx, hostID)
	if err != nil {
		return err
	}
	if !ok {
		return boostErr(http.StatusForbidden, "premium_required", "Boost is for Premium hosts. Join Premium from your host dashboard.")
	}
	return nil
}

// acceptTerms is the terms version and, for an unpaid budget Boost, when the
// budget is due.
func (s *BoostService) acceptTerms(ctx context.Context, r *repository.BoostRepository, b *models.Boost) (string, *time.Time, error) {
	if b.PricingModel != models.BoostPricingBudget {
		return models.BoostTermsVersion, nil, nil
	}
	if b.BudgetStatus != models.BudgetUnpaid {
		return models.BoostBudgetTermsVersion, nil, nil
	}
	st, err := r.Settings(ctx)
	if err != nil {
		return "", nil, err
	}
	due := time.Now().Add(time.Duration(st.PaymentWindowDays) * 24 * time.Hour)
	return models.BoostBudgetTermsVersion, &due, nil
}

/* ------------------------------------------------------------------ */
/*  Estimate                                                           */
/* ------------------------------------------------------------------ */

// BudgetEstimate is the cost of a budget plan and the guide fee per ticket type.
type BudgetEstimate struct {
	Days              int                `json:"days"`
	DailyBudgetCents  int64              `json:"daily_budget_cents"`
	BudgetTotalCents  int64              `json:"budget_total_cents"`
	BudgetGSTCents    int64              `json:"budget_gst_cents"`
	TotalPayableCents int64              `json:"total_payable_cents"`
	LastDay           *string            `json:"last_day"` // latest day the plan may run to
	Fees              []models.BoostRate `json:"fees"`
	Problems          []string           `json:"problems"`
}

// planProblems checks a budget plan against the rules. lastDay "" = no limit.
func planProblems(st *models.BoostSettings, daily int64, from, until, today, lastDay string) []string {
	p := []string{}
	if daily < st.MinDailyBudgetCents {
		p = append(p, fmt.Sprintf("The daily budget is at least %s.", rupeesText(st.MinDailyBudgetCents)))
	} else if daily%st.BudgetStepCents != 0 {
		p = append(p, fmt.Sprintf("Use steps of %s for the daily budget.", rupeesText(st.BudgetStepCents)))
	}
	_, okF := parseDay(from)
	_, okU := parseDay(until)
	if !okF || !okU {
		return append(p, "Pick the start and end dates.")
	}
	if from < today {
		p = append(p, "The start date can't be in the past.")
	}
	if daysIncl(from, until) < st.MinDays {
		p = append(p, fmt.Sprintf("Run it for at least %d days.", st.MinDays))
	}
	if lastDay != "" && until > lastDay {
		p = append(p, fmt.Sprintf("The end date can't be after the event's last date (%s).", lastDay))
	}
	return p
}

// lastBoostDay is the event's last upcoming date (IST), or "" with none.
func (s *BoostService) lastBoostDay(ctx context.Context, evt *models.Event, now time.Time) (string, error) {
	avail, err := s.events.GetEventAvailability(ctx, evt.ID)
	if err != nil {
		return "", err
	}
	var last time.Time
	for _, a := range avail {
		if a.Date.After(now) && !a.IsPaused && a.Date.After(last) {
			last = a.Date
		}
	}
	if last.IsZero() {
		return "", nil
	}
	return istDay(last), nil
}

func (s *BoostService) estimate(ctx context.Context, st *models.BoostSettings, evt *models.Event, daily *int64, from, until string, now time.Time) (*BudgetEstimate, error) {
	last, err := s.lastBoostDay(ctx, evt, now)
	if err != nil {
		return nil, err
	}
	var d int64
	if daily != nil {
		d = *daily
	}
	e := &BudgetEstimate{DailyBudgetCents: d, Problems: planProblems(st, d, from, until, istDay(now), last)}
	if last != "" {
		e.LastDay = &last
	} else {
		e.Problems = append(e.Problems, "This event has no upcoming dates.")
	}
	e.Days = daysIncl(from, until)
	e.BudgetTotalCents = d * int64(e.Days)
	e.BudgetGSTCents = budgetGST(e.BudgetTotalCents, st.BudgetGSTBps)
	e.TotalPayableCents = e.BudgetTotalCents + e.BudgetGSTCents
	return e, nil
}

// Estimate is the host's live preview of a budget plan.
func (s *BoostService) Estimate(ctx context.Context, userID, eventID uuid.UUID, daily *int64, from, until string) (*BudgetEstimate, error) {
	h, err := s.hostOf(ctx, userID)
	if err != nil {
		return nil, err
	}
	evt, err := s.eventFor(ctx, h, eventID, false)
	if err != nil {
		return nil, err
	}
	if err := s.requirePremium(ctx, s.repo, h.ID); err != nil {
		return nil, err
	}
	st, err := s.repo.Settings(ctx)
	if err != nil {
		return nil, err
	}
	e, err := s.estimate(ctx, st, evt, daily, from, until, time.Now())
	if err != nil {
		return nil, err
	}
	types, err := s.ticketTypes(ctx, evt)
	if err != nil {
		return nil, err
	}
	bands, err := s.repo.Bands(ctx, models.BoostPricingBudget)
	if err != nil {
		return nil, err
	}
	if e.Fees, err = guideRates(types, bands); err != nil {
		return nil, err
	}
	return e, nil
}

/* ------------------------------------------------------------------ */
/*  Proposal and go-live                                               */
/* ------------------------------------------------------------------ */

// proposeBudget applies the final budget plan and channels of a proposal.
// A paid budget carries over and can't change.
func (s *BoostService) proposeBudget(ctx context.Context, r *repository.BoostRepository, b *models.Boost, evt *models.Event, p Proposal) error {
	if len(p.Channels) > 0 {
		var chans []string
		seen := map[string]bool{}
		for _, c := range p.Channels {
			c = strings.ToLower(strings.TrimSpace(c))
			if !models.BoostChannels[c] {
				return boostErr(http.StatusBadRequest, "invalid_channels", "Unknown channel "+c+".")
			}
			if !seen[c] {
				seen[c] = true
				chans = append(chans, c)
			}
		}
		if err := r.SetChannels(ctx, b.ID, chans); err != nil {
			return err
		}
	}
	if p.DailyBudgetCents == nil && p.BudgetFrom == "" && p.BudgetUntil == "" {
		return nil
	}
	if b.PricingModel != models.BoostPricingBudget {
		// Launch: an open no_upfront request re-proposed with a budget plan
		// moves to the new model (version 2 rates, budget paid upfront).
		if b.AcceptedAt != nil {
			return boostErr(http.StatusConflict, "already_accepted", "An accepted Boost keeps its terms until it ends.")
		}
		if err := s.requirePremium(ctx, r, b.HostID); err != nil {
			return err
		}
		if err := r.SetPricingModel(ctx, b.ID, models.BoostPricingBudget, models.BudgetUnpaid); err != nil {
			return err
		}
		b.PricingModel, b.BudgetStatus = models.BoostPricingBudget, models.BudgetUnpaid
		if err := r.Log(ctx, b.ID, "system", "", "moved_to_budget_model", nil); err != nil {
			return err
		}
	}
	daily, from, until := p.DailyBudgetCents, p.BudgetFrom, p.BudgetUntil
	if daily == nil {
		daily = b.DailyBudgetCents
	}
	if from == "" && b.BudgetFrom != nil {
		from = *b.BudgetFrom
	}
	if until == "" && b.BudgetUntil != nil {
		until = *b.BudgetUntil
	}
	if b.BudgetStatus == models.BudgetPaid {
		same := daily != nil && b.DailyBudgetCents != nil && *daily == *b.DailyBudgetCents &&
			b.BudgetFrom != nil && from == *b.BudgetFrom && b.BudgetUntil != nil && until == *b.BudgetUntil
		if !same {
			return boostErr(http.StatusConflict, "budget_locked", "The budget is already paid, so the plan can't change. Add days with a top-up once it's live.")
		}
		return nil
	}
	st, err := r.Settings(ctx)
	if err != nil {
		return err
	}
	e, err := s.estimate(ctx, st, evt, daily, from, until, time.Now())
	if err != nil {
		return err
	}
	if len(e.Problems) > 0 {
		return boostErr(http.StatusBadRequest, "invalid_budget", strings.Join(e.Problems, " "))
	}
	return r.SetBudgetPlan(ctx, b.ID, *daily, from, until, e.BudgetTotalCents, e.BudgetGSTCents)
}

// goLiveBudgetCheck: a budget Boost goes live only paid and with a creative in use.
func goLiveBudgetCheck(ctx context.Context, r *repository.BoostRepository, b *models.Boost) error {
	if b.BudgetStatus != models.BudgetPaid {
		return boostErr(http.StatusConflict, "budget_unpaid", "The ad budget isn't paid yet.")
	}
	ok, err := r.HasCreativeInUse(ctx, b.ID)
	if err != nil {
		return err
	}
	if !ok {
		return boostErr(http.StatusConflict, "no_creative", "Mark at least one creative In use before going live.")
	}
	return nil
}

/* ------------------------------------------------------------------ */
/*  Background: expiry and invoices                                    */
/* ------------------------------------------------------------------ */

// expireUnpaid cancels budget Boosts not paid within the payment window.
func (s *BoostService) expireUnpaid(ctx context.Context) {
	ids, err := s.repo.DueForPayment(ctx, time.Now())
	if err != nil {
		log.Printf("[BOOST] payment expiry: %v", err)
		return
	}
	for _, id := range ids {
		expired := false
		err := s.inTx(ctx, func(r *repository.BoostRepository) error {
			b, err := r.GetForUpdate(ctx, id)
			if err != nil || b == nil {
				return err
			}
			if b.Status != models.BoostQuoteReady || b.BudgetStatus != models.BudgetUnpaid || b.PaymentDueBy == nil || b.PaymentDueBy.After(time.Now()) {
				return nil
			}
			if err := r.SetCancelledFor(ctx, id, "payment_expired"); err != nil {
				return err
			}
			if err := r.ExpireOpenPayments(ctx, id); err != nil {
				return err
			}
			expired = true
			return r.Log(ctx, id, "system", "", "payment_expired", nil)
		})
		if err != nil {
			log.Printf("[BOOST] expire %s: %v", id, err)
			continue
		}
		if expired {
			if b, err := s.repo.GetByID(ctx, id); err == nil && b != nil {
				s.mailHost(ctx, b, "Your Boost plan expired",
					fmt.Sprintf("<p>The ad budget for <strong>%s</strong> wasn't paid in time, so the plan expired. Nothing was charged. You can request a new Boost any time.</p>", html.EscapeString(b.EventTitle)))
			}
		}
	}
}

// invoicePending issues budget invoices for paid payments that have none yet
// (billing was off when paid, or the first try failed).
func (s *BoostService) invoicePending(ctx context.Context) {
	if s.billing == nil {
		return
	}
	ids, err := s.repo.UninvoicedPayments(ctx, 50)
	if err != nil {
		log.Printf("[BOOST] budget invoices: %v", err)
		return
	}
	for _, id := range ids {
		if err := s.issueBudgetInvoice(ctx, id); err != nil {
			log.Printf("[BOOST] budget invoice %s: %v", id, err)
		}
	}
}

func (s *BoostService) budgetSale(ctx context.Context, p *models.BoostPayment) (*BudgetSale, error) {
	b, err := s.repo.GetByID(ctx, p.BoostID)
	if err != nil || b == nil {
		return nil, fmt.Errorf("load boost %s: %v", p.BoostID, err)
	}
	desc := "Marketing Boost ad budget: " + b.EventTitle
	if b.BudgetFrom != nil && b.BudgetUntil != nil && p.Kind == models.BoostPaymentBudget {
		desc += fmt.Sprintf(" (%s to %s)", *b.BudgetFrom, *b.BudgetUntil)
	}
	if p.Kind == models.BoostPaymentTopUp {
		desc = "Marketing Boost ad budget top-up: " + b.EventTitle
	}
	return &BudgetSale{PaymentID: p.ID, HostID: b.HostID, EventID: b.EventID, EventTitle: b.EventTitle,
		Description: desc, AmountCents: p.AmountCents, GSTCents: p.GSTCents}, nil
}

func (s *BoostService) issueBudgetInvoice(ctx context.Context, paymentID uuid.UUID) error {
	if s.billing == nil {
		return nil
	}
	p, err := s.repo.GetPayment(ctx, paymentID)
	if err != nil || p == nil || p.Status != models.BoostPayPaid || p.InvoiceID != nil {
		return err
	}
	sale, err := s.budgetSale(ctx, p)
	if err != nil {
		return err
	}
	inv, err := s.billing.IssueBudgetDoc(ctx, models.InvoiceKindInvoice, *sale)
	if err != nil || inv == nil {
		return err
	}
	return s.repo.SetPaymentInvoice(ctx, p.ID, inv.ID)
}

/* ------------------------------------------------------------------ */
/*  Host access to one Boost                                           */
/* ------------------------------------------------------------------ */

// hostBoost loads a Boost the signed-in host may read (owner or co-host) or,
// with write, act on (owner only).
func (s *BoostService) hostBoost(ctx context.Context, userID, boostID uuid.UUID, write bool) (*models.Boost, *models.Host, error) {
	h, err := s.hostOf(ctx, userID)
	if err != nil {
		return nil, nil, err
	}
	b, err := s.repo.GetByID(ctx, boostID)
	if err != nil {
		return nil, nil, err
	}
	if b != nil && b.HostID == h.ID {
		return b, h, nil
	}
	if b != nil && !write {
		if ok, err := s.eventRepo.HostCanManage(ctx, b.EventID, h.ID); err == nil && ok {
			return b, h, nil
		}
	}
	return nil, nil, boostErr(http.StatusNotFound, "boost_not_found", "Boost not found.")
}

func (s *BoostService) adminBoost(ctx context.Context, boostID uuid.UUID) (*models.Boost, error) {
	b, err := s.repo.GetByID(ctx, boostID)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, boostErr(http.StatusNotFound, "boost_not_found", "Boost not found.")
	}
	return b, nil
}

/* ------------------------------------------------------------------ */
/*  Payments                                                           */
/* ------------------------------------------------------------------ */

// PaymentRequest asks for the budget or a top-up.
type PaymentRequest struct {
	Kind             string `json:"kind"`
	DaysAdded        int    `json:"days_added"`
	DailyBudgetCents *int64 `json:"daily_budget_cents"` // top-up: the new, higher daily budget
}

// PaymentOrder is what Razorpay checkout needs.
type PaymentOrder struct {
	PaymentID   uuid.UUID `json:"payment_id"`
	KeyID       string    `json:"key_id"`
	OrderID     string    `json:"order_id"`
	AmountCents int64     `json:"amount_cents"`
	Currency    string    `json:"currency"`
}

// quote is what a payment will cost (before GST, its GST) and what it buys.
type quote struct {
	kind     string
	amount   int64
	gst      int64
	days     int
	newDaily *int64
}

// quotePayment prices the budget or a top-up from the Boost's own plan; the
// client never sends an amount.
func (s *BoostService) quotePayment(ctx context.Context, b *models.Boost, req PaymentRequest, now time.Time) (*quote, error) {
	if b.PricingModel != models.BoostPricingBudget {
		return nil, boostErr(http.StatusConflict, "no_budget", "This Boost has no ad budget to pay.")
	}
	st, err := s.repo.Settings(ctx)
	if err != nil {
		return nil, err
	}
	switch req.Kind {
	case models.BoostPaymentBudget, "":
		if b.Status != models.BoostQuoteReady || b.AcceptedAt == nil {
			return nil, boostErr(http.StatusConflict, "invalid_state", "Accept the proposal first, then pay the budget.")
		}
		if b.BudgetStatus == models.BudgetPaid {
			return nil, boostErr(http.StatusConflict, "already_paid", "The budget is already paid.")
		}
		if b.PaymentDueBy != nil && !b.PaymentDueBy.After(now) {
			return nil, boostErr(http.StatusConflict, "payment_expired", "The time to pay this plan has run out.")
		}
		if b.BudgetTotalCents <= 0 {
			return nil, boostErr(http.StatusConflict, "no_budget", "This Boost has no budget plan yet.")
		}
		return &quote{kind: models.BoostPaymentBudget, amount: b.BudgetTotalCents, gst: b.BudgetGSTCents}, nil
	case models.BoostPaymentTopUp:
		if b.Status != models.BoostLive {
			return nil, boostErr(http.StatusConflict, "invalid_state", "You can top up a live Boost.")
		}
		if b.DailyBudgetCents == nil || b.BudgetFrom == nil || b.BudgetUntil == nil {
			return nil, boostErr(http.StatusConflict, "no_budget", "This Boost has no budget plan.")
		}
		q := &quote{kind: models.BoostPaymentTopUp}
		switch {
		case req.DaysAdded > 0 && req.DailyBudgetCents == nil:
			if req.DaysAdded > 365 {
				return nil, boostErr(http.StatusBadRequest, "invalid_top_up", "That's too many days.")
			}
			evt, err := s.eventRepo.GetByID(ctx, b.EventID)
			if err != nil || evt == nil {
				return nil, fmt.Errorf("load event: %v", err)
			}
			last, err := s.lastBoostDay(ctx, evt, now)
			if err != nil {
				return nil, err
			}
			if newUntil := addDay(*b.BudgetUntil, req.DaysAdded); last == "" || newUntil > last {
				return nil, boostErr(http.StatusBadRequest, "invalid_top_up", "The Boost can't run past the event's last date.")
			}
			q.days, q.amount = req.DaysAdded, *b.DailyBudgetCents*int64(req.DaysAdded)
		case req.DailyBudgetCents != nil && req.DaysAdded == 0:
			nd := *req.DailyBudgetCents
			if nd <= *b.DailyBudgetCents || nd%st.BudgetStepCents != 0 {
				return nil, boostErr(http.StatusBadRequest, "invalid_top_up", fmt.Sprintf("Raise the daily budget above %s, in steps of %s.", rupeesText(*b.DailyBudgetCents), rupeesText(st.BudgetStepCents)))
			}
			from := istDay(now)
			if *b.BudgetFrom > from {
				from = *b.BudgetFrom
			}
			left := daysIncl(from, *b.BudgetUntil)
			if left < 1 {
				return nil, boostErr(http.StatusBadRequest, "invalid_top_up", "No days are left to raise the budget for.")
			}
			q.newDaily, q.amount = &nd, (nd-*b.DailyBudgetCents)*int64(left)
		default:
			return nil, boostErr(http.StatusBadRequest, "invalid_top_up", "Add days or raise the daily budget (one of them).")
		}
		q.gst = budgetGST(q.amount, st.BudgetGSTBps)
		return q, nil
	}
	return nil, boostErr(http.StatusBadRequest, "invalid_kind", "kind is budget or top_up.")
}

func newBoostPayment(b *models.Boost, q *quote, method string) *models.BoostPayment {
	return &models.BoostPayment{ID: uuid.New(), BoostID: b.ID, Kind: q.kind, AmountCents: q.amount, GSTCents: q.gst,
		DaysAdded: q.days, NewDailyBudgetCents: q.newDaily, Method: method, Status: models.BoostPayCreated}
}

// CreatePayment opens a Razorpay order for the budget or a top-up.
func (s *BoostService) CreatePayment(ctx context.Context, userID, boostID uuid.UUID, req PaymentRequest) (*PaymentOrder, error) {
	if s.payments == nil {
		return nil, errPlanOff
	}
	b, h, err := s.hostBoost(ctx, userID, boostID, true)
	if err != nil {
		return nil, err
	}
	if err := s.requirePremium(ctx, s.repo, h.ID); err != nil {
		return nil, err
	}
	q, err := s.quotePayment(ctx, b, req, time.Now())
	if err != nil {
		return nil, err
	}
	p := newBoostPayment(b, q, models.BoostPayCheckout)
	order, err := s.payments.CreateOrder(ctx, payment.OrderRequest{AmountCents: p.TotalCents(), Currency: "INR",
		ReceiptID: "bst_" + strings.ReplaceAll(p.ID.String(), "-", "")[:32],
		Notes:     map[string]string{"boost_payment_id": p.ID.String(), "boost_id": b.ID.String(), "kind": p.Kind}})
	if err != nil {
		return nil, fmt.Errorf("create order: %w", err)
	}
	p.RazorpayOrderID = &order.OrderID
	if err := s.repo.CreatePayment(ctx, p); err != nil {
		return nil, err
	}
	_ = s.repo.Log(ctx, b.ID, "host", h.ID.String(), "payment_started", map[string]any{"payment_id": p.ID, "kind": p.Kind, "total_cents": p.TotalCents()})
	return &PaymentOrder{PaymentID: p.ID, KeyID: s.payments.GetKeyID(), OrderID: order.OrderID, AmountCents: p.TotalCents(), Currency: "INR"}, nil
}

// VerifyRequest is the checkout callback.
type VerifyRequest struct {
	OrderID   string `json:"razorpay_order_id"`
	PaymentID string `json:"razorpay_payment_id"`
	Signature string `json:"razorpay_signature"`
}

// VerifyPayment checks the checkout signature and records the payment.
func (s *BoostService) VerifyPayment(ctx context.Context, userID, boostID, paymentID uuid.UUID, req VerifyRequest) (*models.Boost, error) {
	if s.payments == nil {
		return nil, errPlanOff
	}
	b, h, err := s.hostBoost(ctx, userID, boostID, true)
	if err != nil {
		return nil, err
	}
	p, err := s.repo.GetPayment(ctx, paymentID)
	if err != nil {
		return nil, err
	}
	if p == nil || p.BoostID != b.ID || p.RazorpayOrderID == nil || *p.RazorpayOrderID != req.OrderID {
		return nil, boostErr(http.StatusNotFound, "payment_not_found", "Payment not found.")
	}
	if !s.payments.VerifyPaymentSignature(payment.VerifyRequest{OrderID: req.OrderID, PaymentID: req.PaymentID, Signature: req.Signature}) {
		return nil, boostErr(http.StatusBadRequest, "invalid_signature", "The payment couldn't be verified.")
	}
	if err := s.markPaid(ctx, paymentID, req.OrderID, req.PaymentID, nil, "host", h.ID.String()); err != nil {
		return nil, err
	}
	return s.repo.GetByID(ctx, boostID)
}

// HandleGatewayPayment records a Razorpay payment from the webhook
// (payment.captured or payment_link.paid). handled is false when the order
// isn't a Boost payment, so the caller can try the wallet.
func (s *BoostService) HandleGatewayPayment(ctx context.Context, orderID, linkID, paymentID string, amountPaise int64, notes map[string]string) (bool, error) {
	id, err := s.repo.PaymentIDByGateway(ctx, orderID, linkID)
	if err != nil {
		return false, err
	}
	if id == nil {
		v, ok := notes["boost_payment_id"]
		if !ok {
			return false, nil
		}
		pid, err := uuid.Parse(v)
		if err != nil {
			return false, nil
		}
		id = &pid
	}
	var amt *int64
	if amountPaise > 0 {
		amt = &amountPaise
	}
	return true, s.markPaid(ctx, *id, orderID, paymentID, amt, "system", "razorpay")
}

// markPaid records a payment as paid and applies it to the Boost. Safe to
// run more than once (verify, webhook, a replayed webhook): only the first
// moves it. A payment that arrives after its plan expired is recorded and the
// team is told, since the money has to be refunded or the Boost restored.
func (s *BoostService) markPaid(ctx context.Context, paymentID uuid.UUID, orderID, gatewayPaymentID string, amountPaise *int64, actorKind, actor string) error {
	pre, err := s.repo.GetPayment(ctx, paymentID)
	if err != nil {
		return err
	}
	if pre == nil {
		return boostErr(http.StatusNotFound, "payment_not_found", "Payment not found.")
	}
	if amountPaise != nil && *amountPaise != pre.TotalCents() {
		return fmt.Errorf("boost payment %s: paid %d paise, expected %d", paymentID, *amountPaise, pre.TotalCents())
	}
	applied, orphan, duplicate := false, false, false
	var b *models.Boost
	err = s.inTx(ctx, func(r *repository.BoostRepository) error {
		var err error
		// Lock order: the Boost, then its payment (as the expiry job does).
		if b, err = r.GetForUpdate(ctx, pre.BoostID); err != nil || b == nil {
			return fmt.Errorf("load boost: %v", err)
		}
		p, err := r.GetPaymentForUpdate(ctx, paymentID)
		if err != nil {
			return err
		}
		if p.Status == models.BoostPayPaid {
			return nil
		}
		if p.Status == models.BoostPayReversed {
			orphan = true
			return r.Log(ctx, b.ID, actorKind, actor, "payment_after_reversal", map[string]any{"payment_id": p.ID, "gateway_payment_id": gatewayPaymentID})
		}
		if ok, err := r.MarkPaymentPaid(ctx, p.ID, orderID, gatewayPaymentID); err != nil || !ok {
			return err
		}
		applied = true
		live := b.Status == models.BoostQuoteReady || b.Status == models.BoostLive
		switch {
		case !live:
			orphan = true
		case p.Kind == models.BoostPaymentBudget && b.BudgetStatus == models.BudgetPaid:
			duplicate = true
		case p.Kind == models.BoostPaymentBudget:
			if err := r.SetBudgetStatus(ctx, b.ID, models.BudgetPaid); err != nil {
				return err
			}
		default:
			if err := r.ApplyTopUp(ctx, b.ID, p.AmountCents, p.GSTCents, p.DaysAdded, p.NewDailyBudgetCents); err != nil {
				return err
			}
		}
		return r.Log(ctx, b.ID, actorKind, actor, "payment_paid", map[string]any{"payment_id": p.ID, "kind": p.Kind,
			"method": p.Method, "total_cents": p.TotalCents(), "gateway_payment_id": gatewayPaymentID,
			"orphan": orphan, "duplicate": duplicate})
	})
	if err != nil || !applied && !orphan {
		return err
	}
	if orphan || duplicate {
		s.mailAdmin("Boost payment needs a look: "+b.EventTitle, fmt.Sprintf(
			"<p>A %s payment of %s arrived for the Boost on <strong>%s</strong>, which is %s%s. Refund it or restore the Boost by hand.</p>",
			pre.Kind, rupeesText(pre.TotalCents()), html.EscapeString(b.EventTitle), b.Status,
			map[bool]string{true: " and already paid", false: ""}[duplicate]))
		return nil
	}
	if err := s.issueBudgetInvoice(ctx, paymentID); err != nil {
		log.Printf("[BOOST] budget invoice %s: %v (retried later)", paymentID, err)
	}
	if out, err := s.repo.GetByID(ctx, b.ID); err == nil && out != nil {
		what := "ad budget"
		if pre.Kind == models.BoostPaymentTopUp {
			what = "budget top-up"
		}
		s.mailHost(ctx, out, "Payment received for your Boost",
			fmt.Sprintf("<p>We received %s for the %s of your Boost on <strong>%s</strong>. Your tax invoice is in Earnings and invoices.</p>",
				rupeesText(pre.TotalCents()), what, html.EscapeString(out.EventTitle)))
		s.mailAdmin("Boost budget paid: "+out.EventTitle, fmt.Sprintf("<p>The %s for <strong>%s</strong> is paid (%s).%s</p>",
			what, html.EscapeString(out.EventTitle), rupeesText(pre.TotalCents()),
			map[bool]string{true: " It's ready to go live once a creative is In use.", false: ""}[pre.Kind == models.BoostPaymentBudget]))
	}
	return nil
}

// PaymentLinkResult is the link the team can copy.
type PaymentLinkResult struct {
	Payment *models.BoostPayment `json:"payment"`
	URL     string               `json:"url"`
}

// AdminPaymentLink creates a Razorpay payment link for the budget or a top-up;
// Razorpay sends it to the host by SMS and email.
func (s *BoostService) AdminPaymentLink(ctx context.Context, admin string, boostID uuid.UUID, req PaymentRequest) (*PaymentLinkResult, error) {
	if s.payments == nil {
		return nil, errPlanOff
	}
	b, err := s.adminBoost(ctx, boostID)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	q, err := s.quotePayment(ctx, b, req, now)
	if err != nil {
		return nil, err
	}
	h, err := s.hostRepo.GetByID(ctx, b.HostID)
	if err != nil || h == nil {
		return nil, fmt.Errorf("load host: %v", err)
	}
	u, _ := s.userRepo.GetByID(ctx, h.UserID)
	email, phone := "", h.PhnNumber
	if u != nil {
		email = u.Email
		if phone == "" {
			phone = u.PhnNumber
		}
	}
	phone = strings.ReplaceAll(strings.TrimSpace(phone), " ", "")
	if len(phone) == 10 {
		phone = "+91" + phone
	}
	expire := now.Add(7 * 24 * time.Hour)
	if q.kind == models.BoostPaymentBudget && b.PaymentDueBy != nil {
		expire = *b.PaymentDueBy
	}
	if expire.Before(now.Add(20 * time.Minute)) {
		expire = now.Add(20 * time.Minute) // Razorpay needs at least 15 minutes
	}
	p := newBoostPayment(b, q, models.BoostPayLink)
	what := "Ad budget"
	if q.kind == models.BoostPaymentTopUp {
		what = "Ad budget top-up"
	}
	link, err := s.payments.CreatePaymentLink(ctx, payment.PaymentLinkRequest{AmountCents: p.TotalCents(), ReferenceID: p.ID.String(),
		Description: fmt.Sprintf("%s for your MySlotMate Boost: %s (incl. GST)", what, b.EventTitle),
		Name:        strings.TrimSpace(h.FirstName + " " + h.LastName), Email: email, Phone: phone, ExpireBy: expire,
		Notes: map[string]string{"boost_payment_id": p.ID.String(), "boost_id": b.ID.String(), "kind": p.Kind}})
	if err != nil {
		return nil, fmt.Errorf("create payment link: %w", err)
	}
	p.RazorpayLinkID, p.LinkURL = &link.ID, &link.ShortURL
	if err := s.repo.CreatePayment(ctx, p); err != nil {
		return nil, err
	}
	_ = s.repo.Log(ctx, b.ID, "admin", admin, "payment_link_sent", map[string]any{"payment_id": p.ID, "kind": p.Kind,
		"total_cents": p.TotalCents(), "expires": expire})
	if wa, ok := s.notify.(BoostWhatsApp); ok && phone != "" {
		params := map[string]string{"name": strings.TrimSpace(h.FirstName), "event_name": b.EventTitle,
			"amount": rupeesText(p.TotalCents()), "link": link.ShortURL, "expires": expire.In(istZone).Format("2 Jan 2006")}
		go func() {
			if err := wa.BoostPaymentWhatsApp(context.Background(), phone, params); err != nil {
				log.Printf("[BOOST] payment link WhatsApp %s: %v", b.ID, err)
			}
		}()
	}
	s.mailHost(ctx, b, "Pay your Boost budget", fmt.Sprintf(
		"<p>%s for your Boost on <strong>%s</strong>: %s (%s plus %s GST).</p><p><a href=\"%s\">Pay now</a>. The link expires on %s.</p>",
		what, html.EscapeString(b.EventTitle), rupeesText(p.TotalCents()), rupeesText(p.AmountCents), rupeesText(p.GSTCents),
		html.EscapeString(link.ShortURL), expire.In(istZone).Format("2 Jan 2006, 3:04 PM")))
	return &PaymentLinkResult{Payment: p, URL: link.ShortURL}, nil
}

// OfflinePayment is a payment made another way (bank transfer, UPI).
type OfflinePayment struct {
	PaymentRequest
	AmountCents int64  `json:"amount_cents"` // what arrived, GST included: must match the quote
	PaidOn      string `json:"paid_on"`      // YYYY-MM-DD
	Reference   string `json:"reference"`
}

// AdminOfflinePayment records a budget or top-up paid outside Razorpay.
func (s *BoostService) AdminOfflinePayment(ctx context.Context, admin string, boostID uuid.UUID, in OfflinePayment) (*models.Boost, error) {
	ref := strings.TrimSpace(in.Reference)
	if ref == "" || tooLong(ref, 200) {
		return nil, boostErr(http.StatusBadRequest, "reference_required", "Give the bank or UPI reference (up to 200 characters).")
	}
	if _, ok := parseDay(in.PaidOn); !ok || in.PaidOn > istDay(time.Now()) {
		return nil, boostErr(http.StatusBadRequest, "invalid_date", "Give the date it was paid (not in the future).")
	}
	b, err := s.adminBoost(ctx, boostID)
	if err != nil {
		return nil, err
	}
	q, err := s.quotePayment(ctx, b, in.PaymentRequest, time.Now())
	if err != nil {
		return nil, err
	}
	p := newBoostPayment(b, q, models.BoostPayOffline)
	if in.AmountCents != p.TotalCents() {
		return nil, boostErr(http.StatusBadRequest, "amount_mismatch", fmt.Sprintf("This payment should be %s (%s plus %s GST).",
			rupeesText(p.TotalCents()), rupeesText(p.AmountCents), rupeesText(p.GSTCents)))
	}
	paidOn := in.PaidOn
	p.OfflineReference, p.OfflinePaidOn, p.RecordedBy = &ref, &paidOn, &admin
	if err := s.repo.CreatePayment(ctx, p); err != nil {
		return nil, err
	}
	if err := s.markPaid(ctx, p.ID, "", "", nil, "admin", admin); err != nil {
		return nil, err
	}
	return s.repo.GetByID(ctx, boostID)
}

// AdminReversePayment undoes a recorded offline budget payment that never
// arrived, before the Boost goes live. A budget invoice gets a credit note.
func (s *BoostService) AdminReversePayment(ctx context.Context, admin string, boostID, paymentID uuid.UUID, reason string) (*models.Boost, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" || tooLong(reason, 500) {
		return nil, boostErr(http.StatusBadRequest, "reason_required", "Say why (up to 500 characters).")
	}
	err := s.inTx(ctx, func(r *repository.BoostRepository) error {
		b, err := r.GetForUpdate(ctx, boostID)
		if err != nil {
			return err
		}
		if b == nil {
			return boostErr(http.StatusNotFound, "boost_not_found", "Boost not found.")
		}
		p, err := r.GetPaymentForUpdate(ctx, paymentID)
		if err != nil {
			return err
		}
		if p == nil || p.BoostID != b.ID {
			return boostErr(http.StatusNotFound, "payment_not_found", "Payment not found.")
		}
		if p.Method != models.BoostPayOffline || p.Status != models.BoostPayPaid || p.Kind != models.BoostPaymentBudget {
			return boostErr(http.StatusConflict, "not_reversible", "Only a recorded offline budget payment can be reversed.")
		}
		if b.Status != models.BoostQuoteReady {
			return boostErr(http.StatusConflict, "already_live", "The Boost has gone live: handle this outside the system and note it in the history.")
		}
		if err := r.SetPaymentStatus(ctx, p.ID, models.BoostPayReversed, &reason); err != nil {
			return err
		}
		if err := r.SetBudgetStatus(ctx, b.ID, models.BudgetUnpaid); err != nil {
			return err
		}
		return r.Log(ctx, b.ID, "admin", admin, "payment_reversed", map[string]any{"payment_id": p.ID, "reason": reason})
	})
	if err != nil {
		return nil, err
	}
	if s.billing != nil {
		if p, err := s.repo.GetPayment(ctx, paymentID); err == nil && p != nil && p.InvoiceID != nil {
			if sale, err := s.budgetSale(ctx, p); err == nil {
				if _, err := s.billing.IssueBudgetDoc(ctx, models.InvoiceKindCreditNote, *sale); err != nil {
					log.Printf("[BOOST] budget credit note %s: %v", paymentID, err)
				}
			}
		}
	}
	return s.repo.GetByID(ctx, boostID)
}

// Documents lists the Boost's budget invoices and credit notes.
func (s *BoostService) Documents(ctx context.Context, userID, boostID uuid.UUID) ([]repository.BudgetInvoice, error) {
	b, _, err := s.hostBoost(ctx, userID, boostID, true)
	if err != nil {
		return nil, err
	}
	docs, err := s.repo.BudgetInvoices(ctx, b.ID)
	for i := range docs {
		u := fmt.Sprintf("/billing/documents/%s/pdf", docs[i].ID)
		docs[i].PDFURL = &u
	}
	return docs, err
}

/* ------------------------------------------------------------------ */
/*  Spend and results                                                  */
/* ------------------------------------------------------------------ */

// SpendInput is one row of the team's spend grid.
type SpendInput struct {
	Day         string `json:"day"`
	Channel     string `json:"channel"`
	SpendCents  int64  `json:"spend_cents"`
	Impressions int64  `json:"impressions"`
	Reach       int64  `json:"reach"`
	Clicks      int64  `json:"clicks"`
	Note        string `json:"note"`
}

// AdminSpend upserts daily spend entries. Refuses a total above the paid
// budget, and any change once the Boost is closed.
func (s *BoostService) AdminSpend(ctx context.Context, admin string, boostID uuid.UUID, entries []SpendInput) (*models.Boost, error) {
	if len(entries) == 0 || len(entries) > spendEntriesMax {
		return nil, boostErr(http.StatusBadRequest, "invalid_entries", fmt.Sprintf("Send 1 to %d entries.", spendEntriesMax))
	}
	today := istDay(time.Now())
	clean := make([]models.BoostSpendEntry, 0, len(entries))
	for _, e := range entries {
		c := strings.ToLower(strings.TrimSpace(e.Channel))
		if _, ok := parseDay(e.Day); !ok || e.Day > today {
			return nil, boostErr(http.StatusBadRequest, "invalid_day", "Each entry needs a day (YYYY-MM-DD), not in the future.")
		}
		if !models.BoostChannels[c] {
			return nil, boostErr(http.StatusBadRequest, "invalid_channels", "Unknown channel "+e.Channel+".")
		}
		if e.SpendCents < 0 || e.Impressions < 0 || e.Reach < 0 || e.Clicks < 0 {
			return nil, boostErr(http.StatusBadRequest, "invalid_numbers", "Numbers can't be negative.")
		}
		if tooLong(e.Note, 500) {
			return nil, boostErr(http.StatusBadRequest, "too_long", "Keep notes under 500 characters.")
		}
		clean = append(clean, models.BoostSpendEntry{Day: e.Day, Channel: c, SpendCents: e.SpendCents, Impressions: e.Impressions,
			Reach: e.Reach, Clicks: e.Clicks, Note: trimmedPtr(e.Note), EnteredBy: admin})
	}
	var before, after, paid int64
	err := s.inTx(ctx, func(r *repository.BoostRepository) error {
		b, err := r.GetForUpdate(ctx, boostID)
		if err != nil {
			return err
		}
		if b == nil {
			return boostErr(http.StatusNotFound, "boost_not_found", "Boost not found.")
		}
		if b.PricingModel != models.BoostPricingBudget {
			return boostErr(http.StatusConflict, "no_budget", "This Boost has no ad budget.")
		}
		if b.Status != models.BoostLive && b.Status != models.BoostEnded {
			return boostErr(http.StatusConflict, "invalid_state", "Spend can be logged once the Boost is live.")
		}
		if b.ClosedAt != nil {
			return boostErr(http.StatusConflict, "closed", "This Boost is closed: its spend is locked.")
		}
		before, paid = b.SpentCents, b.PaidCents
		for _, e := range clean {
			if err := r.UpsertSpend(ctx, b.ID, e); err != nil {
				return err
			}
		}
		if after, err = r.RefreshSpent(ctx, b.ID); err != nil {
			return err
		}
		if after > paid {
			left := paid - before
			if left < 0 {
				left = 0
			}
			return boostErr(http.StatusUnprocessableEntity, "over_budget", fmt.Sprintf(
				"That makes the spend %s, above the %s paid. %s was left before these entries.", rupeesText(after), rupeesText(paid), rupeesText(left)))
		}
		return r.Log(ctx, b.ID, "admin", admin, "spend_logged", map[string]any{"entries": len(clean), "spent_cents": after})
	})
	if err != nil {
		return nil, err
	}
	out, err := s.repo.GetByID(ctx, boostID)
	if err == nil && out != nil && paid > 0 && before*10000 < paid*budgetWarnBps && after*10000 >= paid*budgetWarnBps {
		s.mailHost(ctx, out, "80% of your Boost budget is spent", fmt.Sprintf(
			"<p>Your Boost on <strong>%s</strong> has spent %s of the %s budget. Top up from the event page to keep it running to the end.</p>",
			html.EscapeString(out.EventTitle), rupeesText(after), rupeesText(paid)))
	}
	return out, err
}

// ResultsDay is one day of a Boost's results.
type ResultsDay struct {
	Day         string `json:"day"`
	SpendCents  int64  `json:"spend_cents"`
	Impressions int64  `json:"impressions"`
	Reach       int64  `json:"reach"`
	Clicks      int64  `json:"clicks"`
	TicketsSold int    `json:"tickets_sold"`
}

type ResultsChannel struct {
	Channel     string `json:"channel"`
	SpendCents  int64  `json:"spend_cents"`
	Impressions int64  `json:"impressions"`
	Reach       int64  `json:"reach"`
	Clicks      int64  `json:"clicks"`
}

type Results struct {
	Days     []ResultsDay     `json:"days"`
	Channels []ResultsChannel `json:"channels"`
	Totals   ResultsDay       `json:"totals"` // day is ""
}

// buildResults merges the spend log with tickets sold per day.
func buildResults(spend []models.BoostSpendEntry, tickets map[string]int) *Results {
	byDay := map[string]*ResultsDay{}
	var days []string
	day := func(d string) *ResultsDay {
		if x, ok := byDay[d]; ok {
			return x
		}
		x := &ResultsDay{Day: d}
		byDay[d] = x
		days = append(days, d)
		return x
	}
	byChan := map[string]*ResultsChannel{}
	var chans []string
	res := &Results{Days: []ResultsDay{}, Channels: []ResultsChannel{}}
	for _, e := range spend {
		d := day(e.Day)
		d.SpendCents += e.SpendCents
		d.Impressions += e.Impressions
		d.Reach += e.Reach
		d.Clicks += e.Clicks
		c, ok := byChan[e.Channel]
		if !ok {
			c = &ResultsChannel{Channel: e.Channel}
			byChan[e.Channel] = c
			chans = append(chans, e.Channel)
		}
		c.SpendCents += e.SpendCents
		c.Impressions += e.Impressions
		c.Reach += e.Reach
		c.Clicks += e.Clicks
	}
	for d, n := range tickets {
		day(d).TicketsSold += n
	}
	sort.Strings(days)
	for _, d := range days {
		x := *byDay[d]
		res.Days = append(res.Days, x)
		res.Totals.SpendCents += x.SpendCents
		res.Totals.Impressions += x.Impressions
		res.Totals.Reach += x.Reach
		res.Totals.Clicks += x.Clicks
		res.Totals.TicketsSold += x.TicketsSold
	}
	for _, c := range chans {
		res.Channels = append(res.Channels, *byChan[c])
	}
	return res
}

func (s *BoostService) results(ctx context.Context, boostID uuid.UUID) (*Results, error) {
	spend, err := s.repo.SpendEntries(ctx, boostID)
	if err != nil {
		return nil, err
	}
	tickets, err := s.repo.TicketsPerDay(ctx, boostID)
	if err != nil {
		return nil, err
	}
	return buildResults(spend, tickets), nil
}

// Results is the host's daily results.
func (s *BoostService) Results(ctx context.Context, userID, boostID uuid.UUID) (*Results, error) {
	if _, _, err := s.hostBoost(ctx, userID, boostID, false); err != nil {
		return nil, err
	}
	return s.results(ctx, boostID)
}

func (s *BoostService) AdminResults(ctx context.Context, boostID uuid.UUID) (*Results, error) {
	if _, err := s.adminBoost(ctx, boostID); err != nil {
		return nil, err
	}
	return s.results(ctx, boostID)
}

/* ------------------------------------------------------------------ */
/*  Close                                                              */
/* ------------------------------------------------------------------ */

// AdminClose ends the team's work on an ended Boost and locks its spend log.
func (s *BoostService) AdminClose(ctx context.Context, admin string, boostID uuid.UUID) (*models.Boost, error) {
	err := s.inTx(ctx, func(r *repository.BoostRepository) error {
		b, err := r.GetForUpdate(ctx, boostID)
		if err != nil {
			return err
		}
		if b == nil {
			return boostErr(http.StatusNotFound, "boost_not_found", "Boost not found.")
		}
		if b.Status != models.BoostEnded {
			return boostErr(http.StatusConflict, "invalid_state", "End the Boost first.")
		}
		if b.ClosedAt != nil {
			return boostErr(http.StatusConflict, "closed", "This Boost is already closed.")
		}
		ok, err := r.HasFinalReport(ctx, b.ID)
		if err != nil {
			return err
		}
		if !ok {
			return boostErr(http.StatusConflict, "no_final_report", "Attach the final report before closing.")
		}
		if err := r.SetClosed(ctx, b.ID, admin); err != nil {
			return err
		}
		return r.Log(ctx, b.ID, "admin", admin, "closed", map[string]any{"spent_cents": b.SpentCents, "paid_cents": b.PaidCents})
	})
	if err != nil {
		return nil, err
	}
	out, err := s.repo.GetByID(ctx, boostID)
	if err == nil && out != nil {
		s.mailHost(ctx, out, "Your Boost's final report is ready", fmt.Sprintf(
			"<p>The final report for your Boost on <strong>%s</strong> is ready: %d tickets sold while live, %s spent on ads of the %s paid.</p>",
			html.EscapeString(out.EventTitle), out.TicketsSold, rupeesText(out.SpentCents), rupeesText(out.PaidCents)))
	}
	return out, err
}

/* ------------------------------------------------------------------ */
/*  Uploads (creatives and reports)                                    */
/* ------------------------------------------------------------------ */

// UploadRequest asks for a signed URL for one file.
type UploadRequest struct {
	Name     string `json:"name"`
	MimeType string `json:"mime_type"`
	Size     int64  `json:"size"`
}

type UploadURL struct {
	UploadURL string `json:"upload_url"`
	Key       string `json:"key"`
	URL       string `json:"url"`
}

var creativeMimes = map[string]bool{"image/jpeg": true, "image/png": true, "video/mp4": true, "video/quicktime": true,
	"application/pdf": true, "application/zip": true}

var reportMimes = map[string]bool{"text/csv": true, "application/vnd.ms-excel": true,
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet": true,
	"application/pdf": true, "image/png": true, "image/jpeg": true}

// creativeRules per type: allowed types and how many files.
var creativeRules = map[string]struct {
	mimes    map[string]bool
	maxFiles int
}{
	models.CreativeReel:  {map[string]bool{"video/mp4": true, "video/quicktime": true}, 1},
	models.CreativePost:  {map[string]bool{"image/jpeg": true, "image/png": true, "video/mp4": true, "video/quicktime": true}, 10},
	models.CreativeStory: {map[string]bool{"image/jpeg": true, "image/png": true, "video/mp4": true, "video/quicktime": true}, 1},
	models.CreativeBrand: {map[string]bool{"image/jpeg": true, "image/png": true, "application/pdf": true, "application/zip": true}, 10},
}

// creativeSizeLimit is the most one file may weigh (boost_settings).
// typ "" is the upload-URL check, before the creative's type is known.
func creativeSizeLimit(st *models.BoostSettings, typ, mime string) int64 {
	switch {
	case typ == models.CreativeBrand:
		return int64(st.CreativeOtherMaxMB) * mb
	case strings.HasPrefix(mime, "video/"):
		return int64(st.CreativeVideoMaxMB) * mb
	case strings.HasPrefix(mime, "image/"):
		if typ == "" && st.CreativeOtherMaxMB > st.CreativeImageMaxMB {
			return int64(st.CreativeOtherMaxMB) * mb // could still be brand material
		}
		return int64(st.CreativeImageMaxMB) * mb
	}
	return int64(st.CreativeOtherMaxMB) * mb
}

func creativeFolder(boostID uuid.UUID) string { return "boosts/" + boostID.String() + "/creatives/" }
func reportFolder(boostID uuid.UUID) string   { return "boosts/" + boostID.String() + "/reports/" }

// signUpload signs one upload. report picks the report limits, else the creative ones.
func (s *BoostService) signUpload(ctx context.Context, folder string, req UploadRequest, report bool) (*UploadURL, error) {
	if s.files == nil {
		return nil, errPlanOff
	}
	mimes := creativeMimes
	if report {
		mimes = reportMimes
	}
	if !mimes[req.MimeType] {
		return nil, boostErr(http.StatusBadRequest, "invalid_file_type", "This file type isn't accepted here.")
	}
	st, err := s.repo.Settings(ctx)
	if err != nil {
		return nil, err
	}
	maxSize := int64(st.ReportMaxMB) * mb
	if !report {
		maxSize = creativeSizeLimit(st, "", req.MimeType)
	}
	if req.Size <= 0 || req.Size > maxSize {
		return nil, boostErr(http.StatusBadRequest, "file_too_large", fmt.Sprintf("Files can be up to %d MB.", maxSize/mb))
	}
	key := folder + uuid.NewString() + "/" + safeName(req.Name)
	u, err := s.files.PresignBoostUpload(ctx, key, req.MimeType, req.Size)
	if err != nil {
		return nil, err
	}
	return &UploadURL{UploadURL: u, Key: key, URL: s.files.SignBoostDownload(key, path.Base(key))}, nil
}

// storedFile checks an uploaded key is under folder and really in storage.
func (s *BoostService) storedFile(ctx context.Context, folder, key string) (*models.StoredFile, error) {
	if !strings.HasPrefix(key, folder) || strings.Contains(key, "..") || strings.Count(strings.TrimPrefix(key, folder), "/") != 1 {
		return nil, boostErr(http.StatusBadRequest, "invalid_file", "That file wasn't uploaded for this Boost.")
	}
	size, mime, err := s.files.HeadBoostObject(ctx, key)
	if err != nil {
		return nil, boostErr(http.StatusBadRequest, "file_missing", "A file didn't finish uploading. Try again.")
	}
	return &models.StoredFile{Key: key, Name: path.Base(key), MimeType: mime, Size: size}, nil
}

func (s *BoostService) signFiles(files []models.StoredFile) []models.StoredFile {
	out := make([]models.StoredFile, len(files))
	for i, f := range files {
		if s.files != nil {
			f.URL = s.files.SignBoostDownload(f.Key, f.Name)
			f.DownloadURL = s.files.SignBoostAttachment(f.Key, f.Name)
		}
		out[i] = f
	}
	return out
}

/* ------------------------------------------------------------------ */
/*  Creatives                                                          */
/* ------------------------------------------------------------------ */

// CreativeInput saves a creative from uploaded files.
type CreativeInput struct {
	Type       string     `json:"type"`
	FileKeys   []string   `json:"file_keys"`
	Caption    string     `json:"caption"`
	Channels   []string   `json:"channels"`
	ReplacesID *uuid.UUID `json:"replaces_id"` // team: the earlier version this replaces
}

func creativesOpen(b *models.Boost) bool {
	return b.Status == models.BoostUnderReview || b.Status == models.BoostQuoteReady || b.Status == models.BoostLive
}

func (s *BoostService) CreativeUploadURL(ctx context.Context, userID, boostID uuid.UUID, req UploadRequest) (*UploadURL, error) {
	b, _, err := s.hostBoost(ctx, userID, boostID, true)
	if err != nil {
		return nil, err
	}
	if !creativesOpen(b) {
		return nil, boostErr(http.StatusConflict, "invalid_state", "Creatives can be added until the Boost ends.")
	}
	return s.signUpload(ctx, creativeFolder(b.ID), req, false)
}

func (s *BoostService) AdminCreativeUploadURL(ctx context.Context, boostID uuid.UUID, req UploadRequest) (*UploadURL, error) {
	b, err := s.adminBoost(ctx, boostID)
	if err != nil {
		return nil, err
	}
	return s.signUpload(ctx, creativeFolder(b.ID), req, false)
}

// saveCreative validates the files against the type's rules and stores it.
func (s *BoostService) saveCreative(ctx context.Context, b *models.Boost, in CreativeInput, byKind, by string) (*models.BoostCreative, error) {
	if s.files == nil {
		return nil, errPlanOff
	}
	rule, ok := creativeRules[in.Type]
	if !ok {
		return nil, boostErr(http.StatusBadRequest, "invalid_type", "type is reel, post, story or brand.")
	}
	if len(in.FileKeys) == 0 || len(in.FileKeys) > rule.maxFiles {
		return nil, boostErr(http.StatusBadRequest, "invalid_files", fmt.Sprintf("A %s takes 1 to %d files.", in.Type, rule.maxFiles))
	}
	if tooLong(in.Caption, 2200) {
		return nil, boostErr(http.StatusBadRequest, "too_long", "Keep the caption under 2,200 characters.")
	}
	var chans []string
	for _, c := range in.Channels {
		c = strings.ToLower(strings.TrimSpace(c))
		if !models.BoostChannels[c] {
			return nil, boostErr(http.StatusBadRequest, "invalid_channels", "Unknown channel "+c+".")
		}
		chans = append(chans, c)
	}
	st, err := s.repo.Settings(ctx)
	if err != nil {
		return nil, err
	}
	c := &models.BoostCreative{ID: uuid.New(), BoostID: b.ID, Type: in.Type, Caption: trimmedPtr(in.Caption), Channels: chans,
		UploadedByKind: byKind, UploadedBy: by, CreatedAt: time.Now(), Status: models.CreativeSubmitted}
	if c.Channels == nil {
		c.Channels = []string{}
	}
	videos := 0
	for _, k := range in.FileKeys {
		f, err := s.storedFile(ctx, creativeFolder(b.ID), k)
		if err != nil {
			return nil, err
		}
		if !rule.mimes[f.MimeType] {
			return nil, boostErr(http.StatusBadRequest, "invalid_file_type", fmt.Sprintf("%s isn't accepted for a %s.", f.Name, in.Type))
		}
		if limit := creativeSizeLimit(st, in.Type, f.MimeType); f.Size > limit {
			return nil, boostErr(http.StatusBadRequest, "file_too_large", fmt.Sprintf("%s is over %d MB.", f.Name, limit/mb))
		}
		if strings.HasPrefix(f.MimeType, "video/") {
			videos++
		}
		c.Files = append(c.Files, *f)
	}
	if in.Type == models.CreativePost && videos > 0 && len(c.Files) > 1 {
		return nil, boostErr(http.StatusBadRequest, "invalid_files", "A post is a carousel of images or one video.")
	}
	if byKind == "admin" {
		c.Status = models.CreativeAwaitingApproval
		if in.ReplacesID != nil {
			old, err := s.repo.GetCreative(ctx, *in.ReplacesID)
			if err != nil {
				return nil, err
			}
			if old == nil || old.BoostID != b.ID {
				return nil, boostErr(http.StatusBadRequest, "invalid_replaces", "The creative it replaces isn't on this Boost.")
			}
			c.ReplacesID = in.ReplacesID
		}
	}
	if err := s.repo.CreateCreative(ctx, c); err != nil {
		return nil, err
	}
	_ = s.repo.Log(ctx, b.ID, byKind, by, "creative_added", map[string]any{"creative_id": c.ID, "type": c.Type, "files": len(c.Files)})
	c.Files = s.signFiles(c.Files)
	return c, nil
}

func (s *BoostService) SaveCreative(ctx context.Context, userID, boostID uuid.UUID, in CreativeInput) (*models.BoostCreative, error) {
	b, h, err := s.hostBoost(ctx, userID, boostID, true)
	if err != nil {
		return nil, err
	}
	if !creativesOpen(b) {
		return nil, boostErr(http.StatusConflict, "invalid_state", "Creatives can be added until the Boost ends.")
	}
	in.ReplacesID = nil
	c, err := s.saveCreative(ctx, b, in, "host", h.ID.String())
	if err == nil {
		s.mailAdmin("New Boost creative: "+b.EventTitle, fmt.Sprintf("<p>The host uploaded a %s for the Boost on <strong>%s</strong>. Review it in the admin.</p>", c.Type, html.EscapeString(b.EventTitle)))
	}
	return c, err
}

func (s *BoostService) AdminSaveCreative(ctx context.Context, admin string, boostID uuid.UUID, in CreativeInput) (*models.BoostCreative, error) {
	b, err := s.adminBoost(ctx, boostID)
	if err != nil {
		return nil, err
	}
	if !creativesOpen(b) {
		return nil, boostErr(http.StatusConflict, "invalid_state", "Creatives can be added until the Boost ends.")
	}
	c, err := s.saveCreative(ctx, b, in, "admin", admin)
	if err == nil {
		s.mailHost(ctx, b, "A creative is waiting for your approval", fmt.Sprintf(
			"<p>The MySlotMate team made a %s for your Boost on <strong>%s</strong>. Approve it or ask for changes on the event page.</p>", c.Type, html.EscapeString(b.EventTitle)))
	}
	return c, err
}

func (s *BoostService) listCreatives(ctx context.Context, boostID uuid.UUID) ([]*models.BoostCreative, error) {
	list, err := s.repo.ListCreatives(ctx, boostID)
	for _, c := range list {
		c.Files = s.signFiles(c.Files)
	}
	return list, err
}

func (s *BoostService) Creatives(ctx context.Context, userID, boostID uuid.UUID) ([]*models.BoostCreative, error) {
	if _, _, err := s.hostBoost(ctx, userID, boostID, false); err != nil {
		return nil, err
	}
	return s.listCreatives(ctx, boostID)
}

func (s *BoostService) AdminCreatives(ctx context.Context, boostID uuid.UUID) ([]*models.BoostCreative, error) {
	if _, err := s.adminBoost(ctx, boostID); err != nil {
		return nil, err
	}
	return s.listCreatives(ctx, boostID)
}

// creativeMove changes one creative's status in a transaction. check says
// whether the move is allowed from the creative's current state.
func (s *BoostService) creativeMove(ctx context.Context, boostID, creativeID uuid.UUID, actorKind, actor, to string, note *string,
	check func(c *models.BoostCreative) error) (*models.BoostCreative, error) {
	err := s.inTx(ctx, func(r *repository.BoostRepository) error {
		c, err := r.GetCreativeForUpdate(ctx, boostID, creativeID)
		if err != nil {
			return err
		}
		if c == nil {
			return boostErr(http.StatusNotFound, "creative_not_found", "Creative not found.")
		}
		if err := check(c); err != nil {
			return err
		}
		if err := r.SetCreativeStatus(ctx, c.ID, to, note); err != nil {
			return err
		}
		return r.Log(ctx, boostID, actorKind, actor, "creative_"+to, map[string]any{"creative_id": c.ID, "note": note})
	})
	if err != nil {
		return nil, err
	}
	c, err := s.repo.GetCreative(ctx, creativeID)
	if c != nil {
		c.Files = s.signFiles(c.Files)
	}
	return c, err
}

func teamAwaiting(c *models.BoostCreative) error {
	if c.UploadedByKind != "admin" || c.Status != models.CreativeAwaitingApproval {
		return boostErr(http.StatusConflict, "invalid_state", "Only a creative from the team that's waiting for you can be approved or sent back.")
	}
	return nil
}

func (s *BoostService) ApproveCreative(ctx context.Context, userID, boostID, creativeID uuid.UUID) (*models.BoostCreative, error) {
	b, h, err := s.hostBoost(ctx, userID, boostID, true)
	if err != nil {
		return nil, err
	}
	c, err := s.creativeMove(ctx, b.ID, creativeID, "host", h.ID.String(), models.CreativeApproved, nil, teamAwaiting)
	if err == nil {
		s.mailAdmin("Creative approved: "+b.EventTitle, fmt.Sprintf("<p>The host approved a %s for <strong>%s</strong>. Mark it In use when it runs.</p>", c.Type, html.EscapeString(b.EventTitle)))
	}
	return c, err
}

func (s *BoostService) RequestCreativeChanges(ctx context.Context, userID, boostID, creativeID uuid.UUID, note string) (*models.BoostCreative, error) {
	note = strings.TrimSpace(note)
	if note == "" || tooLong(note, boostNoteMax) {
		return nil, boostErr(http.StatusBadRequest, "note_required", "Say what to change (up to 1,000 characters).")
	}
	b, h, err := s.hostBoost(ctx, userID, boostID, true)
	if err != nil {
		return nil, err
	}
	c, err := s.creativeMove(ctx, b.ID, creativeID, "host", h.ID.String(), models.CreativeChangesRequested, &note, teamAwaiting)
	if err == nil {
		s.mailAdmin("Creative changes asked: "+b.EventTitle, fmt.Sprintf("<p>The host asked for changes to a %s on <strong>%s</strong>:</p><p>%s</p>", c.Type, html.EscapeString(b.EventTitle), html.EscapeString(note)))
	}
	return c, err
}

// DeleteCreative removes the host's own creative until it's in use.
func (s *BoostService) DeleteCreative(ctx context.Context, userID, boostID, creativeID uuid.UUID) error {
	b, h, err := s.hostBoost(ctx, userID, boostID, true)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(r *repository.BoostRepository) error {
		c, err := r.GetCreativeForUpdate(ctx, b.ID, creativeID)
		if err != nil {
			return err
		}
		if c == nil || c.UploadedByKind != "host" {
			return boostErr(http.StatusNotFound, "creative_not_found", "Creative not found.")
		}
		if c.Status == models.CreativeInUse {
			return boostErr(http.StatusConflict, "in_use", "This creative is running in the ads, so it can't be deleted.")
		}
		if err := r.DeleteCreative(ctx, c.ID); err != nil {
			return err
		}
		return r.Log(ctx, b.ID, "host", h.ID.String(), "creative_deleted", map[string]any{"creative_id": c.ID})
	})
}

// AdminCreativeStatus sets a creative In use, Needs changes or Not used.
func (s *BoostService) AdminCreativeStatus(ctx context.Context, admin string, boostID, creativeID uuid.UUID, status, note string) (*models.BoostCreative, error) {
	n := trimmedPtr(note)
	if tooLong(note, boostNoteMax) {
		return nil, boostErr(http.StatusBadRequest, "too_long", "Keep the note under 1,000 characters.")
	}
	b, err := s.adminBoost(ctx, boostID)
	if err != nil {
		return nil, err
	}
	check := func(c *models.BoostCreative) error {
		switch {
		case c.UploadedByKind == "host" && (status == models.CreativeInUse || status == models.CreativeNeedsChanges || status == models.CreativeNotUsed):
			if status != models.CreativeInUse && n == nil {
				return boostErr(http.StatusBadRequest, "note_required", "Tell the host why.")
			}
			return nil
		case c.UploadedByKind == "admin" && status == models.CreativeInUse && c.Status == models.CreativeApproved:
			return nil
		case c.UploadedByKind == "admin" && status == models.CreativeInUse:
			return boostErr(http.StatusConflict, "not_approved", "The host has to approve the team's creative first.")
		}
		return boostErr(http.StatusBadRequest, "invalid_status", "Set in_use, needs_changes or not_used (the last two on the host's creatives).")
	}
	c, err := s.creativeMove(ctx, b.ID, creativeID, "admin", admin, status, n, check)
	if err == nil && status == models.CreativeNeedsChanges {
		s.mailHost(ctx, b, "A creative needs changes", fmt.Sprintf("<p>The team asked for changes to your %s for <strong>%s</strong>:</p><p>%s</p>",
			c.Type, html.EscapeString(b.EventTitle), html.EscapeString(note)))
	}
	return c, err
}

/* ------------------------------------------------------------------ */
/*  Reports                                                            */
/* ------------------------------------------------------------------ */

// ReportInput attaches or edits a report.
type ReportInput struct {
	Kind        string  `json:"kind"`
	Title       string  `json:"title"`
	CoversFrom  *string `json:"covers_from"`
	CoversUntil *string `json:"covers_until"`
	Note        *string `json:"note"`
	FileKey     string  `json:"file_key"`
}

func (s *BoostService) AdminReportUploadURL(ctx context.Context, boostID uuid.UUID, req UploadRequest) (*UploadURL, error) {
	b, err := s.adminBoost(ctx, boostID)
	if err != nil {
		return nil, err
	}
	return s.signUpload(ctx, reportFolder(b.ID), req, true)
}

func checkReportFields(kind, title string, from, until *string) error {
	if kind != "update" && kind != "final" {
		return boostErr(http.StatusBadRequest, "invalid_kind", "kind is update or final.")
	}
	if title == "" || tooLong(title, boostTitleMax) {
		return boostErr(http.StatusBadRequest, "title_required", "Give it a title (up to 200 characters).")
	}
	for _, d := range []*string{from, until} {
		if d != nil && *d != "" {
			if _, ok := parseDay(*d); !ok {
				return boostErr(http.StatusBadRequest, "invalid_day", "Dates are YYYY-MM-DD.")
			}
		}
	}
	if from != nil && until != nil && *from != "" && *until != "" && *until < *from {
		return boostErr(http.StatusBadRequest, "invalid_window", "The end date is before the start.")
	}
	return nil
}

func (s *BoostService) checkReportFile(ctx context.Context, f *models.StoredFile) error {
	st, err := s.repo.Settings(ctx)
	if err != nil {
		return err
	}
	if !reportMimes[f.MimeType] || f.Size > int64(st.ReportMaxMB)*mb {
		return boostErr(http.StatusBadRequest, "invalid_file_type", fmt.Sprintf("Reports are CSV, XLSX, PDF, PNG or JPG, up to %d MB.", st.ReportMaxMB))
	}
	return nil
}

func blankNil(p *string) *string {
	if p == nil {
		return nil
	}
	return trimmedPtr(*p)
}

func (s *BoostService) AdminAddReport(ctx context.Context, admin string, boostID uuid.UUID, in ReportInput) (*models.BoostReport, error) {
	if s.files == nil {
		return nil, errPlanOff
	}
	b, err := s.adminBoost(ctx, boostID)
	if err != nil {
		return nil, err
	}
	if b.Status != models.BoostLive && b.Status != models.BoostEnded {
		return nil, boostErr(http.StatusConflict, "invalid_state", "Reports can be added once the Boost is live.")
	}
	in.Title = strings.TrimSpace(in.Title)
	in.CoversFrom, in.CoversUntil, in.Note = blankNil(in.CoversFrom), blankNil(in.CoversUntil), blankNil(in.Note)
	if err := checkReportFields(in.Kind, in.Title, in.CoversFrom, in.CoversUntil); err != nil {
		return nil, err
	}
	if in.Note != nil && tooLong(*in.Note, boostNoteMax) {
		return nil, boostErr(http.StatusBadRequest, "too_long", "Keep the note under 1,000 characters.")
	}
	f, err := s.storedFile(ctx, reportFolder(b.ID), in.FileKey)
	if err != nil {
		return nil, err
	}
	if err := s.checkReportFile(ctx, f); err != nil {
		return nil, err
	}
	x := &models.BoostReport{ID: uuid.New(), BoostID: b.ID, Kind: in.Kind, Title: in.Title, CoversFrom: in.CoversFrom,
		CoversUntil: in.CoversUntil, Note: in.Note, File: *f, UploadedBy: admin, CreatedAt: time.Now()}
	if err := s.repo.CreateReport(ctx, x); err != nil {
		return nil, err
	}
	_ = s.repo.Log(ctx, b.ID, "admin", admin, "report_added", map[string]any{"report_id": x.ID, "kind": x.Kind, "title": x.Title})
	s.mailHost(ctx, b, "New report on your Boost: "+x.Title, fmt.Sprintf("<p>The team added a report to your Boost on <strong>%s</strong>: %s.</p>",
		html.EscapeString(b.EventTitle), html.EscapeString(x.Title)))
	x.File.URL = s.files.SignBoostDownload(x.File.Key, x.File.Name)
	return x, nil
}

// AdminReplaceReport replaces a report's file or details. The old row stays
// in the history (replaced_at set) and is no longer shown to the host.
func (s *BoostService) AdminReplaceReport(ctx context.Context, admin string, boostID, reportID uuid.UUID, in ReportInput) (*models.BoostReport, error) {
	if s.files == nil {
		return nil, errPlanOff
	}
	var f *models.StoredFile
	if in.FileKey != "" {
		var err error
		if f, err = s.storedFile(ctx, reportFolder(boostID), in.FileKey); err != nil {
			return nil, err
		}
		if err := s.checkReportFile(ctx, f); err != nil {
			return nil, err
		}
	}
	var x *models.BoostReport
	err := s.inTx(ctx, func(r *repository.BoostRepository) error {
		old, err := r.GetReportForUpdate(ctx, boostID, reportID)
		if err != nil {
			return err
		}
		if old == nil || old.ReplacedAt != nil {
			return boostErr(http.StatusNotFound, "report_not_found", "Report not found (or already replaced).")
		}
		x = &models.BoostReport{ID: uuid.New(), BoostID: boostID, Kind: old.Kind, Title: old.Title, CoversFrom: old.CoversFrom,
			CoversUntil: old.CoversUntil, Note: old.Note, File: old.File, UploadedBy: admin, ReplacesID: &old.ID, CreatedAt: time.Now()}
		if in.Kind != "" {
			x.Kind = in.Kind
		}
		if t := strings.TrimSpace(in.Title); t != "" {
			x.Title = t
		}
		if in.CoversFrom != nil {
			x.CoversFrom = blankNil(in.CoversFrom)
		}
		if in.CoversUntil != nil {
			x.CoversUntil = blankNil(in.CoversUntil)
		}
		if in.Note != nil {
			x.Note = blankNil(in.Note)
			if x.Note != nil && tooLong(*x.Note, boostNoteMax) {
				return boostErr(http.StatusBadRequest, "too_long", "Keep the note under 1,000 characters.")
			}
		}
		if f != nil {
			x.File = *f
		}
		if err := checkReportFields(x.Kind, x.Title, x.CoversFrom, x.CoversUntil); err != nil {
			return err
		}
		if err := r.MarkReportReplaced(ctx, old.ID); err != nil {
			return err
		}
		if err := r.CreateReport(ctx, x); err != nil {
			return err
		}
		return r.Log(ctx, boostID, "admin", admin, "report_replaced", map[string]any{"report_id": x.ID, "replaces": old.ID})
	})
	if err != nil {
		return nil, err
	}
	x.File.URL = s.files.SignBoostDownload(x.File.Key, x.File.Name)
	return x, nil
}

func (s *BoostService) listReports(ctx context.Context, boostID uuid.UUID, history bool) ([]*models.BoostReport, error) {
	list, err := s.repo.ListReports(ctx, boostID, history)
	for _, x := range list {
		if s.files != nil {
			x.File.URL = s.files.SignBoostDownload(x.File.Key, x.File.Name)
		}
		if !history {
			x.UploadedBy = ""
		}
	}
	return list, err
}

func (s *BoostService) Reports(ctx context.Context, userID, boostID uuid.UUID) ([]*models.BoostReport, error) {
	if _, _, err := s.hostBoost(ctx, userID, boostID, false); err != nil {
		return nil, err
	}
	return s.listReports(ctx, boostID, false)
}

func (s *BoostService) AdminReports(ctx context.Context, boostID uuid.UUID) ([]*models.BoostReport, error) {
	if _, err := s.adminBoost(ctx, boostID); err != nil {
		return nil, err
	}
	return s.listReports(ctx, boostID, true)
}

/* ------------------------------------------------------------------ */
/*  Premium Hosts                                                      */
/* ------------------------------------------------------------------ */

func noPremium(hostID uuid.UUID) *models.PremiumMembership {
	return &models.PremiumMembership{HostID: hostID, Status: models.PremiumNone}
}

// HostPremium is the signed-in host's Premium status ("none" if never applied).
func (s *BoostService) HostPremium(ctx context.Context, userID uuid.UUID) (*models.PremiumMembership, error) {
	h, err := s.hostOf(ctx, userID)
	if err != nil {
		return nil, err
	}
	m, err := s.repo.Premium(ctx, h.ID)
	if err != nil || m != nil {
		return m, err
	}
	return noPremium(h.ID), nil
}

// ApplyPremium records the host's application. Approved hosts only; a
// declined host waits PremiumReapplyDays.
func (s *BoostService) ApplyPremium(ctx context.Context, userID uuid.UUID, note string) (*models.PremiumMembership, error) {
	note = strings.TrimSpace(note)
	if tooLong(note, boostNoteMax) {
		return nil, boostErr(http.StatusBadRequest, "too_long", "Keep the note under 1,000 characters.")
	}
	h, err := s.hostOf(ctx, userID)
	if err != nil {
		return nil, err
	}
	acc, err := s.repo.HostAccess(ctx, h.ID)
	if err != nil {
		return nil, err
	}
	if acc == nil || !acc.Approved {
		return nil, boostErr(http.StatusForbidden, "host_not_approved", "Your host profile must be approved before you apply.")
	}
	err = s.inTx(ctx, func(r *repository.BoostRepository) error {
		m, err := r.PremiumForUpdate(ctx, h.ID)
		if err != nil {
			return err
		}
		if m == nil {
			m = noPremium(h.ID)
		}
		switch m.Status {
		case models.PremiumApplied:
			return boostErr(http.StatusConflict, "already_applied", "Your application is with the team.")
		case models.PremiumActive:
			return boostErr(http.StatusConflict, "already_premium", "You're already a Premium host.")
		case models.PremiumDeclined:
			if m.DecidedAt != nil && time.Since(*m.DecidedAt) < models.PremiumReapplyDays*24*time.Hour {
				again := m.DecidedAt.Add(models.PremiumReapplyDays * 24 * time.Hour).In(istZone).Format("2 Jan 2006")
				return boostErr(http.StatusConflict, "too_soon", "You can apply again from "+again+".")
			}
		}
		now := time.Now()
		m.Status, m.AppliedAt, m.ApplicationNote = models.PremiumApplied, &now, trimmedPtr(note)
		m.DecidedAt, m.DecidedBy, m.DeclineReason, m.RemoveReason, m.Granted = nil, nil, nil, nil, false
		if err := r.SavePremium(ctx, m); err != nil {
			return err
		}
		return r.PremiumLog(ctx, h.ID, "host", h.ID.String(), "applied", map[string]any{"note": note})
	})
	if err != nil {
		return nil, err
	}
	s.mailAdmin("Premium application: "+strings.TrimSpace(h.FirstName+" "+h.LastName),
		fmt.Sprintf("<p><strong>%s</strong> applied to Premium Hosts.</p><p>%s</p><p>Review it in the admin under Premium.</p>",
			html.EscapeString(strings.TrimSpace(h.FirstName+" "+h.LastName)), html.EscapeString(note)))
	s.mailHostID(ctx, h.ID, "We got your Premium application", "<p>Thanks for applying to Premium Hosts. The MySlotMate team will review it and get back to you.</p>")
	return s.repo.Premium(ctx, h.ID)
}

// PremiumDecision is the team's input on an application or membership.
type PremiumDecision struct {
	ManagerName    string `json:"manager_name"`
	ManagerContact string `json:"manager_contact"`
	Reason         string `json:"reason"`
}

// premiumMove changes a membership. from lists the statuses it may move from
// ("none" = no row yet).
func (s *BoostService) premiumMove(ctx context.Context, admin string, hostID uuid.UUID, action, to string, from []string,
	d PremiumDecision) (*models.PremiumMembership, error) {
	if tooLong(d.ManagerName, 200) || tooLong(d.ManagerContact, 200) || tooLong(d.Reason, boostNoteMax) {
		return nil, boostErr(http.StatusBadRequest, "too_long", "That's too long.")
	}
	h, err := s.hostRepo.GetByID(ctx, hostID)
	if err != nil {
		return nil, err
	}
	if h == nil {
		return nil, boostErr(http.StatusNotFound, "host_not_found", "Host not found.")
	}
	err = s.inTx(ctx, func(r *repository.BoostRepository) error {
		m, err := r.PremiumForUpdate(ctx, hostID)
		if err != nil {
			return err
		}
		if m == nil {
			m = noPremium(hostID)
		}
		ok := false
		for _, f := range from {
			ok = ok || f == m.Status
		}
		if !ok {
			return boostErr(http.StatusConflict, "invalid_state", fmt.Sprintf("Can't %s a membership that is %s.", action, m.Status))
		}
		now := time.Now()
		m.Status, m.DecidedAt, m.DecidedBy = to, &now, &admin
		switch to {
		case models.PremiumActive:
			m.Granted = action == "grant"
			m.DeclineReason, m.RemoveReason = nil, nil
			if v := trimmedPtr(d.ManagerName); v != nil {
				m.ManagerName = v
			}
			if v := trimmedPtr(d.ManagerContact); v != nil {
				m.ManagerContact = v
			}
		case models.PremiumDeclined:
			m.DeclineReason = trimmedPtr(d.Reason)
		case models.PremiumRemoved:
			m.RemoveReason = trimmedPtr(d.Reason)
		}
		if err := r.SavePremium(ctx, m); err != nil {
			return err
		}
		return r.PremiumLog(ctx, hostID, "admin", admin, action, d)
	})
	if err != nil {
		return nil, err
	}
	return s.repo.Premium(ctx, hostID)
}

func (s *BoostService) PremiumApprove(ctx context.Context, admin string, hostID uuid.UUID, d PremiumDecision) (*models.PremiumMembership, error) {
	if strings.TrimSpace(d.ManagerName) == "" {
		return nil, boostErr(http.StatusBadRequest, "manager_required", "Name the campaign manager.")
	}
	m, err := s.premiumMove(ctx, admin, hostID, "approve", models.PremiumActive, []string{models.PremiumApplied}, d)
	if err == nil {
		s.mailPremiumActive(ctx, m)
	}
	return m, err
}

func (s *BoostService) PremiumGrant(ctx context.Context, admin string, hostID uuid.UUID, d PremiumDecision) (*models.PremiumMembership, error) {
	m, err := s.premiumMove(ctx, admin, hostID, "grant", models.PremiumActive,
		[]string{models.PremiumNone, models.PremiumApplied, models.PremiumDeclined, models.PremiumRemoved}, d)
	if err == nil {
		s.mailPremiumActive(ctx, m)
	}
	return m, err
}

func (s *BoostService) PremiumDecline(ctx context.Context, admin string, hostID uuid.UUID, d PremiumDecision) (*models.PremiumMembership, error) {
	if strings.TrimSpace(d.Reason) == "" {
		return nil, boostErr(http.StatusBadRequest, "reason_required", "Tell the host why.")
	}
	m, err := s.premiumMove(ctx, admin, hostID, "decline", models.PremiumDeclined, []string{models.PremiumApplied}, d)
	if err == nil {
		s.mailHostID(ctx, hostID, "About your Premium application", fmt.Sprintf(
			"<p>We can't add you to Premium Hosts right now.</p><p>%s</p><p>You can apply again in %d days.</p>", html.EscapeString(d.Reason), models.PremiumReapplyDays))
	}
	return m, err
}

// PremiumRemove ends a membership: Boost locks again; a live Boost runs to its end.
func (s *BoostService) PremiumRemove(ctx context.Context, admin string, hostID uuid.UUID, d PremiumDecision) (*models.PremiumMembership, error) {
	if strings.TrimSpace(d.Reason) == "" {
		return nil, boostErr(http.StatusBadRequest, "reason_required", "Tell the host why.")
	}
	m, err := s.premiumMove(ctx, admin, hostID, "remove", models.PremiumRemoved, []string{models.PremiumActive}, d)
	if err == nil {
		s.mailHostID(ctx, hostID, "Your Premium membership has ended", fmt.Sprintf(
			"<p>Your Premium Hosts membership has ended.</p><p>%s</p><p>A Boost that is live now runs to its end date; new Boosts need Premium again.</p>", html.EscapeString(d.Reason)))
	}
	return m, err
}

func (s *BoostService) mailPremiumActive(ctx context.Context, m *models.PremiumMembership) {
	if m == nil {
		return
	}
	mgr := ""
	if m.ManagerName != nil {
		mgr = fmt.Sprintf(" Your campaign manager is <strong>%s</strong>", html.EscapeString(*m.ManagerName))
		if m.ManagerContact != nil {
			mgr += " (" + html.EscapeString(*m.ManagerContact) + ")"
		}
		mgr += "."
	}
	s.mailHostID(ctx, m.HostID, "Welcome to Premium Hosts",
		"<p>You're now a Premium host: Boost is unlocked for your events, with free creatives, same-day review and featured placement."+mgr+"</p>")
}

// PremiumList is the admin's list with counts per status.
func (s *BoostService) PremiumList(ctx context.Context, status string) ([]*models.PremiumMembership, map[string]int, error) {
	list, err := s.repo.ListPremium(ctx, status)
	if err != nil {
		return nil, nil, err
	}
	counts, err := s.repo.CountPremium(ctx)
	return list, counts, err
}

// PremiumDetail is one host's membership and its history.
type PremiumDetail struct {
	Membership *models.PremiumMembership `json:"membership"`
	Log        []models.BoostLogEntry    `json:"log"`
}

func (s *BoostService) PremiumGet(ctx context.Context, hostID uuid.UUID) (*PremiumDetail, error) {
	m, err := s.repo.Premium(ctx, hostID)
	if err != nil {
		return nil, err
	}
	if m == nil {
		m = noPremium(hostID)
	}
	lg, err := s.repo.PremiumLogEntries(ctx, hostID)
	return &PremiumDetail{Membership: m, Log: lg}, err
}

/* ------------------------------------------------------------------ */
/*  Bands and settings                                                 */
/* ------------------------------------------------------------------ */

func (s *BoostService) AllBands(ctx context.Context) ([]models.BoostBand, error) {
	b, err := s.repo.AllBands(ctx)
	if b == nil {
		b = []models.BoostBand{}
	}
	return b, err
}

// checkBands: cheapest first from ₹0, no gaps, the last open-ended, rates in range.
func checkBands(bands []models.BoostBand) error {
	if len(bands) == 0 || len(bands) > 20 {
		return errors.New("send 1 to 20 bands")
	}
	if bands[0].MinPriceCents != 0 {
		return errors.New("the first band starts at ₹0")
	}
	for i, b := range bands {
		if b.MinRateBps < 0 || b.MaxRateBps > 10000 || b.MinRateBps > b.GuideRateBps || b.GuideRateBps > b.MaxRateBps {
			return fmt.Errorf("band %d: rates must be 0–100%% with min ≤ guide ≤ max", i+1)
		}
		last := i == len(bands)-1
		if last != (b.MaxPriceCents == nil) {
			return errors.New("only the last band is open-ended")
		}
		if !last {
			if *b.MaxPriceCents < b.MinPriceCents || bands[i+1].MinPriceCents != *b.MaxPriceCents+1 {
				return fmt.Errorf("band %d: the next band must start right after it", i+1)
			}
		}
	}
	return nil
}

// PublishBands stores a new band version for budget Boosts. Proposals made
// from now on use it; Boosts already priced keep theirs.
func (s *BoostService) PublishBands(ctx context.Context, admin string, bands []models.BoostBand) (int, error) {
	if err := checkBands(bands); err != nil {
		return 0, boostErr(http.StatusBadRequest, "invalid_bands", strings.ToUpper(err.Error()[:1])+err.Error()[1:]+".")
	}
	var v int
	err := s.inTx(ctx, func(r *repository.BoostRepository) error {
		var err error
		v, err = r.InsertBandVersion(ctx, bands)
		return err
	})
	if err == nil {
		log.Printf("[BOOST] %s published band version %d", admin, v)
	}
	return v, err
}

func (s *BoostService) Settings(ctx context.Context) (*models.BoostSettings, error) {
	return s.repo.Settings(ctx)
}

func (s *BoostService) SaveSettings(ctx context.Context, in models.BoostSettings) (*models.BoostSettings, error) {
	if in.MinDailyBudgetCents <= 0 || in.BudgetStepCents <= 0 || in.MinDays <= 0 || in.MinDays > 365 ||
		in.PaymentWindowDays <= 0 || in.PaymentWindowDays > 60 || in.BudgetGSTBps < 0 || in.BudgetGSTBps > 2800 {
		return nil, boostErr(http.StatusBadRequest, "invalid_settings", "Budget minimums, step, days and the payment window must be positive; GST 0–28%.")
	}
	for _, v := range []int{in.CreativeImageMaxMB, in.CreativeVideoMaxMB, in.CreativeOtherMaxMB, in.ReportMaxMB} {
		if v < 1 || v > 1000 {
			return nil, boostErr(http.StatusBadRequest, "invalid_settings", "Upload limits are 1 to 1,000 MB.")
		}
	}
	if err := s.repo.SaveSettings(ctx, &in); err != nil {
		return nil, err
	}
	return s.repo.Settings(ctx)
}

/* ------------------------------------------------------------------ */
/*  Reminders and the weekly summary                                   */
/* ------------------------------------------------------------------ */

// IsPremium reports whether the host is an Active Premium host (the public
// profile's badge). Errors read as false.
func (s *BoostService) IsPremium(ctx context.Context, hostID uuid.UUID) bool {
	ok, err := s.repo.PremiumActive(ctx, hostID)
	return err == nil && ok
}

// sendReminders sends each reminder once: payment 2 and 6 days after
// accepting, creatives 2 days before the planned start, and a weekly summary
// while live. The log entry is written first, so a reminder is never sent
// twice (at worst a failed email is skipped).
func (s *BoostService) sendReminders(ctx context.Context, now time.Time) {
	c, err := s.repo.ReminderCandidates(ctx, now)
	if err != nil {
		log.Printf("[BOOST] reminders: %v", err)
		return
	}
	once := func(id uuid.UUID, action string) *models.Boost {
		b, err := s.repo.GetByID(ctx, id)
		if err != nil || b == nil {
			return nil
		}
		if ok, err := s.repo.LogOnce(ctx, id, action); err != nil || !ok {
			return nil
		}
		return b
	}
	for _, id := range c.Unpaid {
		b, err := s.repo.GetByID(ctx, id)
		if err != nil || b == nil || b.AcceptedAt == nil {
			continue
		}
		key := "reminder_payment_2d"
		if now.Sub(*b.AcceptedAt) >= 6*24*time.Hour {
			key = "reminder_payment_6d"
		}
		if b = once(id, key); b != nil {
			due := ""
			if b.PaymentDueBy != nil {
				due = " The plan expires on " + b.PaymentDueBy.In(istZone).Format("2 Jan 2006") + "."
			}
			s.mailHost(ctx, b, "Reminder: pay your Boost budget", fmt.Sprintf(
				"<p>The ad budget for your Boost on <strong>%s</strong> (%s incl. GST) isn't paid yet.%s Pay it from the event page to go live.</p>",
				html.EscapeString(b.EventTitle), rupeesText(b.BudgetTotalCents+b.BudgetGSTCents), due))
		}
	}
	for _, id := range c.NoCreatives {
		if b := once(id, "reminder_no_creatives"); b != nil {
			s.mailHost(ctx, b, "Upload your Boost creatives", fmt.Sprintf(
				"<p>Your Boost on <strong>%s</strong> starts soon and has no creatives yet. Upload the reels, posts or stories to run from the event page, or tell the team to make them.</p>",
				html.EscapeString(b.EventTitle)))
		}
	}
	y, w := now.In(istZone).ISOWeek()
	for _, id := range c.Live {
		if b := once(id, fmt.Sprintf("weekly_summary_%d_%02d", y, w)); b != nil {
			s.mailWeeklySummary(ctx, b)
		}
	}
}

// weeklySummary is the text of a live Boost's weekly email.
func weeklySummary(b *models.Boost, res *Results) string {
	cpt := "–"
	if res.Totals.TicketsSold > 0 {
		cpt = rupeesText(res.Totals.SpendCents / int64(res.Totals.TicketsSold))
	}
	return fmt.Sprintf(`<p>Your Boost on <strong>%s</strong> so far:</p><ul>
<li>Spent: %s of %s</li><li>Reach: %d · Clicks: %d</li><li>Tickets sold: %d</li><li>Ad cost per ticket: %s</li></ul>`,
		html.EscapeString(b.EventTitle), rupeesText(res.Totals.SpendCents), rupeesText(b.PaidCents),
		res.Totals.Reach, res.Totals.Clicks, res.Totals.TicketsSold, cpt)
}

func (s *BoostService) mailWeeklySummary(ctx context.Context, b *models.Boost) {
	res, err := s.results(ctx, b.ID)
	if err != nil {
		log.Printf("[BOOST] weekly summary %s: %v", b.ID, err)
		return
	}
	s.mailHost(ctx, b, "Your weekly Boost summary", weeklySummary(b, res))
}

/* ------------------------------------------------------------------ */
/*  Email to a host by id                                              */
/* ------------------------------------------------------------------ */

func (s *BoostService) mailHostID(ctx context.Context, hostID uuid.UUID, subject, body string) {
	if s.notify == nil {
		log.Printf("[BOOST] (no email) %s", subject)
		return
	}
	h, err := s.hostRepo.GetByID(ctx, hostID)
	if err != nil || h == nil {
		return
	}
	u, err := s.userRepo.GetByID(ctx, h.UserID)
	if err != nil || u == nil || u.Email == "" {
		return
	}
	if s.siteURL != "" {
		body += fmt.Sprintf(`<p><a href="%s/host-dashboard/premium">Open Premium</a></p>`, s.siteURL)
	}
	go func() {
		if err := s.notify.BoostEmail(u.Email, subject+" · MySlotMate", "<html><body style=\"font-family: Arial, sans-serif; line-height: 1.6; color: #333;\">"+body+"</body></html>"); err != nil {
			log.Printf("[BOOST] host email %q: %v", subject, err)
		}
	}()
}

// AdminCreativesZip writes the Boost's creatives (or only creativeID's, when
// set) to w as a ZIP: one folder per creative, "01-post-host/<file>". It
// streams each file from storage, so a large reel is never held in memory.
func (s *BoostService) AdminCreativesZip(ctx context.Context, boostID uuid.UUID, creativeID *uuid.UUID, w io.Writer) error {
	if s.files == nil {
		return errPlanOff
	}
	list, err := s.repo.ListCreatives(ctx, boostID)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(w)
	n := 0
	for i := len(list) - 1; i >= 0; i-- { // oldest first, numbered in upload order
		c := list[i]
		n++
		if creativeID != nil && c.ID != *creativeID {
			continue
		}
		dir := fmt.Sprintf("%02d-%s-%s", n, c.Type, c.UploadedByKind)
		for j, f := range c.Files {
			body, err := s.files.OpenBoostObject(ctx, f.Key)
			if err != nil {
				return fmt.Errorf("open %s: %w", f.Key, err)
			}
			name := fmt.Sprintf("%s/%02d-%s", dir, j+1, safeName(f.Name))
			// Images and videos are already compressed: store, don't deflate.
			fw, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store, Modified: c.CreatedAt})
			if err == nil {
				_, err = io.Copy(fw, body)
			}
			body.Close()
			if err != nil {
				return err
			}
		}
	}
	return zw.Close()
}

// CreativesExist reports whether a Boost has any creative (to answer 404
// before a ZIP starts streaming).
func (s *BoostService) CreativesExist(ctx context.Context, boostID uuid.UUID) (bool, error) {
	list, err := s.repo.ListCreatives(ctx, boostID)
	return len(list) > 0, err
}
