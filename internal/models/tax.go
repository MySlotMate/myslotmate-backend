package models

import (
	"time"

	"github.com/google/uuid"
)

// TaxSettingsKey is the platform_settings key for statutory tax deductions
// (GST TCS, Section 52 CGST Act; income-tax TDS, Section 194-O).
const TaxSettingsKey = "tax_settings"

// TaxSettings is the "tax_settings" value. Rates are basis points (50 = 0.5%).
// Both switches ship off: Enabled starts stamping TCS/TDS on new bookings,
// RequirePANForWithdrawal refuses withdrawals until a tax profile exists.
type TaxSettings struct {
	Enabled                 bool  `json:"enabled"`
	RequirePANForWithdrawal bool  `json:"require_pan_for_withdrawal"`
	TCSBps                  int64 `json:"tcs_bps"`
	TDSBps                  int64 `json:"tds_bps"`
	TDSThresholdCents       int64 `json:"tds_threshold_cents"`
}

// Host entity types. Individuals and HUFs get the ₹5 lakh TDS exemption.
const (
	EntityIndividual = "individual"
	EntityHUF        = "huf"
	EntityCompany    = "company"
	EntityFirm       = "firm"
	EntityLLP        = "llp"
	EntityOther      = "other"
)

// TaxProfile is a host's PAN/GSTIN, collected at first withdrawal.
type TaxProfile struct {
	HostID       uuid.UUID `db:"host_id" json:"host_id"`
	EntityType   string    `db:"entity_type" json:"entity_type"`
	PAN          string    `db:"pan" json:"pan"`
	PANName      string    `db:"pan_name" json:"pan_name"`
	GSTIN        *string   `db:"gstin" json:"gstin,omitempty"`
	GSTStateCode *string   `db:"gst_state_code" json:"gst_state_code,omitempty"`
	UpdatedAt    time.Time `db:"updated_at" json:"updated_at"`
}

// Valid bounds the setting so a typo cannot take a large slice of every host's
// payout: TCS at most 1% (statute is 0.5%; was 1% before Jul 2024), TDS at
// most 5% (the no-PAN rate), and a positive threshold.
func (t TaxSettings) Valid() bool {
	return t.TCSBps >= 0 && t.TCSBps <= 100 &&
		t.TDSBps >= 0 && t.TDSBps <= 500 &&
		t.TDSThresholdCents > 0
}
