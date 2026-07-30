package models

import (
	"time"

	"github.com/google/uuid"
)

// Coupon is a host-issued comp code. In v1 a coupon is always a FULL waiver — it
// makes a booking free (no partial discounts). Scope is a single event
// (EventID set) or every event the host runs (EventID nil).
type Coupon struct {
	ID     uuid.UUID  `db:"id" json:"id"`
	HostID uuid.UUID  `db:"host_id" json:"host_id"`
	// EventID scopes the coupon to one event; nil = all of the host's events.
	EventID *uuid.UUID `db:"event_id" json:"event_id,omitempty"`
	Code    string     `db:"code" json:"code"`

	// GrantsFree distinguishes the two independent code kinds:
	//   false → ACCESS code (a per-guest passkey): unlocks a private event, but
	//           the guest pays the normal price.
	//   true  → FREE-BOOKING code (comp): waives payment to ₹0 (and unlocks).
	GrantsFree bool `db:"grants_free" json:"grants_free"`

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
