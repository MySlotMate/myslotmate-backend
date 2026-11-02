package models

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// PlatformSettings stores config like platform fee percentages.
type PlatformSettings struct {
	ID        uuid.UUID       `db:"id" json:"id"`
	Key       string          `db:"key" json:"key"`
	Value     json.RawMessage `db:"value" json:"value"`
	CreatedAt time.Time       `db:"created_at" json:"created_at"`
	UpdatedAt time.Time       `db:"updated_at" json:"updated_at"`
}

// PlatformFeeConfig is the structure for the "platform_fee" key.
type PlatformFeeConfig struct {
	HostPercentage     int `json:"host_percentage"`     // e.g. 70
	PlatformPercentage int `json:"platform_percentage"` // e.g. 30
}

// BookingFeeKey is the platform_settings key for the guest-paid booking fee.
const BookingFeeKey = "booking_fee"

// BookingFeeConfig is the "booking_fee" key: a flat amount the guest pays on
// top of the ticket total, once per paid booking, kept whole by the platform.
// Unlike PlatformFeeConfig (the host's commission, invisible to the guest)
// this raises what the buyer pays. 0 — the default when unseeded — turns it off.
type BookingFeeConfig struct {
	AmountCents int64 `json:"amount_cents"`
}

// MaxBookingFeeCents caps the admin-set fee (₹1,000) so a typo can't drain wallets.
const MaxBookingFeeCents = 100000

// BookingFeeCents is the guest fee for a booking: the flat amount on any paid
// booking, nothing on a free one. The Node backend and both clients mirror it.
func BookingFeeCents(ticketTotalCents int64, feeCents int64) int64 {
	if ticketTotalCents <= 0 || feeCents <= 0 {
		return 0
	}
	return feeCents
}

// EffectiveFeeConfig applies a host's commission override (Host.PlatformFeePercentage)
// on top of the global default. Pass nil for hostOverridePct to get the global
// config back unchanged. Callers must use the returned config (not the global
// one) when computing a booking's fee split, so per-host rates take effect.
func EffectiveFeeConfig(global *PlatformFeeConfig, hostOverridePct *int) *PlatformFeeConfig {
	if hostOverridePct == nil {
		return global
	}
	pct := *hostOverridePct
	return &PlatformFeeConfig{
		HostPercentage:     100 - pct,
		PlatformPercentage: pct,
	}
}
