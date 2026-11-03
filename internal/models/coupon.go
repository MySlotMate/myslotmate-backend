package models

import (
	"time"

	"github.com/google/uuid"
)

// Coupon is a host-issued code: an access code, a free-booking code, or a
// percentage-discount code (see GrantsFree / DiscountPercent). Scope is a single event
// (EventID set) or every event the host runs (EventID nil).
type Coupon struct {
	ID     uuid.UUID `db:"id" json:"id"`
	HostID uuid.UUID `db:"host_id" json:"host_id"`
	// EventID scopes the coupon to one event; nil = all of the host's events.
	EventID *uuid.UUID `db:"event_id" json:"event_id,omitempty"`
	Code    string     `db:"code" json:"code"`

	// GrantsFree distinguishes the two independent code kinds:
	//   false → ACCESS code (a per-guest passkey): unlocks a private event, but
	//           the guest pays the normal price.
	//   true  → FREE-BOOKING code (comp): waives payment to ₹0 (and unlocks).
	GrantsFree bool `db:"grants_free" json:"grants_free"`

	// DiscountPercent (1–99) makes this a PERCENTAGE-DISCOUNT code: the guest
	// pays the ticket total minus this share (and it unlocks, like any code).
	// Nil for access and free codes; never set together with GrantsFree.
	DiscountPercent *int `db:"discount_percent" json:"discount_percent,omitempty"`

	// Limits. Nil means unlimited on that axis.
	MaxRedemptions *int `db:"max_redemptions" json:"max_redemptions,omitempty"`
	TimesRedeemed  int  `db:"times_redeemed" json:"times_redeemed"`
	PerUserLimit   *int `db:"per_user_limit" json:"per_user_limit,omitempty"`

	// Validity window. Nil bounds mean open-ended on that side.
	ValidFrom  *time.Time `db:"valid_from" json:"valid_from,omitempty"`
	ValidUntil *time.Time `db:"valid_until" json:"valid_until,omitempty"`

	IsActive  bool      `db:"is_active" json:"is_active"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
	UpdatedAt time.Time `db:"updated_at" json:"updated_at"`
}

// DiscountCents is the amount a percentage-discount code takes off totalCents,
// floored so the guest never pays less than the exact percentage. Zero for
// access and free codes (free is handled by GrantsFree, not here).
func (c *Coupon) DiscountCents(totalCents int64) int64 {
	if c == nil || c.DiscountPercent == nil || totalCents <= 0 {
		return 0
	}
	return totalCents * int64(*c.DiscountPercent) / 100
}
