package controller

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"myslotmate-backend/internal/auth"
	"myslotmate-backend/internal/service"

	fbauth "firebase.google.com/go/v4/auth"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// WalkInController exposes admin-only "on-spot" booking endpoints: an admin
// enters a guest's name + phone, then collects payment via Razorpay on-screen.
type WalkInController struct {
	walkInService service.WalkInService
	firebaseAuth  *fbauth.Client
	adminEmail    string
	jwtSecret     string
}

func NewWalkInController(ws service.WalkInService, fa *fbauth.Client, adminEmail, jwtSecret string) *WalkInController {
	return &WalkInController{
		walkInService: ws,
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

type walkInInitiateBody struct {
	GuestName      string `json:"guest_name"`
	GuestPhone     string `json:"guest_phone"`
	EventID        string `json:"event_id"`
	Quantity       int    `json:"quantity"`
	OccurrenceDate string `json:"occurrence_date,omitempty"` // RFC3339
}

func (c *WalkInController) Initiate(w http.ResponseWriter, r *http.Request) {
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

	occ, err := parseOptionalOccurrence(body.OccurrenceDate)
	if err != nil {
		RespondError(w, http.StatusBadRequest, err.Error())
		return
	}

	resp, err := c.walkInService.InitiateWalkIn(r.Context(), service.WalkInInitiateRequest{
		GuestName:      body.GuestName,
		GuestPhone:     body.GuestPhone,
		EventID:        eventID,
		Quantity:       body.Quantity,
		OccurrenceDate: occ,
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
}

func (c *WalkInController) Complete(w http.ResponseWriter, r *http.Request) {
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
	})
	if err != nil {
		respondWalkInError(w, err)
		return
	}
	RespondSuccess(w, http.StatusCreated, booking)
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
	}
	switch err.Error() {
	case "event not found":
		RespondError(w, http.StatusNotFound, err.Error())
	case "event capacity exceeded":
		RespondError(w, http.StatusConflict, err.Error())
	case "guest name is required", "guest phone number is required",
		"quantity must be at least 1", "a date is required for recurring events":
		RespondError(w, http.StatusBadRequest, err.Error())
	default:
		RespondError(w, http.StatusInternalServerError, err.Error())
	}
}
