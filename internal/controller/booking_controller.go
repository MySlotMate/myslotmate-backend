package controller

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"myslotmate-backend/internal/lib/ratelimit"
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
		r.Post("/{bookingID}/ticket-notification", c.SendTicketNotification)
	})

	// Door check-in. Scoped under /hosts because every call is judged against
	// the event+occurrence the calling host is manning, not just the ticket.
	r.Route("/hosts/scan", func(r chi.Router) {
		r.Post("/verify", c.VerifyScannedTicket)
		r.Post("/check-in", c.CheckInScannedTicket)
	})
}

// ScanRequestBody is the door session (which host, which event, which date)
// plus the booking the camera just read. Count is only used on check-in.
type ScanRequestBody struct {
	HostID         uuid.UUID `json:"host_id"`
	BookingID      uuid.UUID `json:"booking_id"`
	EventID        uuid.UUID `json:"event_id"`
	OccurrenceDate time.Time `json:"occurrence_date"`
	Count          int       `json:"count,omitempty"`
}

func (b ScanRequestBody) toVerifyRequest() service.ScanVerifyRequest {
	return service.ScanVerifyRequest{
		HostID:         b.HostID,
		BookingID:      b.BookingID,
		EventID:        b.EventID,
		OccurrenceDate: b.OccurrenceDate,
	}
}

// decodeScanRequest reads and validates the session fields common to both scan
// endpoints. It reports whether the request was usable, responding itself when
// not.
func decodeScanRequest(w http.ResponseWriter, r *http.Request) (ScanRequestBody, bool) {
	var req ScanRequestBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return req, false
	}
	switch {
	case req.HostID == uuid.Nil:
		RespondError(w, http.StatusBadRequest, "host_id is required")
		return req, false
	case req.EventID == uuid.Nil:
		RespondError(w, http.StatusBadRequest, "event_id is required")
		return req, false
	case req.BookingID == uuid.Nil:
		RespondError(w, http.StatusBadRequest, "booking_id is required")
		return req, false
	case req.OccurrenceDate.IsZero():
		RespondError(w, http.StatusBadRequest, "occurrence_date is required")
		return req, false
	}
	return req, true
}

// VerifyScannedTicket judges a ticket without admitting anyone. A rejected
// ticket is still a successful request — the verdict is in the body, so the
// door screen can show why rather than a generic error.
func (c *BookingController) VerifyScannedTicket(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeScanRequest(w, r)
	if !ok {
		return
	}

	result, err := c.bookingService.VerifyScannedTicket(r.Context(), req.toVerifyRequest())
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, result)
}

// CheckInScannedTicket admits `count` guests against a scanned ticket. Callable
// repeatedly for one booking as a group arrives in waves, up to its quantity.
func (c *BookingController) CheckInScannedTicket(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeScanRequest(w, r)
	if !ok {
		return
	}
	if req.Count < 1 {
		RespondError(w, http.StatusBadRequest, "count must be at least 1")
		return
	}

	result, err := c.bookingService.CheckInScannedTicket(r.Context(), service.ScanCheckInRequest{
		ScanVerifyRequest: req.toVerifyRequest(),
		Count:             req.Count,
	})
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, result)
}

type CreateBookingRequest struct {
	UserID         uuid.UUID  `json:"user_id"`
	EventID        uuid.UUID  `json:"event_id"`
	Quantity       int        `json:"quantity"`
	OccurrenceDate string     `json:"occurrence_date,omitempty"`
	IdempotencyKey string     `json:"idempotency_key,omitempty"`
	PriceTierID    *uuid.UUID `json:"price_tier_id,omitempty"`
	// Passkey unlocks a private event (and comps it when the event opts in);
	// CouponCode is an optional comp code that waives the booking to free.
	Passkey    string `json:"passkey,omitempty"`
	CouponCode string `json:"coupon_code,omitempty"`
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
		PriceTierID:    req.PriceTierID,
		Passkey:        req.Passkey,
		CouponCode:     req.CouponCode,
		// Confirm in the same transaction so a paid booking can't get stuck at
		// `pending` if the separate confirm call fails. Notify the guest as before.
		AutoConfirm: true,
		Notify:      true,
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
		case "event capacity exceeded", "this coupon has reached its redemption limit":
			RespondError(w, http.StatusConflict, err.Error())
		case "your account is blocked due to suspicious activity":
			RespondError(w, http.StatusForbidden, err.Error())
		case "invalid passkey":
			// Throttle wrong-passkey attempts per IP+event too — otherwise
			// unlock throttling just pushes a brute-forcer to POST /bookings/,
			// which also compares the passkey. Each miss burns a token.
			if !ratelimit.Passkey.Allow(clientIP(r) + ":" + req.EventID.String()) {
				RespondError(w, http.StatusTooManyRequests, "Too many attempts. Please try again in a minute.")
				return
			}
			RespondError(w, http.StatusBadRequest, err.Error())
		case "invalid coupon code",
			"this coupon is no longer active",
			"this coupon is not valid yet",
			"this coupon has expired",
			"you have already used this coupon":
			RespondError(w, http.StatusBadRequest, err.Error())
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

func (c *BookingController) SendTicketNotification(w http.ResponseWriter, r *http.Request) {
	bookingID, err := uuid.Parse(chi.URLParam(r, "bookingID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid booking ID")
		return
	}

	// Parse multipart form (max 10MB memory)
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		RespondError(w, http.StatusBadRequest, "Failed to parse multipart form")
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Missing file field")
		return
	}
	defer file.Close()

	pdfBytes, err := io.ReadAll(file)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, "Failed to read file contents")
		return
	}

	err = c.bookingService.SendTicketNotification(r.Context(), bookingID, header.Filename, pdfBytes)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, map[string]interface{}{"success": true})
}
