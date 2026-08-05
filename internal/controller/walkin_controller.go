package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"myslotmate-backend/internal/auth"
	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"
	"myslotmate-backend/internal/service"

	fbauth "firebase.google.com/go/v4/auth"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// WalkInController exposes admin-only "on-spot" booking endpoints: an admin
// enters a guest's name + phone, then collects payment via Razorpay on-screen.
type WalkInController struct {
	walkInService service.WalkInService
	// userRepo/hostRepo back resolveHostID — the auth token is the only accepted
	// source of "which host is acting" on the host lookup route.
	userRepo     repository.UserRepository
	hostRepo     repository.HostRepository
	firebaseAuth *fbauth.Client
	adminEmail   string
	jwtSecret    string
}

func NewWalkInController(ws service.WalkInService, ur repository.UserRepository, hr repository.HostRepository, fa *fbauth.Client, adminEmail, jwtSecret string) *WalkInController {
	return &WalkInController{
		walkInService: ws,
		userRepo:      ur,
		hostRepo:      hr,
		firebaseAuth:  fa,
		adminEmail:    adminEmail,
		jwtSecret:     jwtSecret,
	}
}

func (c *WalkInController) RegisterRoutes(r chi.Router) {
	r.Route("/admin/bookings/walk-in", func(r chi.Router) {
		r.Use(auth.RequireAdmin(c.firebaseAuth, c.adminEmail, c.jwtSecret))
		r.Get("/lookup", c.Lookup)
		r.Post("/initiate", c.Initiate)
		r.Post("/complete", c.Complete)
	})

	// Host on-spot booking — a host books a guest onto their OWN event. Scoped by
	// host_id (ownership verified in the service), matching the /events posture;
	// no admin session required.
	r.Route("/host/bookings/walk-in", func(r chi.Router) {
		r.Post("/initiate", c.HostInitiate)
		r.Post("/complete", c.HostComplete)

		// Lookup is phone→name, so it stays behind auth even though its siblings
		// don't: unauthenticated it would be an open account-enumeration oracle.
		// RequireUser proves a real account, resolveHostID proves that account is
		// a host, and the event_id ownership check confines them to guests they
		// could already have booked. Body host_id is ignored here on purpose.
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireUser(c.firebaseAuth, c.jwtSecret))
			r.Get("/lookup", c.HostLookup)
		})
	})
}

// Lookup reports whether a phone already belongs to a user (so the modal can
// auto-fill the name). GET /admin/bookings/walk-in/lookup?phone=+91XXXXXXXXXX
func (c *WalkInController) Lookup(w http.ResponseWriter, r *http.Request) {
	phone := strings.TrimSpace(r.URL.Query().Get("phone"))
	if phone == "" {
		RespondError(w, http.StatusBadRequest, "phone is required")
		return
	}
	res, err := c.walkInService.LookupByPhone(r.Context(), phone)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, res)
}

// resolveHostID derives the caller's host UUID from the auth context — the
// single source of truth for "which host is acting". Any host_id in the query
// or body MUST be ignored (mirrors PayoutController.resolveHostID).
func (c *WalkInController) resolveHostID(ctx context.Context) (uuid.UUID, error) {
	uid, ok := ctx.Value(auth.ContextKeyUID).(string)
	if !ok || uid == "" {
		return uuid.Nil, errors.New("unauthenticated")
	}
	user, err := c.userRepo.GetByAuthUID(ctx, uid)
	if err != nil {
		return uuid.Nil, fmt.Errorf("user lookup failed: %w", err)
	}
	if user == nil {
		return uuid.Nil, errors.New("user not found")
	}
	host, err := c.hostRepo.GetByUserID(ctx, user.ID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("host lookup failed: %w", err)
	}
	if host == nil {
		return uuid.Nil, errors.New("caller is not a host")
	}
	return host.ID, nil
}

// HostLookup is the host-side phone lookup, confined to events the caller owns.
// GET /host/bookings/walk-in/lookup?phone=+91XXXXXXXXXX&event_id=<uuid>
func (c *WalkInController) HostLookup(w http.ResponseWriter, r *http.Request) {
	hostID, err := c.resolveHostID(r.Context())
	if err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}
	phone := strings.TrimSpace(r.URL.Query().Get("phone"))
	if phone == "" {
		RespondError(w, http.StatusBadRequest, "phone is required")
		return
	}
	eventID, err := uuid.Parse(strings.TrimSpace(r.URL.Query().Get("event_id")))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "valid event_id is required")
		return
	}

	res, err := c.walkInService.LookupByPhoneForHost(r.Context(), hostID, eventID, phone)
	if err != nil {
		if errors.Is(err, service.ErrWalkInNotOwner) {
			RespondError(w, http.StatusForbidden, err.Error())
			return
		}
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, res)
}

type walkInInitiateBody struct {
	GuestName      string `json:"guest_name"`
	GuestPhone     string `json:"guest_phone"`
	EventID        string `json:"event_id"`
	Quantity       int    `json:"quantity"`
	OccurrenceDate string `json:"occurrence_date,omitempty"` // RFC3339
	// CouponCode, when a valid free-booking code, comps the walk-in to ₹0 and
	// skips the payment step entirely.
	CouponCode string `json:"coupon_code,omitempty"`
	// HostID scopes the request to a host booking their own event (host flow).
	// Omitted by the admin flow.
	HostID string `json:"host_id,omitempty"`
	// AttendeeDetails carries the extra attendee-profile answers for events that
	// require them. Absent when the event needs none.
	AttendeeDetails *attendeeDetailsBody `json:"attendee_details,omitempty"`
}

// attendeeDetailsBody mirrors the attendee-profile field catalog. Only the fields
// the event requires are sent; the rest stay nil and are left untouched.
type attendeeDetailsBody struct {
	Name             *string `json:"name,omitempty"`
	Age              *int    `json:"age,omitempty"`
	Gender           *string `json:"gender,omitempty"`
	Qualification    *string `json:"qualification,omitempty"`
	Occupation       *string `json:"occupation,omitempty"`
	MaritalStatus    *string `json:"marital_status,omitempty"`
	ContactNumber    *string `json:"contact_number,omitempty"`
	WhatsappNumber   *string `json:"whatsapp_number,omitempty"`
	RegistrationType *string `json:"registration_type,omitempty"`
	GovtIDURL        *string `json:"govt_id_url,omitempty"`
	Travel           *bool   `json:"travel,omitempty"`
	SocialLink       *string `json:"social_link,omitempty"`
}

func (b *attendeeDetailsBody) toModel() *models.AttendeeProfile {
	if b == nil {
		return nil
	}
	return &models.AttendeeProfile{
		Name:             b.Name,
		Age:              b.Age,
		Gender:           b.Gender,
		Qualification:    b.Qualification,
		Occupation:       b.Occupation,
		MaritalStatus:    b.MaritalStatus,
		ContactNumber:    b.ContactNumber,
		WhatsappNumber:   b.WhatsappNumber,
		RegistrationType: b.RegistrationType,
		GovtIDURL:        b.GovtIDURL,
		Travel:           b.Travel,
		SocialLink:       b.SocialLink,
	}
}

// Initiate is the admin on-spot flow — can book any event (host_id ignored).
func (c *WalkInController) Initiate(w http.ResponseWriter, r *http.Request) {
	c.initiate(w, r, false)
}

// HostInitiate is the host on-spot flow — host_id is required and the event must
// belong to that host.
func (c *WalkInController) HostInitiate(w http.ResponseWriter, r *http.Request) {
	c.initiate(w, r, true)
}

func (c *WalkInController) initiate(w http.ResponseWriter, r *http.Request, hostRequired bool) {
	var body walkInInitiateBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	eventID, err := uuid.Parse(body.EventID)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid event_id")
		return
	}

	hostID, err := parseScopeHostID(body.HostID, hostRequired)
	if err != nil {
		RespondError(w, http.StatusBadRequest, err.Error())
		return
	}

	occ, err := parseOptionalOccurrence(body.OccurrenceDate)
	if err != nil {
		RespondError(w, http.StatusBadRequest, err.Error())
		return
	}

	resp, err := c.walkInService.InitiateWalkIn(r.Context(), service.WalkInInitiateRequest{
		GuestName:       body.GuestName,
		GuestPhone:      body.GuestPhone,
		EventID:         eventID,
		Quantity:        body.Quantity,
		OccurrenceDate:  occ,
		CouponCode:      body.CouponCode,
		AttendeeDetails: body.AttendeeDetails.toModel(),
		HostID:          hostID,
	})
	if err != nil {
		respondWalkInError(w, err)
		return
	}
	RespondSuccess(w, http.StatusOK, resp)
}

type walkInCompleteBody struct {
	GuestUserID       string `json:"guest_user_id"`
	EventID           string `json:"event_id"`
	Quantity          int    `json:"quantity"`
	OccurrenceDate    string `json:"occurrence_date,omitempty"`
	RazorpayOrderID   string `json:"razorpay_order_id"`
	RazorpayPaymentID string `json:"razorpay_payment_id"`
	RazorpaySignature string `json:"razorpay_signature"`
	// HostID scopes completion to the owning host (host flow); omitted by admin.
	HostID string `json:"host_id,omitempty"`
}

// Complete is the admin on-spot completion — can complete any event.
func (c *WalkInController) Complete(w http.ResponseWriter, r *http.Request) {
	c.complete(w, r, false)
}

// HostComplete is the host on-spot completion — host_id required, ownership
// re-checked (the paid path books here).
func (c *WalkInController) HostComplete(w http.ResponseWriter, r *http.Request) {
	c.complete(w, r, true)
}

func (c *WalkInController) complete(w http.ResponseWriter, r *http.Request, hostRequired bool) {
	var body walkInCompleteBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	guestID, err := uuid.Parse(body.GuestUserID)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid guest_user_id")
		return
	}
	eventID, err := uuid.Parse(body.EventID)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid event_id")
		return
	}
	hostID, err := parseScopeHostID(body.HostID, hostRequired)
	if err != nil {
		RespondError(w, http.StatusBadRequest, err.Error())
		return
	}
	occ, err := parseOptionalOccurrence(body.OccurrenceDate)
	if err != nil {
		RespondError(w, http.StatusBadRequest, err.Error())
		return
	}

	booking, err := c.walkInService.CompleteWalkIn(r.Context(), service.WalkInCompleteRequest{
		GuestUserID:       guestID,
		EventID:           eventID,
		Quantity:          body.Quantity,
		OccurrenceDate:    occ,
		RazorpayOrderID:   body.RazorpayOrderID,
		RazorpayPaymentID: body.RazorpayPaymentID,
		RazorpaySignature: body.RazorpaySignature,
		HostID:            hostID,
	})
	if err != nil {
		respondWalkInError(w, err)
		return
	}
	RespondSuccess(w, http.StatusCreated, booking)
}

// parseScopeHostID parses the optional host_id that scopes a walk-in to a host's
// own event. Required for host routes; empty (nil) for the admin flow.
func parseScopeHostID(raw string, required bool) (*uuid.UUID, error) {
	if raw == "" {
		if required {
			return nil, errors.New("host_id is required")
		}
		return nil, nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return nil, errors.New("Invalid host_id")
	}
	return &id, nil
}

func parseOptionalOccurrence(s string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, errors.New("Invalid occurrence_date format; expected RFC3339")
	}
	return &t, nil
}

func respondWalkInError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrWalkInDuplicate):
		RespondError(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, service.ErrWalkInNotOwner):
		RespondError(w, http.StatusForbidden, err.Error())
		return
	case errors.Is(err, service.ErrWalkInTiered):
		RespondError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	switch err.Error() {
	case "event not found":
		RespondError(w, http.StatusNotFound, err.Error())
	case "event capacity exceeded":
		RespondError(w, http.StatusConflict, err.Error())
	case "guest name is required", "guest phone number is required",
		"quantity must be at least 1", "a date is required for recurring events",
		"attendee details are required for this event",
		"invalid coupon code", "this coupon is no longer active",
		"this coupon is not valid yet", "this coupon has expired",
		"this coupon has reached its redemption limit",
		"you have already used this coupon",
		"that code only grants access, not a free booking":
		RespondError(w, http.StatusBadRequest, err.Error())
	default:
		RespondError(w, http.StatusInternalServerError, err.Error())
	}
}
