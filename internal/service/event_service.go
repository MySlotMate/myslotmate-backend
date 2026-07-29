package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"myslotmate-backend/internal/lib/event"
	"myslotmate-backend/internal/lib/slug"
	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/teambition/rrule-go"
)

type EventService interface {
	CreateEvent(ctx context.Context, hostID uuid.UUID, req EventCreateRequest) (*models.Event, error)
	UpdateEvent(ctx context.Context, eventID uuid.UUID, hostID uuid.UUID, req EventUpdateRequest) (*models.Event, error)
	DeleteEvent(ctx context.Context, eventID uuid.UUID, hostID uuid.UUID) error
	// CancelEvent soft-cancels an event: marks status=cancelled, refunds every
	// upcoming confirmed booking via F4 (CancelBookingByHost), and leaves the
	// event row in place for history. Returns the updated event.
	CancelEvent(ctx context.Context, eventID uuid.UUID, hostID uuid.UUID) (*models.Event, error)
	GetEvent(ctx context.Context, eventID uuid.UUID) (*models.Event, error)
	// GetEventBySlugOrID resolves a public route param that may be either a
	// clean slug or a raw UUID (so old /experience/{uuid} links keep working).
	GetEventBySlugOrID(ctx context.Context, param string) (*models.Event, error)
	// IsSlugAvailable reports whether a slug is free to use, ignoring the event
	// identified by excludeID (nil to check globally). The input is slugified
	// first so the caller checks the same value that would be stored.
	IsSlugAvailable(ctx context.Context, rawSlug string, excludeID *uuid.UUID) (available bool, normalized string, err error)
	GetHostEvents(ctx context.Context, hostID uuid.UUID) ([]*models.Event, error)
	GetHostEventsFiltered(ctx context.Context, hostID uuid.UUID, status *models.EventStatus, search string, sortBy string, limit, offset int) ([]*models.Event, error)
	GetCalendarEvents(ctx context.Context, hostID uuid.UUID, start, end time.Time) ([]*models.Event, error)
	GetTodaySchedule(ctx context.Context, hostID uuid.UUID) ([]*models.Event, error)
	PublishEvent(ctx context.Context, eventID uuid.UUID, hostID uuid.UUID) (*models.Event, error)
	PauseEvent(ctx context.Context, eventID uuid.UUID, hostID uuid.UUID, pausedFrom *time.Time, pausedDate *time.Time) (*models.Event, error)
	ResumeEvent(ctx context.Context, eventID uuid.UUID, hostID uuid.UUID) (*models.Event, error)
	GetEventAttendees(ctx context.Context, eventID uuid.UUID, occurrenceDate *time.Time) ([]*models.Attendee, error)
	ListPublishedEvents(ctx context.Context, limit, offset int) ([]*models.Event, error)
	GetEventAvailability(ctx context.Context, eventID uuid.UUID) ([]models.OccurrenceAvailability, error)
	GetEventOccurrencesForHost(ctx context.Context, eventID uuid.UUID, hostID uuid.UUID) ([]models.OccurrenceAvailability, error)
}

// PriceTierInput is a single ticket tier supplied on event create/update.
type PriceTierInput struct {
	Name       string `json:"name"`
	PriceCents int64  `json:"price_cents"`
	Capacity   *int   `json:"capacity,omitempty"`
	SortOrder  int    `json:"sort_order,omitempty"`
}

type EventCreateRequest struct {
	Title              string                     `json:"title"`
	HookLine           *string                    `json:"hook_line,omitempty"`
	Mood               *models.EventMood          `json:"mood,omitempty"`
	Description        *string                    `json:"description,omitempty"`
	CoverImageURL      *string                    `json:"cover_image_url,omitempty"`
	GalleryURLs        []string                   `json:"gallery_urls,omitempty"`
	IsOnline           bool                       `json:"is_online"`
	MeetingLink        *string                    `json:"meeting_link,omitempty"` // for online events
	Location           *string                    `json:"location,omitempty"`
	LocationLat        *float64                   `json:"location_lat,omitempty"`
	LocationLng        *float64                   `json:"location_lng,omitempty"`
	GoogleMapsURL      *string                    `json:"google_maps_url,omitempty"` // for location-based events
	DurationMinutes    *int                       `json:"duration_minutes,omitempty"`
	MinGroupSize       *int                       `json:"min_group_size,omitempty"`
	MaxGroupSize       *int                       `json:"max_group_size,omitempty"`
	Capacity           int                        `json:"capacity"`
	Languages          []string                   `json:"languages,omitempty"`
	Level              *string                    `json:"level,omitempty"`
	PriceCents         *int64                     `json:"price_cents,omitempty"`
	IsFree             bool                       `json:"is_free"`
	Time               time.Time                  `json:"time"`
	EndTime            *time.Time                 `json:"end_time,omitempty"`
	IsRecurring        bool                       `json:"is_recurring"`
	RecurrenceRule     *string                    `json:"recurrence_rule,omitempty"`
	CancellationPolicy *models.CancellationPolicy `json:"cancellation_policy,omitempty"`
	Status             models.EventStatus         `json:"status"` // draft or live
	AISuggestion       *string                    `json:"ai_suggestion,omitempty"`
	PriceTiers         []PriceTierInput           `json:"price_tiers,omitempty"` // named ticket tiers; empty = single-price via PriceCents

	RequiresAttendeeDetails bool     `json:"requires_attendee_details"`
	AttendeeFields          []string `json:"attendee_fields,omitempty"`
	TermsAndConditions      *string  `json:"terms_and_conditions,omitempty"`

	IsPrivate         bool    `json:"is_private"`
	AccessPasskey     *string `json:"access_passkey,omitempty"`
	PasskeyGrantsFree bool    `json:"passkey_grants_free"`
}

type EventUpdateRequest struct {
	Title              *string                    `json:"title,omitempty"`
	Slug               *string                    `json:"slug,omitempty"` // optional; when set and changed, updates the event's URL slug
	HookLine           *string                    `json:"hook_line,omitempty"`
	Mood               *models.EventMood          `json:"mood,omitempty"`
	Description        *string                    `json:"description,omitempty"`
	CoverImageURL      *string                    `json:"cover_image_url,omitempty"`
	GalleryURLs        []string                   `json:"gallery_urls,omitempty"`
	IsOnline           *bool                      `json:"is_online,omitempty"`
	MeetingLink        *string                    `json:"meeting_link,omitempty"` // for online events
	Location           *string                    `json:"location,omitempty"`
	LocationLat        *float64                   `json:"location_lat,omitempty"`
	LocationLng        *float64                   `json:"location_lng,omitempty"`
	GoogleMapsURL      *string                    `json:"google_maps_url,omitempty"` // for location-based events
	DurationMinutes    *int                       `json:"duration_minutes,omitempty"`
	MinGroupSize       *int                       `json:"min_group_size,omitempty"`
	MaxGroupSize       *int                       `json:"max_group_size,omitempty"`
	Capacity           *int                       `json:"capacity,omitempty"`
	Languages          []string                   `json:"languages,omitempty"`
	Level              *string                    `json:"level,omitempty"`
	PriceCents         *int64                     `json:"price_cents,omitempty"`
	IsFree             *bool                      `json:"is_free,omitempty"`
	Time               *time.Time                 `json:"time,omitempty"`
	EndTime            *time.Time                 `json:"end_time,omitempty"`
	IsRecurring        *bool                      `json:"is_recurring,omitempty"`
	RecurrenceRule     *string                    `json:"recurrence_rule,omitempty"`
	CancellationPolicy *models.CancellationPolicy `json:"cancellation_policy,omitempty"`
	PriceTiers         []PriceTierInput           `json:"price_tiers,omitempty"` // when non-nil, replaces the event's tier set

	RequiresAttendeeDetails *bool    `json:"requires_attendee_details,omitempty"`
	AttendeeFields          []string `json:"attendee_fields,omitempty"`
	TermsAndConditions      *string  `json:"terms_and_conditions,omitempty"`

	IsPrivate         *bool   `json:"is_private,omitempty"`
	AccessPasskey     *string `json:"access_passkey,omitempty"`
	PasskeyGrantsFree *bool   `json:"passkey_grants_free,omitempty"`
}

type eventService struct {
	eventRepo      repository.EventRepository
	bookingRepo    repository.BookingRepository
	accountRepo    repository.AccountRepository
	ledgerRepo     repository.TransactionLedgerRepository
	tierRepo       repository.EventPriceTierRepository
	attendeeRepo   repository.AttendeeProfileRepository
	dispatcher     *event.Dispatcher
	bookingService BookingService
}

var ErrInvalidEventMood = errors.New("invalid event mood")

// ErrSlugTaken is returned when an admin-supplied slug is already used by
// another event.
var ErrSlugTaken = errors.New("slug already in use")

func NewEventService(
	er repository.EventRepository,
	br repository.BookingRepository,
	ar repository.AccountRepository,
	lr repository.TransactionLedgerRepository,
	tr repository.EventPriceTierRepository,
	apr repository.AttendeeProfileRepository,
	d *event.Dispatcher,
	bs BookingService,
) EventService {
	return &eventService{
		eventRepo:      er,
		bookingRepo:    br,
		accountRepo:    ar,
		ledgerRepo:     lr,
		tierRepo:       tr,
		attendeeRepo:   apr,
		dispatcher:     d,
		bookingService: bs,
	}
}

func normalizeEventMood(mood *models.EventMood) (*models.EventMood, error) {
	canonical, err := models.NormalizeEventMood(mood)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidEventMood, err)
	}
	return canonical, nil
}

func (s *eventService) CreateEvent(ctx context.Context, hostID uuid.UUID, req EventCreateRequest) (*models.Event, error) {
	normalizedMood, err := normalizeEventMood(req.Mood)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	// A new event may only be created as a draft or live. Paused/cancelled are
	// lifecycle states reached through their own endpoints, never at creation.
	status := req.Status
	switch status {
	case "":
		status = models.EventStatusDraft
	case models.EventStatusDraft, models.EventStatusLive:
		// ok
	default:
		return nil, fmt.Errorf("invalid status %q: a new event must be draft or live", status)
	}

	newEvent := &models.Event{
		ID:                 uuid.New(),
		HostID:             hostID,
		Title:              req.Title,
		HookLine:           req.HookLine,
		Mood:               normalizedMood,
		Description:        req.Description,
		CoverImageURL:      req.CoverImageURL,
		GalleryURLs:        pq.StringArray(req.GalleryURLs),
		IsOnline:           req.IsOnline,
		MeetingLink:        req.MeetingLink,
		Location:           req.Location,
		LocationLat:        req.LocationLat,
		LocationLng:        req.LocationLng,
		GoogleMapsURL:      req.GoogleMapsURL,
		DurationMinutes:    req.DurationMinutes,
		MinGroupSize:       req.MinGroupSize,
		MaxGroupSize:       req.MaxGroupSize,
		Capacity:           req.Capacity,
		Languages:          pq.StringArray(req.Languages),
		Level:              req.Level,
		PriceCents:         req.PriceCents,
		IsFree:             req.IsFree,
		Time:               req.Time,
		EndTime:            req.EndTime,
		IsRecurring:        req.IsRecurring,
		RecurrenceRule:     req.RecurrenceRule,
		CancellationPolicy: req.CancellationPolicy,
		Status:             status,
		AISuggestion:       req.AISuggestion,
		CreatedAt:          now,
		UpdatedAt:          now,

		RequiresAttendeeDetails: req.RequiresAttendeeDetails,
		AttendeeFields:          pq.StringArray(req.AttendeeFields),
		TermsAndConditions:      req.TermsAndConditions,

		IsPrivate:         req.IsPrivate,
		AccessPasskey:     normalizePasskey(req.AccessPasskey),
		PasskeyGrantsFree: req.PasskeyGrantsFree,
	}

	if status == models.EventStatusLive {
		newEvent.PublishedAt = &now
	}

	// Generate a clean, unique slug once at create time. It is never
	// regenerated on update, so shared /experience/{slug} links stay valid.
	newEvent.Slug, err = slug.Disambiguate(slug.Make(req.Title, "event"), func(candidate string) (bool, error) {
		return s.eventRepo.SlugExists(ctx, candidate)
	})
	if err != nil {
		return nil, err
	}

	if err := s.eventRepo.Create(ctx, newEvent); err != nil {
		return nil, err
	}

	if len(req.PriceTiers) > 0 {
		if err := s.tierRepo.ReplaceForEvent(ctx, newEvent.ID, tierInputsToModels(req.PriceTiers)); err != nil {
			return nil, err
		}
		tiers, err := s.tierRepo.ListByEventID(ctx, newEvent.ID)
		if err != nil {
			return nil, err
		}
		newEvent.PriceTiers = tiers
	} else {
		newEvent.PriceTiers = []models.EventPriceTier{}
	}

	s.dispatcher.Publish(event.EventCreated, newEvent)

	return newEvent, nil
}

// tierInputsToModels converts API tier inputs into tier models for persistence.
// normalizePasskey trims a supplied passkey; an empty (or whitespace-only) value
// becomes nil so a private event never carries a blank, un-typeable passkey.
func normalizePasskey(p *string) *string {
	if p == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*p)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

func tierInputsToModels(inputs []PriceTierInput) []models.EventPriceTier {
	tiers := make([]models.EventPriceTier, 0, len(inputs))
	for i, in := range inputs {
		tiers = append(tiers, models.EventPriceTier{
			Name:       in.Name,
			PriceCents: in.PriceCents,
			Capacity:   in.Capacity,
			SortOrder:  i,
		})
	}
	return tiers
}

func (s *eventService) UpdateEvent(ctx context.Context, eventID uuid.UUID, hostID uuid.UUID, req EventUpdateRequest) (*models.Event, error) {
	evt, err := s.eventRepo.GetByID(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if evt == nil {
		return nil, errors.New("event not found")
	}
	if evt.HostID != hostID {
		return nil, errors.New("unauthorized: you do not own this event")
	}

	if req.Title != nil {
		evt.Title = *req.Title
	}
	// Admins may override the URL slug. A blank value keeps the current slug.
	// A changed value is slugified and rejected if it collides with another
	// event (the event's own current slug never counts as a conflict).
	if req.Slug != nil {
		desired := slug.Make(*req.Slug, evt.Slug)
		if desired != evt.Slug {
			taken, err := s.eventRepo.SlugExistsExcluding(ctx, desired, evt.ID)
			if err != nil {
				return nil, err
			}
			if taken {
				return nil, ErrSlugTaken
			}
			evt.Slug = desired
		}
	}
	if req.HookLine != nil {
		evt.HookLine = req.HookLine
	}
	if req.Mood != nil {
		normalizedMood, err := normalizeEventMood(req.Mood)
		if err != nil {
			return nil, err
		}
		evt.Mood = normalizedMood
	}
	if req.Description != nil {
		evt.Description = req.Description
	}
	if req.CoverImageURL != nil {
		evt.CoverImageURL = req.CoverImageURL
	}
	if req.GalleryURLs != nil {
		evt.GalleryURLs = pq.StringArray(req.GalleryURLs)
	}
	if req.IsOnline != nil {
		evt.IsOnline = *req.IsOnline
	}
	if req.MeetingLink != nil {
		evt.MeetingLink = req.MeetingLink
	}
	if req.Location != nil {
		evt.Location = req.Location
	}
	if req.LocationLat != nil {
		evt.LocationLat = req.LocationLat
	}
	if req.LocationLng != nil {
		evt.LocationLng = req.LocationLng
	}
	if req.GoogleMapsURL != nil {
		evt.GoogleMapsURL = req.GoogleMapsURL
	}
	if req.DurationMinutes != nil {
		evt.DurationMinutes = req.DurationMinutes
	}
	if req.MinGroupSize != nil {
		evt.MinGroupSize = req.MinGroupSize
	}
	if req.MaxGroupSize != nil {
		evt.MaxGroupSize = req.MaxGroupSize
	}
	if req.Capacity != nil {
		evt.Capacity = *req.Capacity
	}
	if req.Languages != nil {
		evt.Languages = pq.StringArray(req.Languages)
	}
	if req.Level != nil {
		evt.Level = req.Level
	}
	if req.PriceCents != nil {
		evt.PriceCents = req.PriceCents
	}
	if req.IsFree != nil {
		evt.IsFree = *req.IsFree
	}
	if req.Time != nil {
		evt.Time = *req.Time
	}
	if req.EndTime != nil {
		evt.EndTime = req.EndTime
	}
	if req.IsRecurring != nil {
		evt.IsRecurring = *req.IsRecurring
	}
	if req.RecurrenceRule != nil {
		evt.RecurrenceRule = req.RecurrenceRule
	}
	if req.CancellationPolicy != nil {
		evt.CancellationPolicy = req.CancellationPolicy
	}
	if req.RequiresAttendeeDetails != nil {
		evt.RequiresAttendeeDetails = *req.RequiresAttendeeDetails
	}
	if req.AttendeeFields != nil {
		evt.AttendeeFields = pq.StringArray(req.AttendeeFields)
	}
	if req.TermsAndConditions != nil {
		evt.TermsAndConditions = req.TermsAndConditions
	}
	if req.IsPrivate != nil {
		evt.IsPrivate = *req.IsPrivate
	}
	// A nil AccessPasskey keeps the current passkey (so the host can toggle other
	// fields without re-entering it); a supplied value replaces it. An explicit
	// empty string clears it.
	if req.AccessPasskey != nil {
		evt.AccessPasskey = normalizePasskey(req.AccessPasskey)
	}
	if req.PasskeyGrantsFree != nil {
		evt.PasskeyGrantsFree = *req.PasskeyGrantsFree
	}

	if err := s.eventRepo.Update(ctx, evt); err != nil {
		return nil, err
	}

	// When PriceTiers is supplied, replace the event's tier set. A non-nil empty
	// slice clears tiers (revert to single-price); nil leaves tiers untouched.
	if req.PriceTiers != nil {
		if err := s.tierRepo.ReplaceForEvent(ctx, evt.ID, tierInputsToModels(req.PriceTiers)); err != nil {
			return nil, err
		}
	}
	tiers, err := s.tierRepo.ListByEventID(ctx, evt.ID)
	if err != nil {
		return nil, err
	}
	evt.PriceTiers = tiers

	return evt, nil
}

// DeleteEvent hard-deletes an event. Allowed only when there are no active
// (pending/confirmed) bookings left — the host must CancelEvent first to
// refund those, OR wait until every booking is in a terminal state. This
// guard prevents the previous behavior where deleting an event with confirmed
// bookings ran an ad-hoc refund loop that bypassed F4 — that path was
// broken: it skipped host_earnings decrement, missed the host/platform
// cancellation_debit ledger entries, didn't reverse the booking payment,
// AND refunded past attendees too.
func (s *eventService) DeleteEvent(ctx context.Context, eventID uuid.UUID, hostID uuid.UUID) error {
	evt, err := s.eventRepo.GetByID(ctx, eventID)
	if err != nil {
		return err
	}
	if evt == nil {
		return errors.New("event not found")
	}
	if evt.HostID != hostID {
		return errors.New("unauthorized: you do not own this event")
	}

	bookings, err := s.bookingRepo.ListByEventID(ctx, eventID)
	if err != nil {
		return fmt.Errorf("failed to fetch bookings: %w", err)
	}
	var active int
	for _, b := range bookings {
		if b.Status == models.BookingStatusPending || b.Status == models.BookingStatusConfirmed {
			active++
		}
	}
	if active > 0 {
		return fmt.Errorf("cannot delete event: %d active booking(s) — cancel the event first so attendees are refunded, then delete it", active)
	}

	if err := s.eventRepo.Delete(ctx, eventID); err != nil {
		return err
	}
	s.dispatcher.Publish(event.EventDeleted, evt)
	fmt.Printf("[EVENT] Event %s deleted (no active bookings)\n", eventID)
	return nil
}

// CancelEvent marks the event cancelled and refunds every still-active booking
// via the F4 path (CancelBookingByHost). Past confirmed bookings are LEFT AS
// IS — those attendees already attended; refunding them would create money
// out of thin air. Future / pending bookings get the full F4 treatment: user
// wallet credited, host_earnings decremented, host + platform cancellation_debit
// ledger entries written, original booking payment reversed.
func (s *eventService) CancelEvent(ctx context.Context, eventID uuid.UUID, hostID uuid.UUID) (*models.Event, error) {
	evt, err := s.eventRepo.GetByID(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if evt == nil {
		return nil, errors.New("event not found")
	}
	if evt.HostID != hostID {
		return nil, errors.New("unauthorized: you do not own this event")
	}

	bookings, err := s.bookingRepo.ListByEventID(ctx, eventID)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch bookings: %w", err)
	}

	now := time.Now()
	var refunded, skippedPast, errs int
	for _, b := range bookings {
		if b.Status != models.BookingStatusPending && b.Status != models.BookingStatusConfirmed {
			continue
		}
		// Past confirmed bookings — attendees already showed up. Do NOT refund.
		if b.OccurrenceDate.Before(now) {
			skippedPast++
			fmt.Printf("[EVENT CANCEL] booking %s skipped (event occurrence %s is in the past)\n", b.ID, b.OccurrenceDate)
			continue
		}
		if _, err := s.bookingService.CancelBookingByHost(ctx, b.ID); err != nil {
			errs++
			fmt.Printf("[EVENT CANCEL] booking %s refund failed: %v\n", b.ID, err)
			continue
		}
		refunded++
	}
	if errs > 0 {
		// Surface as error so the host knows some refunds didn't go through.
		// They can retry the cancel — the F4 idempotency keys make it safe.
		return nil, fmt.Errorf("event cancel: %d refund(s) succeeded, %d failed; retry the cancel — F4 is idempotent", refunded, errs)
	}

	if err := s.eventRepo.UpdateStatus(ctx, eventID, models.EventStatusCancelled); err != nil {
		return nil, fmt.Errorf("failed to mark event cancelled: %w", err)
	}
	evt.Status = models.EventStatusCancelled
	s.dispatcher.Publish(event.EventCancelled, evt)
	fmt.Printf("[EVENT CANCEL] event=%s refunded=%d skipped_past=%d\n", eventID, refunded, skippedPast)
	return evt, nil
}

// attachTiers loads and attaches the active ticket tiers for a single event.
func (s *eventService) attachTiers(ctx context.Context, evt *models.Event) error {
	if evt == nil {
		return nil
	}
	tiers, err := s.tierRepo.ListByEventID(ctx, evt.ID)
	if err != nil {
		return err
	}
	evt.PriceTiers = tiers
	return nil
}

// attachTiersBatch loads tiers for many events in one query (avoids N+1).
func (s *eventService) attachTiersBatch(ctx context.Context, events []*models.Event) error {
	if len(events) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(events))
	for _, e := range events {
		ids = append(ids, e.ID)
	}
	byEvent, err := s.tierRepo.ListByEventIDs(ctx, ids)
	if err != nil {
		return err
	}
	for _, e := range events {
		if tiers, ok := byEvent[e.ID]; ok {
			e.PriceTiers = tiers
		} else {
			e.PriceTiers = []models.EventPriceTier{}
		}
	}
	return nil
}

func (s *eventService) GetEvent(ctx context.Context, eventID uuid.UUID) (*models.Event, error) {
	evt, err := s.eventRepo.GetByID(ctx, eventID)
	if err != nil || evt == nil {
		return evt, err
	}
	return s.enrichEvent(ctx, evt)
}

func (s *eventService) IsSlugAvailable(ctx context.Context, rawSlug string, excludeID *uuid.UUID) (bool, string, error) {
	normalized := slug.Make(rawSlug, "")
	if normalized == "" {
		return false, "", nil
	}
	var taken bool
	var err error
	if excludeID != nil {
		taken, err = s.eventRepo.SlugExistsExcluding(ctx, normalized, *excludeID)
	} else {
		taken, err = s.eventRepo.SlugExists(ctx, normalized)
	}
	if err != nil {
		return false, normalized, err
	}
	return !taken, normalized, nil
}

func (s *eventService) GetEventBySlugOrID(ctx context.Context, param string) (*models.Event, error) {
	if id, err := uuid.Parse(param); err == nil {
		return s.GetEvent(ctx, id)
	}
	evt, err := s.eventRepo.GetBySlug(ctx, param)
	if err != nil || evt == nil {
		return evt, err
	}
	return s.enrichEvent(ctx, evt)
}

// enrichEvent attaches derived fields (weekly booking count, price tiers) to a
// freshly loaded event. Shared by the id and slug lookup paths.
func (s *eventService) enrichEvent(ctx context.Context, evt *models.Event) (*models.Event, error) {
	weekAgo := time.Now().AddDate(0, 0, -7)
	if count, err := s.bookingRepo.GetBookedQuantitySince(ctx, evt.ID, weekAgo); err == nil {
		evt.BookingsLastWeek = count
	} else {
		fmt.Printf("[EVENT_SERVICE] Error fetching weekly bookings for %s: %v\n", evt.ID, err)
	}

	if err := s.attachTiers(ctx, evt); err != nil {
		return nil, err
	}

	return evt, nil
}

func (s *eventService) GetHostEvents(ctx context.Context, hostID uuid.UUID) ([]*models.Event, error) {
	events, err := s.eventRepo.ListByHostID(ctx, hostID)
	if err != nil {
		return nil, err
	}
	if err := s.attachTiersBatch(ctx, events); err != nil {
		return nil, err
	}
	return events, nil
}

func (s *eventService) GetHostEventsFiltered(ctx context.Context, hostID uuid.UUID, status *models.EventStatus, search string, sortBy string, limit, offset int) ([]*models.Event, error) {
	events, err := s.eventRepo.ListByHostIDFiltered(ctx, hostID, status, search, sortBy, limit, offset)
	if err != nil {
		return nil, err
	}
	if err := s.attachTiersBatch(ctx, events); err != nil {
		return nil, err
	}
	return events, nil
}

func normalizeRRule(rule string) string {
	rule = strings.TrimSpace(rule)
	if rule == "" {
		return ""
	}
	upper := strings.ToUpper(rule)
	if strings.Contains(upper, "FREQ=") {
		return rule
	}
	switch strings.ToLower(rule) {
	case "daily":
		return "FREQ=DAILY"
	case "weekly":
		return "FREQ=WEEKLY"
	case "monthly":
		return "FREQ=MONTHLY"
	case "biweekly":
		return "FREQ=WEEKLY;INTERVAL=2"
	}
	return rule
}

func (s *eventService) GetCalendarEvents(ctx context.Context, hostID uuid.UUID, start, end time.Time) ([]*models.Event, error) {
	events, err := s.eventRepo.ListByHostID(ctx, hostID)
	if err != nil {
		return nil, err
	}

	var result []*models.Event
	for _, e := range events {
		if !e.IsRecurring || e.RecurrenceRule == nil || *e.RecurrenceRule == "" {
			if (e.Time.After(start) || e.Time.Equal(start)) && e.Time.Before(end) {
				result = append(result, e)
			}
			continue
		}

		ruleStr := normalizeRRule(*e.RecurrenceRule)
		rule, err := rrule.StrToRRule(ruleStr)
		if err != nil {
			fmt.Printf("[EVENT_SERVICE] Invalid recurrence rule for event %s: %v\n", e.ID, err)
			// fallback: check if base time is in range
			if (e.Time.After(start) || e.Time.Equal(start)) && e.Time.Before(end) {
				result = append(result, e)
			}
			continue
		}
		rule.DTStart(e.Time)

		occurrences := rule.Between(start, end, true)
		for _, occ := range occurrences {
			instance := *e
			instance.Time = occ
			if e.EndTime != nil {
				duration := e.EndTime.Sub(e.Time)
				newEnd := occ.Add(duration)
				instance.EndTime = &newEnd
			}
			result = append(result, &instance)
		}
	}
	return result, nil
}

func (s *eventService) GetTodaySchedule(ctx context.Context, hostID uuid.UUID) ([]*models.Event, error) {
	now := time.Now()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	dayEnd := dayStart.Add(24 * time.Hour)
	return s.eventRepo.ListTodayByHostID(ctx, hostID, dayStart, dayEnd)
}

func (s *eventService) PublishEvent(ctx context.Context, eventID uuid.UUID, hostID uuid.UUID) (*models.Event, error) {
	evt, err := s.eventRepo.GetByID(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if evt == nil {
		return nil, errors.New("event not found")
	}
	if evt.HostID != hostID {
		return nil, errors.New("unauthorized")
	}
	// Only a draft is promoted to live. Publishing an already-live, paused, or
	// cancelled event is a no-op: this lets an edit-time "save" call publish
	// unconditionally without silently un-pausing, resurrecting a cancelled
	// event, or resetting published_at on every edit. (Un-pausing has its own
	// path, ResumeEvent.)
	if evt.Status != models.EventStatusDraft {
		return evt, nil
	}
	if err := s.eventRepo.UpdateStatus(ctx, eventID, models.EventStatusLive); err != nil {
		return nil, err
	}
	evt.Status = models.EventStatusLive
	now := time.Now()
	evt.PublishedAt = &now
	return evt, nil
}

func (s *eventService) PauseEvent(ctx context.Context, eventID uuid.UUID, hostID uuid.UUID, pausedFrom *time.Time, pausedDate *time.Time) (*models.Event, error) {
	evt, err := s.eventRepo.GetByID(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if evt == nil {
		return nil, errors.New("event not found")
	}
	if evt.HostID != hostID {
		return nil, errors.New("unauthorized")
	}

	// Any pause mode (full, from-session, or single-session) flips status to
	// "paused" so the host UI can surface the paused state on the event card.
	// The granular fields paused_from / paused_dates determine which specific
	// occurrences are filtered; a "full pause" is identified downstream by
	// status=paused with both granular fields empty.
	now := time.Now()
	evt.Status = models.EventStatusPaused
	evt.PausedAt = &now
	evt.PausedFrom = pausedFrom
	if pausedDate != nil {
		evt.PausedDates = append(evt.PausedDates, pausedDate.Format(time.RFC3339))
	}

	if err := s.eventRepo.Update(ctx, evt); err != nil {
		return nil, err
	}

	// TRIGGER AUTOMATIC REFUNDS
	go func() {
		bgCtx := context.Background()
		// 1. Get all pending/confirmed bookings for this event
		bookings, err := s.bookingRepo.ListByEventID(bgCtx, eventID)
		if err != nil {
			fmt.Printf("[PAUSE_REFUND] Failed to list bookings for event %s: %v\n", eventID, err)
			return
		}

		for _, b := range bookings {
			shouldRefund := false

			if pausedDate != nil {
				// Option: Single session pause
				if b.OccurrenceDate.Equal(*pausedDate) {
					shouldRefund = true
				}
			} else if pausedFrom != nil {
				// Option: From specific date onwards
				if !b.OccurrenceDate.Before(*pausedFrom) {
					shouldRefund = true
				}
			} else {
				// Option: Full pause (pause all future sessions)
				if !b.OccurrenceDate.Before(now) {
					shouldRefund = true
				}
			}

			if shouldRefund {
				fmt.Printf("[PAUSE_REFUND] Refunding booking %s (occurrence %s) due to event pause\n", b.ID, b.OccurrenceDate)
				_, err := s.bookingService.CancelBookingByHost(bgCtx, b.ID)
				if err != nil {
					fmt.Printf("[PAUSE_REFUND] Failed to refund booking %s: %v\n", b.ID, err)
				}
			}
		}
	}()

	return evt, nil
}

func (s *eventService) ResumeEvent(ctx context.Context, eventID uuid.UUID, hostID uuid.UUID) (*models.Event, error) {
	evt, err := s.eventRepo.GetByID(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if evt == nil {
		return nil, errors.New("event not found")
	}
	if evt.HostID != hostID {
		return nil, errors.New("unauthorized")
	}

	// Clear all pause state so the event returns to a normal live schedule.
	evt.Status = models.EventStatusLive
	evt.PausedAt = nil
	evt.PausedFrom = nil
	evt.PausedDates = nil

	if err := s.eventRepo.Update(ctx, evt); err != nil {
		return nil, err
	}
	return evt, nil
}

func (s *eventService) GetEventAttendees(ctx context.Context, eventID uuid.UUID, occurrenceDate *time.Time) ([]*models.Attendee, error) {
	var attendees []*models.Attendee
	var err error
	if occurrenceDate != nil {
		attendees, err = s.bookingRepo.ListAttendeesByEventOccurrence(ctx, eventID, *occurrenceDate)
	} else {
		attendees, err = s.bookingRepo.ListAttendeesByEventID(ctx, eventID)
	}
	if err != nil {
		return nil, err
	}

	// Attach each attendee's submitted details (name, age, Govt-ID, …) for the
	// host roster. Batch-loaded to avoid N+1.
	if len(attendees) > 0 {
		userIDs := make([]uuid.UUID, 0, len(attendees))
		for _, a := range attendees {
			userIDs = append(userIDs, a.UserID)
		}
		profiles, perr := s.attendeeRepo.ListByUserIDs(ctx, userIDs)
		if perr != nil {
			return nil, perr
		}
		for _, a := range attendees {
			if p, ok := profiles[a.UserID]; ok {
				a.AttendeeProfile = p
			}
		}
	}

	return attendees, nil
}

func (s *eventService) ListPublishedEvents(ctx context.Context, limit, offset int) ([]*models.Event, error) {
	events, err := s.eventRepo.ListPublished(ctx, limit, offset)
	if err != nil {
		return nil, err
	}

	if len(events) == 0 {
		return events, nil
	}

	// 1. Collect all event IDs
	eventIDs := make([]uuid.UUID, len(events))
	for i, e := range events {
		eventIDs[i] = e.ID
	}

	// 2. Fetch occupancy for all these events in one batch
	// We check from "now" to handle upcoming sessions
	occupancyMap, err := s.bookingRepo.GetOccupancyForEvents(ctx, eventIDs, time.Now().Add(-24*time.Hour))
	if err != nil {
		fmt.Printf("[EVENT_SERVICE] Error fetching batch occupancy: %v\n", err)
		// Fallback to empty map to avoid failing the whole request
		occupancyMap = make(map[uuid.UUID]map[string]int)
	}

	// 3. For each event, calculate next available date using the batch data
	for _, e := range events {
		eventOccupancy := occupancyMap[e.ID]
		
		if e.IsRecurring && e.RecurrenceRule != nil {
			// calculateAvailabilityInternal(includePaused=false) already drops
			// paused occurrences, so anything left here is bookable.
			avail := s.calculateAvailabilityInternal(e, eventOccupancy, false)
			for _, a := range avail {
				if !a.IsFullyBooked {
					e.NextAvailableDate = &a.Date
					break
				}
			}
		} else {
			// Non-recurring: the single occurrence must be neither paused nor
			// full. Without the pause check a paused one-off event kept a
			// next_available_date and stayed visible in the public feed.
			booked := eventOccupancy[e.Time.Format(time.RFC3339)]
			if booked < e.Capacity && !isOccurrencePaused(e, e.Time) {
				e.NextAvailableDate = &e.Time
			}
		}
	}
	// 4. Filter out events that have no available dates (fully paused or fully booked)
	// EXCEPT if they are Live (we might still want to show fully booked live events)
	finalEvents := make([]*models.Event, 0, len(events))
	for _, e := range events {
		if e.NextAvailableDate != nil || e.Status == models.EventStatusLive {
			finalEvents = append(finalEvents, e)
		}
	}

	if err := s.attachTiersBatch(ctx, finalEvents); err != nil {
		fmt.Printf("[EVENT_SERVICE] Error attaching tiers to published events: %v\n", err)
	}

	return finalEvents, nil
}

// isOccurrencePaused reports whether one specific occurrence of an event is
// paused. Mirrors the rule used by calculateAvailabilityInternal so the public
// feed and the availability calendar agree:
//   - the date is listed in paused_dates (single-session pause), or
//   - it falls at/after paused_from (pause-from-session-onwards), or
//   - the event is fully paused (status=paused with neither granular field set).
func isOccurrencePaused(evt *models.Event, t time.Time) bool {
	for _, pdStr := range evt.PausedDates {
		if pd, err := time.Parse(time.RFC3339, pdStr); err == nil && pd.Equal(t) {
			return true
		}
		if strings.HasPrefix(pdStr, t.Format("2006-01-02")) {
			return true
		}
	}
	if evt.PausedFrom != nil && !t.Before(*evt.PausedFrom) {
		return true
	}
	return evt.Status == models.EventStatusPaused &&
		evt.PausedFrom == nil && len(evt.PausedDates) == 0
}

func (s *eventService) GetEventAvailability(ctx context.Context, eventID uuid.UUID) ([]models.OccurrenceAvailability, error) {
	evt, err := s.eventRepo.GetByID(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if evt == nil {
		return nil, errors.New("event not found")
	}

	// For a single event, we can still fetch occupancy in batch for its occurrences
	occupancy, err := s.bookingRepo.GetOccupancyForEvents(ctx, []uuid.UUID{eventID}, time.Now().Add(-24*time.Hour))
	if err != nil {
		return nil, err
	}

	return s.calculateAvailabilityInternal(evt, occupancy[eventID], false), nil
}

// GetEventOccurrencesForHost returns the full upcoming occurrence list for the host's
// pause/manage UI. Unlike GetEventAvailability (public/booking view), paused occurrences
// are included with IsPaused=true so the host can see and act on the entire schedule.
func (s *eventService) GetEventOccurrencesForHost(ctx context.Context, eventID uuid.UUID, hostID uuid.UUID) ([]models.OccurrenceAvailability, error) {
	evt, err := s.eventRepo.GetByID(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if evt == nil {
		return nil, errors.New("event not found")
	}
	if evt.HostID != hostID {
		return nil, errors.New("unauthorized")
	}

	occupancy, err := s.bookingRepo.GetOccupancyForEvents(ctx, []uuid.UUID{eventID}, time.Now().Add(-24*time.Hour))
	if err != nil {
		return nil, err
	}

	return s.calculateAvailabilityInternal(evt, occupancy[eventID], true), nil
}

// calculateAvailabilityInternal is the core logic that takes an event and its pre-fetched occupancy
// and returns the availability for its next few occurrences.
//
// When includePaused is false (public/booking view), paused occurrences are filtered out entirely.
// When includePaused is true (host pause-management view), paused occurrences are returned with
// IsPaused=true so the host can see and manage the full upcoming schedule.
func (s *eventService) calculateAvailabilityInternal(evt *models.Event, occupancy map[string]int, includePaused bool) []models.OccurrenceAvailability {
	type occurrence struct {
		t        time.Time
		isPaused bool
	}
	var occurrences []occurrence

	// Generate occurrences using rrule
	if (evt.IsRecurring || (evt.RecurrenceRule != nil && *evt.RecurrenceRule != "")) && evt.RecurrenceRule != nil {
		ruleStr := normalizeRRule(*evt.RecurrenceRule)
		var r *rrule.RRule
		var err error

		switch strings.ToUpper(ruleStr) {
		case "FREQ=DAILY":
			r, err = rrule.NewRRule(rrule.ROption{Freq: rrule.DAILY, Dtstart: evt.Time})
		case "FREQ=WEEKLY":
			r, err = rrule.NewRRule(rrule.ROption{Freq: rrule.WEEKLY, Dtstart: evt.Time})
		case "FREQ=MONTHLY":
			r, err = rrule.NewRRule(rrule.ROption{Freq: rrule.MONTHLY, Dtstart: evt.Time})
		case "FREQ=WEEKLY;INTERVAL=2":
			r, err = rrule.NewRRule(rrule.ROption{Freq: rrule.WEEKLY, Interval: 2, Dtstart: evt.Time})
		default:
			r, err = rrule.StrToRRule(ruleStr)
			if err == nil {
				r.DTStart(evt.Time)
			}
		}

		if err != nil {
			// Unparseable rule → fall back to the single base occurrence, but
			// still honour the pause rule so a paused event can't leak through.
			if p := isOccurrencePaused(evt, evt.Time); !p || includePaused {
				occurrences = append(occurrences, occurrence{t: evt.Time, isPaused: p})
			}
		} else {
			now := time.Now()
			curr := now.Add(-1 * time.Hour)
			if evt.Time.After(curr) {
				curr = evt.Time.Add(-1 * time.Second)
			}

			for i := 0; i < 12; i++ {
				next := r.After(curr, false)
				if next.IsZero() {
					break
				}

				// Check if this specific date is paused
				isDatePaused := false
				for _, pdStr := range evt.PausedDates {
					pd, err := time.Parse(time.RFC3339, pdStr)
					if err == nil && pd.Equal(next) {
						isDatePaused = true
						break
					}
					if strings.HasPrefix(pdStr, next.Format("2006-01-02")) {
						isDatePaused = true
						break
					}
				}

				isAfterPausedFrom := evt.PausedFrom != nil && !next.Before(*evt.PausedFrom)

				// Full pause: status=paused with no granular options set
				isFullPause := evt.Status == models.EventStatusPaused && evt.PausedFrom == nil && len(evt.PausedDates) == 0

				isPaused := isDatePaused || isAfterPausedFrom || isFullPause

				if !isPaused || includePaused {
					occurrences = append(occurrences, occurrence{t: next, isPaused: isPaused})
				}

				if len(occurrences) >= 6 {
					break
				}
				curr = next
			}
		}
	}

	if len(occurrences) == 0 && (evt.Status != models.EventStatusPaused || includePaused) {
		isPaused := evt.Status == models.EventStatusPaused
		occurrences = append(occurrences, occurrence{t: evt.Time, isPaused: isPaused})
	}

	var availability []models.OccurrenceAvailability
	for _, o := range occurrences {
		booked := 0
		if occupancy != nil {
			booked = occupancy[o.t.Format(time.RFC3339)]
		}

		availability = append(availability, models.OccurrenceAvailability{
			Date:          o.t,
			TotalBooked:   booked,
			Capacity:      evt.Capacity,
			Remaining:     evt.Capacity - booked,
			IsFullyBooked: booked >= evt.Capacity,
			IsPaused:      o.isPaused,
		})
	}

	return availability
}
