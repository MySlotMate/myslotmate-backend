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
	// LookupByPhoneForHost is the same lookup for the host on-spot modal, gated
	// on the caller owning the event they are booking for. Phone→name is exactly
	// the kind of thing that invites account enumeration, so the host variant is
	// deliberately narrower than the admin one: the caller must be an
	// authenticated host (enforced by middleware) AND own this event — i.e. they
	// could already have booked this guest onto it and read the name back off
	// the initiate response.
	LookupByPhoneForHost(ctx context.Context, hostID, eventID uuid.UUID, phone string) (*WalkInGuestLookup, error)
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
	// CouponCode, when a valid free-booking code, comps the walk-in to ₹0 and
	// books immediately (no payment step). CreateBooking validates + redeems it.
	CouponCode string
	// AttendeeDetails carries the extra attendee-profile answers the admin
	// collected for events that require them. Nil when the event needs none.
	// Only its non-nil fields are upserted onto the guest's profile, so the
	// booking gate (which checks the stored profile) passes.
	AttendeeDetails *models.AttendeeProfile
	// HostID scopes the request to a host doing an on-spot booking on their own
	// event: when set, the event must belong to this host. Nil for the admin
	// flow, which can book any event.
	HostID *uuid.UUID
	// PaymentCollectedOffline marks a paid event whose fee the host collected
	// outside the platform (bulk import only — the interactive modal always takes
	// payment through Razorpay). Books at ₹0 with no ledger or host earnings; see
	// BookingCreateRequest.PaymentCollectedOffline.
	PaymentCollectedOffline bool
	// Source/ImportJobID tag the resulting booking for reporting. Source defaults
	// to models.BookingSourceWalkIn when unset.
	Source      string
	ImportJobID *uuid.UUID
	// IdempotencyKey overrides the random key the free path generates. The
	// interactive modal leaves it empty (each click is a distinct attempt), but
	// the bulk importer sets a key derived from job+row so a job re-driven after
	// a process restart re-books nothing. Ignored on the paid path, whose key is
	// already pinned to the Razorpay order.
	IdempotencyKey string
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

	// GuestName is the name actually on the account the booking was attached to,
	// and GuestExisting reports whether that account already existed for this
	// phone. Together they let the host UI say "booked for <name>" after the
	// fact, without a lookup endpoint that would allow phone→name enumeration
	// (see the note on the route registration in walkin_controller.go). The
	// caller already proved event ownership, so this leaks nothing new.
	GuestName     string `json:"guest_name"`
	GuestExisting bool   `json:"guest_existing"`

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
	// HostID scopes completion to the owning host (host on-spot flow); nil for
	// the admin flow. Re-checked here because the paid path books in Complete.
	HostID *uuid.UUID
}

// ErrWalkInNotOwner is returned when a host tries to on-spot book an event that
// isn't theirs.
var ErrWalkInNotOwner = errors.New("not authorized for this event")

// verifyHostOwnership enforces that a host-scoped request targets the host's own
// event. A nil hostID (admin flow) always passes.
func verifyHostOwnership(evt *models.Event, hostID *uuid.UUID) error {
	if hostID == nil {
		return nil
	}
	if evt.HostID != *hostID {
		return ErrWalkInNotOwner
	}
	return nil
}

type walkInService struct {
	userRepo       repository.UserRepository
	bookingRepo    repository.BookingRepository
	eventRepo      repository.EventRepository
	tierRepo       repository.EventPriceTierRepository
	attendeeRepo   repository.AttendeeProfileRepository
	userService    UserService
	bookingService BookingService
}

func NewWalkInService(
	ur repository.UserRepository,
	br repository.BookingRepository,
	er repository.EventRepository,
	tr repository.EventPriceTierRepository,
	apr repository.AttendeeProfileRepository,
	us UserService,
	bs BookingService,
) WalkInService {
	return &walkInService{
		userRepo:       ur,
		bookingRepo:    br,
		eventRepo:      er,
		tierRepo:       tr,
		attendeeRepo:   apr,
		userService:    us,
		bookingService: bs,
	}
}

// ErrWalkInTiered is returned when an admin tries to walk-in book an event that
// uses ticket tiers. Tier selection isn't supported in the walk-in flow yet.
var ErrWalkInTiered = errors.New("walk-in booking is not yet supported for events with ticket tiers")

// rejectIfTiered guards the walk-in flow against events that have active tiers,
// which the walk-in path can't price (it has no tier selection).
func (s *walkInService) rejectIfTiered(ctx context.Context, eventID uuid.UUID) error {
	tiers, err := s.tierRepo.ListByEventID(ctx, eventID)
	if err != nil {
		return err
	}
	if len(tiers) > 0 {
		return ErrWalkInTiered
	}
	return nil
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
	if err := verifyHostOwnership(evt, req.HostID); err != nil {
		return nil, err
	}
	if err := s.rejectIfTiered(ctx, req.EventID); err != nil {
		return nil, err
	}

	occurrenceDate, err := resolveWalkInOccurrence(evt, req.OccurrenceDate)
	if err != nil {
		return nil, err
	}

	// Existing phone → reuse that account (admin UI auto-fills its name); new
	// phone → create a fresh email-less, phone-first account.
	guest, guestExisted, err := s.findOrCreateGuest(ctx, name, phone)
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

	// Attendee-details gate — if the event requires extra attendee details, upsert
	// the admin-collected answers onto the guest's profile now, BEFORE the booking
	// debit. The free path books inside this call and the paid path books in
	// CompleteWalkIn; upserting here (the guest already exists) satisfies the gate
	// in bookingService.CreateBooking for both. Missing/blank fields still fail the
	// gate — the admin UI collects them, matching the customer booking form.
	if evt.RequiresAttendeeDetails && len(evt.AttendeeFields) > 0 {
		if req.AttendeeDetails == nil {
			return nil, errors.New("attendee details are required for this event")
		}
		profile := *req.AttendeeDetails
		profile.UserID = guest.ID
		if err := s.attendeeRepo.Upsert(ctx, &profile); err != nil {
			return nil, fmt.Errorf("failed to save attendee details: %w", err)
		}
	}

	// Compute amount.
	var pricePerTicketCents int64
	if !evt.IsFree && evt.PriceCents != nil && *evt.PriceCents > 0 {
		pricePerTicketCents = *evt.PriceCents
	}
	totalAmount := pricePerTicketCents * int64(req.Quantity)
	couponCode := strings.TrimSpace(req.CouponCode)

	// A coupon on a paid event is verified up-front so the admin gets a precise
	// message: an unknown/expired/exhausted code, or one that only grants access
	// (not a free booking), is rejected here rather than failing later with a
	// confusing "insufficient balance". CreateBooking re-checks and atomically
	// redeems it, so this preview never double-spends the code.
	if totalAmount > 0 && couponCode != "" {
		coupon, err := s.bookingService.ValidateCoupon(ctx, req.EventID, guest.ID, couponCode)
		if err != nil {
			return nil, err
		}
		if !coupon.GrantsFree {
			return nil, errors.New("that code only grants access, not a free booking")
		}
	}

	// FREE event, a coupon that comps it, or a paid event whose fee the host
	// already collected offline → create + confirm immediately, no gateway.
	// CreateBooking validates + redeems the coupon and waives the price.
	if totalAmount == 0 || couponCode != "" || req.PaymentCollectedOffline {
		// Unique per attempt — the duplicate guard above already prevents a guest
		// double-booking the same slot, so a fresh key here is safe. Callers that
		// need to retry an identical attempt safely (the bulk importer, resuming a
		// job after a restart) supply their own stable key instead.
		freeKey := strings.TrimSpace(req.IdempotencyKey)
		if freeKey == "" {
			freeKey = "walkin_free_" + uuid.New().String()
		}
		booking, err := s.createAndConfirm(ctx, guest.ID, req.EventID, req.Quantity, occurrenceDate, freeKey, couponCode, walkInBookingTags{
			Source:                  req.Source,
			ImportJobID:             req.ImportJobID,
			PaymentCollectedOffline: req.PaymentCollectedOffline,
		})
		if err != nil {
			return nil, err
		}
		return &WalkInInitiateResponse{
			Paid:           false,
			Booking:        booking,
			GuestUserID:    guest.ID,
			OccurrenceDate: occurrenceDate,
			GuestName:      guest.Name,
			GuestExisting:  guestExisted,
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
		GuestName:      guest.Name,
		GuestExisting:  guestExisted,
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
	if err := verifyHostOwnership(evt, req.HostID); err != nil {
		return nil, err
	}
	if err := s.rejectIfTiered(ctx, req.EventID); err != nil {
		return nil, err
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
	return s.createAndConfirm(ctx, req.GuestUserID, req.EventID, req.Quantity, occurrenceDate, paidKey, "", walkInBookingTags{})
}

// walkInBookingTags carries the reporting/money flags an on-spot or imported
// booking needs. Zero value = a plain on-spot booking that paid normally.
type walkInBookingTags struct {
	Source                  string
	ImportJobID             *uuid.UUID
	PaymentCollectedOffline bool
}

// createAndConfirm runs the normal booking debit and confirms it in the same
// transaction (AutoConfirm), without notifying the guest (synthetic contact).
func (s *walkInService) createAndConfirm(ctx context.Context, guestID, eventID uuid.UUID, quantity int, occurrenceDate time.Time, idempotencyKey, couponCode string, tags walkInBookingTags) (*models.Booking, error) {
	occ := occurrenceDate
	source := tags.Source
	if source == "" {
		source = models.BookingSourceWalkIn
	}
	return s.bookingService.CreateBooking(ctx, guestID, BookingCreateRequest{
		EventID:        eventID,
		Quantity:       quantity,
		OccurrenceDate: &occ,
		IdempotencyKey: idempotencyKey,
		CouponCode:     couponCode,
		AutoConfirm:    true,
		Notify:         false,
		// Host-driven on-spot booking for their own event — skip the guest passkey gate.
		BypassPasskey:           true,
		Source:                  source,
		ImportJobID:             tags.ImportJobID,
		PaymentCollectedOffline: tags.PaymentCollectedOffline,
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

// LookupByPhoneForHost gates LookupByPhone on the caller owning the event. The
// host ID must come from the auth token, never from the request body — see the
// controller.
func (s *walkInService) LookupByPhoneForHost(ctx context.Context, hostID, eventID uuid.UUID, phone string) (*WalkInGuestLookup, error) {
	evt, err := s.eventRepo.GetByID(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if evt == nil {
		return nil, errors.New("event not found")
	}
	if err := verifyHostOwnership(evt, &hostID); err != nil {
		return nil, err
	}
	return s.LookupByPhone(ctx, phone)
}

// findOrCreateGuest returns the existing user for this phone (booking attaches to
// it; the admin UI shows its name), or creates a fresh phone-first account.
//
// New accounts have NO email — they are claimable later via phone login (Phase 2).
// They carry a placeholder auth_uid (walkin_<uuid>) so they don't collide with
// Firebase-authenticated users until claimed.
// The second return value reports whether the account already existed, so the
// caller can tell the host their guest was matched to a known account rather
// than newly created.
func (s *walkInService) findOrCreateGuest(ctx context.Context, name, phone string) (*models.User, bool, error) {
	existing, err := s.userRepo.GetByPhone(ctx, phone)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		return existing, true, nil
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
		return nil, false, fmt.Errorf("failed to create guest: %w", err)
	}
	return guest, false, nil
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
