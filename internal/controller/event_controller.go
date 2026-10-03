package controller

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"myslotmate-backend/internal/auth"
	"myslotmate-backend/internal/lib/ratelimit"
	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"
	"myslotmate-backend/internal/service"

	fbauth "firebase.google.com/go/v4/auth"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type EventController struct {
	eventService service.EventService
	// Used to turn an authenticated caller into the host they are, so a request
	// cannot simply claim someone else's host_id. All three may be nil, in which
	// case the controller keeps the legacy body-supplied behaviour.
	userRepo     repository.UserRepository
	hostRepo     repository.HostRepository
	firebaseAuth *fbauth.Client
	jwtSecret    string
	// Comma-separated admin allow-list, for the host-scoped reads the admin
	// dashboard legitimately makes against other people's hosts.
	adminEmail string
}

func NewEventController(s service.EventService) *EventController {
	return &EventController{eventService: s}
}

// WithAuth attaches the identity lookups. Separate from the constructor so the
// many existing NewEventController callers (tests included) keep working.
func (c *EventController) WithAuth(ur repository.UserRepository, hr repository.HostRepository, fa *fbauth.Client, jwtSecret string) *EventController {
	c.userRepo = ur
	c.hostRepo = hr
	c.firebaseAuth = fa
	c.jwtSecret = jwtSecret
	return c
}

// WithAdminEmail supplies the admin allow-list. Optional: without it, admin
// Firebase sessions simply do not get the cross-host read, and the static admin
// JWT still does.
func (c *EventController) WithAdminEmail(adminEmail string) *EventController {
	c.adminEmail = adminEmail
	return c
}

// assertHostScope authorises a /host/{hostID}-shaped read: the signed-in host
// may read their own, and an admin may read anyone's.
//
// Fails closed. These routes used to be public, and the data behind them —
// drafts, schedules, guest lists — was never meant to be.
func (c *EventController) assertHostScope(r *http.Request, hostID uuid.UUID) error {
	if auth.IsAdminCaller(r) {
		return nil
	}
	actingHostID, err := c.hostIDFor(r, uuid.Nil)
	if err != nil {
		return err
	}
	if actingHostID != hostID {
		return errors.New("you can only read your own host data")
	}
	return nil
}

// hostIDFor decides which host a mutating request acts as.
//
// For a host the token decides, never the body: a body host_id that disagrees
// with the signed-in host is refused rather than honoured, which is what stops
// one host editing another's experiences. An empty UID means the controller was
// wired without its identity lookups — fail closed rather than trust the body.
//
// An admin is the one caller that acts on someone else's behalf, so for an
// admin session the body host_id is the acting host.
func (c *EventController) hostIDFor(r *http.Request, bodyHostID uuid.UUID) (uuid.UUID, error) {
	// An admin acts on a host's behalf from the dashboard, so for them the body
	// host_id is the acting host — there is no host record behind an admin
	// session token to look up.
	if auth.IsAdminCaller(r) {
		if bodyHostID == uuid.Nil {
			return uuid.Nil, errors.New("host_id is required")
		}
		return bodyHostID, nil
	}

	uid, _ := r.Context().Value(auth.ContextKeyUID).(string)
	if uid == "" || c.userRepo == nil || c.hostRepo == nil {
		return uuid.Nil, errors.New("sign in as a host to do that")
	}

	user, err := c.userRepo.GetByAuthUID(r.Context(), uid)
	if err != nil {
		return uuid.Nil, err
	}
	if user == nil {
		return uuid.Nil, errors.New("user not found")
	}
	host, err := c.hostRepo.GetByUserID(r.Context(), user.ID)
	if err != nil {
		return uuid.Nil, err
	}
	if host == nil {
		return uuid.Nil, errors.New("you are not a host")
	}
	if bodyHostID != uuid.Nil && bodyHostID != host.ID {
		return uuid.Nil, errors.New("host_id does not belong to the signed-in user")
	}
	return host.ID, nil
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
		// Reads are public — discovery, event pages and the booking flow all
		// depend on them.
		r.Get("/", c.ListPublishedEvents)
		r.Get("/slug-available", c.CheckSlugAvailability)
		r.Get("/{eventID}", c.GetEvent)
		r.Post("/{eventID}/unlock", c.UnlockEvent)
		r.Get("/{eventID}/availability", c.GetEventAvailability)
		r.Get("/{eventID}/occurrences", c.GetEventOccurrencesForHost)

		// Host-scoped reads. These were public, which meant anyone holding an
		// event or host id could read a host's drafts and schedule — and, from
		// the roster, every guest's phone number, WhatsApp number, age and
		// government-ID link. Each handler now checks that the caller is that
		// host, or an admin.
		//
		// RequireUserOrAdmin, not RequireUser: the admin dashboard reads other
		// people's hosts by design, and its session token carries a different
		// issuer, which a plain RequireUser would reject before the handler's
		// own scope check ever ran.
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireUserOrAdmin(c.firebaseAuth, c.adminEmail, c.jwtSecret))

			r.Get("/host/{hostID}", c.GetHostEvents)
			r.Get("/host/{hostID}/filtered", c.GetHostEventsFiltered)
			r.Get("/calendar/{hostID}", c.GetCalendarEvents)
			r.Get("/today/{hostID}", c.GetTodaySchedule)
			r.Get("/{eventID}/attendees", c.GetEventAttendees)
		})

		// Everything that changes an experience needs a signed-in host, or an
		// admin acting for one from the dashboard. The acting host comes from
		// the token (see hostIDFor) — a host's body host_id is only honoured
		// when it matches, so no host can act as another.
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireUserOrAdmin(c.firebaseAuth, c.adminEmail, c.jwtSecret))

			r.Post("/", c.CreateEvent)
			r.Put("/{eventID}", c.UpdateEvent)
			r.Delete("/{eventID}", c.DeleteEvent)
			r.Post("/{eventID}/publish", c.PublishEvent)
			r.Post("/{eventID}/pause", c.PauseEvent)
			r.Post("/{eventID}/resume", c.ResumeEvent)
			r.Post("/{eventID}/cancel", c.CancelEvent)
		})
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
	ScheduleType       *models.ScheduleType       `json:"schedule_type,omitempty"`
	CustomDates        []string                   `json:"custom_dates,omitempty"`
	SessionType        *models.SessionType        `json:"session_type,omitempty"`
	BreakMinutes       *int                       `json:"break_minutes,omitempty"`
	SessionWindows     models.SessionWindows      `json:"session_windows,omitempty"`
	PrivateAccessMode  *models.PrivateAccessMode  `json:"private_access_mode,omitempty"`
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

	// Privacy & access. IsPrivate lists the event with a lock; AccessPasskey is
	// required at the Book step; PasskeyGrantsFree makes that passkey also comp a
	// paid booking to free.
	IsPrivate         bool    `json:"is_private"`
	AccessPasskey     *string `json:"access_passkey,omitempty"`
	PasskeyGrantsFree bool    `json:"passkey_grants_free"`

	// Monthly pass config. Omitted leaves it alone; price_cents 0 switches it off.
	MonthlyPass *service.MonthlyPassInput `json:"monthly_pass,omitempty"`
}

type EventUpdateRequestBody struct {
	Title              *string                    `json:"title,omitempty"`
	Slug               *string                    `json:"slug,omitempty"`
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
	ScheduleType       *models.ScheduleType       `json:"schedule_type,omitempty"`
	CustomDates        []string                   `json:"custom_dates,omitempty"`
	SessionType        *models.SessionType        `json:"session_type,omitempty"`
	BreakMinutes       *int                       `json:"break_minutes,omitempty"`
	SessionWindows     models.SessionWindows      `json:"session_windows,omitempty"`
	PrivateAccessMode  *models.PrivateAccessMode  `json:"private_access_mode,omitempty"`
	CancellationPolicy *models.CancellationPolicy `json:"cancellation_policy,omitempty"`
	PriceTiers         []service.PriceTierInput   `json:"price_tiers,omitempty"`

	RequiresAttendeeDetails *bool    `json:"requires_attendee_details,omitempty"`
	AttendeeFields          []string `json:"attendee_fields,omitempty"`
	TermsAndConditions      *string  `json:"terms_and_conditions,omitempty"`

	// Privacy & access (all optional — nil = leave unchanged). AccessPasskey left
	// nil keeps the current passkey; sending a value replaces it.
	IsPrivate         *bool   `json:"is_private,omitempty"`
	AccessPasskey     *string `json:"access_passkey,omitempty"`
	PasskeyGrantsFree *bool   `json:"passkey_grants_free,omitempty"`

	// Monthly pass config. Omitted leaves it alone; price_cents 0 switches it off.
	MonthlyPass *service.MonthlyPassInput `json:"monthly_pass,omitempty"`
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

	// Private-event passkeys never travel in the public discovery feed.
	stripEventPasskeys(events)

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
		ScheduleType:       req.ScheduleType,
		CustomDates:        req.CustomDates,
		SessionType:        req.SessionType,
		BreakMinutes:       req.BreakMinutes,
		SessionWindows:     req.SessionWindows,
		PrivateAccessMode:  req.PrivateAccessMode,
		CancellationPolicy: req.CancellationPolicy,
		AISuggestion:       req.AISuggestion,
		PriceTiers:         req.PriceTiers,
		Status:             req.Status,

		RequiresAttendeeDetails: req.RequiresAttendeeDetails,
		AttendeeFields:          req.AttendeeFields,
		TermsAndConditions:      req.TermsAndConditions,

		IsPrivate:         req.IsPrivate,
		AccessPasskey:     req.AccessPasskey,
		PasskeyGrantsFree: req.PasskeyGrantsFree,
		MonthlyPass:       req.MonthlyPass,
	}

	hostID, err := c.hostIDFor(r, req.HostID)
	if err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}

	evt, err := c.eventService.CreateEvent(r.Context(), hostID, svcReq)
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
		Slug:               body.Slug,
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
		ScheduleType:       body.ScheduleType,
		CustomDates:        body.CustomDates,
		SessionType:        body.SessionType,
		BreakMinutes:       body.BreakMinutes,
		SessionWindows:     body.SessionWindows,
		PrivateAccessMode:  body.PrivateAccessMode,
		CancellationPolicy: body.CancellationPolicy,
		PriceTiers:         body.PriceTiers,

		RequiresAttendeeDetails: body.RequiresAttendeeDetails,
		AttendeeFields:          body.AttendeeFields,
		TermsAndConditions:      body.TermsAndConditions,

		IsPrivate:         body.IsPrivate,
		AccessPasskey:     body.AccessPasskey,
		PasskeyGrantsFree: body.PasskeyGrantsFree,
		MonthlyPass:       body.MonthlyPass,
	}

	hostID, err := c.hostIDFor(r, body.HostID)
	if err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}

	evt, err := c.eventService.UpdateEvent(r.Context(), eventID, hostID, svcReq)
	if err != nil {
		if errors.Is(err, service.ErrInvalidEventMood) {
			RespondError(w, http.StatusBadRequest, err.Error())
			return
		}
		if errors.Is(err, service.ErrSlugTaken) {
			RespondError(w, http.StatusConflict, err.Error())
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

	hostID, err := c.hostIDFor(r, body.HostID)
	if err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}

	if err := c.eventService.DeleteEvent(r.Context(), eventID, hostID); err != nil {
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

// CheckSlugAvailability reports whether a slug is free to use, for the admin
// experience editor's live duplicate check. Query params: slug (required) and
// exclude (optional event UUID to ignore — the event being edited).
// Responds { available: bool, slug: "<normalized>" }.
func (c *EventController) CheckSlugAvailability(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("slug")
	if raw == "" {
		RespondError(w, http.StatusBadRequest, "slug is required")
		return
	}

	var excludeID *uuid.UUID
	if ex := r.URL.Query().Get("exclude"); ex != "" {
		id, err := uuid.Parse(ex)
		if err != nil {
			RespondError(w, http.StatusBadRequest, "invalid exclude id")
			return
		}
		excludeID = &id
	}

	available, normalized, err := c.eventService.IsSlugAvailable(r.Context(), raw, excludeID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, map[string]any{
		"available": available,
		"slug":      normalized,
	})
}

func (c *EventController) GetEvent(w http.ResponseWriter, r *http.Request) {
	// The route param may be a clean slug or a raw UUID (old links). The
	// service resolves either form.
	param := chi.URLParam(r, "eventID")

	evt, err := c.eventService.GetEventBySlugOrID(r.Context(), param)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if evt == nil {
		RespondError(w, http.StatusNotFound, "Event not found")
		return
	}

	// Never expose a private event's passkey to guests. Only the owning host
	// (identified by a matching ?host_id=) may read it back — e.g. the admin
	// edit form prefilling the field so the host can re-share the invite.
	if evt.AccessPasskey != nil {
		if reqHost := r.URL.Query().Get("host_id"); reqHost == "" || reqHost != evt.HostID.String() {
			evt.AccessPasskey = nil
		}
	}

	RespondSuccess(w, http.StatusOK, evt)
}

// UnlockEvent is the dry-run behind the guest's passkey prompt for a private
// event. It reports whether the supplied passkey is correct (and whether it also
// comps the booking) WITHOUT ever returning the passkey itself. The
// authoritative check re-runs inside CreateBooking, so a forged "valid" response
// can't actually book.
func (c *EventController) UnlockEvent(w http.ResponseWriter, r *http.Request) {
	param := chi.URLParam(r, "eventID")

	var body struct {
		Passkey string `json:"passkey"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	evt, err := c.eventService.GetEventBySlugOrID(r.Context(), param)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if evt == nil {
		RespondError(w, http.StatusNotFound, "Event not found")
		return
	}

	// A non-private event is always "unlocked".
	if !evt.IsPrivate {
		RespondSuccess(w, http.StatusOK, map[string]bool{"valid": true, "grants_free": false})
		return
	}

	// Throttle passkey attempts per IP+event so a paid, passkey-comped event
	// can't be brute-forced into a free ticket via this endpoint.
	if !ratelimit.Passkey.Allow(clientIP(r) + ":" + evt.ID.String()) {
		RespondError(w, http.StatusTooManyRequests, "Too many attempts. Please try again in a minute.")
		return
	}

	want := ""
	if evt.AccessPasskey != nil {
		want = strings.TrimSpace(*evt.AccessPasskey)
	}
	valid := want != "" && strings.EqualFold(strings.TrimSpace(body.Passkey), want)
	RespondSuccess(w, http.StatusOK, map[string]bool{
		"valid":       valid,
		"grants_free": valid && evt.PasskeyGrantsFree,
	})
}

func (c *EventController) GetHostEvents(w http.ResponseWriter, r *http.Request) {
	hostID, err := uuid.Parse(chi.URLParam(r, "hostID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid host ID")
		return
	}

	// Host-scoped: the signed-in host may read their own, an admin anyone's.
	if err := c.assertHostScope(r, hostID); err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}

	events, err := c.eventService.GetHostEvents(r.Context(), hostID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// List views never carry the passkey (this endpoint is also public on host
	// profiles). The single GetEvent, gated by host_id, is the only place it leaks.
	stripEventPasskeys(events)

	RespondSuccess(w, http.StatusOK, events)
}

// clientIP returns the request's source IP (host without port). The RealIP
// middleware has already resolved X-Forwarded-For into RemoteAddr upstream.
func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// stripEventPasskeys blanks the private-event passkey on a list of events so it
// never travels in any multi-event response.
func stripEventPasskeys(events []*models.Event) {
	for _, e := range events {
		if e != nil {
			e.AccessPasskey = nil
		}
	}
}

func (c *EventController) GetHostEventsFiltered(w http.ResponseWriter, r *http.Request) {
	hostID, err := uuid.Parse(chi.URLParam(r, "hostID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid host ID")
		return
	}

	// Host-scoped: the signed-in host may read their own, an admin anyone's.
	if err := c.assertHostScope(r, hostID); err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
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

	stripEventPasskeys(events)

	RespondSuccess(w, http.StatusOK, events)
}

func (c *EventController) GetCalendarEvents(w http.ResponseWriter, r *http.Request) {
	hostID, err := uuid.Parse(chi.URLParam(r, "hostID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid host ID")
		return
	}

	// Host-scoped: the signed-in host may read their own, an admin anyone's.
	if err := c.assertHostScope(r, hostID); err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
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

	stripEventPasskeys(events)

	RespondSuccess(w, http.StatusOK, events)
}

func (c *EventController) GetTodaySchedule(w http.ResponseWriter, r *http.Request) {
	hostID, err := uuid.Parse(chi.URLParam(r, "hostID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid host ID")
		return
	}

	// Host-scoped: the signed-in host may read their own, an admin anyone's.
	if err := c.assertHostScope(r, hostID); err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}

	events, err := c.eventService.GetTodaySchedule(r.Context(), hostID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	stripEventPasskeys(events)

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

	hostID, err := c.hostIDFor(r, body.HostID)
	if err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}

	evt, err := c.eventService.PublishEvent(r.Context(), eventID, hostID)
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

	hostID, err := c.hostIDFor(r, body.HostID)
	if err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}

	evt, err := c.eventService.PauseEvent(r.Context(), eventID, hostID, body.PausedFrom, body.PausedDate)
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
	hostID, err := c.hostIDFor(r, body.HostID)
	if err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}

	evt, err := c.eventService.CancelEvent(r.Context(), eventID, hostID)
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

	hostID, err := c.hostIDFor(r, body.HostID)
	if err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}

	evt, err := c.eventService.ResumeEvent(r.Context(), eventID, hostID)
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

	// The roster carries guests' contact details and government-ID links. Only
	// the host running the event (or an accepted co-host) may read it; a 404
	// rather than a 403 so the endpoint does not confirm which ids exist.
	if !auth.IsAdminCaller(r) {
		actingHostID, herr := c.hostIDFor(r, uuid.Nil)
		if herr != nil {
			RespondError(w, http.StatusUnauthorized, herr.Error())
			return
		}
		canManage, cerr := c.eventService.HostCanManageEvent(r.Context(), eventID, actingHostID)
		if cerr != nil {
			RespondError(w, http.StatusInternalServerError, cerr.Error())
			return
		}
		if !canManage {
			RespondError(w, http.StatusNotFound, "Event not found")
			return
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
