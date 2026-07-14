package models

import (
	"time"

	"github.com/google/uuid"
)

// Attendee is a Booking enriched with the booking user's display fields.
// Used by host-facing attendee/bookings views so the UI can show names
// instead of opaque user IDs.
type Attendee struct {
	Booking
	UserName      string  `db:"user_name" json:"user_name"`
	UserEmail     string  `db:"user_email" json:"user_email"`
	UserAvatarURL *string `db:"user_avatar_url" json:"user_avatar_url,omitempty"`
	// AttendeeProfile is the guest's submitted attendee details (name, age,
	// Govt-ID URL, etc.), attached for the host roster. Nil when none saved.
	AttendeeProfile *AttendeeProfile `json:"attendee_profile,omitempty"`
}

// Booking links a user to an event with quantity and status.
type Booking struct {
	ID                               uuid.UUID     `db:"id" json:"id"`
	EventID                          uuid.UUID     `db:"event_id" json:"event_id"`
	UserID                           uuid.UUID     `db:"user_id" json:"user_id"`
	OccurrenceDate                   time.Time     `db:"occurrence_date" json:"occurrence_date"`
	Quantity                         int           `db:"quantity" json:"quantity"`
	Status                           BookingStatus `db:"status" json:"status"`
	PaymentID                        *uuid.UUID    `db:"payment_id" json:"payment_id,omitempty"`
	IdempotencyKey                   *string       `db:"idempotency_key" json:"idempotency_key,omitempty"`
	AmountCents                      *int64        `db:"amount_cents" json:"amount_cents,omitempty"`           // total booking value
	ServiceFeeCents                  *int64        `db:"service_fee_cents" json:"service_fee_cents,omitempty"` // platform fee (15%)
	NetEarningCents                  *int64        `db:"net_earning_cents" json:"net_earning_cents,omitempty"` // host net (85%)
	PriceTierID                      *uuid.UUID    `db:"price_tier_id" json:"price_tier_id,omitempty"`         // chosen ticket tier (nil = single-price/free)
	UnitPriceCents                   *int64        `db:"unit_price_cents" json:"unit_price_cents,omitempty"`   // per-ticket price snapshot at booking time
	CreatedAt                        time.Time     `db:"created_at" json:"created_at"`
	UpdatedAt                        time.Time     `db:"updated_at" json:"updated_at"`
	CancelledAt                      *time.Time    `db:"cancelled_at" json:"cancelled_at,omitempty"`
	NotificationSentWhatsapp         bool          `db:"notification_sent_whatsapp" json:"notification_sent_whatsapp"`
	ReminderNotificationSentEmail    bool          `db:"reminder_notification_sent_email" json:"reminder_notification_sent_email"`
	ReminderNotificationSentAt       *time.Time    `db:"reminder_notification_sent_at" json:"reminder_notification_sent_at,omitempty"`
	NotificationSentEmail            bool          `db:"notification_sent_email" json:"notification_sent_email"`
	ReminderNotificationSentWhatsapp bool          `db:"reminder_notification_sent_whatsapp" json:"reminder_notification_sent_whatsapp"`
	ReminderWhatsappSentAt           *time.Time    `db:"reminder_whatsapp_sent_at" json:"reminder_whatsapp_sent_at,omitempty"`
}
