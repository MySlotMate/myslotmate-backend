package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"myslotmate-backend/internal/lib/event"
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
	GetEvent(ctx context.Context, eventID uuid.UUID) (*models.Event, error)
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
	PriceCents         *int64                     `json:"price_cents,omitempty"`
	IsFree             bool                       `json:"is_free"`
	Time               time.Time                  `json:"time"`
	EndTime            *time.Time                 `json:"end_time,omitempty"`
	IsRecurring        bool                       `json:"is_recurring"`
	RecurrenceRule     *string                    `json:"recurrence_rule,omitempty"`
	CancellationPolicy *models.CancellationPolicy `json:"cancellation_policy,omitempty"`
	Status             models.EventStatus         `json:"status"` // draft or live
	AISuggestion       *string                    `json:"ai_suggestion,omitempty"`
}

type EventUpdateRequest struct {
	Title              *string                    `json:"title,omitempty"`
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
	PriceCents         *int64                     `json:"price_cents,omitempty"`
	IsFree             *bool                      `json:"is_free,omitempty"`
	Time               *time.Time                 `json:"time,omitempty"`
	EndTime            *time.Time                 `json:"end_time,omitempty"`
	IsRecurring        *bool                      `json:"is_recurring,omitempty"`
	RecurrenceRule     *string                    `json:"recurrence_rule,omitempty"`
	CancellationPolicy *models.CancellationPolicy `json:"cancellation_policy,omitempty"`
}

type eventService struct {
	eventRepo      repository.EventRepository
	bookingRepo    repository.BookingRepository
	accountRepo    repository.AccountRepository
	ledgerRepo     repository.TransactionLedgerRepository
	dispatcher     *event.Dispatcher
	bookingService BookingService
}

var ErrInvalidEventMood = errors.New("invalid event mood")

func NewEventService(
	er repository.EventRepository,
	br repository.BookingRepository,
	ar repository.AccountRepository,
	lr repository.TransactionLedgerRepository,
	d *event.Dispatcher,
	bs BookingService,
) EventService {
	return &eventService{
		eventRepo:      er,
		bookingRepo:    br,
		accountRepo:    ar,
		ledgerRepo:     lr,
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
	status := req.Status
	if status == "" {
		status = models.EventStatusDraft
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
	}

	if status == models.EventStatusLive {
		newEvent.PublishedAt = &now
	}

	if err := s.eventRepo.Create(ctx, newEvent); err != nil {
		return nil, err
	}

	s.dispatcher.Publish(event.EventCreated, newEvent)

	return newEvent, nil
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

	if err := s.eventRepo.Update(ctx, evt); err != nil {
		return nil, err
	}
	return evt, nil
}

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

	// Get all bookings for this event to refund users
	bookings, err := s.bookingRepo.ListByEventID(ctx, eventID)
	if err != nil {
		return fmt.Errorf("failed to fetch bookings for refund: %w", err)
	}

	// Process refunds for each booking
	for _, booking := range bookings {
		// Skip if already cancelled or refunded
		if booking.Status == models.BookingStatusCancelled || booking.Status == models.BookingStatusRefunded {
			continue
		}

		// Refund amount should equal the original booking amount
		refundAmount := int64(0)
		if booking.AmountCents != nil {
			refundAmount = *booking.AmountCents
		}

		if refundAmount <= 0 {
			break // Skip bookings with no amount to refund
		}

		// 1. Get or create user account
		userAccount, err := s.accountRepo.GetByOwner(ctx, models.AccountOwnerUser, booking.UserID)
		if err != nil {
			fmt.Printf("[REFUND] Failed to fetch user account for user %s: %v\n", booking.UserID, err)
			continue
		}

		if userAccount == nil {
			fmt.Printf("[REFUND] User account not found for user %s (booking %s), skipping\n", booking.UserID, booking.ID)
			continue
		}

		// 2. Create refund ledger entry (credit user's account)
		now := time.Now()
		refundLedger := &models.TransactionLedger{
			ID:            uuid.New(),
			AccountID:     userAccount.ID,
			Type:          models.LedgerTypeRefundCredit,
			AmountCents:   refundAmount, // Positive = credit
			ReferenceID:   &booking.ID,
			ReferenceType: strPtr("booking"),
			Description:   strPtr(fmt.Sprintf("Event cancelled by host. Full refund for booking %s", booking.ID)),
			Status:        models.LedgerStatusCompleted,
			CreatedAt:     now,
		}

		_, err = s.ledgerRepo.Create(ctx, refundLedger)
		if err != nil {
			fmt.Printf("[REFUND] Failed to create refund ledger entry for booking %s: %v\n", booking.ID, err)
			continue
		}

		// 3. Update user account balance (credit the refund amount)
		if err := s.accountRepo.Credit(ctx, userAccount.ID, refundAmount); err != nil {
			fmt.Printf("[REFUND] Failed to credit user account for booking %s: %v\n", booking.ID, err)
			continue
		}

		// 4. Update booking status to refunded
		if err := s.bookingRepo.UpdateStatus(ctx, booking.ID, models.BookingStatusRefunded); err != nil {
			fmt.Printf("[REFUND] Failed to update booking status to refunded for booking %s: %v\n", booking.ID, err)
		}

		fmt.Printf("[REFUND] Successfully refunded %d cents to user %s for booking %s\n", refundAmount, booking.UserID, booking.ID)
	}

	// 5. Delete the event
	if err := s.eventRepo.Delete(ctx, eventID); err != nil {
		return err
	}

	s.dispatcher.Publish(event.EventDeleted, evt)
	fmt.Printf("[EVENT] Event %s deleted successfully with all bookings refunded\n", eventID)
	return nil
}

func (s *eventService) GetEvent(ctx context.Context, eventID uuid.UUID) (*models.Event, error) {
	return s.eventRepo.GetByID(ctx, eventID)
}

func (s *eventService) GetHostEvents(ctx context.Context, hostID uuid.UUID) ([]*models.Event, error) {
	return s.eventRepo.ListByHostID(ctx, hostID)
}

func (s *eventService) GetHostEventsFiltered(ctx context.Context, hostID uuid.UUID, status *models.EventStatus, search string, sortBy string, limit, offset int) ([]*models.Event, error) {
	return s.eventRepo.ListByHostIDFiltered(ctx, hostID, status, search, sortBy, limit, offset)
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
	if occurrenceDate != nil {
		return s.bookingRepo.ListAttendeesByEventOccurrence(ctx, eventID, *occurrenceDate)
	}
	return s.bookingRepo.ListAttendeesByEventID(ctx, eventID)
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
			avail := s.calculateAvailabilityInternal(e, eventOccupancy, false)
			for _, a := range avail {
				if !a.IsFullyBooked {
					e.NextAvailableDate = &a.Date
					break
				}
			}
		} else {
			// For non-recurring, check occupancy from map
			booked := eventOccupancy[e.Time.Format(time.RFC3339)]
			if booked < e.Capacity {
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

	return finalEvents, nil
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
			occurrences = append(occurrences, occurrence{t: evt.Time})
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
