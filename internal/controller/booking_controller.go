package controller

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"myslotmate-backend/internal/auth"
	"myslotmate-backend/internal/lib/ratelimit"
	"myslotmate-backend/internal/repository"
	"myslotmate-backend/internal/service"

	fbauth "firebase.google.com/go/v4/auth"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type BookingController struct {
	bookingService service.BookingService
	// Identity lookups. These routes move money, so the caller is resolved from
	// their token and never from the request body.
	userRepo     repository.UserRepository
	hostRepo     repository.HostRepository
	eventRepo    repository.EventRepository
	firebaseAuth *fbauth.Client
	jwtSecret    string
	adminEmail   string
}

func NewBookingController(s service.BookingService) *BookingController {
	return &BookingController{bookingService: s}
}

// WithAuth attaches the identity lookups. Separate from the constructor so the
// existing NewBookingController callers keep compiling.
func (c *BookingController) WithAuth(
	ur repository.UserRepository,
	hr repository.HostRepository,
	er repository.EventRepository,
	fa *fbauth.Client,
	jwtSecret, adminEmail string,
) *BookingController {
	c.userRepo = ur
	c.hostRepo = hr
	c.eventRepo = er
	c.firebaseAuth = fa
	c.jwtSecret = jwtSecret
	c.adminEmail = adminEmail
	return c
}

// callerUserID resolves the signed-in user. Fails closed: these routes debit
// wallets, so an unresolvable caller is refused rather than guessed at.
func (c *BookingController) callerUserID(r *http.Request) (uuid.UUID, error) {
	uid, _ := r.Context().Value(auth.ContextKeyUID).(string)
	if uid == "" || c.userRepo == nil {
		return uuid.Nil, errors.New("sign in to do that")
	}
	user, err := c.userRepo.GetByAuthUID(r.Context(), uid)
	if err != nil {
		return uuid.Nil, err
	}
	if user == nil {
		return uuid.Nil, errors.New("user not found")
	}
	return user.ID, nil
}

// assertBookingAccess allows the guest who holds the booking, the host running
// the event, and an admin. Anyone else gets 404: whether a booking id exists is
// not something a stranger needs to learn.
func (c *BookingController) assertBookingAccess(r *http.Request, bookingID uuid.UUID) (uuid.UUID, error) {
	if auth.IsAdminCaller(r) {
		return uuid.Nil, nil
	}
	userID, err := c.callerUserID(r)
	if err != nil {
		return uuid.Nil, err
	}

	booking, err := c.bookingService.GetBooking(r.Context(), bookingID)
	if err != nil || booking == nil {
		return uuid.Nil, errNotYours
	}
	if booking.UserID == userID {
		return userID, nil
	}

	// The host running the event may act on its bookings — that is how a door
	// resend and a host-side cancellation work.
	if c.hostRepo != nil && c.eventRepo != nil {
		if host, herr := c.hostRepo.GetByUserID(r.Context(), userID); herr == nil && host != nil {
			if okManage, merr := c.eventRepo.HostCanManage(r.Context(), booking.EventID, host.ID); merr == nil && okManage {
				return userID, nil
			}
		}
	}
	return uuid.Nil, errNotYours
}

// errNotYours is answered as 404 on purpose — see assertBookingAccess.
var errNotYours = errors.New("booking not found")

func (c *BookingController) RegisterRoutes(r chi.Router) {
	// Every booking route is authenticated. These were public, and
	// CreateBooking took the buyer's id from the request body — so anyone could
	// book as anyone else and spend that person's wallet. Cancel had the same
	// shape, and the reads exposed guests' tickets and booking history.
	//
	// RequireUserOrAdmin, not RequireUser: the admin dashboard resends tickets,
	// and its session token carries a different issuer that RequireUser rejects.
	r.Route("/bookings", func(r chi.Router) {
		r.Use(auth.RequireUserOrAdmin(c.firebaseAuth, c.adminEmail, c.jwtSecret))

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

	// The buyer is the signed-in user. A body user_id that disagrees is refused
	// rather than honoured — that field is what let one account spend another's
	// wallet, and clients still send it.
	buyerID, err := c.callerUserID(r)
	if err != nil {
		RespondError(w, http.StatusUnauthorized, err.Error())
		return
	}
	if req.UserID != uuid.Nil && req.UserID != buyerID {
		RespondError(w, http.StatusForbidden, "user_id does not belong to the signed-in user")
		return
	}

	booking, err := c.bookingService.CreateBooking(r.Context(), buyerID, svcReq)
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

	// Own history only. An admin may read anyone's.
	if !auth.IsAdminCaller(r) {
		callerID, cerr := c.callerUserID(r)
		if cerr != nil {
			RespondError(w, http.StatusUnauthorized, cerr.Error())
			return
		}
		if callerID != userID {
			RespondError(w, http.StatusForbidden, "you can only read your own bookings")
			return
		}
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

	if _, aerr := c.assertBookingAccess(r, bookingID); aerr != nil {
		RespondError(w, http.StatusNotFound, "Booking not found")
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

	// The service checks the booking belongs to this user; passing the body's
	// user_id made that check meaningless. Resolve it here instead.
	ownerID, aerr := c.assertBookingAccess(r, bookingID)
	if aerr != nil {
		RespondError(w, http.StatusNotFound, "Booking not found")
		return
	}
	if ownerID == uuid.Nil {
		// Admin caller: cancel as the booking's own owner.
		existing, gerr := c.bookingService.GetBooking(r.Context(), bookingID)
		if gerr != nil || existing == nil {
			RespondError(w, http.StatusNotFound, "Booking not found")
			return
		}
		ownerID = existing.UserID
	}

	booking, err := c.bookingService.CancelBooking(r.Context(), bookingID, ownerID, dest)
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

	if _, aerr := c.assertBookingAccess(r, bookingID); aerr != nil {
		RespondError(w, http.StatusNotFound, "Booking not found")
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

	if _, aerr := c.assertBookingAccess(r, bookingID); aerr != nil {
		RespondError(w, http.StatusNotFound, "Booking not found")
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
