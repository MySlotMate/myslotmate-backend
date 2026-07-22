package controller

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/service"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type EventController struct {
	eventService service.EventService
}

func NewEventController(s service.EventService) *EventController {
	return &EventController{eventService: s}
}

func resolveCalendarRange(r *http.Request) (time.Time, time.Time, int, string) {
	startStr := r.URL.Query().Get("start")
	endStr := r.URL.Query().Get("end")

	if startStr == "" && endStr == "" {
		now := time.Now()
		start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
		end := start.AddDate(0, 1, 0)
		return start, end, 0, ""
	}

	var (
		start time.Time
		end   time.Time
		err   error
	)

	if startStr != "" {
		start, err = time.Parse(time.RFC3339, startStr)
		if err != nil {
			return time.Time{}, time.Time{}, http.StatusBadRequest, "Invalid start time format"
		}
	}

	if endStr != "" {
		end, err = time.Parse(time.RFC3339, endStr)
		if err != nil {
			return time.Time{}, time.Time{}, http.StatusBadRequest, "Invalid end time format"
		}
	}

	if startStr == "" {
		start = end.AddDate(0, -1, 0)
	}
	if endStr == "" {
		end = start.AddDate(0, 1, 0)
	}

	if !end.After(start) {
		return time.Time{}, time.Time{}, http.StatusBadRequest, "end must be after start"
	}

	return start, end, 0, ""
}

func (c *EventController) RegisterRoutes(r chi.Router) {
	r.Route("/events", func(r chi.Router) {
		r.Get("/", c.ListPublishedEvents)
		r.Post("/", c.CreateEvent)
		r.Put("/{eventID}", c.UpdateEvent)
		r.Delete("/{eventID}", c.DeleteEvent)
		r.Get("/{eventID}", c.GetEvent)
		r.Get("/host/{hostID}", c.GetHostEvents)
		r.Get("/host/{hostID}/filtered", c.GetHostEventsFiltered)
		r.Get("/calendar/{hostID}", c.GetCalendarEvents)
		r.Get("/today/{hostID}", c.GetTodaySchedule)
		r.Post("/{eventID}/publish", c.PublishEvent)
		r.Post("/{eventID}/pause", c.PauseEvent)
		r.Post("/{eventID}/resume", c.ResumeEvent)
		r.Post("/{eventID}/cancel", c.CancelEvent)
		r.Get("/{eventID}/attendees", c.GetEventAttendees)
		r.Get("/{eventID}/availability", c.GetEventAvailability)
		r.Get("/{eventID}/occurrences", c.GetEventOccurrencesForHost)
	})
}

// ── Request types ───────────────────────────────────────────────────────────

type EventCreateRequestBody struct {
	HostID             uuid.UUID                  `json:"host_id"`
	Title              string                     `json:"title"`
	HookLine           *string                    `json:"hook_line,omitempty"`
	Mood               *models.EventMood          `json:"mood,omitempty"`
	Description        *string                    `json:"description,omitempty"`
	CoverImageURL      *string                    `json:"cover_image_url,omitempty"`
	GalleryURLs        []string                   `json:"gallery_urls,omitempty"`
	Time               time.Time                  `json:"time"`
	EndTime            *time.Time                 `json:"end_time,omitempty"`
	IsOnline           bool                       `json:"is_online"`
	MeetingLink        *string                    `json:"meeting_link,omitempty"` // for online events (zoom, teams, google meet, etc.)
	Location           *string                    `json:"location,omitempty"`
	LocationLat        *float64                   `json:"location_lat,omitempty"`
	LocationLng        *float64                   `json:"location_lng,omitempty"`
	GoogleMapsURL      *string                    `json:"google_maps_url,omitempty"` // direct link to Google Maps location
	DurationMinutes    *int                       `json:"duration_minutes,omitempty"`
	Capacity           int                        `json:"capacity"`
	MinGroupSize       *int                       `json:"min_group_size,omitempty"`
	MaxGroupSize       *int                       `json:"max_group_size,omitempty"`
	Languages          []string                   `json:"languages,omitempty"`
	Level              *string                    `json:"level,omitempty"`
	PriceCents         *int64                     `json:"price_cents,omitempty"`
	IsFree             bool                       `json:"is_free"`
	IsRecurring        bool                       `json:"is_recurring"`
	RecurrenceRule     *string                    `json:"recurrence_rule,omitempty"`
	CancellationPolicy *models.CancellationPolicy `json:"cancellation_policy,omitempty"`
	AISuggestion       *string                    `json:"ai_suggestion,omitempty"`
	PriceTiers         []service.PriceTierInput   `json:"price_tiers,omitempty"`

	// Status lets the caller create an event live in a single request instead of
	// creating a draft and publishing separately. Empty defaults to draft (see
	// eventService.CreateEvent).
	Status models.EventStatus `json:"status,omitempty"`

	RequiresAttendeeDetails bool     `json:"requires_attendee_details"`
	AttendeeFields          []string `json:"attendee_fields,omitempty"`
	TermsAndConditions      *string  `json:"terms_and_conditions,omitempty"`
}

type EventUpdateRequestBody struct {
	Title              *string                    `json:"title,omitempty"`
	HookLine           *string                    `json:"hook_line,omitempty"`
	Mood               *models.EventMood          `json:"mood,omitempty"`
	Description        *string                    `json:"description,omitempty"`
	CoverImageURL      *string                    `json:"cover_image_url,omitempty"`
	GalleryURLs        []string                   `json:"gallery_urls,omitempty"`
	Time               *time.Time                 `json:"time,omitempty"`
	EndTime            *time.Time                 `json:"end_time,omitempty"`
	IsOnline           *bool                      `json:"is_online,omitempty"`
	MeetingLink        *string                    `json:"meeting_link,omitempty"` // for online events (zoom, teams, google meet, etc.)
	Location           *string                    `json:"location,omitempty"`
	LocationLat        *float64                   `json:"location_lat,omitempty"`
	LocationLng        *float64                   `json:"location_lng,omitempty"`
	GoogleMapsURL      *string                    `json:"google_maps_url,omitempty"` // direct link to Google Maps location
	DurationMinutes    *int                       `json:"duration_minutes,omitempty"`
	Capacity           *int                       `json:"capacity,omitempty"`
	MinGroupSize       *int                       `json:"min_group_size,omitempty"`
	MaxGroupSize       *int                       `json:"max_group_size,omitempty"`
	Languages          []string                   `json:"languages,omitempty"`
	Level              *string                    `json:"level,omitempty"`
	PriceCents         *int64                     `json:"price_cents,omitempty"`
	IsFree             *bool                      `json:"is_free,omitempty"`
	IsRecurring        *bool                      `json:"is_recurring,omitempty"`
	RecurrenceRule     *string                    `json:"recurrence_rule,omitempty"`
	CancellationPolicy *models.CancellationPolicy `json:"cancellation_policy,omitempty"`
	PriceTiers         []service.PriceTierInput   `json:"price_tiers,omitempty"`

	RequiresAttendeeDetails *bool    `json:"requires_attendee_details,omitempty"`
	AttendeeFields          []string `json:"attendee_fields,omitempty"`
	TermsAndConditions      *string  `json:"terms_and_conditions,omitempty"`
}

// ── Handlers ────────────────────────────────────────────────────────────────

func (c *EventController) ListPublishedEvents(w http.ResponseWriter, r *http.Request) {
	limit := 20
	offset := 0
	if l := r.URL.Query().Get("limit"); l != "" {
		if v, err := strconv.Atoi(l); err == nil && v > 0 {
			limit = v
		}
	}
	if o := r.URL.Query().Get("offset"); o != "" {
		if v, err := strconv.Atoi(o); err == nil && v >= 0 {
			offset = v
		}
	}

	events, err := c.eventService.ListPublishedEvents(r.Context(), limit, offset)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, events)
}

func (c *EventController) CreateEvent(w http.ResponseWriter, r *http.Request) {
	var req EventCreateRequestBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	svcReq := service.EventCreateRequest{
		Title:              req.Title,
		HookLine:           req.HookLine,
		Mood:               req.Mood,
		Description:        req.Description,
		CoverImageURL:      req.CoverImageURL,
		GalleryURLs:        req.GalleryURLs,
		Time:               req.Time,
		EndTime:            req.EndTime,
		IsOnline:           req.IsOnline,
		MeetingLink:        req.MeetingLink,
		Location:           req.Location,
		LocationLat:        req.LocationLat,
		LocationLng:        req.LocationLng,
		GoogleMapsURL:      req.GoogleMapsURL,
		DurationMinutes:    req.DurationMinutes,
		Capacity:           req.Capacity,
		MinGroupSize:       req.MinGroupSize,
		MaxGroupSize:       req.MaxGroupSize,
		Languages:          req.Languages,
		Level:              req.Level,
		PriceCents:         req.PriceCents,
		IsFree:             req.IsFree,
		IsRecurring:        req.IsRecurring,
		RecurrenceRule:     req.RecurrenceRule,
		CancellationPolicy: req.CancellationPolicy,
		AISuggestion:       req.AISuggestion,
		PriceTiers:         req.PriceTiers,
		Status:             req.Status,

		RequiresAttendeeDetails: req.RequiresAttendeeDetails,
		AttendeeFields:          req.AttendeeFields,
		TermsAndConditions:      req.TermsAndConditions,
	}

	evt, err := c.eventService.CreateEvent(r.Context(), req.HostID, svcReq)
	if err != nil {
		if errors.Is(err, service.ErrInvalidEventMood) {
			RespondError(w, http.StatusBadRequest, err.Error())
			return
		}
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusCreated, evt)
}

func (c *EventController) UpdateEvent(w http.ResponseWriter, r *http.Request) {
	eventID, err := uuid.Parse(chi.URLParam(r, "eventID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid event ID")
		return
	}

	var body struct {
		HostID uuid.UUID `json:"host_id"`
		EventUpdateRequestBody
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	svcReq := service.EventUpdateRequest{
		Title:              body.Title,
		HookLine:           body.HookLine,
		Mood:               body.Mood,
		Description:        body.Description,
		CoverImageURL:      body.CoverImageURL,
		GalleryURLs:        body.GalleryURLs,
		Time:               body.Time,
		EndTime:            body.EndTime,
		IsOnline:           body.IsOnline,
		MeetingLink:        body.MeetingLink,
		Location:           body.Location,
		LocationLat:        body.LocationLat,
		LocationLng:        body.LocationLng,
		GoogleMapsURL:      body.GoogleMapsURL,
		DurationMinutes:    body.DurationMinutes,
		Capacity:           body.Capacity,
		MinGroupSize:       body.MinGroupSize,
		MaxGroupSize:       body.MaxGroupSize,
		Languages:          body.Languages,
		Level:              body.Level,
		PriceCents:         body.PriceCents,
		IsFree:             body.IsFree,
		IsRecurring:        body.IsRecurring,
		RecurrenceRule:     body.RecurrenceRule,
		CancellationPolicy: body.CancellationPolicy,
		PriceTiers:         body.PriceTiers,

		RequiresAttendeeDetails: body.RequiresAttendeeDetails,
		AttendeeFields:          body.AttendeeFields,
		TermsAndConditions:      body.TermsAndConditions,
	}

	evt, err := c.eventService.UpdateEvent(r.Context(), eventID, body.HostID, svcReq)
	if err != nil {
		if errors.Is(err, service.ErrInvalidEventMood) {
			RespondError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err.Error() == "unauthorized: you do not own this event" {
			RespondError(w, http.StatusForbidden, err.Error())
			return
		}
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, evt)
}

func (c *EventController) DeleteEvent(w http.ResponseWriter, r *http.Request) {
	eventID, err := uuid.Parse(chi.URLParam(r, "eventID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid event ID")
		return
	}

	var body struct {
		HostID uuid.UUID `json:"host_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	if err := c.eventService.DeleteEvent(r.Context(), eventID, body.HostID); err != nil {
		if err.Error() == "event not found" {
			RespondError(w, http.StatusNotFound, err.Error())
			return
		}
		if err.Error() == "unauthorized: you do not own this event" {
			RespondError(w, http.StatusForbidden, err.Error())
			return
		}
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (c *EventController) GetEvent(w http.ResponseWriter, r *http.Request) {
	eventID, err := uuid.Parse(chi.URLParam(r, "eventID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid event ID")
		return
	}

	evt, err := c.eventService.GetEvent(r.Context(), eventID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, evt)
}

func (c *EventController) GetHostEvents(w http.ResponseWriter, r *http.Request) {
	hostID, err := uuid.Parse(chi.URLParam(r, "hostID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid host ID")
		return
	}

	events, err := c.eventService.GetHostEvents(r.Context(), hostID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, events)
}

func (c *EventController) GetHostEventsFiltered(w http.ResponseWriter, r *http.Request) {
	hostID, err := uuid.Parse(chi.URLParam(r, "hostID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid host ID")
		return
	}

	search := r.URL.Query().Get("search")
	statusStr := r.URL.Query().Get("status")
	var status *models.EventStatus
	if statusStr != "" {
		s := models.EventStatus(statusStr)
		status = &s
	}
	sortBy := r.URL.Query().Get("sort_by")
	if sortBy == "" {
		sortBy = "created_at"
	}

	limit := 20
	offset := 0
	if l := r.URL.Query().Get("limit"); l != "" {
		if v, err := strconv.Atoi(l); err == nil {
			limit = v
		}
	}
	if o := r.URL.Query().Get("offset"); o != "" {
		if v, err := strconv.Atoi(o); err == nil {
			offset = v
		}
	}

	events, err := c.eventService.GetHostEventsFiltered(r.Context(), hostID, status, search, sortBy, limit, offset)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, events)
}

func (c *EventController) GetCalendarEvents(w http.ResponseWriter, r *http.Request) {
	hostID, err := uuid.Parse(chi.URLParam(r, "hostID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid host ID")
		return
	}

	start, end, statusCode, message := resolveCalendarRange(r)
	if statusCode != 0 {
		RespondError(w, statusCode, message)
		return
	}

	events, err := c.eventService.GetCalendarEvents(r.Context(), hostID, start, end)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, events)
}

func (c *EventController) GetTodaySchedule(w http.ResponseWriter, r *http.Request) {
	hostID, err := uuid.Parse(chi.URLParam(r, "hostID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid host ID")
		return
	}

	events, err := c.eventService.GetTodaySchedule(r.Context(), hostID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, events)
}

func (c *EventController) PublishEvent(w http.ResponseWriter, r *http.Request) {
	eventID, err := uuid.Parse(chi.URLParam(r, "eventID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid event ID")
		return
	}

	var body struct {
		HostID uuid.UUID `json:"host_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	evt, err := c.eventService.PublishEvent(r.Context(), eventID, body.HostID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, evt)
}

func (c *EventController) PauseEvent(w http.ResponseWriter, r *http.Request) {
	eventID, err := uuid.Parse(chi.URLParam(r, "eventID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid event ID")
		return
	}

	var body struct {
		HostID     uuid.UUID  `json:"host_id"`
		PausedFrom *time.Time `json:"paused_from,omitempty"`
		PausedDate *time.Time `json:"paused_date,omitempty"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	evt, err := c.eventService.PauseEvent(r.Context(), eventID, body.HostID, body.PausedFrom, body.PausedDate)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, evt)
}

// CancelEvent — host-initiated soft cancel: refunds every upcoming confirmed
// booking via F4 (CancelBookingByHost) and marks the event status=cancelled.
// Past confirmed bookings are left alone (those attendees already attended).
func (c *EventController) CancelEvent(w http.ResponseWriter, r *http.Request) {
	eventID, err := uuid.Parse(chi.URLParam(r, "eventID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid event ID")
		return
	}
	var body struct {
		HostID uuid.UUID `json:"host_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}
	evt, err := c.eventService.CancelEvent(r.Context(), eventID, body.HostID)
	if err != nil {
		if err.Error() == "event not found" {
			RespondError(w, http.StatusNotFound, err.Error())
			return
		}
		RespondError(w, http.StatusBadRequest, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, evt)
}

func (c *EventController) ResumeEvent(w http.ResponseWriter, r *http.Request) {
	eventID, err := uuid.Parse(chi.URLParam(r, "eventID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid event ID")
		return
	}

	var body struct {
		HostID uuid.UUID `json:"host_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	evt, err := c.eventService.ResumeEvent(r.Context(), eventID, body.HostID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, evt)
}

func (c *EventController) GetEventAttendees(w http.ResponseWriter, r *http.Request) {
	eventID, err := uuid.Parse(chi.URLParam(r, "eventID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid event ID")
		return
	}

	var occurrenceDate *time.Time
	dateStr := r.URL.Query().Get("date")
	if dateStr != "" {
		t, err := time.Parse(time.RFC3339, dateStr)
		if err == nil {
			occurrenceDate = &t
		}
	}

	attendees, err := c.eventService.GetEventAttendees(r.Context(), eventID, occurrenceDate)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, attendees)
}

func (c *EventController) GetEventAvailability(w http.ResponseWriter, r *http.Request) {
	eventID, err := uuid.Parse(chi.URLParam(r, "eventID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid event ID")
		return
	}

	availability, err := c.eventService.GetEventAvailability(r.Context(), eventID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, availability)
}

// GetEventOccurrencesForHost returns the full upcoming occurrence list for the host's
// pause-management UI. Paused occurrences are included with is_paused=true.
// Requires ?host_id=<uuid> for ownership verification.
func (c *EventController) GetEventOccurrencesForHost(w http.ResponseWriter, r *http.Request) {
	eventID, err := uuid.Parse(chi.URLParam(r, "eventID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid event ID")
		return
	}

	hostID, err := uuid.Parse(r.URL.Query().Get("host_id"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid or missing host_id")
		return
	}

	occurrences, err := c.eventService.GetEventOccurrencesForHost(r.Context(), eventID, hostID)
	if err != nil {
		if err.Error() == "unauthorized" {
			RespondError(w, http.StatusForbidden, err.Error())
			return
		}
		if err.Error() == "event not found" {
			RespondError(w, http.StatusNotFound, err.Error())
			return
		}
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, occurrences)
}
