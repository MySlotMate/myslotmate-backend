package models

import (
	"time"

	"github.com/google/uuid"
)

// EventPriceTier is a named price option on an event (e.g. General / VIP /
// Student). Events with no tiers fall back to Event.PriceCents. A booking picks
// one tier and snapshots its price onto the booking row.
type EventPriceTier struct {
	ID         uuid.UUID `db:"id" json:"id"`
	EventID    uuid.UUID `db:"event_id" json:"event_id"`
	Name       string    `db:"name" json:"name"`
	PriceCents int64     `db:"price_cents" json:"price_cents"`
	Capacity   *int      `db:"capacity" json:"capacity,omitempty"` // reserved; not enforced in v1
	SortOrder  int       `db:"sort_order" json:"sort_order"`
	IsActive   bool      `db:"is_active" json:"is_active"`
	CreatedAt  time.Time `db:"created_at" json:"created_at"`
	UpdatedAt  time.Time `db:"updated_at" json:"updated_at"`
}
