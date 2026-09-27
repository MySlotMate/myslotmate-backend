package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"myslotmate-backend/internal/lib/notification"
	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"

	"github.com/google/uuid"
)

// PassService sells and cancels monthly passes.
//
// A pass is bought once and then covers sessions for free; that is why ALL the
// money for it moves here, at purchase, through the same creditSplit a booking
// uses. Reserving a covered session later goes through BookingService with
// PassID set and moves nothing. Keep it that way — crediting the host again per
// reserved session would pay them twice for one pass.
type PassService interface {
	// PurchasePass debits the guest's wallet for the event's monthly pass and
	// issues it, valid for models.PassValidityDays from now.
	PurchasePass(ctx context.Context, userID, eventID uuid.UUID, idempotencyKey string) (*models.UserPass, error)
	// CancelPass refunds a pass to the guest's wallet, inside the refund window
	// and only while no session has been used.
	CancelPass(ctx context.Context, userID, passID uuid.UUID) (*models.UserPass, error)
	// GetMyPasses lists the caller's passes, newest first.
	GetMyPasses(ctx context.Context, userID uuid.UUID) ([]*models.UserPass, error)
	// GetPassForEvent returns the caller's live pass for an event (nil when they
	// hold none) plus how many passes are still available.
	GetPassForEvent(ctx context.Context, userID, eventID uuid.UUID) (*models.UserPass, *int, error)
	// ReserveSession holds a seat on one covered date. It is a normal booking
	// with pass_id set, so capacity, the roster and door check-in all work
	// unchanged — it just costs nothing, because the pass was already paid for.
	ReserveSession(ctx context.Context, userID, passID uuid.UUID, occurrence time.Time) (*models.Booking, error)
	// ListHolders is the host's roster of pass holders on their own event.
	ListHolders(ctx context.Context, hostID, eventID uuid.UUID) ([]*models.PassHolder, error)
}

type passService struct {
	db          *sql.DB
	passRepo    repository.PassRepository
	eventRepo   repository.EventRepository
	accountRepo repository.AccountRepository
	paymentRepo repository.PaymentRepository
	payoutRepo  repository.PayoutRepository
	hostRepo    repository.HostRepository
	ledgerRepo  repository.TransactionLedgerRepository
	bookingRepo repository.BookingRepository
	userRepo    repository.UserRepository
	bookings    BookingService
	events      EventService
	notifier    notification.NotificationService
}

func NewPassService(
	db *sql.DB,
	passRepo repository.PassRepository,
	eventRepo repository.EventRepository,
	accountRepo repository.AccountRepository,
	paymentRepo repository.PaymentRepository,
	payoutRepo repository.PayoutRepository,
	hostRepo repository.HostRepository,
	ledgerRepo repository.TransactionLedgerRepository,
	bookingRepo repository.BookingRepository,
	userRepo repository.UserRepository,
	bookings BookingService,
	events EventService,
	notifier notification.NotificationService,
) PassService {
	return &passService{
		db:          db,
		passRepo:    passRepo,
		eventRepo:   eventRepo,
		accountRepo: accountRepo,
		paymentRepo: paymentRepo,
		payoutRepo:  payoutRepo,
		hostRepo:    hostRepo,
		ledgerRepo:  ledgerRepo,
		bookingRepo: bookingRepo,
		userRepo:    userRepo,
		bookings:    bookings,
		events:      events,
		notifier:    notifier,
	}
}

func (s *passService) PurchasePass(ctx context.Context, userID, eventID uuid.UUID, idempotencyKey string) (*models.UserPass, error) {
	if idempotencyKey == "" {
		idempotencyKey = fmt.Sprintf("pass_%s_%d", userID, time.Now().UnixNano())
	}

	// ── Validation & lookups — read-only, before the transaction opens ───

	// Replaying the same purchase returns the original pass rather than selling
	// a second one (the ledger's UNIQUE idempotency key is the hard guard).
	if existing, err := s.ledgerRepo.GetByIdempotencyKey(ctx, idempotencyKey); err != nil {
		return nil, err
	} else if existing != nil && existing.ReferenceID != nil {
		return s.passRepo.GetByID(ctx, *existing.ReferenceID)
	}

	flagged, err := s.payoutRepo.HasActiveFraudFlag(ctx, userID)
	if err != nil {
		return nil, err
	}
	if flagged {
		return nil, errors.New("your account is blocked due to suspicious activity")
	}

	evt, err := s.eventRepo.GetByID(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if evt == nil {
		return nil, errors.New("event not found")
	}
	if evt.MonthlyPassPriceCents == nil || *evt.MonthlyPassPriceCents <= 0 {
		return nil, errors.New("this experience does not offer a monthly pass")
	}
	if evt.Status != models.EventStatusLive {
		return nil, errors.New("this experience is not taking bookings right now")
	}

	// A lapsed pass keeps status 'active' until someone looks at it. Retire it
	// first, or the one-live-pass rule would block next month's purchase.
	if err := s.passRepo.ExpireLapsed(ctx, eventID, userID); err != nil {
		return nil, err
	}
	if live, err := s.passRepo.GetActiveForUserEvent(ctx, eventID, userID); err != nil {
		return nil, err
	} else if live != nil {
		return nil, errors.New("you already have an active pass for this experience")
	}

	if evt.MonthlyPassCapacity != nil {
		sold, err := s.passRepo.CountActive(ctx, eventID)
		if err != nil {
			return nil, err
		}
		if sold >= *evt.MonthlyPassCapacity {
			return nil, errors.New("no passes left for this experience")
		}
	}

	totalAmount := *evt.MonthlyPassPriceCents

	userAccount, err := s.accountRepo.GetByOwner(ctx, models.AccountOwnerUser, userID)
	if err != nil {
		return nil, err
	}
	if userAccount == nil {
		return nil, errors.New("user account not found")
	}
	platformAccount, err := s.accountRepo.GetByOwner(ctx, models.AccountOwnerPlatform, uuid.Nil)
	if err != nil {
		return nil, fmt.Errorf("platform account not found: %w", err)
	}
	host, err := s.hostRepo.GetByID(ctx, evt.HostID)
	if err != nil {
		return nil, fmt.Errorf("host lookup failed: %w", err)
	}
	hostAccount, err := s.accountRepo.GetByOwner(ctx, models.AccountOwnerHost, host.ID)
	if err != nil {
		return nil, fmt.Errorf("host account not found: %w", err)
	}

	feeConfig, err := s.payoutRepo.GetPlatformFeeConfig(ctx)
	if err != nil {
		return nil, err
	}
	feeConfig = models.EffectiveFeeConfig(feeConfig, host.PlatformFeePercentage)
	platformFee := totalAmount * int64(feeConfig.PlatformPercentage) / 100
	hostEarning := totalAmount - platformFee

	// ── Transaction — every write below commits all-or-nothing ───────────
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("pass: begin transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	ledgerTx := s.ledgerRepo.WithTx(tx)
	accountTx := s.accountRepo.WithTx(tx)
	paymentTx := s.paymentRepo.WithTx(tx)
	payoutTx := s.payoutRepo.WithTx(tx)
	passTx := s.passRepo.WithTx(tx)

	if err := accountTx.Debit(ctx, userAccount.ID, totalAmount); err != nil {
		if errors.Is(err, repository.ErrInsufficientBalance) {
			return nil, errors.New("insufficient wallet balance; please top up first")
		}
		return nil, err
	}

	now := time.Now()
	pass := &models.UserPass{
		ID:               uuid.New(),
		EventID:          eventID,
		UserID:           userID,
		PriceCents:       totalAmount,
		ServiceFeeCents:  platformFee,
		NetEarningCents:  hostEarning,
		SessionsIncluded: evt.MonthlyPassSessionLimit,
		ValidFrom:        now,
		ValidUntil:       now.AddDate(0, 0, models.PassValidityDays),
		Status:           models.PassActive,
		IdempotencyKey:   &idempotencyKey,
	}
	if err := passTx.Create(ctx, pass); err != nil {
		return nil, fmt.Errorf("failed to create pass: %w", err)
	}

	// The same four-entry split a booking writes — the ledger points at the pass.
	if err := creditSplit(ctx, ledgerTx, payoutTx, purchaseSplit{
		UserID:              userID,
		UserAccountID:       userAccount.ID,
		PlatformAccountID:   platformAccount.ID,
		HostAccountID:       hostAccount.ID,
		HostID:              host.ID,
		TotalCents:          totalAmount,
		HostEarningCents:    hostEarning,
		ReferenceID:         pass.ID,
		ReferenceType:       "pass",
		IdempotencyKey:      idempotencyKey,
		UserDescription:     fmt.Sprintf("Monthly pass: %s", evt.Title),
		PlatformDescription: "Payment received for monthly pass",
		HostDescription:     fmt.Sprintf("Monthly pass earning (after %d%% commission)", feeConfig.PlatformPercentage),
		PlatformPercentage:  feeConfig.PlatformPercentage,
	}); err != nil {
		return nil, err
	}

	// The payment row is typed `booking` because that enum has no `pass` member;
	// its reference_id points at the pass, which is what tells the two apart.
	displayRef := fmt.Sprintf("MP-%05d", time.Now().UnixMilli()%100000)
	payment := &models.Payment{
		ID:               uuid.New(),
		IdempotencyKey:   idempotencyKey,
		AccountID:        userAccount.ID,
		Type:             models.PaymentTypeBooking,
		ReferenceID:      &pass.ID,
		AmountCents:      totalAmount,
		Status:           models.PaymentStatusCompleted,
		DisplayReference: &displayRef,
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}
	if err := paymentTx.Create(ctx, payment); err != nil {
		return nil, fmt.Errorf("failed to create payment record: %w", err)
	}
	if err := passTx.UpdatePaymentID(ctx, pass.ID, payment.ID); err != nil {
		return nil, fmt.Errorf("failed to link payment to pass: %w", err)
	}
	pass.PaymentID = &payment.ID

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("pass: commit transaction: %w", err)
	}
	committed = true

	fmt.Printf("[PASS] PurchasePass SUCCESS: id=%s, user=%s, event=%s, total=%d, host_earning=%d, platform_fee=%d\n",
		pass.ID, userID, eventID, totalAmount, hostEarning, platformFee)

	// The guest paid for the month, so every covered session is booked for them
	// here — they get tickets, and the host's roster and capacity are right from
	// the moment of sale. Best-effort by design: a session that is already full
	// is skipped, and the pass (with its money) still stands.
	booked := s.reserveCoveredSessions(ctx, pass)
	s.sendPassConfirmationAsync(pass, evt, booked)
	return pass, nil
}

// sendPassConfirmationAsync tells the guest, once, that their pass is live and
// how many sessions it booked. Fire-and-forget on its own context: the pass is
// already paid for and valid, so a messaging outage must not fail the purchase.
func (s *passService) sendPassConfirmationAsync(pass *models.UserPass, evt *models.Event, sessionsBooked int) {
	if s.notifier == nil || s.userRepo == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		user, err := s.userRepo.GetByID(ctx, pass.UserID)
		if err != nil || user == nil {
			fmt.Printf("[PASS] confirmation: user lookup failed for pass %s: %v\n", pass.ID, err)
			return
		}
		if err := s.notifier.SendPassConfirmationWhatsapp(
			ctx, user.PhnNumber, user.Name, evt.Title, sessionsBooked, pass.ValidUntil,
		); err != nil {
			fmt.Printf("[PASS] confirmation: WhatsApp send failed for pass %s: %v\n", pass.ID, err)
		}
	}()
}

// reserveCoveredSessions books a seat on every session the pass covers inside
// its validity window, up to its included-session limit. Runs after the
// purchase has committed; individual failures are logged and skipped rather
// than failing the sale.
func (s *passService) reserveCoveredSessions(ctx context.Context, pass *models.UserPass) int {
	if s.events == nil {
		return 0
	}
	occurrences, err := s.events.GetEventAvailability(ctx, pass.EventID)
	if err != nil {
		fmt.Printf("[PASS] auto-reserve: availability lookup failed for pass %s: %v\n", pass.ID, err)
		return 0
	}

	booked := 0
	for _, occ := range occurrences {
		if pass.SessionsIncluded != nil && booked >= *pass.SessionsIncluded {
			break
		}
		if occ.IsPaused || occ.IsFullyBooked {
			continue
		}
		if occ.Date.Before(pass.ValidFrom) || occ.Date.After(pass.ValidUntil) {
			continue
		}
		// Quiet: one pass confirmation goes out afterwards instead of a
		// confirmation per session, which would be a burst of messages.
		if _, err := s.reserveSession(ctx, pass.UserID, pass.ID, occ.Date, false); err != nil {
			fmt.Printf("[PASS] auto-reserve: %s on %s skipped: %v\n", pass.ID, occ.Date.Format(time.RFC3339), err)
			continue
		}
		booked++
	}
	fmt.Printf("[PASS] auto-reserve: pass %s booked %d session(s)\n", pass.ID, booked)
	return booked
}

func (s *passService) CancelPass(ctx context.Context, userID, passID uuid.UUID) (*models.UserPass, error) {
	pass, err := s.passRepo.GetByID(ctx, passID)
	if err != nil {
		return nil, err
	}
	if pass == nil {
		return nil, errors.New("pass not found")
	}
	if pass.UserID != userID {
		return nil, errors.New("not authorized to cancel this pass")
	}
	// Once the window has closed the pass is final — the guest committed to a
	// month up front and the host has planned for it.
	if !pass.IsRefundable(time.Now()) {
		return nil, errors.New("this pass can no longer be cancelled for a refund")
	}

	// Refund only a pass that has not been used yet. Every covered session was
	// booked at purchase, so "unused" means none of those sessions has started
	// and nobody was admitted on one.
	held, err := s.bookingRepo.ListByPassID(ctx, pass.ID)
	if err != nil {
		return nil, fmt.Errorf("cancel pass: load pass bookings: %w", err)
	}
	now := time.Now()
	for _, b := range held {
		if b.CheckedInCount > 0 || b.OccurrenceDate.Before(now) {
			return nil, errors.New("this pass has already been used and cannot be refunded")
		}
	}

	// Give the seats back before refunding. These bookings moved no money, so
	// cancelling them only frees capacity and returns the pass's sessions.
	for _, b := range held {
		if _, err := s.bookings.CancelBooking(ctx, b.ID, pass.UserID, RefundDestinationWallet); err != nil {
			return nil, fmt.Errorf("cancel pass: release session booking %s: %w", b.ID, err)
		}
	}

	userAccount, err := s.accountRepo.GetByOwner(ctx, models.AccountOwnerUser, pass.UserID)
	if err != nil {
		return nil, fmt.Errorf("cancel pass: load user account: %w", err)
	}
	if userAccount == nil {
		return nil, errors.New("cancel pass: user account not found")
	}

	evt, err := s.eventRepo.GetByID(ctx, pass.EventID)
	if err != nil {
		return nil, fmt.Errorf("cancel pass: load event: %w", err)
	}
	var hostID uuid.UUID
	var hostAccountID uuid.UUID
	if evt != nil && pass.NetEarningCents > 0 {
		hostID = evt.HostID
		ha, err := s.accountRepo.GetByOwner(ctx, models.AccountOwnerHost, evt.HostID)
		if err != nil {
			return nil, fmt.Errorf("cancel pass: load host account: %w", err)
		}
		if ha != nil {
			hostAccountID = ha.ID
		}
	}
	var platformAccount *models.Account
	if pass.ServiceFeeCents > 0 {
		pa, err := s.accountRepo.GetByOwner(ctx, models.AccountOwnerPlatform, uuid.Nil)
		if err != nil {
			return nil, fmt.Errorf("cancel pass: load platform account: %w", err)
		}
		platformAccount = pa
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("cancel pass: begin transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	ledgerTx := s.ledgerRepo.WithTx(tx)
	accountTx := s.accountRepo.WithTx(tx)
	paymentTx := s.paymentRepo.WithTx(tx)
	payoutTx := s.payoutRepo.WithTx(tx)
	passTx := s.passRepo.WithTx(tx)

	if err := reverseSplit(ctx, ledgerTx, accountTx, paymentTx, payoutTx, refundSplit{
		UserID:          pass.UserID,
		UserAccountID:   userAccount.ID,
		HostID:          hostID,
		HostAccountID:   hostAccountID,
		PlatformAccount: platformAccount,
		AmountCents:     pass.PriceCents,
		NetEarningCents: pass.NetEarningCents,
		ServiceFeeCents: pass.ServiceFeeCents,
		ReferenceID:     pass.ID,
		ReferenceType:   "pass",
		Noun:            "monthly pass",
	}); err != nil {
		if errors.Is(err, ErrAlreadyRefunded) {
			return nil, errors.New("this pass has already been refunded")
		}
		return nil, err
	}

	if err := passTx.UpdateStatus(ctx, pass.ID, models.PassRefunded); err != nil {
		return nil, fmt.Errorf("cancel pass: update status: %w", err)
	}

	if purchase, err := paymentTx.GetByReferenceAndType(ctx, pass.ID, models.PaymentTypeBooking); err != nil {
		return nil, fmt.Errorf("cancel pass: load pass payment: %w", err)
	} else if purchase != nil && purchase.Status != models.PaymentStatusReversed {
		if err := paymentTx.UpdateStatus(ctx, purchase.ID, models.PaymentStatusReversed, nil); err != nil {
			return nil, fmt.Errorf("cancel pass: reverse pass payment: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("cancel pass: commit transaction: %w", err)
	}
	committed = true

	pass.Status = models.PassRefunded
	return pass, nil
}

func (s *passService) GetMyPasses(ctx context.Context, userID uuid.UUID) ([]*models.UserPass, error) {
	return s.passRepo.ListByUserID(ctx, userID)
}

func (s *passService) GetPassForEvent(ctx context.Context, userID, eventID uuid.UUID) (*models.UserPass, *int, error) {
	if err := s.passRepo.ExpireLapsed(ctx, eventID, userID); err != nil {
		return nil, nil, err
	}
	pass, err := s.passRepo.GetActiveForUserEvent(ctx, eventID, userID)
	if err != nil {
		return nil, nil, err
	}

	evt, err := s.eventRepo.GetByID(ctx, eventID)
	if err != nil {
		return nil, nil, err
	}
	if evt == nil || evt.MonthlyPassCapacity == nil {
		return pass, nil, nil // no cap configured — passes are unlimited
	}
	sold, err := s.passRepo.CountActive(ctx, eventID)
	if err != nil {
		return nil, nil, err
	}
	left := *evt.MonthlyPassCapacity - sold
	if left < 0 {
		left = 0
	}
	return pass, &left, nil
}

func (s *passService) ReserveSession(ctx context.Context, userID, passID uuid.UUID, occurrence time.Time) (*models.Booking, error) {
	return s.reserveSession(ctx, userID, passID, occurrence, true)
}

// reserveSession holds one covered seat. notify controls whether the guest gets
// the per-booking confirmation — off for the sessions booked automatically at
// purchase, which are covered by the single pass confirmation instead.
func (s *passService) reserveSession(ctx context.Context, userID, passID uuid.UUID, occurrence time.Time, notify bool) (*models.Booking, error) {
	pass, err := s.passRepo.GetByID(ctx, passID)
	if err != nil {
		return nil, err
	}
	if pass == nil {
		return nil, errors.New("pass not found")
	}
	// Ownership is checked again inside CreateBooking; checked here too so the
	// caller gets "not yours" rather than a confusing booking error.
	if pass.UserID != userID {
		return nil, errors.New("not authorized to use this pass")
	}

	// The idempotency key is deterministic per pass+date, and
	// bookings.idempotency_key is UNIQUE — so a double-tap is rejected by the
	// database rather than burning a second included session on one seat.
	booking, err := s.bookings.CreateBooking(ctx, userID, BookingCreateRequest{
		EventID:        pass.EventID,
		Quantity:       1,
		OccurrenceDate: &occurrence,
		PassID:         &pass.ID,
		IdempotencyKey: fmt.Sprintf("pass_%s_%d", pass.ID, occurrence.Unix()),
		AutoConfirm:    true,
		Notify:         notify,
	})
	if err != nil && strings.Contains(err.Error(), "duplicate key") {
		return nil, errors.New("you have already reserved this session")
	}
	return booking, err
}

func (s *passService) ListHolders(ctx context.Context, hostID, eventID uuid.UUID) ([]*models.PassHolder, error) {
	evt, err := s.eventRepo.GetByID(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if evt == nil {
		return nil, errors.New("event not found")
	}
	if !HostCanManageEvent(ctx, s.eventRepo, evt, hostID) {
		return nil, errors.New("not your experience")
	}
	return s.passRepo.ListHolders(ctx, eventID)
}
