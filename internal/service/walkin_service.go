package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"

	"github.com/google/uuid"
)

// WalkInService powers admin "on-spot" bookings: an admin enters a guest's name
// and phone, then collects payment via the Razorpay gateway on their own screen.
//
// Design — walk-in = guest wallet funded by Razorpay, then a normal booking
// debit. This deliberately reuses the entire existing money stack instead of
// forking a parallel gateway→booking path:
//
//  1. Find-or-create a lightweight guest user (matched by phone). The DB trigger
//     auto-creates their wallet account.
//  2. PAID events: create a Razorpay order via the existing top-up flow
//     (userService.InitiateTopUp) against the guest's wallet. The admin completes
//     checkout, then CompleteWalkIn verifies the payment (userService.VerifyTopUp,
//     which credits the wallet idempotently) and runs the standard
//     bookingService.CreateBooking — a pure wallet debit — then confirms silently.
//  3. FREE events: skip the gateway entirely; create + confirm the booking.
//
// Safety property: money lands in the guest's wallet BEFORE the booking debit, so
// if booking creation fails (capacity, etc.) the payment is never lost — it sits
// in the wallet and can be source-refunded by an admin.
type WalkInService interface {
	InitiateWalkIn(ctx context.Context, req WalkInInitiateRequest) (*WalkInInitiateResponse, error)
	CompleteWalkIn(ctx context.Context, req WalkInCompleteRequest) (*models.Booking, error)
	// LookupByPhone tells the admin UI whether a phone already belongs to a user
	// so it can auto-fill (and lock) the name before booking.
	LookupByPhone(ctx context.Context, phone string) (*WalkInGuestLookup, error)
}

// WalkInGuestLookup is the result of a phone lookup for the on-spot modal.
type WalkInGuestLookup struct {
	Exists bool      `json:"exists"`
	UserID uuid.UUID `json:"user_id,omitempty"`
	Name   string    `json:"name,omitempty"`
}

// WalkInInitiateRequest is the admin's input from the on-spot booking modal.
type WalkInInitiateRequest struct {
	GuestName      string
	GuestPhone     string
	EventID        uuid.UUID
	Quantity       int
	OccurrenceDate *time.Time // required for recurring events; ignored otherwise
}

// WalkInInitiateResponse tells the admin UI what to do next.
//
//   - Paid == false: a free booking was created + confirmed immediately; Booking is set.
//   - Paid == true:  open Razorpay checkout with the order fields, then call CompleteWalkIn.
type WalkInInitiateResponse struct {
	Paid           bool            `json:"paid"`
	Booking        *models.Booking `json:"booking,omitempty"`
	GuestUserID    uuid.UUID       `json:"guest_user_id"`
	OccurrenceDate time.Time       `json:"occurrence_date"`

	// Razorpay checkout fields (paid path only).
	OrderID     string `json:"order_id,omitempty"`
	KeyID       string `json:"key_id,omitempty"`
	AmountCents int64  `json:"amount_cents,omitempty"`
	Currency    string `json:"currency,omitempty"`
	PaymentID   string `json:"payment_id,omitempty"`
}

// WalkInCompleteRequest is sent after the admin finishes Razorpay checkout.
type WalkInCompleteRequest struct {
	GuestUserID       uuid.UUID
	EventID           uuid.UUID
	Quantity          int
	OccurrenceDate    *time.Time
	RazorpayOrderID   string
	RazorpayPaymentID string
	RazorpaySignature string
}

type walkInService struct {
	userRepo       repository.UserRepository
	bookingRepo    repository.BookingRepository
	eventRepo      repository.EventRepository
	userService    UserService
	bookingService BookingService
}

func NewWalkInService(
	ur repository.UserRepository,
	br repository.BookingRepository,
	er repository.EventRepository,
	us UserService,
	bs BookingService,
) WalkInService {
	return &walkInService{
		userRepo:       ur,
		bookingRepo:    br,
		eventRepo:      er,
		userService:    us,
		bookingService: bs,
	}
}

// ErrWalkInDuplicate is returned when the guest already holds an active booking
// for the same event occurrence.
var ErrWalkInDuplicate = errors.New("this guest already has a booking for this slot")

func (s *walkInService) InitiateWalkIn(ctx context.Context, req WalkInInitiateRequest) (*WalkInInitiateResponse, error) {
	name := strings.TrimSpace(req.GuestName)
	phone := strings.TrimSpace(req.GuestPhone)
	if name == "" {
		return nil, errors.New("guest name is required")
	}
	if phone == "" {
		return nil, errors.New("guest phone number is required")
	}
	if req.Quantity <= 0 {
		return nil, errors.New("quantity must be at least 1")
	}

	evt, err := s.eventRepo.GetByID(ctx, req.EventID)
	if err != nil {
		return nil, err
	}
	if evt == nil {
		return nil, errors.New("event not found")
	}

	occurrenceDate, err := resolveWalkInOccurrence(evt, req.OccurrenceDate)
	if err != nil {
		return nil, err
	}

	// Existing phone → reuse that account (admin UI auto-fills its name); new
	// phone → create a fresh email-less, phone-first account.
	guest, err := s.findOrCreateGuest(ctx, name, phone)
	if err != nil {
		return nil, err
	}

	// Duplicate guard — before charging anything.
	dup, err := s.bookingRepo.HasActiveBookingForOccurrence(ctx, guest.ID, req.EventID, occurrenceDate)
	if err != nil {
		return nil, err
	}
	if dup {
		return nil, ErrWalkInDuplicate
	}

	// Capacity guard — best-effort pre-check so we don't charge for a full slot.
	// CreateBooking re-checks atomically at booking time.
	booked, err := s.bookingRepo.GetTotalBookedQuantityForOccurrence(ctx, req.EventID, occurrenceDate)
	if err != nil {
		return nil, err
	}
	if booked+req.Quantity > evt.Capacity {
		return nil, errors.New("event capacity exceeded")
	}

	// Compute amount.
	var pricePerTicketCents int64
	if !evt.IsFree && evt.PriceCents != nil && *evt.PriceCents > 0 {
		pricePerTicketCents = *evt.PriceCents
	}
	totalAmount := pricePerTicketCents * int64(req.Quantity)

	// FREE event → create + confirm the booking immediately, no gateway.
	if totalAmount == 0 {
		// Unique per attempt — the duplicate guard above already prevents a guest
		// double-booking the same slot, so a fresh key here is safe.
		freeKey := "walkin_free_" + uuid.New().String()
		booking, err := s.createAndConfirm(ctx, guest.ID, req.EventID, req.Quantity, occurrenceDate, freeKey)
		if err != nil {
			return nil, err
		}
		return &WalkInInitiateResponse{
			Paid:           false,
			Booking:        booking,
			GuestUserID:    guest.ID,
			OccurrenceDate: occurrenceDate,
		}, nil
	}

	// PAID event → create a Razorpay order on the guest's wallet via the
	// existing top-up flow. The admin completes checkout, then calls CompleteWalkIn.
	order, err := s.userService.InitiateTopUp(ctx, guest.ID, TopUpRequest{
		AmountCents:    totalAmount,
		IdempotencyKey: fmt.Sprintf("walkin_topup_%s_%d", guest.ID, time.Now().UnixNano()),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create payment order: %w", err)
	}

	return &WalkInInitiateResponse{
		Paid:           true,
		GuestUserID:    guest.ID,
		OccurrenceDate: occurrenceDate,
		OrderID:        order.OrderID,
		KeyID:          order.KeyID,
		AmountCents:    order.AmountCents,
		Currency:       order.Currency,
		PaymentID:      order.PaymentID,
	}, nil
}

func (s *walkInService) CompleteWalkIn(ctx context.Context, req WalkInCompleteRequest) (*models.Booking, error) {
	if req.GuestUserID == uuid.Nil {
		return nil, errors.New("guest_user_id is required")
	}
	if req.Quantity <= 0 {
		return nil, errors.New("quantity must be at least 1")
	}
	if req.RazorpayOrderID == "" || req.RazorpayPaymentID == "" || req.RazorpaySignature == "" {
		return nil, errors.New("razorpay payment details are required")
	}

	evt, err := s.eventRepo.GetByID(ctx, req.EventID)
	if err != nil {
		return nil, err
	}
	if evt == nil {
		return nil, errors.New("event not found")
	}
	occurrenceDate, err := resolveWalkInOccurrence(evt, req.OccurrenceDate)
	if err != nil {
		return nil, err
	}

	// Re-check the duplicate guard in case CompleteWalkIn is replayed.
	dup, err := s.bookingRepo.HasActiveBookingForOccurrence(ctx, req.GuestUserID, req.EventID, occurrenceDate)
	if err != nil {
		return nil, err
	}
	if dup {
		return nil, ErrWalkInDuplicate
	}

	// Verify the gateway payment and credit the guest wallet (idempotent; closes
	// the verify-vs-webhook race via the ledger's unique idempotency key).
	if _, err := s.userService.VerifyTopUp(ctx, req.GuestUserID, VerifyTopUpRequest{
		RazorpayOrderID:   req.RazorpayOrderID,
		RazorpayPaymentID: req.RazorpayPaymentID,
		RazorpaySignature: req.RazorpaySignature,
	}); err != nil {
		return nil, fmt.Errorf("payment verification failed: %w", err)
	}

	// Now the wallet holds exactly the booking amount — run the standard booking
	// debit + confirm it silently. The idempotency key is tied to the Razorpay
	// order, so a retry of the SAME payment is idempotent, but each distinct
	// payment books (a deterministic guest+event+date key would collide across
	// separate attempts and silently skip the booking).
	paidKey := "walkin_paid_" + req.RazorpayOrderID
	return s.createAndConfirm(ctx, req.GuestUserID, req.EventID, req.Quantity, occurrenceDate, paidKey)
}

// createAndConfirm runs the normal booking debit and confirms it in the same
// transaction (AutoConfirm), without notifying the guest (synthetic contact).
func (s *walkInService) createAndConfirm(ctx context.Context, guestID, eventID uuid.UUID, quantity int, occurrenceDate time.Time, idempotencyKey string) (*models.Booking, error) {
	occ := occurrenceDate
	return s.bookingService.CreateBooking(ctx, guestID, BookingCreateRequest{
		EventID:        eventID,
		Quantity:       quantity,
		OccurrenceDate: &occ,
		IdempotencyKey: idempotencyKey,
		AutoConfirm:    true,
		Notify:         false,
	})
}

// LookupByPhone returns whether a user already exists for this phone, plus their
// name, so the on-spot modal can auto-fill (and lock) it before booking.
func (s *walkInService) LookupByPhone(ctx context.Context, phone string) (*WalkInGuestLookup, error) {
	u, err := s.userRepo.GetByPhone(ctx, strings.TrimSpace(phone))
	if err != nil {
		return nil, err
	}
	if u == nil {
		return &WalkInGuestLookup{Exists: false}, nil
	}
	return &WalkInGuestLookup{Exists: true, UserID: u.ID, Name: u.Name}, nil
}

// findOrCreateGuest returns the existing user for this phone (booking attaches to
// it; the admin UI shows its name), or creates a fresh phone-first account.
//
// New accounts have NO email — they are claimable later via phone login (Phase 2).
// They carry a placeholder auth_uid (walkin_<uuid>) so they don't collide with
// Firebase-authenticated users until claimed.
func (s *walkInService) findOrCreateGuest(ctx context.Context, name, phone string) (*models.User, error) {
	existing, err := s.userRepo.GetByPhone(ctx, phone)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}

	id := uuid.New()
	now := time.Now()
	guest := &models.User{
		ID:        id,
		AuthUID:   "walkin_" + id.String(),
		Name:      name,
		PhnNumber: phone,
		Email:     "", // no email — phone-first account, claimable later
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.userRepo.Create(ctx, guest); err != nil {
		return nil, fmt.Errorf("failed to create guest: %w", err)
	}
	return guest, nil
}

// resolveWalkInOccurrence determines the booking's occurrence datetime. For
// non-recurring events it is always the event's own time (any admin input is
// ignored to avoid a mismatch with capacity tracking). For recurring events the
// admin must supply the specific occurrence date.
func resolveWalkInOccurrence(evt *models.Event, supplied *time.Time) (time.Time, error) {
	if !evt.IsRecurring {
		return evt.Time, nil
	}
	if supplied == nil {
		return time.Time{}, errors.New("a date is required for recurring events")
	}
	return *supplied, nil
}
