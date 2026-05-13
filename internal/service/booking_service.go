package service

import (
	"context"
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
	CancelBooking(ctx context.Context, bookingID uuid.UUID, userID uuid.UUID) (*models.Booking, error)
	CancelBookingByHost(ctx context.Context, bookingID uuid.UUID) (*models.Booking, error)
	GetUserBookings(ctx context.Context, userID uuid.UUID) ([]*models.Booking, error)
	GetBooking(ctx context.Context, bookingID uuid.UUID) (*models.Booking, error)
}

type BookingCreateRequest struct {
	EventID        uuid.UUID
	Quantity       int
	IdempotencyKey string
	OccurrenceDate *time.Time // which specific date the user is booking for
}

type bookingService struct {
	bookingRepo         repository.BookingRepository
	eventRepo           repository.EventRepository
	accountRepo         repository.AccountRepository
	paymentRepo         repository.PaymentRepository
	payoutRepo          repository.PayoutRepository
	hostRepo            repository.HostRepository
	userRepo            repository.UserRepository
	ledgerRepo          repository.TransactionLedgerRepository
	dispatcher          *event.Dispatcher
	notificationService notification.NotificationService
}

func NewBookingService(
	br repository.BookingRepository,
	er repository.EventRepository,
	ar repository.AccountRepository,
	pmr repository.PaymentRepository,
	pr repository.PayoutRepository,
	hr repository.HostRepository,
	ur repository.UserRepository,
	lr repository.TransactionLedgerRepository,
	d *event.Dispatcher,
	ns notification.NotificationService,
) BookingService {
	return &bookingService{
		bookingRepo:         br,
		eventRepo:           er,
		accountRepo:         ar,
		paymentRepo:         pmr,
		payoutRepo:          pr,
		hostRepo:            hr,
		userRepo:            ur,
		ledgerRepo:          lr,
		dispatcher:          d,
		notificationService: ns,
	}
}

func (s *bookingService) CreateBooking(ctx context.Context, userID uuid.UUID, req BookingCreateRequest) (*models.Booking, error) {
	idempotencyKey := req.IdempotencyKey
	if idempotencyKey == "" {
		idempotencyKey = fmt.Sprintf("booking_%s_%d", userID, time.Now().UnixNano())
	}

	// 1. Idempotency check via ledger (prevents duplicate ledger entries)
	actualLedger, err := s.ledgerRepo.GetByIdempotencyKey(ctx, idempotencyKey)
	if err != nil {
		return nil, err
	}
	if actualLedger != nil {
		// Already processed this booking, return the original
		if actualLedger.ReferenceID != nil {
			return s.bookingRepo.GetByID(ctx, *actualLedger.ReferenceID)
		}
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

	// 4. Resolve occurrence date
	// For recurring events the frontend sends the specific date the user picked.
	// For non-recurring events (or if omitted) we fall back to the event's own time.
	occurrenceDate := evt.Time
	if req.OccurrenceDate != nil {
		occurrenceDate = *req.OccurrenceDate
	}

	// 5. Overbooking prevention — check capacity per occurrence date
	currentBooked, err := s.bookingRepo.GetTotalBookedQuantityForOccurrence(ctx, req.EventID, occurrenceDate)
	if err != nil {
		return nil, err
	}
	if currentBooked+req.Quantity > evt.Capacity {
		return nil, errors.New("event capacity exceeded")
	}

	// 5. Get platform fee config
	feeConfig, err := s.payoutRepo.GetPlatformFeeConfig(ctx)
	if err != nil {
		return nil, err
	}

	// Price calculation - use event's price, default to 0 for free events
	var pricePerTicketCents int64 = 0
	if evt.PriceCents != nil && *evt.PriceCents > 0 {
		pricePerTicketCents = *evt.PriceCents
	}
	totalAmount := pricePerTicketCents * int64(req.Quantity)
	platformFee := totalAmount * int64(feeConfig.PlatformPercentage) / 100
	hostEarning := totalAmount - platformFee

	// 6. Get user account (must exist)
	userAccount, err := s.accountRepo.GetByOwner(ctx, models.AccountOwnerUser, userID)
	if err != nil {
		return nil, err
	}
	if userAccount == nil {
		return nil, errors.New("user account not found")
	}

	// 7. Check sufficient balance BEFORE creating ledger (only for paid events)
	if totalAmount > 0 {
		if err := s.accountRepo.Debit(ctx, userAccount.ID, totalAmount); err != nil {
			if errors.Is(err, repository.ErrInsufficientBalance) {
				return nil, errors.New("insufficient wallet balance; please top up first")
			}
			return nil, err
		}
	}

	// ─────────────────────────────────────────────────────────────────────
	// SWIGGY-STYLE LEDGER FLOW (Immutable Journal)
	// Only process ledger entries for paid events (totalAmount > 0)
	// ─────────────────────────────────────────────────────────────────────

	var platformCreditID *uuid.UUID

	if totalAmount > 0 {
		// 8. Ledger Entry 1: User DEBIT - User pays amount to platform
		// (This records money flowing from user → platform)
		userDebitLedger := &models.TransactionLedger{
			ID:             uuid.New(),
			AccountID:      userAccount.ID,
			Type:           models.LedgerTypeBookingCredit, // CREDIT from user's perspective (they paid)
			AmountCents:    -totalAmount,                   // NEGATIVE = money out
			ReferenceID:    &req.EventID,
			ReferenceType:  strPtr("event"),
			IdempotencyKey: strPtr(idempotencyKey),
			Description:    strPtr(fmt.Sprintf("Event registration: %d tickets", req.Quantity)),
			Status:         models.LedgerStatusCompleted,
			CreatedAt:      time.Now(),
			CreatedBy:      &userID,
		}
		userDebit, err := s.ledgerRepo.Create(ctx, userDebitLedger)
		if err != nil {
			// Rollback the account debit
			_ = s.accountRepo.Credit(ctx, userAccount.ID, totalAmount)
			return nil, fmt.Errorf("failed to create user debit ledger: %w", err)
		}

		// 9. Get platform account
		platformAccount, err := s.accountRepo.GetByOwner(ctx, models.AccountOwnerPlatform, uuid.Nil)
		if err != nil {
			// Rollback
			_ = s.accountRepo.Credit(ctx, userAccount.ID, totalAmount)
			return nil, fmt.Errorf("platform account not found: %w", err)
		}

		// 10. Ledger Entry 2: Platform CREDIT - Receives user payment
		platformCreditLedger := &models.TransactionLedger{
			ID:            uuid.New(),
			AccountID:     platformAccount.ID,
			Type:          models.LedgerTypeBookingCredit,
			AmountCents:   totalAmount, // POSITIVE = money in
			ReferenceID:   &userDebit.ID,
			ReferenceType: strPtr("ledger"),
			Description:   strPtr("Payment received for event registration"),
			Status:        models.LedgerStatusCompleted,
			CreatedAt:     time.Now(),
		}
		platformCredit, err := s.ledgerRepo.Create(ctx, platformCreditLedger)
		if err != nil {
			_ = s.accountRepo.Credit(ctx, userAccount.ID, totalAmount)
			return nil, fmt.Errorf("failed to create platform credit ledger: %w", err)
		}
		platformCreditID = &platformCredit.ID

		// 11. Ledger Entry 3: Platform FEE DEBIT - Platform keeps commission
		platformFeeDebitLedger := &models.TransactionLedger{
			ID:            uuid.New(),
			AccountID:     platformAccount.ID,
			Type:          models.LedgerTypePlatformFeeCredit,
			AmountCents:   -platformFee, // NEGATIVE = platform fee reserved
			ReferenceID:   &req.EventID,
			ReferenceType: strPtr("event"),
			Description:   strPtr(fmt.Sprintf("Commission: %d%% of booking", feeConfig.PlatformPercentage)),
			Status:        models.LedgerStatusCompleted,
			CreatedAt:     time.Now(),
		}
		_, err = s.ledgerRepo.Create(ctx, platformFeeDebitLedger)
		if err != nil {
			_ = s.accountRepo.Credit(ctx, userAccount.ID, totalAmount)
			return nil, fmt.Errorf("failed to create platform fee ledger: %w", err)
		}

		// 12. Get host account
		host, err := s.hostRepo.GetByID(ctx, evt.HostID)
		if err != nil {
			_ = s.accountRepo.Credit(ctx, userAccount.ID, totalAmount)
			return nil, fmt.Errorf("host lookup failed: %w", err)
		}

		hostAccount, err := s.accountRepo.GetByOwner(ctx, models.AccountOwnerHost, host.ID)
		if err != nil {
			_ = s.accountRepo.Credit(ctx, userAccount.ID, totalAmount)
			return nil, fmt.Errorf("host account not found: %w", err)
		}

		// 13. Ledger Entry 4: Host CREDIT - Host's earning (pending settlement)
		hostCreditLedger := &models.TransactionLedger{
			ID:            uuid.New(),
			AccountID:     hostAccount.ID,
			Type:          models.LedgerTypeBookingCredit,
			AmountCents:   hostEarning, // POSITIVE = money reserved for host
			ReferenceID:   platformCreditID,
			ReferenceType: strPtr("ledger"),
			Description:   strPtr(fmt.Sprintf("Booking earning (after %d%% commission)", feeConfig.PlatformPercentage)),
			Status:        models.LedgerStatusCompleted,
			CreatedAt:     time.Now(),
		}
		_, err = s.ledgerRepo.Create(ctx, hostCreditLedger)
		if err != nil {
			_ = s.accountRepo.Credit(ctx, userAccount.ID, totalAmount)
			return nil, fmt.Errorf("failed to create host credit ledger: %w", err)
		}

		// 16. Update host earnings aggregate (only for paid events)
		_ = s.payoutRepo.IncrementEarnings(ctx, host.ID, hostEarning)
		_ = s.payoutRepo.AddPendingClearance(ctx, host.ID, hostEarning)
	}

	// 14. Create the booking record
	newBooking := &models.Booking{
		ID:              uuid.New(),
		EventID:         req.EventID,
		UserID:          userID,
		OccurrenceDate:  occurrenceDate,
		Quantity:        req.Quantity,
		Status:          models.BookingStatusPending,
		IdempotencyKey:  &idempotencyKey,
		AmountCents:     &totalAmount,
		ServiceFeeCents: &platformFee,
		NetEarningCents: &hostEarning,
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	}

	if err := s.bookingRepo.Create(ctx, newBooking); err != nil {
		if totalAmount > 0 {
			_ = s.accountRepo.Credit(ctx, userAccount.ID, totalAmount)
		}
		return nil, fmt.Errorf("failed to create booking: %w", err)
	}

	// 15. Create payment record (for legacy payment history)
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

	if err := s.paymentRepo.Create(ctx, bookingPayment); err != nil {
		// Non-critical — ledger entries are source of truth
		fmt.Printf("[BOOKING] Warning: Payment record creation failed: %v\n", err)
	}
	newBooking.PaymentID = &bookingPayment.ID

	// 17. Publish event
	s.dispatcher.Publish(event.BookingCreated, newBooking)

	fmt.Printf("[BOOKING] CreateBooking SUCCESS: id=%s, user=%s, event=%s, total=%d, host_earning=%d, platform_fee=%d\n",
		newBooking.ID, userID, req.EventID, totalAmount, hostEarning, platformFee)

	return newBooking, nil
}

// Helper to create string pointers
func strPtr(s string) *string {
	return &s
}

func (s *bookingService) ConfirmBooking(ctx context.Context, bookingID uuid.UUID) (*models.Booking, error) {
	fmt.Printf("[BOOKING] ConfirmBooking: bookingID=%s\n", bookingID)

	booking, err := s.bookingRepo.GetByID(ctx, bookingID)
	if err != nil {
		fmt.Printf("[BOOKING] ConfirmBooking: GetByID error: %v\n", err)
		return nil, err
	}
	if booking == nil {
		fmt.Printf("[BOOKING] ConfirmBooking: booking not found\n")
		return nil, errors.New("booking not found")
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

	// Send booking confirmation notification (async, non-blocking)
	fmt.Printf("[BOOKING] ConfirmBooking: sending notifications\n")
	go func() {
		// Fetch user and event for notification
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

		// Send WhatsApp confirmation notification
		if err := s.notificationService.SendBookingConfirmationWhatsapp(context.Background(), booking, user, evt); err != nil {
			fmt.Printf("[BOOKING] ERROR: Failed to send WhatsApp notification: %v\n", err)
			// Non-critical — continue anyway
		}

		// Send Email confirmation notification
		if err := s.notificationService.SendBookingConfirmationEmail(context.Background(), booking, user, evt); err != nil {
			fmt.Printf("[BOOKING] ERROR: Failed to send Email notification: %v\n", err)
			// Non-critical — continue anyway
		}
	}()

	fmt.Printf("[BOOKING] ConfirmBooking: SUCCESS\n")
	s.dispatcher.Publish(event.BookingConfirmed, booking)
	return booking, nil
}

func (s *bookingService) CancelBooking(ctx context.Context, bookingID uuid.UUID, userID uuid.UUID) (*models.Booking, error) {
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

	return s.performCancellation(ctx, booking)
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

func (s *bookingService) performCancellation(ctx context.Context, booking *models.Booking) (*models.Booking, error) {
	now := time.Now()

	// 1. Update booking status
	if err := s.bookingRepo.UpdateStatus(ctx, booking.ID, models.BookingStatusCancelled); err != nil {
		return nil, err
	}
	booking.Status = models.BookingStatusCancelled
	booking.CancelledAt = &now

	// 2. Refund user wallet
	if booking.AmountCents != nil && *booking.AmountCents > 0 {
		userAccount, err := s.accountRepo.GetByOwner(ctx, models.AccountOwnerUser, booking.UserID)
		if err == nil && userAccount != nil {
			_ = s.accountRepo.Credit(ctx, userAccount.ID, *booking.AmountCents)

			// Create refund payment record
			refundKey := fmt.Sprintf("refund_%s", booking.ID)
			displayRef := fmt.Sprintf("RF-%05d", time.Now().UnixMilli()%100000)
			refundPayment := &models.Payment{
				ID:               uuid.New(),
				IdempotencyKey:   refundKey,
				AccountID:        userAccount.ID,
				Type:             models.PaymentTypeRefund,
				ReferenceID:      &booking.ID,
				AmountCents:      *booking.AmountCents,
				Status:           models.PaymentStatusCompleted,
				DisplayReference: &displayRef,
				CreatedAt:        time.Now(),
				UpdatedAt:        time.Now(),
			}
			_ = s.paymentRepo.Create(ctx, refundPayment)
		}

		// 3. Debit host wallet for the net earning
		if booking.NetEarningCents != nil && *booking.NetEarningCents > 0 {
			evt, err := s.eventRepo.GetByID(ctx, booking.EventID)
			if err == nil && evt != nil {
				hostAccount, err := s.accountRepo.GetByOwner(ctx, models.AccountOwnerHost, evt.HostID)
				if err == nil && hostAccount != nil {
					_ = s.accountRepo.Debit(ctx, hostAccount.ID, *booking.NetEarningCents)
					_ = s.payoutRepo.ClearPending(ctx, evt.HostID, *booking.NetEarningCents)
				}
			}
		}
	}

	s.dispatcher.Publish(event.BookingCancelled, booking)
	return booking, nil
}

func (s *bookingService) GetUserBookings(ctx context.Context, userID uuid.UUID) ([]*models.Booking, error) {
	return s.bookingRepo.ListByUserID(ctx, userID)
}

func (s *bookingService) GetBooking(ctx context.Context, bookingID uuid.UUID) (*models.Booking, error) {
	return s.bookingRepo.GetByID(ctx, bookingID)
}
