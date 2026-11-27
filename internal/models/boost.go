package models

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Event Boost: a host asks MySlotMate to promote one event; the admin team
// proposes a commission per ticket type; once accepted and live, that
// commission is taken on the event's tickets sold while live.

const (
	BoostUnderReview = "under_review"
	BoostQuoteReady  = "quote_ready" // accepted_at set once the host accepts
	BoostLive        = "live"
	BoostEnded       = "ended"
	BoostDeclined    = "declined"
	BoostCancelled   = "cancelled"
)

// BoostChannels are where a Boost may run.
var BoostChannels = map[string]bool{"instagram": true, "facebook": true, "google": true, "whatsapp": true, "influencers": true}

// BoostTermsVersion is recorded with each acceptance.
const BoostTermsVersion = "boost-v1-2026-11"

// BoostBand is one host-guide price band.
type BoostBand struct {
	Version       int    `json:"version"`
	MinPriceCents int64  `json:"min_price_cents"`
	MaxPriceCents *int64 `json:"max_price_cents"`
	GuideRateBps  int    `json:"guide_rate_bps"`
	MinRateBps    int    `json:"min_rate_bps"`
	MaxRateBps    int    `json:"max_rate_bps"`
}

// Contains reports whether a ticket price falls in the band.
func (b BoostBand) Contains(priceCents int64) bool {
	return priceCents >= b.MinPriceCents && (b.MaxPriceCents == nil || priceCents <= *b.MaxPriceCents)
}

// BoostRate is the commission on one ticket type.
type BoostRate struct {
	TicketKey    string     `json:"ticket_key"`     // "" = single price, else lower-cased ticket name
	PriceTierID  *uuid.UUID `json:"ticket_type_id"` // as quoted; nil = the event's single price
	TicketName   string     `json:"ticket_name"`
	PriceCents   int64      `json:"price_cents"`
	GuideRateBps int        `json:"guide_rate_bps"`
	RateBps      int        `json:"rate_bps"`
	Reason       *string    `json:"reason"` // why it differs from the guide rate
	BandVersion  int        `json:"band_version"`
}

// Boost is one request, in the shape the website's BoostDTO expects.
type Boost struct {
	ID            uuid.UUID   `json:"id"`
	EventID       uuid.UUID   `json:"event_id"`
	HostID        uuid.UUID   `json:"host_id"`
	Status        string      `json:"status"`
	Rates         []BoostRate `json:"rates"`
	TopupReason   *string     `json:"topup_reason"` // first per-ticket reason (older screens)
	AdminNote     *string     `json:"admin_note"`
	DeclineReason *string     `json:"decline_reason"`
	GoalText      *string     `json:"goal_text"`
	AudienceNotes *string     `json:"audience_notes"`
	Channels      []string    `json:"channels"`
	RequestedAt   time.Time   `json:"requested_at"`
	ProposedAt    *time.Time  `json:"proposed_at"`
	PlannedFrom   *time.Time  `json:"planned_from"`
	PlannedUntil  *time.Time  `json:"planned_until"`
	AcceptedAt    *time.Time  `json:"accepted_at"`
	LiveFrom      *time.Time  `json:"live_from"`
	LiveUntil     *time.Time  `json:"live_until"`
	EndedReason   *string     `json:"ended_reason"`
	Reach         int64       `json:"reach"`
	Clicks        int64       `json:"clicks"`
	// Derived from bookings stamped with this Boost, net of refunds.
	TicketsSold     int   `json:"tickets_sold"`
	CommissionCents int64 `json:"commission_cents"`
	// Paid-budget Boost (pricing_model "budget"). Days are IST, YYYY-MM-DD.
	PricingModel     string     `json:"pricing_model"`
	DailyBudgetCents *int64     `json:"daily_budget_cents"`
	BudgetFrom       *string    `json:"budget_from"`
	BudgetUntil      *string    `json:"budget_until"`
	BudgetTotalCents int64      `json:"budget_total_cents"`
	BudgetGSTCents   int64      `json:"budget_gst_cents"`
	BudgetStatus     string     `json:"budget_status"`
	PaymentDueBy     *time.Time `json:"payment_due_by"`
	SpentCents       int64      `json:"spent_cents"`
	PaidCents        int64      `json:"paid_cents"` // budget paid so far, before GST: the spend cap
	ClosedAt         *time.Time `json:"closed_at"`
	CreativesCount   int        `json:"creatives_count"`
	// Host side: team creatives waiting for the host. Admin side: see CreativesToReview.
	CreativesNeedAction int `json:"creatives_need_action"`
	CreativesToReview   int `json:"creatives_to_review"` // host creatives the team hasn't reviewed
	ReportsCount        int `json:"reports_count"`
	// Admin views.
	EventTitle string `json:"event_title,omitempty"`
	HostName   string `json:"host_name,omitempty"`
}

// BoostLogEntry is one line of a Boost's audit trail.
type BoostLogEntry struct {
	ActorKind string          `json:"actor_kind"`
	Actor     *string         `json:"actor"`
	Action    string          `json:"action"`
	Details   json.RawMessage `json:"details,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// BoostTicketKey is how a ticket type is matched to its Boost rate: "" for the
// event's single price, else the lower-cased, trimmed ticket name.
func BoostTicketKey(tierName *string) string {
	if tierName == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(*tierName))
}
