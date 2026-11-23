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
}

// eligibility returns the first reason the event can't be boosted, or "".
func (s *BoostService) eligibility(ctx context.Context, h *models.Host, evt *models.Event, types []models.BoostRate, now time.Time) (string, error) {
	if evt.IsFree || len(types) == 0 {
		return "Free events can't be boosted.", nil
	}
	if evt.Status != models.EventStatusLive {
		return "Publish the event first.", nil
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
	acc, err := s.repo.HostAccess(ctx, h.ID)
	if err != nil {
		return nil, err
	}
	if acc == nil || !acc.Invited {
		return nil, boostErr(http.StatusForbidden, "boost_not_invited", "Event Boost is in a pilot with invited hosts. Ask the MySlotMate team to add you.")
	}

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
	if reason, err := s.eligibility(ctx, h, evt, types, now); err != nil {
		return nil, err
	} else if reason != "" {
		return nil, boostErr(http.StatusUnprocessableEntity, "event_not_eligible", reason)
	}
	bands, err := s.repo.Bands(ctx)
	if err != nil {
		return nil, err
	}
	rates, err := guideRates(types, bands)
	if err != nil {
		return nil, err
	}

	b := &models.Boost{ID: uuid.New(), EventID: evt.ID, HostID: h.ID, Status: models.BoostUnderReview,
		Channels: channels, RequestedAt: now, Rates: rates}
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
		return r.Log(ctx, b.ID, "host", h.ID.String(), "requested", map[string]any{"channels": channels})
	}); err != nil {
		if errors.Is(err, repository.ErrBoostOpen) {
			return nil, boostErr(http.StatusConflict, "boost_already_open", "This event already has a Boost request or a live Boost.")
		}
		return nil, err
	}
	s.mailAdmin(fmt.Sprintf("New Boost request: %s", evt.Title),
		fmt.Sprintf("<p><strong>%s</strong> asked to boost <strong>%s</strong>.</p><p>Review it in the admin under Boosts.</p>",
			html.EscapeString(strings.TrimSpace(h.FirstName+" "+h.LastName)), html.EscapeString(evt.Title)))
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
		evtTitle = evt.Title
		if err := r.SetAccepted(ctx, b.ID, ip, userAgent); err != nil {
			return err
		}
		return r.Log(ctx, b.ID, "host", h.ID.String(), "accepted", map[string]any{"terms_version": models.BoostTermsVersion})
	})
	if err != nil {
		return nil, err
	}
	s.mailAdmin("Boost accepted: "+evtTitle, fmt.Sprintf("<p>The host accepted the proposal for <strong>%s</strong>. Go live from the admin when the ads are ready.</p>", html.EscapeString(evtTitle)))
	return s.repo.GetByID(ctx, boostID)
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
}

func (s *BoostService) AdminList(ctx context.Context, status string) ([]*models.Boost, map[string]int, error) {
	list, err := s.repo.ListByStatus(ctx, status, 200)
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
	bands, err := s.repo.Bands(ctx)
	if err != nil {
		return nil, err
	}
	cur, err := guideRates(types, bands)
	if err != nil {
		return nil, err
	}
	d := &BoostDetail{Boost: b, Event: evt, CurrentRates: cur, Bands: bands}
	if h, err := s.hostRepo.GetByID(ctx, b.HostID); err == nil && h != nil {
		if reason, err := s.eligibility(ctx, h, evt, types, time.Now()); err == nil {
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
		bands, err := r.Bands(ctx)
		if err != nil {
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
			fmt.Sprintf("<p>The MySlotMate team sent a proposal to boost <strong>%s</strong>. Open the event in your host dashboard to see the commission per ticket and accept or decline it.</p>", html.EscapeString(out.EventTitle)))
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
		end := until
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
			fmt.Sprintf("<p>Your Boost for <strong>%s</strong> is live. The agreed commission applies to tickets sold from now until it ends. You can stop it any time from the event page.</p>", html.EscapeString(out.EventTitle)))
	}
	return out, err
}

// End stops a live Boost now.
func (s *BoostService) End(ctx context.Context, actorKind, actor string, boostID uuid.UUID, at time.Time, reason string) (*models.Boost, error) {
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
		return r.Log(ctx, b.ID, actorKind, actor, "ended", map[string]any{"reason": reason})
	})
	if err != nil {
		return nil, err
	}
	out, err := s.repo.GetByID(ctx, boostID)
	if err == nil && out != nil {
		s.mailHost(ctx, out, "Your Boost has ended",
			fmt.Sprintf("<p>Your Boost for <strong>%s</strong> has ended: %d tickets sold while it was live. Tickets sold from now on carry no Boost commission.</p>",
				html.EscapeString(out.EventTitle), out.TicketsSold))
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
func (s *BoostService) SetHostInvited(ctx context.Context, hostID uuid.UUID, invited bool) error {
	return s.repo.SetInvited(ctx, hostID, invited)
}

// AutoEnd ends live Boosts whose window is over. Returns how many ended.
func (s *BoostService) AutoEnd(ctx context.Context) (int, error) {
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
	return &PayoutEligibility{Reasons: []string{"Weekly payouts aren't available yet."}, CompletedEvents: acc.CompletedEvents,
		BoostInvited: acc.Invited, EarlyBackstopCents: 5000000}, nil
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
