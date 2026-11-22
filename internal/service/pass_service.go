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
	// On a private experience the guest must get through the same door a
	// booking does (see PassAccess) — checked BEFORE any money moves.
	PurchasePass(ctx context.Context, userID, eventID uuid.UUID, idempotencyKey string, access PassAccess) (*models.UserPass, error)
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
	// tierID picks the ticket type; nil means the basic ticket the pass covers.
	// A pricier tier charges only the difference (see bookingService).
	ReserveSession(ctx context.Context, userID, passID uuid.UUID, occurrence time.Time, tierID *uuid.UUID) (*models.Booking, error)
	// SendExpiryReminders tells guests whose pass ends within
	// PassExpiryReminderWindow and still has sessions left. Each pass is
	// reminded once. Safe to call on a timer.
	SendExpiryReminders(ctx context.Context) (int, error)
	// ListHolders is the host's roster of pass holders on their own event.
	ListHolders(ctx context.Context, hostID, eventID uuid.UUID) ([]*models.PassHolder, error)
}

// PassAccess is what the guest typed to get into a private experience.
//
// The private gate is checked ONCE, at purchase, and not again per session:
// a pass spans many sessions, and the guest has already paid for all of them.
// Before this, purchase was ungated while every pass-covered booking was gated,
// so a guest could pay for a pass on a private experience and then be refused
// every session ("invalid passkey") — money taken, nothing booked.
type PassAccess struct {
	Passkey    string // the event's shared passkey (passkey mode)
	CouponCode string // a per-guest access code (passkey mode)
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

	// Private-experience gate at purchase (see PassAccess).
	couponRepo      repository.CouponRepository
	joinRequestRepo repository.JoinRequestRepository
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
	couponRepo repository.CouponRepository,
	joinRequestRepo repository.JoinRequestRepository,
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

		couponRepo:      couponRepo,
		joinRequestRepo: joinRequestRepo,
	}
}

func (s *passService) PurchasePass(ctx context.Context, userID, eventID uuid.UUID, idempotencyKey string, access PassAccess) (*models.UserPass, error) {
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
	accessCode, err := s.checkPrivateAccess(ctx, evt, userID, access)
	if err != nil {
		return nil, err
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

	// Guest-paid booking fee, on top of the pass price — the same fee and the
	// same rule a ticket booking uses (bookingService.CreateBooking): commission
	// above is taken on the price only, the fee goes to the platform whole, and
	// the buyer pays price + fee. Stored in PriceCents/ServiceFeeCents with the
	// rest, so a pass refund (CancelPass → reverseSplit) returns it too, exactly
	// as a refunded ticket returns its fee.
	bookingFeeConfig, err := s.payoutRepo.GetBookingFeeConfig(ctx)
	if err != nil {
		return nil, err
	}
	bookingFee := bookingFeeConfig.FeeCents(totalAmount)
	totalAmount += bookingFee
	platformFee += bookingFee

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
	// A per-guest access code is single-use: spend it with the sale, or the
	// same code could unlock passes for any number of people.
	if accessCode != nil {
		if err := s.couponRepo.WithTx(tx).Redeem(ctx, accessCode.ID); err != nil {
			if errors.Is(err, repository.ErrCouponExhausted) {
				return nil, errors.New("this coupon has reached its redemption limit")
			}
			return nil, fmt.Errorf("failed to redeem access code: %w", err)
		}
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
	// A pass is a balance of sessions, not a set of bookings: the guest picks
	// each session they want and pays for it with one session off the pass.
	// Nothing is booked here, so no seats are held for weeks the guest may
	// never come to.
	s.sendPassConfirmationAsync(pass, evt)
	return pass, nil
}

// checkPrivateAccess is the pass-purchase twin of the booking's private gate.
// It returns the per-guest access code that opened the door (to be redeemed
// with the sale), or nil when none was used or the experience is public.
//
//   - rsvp: the host must have approved this guest for at least one session.
//     Approval vets the person; a pass then covers the sessions they paid for.
//   - passkey: the shared passkey, or a valid per-guest ACCESS code. A free or
//     discount code is refused here: its value is a cheaper booking, and
//     spending it on a full-price pass would quietly burn it.
func (s *passService) checkPrivateAccess(ctx context.Context, evt *models.Event, userID uuid.UUID, access PassAccess) (*models.Coupon, error) {
	if !evt.IsPrivate {
		return nil, nil
	}
	if evt.PrivateAccessMode == models.PrivateAccessModeRSVP {
		if s.joinRequestRepo == nil {
			return nil, errors.New("the host needs to approve your request to join before you can buy a pass")
		}
		ok, err := s.joinRequestRepo.HasApprovedForEvent(ctx, evt.ID, userID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errors.New("the host needs to approve your request to join before you can buy a pass")
		}
		return nil, nil
	}
	if passkeyMatches(evt, access.Passkey) {
		return nil, nil
	}
	code := strings.TrimSpace(access.CouponCode)
	if code == "" {
		code = strings.TrimSpace(access.Passkey)
	}
	if code == "" || s.couponRepo == nil || s.bookings == nil {
		return nil, errors.New("invalid passkey")
	}
	c, err := s.bookings.ValidateCoupon(ctx, evt.ID, userID, code)
	if err != nil {
		if strings.TrimSpace(access.CouponCode) == "" {
			// They typed into the passkey box; keep the passkey wording.
			return nil, errors.New("invalid passkey")
		}
		return nil, err
	}
	if c.GrantsFree || c.DiscountPercent != nil {
		return nil, errors.New("this code can't be used to buy a monthly pass")
	}
	return c, nil
}

// sendPassConfirmationAsync tells the guest, once, that their pass is live,
// how many sessions it holds and when it runs out. Fire-and-forget on its own
// context: the pass is already paid for and valid, so a messaging outage must
// not fail the purchase.
func (s *passService) sendPassConfirmationAsync(pass *models.UserPass, evt *models.Event) {
	if s.notifier == nil || s.userRepo == nil {
		return
	}
	sessions := 0 // 0 = every session in the window (legacy passes)
	if pass.SessionsIncluded != nil {
		sessions = *pass.SessionsIncluded
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
			ctx, user.PhnNumber, user.Name, evt.Title, sessions, pass.ValidUntil,
		); err != nil {
			fmt.Printf("[PASS] confirmation: WhatsApp send failed for pass %s: %v\n", pass.ID, err)
		}
	}()
}

// SendExpiryReminders nudges guests to use the sessions left on a pass before
// it runs out. A pass is claimed (MarkExpiryReminded) before the message goes
// out, so a crash or a second server can never send the same reminder twice;
// the cost is that a failed send is not retried.
func (s *passService) SendExpiryReminders(ctx context.Context) (int, error) {
	if s.notifier == nil || s.userRepo == nil {
		return 0, nil
	}
	due, err := s.passRepo.ListExpiringUnreminded(ctx, time.Now().Add(models.PassExpiryReminderWindow), 100)
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, p := range due {
		left, limited := p.SessionsRemaining()
		if limited && left <= 0 {
			continue // nothing left to use — no reason to message
		}
		claimed, err := s.passRepo.MarkExpiryReminded(ctx, p.ID)
		if err != nil || !claimed {
			continue
		}
		user, err := s.userRepo.GetByID(ctx, p.UserID)
		if err != nil || user == nil {
			continue
		}
		evt, err := s.eventRepo.GetByID(ctx, p.EventID)
		if err != nil || evt == nil {
			continue
		}
		sessionsLeft := 0 // 0 = unlimited (legacy pass)
		if limited {
			sessionsLeft = left
		}
		if err := s.notifier.SendPassExpiringWhatsapp(
			ctx, user.PhnNumber, user.Name, evt.Title, evt.Slug, sessionsLeft, p.ValidUntil,
		); err != nil {
			fmt.Printf("[PASS] expiry reminder failed for pass %s: %v\n", p.ID, err)
			continue
		}
		sent++
	}
	return sent, nil
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
		// Cancelled the way a host cancel is: always returns the session and any
		// tier-upgrade money, whatever the experience's refund policy — the
		// whole pass is being unwound, not one session given up.
		if _, err := s.bookings.CancelBookingByHost(ctx, b.ID); err != nil {
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

// ReserveSession pays for one session with one session off the pass.
func (s *passService) ReserveSession(ctx context.Context, userID, passID uuid.UUID, occurrence time.Time, tierID *uuid.UUID) (*models.Booking, error) {
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
		PriceTierID:    tierID,
		IdempotencyKey: fmt.Sprintf("pass_%s_%d", pass.ID, occurrence.Unix()),
		// The private gate was passed at purchase (see PassAccess). Gating each
		// covered session again would refuse a guest who already paid for it.
		// Safe because CreateBooking still checks the pass belongs to this user
		// and this event.
		BypassPasskey: true,
		AutoConfirm:   true,
		Notify:        true,
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
