package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"myslotmate-backend/internal/lib/event"
	"myslotmate-backend/internal/lib/notification"
	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"

	"github.com/google/uuid"
)

type BookingService interface {
	CreateBooking(ctx context.Context, userID uuid.UUID, req BookingCreateRequest) (*models.Booking, error)
	ConfirmBooking(ctx context.Context, bookingID uuid.UUID) (*models.Booking, error)
	// ConfirmBookingSilent confirms a booking without sending the guest any
	// WhatsApp/email confirmation. Used by admin on-spot bookings, whose guest
	// has only a synthetic contact on file.
	ConfirmBookingSilent(ctx context.Context, bookingID uuid.UUID) (*models.Booking, error)
	CancelBooking(ctx context.Context, bookingID uuid.UUID, userID uuid.UUID, refundDestination RefundDestination) (*models.Booking, error)
	CancelBookingByHost(ctx context.Context, bookingID uuid.UUID) (*models.Booking, error)
	GetUserBookings(ctx context.Context, userID uuid.UUID) ([]*models.Booking, error)
	GetBooking(ctx context.Context, bookingID uuid.UUID) (*models.Booking, error)
	SendTicketNotification(ctx context.Context, bookingID uuid.UUID, fileName string, pdfBytes []byte) error
}

type BookingCreateRequest struct {
	EventID        uuid.UUID
	Quantity       int
	IdempotencyKey string
	OccurrenceDate *time.Time // which specific date the user is booking for
	PriceTierID    *uuid.UUID // chosen ticket tier; required when the event has tiers

	// AutoConfirm makes the booking commit as `confirmed` in the same transaction
	// instead of `pending`, removing the fragile separate confirm call that could
	// leave a paid booking stuck at pending. Notify controls whether the guest
	// gets the WhatsApp/email confirmation (false for admin on-spot bookings).
	AutoConfirm bool
	Notify      bool
}

// RefundDestination is where the refund money ends up on a user-initiated
// booking cancellation. "wallet" (default) keeps it in the user's wallet —
// the F4 flow alone. "source" additionally chains a Razorpay source-refund
// against the user's most-recent refundable top-up so the money goes back
// to the original card/UPI. See skill flows.md §4.
type RefundDestination string

const (
	RefundDestinationWallet RefundDestination = "wallet"
	RefundDestinationSource RefundDestination = "source"
)

type bookingService struct {
	db                  *sql.DB
	bookingRepo         repository.BookingRepository
	eventRepo           repository.EventRepository
	accountRepo         repository.AccountRepository
	paymentRepo         repository.PaymentRepository
	payoutRepo          repository.PayoutRepository
	hostRepo            repository.HostRepository
	userRepo            repository.UserRepository
	ledgerRepo          repository.TransactionLedgerRepository
	tierRepo            repository.EventPriceTierRepository
	attendeeRepo        repository.AttendeeProfileRepository
	userService         UserService // for chaining source-refund on cancel
	dispatcher          *event.Dispatcher
	notificationService notification.NotificationService
}

func NewBookingService(
	db *sql.DB,
	br repository.BookingRepository,
	er repository.EventRepository,
	ar repository.AccountRepository,
	pmr repository.PaymentRepository,
	pr repository.PayoutRepository,
	hr repository.HostRepository,
	ur repository.UserRepository,
	lr repository.TransactionLedgerRepository,
	tr repository.EventPriceTierRepository,
	apr repository.AttendeeProfileRepository,
	us UserService,
	d *event.Dispatcher,
	ns notification.NotificationService,
) BookingService {
	return &bookingService{
		db:                  db,
		bookingRepo:         br,
		eventRepo:           er,
		accountRepo:         ar,
		paymentRepo:         pmr,
		payoutRepo:          pr,
		hostRepo:            hr,
		userRepo:            ur,
		ledgerRepo:          lr,
		tierRepo:            tr,
		attendeeRepo:        apr,
		userService:         us,
		dispatcher:          d,
		notificationService: ns,
	}
}

func (s *bookingService) CreateBooking(ctx context.Context, userID uuid.UUID, req BookingCreateRequest) (*models.Booking, error) {
	idempotencyKey := req.IdempotencyKey
	if idempotencyKey == "" {
		idempotencyKey = fmt.Sprintf("booking_%s_%d", userID, time.Now().UnixNano())
	}

	// ── Validation & lookups — read-only, before the transaction opens ───

	// 1. Idempotency check via ledger (prevents duplicate ledger entries).
	actualLedger, err := s.ledgerRepo.GetByIdempotencyKey(ctx, idempotencyKey)
	if err != nil {
		return nil, err
	}
	if actualLedger != nil && actualLedger.ReferenceID != nil {
		// Already processed this booking, return the original.
		return s.bookingRepo.GetByID(ctx, *actualLedger.ReferenceID)
	}

	// 2. Fraud check
	flagged, err := s.payoutRepo.HasActiveFraudFlag(ctx, userID)
	if err != nil {
		return nil, err
	}
	if flagged {
		return nil, errors.New("your account is blocked due to suspicious activity")
	}

	// 3. Validate event exists
	evt, err := s.eventRepo.GetByID(ctx, req.EventID)
	if err != nil {
		return nil, err
	}
	if evt == nil {
		return nil, errors.New("event not found")
	}

	// 3b. Attendee-details gate — if this event requires extra attendee details,
	// the guest's saved attendee profile must have every required field. This is
	// the server-side guard behind the booking-form; the client collects + upserts
	// the profile before calling this.
	if evt.RequiresAttendeeDetails && len(evt.AttendeeFields) > 0 {
		profile, err := s.attendeeRepo.GetByUserID(ctx, userID)
		if err != nil {
			return nil, err
		}
		for _, field := range evt.AttendeeFields {
			if !profile.HasField(field) {
				return nil, errors.New("please complete attendee details before booking")
			}
		}
	}

	// 4. Resolve occurrence date
	// For recurring events the frontend sends the specific date the user picked.
	// For non-recurring events (or if omitted) we fall back to the event's own time.
	occurrenceDate := evt.Time
	if req.OccurrenceDate != nil {
		occurrenceDate = *req.OccurrenceDate
	}

	// 5. Overbooking prevention — check capacity per occurrence date.
	// NOTE: this is still a check-then-insert race (bug C4). Wrapping the writes
	// in a transaction gives atomicity but does NOT serialise two concurrent
	// capacity checks — closing C4 needs a SELECT ... FOR UPDATE on the event row.
	currentBooked, err := s.bookingRepo.GetTotalBookedQuantityForOccurrence(ctx, req.EventID, occurrenceDate)
	if err != nil {
		return nil, err
	}
	if currentBooked+req.Quantity > evt.Capacity {
		return nil, errors.New("event capacity exceeded")
	}

	// 6. Price calculation. The unit price is always server-derived — the client
	//    only chooses WHICH tier, never the amount. Resolution order:
	//    - a tier was chosen → validate it belongs to this event & is active, use it;
	//    - the event has tiers but none chosen → reject (must pick a ticket type);
	//    - no tiers → legacy single event price.
	var pricePerTicketCents int64 = 0
	var priceTierID *uuid.UUID
	if req.PriceTierID != nil {
		tier, err := s.tierRepo.GetByID(ctx, *req.PriceTierID)
		if err != nil {
			return nil, err
		}
		if tier == nil || tier.EventID != evt.ID || !tier.IsActive {
			return nil, errors.New("invalid ticket tier for this event")
		}
		pricePerTicketCents = tier.PriceCents
		priceTierID = &tier.ID
	} else {
		tiers, err := s.tierRepo.ListByEventID(ctx, evt.ID)
		if err != nil {
			return nil, err
		}
		if len(tiers) > 0 {
			return nil, errors.New("please select a ticket type")
		}
		if evt.PriceCents != nil && *evt.PriceCents > 0 {
			pricePerTicketCents = *evt.PriceCents
		}
	}
	totalAmount := pricePerTicketCents * int64(req.Quantity)
	unitPriceCents := pricePerTicketCents

	// 7. Get user account (must exist)
	userAccount, err := s.accountRepo.GetByOwner(ctx, models.AccountOwnerUser, userID)
	if err != nil {
		return nil, err
	}
	if userAccount == nil {
		return nil, errors.New("user account not found")
	}

	// 8. Platform & host accounts + fee split (paid events only). Each host may
	// have its own platform_fee_percentage override (set by admin); it falls
	// back to the global platform_settings default when unset.
	var platformAccount, hostAccount *models.Account
	var host *models.Host
	var platformFee, hostEarning int64
	var appliedPlatformPercentage int
	if totalAmount > 0 {
		platformAccount, err = s.accountRepo.GetByOwner(ctx, models.AccountOwnerPlatform, uuid.Nil)
		if err != nil {
			return nil, fmt.Errorf("platform account not found: %w", err)
		}
		host, err = s.hostRepo.GetByID(ctx, evt.HostID)
		if err != nil {
			return nil, fmt.Errorf("host lookup failed: %w", err)
		}
		hostAccount, err = s.accountRepo.GetByOwner(ctx, models.AccountOwnerHost, host.ID)
		if err != nil {
			return nil, fmt.Errorf("host account not found: %w", err)
		}

		feeConfig, err := s.payoutRepo.GetPlatformFeeConfig(ctx)
		if err != nil {
			return nil, err
		}
		feeConfig = models.EffectiveFeeConfig(feeConfig, host.PlatformFeePercentage)
		appliedPlatformPercentage = feeConfig.PlatformPercentage
		platformFee = totalAmount * int64(feeConfig.PlatformPercentage) / 100
		hostEarning = totalAmount - platformFee
	}

	// ── Transaction — every write below commits all-or-nothing ───────────
	// A crash or error at any point rolls the whole booking back: no orphan
	// debit, no partial ledger, no booking without its payment row (bug C3).
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("booking: begin transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	ledgerTx := s.ledgerRepo.WithTx(tx)
	accountTx := s.accountRepo.WithTx(tx)
	bookingTx := s.bookingRepo.WithTx(tx)
	paymentTx := s.paymentRepo.WithTx(tx)
	payoutTx := s.payoutRepo.WithTx(tx)

	// ─────────────────────────────────────────────────────────────────────
	// SWIGGY-STYLE LEDGER FLOW (Immutable Journal)
	// Only process ledger entries for paid events (totalAmount > 0)
	// ─────────────────────────────────────────────────────────────────────
	if totalAmount > 0 {
		// Debit the user's wallet (the DB CHECK rejects an insufficient balance).
		if err := accountTx.Debit(ctx, userAccount.ID, totalAmount); err != nil {
			if errors.Is(err, repository.ErrInsufficientBalance) {
				return nil, errors.New("insufficient wallet balance; please top up first")
			}
			return nil, err
		}

		// Ledger Entry 1: user debit — money flowing user → platform.
		userDebit, err := ledgerTx.Create(ctx, &models.TransactionLedger{
			ID:             uuid.New(),
			AccountID:      userAccount.ID,
			Type:           models.LedgerTypeBookingCredit,
			AmountCents:    -totalAmount, // NEGATIVE = money out
			ReferenceID:    &req.EventID,
			ReferenceType:  strPtr("event"),
			IdempotencyKey: strPtr(idempotencyKey),
			Description:    strPtr(fmt.Sprintf("Event registration: %d tickets", req.Quantity)),
			Status:         models.LedgerStatusCompleted,
			CreatedAt:      time.Now(),
			CreatedBy:      &userID,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create user debit ledger: %w", err)
		}

		// Ledger Entry 2: platform credit — receives the user payment.
		platformCredit, err := ledgerTx.Create(ctx, &models.TransactionLedger{
			ID:            uuid.New(),
			AccountID:     platformAccount.ID,
			Type:          models.LedgerTypeBookingCredit,
			AmountCents:   totalAmount, // POSITIVE = money in
			ReferenceID:   &userDebit.ID,
			ReferenceType: strPtr("ledger"),
			Description:   strPtr("Payment received for event registration"),
			Status:        models.LedgerStatusCompleted,
			CreatedAt:     time.Now(),
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create platform credit ledger: %w", err)
		}

		// Ledger Entry 3: platform disburses the host's share. Amount MUST be
		// -hostEarning (not -platformFee) so the four entries net to zero.
		if _, err := ledgerTx.Create(ctx, &models.TransactionLedger{
			ID:            uuid.New(),
			AccountID:     platformAccount.ID,
			Type:          models.LedgerTypePlatformFeeCredit,
			AmountCents:   -hostEarning, // NEGATIVE = host's share paid out of platform account
			ReferenceID:   &req.EventID,
			ReferenceType: strPtr("event"),
			Description:   strPtr(fmt.Sprintf("Host earning disbursed (platform keeps %d%% commission)", appliedPlatformPercentage)),
			Status:        models.LedgerStatusCompleted,
			CreatedAt:     time.Now(),
		}); err != nil {
			return nil, fmt.Errorf("failed to create platform fee ledger: %w", err)
		}

		// Ledger Entry 4: host credit — the host's earning (pending settlement).
		if _, err := ledgerTx.Create(ctx, &models.TransactionLedger{
			ID:            uuid.New(),
			AccountID:     hostAccount.ID,
			Type:          models.LedgerTypeBookingCredit,
			AmountCents:   hostEarning, // POSITIVE = money reserved for host
			ReferenceID:   &platformCredit.ID,
			ReferenceType: strPtr("ledger"),
			Description:   strPtr(fmt.Sprintf("Booking earning (after %d%% commission)", appliedPlatformPercentage)),
			Status:        models.LedgerStatusCompleted,
			CreatedAt:     time.Now(),
		}); err != nil {
			return nil, fmt.Errorf("failed to create host credit ledger: %w", err)
		}

		// Update host earnings aggregate.
		if err := payoutTx.IncrementEarnings(ctx, host.ID, hostEarning); err != nil {
			return nil, fmt.Errorf("failed to increment host earnings: %w", err)
		}
		if err := payoutTx.AddPendingClearance(ctx, host.ID, hostEarning); err != nil {
			return nil, fmt.Errorf("failed to add host pending clearance: %w", err)
		}
	}

	// Create the booking record. When AutoConfirm is set the booking is written
	// as `confirmed` directly in this transaction — so a paid booking is never
	// left stranded at `pending` if a later, separate confirm call fails.
	bookingStatus := models.BookingStatusPending
	if req.AutoConfirm {
		bookingStatus = models.BookingStatusConfirmed
	}
	newBooking := &models.Booking{
		ID:              uuid.New(),
		EventID:         req.EventID,
		UserID:          userID,
		OccurrenceDate:  occurrenceDate,
		Quantity:        req.Quantity,
		Status:          bookingStatus,
		IdempotencyKey:  &idempotencyKey,
		AmountCents:     &totalAmount,
		ServiceFeeCents: &platformFee,
		NetEarningCents: &hostEarning,
		PriceTierID:     priceTierID,
		UnitPriceCents:  &unitPriceCents,
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	}
	if err := bookingTx.Create(ctx, newBooking); err != nil {
		return nil, fmt.Errorf("failed to create booking: %w", err)
	}

	// Create the payment record and link it to the booking (bookings.payment_id).
	displayRef := fmt.Sprintf("BK-%05d", time.Now().UnixMilli()%100000)
	bookingPayment := &models.Payment{
		ID:               uuid.New(),
		IdempotencyKey:   idempotencyKey,
		AccountID:        userAccount.ID,
		Type:             models.PaymentTypeBooking,
		ReferenceID:      &newBooking.ID,
		AmountCents:      totalAmount,
		Status:           models.PaymentStatusCompleted,
		RetryCount:       0,
		DisplayReference: &displayRef,
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}
	if err := paymentTx.Create(ctx, bookingPayment); err != nil {
		return nil, fmt.Errorf("failed to create payment record: %w", err)
	}
	if err := bookingTx.UpdatePaymentID(ctx, newBooking.ID, bookingPayment.ID); err != nil {
		return nil, fmt.Errorf("failed to link payment to booking: %w", err)
	}
	newBooking.PaymentID = &bookingPayment.ID

	// ── Commit ───────────────────────────────────────────────────────────
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("booking: commit transaction: %w", err)
	}
	committed = true

	s.dispatcher.Publish(event.BookingCreated, newBooking)

	fmt.Printf("[BOOKING] CreateBooking SUCCESS: id=%s, user=%s, event=%s, total=%d, host_earning=%d, platform_fee=%d, autoconfirm=%t\n",
		newBooking.ID, userID, req.EventID, totalAmount, hostEarning, platformFee, req.AutoConfirm)

	// Auto-confirm side effects: the status is already `confirmed` (committed
	// above), so here we only bump the event's booking counter, optionally notify
	// the guest, and publish the confirmed event. These are non-fatal — the
	// booking is already valid and paid.
	if req.AutoConfirm {
		if err := s.eventRepo.IncrementBookingCount(ctx, newBooking.EventID, newBooking.Quantity); err != nil {
			fmt.Printf("[BOOKING] ERROR: IncrementBookingCount failed: %v\n", err)
		}
		if req.Notify {
			s.sendConfirmationNotificationsAsync(newBooking)
		}
		s.dispatcher.Publish(event.BookingConfirmed, newBooking)
	}

	return newBooking, nil
}

// Helper to create string pointers
func strPtr(s string) *string {
	return &s
}

func (s *bookingService) ConfirmBooking(ctx context.Context, bookingID uuid.UUID) (*models.Booking, error) {
	return s.confirmBooking(ctx, bookingID, true)
}

// ConfirmBookingSilent confirms a booking but skips the guest notifications.
func (s *bookingService) ConfirmBookingSilent(ctx context.Context, bookingID uuid.UUID) (*models.Booking, error) {
	return s.confirmBooking(ctx, bookingID, false)
}

func (s *bookingService) confirmBooking(ctx context.Context, bookingID uuid.UUID, notify bool) (*models.Booking, error) {
	fmt.Printf("[BOOKING] ConfirmBooking: bookingID=%s notify=%t\n", bookingID, notify)

	booking, err := s.bookingRepo.GetByID(ctx, bookingID)
	if err != nil {
		fmt.Printf("[BOOKING] ConfirmBooking: GetByID error: %v\n", err)
		return nil, err
	}
	if booking == nil {
		fmt.Printf("[BOOKING] ConfirmBooking: booking not found\n")
		return nil, errors.New("booking not found")
	}
	// Idempotent: an already-confirmed booking is a no-op success. This makes the
	// frontend's separate confirm call harmless now that paid bookings are
	// auto-confirmed at creation (no double-notify, no error).
	if booking.Status == models.BookingStatusConfirmed {
		fmt.Printf("[BOOKING] ConfirmBooking: already confirmed, no-op\n")
		return booking, nil
	}
	if booking.Status != models.BookingStatusPending {
		fmt.Printf("[BOOKING] ConfirmBooking: cannot confirm from status=%s\n", booking.Status)
		return nil, fmt.Errorf("booking cannot be confirmed from status: %s", booking.Status)
	}

	fmt.Printf("[BOOKING] ConfirmBooking: status=%s -> confirmed, quantity=%d\n", booking.Status, booking.Quantity)
	booking.Status = models.BookingStatusConfirmed
	booking.UpdatedAt = time.Now()

	if err := s.bookingRepo.UpdateStatus(ctx, bookingID, models.BookingStatusConfirmed); err != nil {
		fmt.Printf("[BOOKING] ConfirmBooking: UpdateStatus error: %v\n", err)
		return nil, err
	}

	// Increment event's total_bookings counter
	fmt.Printf("[BOOKING] ConfirmBooking: incrementing event %s by %d\n", booking.EventID, booking.Quantity)
	if err := s.eventRepo.IncrementBookingCount(ctx, booking.EventID, booking.Quantity); err != nil {
		fmt.Printf("[BOOKING] ERROR: IncrementBookingCount failed: %v\n", err)
		// Non-critical — booking already confirmed, counter reconciliation can happen in background
	}

	// Send booking confirmation notification (async, non-blocking). Skipped for
	// silent confirmations (admin on-spot bookings have only a synthetic contact).
	if notify {
		s.sendConfirmationNotificationsAsync(booking)
	}

	fmt.Printf("[BOOKING] ConfirmBooking: SUCCESS (notify=%t)\n", notify)
	s.dispatcher.Publish(event.BookingConfirmed, booking)
	return booking, nil
}

// sendConfirmationNotificationsAsync sends the WhatsApp + email booking
// confirmation in the background. Best-effort; failures are logged, not fatal.
func (s *bookingService) sendConfirmationNotificationsAsync(booking *models.Booking) {
	go func() {
		user, err := s.userRepo.GetByID(context.Background(), booking.UserID)
		if err != nil || user == nil {
			fmt.Printf("[BOOKING] ERROR: Failed to fetch user for notification: %v\n", err)
			return
		}
		evt, err := s.eventRepo.GetByID(context.Background(), booking.EventID)
		if err != nil || evt == nil {
			fmt.Printf("[BOOKING] ERROR: Failed to fetch event for notification: %v\n", err)
			return
		}
		if s.notificationService.GetKapsoClient() == nil {
			if err := s.notificationService.SendBookingConfirmationWhatsapp(context.Background(), booking, user, evt); err != nil {
				fmt.Printf("[BOOKING] ERROR: Failed to send WhatsApp notification: %v\n", err)
			}
		}
		if err := s.notificationService.SendBookingConfirmationEmail(context.Background(), booking, user, evt); err != nil {
			fmt.Printf("[BOOKING] ERROR: Failed to send Email notification: %v\n", err)
		}
	}()
}

func (s *bookingService) CancelBooking(ctx context.Context, bookingID uuid.UUID, userID uuid.UUID, refundDestination RefundDestination) (*models.Booking, error) {
	booking, err := s.bookingRepo.GetByID(ctx, bookingID)
	if err != nil {
		return nil, err
	}
	if booking == nil {
		return nil, errors.New("booking not found")
	}
	if booking.UserID != userID {
		return nil, errors.New("not authorized to cancel this booking")
	}
	if booking.Status == models.BookingStatusCancelled || booking.Status == models.BookingStatusRefunded {
		return nil, errors.New("booking is already cancelled or refunded")
	}
	// Guard: cannot cancel after the event has already happened. The buyer has
	// either attended or missed the event; refunding now would create money out
	// of thin air on the host's side. (Same reason CancelEvent skips past
	// bookings — see eventService.CancelEvent.)
	if booking.OccurrenceDate.Before(time.Now()) {
		return nil, errors.New("this event has already happened — cancellation is no longer available")
	}

	cancelled, err := s.performCancellation(ctx, booking)
	if err != nil {
		return nil, err
	}

	// If the user picked "refund to source", additionally chain a Razorpay
	// refund of the funding top-up — money goes from their wallet back to the
	// original card/UPI. Best-effort: if it fails (no eligible top-up, gateway
	// rejection, etc.), the F4 wallet refund stands and the booking is still
	// correctly cancelled — the user just keeps the money in their wallet.
	if refundDestination == RefundDestinationSource {
		s.tryChainSourceRefundOnCancel(ctx, cancelled)
	}
	return cancelled, nil
}

// tryChainSourceRefundOnCancel attempts to source-refund the booking amount
// against the user's most-recent refundable top-up. Best-effort — any failure
// is logged and ignored; the booking-cancel + wallet-credit already committed
// so the user is whole either way.
//
// Lot selection is LIFO (newest top-up first), matching the policy documented
// in skill flows.md §11 / bugs-and-risks.md F7.
func (s *bookingService) tryChainSourceRefundOnCancel(ctx context.Context, b *models.Booking) {
	if b == nil || b.AmountCents == nil || *b.AmountCents <= 0 {
		return // free booking — nothing to source-refund
	}
	if s.userService == nil {
		fmt.Printf("[CANCEL/SOURCE] userService not wired — skipping source refund for booking %s\n", b.ID)
		return
	}
	amountCents := *b.AmountCents

	userAccount, err := s.accountRepo.GetByOwner(ctx, models.AccountOwnerUser, b.UserID)
	if err != nil || userAccount == nil {
		fmt.Printf("[CANCEL/SOURCE] user account lookup failed for %s: %v — keeping wallet refund\n", b.UserID, err)
		return
	}
	topup, err := s.paymentRepo.FindMostRecentRefundableTopup(ctx, userAccount.ID, amountCents)
	if err != nil {
		fmt.Printf("[CANCEL/SOURCE] top-up lookup error for user %s: %v — keeping wallet refund\n", b.UserID, err)
		return
	}
	if topup == nil {
		fmt.Printf("[CANCEL/SOURCE] no refundable top-up for user %s with >= %d cents headroom — keeping wallet refund\n", b.UserID, amountCents)
		return
	}

	if _, err := s.userService.RefundTopUpToSource(ctx, topup.ID, SourceRefundRequest{
		AmountCents:    amountCents,
		Reason:         fmt.Sprintf("User-requested source refund on booking cancellation %s", b.ID),
		IdempotencyKey: "source_refund_" + b.ID.String(),
	}); err != nil {
		fmt.Printf("[CANCEL/SOURCE] source refund failed for booking %s (top-up %s): %v — wallet refund retained\n", b.ID, topup.ID, err)
	}
}

func (s *bookingService) CancelBookingByHost(ctx context.Context, bookingID uuid.UUID) (*models.Booking, error) {
	booking, err := s.bookingRepo.GetByID(ctx, bookingID)
	if err != nil {
		return nil, err
	}
	if booking == nil {
		return nil, errors.New("booking not found")
	}
	if booking.Status == models.BookingStatusCancelled || booking.Status == models.BookingStatusRefunded {
		return nil, errors.New("booking is already cancelled or refunded")
	}

	// For host-initiated cancellations, we could trigger a specific "Host Cancelled" event
	return s.performCancellation(ctx, booking)
}

// performCancellation cancels a booking and reverses its money movements.
//
// It reverses the booking's effect through the transaction ledger so that
// accounts.balance_cents stays in sync with SUM(transaction_ledger) — the
// previous implementation moved balances directly with no ledger entries,
// which permanently drifted the ledger (bug H2).
//
// The four reversal effects:
//   - user:     refund_credit  (+amount)   ledger entry + wallet credit
//   - host:     cancellation_debit (-net)  ledger entry; host_earnings reduced
//   - platform: cancellation_debit (-fee)  ledger entry
//   - the original `booking` payment row is marked `reversed`
//
// Host/platform accounts.balance_cents are intentionally NOT touched: the
// booking flow never credits them (host money lives in host_earnings), so
// debiting them here would only recreate drift. See bugs-and-risks.md "3c".
//
// All writes run on one *sql.Tx and commit together, or roll back together —
// a crash or error mid-way leaves the booking entirely uncancelled rather than
// in a partial state.
func (s *bookingService) performCancellation(ctx context.Context, booking *models.Booking) (*models.Booking, error) {
	now := time.Now()

	amountCents := int64(0)
	if booking.AmountCents != nil {
		amountCents = *booking.AmountCents
	}
	netEarningCents := int64(0)
	if booking.NetEarningCents != nil {
		netEarningCents = *booking.NetEarningCents
	}
	serviceFeeCents := int64(0)
	if booking.ServiceFeeCents != nil {
		serviceFeeCents = *booking.ServiceFeeCents
	}
	isRefund := amountCents > 0

	// A paid cancellation becomes `refunded`; a free booking becomes `cancelled`.
	finalStatus := models.BookingStatusCancelled
	if isRefund {
		finalStatus = models.BookingStatusRefunded
	}

	// ── Read-only lookups — done before the transaction opens ────────────
	var userAccount *models.Account
	if isRefund {
		ua, err := s.accountRepo.GetByOwner(ctx, models.AccountOwnerUser, booking.UserID)
		if err != nil {
			return nil, fmt.Errorf("cancel: load user account: %w", err)
		}
		if ua == nil {
			return nil, errors.New("cancel: user account not found")
		}
		userAccount = ua
	}

	var hostID uuid.UUID
	var hostAccount *models.Account
	if netEarningCents > 0 {
		evt, err := s.eventRepo.GetByID(ctx, booking.EventID)
		if err != nil {
			return nil, fmt.Errorf("cancel: load event: %w", err)
		}
		if evt != nil {
			hostID = evt.HostID
			ha, err := s.accountRepo.GetByOwner(ctx, models.AccountOwnerHost, evt.HostID)
			if err != nil {
				return nil, fmt.Errorf("cancel: load host account: %w", err)
			}
			hostAccount = ha
		}
	}

	var platformAccount *models.Account
	if serviceFeeCents > 0 {
		pa, err := s.accountRepo.GetByOwner(ctx, models.AccountOwnerPlatform, uuid.Nil)
		if err != nil {
			return nil, fmt.Errorf("cancel: load platform account: %w", err)
		}
		platformAccount = pa
	}

	// ── Transaction — everything below commits all-or-nothing ────────────
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("cancel: begin transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	ledgerTx := s.ledgerRepo.WithTx(tx)
	accountTx := s.accountRepo.WithTx(tx)
	bookingTx := s.bookingRepo.WithTx(tx)
	paymentTx := s.paymentRepo.WithTx(tx)
	payoutTx := s.payoutRepo.WithTx(tx)

	// ── User refund ──────────────────────────────────────────────────────
	if isRefund {
		// Idempotency guard: one refund_credit ledger entry per booking. If it
		// already exists, this booking was already refunded — abort rather than
		// refund twice. (The ledger's UNIQUE(idempotency_key) is the hard guard;
		// this check just yields a friendlier error.)
		refundLedgerKey := "refund_credit_" + booking.ID.String()
		if existing, err := ledgerTx.GetByIdempotencyKey(ctx, refundLedgerKey); err != nil {
			return nil, fmt.Errorf("cancel: refund idempotency check: %w", err)
		} else if existing != nil {
			return nil, errors.New("booking has already been refunded")
		}

		if _, err := ledgerTx.Create(ctx, &models.TransactionLedger{
			ID:             uuid.New(),
			AccountID:      userAccount.ID,
			Type:           models.LedgerTypeRefundCredit,
			AmountCents:    amountCents, // POSITIVE = money back into the user's wallet
			ReferenceID:    &booking.ID,
			ReferenceType:  strPtr("booking"),
			IdempotencyKey: &refundLedgerKey,
			Description:    strPtr("Refund for cancelled booking"),
			Status:         models.LedgerStatusCompleted,
			CreatedAt:      time.Now(),
			CreatedBy:      &booking.UserID,
		}); err != nil {
			return nil, fmt.Errorf("cancel: write refund ledger entry: %w", err)
		}

		if err := accountTx.Credit(ctx, userAccount.ID, amountCents); err != nil {
			return nil, fmt.Errorf("cancel: credit user wallet: %w", err)
		}

		refundKey := fmt.Sprintf("refund_%s", booking.ID)
		displayRef := fmt.Sprintf("RF-%05d", time.Now().UnixMilli()%100000)
		if err := paymentTx.Create(ctx, &models.Payment{
			ID:               uuid.New(),
			IdempotencyKey:   refundKey,
			AccountID:        userAccount.ID,
			Type:             models.PaymentTypeRefund,
			ReferenceID:      &booking.ID,
			AmountCents:      amountCents,
			Status:           models.PaymentStatusCompleted,
			DisplayReference: &displayRef,
			CreatedAt:        time.Now(),
			UpdatedAt:        time.Now(),
		}); err != nil {
			return nil, fmt.Errorf("cancel: create refund payment record: %w", err)
		}
	}

	// ── Mark the booking ─────────────────────────────────────────────────
	if err := bookingTx.UpdateStatus(ctx, booking.ID, finalStatus); err != nil {
		return nil, fmt.Errorf("cancel: update booking status: %w", err)
	}

	// ── Reverse the host side ────────────────────────────────────────────
	if netEarningCents > 0 && hostAccount != nil {
		cancelLedgerKey := "cancellation_debit_" + booking.ID.String()
		if _, err := ledgerTx.Create(ctx, &models.TransactionLedger{
			ID:             uuid.New(),
			AccountID:      hostAccount.ID,
			Type:           models.LedgerTypeCancellationDebit,
			AmountCents:    -netEarningCents, // NEGATIVE = host earning reversed
			ReferenceID:    &booking.ID,
			ReferenceType:  strPtr("booking"),
			IdempotencyKey: &cancelLedgerKey,
			Description:    strPtr("Host earning reversed — booking cancelled"),
			Status:         models.LedgerStatusCompleted,
			CreatedAt:      time.Now(),
		}); err != nil {
			return nil, fmt.Errorf("cancel: write host cancellation ledger entry: %w", err)
		}
	}
	if netEarningCents > 0 && hostID != uuid.Nil {
		// Reduce both the pending clearance and the lifetime earnings total.
		if err := payoutTx.ClearPending(ctx, hostID, netEarningCents); err != nil {
			return nil, fmt.Errorf("cancel: clear host pending clearance: %w", err)
		}
		if err := payoutTx.DecrementEarnings(ctx, hostID, netEarningCents); err != nil {
			return nil, fmt.Errorf("cancel: decrement host earnings: %w", err)
		}
	}

	// ── Reverse the platform commission ──────────────────────────────────
	// With the user fully refunded and the host earning reversed, this leaves
	// the cancelled booking with a net-zero footprint across the ledger.
	if serviceFeeCents > 0 && platformAccount != nil {
		platformCancelKey := "cancellation_platform_" + booking.ID.String()
		if _, err := ledgerTx.Create(ctx, &models.TransactionLedger{
			ID:             uuid.New(),
			AccountID:      platformAccount.ID,
			Type:           models.LedgerTypeCancellationDebit,
			AmountCents:    -serviceFeeCents, // NEGATIVE = platform commission reversed
			ReferenceID:    &booking.ID,
			ReferenceType:  strPtr("booking"),
			IdempotencyKey: &platformCancelKey,
			Description:    strPtr("Platform commission reversed — booking cancelled"),
			Status:         models.LedgerStatusCompleted,
			CreatedAt:      time.Now(),
		}); err != nil {
			return nil, fmt.Errorf("cancel: write platform cancellation ledger entry: %w", err)
		}
	}

	// ── Reverse the original booking payment record ──────────────────────
	if bookingPayment, err := paymentTx.GetByReferenceAndType(ctx, booking.ID, models.PaymentTypeBooking); err != nil {
		return nil, fmt.Errorf("cancel: load booking payment: %w", err)
	} else if bookingPayment != nil && bookingPayment.Status != models.PaymentStatusReversed {
		if err := paymentTx.UpdateStatus(ctx, bookingPayment.ID, models.PaymentStatusReversed, nil); err != nil {
			return nil, fmt.Errorf("cancel: reverse booking payment: %w", err)
		}
	}

	// ── Commit ───────────────────────────────────────────────────────────
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("cancel: commit transaction: %w", err)
	}
	committed = true

	booking.Status = finalStatus
	booking.CancelledAt = &now
	s.dispatcher.Publish(event.BookingCancelled, booking)
	return booking, nil
}

func (s *bookingService) GetUserBookings(ctx context.Context, userID uuid.UUID) ([]*models.Booking, error) {
	return s.bookingRepo.ListByUserID(ctx, userID)
}

func (s *bookingService) GetBooking(ctx context.Context, bookingID uuid.UUID) (*models.Booking, error) {
	return s.bookingRepo.GetByID(ctx, bookingID)
}

func (s *bookingService) SendTicketNotification(ctx context.Context, bookingID uuid.UUID, fileName string, pdfBytes []byte) error {
	booking, err := s.bookingRepo.GetByID(ctx, bookingID)
	if err != nil {
		return fmt.Errorf("failed to fetch booking: %w", err)
	}
	if booking == nil {
		return fmt.Errorf("booking not found")
	}

	user, err := s.userRepo.GetByID(ctx, booking.UserID)
	if err != nil {
		return fmt.Errorf("failed to fetch user: %w", err)
	}
	if user == nil {
		return fmt.Errorf("user not found")
	}

	event, err := s.eventRepo.GetByID(ctx, booking.EventID)
	if err != nil {
		return fmt.Errorf("failed to fetch event: %w", err)
	}
	if event == nil {
		return fmt.Errorf("event not found")
	}

	return s.notificationService.SendBookingConfirmationWhatsappWithPDF(ctx, booking, user, event, fileName, pdfBytes)
}
