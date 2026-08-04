package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"myslotmate-backend/internal/lib/event"
	"myslotmate-backend/internal/lib/payout"
	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"

	"github.com/google/uuid"
)

// PayoutService handles payout methods, withdrawal requests, earnings queries, and webhooks.
type PayoutService interface {
	// Payout Methods (Host)
	AddPayoutMethod(ctx context.Context, hostID uuid.UUID, req AddPayoutMethodRequest) (*models.PayoutMethod, error)
	ListPayoutMethods(ctx context.Context, hostID uuid.UUID) ([]*models.PayoutMethod, error)
	SetPrimaryMethod(ctx context.Context, hostID uuid.UUID, methodID uuid.UUID) error
	DeletePayoutMethod(ctx context.Context, hostID uuid.UUID, methodID uuid.UUID) error

	// Withdrawals (Host)
	RequestWithdrawal(ctx context.Context, hostID uuid.UUID, req WithdrawalRequest) (*models.Payment, error)

	// Earnings Dashboard (Host)
	GetEarningsSummary(ctx context.Context, hostID uuid.UUID) (*EarningsSummary, error)
	GetHostSales(ctx context.Context, hostID uuid.UUID, limit, offset int, fromDate *time.Time) ([]*repository.HostSale, error)

	// Payment History (Host)
	GetPayoutHistory(ctx context.Context, hostID uuid.UUID, limit, offset int) ([]*models.Payment, error)

	// Admin Platform Payout Methods
	AddAdminPayoutMethod(ctx context.Context, req AddPayoutMethodRequest) (*models.PayoutMethod, error)
	ListAdminPayoutMethods(ctx context.Context) ([]*models.PayoutMethod, error)
	SetAdminPrimaryMethod(ctx context.Context, methodID uuid.UUID) error
	DeleteAdminPayoutMethod(ctx context.Context, methodID uuid.UUID) error

	// Admin Platform Withdrawal
	RequestAdminWithdrawal(ctx context.Context, req WithdrawalRequest) (*models.Payment, error)

	// Admin Platform Balance
	GetPlatformBalance(ctx context.Context) (*PlatformBalanceInfo, error)

	// Webhook
	HandlePayoutWebhook(ctx context.Context, paymentID uuid.UUID, status string, providerError string) error

	// ReconcilePendingPayouts polls the provider for every payout stuck in
	// 'processing' and finalizes those that have reached a terminal state — the
	// safety net for missed payout webhooks. Safe to call repeatedly.
	ReconcilePendingPayouts(ctx context.Context) (ReconcileResult, error)

	// Platform Settings Settings
	GetPlatformSetting(ctx context.Context, key string) (json.RawMessage, error)
	SavePlatformSetting(ctx context.Context, key string, value json.RawMessage) error

	// GetPlatformFeeConfig returns the effective global commission split
	// (falls back to the 70/30 default when platform_settings has no
	// 'platform_fee' row seeded yet) — used to show admins what "default"
	// actually resolves to for hosts without a per-host override.
	GetPlatformFeeConfig(ctx context.Context) (*models.PlatformFeeConfig, error)
}

// ── Request / Response types ────────────────────────────────────────────────

type AddPayoutMethodRequest struct {
	Type            models.PayoutMethodType
	BankName        *string
	AccountType     *string
	AccountNumber   *string // will be encrypted + last 4 stored
	IFSC            *string
	BeneficiaryName *string
	UPIID           *string
}

type WithdrawalRequest struct {
	AmountCents    int64
	PayoutMethodID *uuid.UUID // if nil, use primary
	IdempotencyKey string
}

// EarningsSummary is the host-facing earnings + balance view, computed live
// from the bookings table (the authoritative source) and the payments table
// (for in-flight withdrawals). The numbers always satisfy:
//
//	TotalEarningsCents     = PendingClearanceCents + AvailableBalanceCents + InFlightPayoutsCents
//	CurrentBalanceCents    = TotalEarningsCents - InFlightPayoutsCents
//	                       = PendingClearanceCents + AvailableBalanceCents
//
// Tab 1 ("Earnings") usually shows TotalEarningsCents + AvailableBalanceCents.
// Tab 2 ("Balance")  usually shows CurrentBalanceCents + PendingClearanceCents.
type EarningsSummary struct {
	// Lifetime net earnings (refunds / cancellations already deducted —
	// only confirmed bookings count).
	TotalEarningsCents int64 `json:"total_earnings_cents"`
	// Earnings ready to withdraw right now: confirmed bookings whose event
	// has already happened, minus payouts already in flight or completed.
	AvailableBalanceCents int64 `json:"available_balance_cents"`
	// Earnings still locked because the event has not happened yet.
	PendingClearanceCents int64 `json:"pending_clearance_cents"`
	// Total still owed to the host (Pending + Available) — i.e. the
	// lifetime net minus what's been paid out.
	CurrentBalanceCents int64 `json:"current_balance_cents"`
	// Amount currently in flight or already paid out (status pending /
	// processing / completed on payout-type payments).
	InFlightPayoutsCents int64 `json:"in_flight_payouts_cents"`

	EstimatedClearanceAt *time.Time                `json:"estimated_clearance_at,omitempty"`
	PlatformFee          *models.PlatformFeeConfig `json:"platform_fee"`
}

type PlatformBalanceInfo struct {
	AccountID            uuid.UUID `json:"account_id"`
	BalanceCents         int64     `json:"balance_cents"`
	CollectedFromBooking int64     `json:"collected_from_bookings"`
}

// ── Implementation ──────────────────────────────────────────────────────────

type payoutService struct {
	payoutRepo  repository.PayoutRepository
	accountRepo repository.AccountRepository
	paymentRepo repository.PaymentRepository
	bookingRepo repository.BookingRepository
	hostRepo    repository.HostRepository
	ledgerRepo  repository.TransactionLedgerRepository
	provider    payout.Provider
	dispatcher  *event.Dispatcher
}

func NewPayoutService(
	pr repository.PayoutRepository,
	ar repository.AccountRepository,
	pmr repository.PaymentRepository,
	br repository.BookingRepository,
	hr repository.HostRepository,
	lr repository.TransactionLedgerRepository,
	provider payout.Provider,
	d *event.Dispatcher,
) PayoutService {
	return &payoutService{
		payoutRepo:  pr,
		accountRepo: ar,
		paymentRepo: pmr,
		bookingRepo: br,
		hostRepo:    hr,
		ledgerRepo:  lr,
		provider:    provider,
		dispatcher:  d,
	}
}

// ── Payout Methods ──────────────────────────────────────────────────────────

func (s *payoutService) AddPayoutMethod(ctx context.Context, hostID uuid.UUID, req AddPayoutMethodRequest) (*models.PayoutMethod, error) {
	fmt.Printf("[PAYOUT] AddPayoutMethod: hostID=%s, type=%s\n", hostID, req.Type)

	// Determine if this is the first method (auto-set as primary)
	existing, err := s.payoutRepo.ListPayoutMethodsByHostID(ctx, hostID)
	if err != nil {
		fmt.Printf("[PAYOUT] AddPayoutMethod: list methods error: %v\n", err)
		return nil, err
	}
	isPrimary := len(existing) == 0
	fmt.Printf("[PAYOUT] AddPayoutMethod: existing methods=%d, isPrimary=%v\n", len(existing), isPrimary)

	// Mask + encrypt account number for bank type
	var lastFour *string
	var encrypted *string
	if req.Type == models.PayoutMethodBank && req.AccountNumber != nil {
		num := *req.AccountNumber
		if len(num) >= 4 {
			l4 := num[len(num)-4:]
			lastFour = &l4
		}
		// In production, encrypt with KMS/vault. For now, store raw (marked as encrypted field).
		encrypted = req.AccountNumber
	}

	methodID := uuid.New()
	// Generate Cashfree beneficiary ID: remove hyphens from method ID (alphanumeric only)
	cashfreBeneID := strings.ReplaceAll(methodID.String(), "-", "")

	pm := &models.PayoutMethod{
		ID:                     methodID,
		HostID:                 &hostID,
		Type:                   req.Type,
		BankName:               req.BankName,
		AccountType:            req.AccountType,
		LastFourDigits:         lastFour,
		AccountNumberEncrypted: encrypted,
		IFSC:                   req.IFSC,
		BeneficiaryName:        req.BeneficiaryName,
		UPIID:                  req.UPIID,
		CashfreeBeID:           &cashfreBeneID,
		IsVerified:             true, // auto-verified by default
		IsPrimary:              isPrimary,
		CreatedAt:              time.Now(),
		UpdatedAt:              time.Now(),
	}

	fmt.Printf("[PAYOUT] AddPayoutMethod: creating method - methodID=%s, cashfreBeneID=%s, verified=true, primary=%v\n", pm.ID, cashfreBeneID, isPrimary)
	if err := s.payoutRepo.CreatePayoutMethod(ctx, pm); err != nil {
		fmt.Printf("[PAYOUT] AddPayoutMethod: create error: %v\n", err)
		return nil, err
	}

	// Create beneficiary on Cashfree
	fmt.Printf("[PAYOUT] AddPayoutMethod: creating beneficiary on Cashfree...\n")
	transferReq := payout.TransferRequest{
		BeneID:     cashfreBeneID,
		MethodType: string(req.Type),
	}

	// Set beneficiary name (handle nil case)
	if req.BeneficiaryName != nil {
		transferReq.BeneficiaryName = *req.BeneficiaryName
	} else {
		transferReq.BeneficiaryName = "MySlotMate Host"
	}

	// Set bank or UPI details
	if req.Type == models.PayoutMethodBank {
		if encrypted != nil {
			transferReq.AccountNumber = *encrypted
		}
		if req.IFSC != nil {
			transferReq.IFSC = *req.IFSC
		}
		if req.BankName != nil {
			transferReq.BankName = *req.BankName
		}
	} else if req.Type == models.PayoutMethodUPI {
		if req.UPIID != nil {
			transferReq.UPIID = *req.UPIID
		}
	}

	// Call provider to create beneficiary on Cashfree
	err = s.provider.RegisterBeneficiary(ctx, transferReq)
	if err != nil {
		fmt.Printf("[PAYOUT] Warning: failed to create beneficiary on Cashfree during method creation: %v\n", err)
		// Don't fail the entire AddPayoutMethod - the beneficiary will be created during first withdrawal
	} else {
		fmt.Printf("[PAYOUT] AddPayoutMethod: beneficiary created on Cashfree successfully\n")
	}

	fmt.Printf("[PAYOUT] AddPayoutMethod: method created successfully\n")
	return pm, nil
}

func (s *payoutService) ListPayoutMethods(ctx context.Context, hostID uuid.UUID) ([]*models.PayoutMethod, error) {
	fmt.Printf("[PAYOUT] ListPayoutMethods: hostID=%s\n", hostID)
	methods, err := s.payoutRepo.ListPayoutMethodsByHostID(ctx, hostID)
	if err != nil {
		fmt.Printf("[PAYOUT] ListPayoutMethods: error: %v\n", err)
		return nil, err
	}
	fmt.Printf("[PAYOUT] ListPayoutMethods: found %d methods\n", len(methods))
	return methods, nil
}

func (s *payoutService) SetPrimaryMethod(ctx context.Context, hostID uuid.UUID, methodID uuid.UUID) error {
	fmt.Printf("[PAYOUT] SetPrimaryMethod: hostID=%s, methodID=%s\n", hostID, methodID)

	pm, err := s.payoutRepo.GetPayoutMethodByID(ctx, methodID)
	if err != nil {
		fmt.Printf("[PAYOUT] SetPrimaryMethod: fetch error: %v\n", err)
		return err
	}
	if pm == nil {
		fmt.Printf("[PAYOUT] SetPrimaryMethod: method not found\n")
		return errors.New("payout method not found")
	}
	if pm.HostID == nil || *pm.HostID != hostID {
		fmt.Printf("[PAYOUT] SetPrimaryMethod: method does not belong to host\n")
		return errors.New("payout method does not belong to this host")
	}

	fmt.Printf("[PAYOUT] SetPrimaryMethod: setting as primary\n")
	err = s.payoutRepo.SetPrimary(ctx, hostID, methodID)
	if err != nil {
		fmt.Printf("[PAYOUT] SetPrimaryMethod: set primary error: %v\n", err)
		return err
	}
	fmt.Printf("[PAYOUT] SetPrimaryMethod: successfully set as primary\n")
	return nil
}

func (s *payoutService) DeletePayoutMethod(ctx context.Context, hostID uuid.UUID, methodID uuid.UUID) error {
	fmt.Printf("[PAYOUT] DeletePayoutMethod: hostID=%s, methodID=%s\n", hostID, methodID)

	pm, err := s.payoutRepo.GetPayoutMethodByID(ctx, methodID)
	if err != nil {
		fmt.Printf("[PAYOUT] DeletePayoutMethod: fetch error: %v\n", err)
		return err
	}
	if pm == nil {
		fmt.Printf("[PAYOUT] DeletePayoutMethod: method not found\n")
		return errors.New("payout method not found")
	}
	if pm.HostID == nil || *pm.HostID != hostID {
		fmt.Printf("[PAYOUT] DeletePayoutMethod: method does not belong to host\n")
		return errors.New("payout method does not belong to this host")
	}
	if pm.IsPrimary {
		fmt.Printf("[PAYOUT] DeletePayoutMethod: cannot delete primary method\n")
		return errors.New("cannot delete the primary payout method; set another as primary first")
	}

	fmt.Printf("[PAYOUT] DeletePayoutMethod: deleting method\n")
	err = s.payoutRepo.DeletePayoutMethod(ctx, methodID)
	if err != nil {
		fmt.Printf("[PAYOUT] DeletePayoutMethod: delete error: %v\n", err)
		return err
	}
	fmt.Printf("[PAYOUT] DeletePayoutMethod: deleted successfully\n")
	return nil
}

// ── Withdrawal ──────────────────────────────────────────────────────────────

func (s *payoutService) RequestWithdrawal(ctx context.Context, hostID uuid.UUID, req WithdrawalRequest) (*models.Payment, error) {
	fmt.Printf("[PAYOUT] RequestWithdrawal started: hostID=%s, amount=%d, idempotencyKey=%s\n", hostID, req.AmountCents, req.IdempotencyKey)

	if req.AmountCents <= 0 {
		fmt.Printf("[PAYOUT] RequestWithdrawal failed: invalid amount %d\n", req.AmountCents)
		return nil, errors.New("withdrawal amount must be positive")
	}

	// 1. Check idempotency via ledger entries (prevents duplicate withdrawals)
	if req.IdempotencyKey != "" {
		fmt.Printf("[PAYOUT] Checking idempotency for key: %s\n", req.IdempotencyKey)
		existing, err := s.ledgerRepo.GetByIdempotencyKey(ctx, req.IdempotencyKey)
		if err == nil && existing != nil {
			// Already processed this withdrawal — return the payment if it exists
			fmt.Printf("[PAYOUT] Idempotent request - withdrawal already processed\n")
			payment, err := s.paymentRepo.GetByIdempotencyKey(ctx, req.IdempotencyKey)
			if err == nil && payment != nil {
				return payment, nil
			}
		}
	}

	// 2. Get host and check fraud flags
	fmt.Printf("[PAYOUT] Fetching host: %s\n", hostID)
	host, err := s.hostRepo.GetByID(ctx, hostID)
	if err != nil {
		fmt.Printf("[PAYOUT] Host fetch error: %v\n", err)
		return nil, err
	}
	if host == nil {
		fmt.Printf("[PAYOUT] Host not found: %s\n", hostID)
		return nil, errors.New("host not found")
	}

	fmt.Printf("[PAYOUT] Checking fraud flags for user: %s\n", host.UserID)
	flagged, err := s.payoutRepo.HasActiveFraudFlag(ctx, host.UserID)
	if err != nil {
		fmt.Printf("[PAYOUT] Fraud flag check error: %v\n", err)
		return nil, err
	}
	if flagged {
		fmt.Printf("[PAYOUT] Account flagged for fraud: userID=%s\n", host.UserID)
		return nil, errors.New("account is blocked due to suspicious activity")
	}

	// 3. Get host account
	fmt.Printf("[PAYOUT] Fetching host account\n")
	account, err := s.accountRepo.GetByOwner(ctx, models.AccountOwnerHost, hostID)
	if err != nil {
		fmt.Printf("[PAYOUT] Account fetch error: %v\n", err)
		return nil, err
	}
	if account == nil {
		fmt.Printf("[PAYOUT] Host account not found\n")
		return nil, errors.New("host account not found")
	}
	fmt.Printf("[PAYOUT] Account found: accountID=%s, currentBalance=%d\n", account.ID, account.BalanceCents)

	// 3b. Available-to-withdraw gate.
	//
	// The host's spendable balance is computed from host_earnings (where booking
	// earnings live by design — see skill "3c"), minus any payouts already in
	// flight or paid out. This closes two bugs at once:
	//   C2  — withdrawing money the host doesn't have / withdrawing the same
	//         balance repeatedly (the old code never debited anything).
	//   "Case B" — withdrawing a booking's earning before the cancellation
	//         window has passed (pending_clearance_cents holds that back).
	//
	// available = event_passed_earnings − active_payouts
	// where event_passed_earnings is the sum of net_earning_cents on confirmed
	// bookings whose occurrence_date < now() (computed live from the bookings
	// table — single source of truth), and active_payouts is the sum of
	// payout/withdrawal payments whose status is pending / processing / completed.
	breakdown, err := s.bookingRepo.GetHostEarningsBreakdown(ctx, hostID)
	if err != nil {
		return nil, fmt.Errorf("failed to compute earnings breakdown: %w", err)
	}
	activePayouts, err := s.paymentRepo.SumActivePayoutAmountByAccount(ctx, account.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to compute active payouts: %w", err)
	}
	available := breakdown.EventPassedCents - activePayouts
	fmt.Printf("[PAYOUT] Available check: total=%d, event_passed=%d, event_upcoming=%d, in_flight=%d => available=%d, requested=%d\n",
		breakdown.TotalCents, breakdown.EventPassedCents, breakdown.EventUpcomingCents, activePayouts, available, req.AmountCents)
	if available < 0 {
		// Defensive — should never happen, but don't let a corrupted aggregate
		// allow a withdrawal.
		return nil, errors.New("withdrawable balance is negative; contact support")
	}
	if req.AmountCents > available {
		return nil, fmt.Errorf("insufficient withdrawable balance: requested %d, available %d (event-passed earnings minus payouts in flight; pending %d will unlock as events occur)",
			req.AmountCents, available, breakdown.EventUpcomingCents)
	}

	// 4. Determine payout method
	fmt.Printf("[PAYOUT] Selecting payout method\n")
	var payoutMethod *models.PayoutMethod
	if req.PayoutMethodID != nil {
		fmt.Printf("[PAYOUT] Using specified method: %s\n", *req.PayoutMethodID)
		payoutMethod, err = s.payoutRepo.GetPayoutMethodByID(ctx, *req.PayoutMethodID)
		if err != nil {
			fmt.Printf("[PAYOUT] Payout method fetch error: %v\n", err)
			return nil, err
		}
	} else {
		fmt.Printf("[PAYOUT] Using primary payout method\n")
		payoutMethod, err = s.payoutRepo.GetPrimaryPayoutMethod(ctx, hostID)
		if err != nil {
			fmt.Printf("[PAYOUT] Primary payout method fetch error: %v\n", err)
			return nil, err
		}
	}
	if payoutMethod == nil {
		fmt.Printf("[PAYOUT] No payout method available\n")
		return nil, errors.New("no payout method found; please add a bank account or UPI")
	}
	if payoutMethod.HostID == nil || *payoutMethod.HostID != hostID {
		fmt.Printf("[PAYOUT] Payout method does not belong to host\n")
		return nil, errors.New("payout method does not belong to this host")
	}
	if !payoutMethod.IsVerified {
		fmt.Printf("[PAYOUT] Payout method not verified: %s\n", payoutMethod.ID)
		return nil, errors.New("payout method is not verified yet")
	}
	fmt.Printf("[PAYOUT] Payout method selected: methodID=%s, type=%s, verified=true\n", payoutMethod.ID, payoutMethod.Type)

	// 5. Generate idempotency key if not provided (must happen BEFORE ledger creation)
	idempotencyKey := req.IdempotencyKey
	if idempotencyKey == "" {
		idempotencyKey = fmt.Sprintf("payout_%s_%d", hostID, time.Now().UnixNano())
	}

	// 6. Create withdrawal ledger entry (immutable record of withdrawal request)
	fmt.Printf("[PAYOUT] Creating withdrawal ledger entry: accountID=%s, amount=%d\n", account.ID, req.AmountCents)

	withdrawalLedger := &models.TransactionLedger{
		ID:             uuid.New(),
		AccountID:      account.ID,
		Type:           models.LedgerTypeWithdrawalDebit,
		AmountCents:    -req.AmountCents, // NEGATIVE = debit from host
		ReferenceID:    &account.ID,
		ReferenceType:  strPtr("account"),
		IdempotencyKey: strPtr(idempotencyKey),
		Description:    strPtr(fmt.Sprintf("Withdrawal request to %s", payoutMethod.Type)),
		Status:         models.LedgerStatusPending, // Pending until provider confirms
		CreatedAt:      time.Now(),
		CreatedBy:      &hostID,
	}

	withdrawalLedger, err = s.ledgerRepo.Create(ctx, withdrawalLedger)
	if err != nil {
		fmt.Printf("[PAYOUT] Failed to create withdrawal ledger entry: %v\n", err)
		return nil, fmt.Errorf("failed to record withdrawal: %w", err)
	}
	fmt.Printf("[PAYOUT] Withdrawal ledger entry created successfully\n")

	// 7. Create payment record
	displayRef := fmt.Sprintf("TXN-%05d", time.Now().UnixMilli()%100000)

	payment := &models.Payment{
		ID:               uuid.New(),
		IdempotencyKey:   idempotencyKey,
		AccountID:        account.ID,
		Type:             models.PaymentTypePayout,
		AmountCents:      req.AmountCents,
		Status:           models.PaymentStatusPending,
		RetryCount:       0,
		PayoutMethodID:   &payoutMethod.ID,
		DisplayReference: &displayRef,
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}

	fmt.Printf("[PAYOUT] Creating payment record: paymentID=%s, displayRef=%s\n", payment.ID, displayRef)
	if err := s.paymentRepo.Create(ctx, payment); err != nil {
		fmt.Printf("[PAYOUT] Payment record creation failed: %v (rolling back withdrawal with reversal ledger)\n", err)
		// Create reversal ledger entry instead of direct credit
		reversalLedger := &models.TransactionLedger{
			ID:                 uuid.New(),
			AccountID:          account.ID,
			Type:               models.LedgerTypeWebhookReversal,
			AmountCents:        req.AmountCents, // POSITIVE = credit back
			ReferenceID:        &withdrawalLedger.ID,
			ReferenceType:      strPtr("ledger"),
			IdempotencyKey:     strPtr(fmt.Sprintf("%s_reversal", idempotencyKey)),
			Description:        strPtr("Reversal - payment record creation failed"),
			Status:             models.LedgerStatusCompleted,
			ReversalOfLedgerID: &withdrawalLedger.ID,
			CreatedAt:          time.Now(),
			CreatedBy:          nil, // System operation
		}
		_, _ = s.ledgerRepo.Create(ctx, reversalLedger)
		return nil, err
	}
	fmt.Printf("[PAYOUT] Payment record created successfully\n")

	// 8. Call external provider (async in production, sync with mock)
	transferReq := payout.TransferRequest{
		PaymentID:      payment.ID,
		AmountCents:    req.AmountCents,
		MethodType:     string(payoutMethod.Type),
		IdempotencyKey: idempotencyKey,
	}
	// Use the Cashfree beneficiary ID stored in the payout method
	if payoutMethod.CashfreeBeID != nil {
		transferReq.BeneID = *payoutMethod.CashfreeBeID
	} else {
		// Fallback: generate from method ID if not stored (shouldn't happen in normal flow)
		transferReq.BeneID = strings.ReplaceAll(payoutMethod.ID.String(), "-", "")
	}
	if payoutMethod.BeneficiaryName != nil {
		transferReq.BeneficiaryName = *payoutMethod.BeneficiaryName
	}
	if payoutMethod.Type == models.PayoutMethodBank {
		if payoutMethod.AccountNumberEncrypted != nil {
			transferReq.AccountNumber = *payoutMethod.AccountNumberEncrypted
		}
		if payoutMethod.IFSC != nil {
			transferReq.IFSC = *payoutMethod.IFSC
		}
		if payoutMethod.BankName != nil {
			transferReq.BankName = *payoutMethod.BankName
		}
	} else if payoutMethod.Type == models.PayoutMethodUPI {
		if payoutMethod.UPIID != nil {
			transferReq.UPIID = *payoutMethod.UPIID
		}
	}

	// Update status to processing
	fmt.Printf("[PAYOUT] Updating payment status to PROCESSING\n")
	_ = s.paymentRepo.UpdateStatus(ctx, payment.ID, models.PaymentStatusProcessing, nil)
	payment.Status = models.PaymentStatusProcessing

	fmt.Printf("[PAYOUT] Calling provider: InitiateTransfer with amount=%d, method=%s\n", req.AmountCents, transferReq.MethodType)
	resp, err := s.provider.InitiateTransfer(ctx, transferReq)
	if err != nil {
		fmt.Printf("[PAYOUT] Provider call failed: %v (marking failed + writing reversal ledger)\n", err)
		// Provider call failed. IncrementRetry sets payment.status='failed', which
		// drops this payment from the active-payouts sum — restoring the host's
		// available balance automatically. NO accountRepo.Credit: nothing was
		// debited (host money lives in host_earnings, not accounts.balance_cents
		// — see skill "3c"); the old code's Credit here was a bug that inflated
		// the host wallet by req.AmountCents on every failed payout.
		errMsg := err.Error()
		_ = s.paymentRepo.IncrementRetry(ctx, payment.ID, errMsg)
		// Balance the pending withdrawal_debit ledger entry with a reversal so
		// SUM(ledger) for this account stays in line.
		_, _ = s.ledgerRepo.Create(ctx, &models.TransactionLedger{
			ID:                 uuid.New(),
			AccountID:          account.ID,
			Type:               models.LedgerTypeWebhookReversal,
			AmountCents:        req.AmountCents,
			ReferenceID:        &withdrawalLedger.ID,
			ReferenceType:      strPtr("ledger"),
			IdempotencyKey:     strPtr(fmt.Sprintf("%s_reversal", idempotencyKey)),
			Description:        strPtr("Reversal - provider transfer initiation failed"),
			Status:             models.LedgerStatusCompleted,
			ReversalOfLedgerID: &withdrawalLedger.ID,
			CreatedAt:          time.Now(),
		})
		payment.Status = models.PaymentStatusFailed
		payment.LastError = &errMsg
		return payment, nil
	}

	fmt.Printf("[PAYOUT] Provider response: status=%s, providerRefID=%s, error=%s\n", resp.Status, resp.ProviderRefID, resp.Error)

	// 9. Handle provider response
	if resp.Status == "completed" {
		fmt.Printf("[PAYOUT] Payment completed successfully by provider\n")
		_ = s.paymentRepo.UpdateStatus(ctx, payment.ID, models.PaymentStatusCompleted, nil)
		payment.Status = models.PaymentStatusCompleted
		s.dispatcher.Publish(event.PayoutCompleted, payment)
		fmt.Printf("[PAYOUT] Payment finalized: paymentID=%s, amount=%d, status=COMPLETED\n", payment.ID, req.AmountCents)
	} else if resp.Status == "failed" {
		fmt.Printf("[PAYOUT] Payment failed by provider: %s (marking failed + writing reversal ledger)\n", resp.Error)
		// Same shape as the InitiateTransfer-error path above: failed payment
		// drops out of active_payouts so available restores; NO accountRepo.Credit
		// (nothing was debited — see "3c"); add a reversal ledger entry to keep
		// SUM(ledger) for this account balanced.
		_ = s.paymentRepo.IncrementRetry(ctx, payment.ID, resp.Error)
		_, _ = s.ledgerRepo.Create(ctx, &models.TransactionLedger{
			ID:                 uuid.New(),
			AccountID:          account.ID,
			Type:               models.LedgerTypeWebhookReversal,
			AmountCents:        req.AmountCents,
			ReferenceID:        &withdrawalLedger.ID,
			ReferenceType:      strPtr("ledger"),
			IdempotencyKey:     strPtr(fmt.Sprintf("%s_reversal", idempotencyKey)),
			Description:        strPtr("Reversal - provider reported transfer failed"),
			Status:             models.LedgerStatusCompleted,
			ReversalOfLedgerID: &withdrawalLedger.ID,
			CreatedAt:          time.Now(),
		})
		payment.Status = models.PaymentStatusFailed
		payment.LastError = &resp.Error
	} else {
		fmt.Printf("[PAYOUT] Payment status=%s - waiting for webhook update\n", resp.Status)
	}
	// If "processing", the webhook will finalize

	fmt.Printf("[PAYOUT] RequestWithdrawal completed: paymentID=%s, status=%s\n", payment.ID, payment.Status)
	return payment, nil
}

// ── Earnings Dashboard ──────────────────────────────────────────────────────

func (s *payoutService) GetEarningsSummary(ctx context.Context, hostID uuid.UUID) (*EarningsSummary, error) {
	fmt.Printf("[PAYOUT] GetEarningsSummary: hostID=%s\n", hostID)

	// Need the host account ID to scope the in-flight-payouts sum.
	account, err := s.accountRepo.GetByOwner(ctx, models.AccountOwnerHost, hostID)
	if err != nil {
		return nil, fmt.Errorf("load host account: %w", err)
	}
	if account == nil {
		return nil, errors.New("host account not found")
	}

	// Live breakdown from the bookings table — the single source of truth.
	// Cancelled / refunded bookings are naturally excluded.
	breakdown, err := s.bookingRepo.GetHostEarningsBreakdown(ctx, hostID)
	if err != nil {
		return nil, fmt.Errorf("compute earnings breakdown: %w", err)
	}

	// Amount paid out or in flight (pending / processing / completed payouts).
	inFlight, err := s.paymentRepo.SumActivePayoutAmountByAccount(ctx, account.ID)
	if err != nil {
		return nil, fmt.Errorf("sum active payouts: %w", err)
	}

	// Earnings still owed to host = lifetime − paid out.
	currentBalance := breakdown.TotalCents - inFlight
	if currentBalance < 0 {
		currentBalance = 0
	}
	// Withdrawable right now = event-passed earnings − payouts already made.
	available := breakdown.EventPassedCents - inFlight
	if available < 0 {
		available = 0
	}

	feeConfig, err := s.payoutRepo.GetPlatformFeeConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load platform fee config: %w", err)
	}
	if host, err := s.hostRepo.GetByID(ctx, hostID); err != nil {
		return nil, fmt.Errorf("load host: %w", err)
	} else if host != nil {
		feeConfig = models.EffectiveFeeConfig(feeConfig, host.PlatformFeePercentage)
	}

	// host_earnings aggregate is kept for legacy /  back-compat reads; the
	// authoritative numbers come from the bookings breakdown above.
	var estimatedClearanceAt *time.Time
	if earnings, _ := s.payoutRepo.GetHostEarnings(ctx, hostID); earnings != nil {
		estimatedClearanceAt = earnings.EstimatedClearanceAt
	}

	summary := &EarningsSummary{
		TotalEarningsCents:    breakdown.TotalCents,
		AvailableBalanceCents: available,
		PendingClearanceCents: breakdown.EventUpcomingCents,
		CurrentBalanceCents:   currentBalance,
		InFlightPayoutsCents:  inFlight,
		EstimatedClearanceAt:  estimatedClearanceAt,
		PlatformFee:           feeConfig,
	}
	fmt.Printf("[PAYOUT] GetEarningsSummary: host=%s total=%d available=%d pending=%d current=%d inflight=%d\n",
		hostID, summary.TotalEarningsCents, summary.AvailableBalanceCents,
		summary.PendingClearanceCents, summary.CurrentBalanceCents, summary.InFlightPayoutsCents)
	return summary, nil
}

// ── Payment History ─────────────────────────────────────────────────────────

// GetHostSales lists every booking on this host's events — buyer + event +
// amount + status — so the dashboard can show "where the ₹X came from."
// `fromDate` is optional; pass nil for all time.
func (s *payoutService) GetHostSales(ctx context.Context, hostID uuid.UUID, limit, offset int, fromDate *time.Time) ([]*repository.HostSale, error) {
	return s.bookingRepo.ListHostSales(ctx, hostID, limit, offset, fromDate)
}

func (s *payoutService) GetPayoutHistory(ctx context.Context, hostID uuid.UUID, limit, offset int) ([]*models.Payment, error) {
	fmt.Printf("[PAYOUT] GetPayoutHistory: hostID=%s, limit=%d, offset=%d\n", hostID, limit, offset)

	account, err := s.accountRepo.GetByOwner(ctx, models.AccountOwnerHost, hostID)
	if err != nil {
		fmt.Printf("[PAYOUT] GetPayoutHistory: account fetch error: %v\n", err)
		return nil, err
	}
	if account == nil {
		fmt.Printf("[PAYOUT] GetPayoutHistory: host account not found\n")
		return nil, errors.New("host account not found")
	}

	if limit <= 0 {
		limit = 20
	}

	payments, err := s.paymentRepo.ListByTypeAndAccount(ctx, account.ID, models.PaymentTypePayout, limit, offset)
	if err != nil {
		fmt.Printf("[PAYOUT] GetPayoutHistory: list error: %v\n", err)
		return nil, err
	}

	fmt.Printf("[PAYOUT] GetPayoutHistory: found %d payments\n", len(payments))
	for i, p := range payments {
		fmt.Printf("[PAYOUT]   [%d] paymentID=%s, amount=%d, status=%s, createdAt=%v\n",
			i+1, p.ID, p.AmountCents, p.Status, p.CreatedAt)
	}

	return payments, nil
}

// ── Webhook Handler ─────────────────────────────────────────────────────────

func (s *payoutService) HandlePayoutWebhook(ctx context.Context, paymentID uuid.UUID, status string, providerError string) error {
	fmt.Printf("[PAYOUT] HandlePayoutWebhook: paymentID=%s, status=%s, error=%s\n", paymentID, status, providerError)

	payment, err := s.paymentRepo.GetByID(ctx, paymentID)
	if err != nil {
		fmt.Printf("[PAYOUT] Webhook: payment fetch error: %v\n", err)
		return err
	}
	if payment == nil {
		fmt.Printf("[PAYOUT] Webhook: payment not found: %s\n", paymentID)
		return errors.New("payment not found")
	}
	if payment.Type != models.PaymentTypePayout {
		fmt.Printf("[PAYOUT] Webhook: payment is not a payout: %s\n", paymentID)
		return errors.New("payment is not a payout")
	}

	// Check webhook idempotency - prevent replay attacks
	webhookExecKey := fmt.Sprintf("cashfree_payout_%s_%s", paymentID, status)
	existing, err := s.ledgerRepo.GetWebhookExecution(ctx, "cashfree_payout", paymentID.String(), fmt.Sprintf("%s_%s", paymentID, status))
	if err == nil && existing != nil {
		fmt.Printf("[PAYOUT] Webhook: already processed - idempotency key=%s\n", webhookExecKey)
		return nil // Already processed this webhook
	}

	fmt.Printf("[PAYOUT] Webhook: processing payout update - current status=%s, new status=%s\n", payment.Status, status)

	switch status {
	case "completed":
		fmt.Printf("[PAYOUT] Webhook: marking payment as completed\n")
		if err := s.paymentRepo.UpdateStatus(ctx, paymentID, models.PaymentStatusCompleted, nil); err != nil {
			return err
		}
		// Record webhook execution after successful processing
		_ = s.ledgerRepo.RecordWebhookExecution(ctx, &models.WebhookExecution{
			ID:              uuid.New(),
			EventType:       "cashfree_payout",
			ProviderID:      "cashfree",
			ExternalEventID: fmt.Sprintf("%s_%s", paymentID, status),
			IdempotencyKey:  webhookExecKey,
			LedgerID:        nil,
			Status:          "success",
			ReceivedAt:      time.Now(),
			ProcessedAt:     time.Now(),
		})
		return nil

	case "failed":
		fmt.Printf("[PAYOUT] Webhook: payment failed, creating reversal ledger entry: amount=%d\n", payment.AmountCents)
		// Create reversal ledger entry to credit back the host
		reversalLedger := &models.TransactionLedger{
			ID:                  uuid.New(),
			AccountID:           payment.AccountID,
			Type:                models.LedgerTypeWebhookReversal,
			AmountCents:         payment.AmountCents, // POSITIVE = credit back
			ReferenceID:         &payment.ID,
			ReferenceType:       strPtr("payment"),
			IdempotencyKey:      strPtr(fmt.Sprintf("%s_failed_reversal", payment.IdempotencyKey)),
			Description:         strPtr(fmt.Sprintf("Payout failed reversal - %s", providerError)),
			Status:              models.LedgerStatusCompleted,
			ExternalReferenceID: strPtr(webhookExecKey),
			CreatedAt:           time.Now(),
			CreatedBy:           nil, // System actor
		}

		if _, err := s.ledgerRepo.Create(ctx, reversalLedger); err != nil {
			fmt.Printf("[PAYOUT] Webhook: reversal ledger creation failed: %v\n", err)
			return fmt.Errorf("failed to create reversal ledger on payout failure: %w", err)
		}

		if err := s.paymentRepo.IncrementRetry(ctx, paymentID, providerError); err != nil {
			return err
		}

		// Record webhook execution
		_ = s.ledgerRepo.RecordWebhookExecution(ctx, &models.WebhookExecution{
			ID:              uuid.New(),
			EventType:       "cashfree_payout",
			ProviderID:      "cashfree",
			ExternalEventID: fmt.Sprintf("%s_%s", paymentID, status),
			IdempotencyKey:  webhookExecKey,
			LedgerID:        &reversalLedger.ID,
			Status:          "success",
			ReceivedAt:      time.Now(),
			ProcessedAt:     time.Now(),
		})
		return nil

	case "reversed":
		fmt.Printf("[PAYOUT] Webhook: payment reversed, creating reversal ledger entry: amount=%d\n", payment.AmountCents)
		// Create reversal ledger entry to credit back the host
		reversalLedger := &models.TransactionLedger{
			ID:                  uuid.New(),
			AccountID:           payment.AccountID,
			Type:                models.LedgerTypeWebhookReversal,
			AmountCents:         payment.AmountCents, // POSITIVE = credit back
			ReferenceID:         &payment.ID,
			ReferenceType:       strPtr("payment"),
			IdempotencyKey:      strPtr(fmt.Sprintf("%s_reversed_reversal", payment.IdempotencyKey)),
			Description:         strPtr(fmt.Sprintf("Payout reversed - %s", providerError)),
			Status:              models.LedgerStatusCompleted,
			ExternalReferenceID: strPtr(webhookExecKey),
			CreatedAt:           time.Now(),
			CreatedBy:           nil, // System/webhook actor
		}

		if _, err := s.ledgerRepo.Create(ctx, reversalLedger); err != nil {
			fmt.Printf("[PAYOUT] Webhook: reversal ledger creation failed: %v\n", err)
			return fmt.Errorf("failed to create reversal ledger on payout reversal: %w", err)
		}

		if err := s.paymentRepo.UpdateStatus(ctx, paymentID, models.PaymentStatusReversed, &providerError); err != nil {
			return err
		}

		// Record webhook execution
		_ = s.ledgerRepo.RecordWebhookExecution(ctx, &models.WebhookExecution{
			ID:              uuid.New(),
			EventType:       "cashfree_payout",
			ProviderID:      "cashfree",
			ExternalEventID: fmt.Sprintf("%s_%s", paymentID, status),
			IdempotencyKey:  webhookExecKey,
			LedgerID:        &reversalLedger.ID,
			Status:          "success",
			ReceivedAt:      time.Now(),
			ProcessedAt:     time.Now(),
		})
		return nil

	default:
		fmt.Printf("[PAYOUT] Webhook: unknown payout status: %s\n", status)
		return fmt.Errorf("unknown payout status: %s", status)
	}
}

// ReconcileResult summarises a reconciliation run.
type ReconcileResult struct {
	Checked   int `json:"checked"`   // payouts inspected
	Finalized int `json:"finalized"` // transitioned to completed/failed/reversed
	Skipped   int `json:"skipped"`   // still processing at the provider
	Errors    int `json:"errors"`    // provider/finalize errors (left for next run)
}

// ReconcilePendingPayouts is the safety net for missed payout webhooks: it polls
// the provider for every payout stuck in 'processing' and, for those that have
// reached a terminal state, applies the SAME finalize logic the webhook uses
// (via HandlePayoutWebhook, which is idempotent — the reversal ledger's unique
// key and webhook_executions dedup mean a later real webhook can't double-apply).
func (s *payoutService) ReconcilePendingPayouts(ctx context.Context) (ReconcileResult, error) {
	var res ReconcileResult

	stuck, err := s.paymentRepo.ListStuckPayouts(ctx, 200)
	if err != nil {
		return res, fmt.Errorf("list stuck payouts: %w", err)
	}
	fmt.Printf("[PAYOUT] Reconcile: %d payout(s) in 'processing'\n", len(stuck))

	for _, p := range stuck {
		res.Checked++

		// Cashfree's transfer_id is our payment ID (see buildCashfreeTransferRequest),
		// so we can query status even when gateway_payment_id was never stored.
		resp, err := s.provider.CheckStatus(ctx, p.ID.String())
		if err != nil {
			fmt.Printf("[PAYOUT] Reconcile: CheckStatus failed for %s: %v\n", p.ID, err)
			res.Errors++
			continue
		}

		switch resp.Status {
		case "completed", "failed", "reversed":
			if err := s.HandlePayoutWebhook(ctx, p.ID, resp.Status, resp.Error); err != nil {
				fmt.Printf("[PAYOUT] Reconcile: finalize %s as %s failed: %v\n", p.ID, resp.Status, err)
				res.Errors++
				continue
			}
			fmt.Printf("[PAYOUT] Reconcile: finalized %s as %s\n", p.ID, resp.Status)
			res.Finalized++
		default:
			// "processing" / unknown — still in flight; leave for a later run.
			res.Skipped++
		}
	}

	fmt.Printf("[PAYOUT] Reconcile done: checked=%d finalized=%d skipped=%d errors=%d\n",
		res.Checked, res.Finalized, res.Skipped, res.Errors)
	return res, nil
}

// ── Admin Platform Payout Management ────────────────────────────────────────

func (s *payoutService) AddAdminPayoutMethod(ctx context.Context, req AddPayoutMethodRequest) (*models.PayoutMethod, error) {
	fmt.Printf("[PAYOUT] AddAdminPayoutMethod: type=%s\n", req.Type)

	// Mask + encrypt account number for bank type
	var lastFour *string
	var encrypted *string
	if req.Type == models.PayoutMethodBank && req.AccountNumber != nil {
		num := *req.AccountNumber
		if len(num) >= 4 {
			l4 := num[len(num)-4:]
			lastFour = &l4
		}
		encrypted = req.AccountNumber
	}

	// Check if admin already has a payout method
	existing, err := s.payoutRepo.ListAdminPayoutMethods(ctx)
	if err != nil {
		return nil, err
	}
	isPrimary := len(existing) == 0

	pm := &models.PayoutMethod{
		ID:                     uuid.New(),
		HostID:                 nil, // Platform account - no host
		Type:                   req.Type,
		BankName:               req.BankName,
		AccountType:            req.AccountType,
		LastFourDigits:         lastFour,
		AccountNumberEncrypted: encrypted,
		IFSC:                   req.IFSC,
		BeneficiaryName:        req.BeneficiaryName,
		UPIID:                  req.UPIID,
		IsVerified:             true,
		IsPrimary:              isPrimary,
		CreatedAt:              time.Now(),
		UpdatedAt:              time.Now(),
	}

	if err := s.payoutRepo.CreatePayoutMethod(ctx, pm); err != nil {
		return nil, err
	}

	fmt.Printf("[PAYOUT] AddAdminPayoutMethod: created method %s\n", pm.ID)
	return pm, nil
}

func (s *payoutService) ListAdminPayoutMethods(ctx context.Context) ([]*models.PayoutMethod, error) {
	fmt.Printf("[PAYOUT] ListAdminPayoutMethods\n")
	methods, err := s.payoutRepo.ListAdminPayoutMethods(ctx)
	if err != nil {
		return nil, err
	}
	return methods, nil
}

func (s *payoutService) SetAdminPrimaryMethod(ctx context.Context, methodID uuid.UUID) error {
	fmt.Printf("[PAYOUT] SetAdminPrimaryMethod: methodID=%s\n", methodID)

	pm, err := s.payoutRepo.GetPayoutMethodByID(ctx, methodID)
	if err != nil {
		return err
	}
	if pm == nil {
		return errors.New("payout method not found")
	}
	if pm.HostID != nil {
		return errors.New("method does not belong to platform")
	}

	return s.payoutRepo.SetAdminPrimary(ctx, methodID)
}

func (s *payoutService) DeleteAdminPayoutMethod(ctx context.Context, methodID uuid.UUID) error {
	fmt.Printf("[PAYOUT] DeleteAdminPayoutMethod: methodID=%s\n", methodID)

	pm, err := s.payoutRepo.GetPayoutMethodByID(ctx, methodID)
	if err != nil {
		return err
	}
	if pm == nil {
		return errors.New("payout method not found")
	}
	if pm.HostID != nil {
		return errors.New("method does not belong to platform")
	}
	if pm.IsPrimary {
		return errors.New("cannot delete the primary payout method")
	}

	return s.payoutRepo.DeletePayoutMethod(ctx, methodID)
}

func (s *payoutService) RequestAdminWithdrawal(ctx context.Context, req WithdrawalRequest) (*models.Payment, error) {
	fmt.Printf("[PAYOUT] RequestAdminWithdrawal: amount=%d\n", req.AmountCents)

	if req.AmountCents <= 0 {
		return nil, errors.New("withdrawal amount must be positive")
	}

	// 1. Check idempotency (use ledger as source of truth)
	if req.IdempotencyKey != "" {
		existing, err := s.ledgerRepo.GetByIdempotencyKey(ctx, req.IdempotencyKey)
		if err == nil && existing != nil {
			// Ledger entry exists, find associated payment by reference
			if existing.ReferenceID != nil {
				payment, _ := s.paymentRepo.GetByID(ctx, *existing.ReferenceID)
				if payment != nil {
					return payment, nil
				}
			}
		}
		// Fallback: check payment repo directly
		paymentRec, err := s.paymentRepo.GetByIdempotencyKey(ctx, req.IdempotencyKey)
		if err != nil {
			return nil, err
		}
		if paymentRec != nil {
			return paymentRec, nil
		}
	}

	// 2. Get platform account
	account, err := s.accountRepo.GetByOwner(ctx, models.AccountOwnerPlatform, uuid.Nil)
	if err != nil {
		return nil, err
	}
	if account == nil {
		return nil, errors.New("platform account not found")
	}

	// 3. Get payout method
	var payoutMethod *models.PayoutMethod
	if req.PayoutMethodID != nil {
		payoutMethod, err = s.payoutRepo.GetPayoutMethodByID(ctx, *req.PayoutMethodID)
		if err != nil {
			return nil, err
		}
	} else {
		payoutMethod, err = s.payoutRepo.GetAdminPrimaryPayoutMethod(ctx)
		if err != nil {
			return nil, err
		}
	}

	if payoutMethod == nil {
		return nil, errors.New("no payout method found")
	}

	// 4. Create withdrawal ledger entry (immutable record)
	fmt.Printf("[PAYOUT] Creating admin withdrawal ledger entry: accountID=%s, amount=%d\n", account.ID, req.AmountCents)

	withdrawalLedger := &models.TransactionLedger{
		ID:             uuid.New(),
		AccountID:      account.ID,
		Type:           models.LedgerTypeWithdrawalDebit,
		AmountCents:    -req.AmountCents, // NEGATIVE = debit from platform
		ReferenceID:    &account.ID,
		ReferenceType:  strPtr("account"),
		IdempotencyKey: strPtr(req.IdempotencyKey),
		Description:    strPtr(fmt.Sprintf("Admin withdrawal to %s", payoutMethod.Type)),
		Status:         models.LedgerStatusPending,
		CreatedAt:      time.Now(),
		CreatedBy:      nil, // System operation
	}

	withdrawalLedger, err = s.ledgerRepo.Create(ctx, withdrawalLedger)
	if err != nil {
		fmt.Printf("[PAYOUT] Failed to create admin withdrawal ledger: %v\n", err)
		return nil, fmt.Errorf("failed to record withdrawal: %w", err)
	}

	// 5. Create payment record
	idempotencyKey := req.IdempotencyKey
	if idempotencyKey == "" {
		idempotencyKey = fmt.Sprintf("admin_payout_%d", time.Now().UnixNano())
	}
	displayRef := fmt.Sprintf("ADM-%05d", time.Now().UnixMilli()%100000)

	payment := &models.Payment{
		ID:               uuid.New(),
		IdempotencyKey:   idempotencyKey,
		AccountID:        account.ID,
		Type:             models.PaymentTypePayout,
		AmountCents:      req.AmountCents,
		Status:           models.PaymentStatusPending,
		PayoutMethodID:   &payoutMethod.ID,
		DisplayReference: &displayRef,
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}

	if err := s.paymentRepo.Create(ctx, payment); err != nil {
		fmt.Printf("[PAYOUT] Payment record creation failed: %v (rolling back with reversal ledger)\n", err)
		// Create reversal ledger entry instead of direct credit
		reversalLedger := &models.TransactionLedger{
			ID:                 uuid.New(),
			AccountID:          account.ID,
			Type:               models.LedgerTypeWebhookReversal,
			AmountCents:        req.AmountCents, // POSITIVE = credit back
			ReferenceID:        &withdrawalLedger.ID,
			ReferenceType:      strPtr("ledger"),
			IdempotencyKey:     strPtr(fmt.Sprintf("%s_reversal", req.IdempotencyKey)),
			Description:        strPtr("Reversal - admin payment record creation failed"),
			Status:             models.LedgerStatusCompleted,
			ReversalOfLedgerID: &withdrawalLedger.ID,
			CreatedAt:          time.Now(),
			CreatedBy:          nil, // System operation, no user actor
		}
		_, _ = s.ledgerRepo.Create(ctx, reversalLedger)
		return nil, err
	}

	// 6. Call Cashfree
	transferReq := payout.TransferRequest{
		PaymentID:      payment.ID,
		AmountCents:    req.AmountCents,
		MethodType:     string(payoutMethod.Type),
		IdempotencyKey: idempotencyKey,
	}
	// Use the Cashfree beneficiary ID stored in the payout method
	if payoutMethod.CashfreeBeID != nil {
		transferReq.BeneID = *payoutMethod.CashfreeBeID
	} else {
		// Fallback: generate from method ID if not stored (shouldn't happen in normal flow)
		transferReq.BeneID = strings.ReplaceAll(payoutMethod.ID.String(), "-", "")
	}
	if payoutMethod.BeneficiaryName != nil {
		transferReq.BeneficiaryName = *payoutMethod.BeneficiaryName
	}
	if payoutMethod.Type == models.PayoutMethodBank {
		if payoutMethod.AccountNumberEncrypted != nil {
			transferReq.AccountNumber = *payoutMethod.AccountNumberEncrypted
		}
		if payoutMethod.IFSC != nil {
			transferReq.IFSC = *payoutMethod.IFSC
		}
		if payoutMethod.BankName != nil {
			transferReq.BankName = *payoutMethod.BankName
		}
	} else {
		if payoutMethod.UPIID != nil {
			transferReq.UPIID = *payoutMethod.UPIID
		}
	}

	_ = s.paymentRepo.UpdateStatus(ctx, payment.ID, models.PaymentStatusProcessing, nil)

	resp, err := s.provider.InitiateTransfer(ctx, transferReq)
	if err != nil {
		errMsg := err.Error()
		_ = s.paymentRepo.IncrementRetry(ctx, payment.ID, errMsg)
		_ = s.accountRepo.Credit(ctx, account.ID, req.AmountCents)
		return payment, nil
	}

	if resp.Status == "completed" {
		_ = s.paymentRepo.UpdateStatus(ctx, payment.ID, models.PaymentStatusCompleted, nil)
		payment.Status = models.PaymentStatusCompleted
	} else if resp.Status == "processing" {
		payment.Status = models.PaymentStatusProcessing
	} else if resp.Status == "failed" {
		_ = s.paymentRepo.UpdateStatus(ctx, payment.ID, models.PaymentStatusFailed, &resp.Error)
		_ = s.accountRepo.Credit(ctx, account.ID, req.AmountCents)
		payment.Status = models.PaymentStatusFailed
	}

	fmt.Printf("[PAYOUT] RequestAdminWithdrawal: payment=%s, status=%s\n", payment.ID, payment.Status)
	return payment, nil
}

func (s *payoutService) GetPlatformBalance(ctx context.Context) (*PlatformBalanceInfo, error) {
	fmt.Printf("[PAYOUT] GetPlatformBalance\n")

	account, err := s.accountRepo.GetByOwner(ctx, models.AccountOwnerPlatform, uuid.Nil)
	if err != nil {
		return nil, err
	}
	if account == nil {
		return nil, errors.New("platform account not found")
	}

	return &PlatformBalanceInfo{
		AccountID:            account.ID,
		BalanceCents:         account.BalanceCents,
		CollectedFromBooking: account.BalanceCents,
	}, nil
}

func (s *payoutService) GetPlatformSetting(ctx context.Context, key string) (json.RawMessage, error) {
	return s.payoutRepo.GetPlatformSetting(ctx, key)
}

func (s *payoutService) GetPlatformFeeConfig(ctx context.Context) (*models.PlatformFeeConfig, error) {
	return s.payoutRepo.GetPlatformFeeConfig(ctx)
}

func (s *payoutService) SavePlatformSetting(ctx context.Context, key string, value json.RawMessage) error {
	return s.payoutRepo.SavePlatformSetting(ctx, key, value)
}
