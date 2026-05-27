package controller

import (
	"encoding/json"
	"net/http"
	"time"

	"myslotmate-backend/internal/service"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type BookingController struct {
	bookingService service.BookingService
}

func NewBookingController(s service.BookingService) *BookingController {
	return &BookingController{bookingService: s}
}

func (c *BookingController) RegisterRoutes(r chi.Router) {
	r.Route("/bookings", func(r chi.Router) {
		r.Post("/", c.CreateBooking)
		r.Post("/{bookingID}/confirm", c.ConfirmBooking)
		r.Post("/{bookingID}/cancel", c.CancelBooking)
		r.Get("/{bookingID}", c.GetBooking)
		r.Get("/user/{userID}", c.GetUserBookings)
	})
}

type CreateBookingRequest struct {
	UserID         uuid.UUID `json:"user_id"`
	EventID        uuid.UUID `json:"event_id"`
	Quantity       int       `json:"quantity"`
	OccurrenceDate string    `json:"occurrence_date,omitempty"`
	IdempotencyKey string    `json:"idempotency_key,omitempty"`
}

func (c *BookingController) CreateBooking(w http.ResponseWriter, r *http.Request) {
	var req CreateBookingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	svcReq := service.BookingCreateRequest{
		EventID:        req.EventID,
		Quantity:       req.Quantity,
		IdempotencyKey: req.IdempotencyKey,
	}

	// Parse occurrence_date if provided (for recurring events)
	if req.OccurrenceDate != "" {
		t, err := time.Parse(time.RFC3339, req.OccurrenceDate)
		if err != nil {
			RespondError(w, http.StatusBadRequest, "Invalid occurrence_date format; expected RFC3339")
			return
		}
		svcReq.OccurrenceDate = &t
	}

	booking, err := c.bookingService.CreateBooking(r.Context(), req.UserID, svcReq)
	if err != nil {
		switch err.Error() {
		case "insufficient wallet balance; please top up first":
			RespondError(w, http.StatusPaymentRequired, err.Error())
		case "event not found", "user account not found":
			RespondError(w, http.StatusNotFound, err.Error())
		case "event capacity exceeded":
			RespondError(w, http.StatusConflict, err.Error())
		case "your account is blocked due to suspicious activity":
			RespondError(w, http.StatusForbidden, err.Error())
		default:
			RespondError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	RespondSuccess(w, http.StatusCreated, booking)
}


func (c *BookingController) GetUserBookings(w http.ResponseWriter, r *http.Request) {
	userIDStr := chi.URLParam(r, "userID")
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid user ID")
		return
	}

	bookings, err := c.bookingService.GetUserBookings(r.Context(), userID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, bookings)
}

func (c *BookingController) ConfirmBooking(w http.ResponseWriter, r *http.Request) {
	bookingID, err := uuid.Parse(chi.URLParam(r, "bookingID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid booking ID")
		return
	}

	booking, err := c.bookingService.ConfirmBooking(r.Context(), bookingID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, booking)
}

func (c *BookingController) CancelBooking(w http.ResponseWriter, r *http.Request) {
	bookingID, err := uuid.Parse(chi.URLParam(r, "bookingID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid booking ID")
		return
	}

	var body struct {
		UserID            uuid.UUID `json:"user_id"`
		RefundDestination string    `json:"refund_destination"` // "wallet" (default) | "source"
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	dest := service.RefundDestinationWallet
	if body.RefundDestination == string(service.RefundDestinationSource) {
		dest = service.RefundDestinationSource
	}

	booking, err := c.bookingService.CancelBooking(r.Context(), bookingID, body.UserID, dest)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, booking)
}

func (c *BookingController) GetBooking(w http.ResponseWriter, r *http.Request) {
	bookingID, err := uuid.Parse(chi.URLParam(r, "bookingID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid booking ID")
		return
	}

	booking, err := c.bookingService.GetBooking(r.Context(), bookingID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, booking)
}
