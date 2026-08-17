package models

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

type ScheduleType string

const (
	ScheduleTypeOneTime     ScheduleType = "one_time"
	ScheduleTypeRecurring   ScheduleType = "recurring"
	ScheduleTypeCustomDates ScheduleType = "custom_dates"
)

// SessionType distinguishes a normal group experience from a one-on-one
// booking calendar. A one-on-one event always has capacity 1 — SessionType
// exists so the public page, edit form and pause logic can recognise it without
// inferring intent from `capacity == 1`.
type SessionType string

const (
	SessionTypeGroup    SessionType = "group"
	SessionTypeOneOnOne SessionType = "one_on_one"
)

// SessionWindow is one continuous stretch of a day the host is available for
// one-on-one sessions (e.g. 10:00–12:00). Date and times are IST wall-clock,
// exactly as the host typed them; the expanded slot start times live in
// Event.CustomDates. Kept so the edit form can round-trip the host's input.
//
// A window is either dated (Date set — a one-off calendar) or weekly (Weekday
// set — recurring office hours that repeat every week). Exactly one of the two
// is populated, decided by the event's IsRecurring flag.
type SessionWindow struct {
	Date  string `json:"date"`  // "2006-01-02", one-off windows only
	Start string `json:"start"` // "15:04"
	End   string `json:"end"`   // "15:04"
	// Weekday is time.Weekday (0 = Sunday), set on recurring windows only.
	Weekday *int `json:"weekday,omitempty"`
}

// SessionWindows is a JSONB-backed slice of SessionWindow.
type SessionWindows []SessionWindow

func (w *SessionWindows) Scan(src interface{}) error {
	if src == nil {
		*w = nil
		return nil
	}
	var data []byte
	switch v := src.(type) {
	case []byte:
		data = v
	case string:
		data = []byte(v)
	default:
		return fmt.Errorf("session_windows: unsupported scan type %T", src)
	}
	if len(data) == 0 {
		*w = nil
		return nil
	}
	return json.Unmarshal(data, w)
}

func (w SessionWindows) Value() (driver.Value, error) {
	if w == nil {
		return []byte("[]"), nil
	}
	return json.Marshal(w)
}

// Event (Experience) is created by a host. Contains all listing details.
type Event struct {
	ID     uuid.UUID `db:"id" json:"id"`
	HostID uuid.UUID `db:"host_id" json:"host_id"`

	// Slug is the clean, URL-safe identifier used in public links
	// (/experience/{slug}). Generated once at create time; immutable.
	Slug string `db:"slug" json:"slug"`

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
	Languages pq.StringArray `db:"languages" json:"languages"`   // languages the experience is run in
	Level     *string        `db:"level" json:"level,omitempty"` // Beginner Friendly / Intermediate / Advanced

	// ── Schedule & Pricing ──────────────────────────────────────────────────
	ScheduleType   ScheduleType   `db:"schedule_type" json:"schedule_type"`
	PriceCents     *int64         `db:"price_cents" json:"price_cents,omitempty"` // per guest; nil = free
	IsFree         bool           `db:"is_free" json:"is_free"`
	Time           time.Time      `db:"time" json:"time"`
	EndTime        *time.Time     `db:"end_time" json:"end_time,omitempty"`
	IsRecurring    bool           `db:"is_recurring" json:"is_recurring"`
	RecurrenceRule *string        `db:"recurrence_rule" json:"recurrence_rule,omitempty"` // e.g. "FREQ=WEEKLY;BYDAY=MO"
	CustomDates    pq.StringArray `db:"custom_dates" json:"custom_dates"`                 // ISO timestamps for custom/dynamic date slots

	// ── One-on-one sessions ─────────────────────────────────────────────────
	// SessionType == one_on_one means Capacity is 1 and the sessions come from
	// SessionWindows, DurationMinutes apart with BreakMinutes between them.
	// Dated windows are expanded once into CustomDates; weekly windows (the
	// recurring flavour) are expanded on every read by lib/event.
	// GenerateWeeklySessions, so CustomDates stays empty and never expires.
	SessionType    SessionType    `db:"session_type" json:"session_type"`
	BreakMinutes   int            `db:"break_minutes" json:"break_minutes"`
	SessionWindows SessionWindows `db:"session_windows" json:"session_windows"`

	// ── Attendee details ────────────────────────────────────────────────────
	// When RequiresAttendeeDetails is true, guests must supply the fields listed
	// in AttendeeFields (keys from the fixed attendee-field catalog) at booking.
	RequiresAttendeeDetails bool           `db:"requires_attendee_details" json:"requires_attendee_details"`
	AttendeeFields          pq.StringArray `db:"attendee_fields" json:"attendee_fields"`

	// ── Privacy & Access ────────────────────────────────────────────────────
	// Private events stay LISTED in discovery (the UI shows a lock badge); the
	// AccessPasskey is required only at the Book step, not to view. When
	// PasskeyGrantsFree is true, entering the correct passkey also comps a paid
	// booking to zero (the host comps the guest — no wallet debit, no fee split).
	IsPrivate         bool    `db:"is_private" json:"is_private"`
	AccessPasskey     *string `db:"access_passkey" json:"access_passkey,omitempty"`
	PasskeyGrantsFree bool    `db:"passkey_grants_free" json:"passkey_grants_free"`
	// PrivateAccessMode picks WHICH gate a private event uses: a passkey the
	// guest types, or an approved join request. Meaningless when IsPrivate is
	// false. Unlike AccessPasskey this is NOT stripped from the public event
	// response — the booking page has to know which gate to render.
	PrivateAccessMode PrivateAccessMode `db:"private_access_mode" json:"private_access_mode"`

	// ── Policies ────────────────────────────────────────────────────────────
	CancellationPolicy *CancellationPolicy `db:"cancellation_policy" json:"cancellation_policy,omitempty"`
	// TermsAndConditions is free text set per experience and printed on the ticket PDF.
	TermsAndConditions *string `db:"terms_and_conditions" json:"terms_and_conditions,omitempty"`

	// ── Status ──────────────────────────────────────────────────────────────
	Status      EventStatus    `db:"status" json:"status"`
	PublishedAt *time.Time     `db:"published_at" json:"published_at,omitempty"`
	PausedAt    *time.Time     `db:"paused_at" json:"paused_at,omitempty"`
	PausedFrom  *time.Time     `db:"paused_from" json:"paused_from,omitempty"`
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

	// Named ticket tiers loaded from event_price_tiers (not a column). Empty
	// slice = single-price event driven by PriceCents.
	PriceTiers []EventPriceTier `json:"price_tiers"`

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
