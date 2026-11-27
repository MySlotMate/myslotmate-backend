package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html"
	"log"
	"net/http"
	"strings"
	"time"

	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"

	"github.com/google/uuid"
)

// Event Boost (roadmap task 21). A host requests a Boost for one event; the
// admin team proposes a commission per ticket type within the host-guide
// bands; the host accepts; an admin goes live. While live, each paid ticket
// booking on the event carries the Boost commission instead of any per-host
// commission (see boostCommissionFor). Refunds return it with the rest of the
// booking. Passes, walk-ins, imports and free tickets never carry it.

// Request guardrails (PRD §3.2).
const (
	BoostMinDaysAhead      = 10
	BoostMinUnsold         = 20
	BoostMinUnsoldUnder500 = 50
	boostCheapPriceCents   = 50000 // ₹500
	boostGoalMax           = 200
	boostAudienceMax       = 500
)

// BoostAllowsRefundPolicy reports whether an event with this refund policy
// may be boosted. The ads are paid for up front and a fully refunded booking
// returns its commission, so a boosted event needs a policy whose free-cancel
// window closes well before the event: Moderate (5 days), Strict or No
// refunds. Flexible (24 hours) and an unset policy (always refunded) are out.
func BoostAllowsRefundPolicy(p *models.CancellationPolicy) bool {
	if p == nil {
		return false
	}
	switch *p {
	case models.CancellationModerate, models.CancellationStrict, models.CancellationNoRefund:
		return true
	}
	return false
}

// BoostRefundPolicyProblem is what the host is told when the policy rules a
// Boost out.
const BoostRefundPolicyProblem = "Boosted events need a Moderate, Strict or No refunds policy. Change it in Refunds & payout first."

// BoostError carries the HTTP status and a stable code for the website.
type BoostError struct {
	Status  int
	Code    string
	Message string
}

func (e *BoostError) Error() string { return e.Code + ": " + e.Message }

func boostErr(status int, code, msg string) *BoostError {
	return &BoostError{Status: status, Code: code, Message: msg}
}

// BoostNotifier sends Boost emails. Nil-safe: a missing notifier only logs.
type BoostNotifier interface {
	BoostEmail(to, subject, htmlBody string) error
}

type BoostService struct {
	db        *sql.DB
	repo      *repository.BoostRepository
	eventRepo repository.EventRepository
	tierRepo  repository.EventPriceTierRepository
	hostRepo  repository.HostRepository
	userRepo  repository.UserRepository
	events    EventService
	notify    BoostNotifier
	adminTo   string // admin inbox for new requests
	siteURL   string
	// Paid-budget Boost (WithPlan); nil until wired.
	payments BoostPayments
	files    BoostFiles
	billing  *BillingService
}

func NewBoostService(db *sql.DB, er repository.EventRepository, tr repository.EventPriceTierRepository,
	hr repository.HostRepository, ur repository.UserRepository, es EventService, n BoostNotifier, adminTo, siteURL string) *BoostService {
	return &BoostService{db: db, repo: repository.NewBoostRepository(db), eventRepo: er, tierRepo: tr, hostRepo: hr,
		userRepo: ur, events: es, notify: n, adminTo: adminTo, siteURL: strings.TrimRight(siteURL, "/")}
}

/* ------------------------------------------------------------------ */
/*  Rates                                                              */
/* ------------------------------------------------------------------ */

// bandFor returns the band a ticket price falls in.
func bandFor(bands []models.BoostBand, priceCents int64) (models.BoostBand, bool) {
	for _, b := range bands {
		if b.Contains(priceCents) {
			return b, true
		}
	}
	return models.BoostBand{}, false
}

// BoostCommission is the commission on a ticket value at rateBps, half-up to
// the paisa.
func BoostCommission(ticketCents int64, rateBps int) int64 {
	if ticketCents <= 0 || rateBps <= 0 {
		return 0
	}
	return (ticketCents*int64(rateBps) + 5000) / 10000
}

// ticketTypes is what an event sells now: its active paid ticket types, or the
// single price.
func (s *BoostService) ticketTypes(ctx context.Context, evt *models.Event) ([]models.BoostRate, error) {
	tiers, err := s.tierRepo.ListByEventID(ctx, evt.ID)
	if err != nil {
		return nil, err
	}
	var out []models.BoostRate
	for _, t := range tiers {
		if t.IsActive && t.PriceCents > 0 {
			id, name := t.ID, t.Name
			out = append(out, models.BoostRate{TicketKey: models.BoostTicketKey(&name), PriceTierID: &id, TicketName: t.Name, PriceCents: t.PriceCents})
		}
	}
	if len(out) == 0 && evt.PriceCents != nil && *evt.PriceCents > 0 {
		out = append(out, models.BoostRate{TicketKey: "", TicketName: "Ticket", PriceCents: *evt.PriceCents})
	}
	return out, nil
}

// guideRates fills each ticket type's guide rate from the current bands.
func guideRates(types []models.BoostRate, bands []models.BoostBand) ([]models.BoostRate, error) {
	out := make([]models.BoostRate, len(types))
	for i, t := range types {
		b, ok := bandFor(bands, t.PriceCents)
		if !ok {
			return nil, fmt.Errorf("no Boost band for a ₹%d ticket", t.PriceCents/100)
		}
		t.GuideRateBps, t.RateBps, t.BandVersion, t.Reason = b.GuideRateBps, b.GuideRateBps, b.Version, nil
		out[i] = t
	}
	return out, nil
}

// pricesUnchanged reports whether every quoted ticket type still sells at its
// quoted price and nothing new was added (else the quote must be redone).
func pricesUnchanged(quoted, now []models.BoostRate) bool {
	if len(quoted) != len(now) {
		return false
	}
	byKey := map[string]int64{}
	for _, q := range quoted {
		byKey[q.TicketKey] = q.PriceCents
	}
	for _, n := range now {
		p, ok := byKey[n.TicketKey]
		if !ok || p != n.PriceCents {
			return false
		}
	}
	return true
}

/* ------------------------------------------------------------------ */
/*  Host side                                                          */
/* ------------------------------------------------------------------ */

// hostOf resolves the signed-in user's host.
func (s *BoostService) hostOf(ctx context.Context, userID uuid.UUID) (*models.Host, error) {
	h, err := s.hostRepo.GetByUserID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if h == nil {
		return nil, boostErr(http.StatusForbidden, "not_a_host", "Only hosts can boost an event.")
	}
	return h, nil
}

// eventFor loads the event and checks the host may read it (owner or co-host)
// or, when write is set, act on it (owner only).
func (s *BoostService) eventFor(ctx context.Context, h *models.Host, eventID uuid.UUID, write bool) (*models.Event, error) {
	evt, err := s.eventRepo.GetByID(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if evt == nil {
		return nil, boostErr(http.StatusNotFound, "event_not_found", "Event not found.")
	}
	if evt.HostID == h.ID {
		return evt, nil
	}
	if !write {
		if ok, err := s.eventRepo.HostCanManage(ctx, eventID, h.ID); err == nil && ok {
			return evt, nil
		}
	}
	return nil, boostErr(http.StatusForbidden, "not_event_owner", "Only the event's host can do this.")
}

// EventBoost is the event's newest Boost (nil when it has never had one).
func (s *BoostService) EventBoost(ctx context.Context, userID, eventID uuid.UUID) (*models.Boost, error) {
	h, err := s.hostOf(ctx, userID)
	if err != nil {
		return nil, err
	}
	if _, err := s.eventFor(ctx, h, eventID, false); err != nil {
		return nil, err
	}
	return s.repo.LatestForEvent(ctx, eventID)
}

// HostBoosts lists every Boost of the signed-in host, newest first.
func (s *BoostService) HostBoosts(ctx context.Context, userID uuid.UUID) ([]*models.Boost, error) {
	h, err := s.hostOf(ctx, userID)
	if err != nil {
		return nil, err
	}
	out, err := s.repo.ListByHost(ctx, h.ID)
	if out == nil {
		out = []*models.Boost{}
	}
	return out, err
}

// BoostRequest is what the host sends.
type BoostRequest struct {
	GoalText      string   `json:"goal_text"`
	AudienceNotes string   `json:"audience_notes"`
	Channels      []string `json:"channels"`
	// The budget plan. With it the Boost is a paid-budget one; without it a
	// no_upfront one (refused once boost_settings.budget_required is on).
	DailyBudgetCents *int64 `json:"daily_budget_cents"`
	BudgetFrom       string `json:"budget_from"`  // YYYY-MM-DD
	BudgetUntil      string `json:"budget_until"` // YYYY-MM-DD, included
}

// eligibility returns the first reason the event can't be boosted, or "".
// byAdmin keeps only what a Boost can't run without (paid tickets, a
// published event, the refund policy, an upcoming date) and drops the rest:
// the BoostMinDaysAhead lead time, the unsold-ticket minimum, host approval
// and a payout method are the team's call. Commission is taken from each
// booking, so a host without a payout method still owes it; their earnings
// simply wait until they add one. Host requests keep every rule.
func (s *BoostService) eligibility(ctx context.Context, h *models.Host, evt *models.Event, types []models.BoostRate, now time.Time, byAdmin bool) (string, error) {
	if evt.IsFree || len(types) == 0 {
		return "Free events can't be boosted.", nil
	}
	if evt.Status != models.EventStatusLive {
		return "Publish the event first.", nil
	}
	if !BoostAllowsRefundPolicy(evt.CancellationPolicy) {
		return BoostRefundPolicyProblem, nil
	}
	avail, err := s.events.GetEventAvailability(ctx, evt.ID)
	if err != nil {
		return "", err
	}
	var next *time.Time
	unsold := 0
	for i := range avail {
		a := avail[i]
		if a.Date.After(now) && !a.IsPaused {
			if next == nil || a.Date.Before(*next) {
				d := a.Date
				next = &d
			}
			unsold += a.Remaining
		}
	}
	if next == nil {
		return "This event has no upcoming dates.", nil
	}
	if byAdmin {
		return "", nil
	}
	if next.Sub(now) < BoostMinDaysAhead*24*time.Hour {
		return fmt.Sprintf("The next date is less than %d days away.", BoostMinDaysAhead), nil
	}
	allCheap := true
	for _, t := range types {
		if t.PriceCents >= boostCheapPriceCents {
			allCheap = false
		}
	}
	need := BoostMinUnsold
	if allCheap {
		need = BoostMinUnsoldUnder500
	}
	if unsold < need {
		return fmt.Sprintf("A Boost needs at least %d unsold tickets (%d left).", need, unsold), nil
	}
	acc, err := s.repo.HostAccess(ctx, h.ID)
	if err != nil {
		return "", err
	}
	if acc == nil || !acc.Approved {
		return "Your host profile must be approved first.", nil
	}
	if !acc.HasPayoutMethod {
		return "Add a bank account or UPI for payouts first.", nil
	}
	return "", nil
}

// Request creates a Boost request with the guide rates as a preview.
func (s *BoostService) Request(ctx context.Context, userID, eventID uuid.UUID, req BoostRequest) (*models.Boost, error) {
	h, err := s.hostOf(ctx, userID)
	if err != nil {
		return nil, err
	}
	evt, err := s.eventFor(ctx, h, eventID, true)
	if err != nil {
		return nil, err
	}
	if err := s.requirePremium(ctx, s.repo, h.ID); err != nil {
		return nil, err
	}
	b, err := s.create(ctx, h, evt, req, "host", h.ID.String(), "requested")
	if err != nil {
		return nil, err
	}
	s.mailAdmin(fmt.Sprintf("New Boost request: %s", evt.Title),
		fmt.Sprintf("<p><strong>%s</strong> asked to boost <strong>%s</strong>.</p><p>Review it in the admin under Boosts.</p>",
			html.EscapeString(strings.TrimSpace(h.FirstName+" "+h.LastName)), html.EscapeString(evt.Title)))
	return b, nil
}

// AdminStart opens a Boost on any host's event from the admin, as if the host
// had asked: it lands in the queue as a new request, the admin sends the
// proposal, and nothing is charged until the host accepts it and the Boost
// goes live. The pilot invite is not needed; every other request check is.
func (s *BoostService) AdminStart(ctx context.Context, admin string, eventID uuid.UUID, req BoostRequest) (*models.Boost, error) {
	evt, err := s.eventRepo.GetByID(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if evt == nil {
		return nil, boostErr(http.StatusNotFound, "event_not_found", "Event not found.")
	}
	h, err := s.hostRepo.GetByID(ctx, evt.HostID)
	if err != nil {
		return nil, err
	}
	if h == nil {
		return nil, boostErr(http.StatusUnprocessableEntity, "event_not_eligible", "This event has no host.")
	}
	if err := s.requirePremium(ctx, s.repo, h.ID); err != nil {
		return nil, err
	}
	return s.create(ctx, h, evt, req, "admin", admin, "started_by_admin")
}

// create validates a Boost request for the event and stores it as a new
// request with the guide rates as a preview.
func (s *BoostService) create(ctx context.Context, h *models.Host, evt *models.Event, req BoostRequest, actorKind, actor, action string) (*models.Boost, error) {
	seen := map[string]bool{}
	channels := []string{}
	for _, c := range req.Channels {
		c = strings.ToLower(strings.TrimSpace(c))
		if !models.BoostChannels[c] {
			return nil, boostErr(http.StatusBadRequest, "invalid_channels", "Pick where to promote it: Instagram, Facebook, Google, WhatsApp or influencers.")
		}
		if !seen[c] {
			seen[c] = true
			channels = append(channels, c)
		}
	}
	if len(channels) == 0 {
		return nil, boostErr(http.StatusBadRequest, "invalid_channels", "Pick at least one place to promote it.")
	}
	goal, audience := strings.TrimSpace(req.GoalText), strings.TrimSpace(req.AudienceNotes)
	if len([]rune(goal)) > boostGoalMax || len([]rune(audience)) > boostAudienceMax {
		return nil, boostErr(http.StatusBadRequest, "too_long", fmt.Sprintf("Keep the goal under %d characters and the audience notes under %d.", boostGoalMax, boostAudienceMax))
	}

	types, err := s.ticketTypes(ctx, evt)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if reason, err := s.eligibility(ctx, h, evt, types, now, actorKind == "admin"); err != nil {
		return nil, err
	} else if reason != "" {
		return nil, boostErr(http.StatusUnprocessableEntity, "event_not_eligible", reason)
	}
	b := &models.Boost{ID: uuid.New(), EventID: evt.ID, HostID: h.ID, Status: models.BoostUnderReview,
		Channels: channels, RequestedAt: now, PricingModel: models.BoostPricingNoUpfront, BudgetStatus: models.BudgetNotRequired}
	st, err := s.repo.Settings(ctx)
	if err != nil {
		return nil, err
	}
	if req.DailyBudgetCents != nil || req.BudgetFrom != "" || req.BudgetUntil != "" {
		est, err := s.estimate(ctx, st, evt, req.DailyBudgetCents, req.BudgetFrom, req.BudgetUntil, now)
		if err != nil {
			return nil, err
		}
		if len(est.Problems) > 0 {
			return nil, boostErr(http.StatusBadRequest, "invalid_budget", strings.Join(est.Problems, " "))
		}
		b.PricingModel, b.BudgetStatus = models.BoostPricingBudget, models.BudgetUnpaid
		b.DailyBudgetCents, b.BudgetFrom, b.BudgetUntil = req.DailyBudgetCents, &req.BudgetFrom, &req.BudgetUntil
		b.BudgetTotalCents, b.BudgetGSTCents = est.BudgetTotalCents, est.BudgetGSTCents
	} else if st.BudgetRequired {
		return nil, boostErr(http.StatusBadRequest, "budget_required", "Plan the ad budget: a daily amount and the dates to run it.")
	}
	bands, err := s.repo.Bands(ctx, b.PricingModel)
	if err != nil {
		return nil, err
	}
	if b.Rates, err = guideRates(types, bands); err != nil {
		return nil, err
	}
	if goal != "" {
		b.GoalText = &goal
	}
	if audience != "" {
		b.AudienceNotes = &audience
	}
	if err := s.inTx(ctx, func(r *repository.BoostRepository) error {
		if err := r.Create(ctx, b); err != nil {
			return err
		}
		return r.Log(ctx, b.ID, actorKind, actor, action, map[string]any{"channels": channels, "pricing_model": b.PricingModel,
			"daily_budget_cents": b.DailyBudgetCents, "budget_from": b.BudgetFrom, "budget_until": b.BudgetUntil})
	}); err != nil {
		if errors.Is(err, repository.ErrBoostOpen) {
			return nil, boostErr(http.StatusConflict, "boost_already_open", "This event already has a Boost request or a live Boost.")
		}
		return nil, err
	}
	return s.repo.GetByID(ctx, b.ID)
}

// Accept records the host's acceptance of the proposal. Commission starts
// only when an admin then goes live.
func (s *BoostService) Accept(ctx context.Context, userID, boostID uuid.UUID, ip, userAgent string) (*models.Boost, error) {
	h, err := s.hostOf(ctx, userID)
	if err != nil {
		return nil, err
	}
	var evtTitle string
	err = s.inTx(ctx, func(r *repository.BoostRepository) error {
		b, err := r.GetForUpdate(ctx, boostID)
		if err != nil {
			return err
		}
		if b == nil || b.HostID != h.ID {
			return boostErr(http.StatusNotFound, "boost_not_found", "Boost not found.")
		}
		if b.Status != models.BoostQuoteReady || b.AcceptedAt != nil {
			return boostErr(http.StatusConflict, "invalid_state", "There's no proposal waiting for you to accept.")
		}
		evt, err := s.eventRepo.GetByID(ctx, b.EventID)
		if err != nil || evt == nil {
			return fmt.Errorf("load event: %v", err)
		}
		now, err := s.ticketTypes(ctx, evt)
		if err != nil {
			return err
		}
		if !pricesUnchanged(b.Rates, now) {
			return boostErr(http.StatusUnprocessableEntity, "quote_expired", "Your ticket prices changed after this proposal. The MySlotMate team will send a new one.")
		}
		if b.PricingModel == models.BoostPricingBudget {
			if err := s.requirePremium(ctx, r, h.ID); err != nil {
				return err
			}
		}
		evtTitle = evt.Title
		terms, due, err := s.acceptTerms(ctx, r, b)
		if err != nil {
			return err
		}
		if err := r.SetAccepted(ctx, b.ID, ip, userAgent, terms, due); err != nil {
			return err
		}
		return r.Log(ctx, b.ID, "host", h.ID.String(), "accepted", map[string]any{"terms_version": terms, "payment_due_by": due})
	})
	if err != nil {
		return nil, err
	}
	s.mailAdmin("Boost accepted: "+evtTitle, fmt.Sprintf("<p>The host accepted the proposal for <strong>%s</strong>. Go live from the admin when the ads are ready.</p>", html.EscapeString(evtTitle)))
	return s.repo.GetByID(ctx, boostID)
}

// AdminAccept accepts the open proposal for the host, when the host agreed
// to it off the website (on a call, say) and asked the team to handle it.
// The same checks as the host's own Accept apply. howAgreed says how the
// host agreed and is kept in the log; the request's IP and browser are
// stored as the acceptance's, and the host is emailed that the team
// accepted on their behalf, so they can stop it if that isn't right.
func (s *BoostService) AdminAccept(ctx context.Context, admin string, boostID uuid.UUID, howAgreed, ip, userAgent string) (*models.Boost, error) {
	howAgreed = strings.TrimSpace(howAgreed)
	if howAgreed == "" {
		return nil, boostErr(http.StatusBadRequest, "how_agreed_required", "Say how the host agreed to the proposal, for example on a call and when.")
	}
	if len([]rune(howAgreed)) > 500 {
		return nil, boostErr(http.StatusBadRequest, "too_long", "Keep it under 500 characters.")
	}
	err := s.inTx(ctx, func(r *repository.BoostRepository) error {
		b, err := r.GetForUpdate(ctx, boostID)
		if err != nil {
			return err
		}
		if b == nil {
			return boostErr(http.StatusNotFound, "boost_not_found", "Boost not found.")
		}
		if b.Status != models.BoostQuoteReady || b.AcceptedAt != nil {
			return boostErr(http.StatusConflict, "invalid_state", "There's no proposal waiting to be accepted.")
		}
		evt, err := s.eventRepo.GetByID(ctx, b.EventID)
		if err != nil || evt == nil {
			return fmt.Errorf("load event: %v", err)
		}
		now, err := s.ticketTypes(ctx, evt)
		if err != nil {
			return err
		}
		if !pricesUnchanged(b.Rates, now) {
			return boostErr(http.StatusUnprocessableEntity, "quote_expired", "The host changed ticket prices after this proposal. Send a new one first.")
		}
		if b.PricingModel == models.BoostPricingBudget {
			if err := s.requirePremium(ctx, r, b.HostID); err != nil {
				return err
			}
		}
		terms, due, err := s.acceptTerms(ctx, r, b)
		if err != nil {
			return err
		}
		if err := r.SetAccepted(ctx, b.ID, ip, userAgent, terms, due); err != nil {
			return err
		}
		return r.Log(ctx, b.ID, "admin", admin, "accepted_for_host",
			map[string]any{"how_agreed": howAgreed, "terms_version": terms, "payment_due_by": due})
	})
	if err != nil {
		return nil, err
	}
	out, err := s.repo.GetByID(ctx, boostID)
	if err == nil && out != nil {
		rates := make([]string, 0, len(out.Rates))
		for _, rt := range out.Rates {
			rates = append(rates, fmt.Sprintf("%s: %d.%02d%%", html.EscapeString(rt.TicketName), rt.RateBps/100, rt.RateBps%100))
		}
		s.mailHost(ctx, out, "We accepted your Boost proposal for you",
			fmt.Sprintf("<p>As agreed with you (%s), the MySlotMate team accepted the Boost proposal for <strong>%s</strong> on your behalf.</p><p>Commission per ticket sold while the Boost is live: %s. It comes out of your share; guests pay the same price.%s</p><p>If this isn't what you agreed, stop the Boost from the event page or reply to this email.</p>",
				html.EscapeString(howAgreed), html.EscapeString(out.EventTitle), strings.Join(rates, ", "), budgetLine(out)))
	}
	return out, err
}

// Stop withdraws a request, declines a proposal, or stops a live Boost.
func (s *BoostService) Stop(ctx context.Context, userID, boostID uuid.UUID) (*models.Boost, error) {
	h, err := s.hostOf(ctx, userID)
	if err != nil {
		return nil, err
	}
	stoppedLive := false
	var evtTitle string
	err = s.inTx(ctx, func(r *repository.BoostRepository) error {
		b, err := r.GetForUpdate(ctx, boostID)
		if err != nil {
			return err
		}
		if b == nil || b.HostID != h.ID {
			return boostErr(http.StatusNotFound, "boost_not_found", "Boost not found.")
		}
		evtTitle = b.EventTitle
		switch b.Status {
		case models.BoostUnderReview, models.BoostQuoteReady:
			if err := r.SetCancelled(ctx, b.ID); err != nil {
				return err
			}
			return r.Log(ctx, b.ID, "host", h.ID.String(), "cancelled", map[string]any{"from": b.Status})
		case models.BoostLive:
			stoppedLive = true
			if err := r.SetEnded(ctx, b.ID, time.Now(), "host_stopped"); err != nil {
				return err
			}
			return r.Log(ctx, b.ID, "host", h.ID.String(), "stopped", nil)
		}
		return boostErr(http.StatusConflict, "invalid_state", "This Boost has already finished.")
	})
	if err != nil {
		return nil, err
	}
	if stoppedLive {
		s.mailAdmin("Host stopped a live Boost: "+evtTitle, fmt.Sprintf("<p>The host stopped the live Boost on <strong>%s</strong>. Pause the ads.</p>", html.EscapeString(evtTitle)))
	}
	return s.repo.GetByID(ctx, boostID)
}

/* ------------------------------------------------------------------ */
/*  Admin side                                                         */
/* ------------------------------------------------------------------ */

// BoostDetail is one request with what the review needs.
type BoostDetail struct {
	*models.Boost
	Event        *models.Event          `json:"event"`
	CurrentRates []models.BoostRate     `json:"current_ticket_types"` // guide rates for what the event sells now
	Bands        []models.BoostBand     `json:"bands"`
	Eligibility  string                 `json:"eligibility_problem"` // "" = passes the request checks today
	Invited      bool                   `json:"host_invited"`
	Log          []models.BoostLogEntry `json:"log"`
	// Paid-budget Boost.
	Premium  *models.PremiumMembership `json:"premium"`
	Payments []*models.BoostPayment    `json:"payments"`
	Spend    []models.BoostSpendEntry  `json:"spend"`
}

func (s *BoostService) AdminList(ctx context.Context, status, budgetStatus string) ([]*models.Boost, map[string]int, error) {
	list, err := s.repo.ListByStatus(ctx, status, budgetStatus, 200)
	if err != nil {
		return nil, nil, err
	}
	if list == nil {
		list = []*models.Boost{}
	}
	counts, err := s.repo.CountByStatus(ctx)
	return list, counts, err
}

func (s *BoostService) AdminGet(ctx context.Context, boostID uuid.UUID) (*BoostDetail, error) {
	b, err := s.repo.GetByID(ctx, boostID)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, boostErr(http.StatusNotFound, "boost_not_found", "Boost not found.")
	}
	evt, err := s.eventRepo.GetByID(ctx, b.EventID)
	if err != nil || evt == nil {
		return nil, fmt.Errorf("load event: %v", err)
	}
	types, err := s.ticketTypes(ctx, evt)
	if err != nil {
		return nil, err
	}
	bands, err := s.repo.Bands(ctx, b.PricingModel)
	if err != nil {
		return nil, err
	}
	cur, err := guideRates(types, bands)
	if err != nil {
		return nil, err
	}
	d := &BoostDetail{Boost: b, Event: evt, CurrentRates: cur, Bands: bands}
	if h, err := s.hostRepo.GetByID(ctx, b.HostID); err == nil && h != nil {
		if reason, err := s.eligibility(ctx, h, evt, types, time.Now(), true); err == nil {
			d.Eligibility = reason
		}
	}
	if acc, err := s.repo.HostAccess(ctx, b.HostID); err == nil && acc != nil {
		d.Invited = acc.Invited
	}
	if d.Log, err = s.repo.LogEntries(ctx, b.ID); err != nil {
		return nil, err
	}
	if d.Log == nil {
		d.Log = []models.BoostLogEntry{}
	}
	if d.Premium, err = s.repo.Premium(ctx, b.HostID); err != nil {
		return nil, err
	}
	if d.Payments, err = s.repo.ListPayments(ctx, b.ID); err != nil {
		return nil, err
	}
	if d.Spend, err = s.repo.SpendEntries(ctx, b.ID); err != nil {
		return nil, err
	}
	return d, nil
}

// ProposalRate is the admin's rate for one ticket type.
type ProposalRate struct {
	TicketKey string `json:"ticket_key"`
	RateBps   int    `json:"rate_bps"`
	Reason    string `json:"reason"`
}

type Proposal struct {
	Rates        []ProposalRate `json:"rates"`
	Note         string         `json:"note"`
	PlannedFrom  *time.Time     `json:"planned_from"`
	PlannedUntil *time.Time     `json:"planned_until"`
	// The final budget plan (budget Boosts). Left out: the plan stays.
	Channels         []string `json:"channels"`
	DailyBudgetCents *int64   `json:"daily_budget_cents"`
	BudgetFrom       string   `json:"budget_from"`
	BudgetUntil      string   `json:"budget_until"`
}

// applyProposal checks the admin's rates against the bands: within the band's
// range, and a reason when it differs from the guide rate. Ticket types the
// admin left out keep their guide rate.
func applyProposal(guide []models.BoostRate, bands []models.BoostBand, p []ProposalRate) ([]models.BoostRate, error) {
	byKey := map[string]ProposalRate{}
	for _, x := range p {
		byKey[strings.ToLower(strings.TrimSpace(x.TicketKey))] = x
	}
	out := make([]models.BoostRate, len(guide))
	for i, g := range guide {
		x, ok := byKey[g.TicketKey]
		delete(byKey, g.TicketKey)
		if ok && x.RateBps != 0 {
			band, _ := bandFor(bands, g.PriceCents)
			if x.RateBps < band.MinRateBps || x.RateBps > band.MaxRateBps {
				if band.MinRateBps == band.MaxRateBps {
					return nil, fmt.Errorf("%s is fixed at %d%%", g.TicketName, band.GuideRateBps/100)
				}
				return nil, fmt.Errorf("%s must be between %d%% and %d%%", g.TicketName, band.MinRateBps/100, band.MaxRateBps/100)
			}
			g.RateBps = x.RateBps
			if reason := strings.TrimSpace(x.Reason); reason != "" {
				g.Reason = &reason
			}
		}
		if g.RateBps != g.GuideRateBps && (g.Reason == nil || *g.Reason == "") {
			return nil, fmt.Errorf("give a reason for %s: %d%% differs from the guide's %d%%", g.TicketName, g.RateBps/100, g.GuideRateBps/100)
		}
		out[i] = g
	}
	for k := range byKey {
		return nil, fmt.Errorf("the event doesn't sell a ticket type %q any more; reload and try again", k)
	}
	return out, nil
}

// Propose sends (or re-sends) the proposal to the host.
func (s *BoostService) Propose(ctx context.Context, admin string, boostID uuid.UUID, p Proposal) (*models.Boost, error) {
	note := strings.TrimSpace(p.Note)
	if len([]rune(note)) > 1000 {
		return nil, boostErr(http.StatusBadRequest, "too_long", "Keep the note under 1,000 characters.")
	}
	if p.PlannedFrom != nil && p.PlannedUntil != nil && !p.PlannedUntil.After(*p.PlannedFrom) {
		return nil, boostErr(http.StatusBadRequest, "invalid_window", "The planned end must be after the start.")
	}
	var b *models.Boost
	err := s.inTx(ctx, func(r *repository.BoostRepository) error {
		var err error
		b, err = r.GetForUpdate(ctx, boostID)
		if err != nil {
			return err
		}
		if b == nil {
			return boostErr(http.StatusNotFound, "boost_not_found", "Boost not found.")
		}
		if b.Status != models.BoostUnderReview && b.Status != models.BoostQuoteReady {
			return boostErr(http.StatusConflict, "invalid_state", "Only a new request or an open proposal can be (re)proposed.")
		}
		evt, err := s.eventRepo.GetByID(ctx, b.EventID)
		if err != nil || evt == nil {
			return fmt.Errorf("load event: %v", err)
		}
		types, err := s.ticketTypes(ctx, evt)
		if err != nil {
			return err
		}
		bands, err := r.Bands(ctx, b.PricingModel)
		if err != nil {
			return err
		}
		if err := s.proposeBudget(ctx, r, b, evt, p); err != nil {
			return err
		}
		guide, err := guideRates(types, bands)
		if err != nil {
			return err
		}
		rates, err := applyProposal(guide, bands, p.Rates)
		if err != nil {
			return boostErr(http.StatusBadRequest, "invalid_rates", strings.ToUpper(err.Error()[:1])+err.Error()[1:]+".")
		}
		if err := r.ReplaceRates(ctx, b.ID, rates); err != nil {
			return err
		}
		var notePtr *string
		if note != "" {
			notePtr = &note
		}
		if err := r.SetProposal(ctx, b.ID, notePtr, p.PlannedFrom, p.PlannedUntil); err != nil {
			return err
		}
		return r.Log(ctx, b.ID, "admin", admin, "proposed", map[string]any{"rates": rates, "note": note})
	})
	if err != nil {
		return nil, err
	}
	out, err := s.repo.GetByID(ctx, boostID)
	if err == nil && out != nil {
		s.mailHost(ctx, out, "Your Boost proposal is ready",
			fmt.Sprintf("<p>The MySlotMate team sent a proposal to boost <strong>%s</strong>. Open the event in your host dashboard to see the commission per ticket and accept or decline it.%s</p>", html.EscapeString(out.EventTitle), budgetLine(out)))
	}
	return out, err
}

func (s *BoostService) Decline(ctx context.Context, admin string, boostID uuid.UUID, reason string) (*models.Boost, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, boostErr(http.StatusBadRequest, "reason_required", "Tell the host why.")
	}
	err := s.inTx(ctx, func(r *repository.BoostRepository) error {
		b, err := r.GetForUpdate(ctx, boostID)
		if err != nil {
			return err
		}
		if b == nil {
			return boostErr(http.StatusNotFound, "boost_not_found", "Boost not found.")
		}
		if b.Status != models.BoostUnderReview && b.Status != models.BoostQuoteReady {
			return boostErr(http.StatusConflict, "invalid_state", "Only a request or an open proposal can be declined.")
		}
		if err := r.SetDeclined(ctx, b.ID, reason); err != nil {
			return err
		}
		return r.Log(ctx, b.ID, "admin", admin, "declined", map[string]any{"reason": reason})
	})
	if err != nil {
		return nil, err
	}
	out, err := s.repo.GetByID(ctx, boostID)
	if err == nil && out != nil {
		s.mailHost(ctx, out, "About your Boost request",
			fmt.Sprintf("<p>We can't boost <strong>%s</strong> this time.</p><p>%s</p>", html.EscapeString(out.EventTitle), html.EscapeString(reason)))
	}
	return out, err
}

// GoLive starts commission on an accepted proposal. until overrides the
// planned end; a one-time event ends at its start time anyway.
func (s *BoostService) GoLive(ctx context.Context, admin string, boostID uuid.UUID, until *time.Time) (*models.Boost, error) {
	now := time.Now()
	err := s.inTx(ctx, func(r *repository.BoostRepository) error {
		b, err := r.GetForUpdate(ctx, boostID)
		if err != nil {
			return err
		}
		if b == nil {
			return boostErr(http.StatusNotFound, "boost_not_found", "Boost not found.")
		}
		if b.Status != models.BoostQuoteReady || b.AcceptedAt == nil {
			return boostErr(http.StatusConflict, "invalid_state", "The host has to accept the proposal before it can go live.")
		}
		evt, err := s.eventRepo.GetByID(ctx, b.EventID)
		if err != nil || evt == nil {
			return fmt.Errorf("load event: %v", err)
		}
		types, err := s.ticketTypes(ctx, evt)
		if err != nil {
			return err
		}
		if !pricesUnchanged(b.Rates, types) {
			return boostErr(http.StatusUnprocessableEntity, "quote_expired", "The host changed ticket prices after accepting. Send a new proposal.")
		}
		if !BoostAllowsRefundPolicy(evt.CancellationPolicy) {
			return boostErr(http.StatusUnprocessableEntity, "refund_policy", "The event's refund policy is Flexible or not set. Ask the host to switch to Moderate, Strict or No refunds before going live.")
		}
		end := until
		if b.PricingModel == models.BoostPricingBudget {
			if err := goLiveBudgetCheck(ctx, r, b); err != nil {
				return err
			}
			if end == nil {
				end = budgetEnd(b)
			}
		}
		if end == nil {
			end = b.PlannedUntil
		}
		if evt.ScheduleType == models.ScheduleTypeOneTime && (end == nil || end.After(evt.Time)) {
			t := evt.Time
			end = &t
		}
		if end != nil && !end.After(now) {
			return boostErr(http.StatusBadRequest, "invalid_window", "The end time has already passed.")
		}
		if err := r.SetLive(ctx, b.ID, now, end); err != nil {
			return err
		}
		return r.Log(ctx, b.ID, "admin", admin, "went_live", map[string]any{"live_until": end})
	})
	if err != nil {
		return nil, err
	}
	out, err := s.repo.GetByID(ctx, boostID)
	if err == nil && out != nil {
		s.mailHost(ctx, out, "Your Boost is live",
			fmt.Sprintf("<p>Your Boost for <strong>%s</strong> is live. The agreed commission applies to tickets sold from now until it ends. You can stop it any time from the event page.%s</p>", html.EscapeString(out.EventTitle), liveBudgetLine(out)))
	}
	return out, err
}

// End stops a live Boost now.
func (s *BoostService) End(ctx context.Context, actorKind, actor string, boostID uuid.UUID, at time.Time, reason string) (*models.Boost, error) {
	return s.EndWithNote(ctx, actorKind, actor, boostID, at, reason, "")
}

// EndWithNote is End with a note the host sees (the team ending it early).
func (s *BoostService) EndWithNote(ctx context.Context, actorKind, actor string, boostID uuid.UUID, at time.Time, reason, note string) (*models.Boost, error) {
	err := s.inTx(ctx, func(r *repository.BoostRepository) error {
		b, err := r.GetForUpdate(ctx, boostID)
		if err != nil {
			return err
		}
		if b == nil {
			return boostErr(http.StatusNotFound, "boost_not_found", "Boost not found.")
		}
		if b.Status != models.BoostLive {
			return boostErr(http.StatusConflict, "invalid_state", "Only a live Boost can be ended.")
		}
		if err := r.SetEnded(ctx, b.ID, at, reason); err != nil {
			return err
		}
		return r.Log(ctx, b.ID, actorKind, actor, "ended", map[string]any{"reason": reason, "note": note})
	})
	if err != nil {
		return nil, err
	}
	out, err := s.repo.GetByID(ctx, boostID)
	if err == nil && out != nil {
		extra := ""
		if note != "" {
			extra = "<p>" + html.EscapeString(note) + "</p>"
		}
		if out.PricingModel == models.BoostPricingBudget {
			extra += fmt.Sprintf("<p>Ad spend so far: %s of %s paid. The team will share the final report within 2 days.</p>",
				rupeesText(out.SpentCents), rupeesText(out.PaidCents))
		}
		s.mailHost(ctx, out, "Your Boost has ended",
			fmt.Sprintf("<p>Your Boost for <strong>%s</strong> has ended: %d tickets sold while it was live. Tickets sold from now on carry no Boost commission.</p>%s",
				html.EscapeString(out.EventTitle), out.TicketsSold, extra))
	}
	return out, err
}

// Campaign records reach and clicks reported by the ads.
func (s *BoostService) Campaign(ctx context.Context, admin string, boostID uuid.UUID, reach, clicks int64) (*models.Boost, error) {
	if reach < 0 || clicks < 0 {
		return nil, boostErr(http.StatusBadRequest, "invalid_numbers", "Reach and clicks can't be negative.")
	}
	b, err := s.repo.GetByID(ctx, boostID)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, boostErr(http.StatusNotFound, "boost_not_found", "Boost not found.")
	}
	if b.Status != models.BoostLive && b.Status != models.BoostEnded {
		return nil, boostErr(http.StatusConflict, "invalid_state", "Campaign numbers can be logged once the Boost is live.")
	}
	if err := s.repo.SetCampaign(ctx, boostID, reach, clicks); err != nil {
		return nil, err
	}
	_ = s.repo.Log(ctx, boostID, "admin", admin, "campaign", map[string]any{"reach": reach, "clicks": clicks})
	return s.repo.GetByID(ctx, boostID)
}

// HostInvited reports whether the host is in the Boost pilot.
func (s *BoostService) HostInvited(ctx context.Context, hostID uuid.UUID) (bool, error) {
	acc, err := s.repo.HostAccess(ctx, hostID)
	if err != nil {
		return false, err
	}
	if acc == nil {
		return false, boostErr(http.StatusNotFound, "host_not_found", "Host not found.")
	}
	return acc.Invited, nil
}

// SetHostInvited adds or removes a host from the Boost pilot.
// Premium replaces the invite: inviting a host also makes them Premium (a
// grant), so the admin's existing toggle keeps working.
func (s *BoostService) SetHostInvited(ctx context.Context, hostID uuid.UUID, invited bool) error {
	if err := s.repo.SetInvited(ctx, hostID, invited); err != nil {
		return err
	}
	if !invited {
		return nil
	}
	if ok, err := s.repo.PremiumActive(ctx, hostID); err != nil || ok {
		return err
	}
	_, err := s.PremiumGrant(ctx, "boost_invite", hostID, PremiumDecision{})
	return err
}

// AutoEnd ends live Boosts whose window is over. Returns how many ended.
func (s *BoostService) AutoEnd(ctx context.Context) (int, error) {
	s.expireUnpaid(ctx)
	s.invoicePending(ctx)
	due, err := s.repo.DueToEnd(ctx, time.Now())
	if err != nil {
		return 0, err
	}
	n := 0
	for id, at := range due {
		if _, err := s.End(ctx, "system", "", id, at, "window_over"); err != nil {
			log.Printf("[BOOST] auto-end %s: %v", id, err)
			continue
		}
		n++
	}
	return n, nil
}

// RunAutoEnd ends finished Boosts every interval until ctx ends.
func (s *BoostService) RunAutoEnd(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if n, err := s.AutoEnd(ctx); err != nil {
			log.Printf("[BOOST] auto-end: %v", err)
		} else if n > 0 {
			log.Printf("[BOOST] ended %d Boost(s)", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

/* ------------------------------------------------------------------ */
/*  Payout eligibility (the website's Boost card and payout settings)  */
/* ------------------------------------------------------------------ */

// PayoutEligibility is the website's PayoutEligibilityDTO. Weekly payouts,
// strikes and recovery balances are not built yet (roadmap task 26), so their
// fields are zero and weekly_eligible is false with that reason.
type PayoutEligibility struct {
	WeeklyEligible        bool       `json:"weekly_eligible"`
	Reasons               []string   `json:"reasons"`
	CompletedEvents       int        `json:"completed_events"`
	Strikes90d            int        `json:"strikes_90d"`
	Strikes12m            int        `json:"strikes_12m"`
	PausedUntil           *time.Time `json:"paused_until"`
	BoostBlocked          bool       `json:"boost_blocked"`
	BoostInvited          bool       `json:"boost_invited"`
	RecoveryBalanceCents  int64      `json:"recovery_balance_cents"`
	EarlyOutstandingCents int64      `json:"early_outstanding_cents"`
	EarlyBackstopCents    int64      `json:"early_backstop_cents"`
}

func (s *BoostService) PayoutEligibility(ctx context.Context, userID, hostID uuid.UUID) (*PayoutEligibility, error) {
	h, err := s.hostOf(ctx, userID)
	if err != nil {
		return nil, err
	}
	if h.ID != hostID {
		return nil, boostErr(http.StatusForbidden, "not_your_host", "You can only read your own payout settings.")
	}
	acc, err := s.repo.HostAccess(ctx, h.ID)
	if err != nil || acc == nil {
		return nil, err
	}
	premium, err := s.repo.PremiumActive(ctx, h.ID)
	if err != nil {
		return nil, err
	}
	return &PayoutEligibility{Reasons: []string{"Weekly payouts aren't available yet."}, CompletedEvents: acc.CompletedEvents,
		BoostInvited: premium, EarlyBackstopCents: 5000000}, nil
}

/* ------------------------------------------------------------------ */
/*  Commission on a booking (called from CreateBooking)                */
/* ------------------------------------------------------------------ */

// BoostCharge is the Boost commission on one booking.
type BoostCharge struct {
	BoostID         uuid.UUID
	RateBps         int
	CommissionCents int64
	GSTCents        int64 // 18/118 inside the commission, internal only
}

// boostCommissionFor returns the commission on a ticket booking paid now when
// the event has a live Boost with a rate for its ticket type, or nil. The rate
// applies only while the ticket still sells at the price it was quoted on (a
// changed price needs a new proposal).
func boostCommissionFor(ctx context.Context, db repository.DBTX, eventID uuid.UUID, tierName *string,
	unitPriceCents, ticketCents int64, at time.Time) (*BoostCharge, error) {
	if ticketCents <= 0 {
		return nil, nil
	}
	lr, err := repository.NewBoostRepository(db).LiveRateFor(ctx, eventID, models.BoostTicketKey(tierName), at)
	if err != nil || lr == nil {
		return nil, err
	}
	if lr.PriceCents != unitPriceCents {
		log.Printf("[BOOST] %s: ticket price %d differs from the quoted %d; no commission until re-proposed", lr.BoostID, unitPriceCents, lr.PriceCents)
		return nil, nil
	}
	c := BoostCommission(ticketCents, lr.RateBps)
	return &BoostCharge{BoostID: lr.BoostID, RateBps: lr.RateBps, CommissionCents: c, GSTCents: gstInside(c)}, nil
}

/* ------------------------------------------------------------------ */
/*  Helpers                                                            */
/* ------------------------------------------------------------------ */

func (s *BoostService) inTx(ctx context.Context, fn func(r *repository.BoostRepository) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(s.repo.WithTx(tx)); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *BoostService) mailAdmin(subject, body string) {
	if s.notify == nil || s.adminTo == "" {
		log.Printf("[BOOST] (no email) %s", subject)
		return
	}
	go func() {
		if err := s.notify.BoostEmail(s.adminTo, subject, body); err != nil {
			log.Printf("[BOOST] admin email %q: %v", subject, err)
		}
	}()
}

func (s *BoostService) mailHost(ctx context.Context, b *models.Boost, subject, body string) {
	if s.notify == nil {
		log.Printf("[BOOST] (no email) %s", subject)
		return
	}
	h, err := s.hostRepo.GetByID(ctx, b.HostID)
	if err != nil || h == nil {
		return
	}
	u, err := s.userRepo.GetByID(ctx, h.UserID)
	if err != nil || u == nil || u.Email == "" {
		return
	}
	if s.siteURL != "" {
		body += fmt.Sprintf(`<p><a href="%s/host-dashboard/experiences/%s">Open the event</a></p>`, s.siteURL, b.EventID)
	}
	go func() {
		if err := s.notify.BoostEmail(u.Email, subject+" · MySlotMate", "<html><body style=\"font-family: Arial, sans-serif; line-height: 1.6; color: #333;\">"+body+"</body></html>"); err != nil {
			log.Printf("[BOOST] host email %q: %v", subject, err)
		}
	}()
}
