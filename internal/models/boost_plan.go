package models

import (
	"time"

	"github.com/google/uuid"
)

// Premium Hosts and paid Boost marketing (PRD 2026-11-26): the host pays the
// ad budget upfront and a marketing fee on tickets sold while live.

const (
	BoostPricingNoUpfront = "no_upfront"
	BoostPricingBudget    = "budget"

	BudgetUnpaid      = "unpaid"
	BudgetPaid        = "paid"
	BudgetNotRequired = "not_required"

	// BoostBudgetTermsVersion is recorded when a host accepts a budget Boost.
	BoostBudgetTermsVersion = "boost-budget-v2-2026-11"
	// FirstBudgetBandVersion is the first band version budget Boosts are priced on.
	FirstBudgetBandVersion = 2
)

// BoostSettings are the budget rules the team can change.
type BoostSettings struct {
	MinDailyBudgetCents int64     `json:"min_daily_budget_cents"`
	BudgetStepCents     int64     `json:"budget_step_cents"`
	MinDays             int       `json:"min_days"`
	PaymentWindowDays   int       `json:"payment_window_days"`
	BudgetGSTBps        int       `json:"budget_gst_bps"`
	BudgetRequired      bool      `json:"budget_required"`
	// Upload limits, MB per file.
	CreativeImageMaxMB int       `json:"creative_image_max_mb"`
	CreativeVideoMaxMB int       `json:"creative_video_max_mb"`
	CreativeOtherMaxMB int       `json:"creative_other_max_mb"`
	ReportMaxMB        int       `json:"report_max_mb"`
	UpdatedAt          time.Time `json:"updated_at"`
}

const (
	BoostPaymentBudget = "budget"
	BoostPaymentTopUp  = "top_up"

	BoostPayCheckout = "checkout"
	BoostPayLink     = "payment_link"
	BoostPayOffline  = "offline"

	BoostPayCreated  = "created"
	BoostPayPaid     = "paid"
	BoostPayFailed   = "failed"
	BoostPayExpired  = "expired"
	BoostPayReversed = "reversed"
)

// BoostPayment is one payment towards a Boost's budget. Amounts are before GST.
type BoostPayment struct {
	ID                  uuid.UUID  `json:"id"`
	BoostID             uuid.UUID  `json:"boost_id"`
	Kind                string     `json:"kind"`
	AmountCents         int64      `json:"amount_cents"`
	GSTCents            int64      `json:"gst_cents"`
	DaysAdded           int        `json:"days_added"`
	NewDailyBudgetCents *int64     `json:"new_daily_budget_cents"`
	Method              string     `json:"method"`
	RazorpayOrderID     *string    `json:"razorpay_order_id"`
	RazorpayPaymentID   *string    `json:"razorpay_payment_id"`
	RazorpayLinkID      *string    `json:"razorpay_link_id"`
	LinkURL             *string    `json:"link_url"`
	OfflineReference    *string    `json:"offline_reference"`
	OfflinePaidOn       *string    `json:"offline_paid_on"`
	RecordedBy          *string    `json:"recorded_by"`
	Status              string     `json:"status"`
	ReverseReason       *string    `json:"reverse_reason"`
	InvoiceID           *uuid.UUID `json:"invoice_id"`
	CreatedAt           time.Time  `json:"created_at"`
	PaidAt              *time.Time `json:"paid_at"`
}

// TotalCents is what the host pays: amount plus GST.
func (p *BoostPayment) TotalCents() int64 { return p.AmountCents + p.GSTCents }

// BoostSpendEntry is one day's spend and results on one channel.
type BoostSpendEntry struct {
	Day         string    `json:"day"` // YYYY-MM-DD, IST
	Channel     string    `json:"channel"`
	SpendCents  int64     `json:"spend_cents"`
	Impressions int64     `json:"impressions"`
	Reach       int64     `json:"reach"`
	Clicks      int64     `json:"clicks"`
	Note        *string   `json:"note"`
	EnteredBy   string    `json:"entered_by"`
	UpdatedAt   time.Time `json:"updated_at"`
}

const (
	PremiumNone     = "none" // no row: never applied
	PremiumApplied  = "applied"
	PremiumActive   = "active"
	PremiumDeclined = "declined"
	PremiumRemoved  = "removed"

	// PremiumReapplyDays is how long a declined host waits to apply again.
	PremiumReapplyDays = 30
)

// PremiumMembership is a host's Premium status, in the website's PremiumDTO shape.
type PremiumMembership struct {
	HostID          uuid.UUID  `json:"host_id"`
	Status          string     `json:"status"`
	AppliedAt       *time.Time `json:"applied_at"`
	ApplicationNote *string    `json:"application_note"`
	DecidedAt       *time.Time `json:"decided_at"`
	DecidedBy       *string    `json:"decided_by,omitempty"`
	Granted         bool       `json:"granted"`
	ManagerName     *string    `json:"manager_name"`
	ManagerContact  *string    `json:"manager_contact"`
	DeclineReason   *string    `json:"decline_reason"`
	RemoveReason    *string    `json:"remove_reason"`
	UpdatedAt       *time.Time `json:"updated_at,omitempty"`
	// Admin list.
	HostName  string `json:"host_name,omitempty"`
	HostEmail string `json:"host_email,omitempty"`
}

const (
	CreativeReel  = "reel"
	CreativePost  = "post"
	CreativeStory = "story"
	CreativeBrand = "brand"

	// Host uploads.
	CreativeSubmitted    = "submitted"
	CreativeInUse        = "in_use"
	CreativeNeedsChanges = "needs_changes"
	CreativeNotUsed      = "not_used"
	// Team uploads.
	CreativeAwaitingApproval = "awaiting_approval"
	CreativeApproved         = "approved"
	CreativeChangesRequested = "changes_requested"
)

// StoredFile is one uploaded file; URL is a short-lived signed download link.
type StoredFile struct {
	Key      string `json:"key"`
	URL      string `json:"url"`
	Name     string `json:"name"`
	MimeType string `json:"mime_type"`
	Size     int64  `json:"size"`
}

// BoostCreative is one item in a Boost's creatives space.
type BoostCreative struct {
	ID             uuid.UUID    `json:"id"`
	BoostID        uuid.UUID    `json:"boost_id"`
	Type           string       `json:"type"`
	Files          []StoredFile `json:"files"`
	Caption        *string      `json:"caption"`
	Channels       []string     `json:"channels"`
	UploadedByKind string       `json:"uploaded_by_kind"`
	UploadedBy     string       `json:"-"`
	Status         string       `json:"status"`
	StatusNote     *string      `json:"status_note"`
	ReplacesID     *uuid.UUID   `json:"replaces_id"`
	CreatedAt      time.Time    `json:"created_at"`
}

// BoostReport is a file the team shares on a Boost.
type BoostReport struct {
	ID          uuid.UUID  `json:"id"`
	BoostID     uuid.UUID  `json:"boost_id"`
	Kind        string     `json:"kind"` // update | final
	Title       string     `json:"title"`
	CoversFrom  *string    `json:"covers_from"`
	CoversUntil *string    `json:"covers_until"`
	Note        *string    `json:"note"`
	File        StoredFile `json:"file"`
	UploadedBy  string     `json:"uploaded_by,omitempty"`
	ReplacesID  *uuid.UUID `json:"replaces_id,omitempty"`
	ReplacedAt  *time.Time `json:"replaced_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

// InvoiceSourceBoostBudget is a tax invoice for a Boost's ad budget payment.
const InvoiceSourceBoostBudget = "boost_budget"
