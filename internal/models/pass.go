package models

import (
	"time"

	"github.com/google/uuid"
)

// PassStatus is the life of a monthly pass. Only PassActive may reserve a
// session; the other two are terminal.
type PassStatus string

const (
	PassActive   PassStatus = "active"
	PassExpired  PassStatus = "expired"
	PassRefunded PassStatus = "refunded"
)

// PassValidityDays is how long a monthly pass lasts, counted from the moment it
// was bought — not a calendar month. Rolling is easier to explain to a guest
// and removes any pro-rating argument mid-month.
const PassValidityDays = 30

// UserPass is one guest's monthly pass for one event: paid once, covers every
// session (or SessionsIncluded of them) until ValidUntil.
//
// The money fields are snapshots taken at purchase. The event's pass price may
// change afterwards; what was paid, and what the host earned, must not.
type UserPass struct {
	ID      uuid.UUID `db:"id" json:"id"`
	EventID uuid.UUID `db:"event_id" json:"event_id"`
	UserID  uuid.UUID `db:"user_id" json:"user_id"`

	PriceCents      int64 `db:"price_cents" json:"price_cents"`
	ServiceFeeCents int64 `db:"service_fee_cents" json:"service_fee_cents"`
	NetEarningCents int64 `db:"net_earning_cents" json:"net_earning_cents"`

	// SessionsIncluded nil = every session inside the validity window. Serialised
	// explicitly as null (no omitempty): the client distinguishes "unlimited"
	// from "field missing", and an absent key reads as undefined there.
	SessionsIncluded *int `db:"sessions_included" json:"sessions_included"`
	SessionsUsed     int  `db:"sessions_used" json:"sessions_used"`

	ValidFrom  time.Time `db:"valid_from" json:"valid_from"`
	ValidUntil time.Time `db:"valid_until" json:"valid_until"`

	Status         PassStatus `db:"status" json:"status"`
	PaymentID      *uuid.UUID `db:"payment_id" json:"payment_id,omitempty"`
	IdempotencyKey *string    `db:"idempotency_key" json:"idempotency_key,omitempty"`
	CreatedAt      time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt      time.Time  `db:"updated_at" json:"updated_at"`
}

// SessionsRemaining reports how many more sessions this pass can reserve, and
// whether that number is meaningful (false = unlimited).
func (p *UserPass) SessionsRemaining() (int, bool) {
	if p.SessionsIncluded == nil {
		return 0, false
	}
	remaining := *p.SessionsIncluded - p.SessionsUsed
	if remaining < 0 {
		remaining = 0
	}
	return remaining, true
}

// IsUsable reports whether the pass can cover a session happening at t.
func (p *UserPass) IsUsable(t time.Time) bool {
	if p.Status != PassActive {
		return false
	}
	if t.Before(p.ValidFrom) || t.After(p.ValidUntil) {
		return false
	}
	if remaining, limited := p.SessionsRemaining(); limited && remaining == 0 {
		return false
	}
	return true
}

// PassRefundWindow is how long after purchase a guest may cancel a pass for a
// full refund. After it the pass is final — that is the point of paying up
// front.
//
// The window alone is not the whole test: buying a pass books every covered
// session immediately, so SessionsUsed is above zero from the start and cannot
// stand in for "unused". Whether a covered session has actually happened (or
// the guest was checked in) is decided against the bookings themselves, in
// passService.CancelPass.
const PassRefundWindow = 48 * time.Hour

// IsRefundable reports whether the pass is still inside its refund window.
func (p *UserPass) IsRefundable(now time.Time) bool {
	return p.Status == PassActive && now.Sub(p.CreatedAt) <= PassRefundWindow
}

// PassHolder is a UserPass enriched with the guest's display fields, for the
// host's "pass holders" roster.
type PassHolder struct {
	UserPass
	UserName  string `db:"user_name" json:"user_name"`
	UserEmail string `db:"user_email" json:"user_email"`
	UserPhone string `db:"user_phone" json:"user_phone"`
}
