package models

import (
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Event (Experience) is created by a host. Contains all listing details.
type Event struct {
	ID     uuid.UUID `db:"id" json:"id"`
	HostID uuid.UUID `db:"host_id" json:"host_id"`

	// ── The Basics ──────────────────────────────────────────────────────────
	Title       string     `db:"title" json:"title"`
	HookLine    *string    `db:"hook_line" json:"hook_line,omitempty"`
	Mood        *EventMood `db:"mood" json:"mood,omitempty"`
	Description *string    `db:"description" json:"description,omitempty"`

	// ── Visuals ─────────────────────────────────────────────────────────────
	CoverImageURL *string        `db:"cover_image_url" json:"cover_image_url,omitempty"`
	GalleryURLs   pq.StringArray `db:"gallery_urls" json:"gallery_urls"`

	// ── Logistics ───────────────────────────────────────────────────────────
	IsOnline        bool     `db:"is_online" json:"is_online"`
	MeetingLink     *string  `db:"meeting_link" json:"meeting_link,omitempty"` // for online events (zoom, teams, google meet, etc.)
	Location        *string  `db:"location" json:"location,omitempty"`         // address/landmark
	LocationLat     *float64 `db:"location_lat" json:"location_lat,omitempty"`
	LocationLng     *float64 `db:"location_lng" json:"location_lng,omitempty"`
	GoogleMapsURL   *string  `db:"google_maps_url" json:"google_maps_url,omitempty"` // direct link to Google Maps location
	DurationMinutes *int     `db:"duration_minutes" json:"duration_minutes,omitempty"`
	MinGroupSize    *int     `db:"min_group_size" json:"min_group_size,omitempty"`
	MaxGroupSize    *int     `db:"max_group_size" json:"max_group_size,omitempty"`
	Capacity        int      `db:"capacity" json:"capacity"` // kept for overbooking prevention

	// ── Audience ────────────────────────────────────────────────────────────
	Languages pq.StringArray `db:"languages" json:"languages"`     // languages the experience is run in
	Level     *string        `db:"level" json:"level,omitempty"` // Beginner Friendly / Intermediate / Advanced

	// ── Schedule & Pricing ──────────────────────────────────────────────────
	PriceCents     *int64     `db:"price_cents" json:"price_cents,omitempty"` // per guest; nil = free
	IsFree         bool       `db:"is_free" json:"is_free"`
	Time           time.Time  `db:"time" json:"time"`
	EndTime        *time.Time `db:"end_time" json:"end_time,omitempty"`
	IsRecurring    bool       `db:"is_recurring" json:"is_recurring"`
	RecurrenceRule *string    `db:"recurrence_rule" json:"recurrence_rule,omitempty"` // e.g. "FREQ=WEEKLY;BYDAY=MO"

	// ── Policies ────────────────────────────────────────────────────────────
	CancellationPolicy *CancellationPolicy `db:"cancellation_policy" json:"cancellation_policy,omitempty"`

	// ── Status ──────────────────────────────────────────────────────────────
	Status      EventStatus `db:"status" json:"status"`
	PublishedAt *time.Time  `db:"published_at" json:"published_at,omitempty"`
	PausedAt    *time.Time  `db:"paused_at" json:"paused_at,omitempty"`
	PausedFrom  *time.Time  `db:"paused_from" json:"paused_from,omitempty"`
	PausedDates pq.StringArray `db:"paused_dates" json:"paused_dates,omitempty"`

	// ── AI ───────────────────────────────────────────────────────────────────
	AISuggestion *string `db:"ai_suggestion" json:"ai_suggestion,omitempty"`

	// ── Aggregate stats (denormalized) ──────────────────────────────────────
	AvgRating     *float64 `db:"avg_rating" json:"avg_rating,omitempty"`
	TotalBookings int      `db:"total_bookings" json:"total_bookings"`
	TotalReviews  int      `db:"total_reviews" json:"total_reviews"`
	
	// Calculated fields (not in DB)
	NextAvailableDate *time.Time `json:"next_available_date,omitempty"`
	BookingsLastWeek  int        `json:"bookings_last_week,omitempty"`

	CreatedAt time.Time `db:"created_at" json:"created_at"`
	UpdatedAt time.Time `db:"updated_at" json:"updated_at"`
}

type OccurrenceAvailability struct {
	Date          time.Time `json:"date"`
	TotalBooked   int       `json:"total_booked"`
	Capacity      int       `json:"capacity"`
	Remaining     int       `json:"remaining"`
	IsFullyBooked bool      `json:"is_fully_booked"`
	IsPaused      bool      `json:"is_paused,omitempty"`
}
