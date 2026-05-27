package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"myslotmate-backend/internal/lib/event"
	"myslotmate-backend/internal/lib/identity"
	"myslotmate-backend/internal/lib/payment"
	"myslotmate-backend/internal/lib/notification"
	"myslotmate-backend/internal/lib/validation"
	"myslotmate-backend/internal/lib/worker"
	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"

	"github.com/google/uuid"
)

// UserService defines the business logic interface
type UserService interface {
	SignUp(ctx context.Context, req SignUpRequest) (*models.User, error)
	GetProfile(ctx context.Context, userID uuid.UUID) (*models.User, error)
	UpdateProfile(ctx context.Context, userID uuid.UUID, req UserProfileUpdateRequest) (*models.User, error)
	InitiateAadharVerification(ctx context.Context, userID uuid.UUID, aadharNumber string) (string, error)
	CompleteAadharVerification(ctx context.Context, userID uuid.UUID, transactionID string, otp string) error
	SaveExperience(ctx context.Context, userID, eventID uuid.UUID) error
	UnsaveExperience(ctx context.Context, userID, eventID uuid.UUID) error
	GetSavedExperiences(ctx context.Context, userID uuid.UUID) ([]*models.SavedExperience, error)
	IsExperienceSaved(ctx context.Context, userID, eventID uuid.UUID) (bool, error)
	GetWalletBalance(ctx context.Context, userID uuid.UUID) (*WalletBalanceResponse, error)
	InitiateTopUp(ctx context.Context, userID uuid.UUID, req TopUpRequest) (*TopUpOrderResponse, error)
	VerifyTopUp(ctx context.Context, userID uuid.UUID, req VerifyTopUpRequest) (*WalletBalanceResponse, error)
	CreditWalletFromWebhook(ctx context.Context, orderID string, razorpayPaymentID string) error
	RefundTopUpToSource(ctx context.Context, topupPaymentID uuid.UUID, req SourceRefundRequest) (*models.Payment, error)
	ApplyRefundWebhook(ctx context.Context, gatewayRefundID string, newStatus models.PaymentStatus, reason string) error
	GetWalletTransactions(ctx context.Context, userID uuid.UUID, limit, offset int) ([]*models.Payment, error)
	GetByAuthUID(ctx context.Context, authUID string) (*models.User, error)
	SendPhoneOTP(ctx context.Context, userID uuid.UUID) error
	VerifyPhoneOTP(ctx context.Context, userID uuid.UUID, otp string) error
}

type SignUpRequest struct {
	AuthUID   string
	Email     string
	Name      string
	PhnNumber string
	AvatarURL *string
}

type UserProfileUpdateRequest struct {
	Name      *string `json:"name,omitempty"`
	AvatarURL *string `json:"avatar_url,omitempty"`
	City      *string `json:"city,omitempty"`
}

// WalletBalanceResponse is returned for balance queries and top-ups.
type WalletBalanceResponse struct {
	AccountID    uuid.UUID `json:"account_id"`
	BalanceCents int64     `json:"balance_cents"`
}

// TopUpRequest is the input for wallet top-up (step 1: create order).
type TopUpRequest struct {
	AmountCents    int64  `json:"amount_cents"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// TopUpOrderResponse is returned after creating a Razorpay order. The client
// uses these fields to launch the Razorpay checkout SDK.
type TopUpOrderResponse struct {
	OrderID     string `json:"order_id"`     // Razorpay order_xxxxx
	AmountCents int64  `json:"amount_cents"` // in paise
	Currency    string `json:"currency"`     // "INR"
	KeyID       string `json:"key_id"`       // Razorpay public key for checkout SDK
	PaymentID   string `json:"payment_id"`   // our internal payment UUID
}

// VerifyTopUpRequest is sent by the client after Razorpay checkout completes.
// SourceRefundRequest is the admin-gated request to send wallet money back to
// the original payment instrument (card/UPI/netbank) via Razorpay's Refund API.
// The target top-up payment is identified separately (URL param on the admin
// route). See userService.RefundTopUpToSource.
type SourceRefundRequest struct {
	AmountCents    int64      // amount to refund in paise; partial allowed up to top-up headroom
	Reason         string     // human note, stored on the refund and forwarded as a Razorpay note
	IdempotencyKey string     // required — client-supplied; duplicate keys return the existing refund
	AdminActorID   *uuid.UUID // for audit (who triggered the refund); optional
}

type VerifyTopUpRequest struct {
	RazorpayOrderID   string `json:"razorpay_order_id"`
	RazorpayPaymentID string `json:"razorpay_payment_id"`
	RazorpaySignature string `json:"razorpay_signature"`
}

// userService implements UserService
type userService struct {
	repo            repository.UserRepository
	hostRepo        repository.HostRepository
	savedExpRepo    repository.SavedExperienceRepository
	accountRepo     repository.AccountRepository
	paymentRepo     repository.PaymentRepository
	ledgerRepo      repository.TransactionLedgerRepository
	workerPool      *worker.WorkerPool
	dispatcher      *event.Dispatcher
	aadharProvider  identity.AadharProvider
	paymentProvider payment.Provider
	notifService    notification.NotificationService
}

// NewUserService Factory for UserService
func NewUserService(
	repo repository.UserRepository,
	hostRepo repository.HostRepository,
	savedExpRepo repository.SavedExperienceRepository,
	ar repository.AccountRepository,
	pmr repository.PaymentRepository,
	lr repository.TransactionLedgerRepository,
	wp *worker.WorkerPool,
	dispatcher *event.Dispatcher,
	ap identity.AadharProvider,
	pp payment.Provider,
	ns notification.NotificationService,
) UserService {
	return &userService{
		repo:            repo,
		hostRepo:        hostRepo,
		savedExpRepo:    savedExpRepo,
		accountRepo:     ar,
		paymentRepo:     pmr,
		ledgerRepo:      lr,
		workerPool:      wp,
		dispatcher:      dispatcher,
		aadharProvider:  ap,
		paymentProvider: pp,
		notifService:    ns,
	}
}

// InitiateAadharVerification starts the verification flow
func (s *userService) InitiateAadharVerification(ctx context.Context, userID uuid.UUID, aadharNumber string) (string, error) {
	user, err := s.repo.GetByID(ctx, userID)
	if err != nil {
		return "", err
	}
	if user == nil {
		return "", errors.New("user not found")
	}
	if user.IsVerified {
		return "", errors.New("user is already verified")
	}

	txnID, err := s.aadharProvider.InitiateVerification(ctx, aadharNumber)
	if err != nil {
		return "", err
	}

	return txnID, nil
}

// CompleteAadharVerification validates the OTP and marks user as verified
func (s *userService) CompleteAadharVerification(ctx context.Context, userID uuid.UUID, transactionID string, otp string) error {
	res, err := s.aadharProvider.VerifyOTP(ctx, transactionID, otp)
	if err != nil {
		return err
	}
	if !res.Success {
		return errors.New("verification failed by provider")
	}

	if err := s.repo.SetVerified(ctx, userID); err != nil {
		return err
	}

	// s.dispatcher.Publish("user_verified", userID)

	return nil
}

// SignUp handles user registration logic
func (s *userService) SignUp(ctx context.Context, req SignUpRequest) (*models.User, error) {
	if req.Email == "" {
		return nil, errors.New("email is required")
	}

	exists, err := s.repo.ExistsByEmail(ctx, req.Email)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, errors.New("user already exists")
	}

	newUser := &models.User{
		ID:         uuid.New(),
		AuthUID:    req.AuthUID,
		Name:       req.Name,
		Email:      req.Email,
		PhnNumber:  req.PhnNumber,
		AvatarURL:  req.AvatarURL,
		IsVerified: false,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}

	if err := s.repo.Create(ctx, newUser); err != nil {
		return nil, err
	}

	// Publish Event (Observer Pattern) - "User Created"
	// This helps decouple this service from email service, analytics, etc.
	s.dispatcher.Publish(event.UserCreated, newUser)

	// Execute Background Task (Executor/Worker Pattern) - Example: Send Welcome Email
	// If you want explicit background task here (alternative to observing the event):
	s.workerPool.Submit(func() {
		// Simulate sending email
		fmt.Printf("Sending welcome email to %s (User ID: %s)\n", newUser.Email, newUser.ID)
		time.Sleep(2 * time.Second) // Simulate network delay
		fmt.Printf("Email sent to %s\n", newUser.Email)
	})

	return newUser, nil
}

func (s *userService) GetProfile(ctx context.Context, userID uuid.UUID) (*models.User, error) {
	user, err := s.repo.GetByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if user == nil {
		return nil, errors.New("user not found")
	}
	return user, nil
}

func (s *userService) UpdateProfile(ctx context.Context, userID uuid.UUID, req UserProfileUpdateRequest) (*models.User, error) {
	user, err := s.repo.GetByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if user == nil {
		return nil, errors.New("user not found")
	}

	if req.Name != nil {
		user.Name = *req.Name
	}
	if req.AvatarURL != nil {
		// Validate avatar URL: reject blob URLs and localhost URLs
		if err := validation.ValidateImageURL(*req.AvatarURL); err != nil {
			return nil, err
		}
		user.AvatarURL = req.AvatarURL
	}
	if req.City != nil {
		user.City = req.City
	}
	user.UpdatedAt = time.Now()

	if err := s.repo.Update(ctx, user); err != nil {
		return nil, err
	}
	return user, nil
}

func (s *userService) SaveExperience(ctx context.Context, userID, eventID uuid.UUID) error {
	se := &models.SavedExperience{
		ID:      uuid.New(),
		UserID:  userID,
		EventID: eventID,
		SavedAt: time.Now(),
	}
	return s.savedExpRepo.Save(ctx, se)
}

func (s *userService) UnsaveExperience(ctx context.Context, userID, eventID uuid.UUID) error {
	return s.savedExpRepo.Remove(ctx, userID, eventID)
}

func (s *userService) GetSavedExperiences(ctx context.Context, userID uuid.UUID) ([]*models.SavedExperience, error) {
	return s.savedExpRepo.ListByUserID(ctx, userID)
}

func (s *userService) IsExperienceSaved(ctx context.Context, userID, eventID uuid.UUID) (bool, error) {
	return s.savedExpRepo.Exists(ctx, userID, eventID)
}

func (s *userService) GetWalletBalance(ctx context.Context, userID uuid.UUID) (*WalletBalanceResponse, error) {
	account, err := s.accountRepo.GetByOwner(ctx, models.AccountOwnerUser, userID)
	if err != nil {
		return nil, err
	}
	if account == nil {
		return nil, errors.New("wallet not found")
	}
	return &WalletBalanceResponse{
		AccountID:    account.ID,
		BalanceCents: account.BalanceCents,
	}, nil
}

func (s *userService) InitiateTopUp(ctx context.Context, userID uuid.UUID, req TopUpRequest) (*TopUpOrderResponse, error) {
	if req.AmountCents <= 0 {
		return nil, errors.New("amount must be positive")
	}

	account, err := s.accountRepo.GetByOwner(ctx, models.AccountOwnerUser, userID)
	if err != nil {
		return nil, err
	}
	if account == nil {
		return nil, errors.New("wallet not found")
	}

	// Idempotency: if this key was already used, return the existing order info.
	if req.IdempotencyKey != "" {
		existing, err := s.paymentRepo.GetByIdempotencyKey(ctx, req.IdempotencyKey)
		if err != nil {
			return nil, err
		}
		if existing != nil && existing.GatewayOrderID != nil {
			return &TopUpOrderResponse{
				OrderID:     *existing.GatewayOrderID,
				AmountCents: existing.AmountCents,
				Currency:    "INR",
				KeyID:       s.paymentProvider.GetKeyID(),
				PaymentID:   existing.ID.String(),
			}, nil
		}
	}

	// Generate a unique idempotency key if none provided.
	idempotencyKey := req.IdempotencyKey
	if idempotencyKey == "" {
		idempotencyKey = fmt.Sprintf("topup_%s_%d", userID, time.Now().UnixNano())
	}
	paymentID := uuid.New()
	displayRef := fmt.Sprintf("TU-%05d", time.Now().UnixMilli()%100000)

	// 1. Create Razorpay order.
	orderResp, err := s.paymentProvider.CreateOrder(ctx, payment.OrderRequest{
		AmountCents: req.AmountCents,
		Currency:    "INR",
		ReceiptID:   paymentID.String(),
		Notes: map[string]string{
			"user_id":    userID.String(),
			"payment_id": paymentID.String(),
			"type":       "topup",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create razorpay order: %w", err)
	}

	// 2. Store a pending payment record so we can reconcile later.
	topupPayment := &models.Payment{
		ID:               paymentID,
		IdempotencyKey:   idempotencyKey,
		AccountID:        account.ID,
		Type:             models.PaymentTypeTopup,
		AmountCents:      req.AmountCents,
		Status:           models.PaymentStatusPending,
		GatewayOrderID:   &orderResp.OrderID,
		DisplayReference: &displayRef,
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}
	if err := s.paymentRepo.Create(ctx, topupPayment); err != nil {
		return nil, fmt.Errorf("failed to record pending payment: %w", err)
	}

	return &TopUpOrderResponse{
		OrderID:     orderResp.OrderID,
		AmountCents: orderResp.AmountCents,
		Currency:    orderResp.Currency,
		KeyID:       s.paymentProvider.GetKeyID(),
		PaymentID:   paymentID.String(),
	}, nil
}

// VerifyTopUp is called by the client after completing Razorpay checkout.
// It verifies the signature, credits the wallet, and marks the payment as completed.
func (s *userService) VerifyTopUp(ctx context.Context, userID uuid.UUID, req VerifyTopUpRequest) (*WalletBalanceResponse, error) {
	// 1. Verify Razorpay signature.
	if !s.paymentProvider.VerifyPaymentSignature(payment.VerifyRequest{
		OrderID:   req.RazorpayOrderID,
		PaymentID: req.RazorpayPaymentID,
		Signature: req.RazorpaySignature,
	}) {
		return nil, errors.New("invalid payment signature")
	}

	// 2. Look up the pending payment by gateway order ID.
	pmtRecord, err := s.paymentRepo.GetByGatewayOrderID(ctx, req.RazorpayOrderID)
	if err != nil {
		return nil, err
	}
	if pmtRecord == nil {
		return nil, errors.New("payment record not found for this order")
	}

	// Idempotency: if already completed, just return the balance.
	if pmtRecord.Status == models.PaymentStatusCompleted {
		balance, err := s.accountRepo.GetBalance(ctx, pmtRecord.AccountID)
		if err != nil {
			return nil, err
		}
		return &WalletBalanceResponse{AccountID: pmtRecord.AccountID, BalanceCents: balance}, nil
	}

	// 3. Credit the wallet.
	if err := s.accountRepo.Credit(ctx, pmtRecord.AccountID, pmtRecord.AmountCents); err != nil {
		return nil, fmt.Errorf("failed to credit wallet: %w", err)
	}

	// 3b. Record the top-up in the transaction ledger so the ledger stays in sync
	// with accounts.balance_cents. Idempotent — see recordTopUpLedger.
	s.recordTopUpLedger(ctx, pmtRecord)

	// 4. Mark payment as completed and store the Razorpay payment ID.
	gatewayPaymentID := req.RazorpayPaymentID
	pmtRecord.Status = models.PaymentStatusCompleted
	pmtRecord.GatewayPaymentID = &gatewayPaymentID
	pmtRecord.UpdatedAt = time.Now()
	_ = s.paymentRepo.Update(ctx, pmtRecord)

	// 5. Return updated balance.
	balance, err := s.accountRepo.GetBalance(ctx, pmtRecord.AccountID)
	if err != nil {
		return nil, err
	}
	return &WalletBalanceResponse{AccountID: pmtRecord.AccountID, BalanceCents: balance}, nil
}

// CreditWalletFromWebhook is the server-side fallback called by the webhook controller
// when Razorpay sends a payment.captured event. It ensures the wallet is credited even
// if the client-side verify call was missed.
func (s *userService) CreditWalletFromWebhook(ctx context.Context, orderID string, razorpayPaymentID string) error {
	pmtRecord, err := s.paymentRepo.GetByGatewayOrderID(ctx, orderID)
	if err != nil {
		return err
	}
	if pmtRecord == nil {
		return errors.New("payment record not found for order")
	}

	// Already credited — nothing to do.
	if pmtRecord.Status == models.PaymentStatusCompleted {
		return nil
	}

	// Credit wallet.
	if err := s.accountRepo.Credit(ctx, pmtRecord.AccountID, pmtRecord.AmountCents); err != nil {
		return fmt.Errorf("failed to credit wallet via webhook: %w", err)
	}

	// Record the top-up in the transaction ledger. Idempotent — see recordTopUpLedger.
	s.recordTopUpLedger(ctx, pmtRecord)

	pmtRecord.Status = models.PaymentStatusCompleted
	pmtRecord.GatewayPaymentID = &razorpayPaymentID
	pmtRecord.UpdatedAt = time.Now()
	_ = s.paymentRepo.Update(ctx, pmtRecord)

	return nil
}

// recordTopUpLedger writes a topup_credit entry to the transaction ledger for a
// completed wallet top-up, keeping the ledger in sync with accounts.balance_cents.
// It is idempotent: the idempotency key is derived from the payment ID, and the
// ledger's UNIQUE(idempotency_key) constraint means a concurrent VerifyTopUp and
// payment.captured webhook cannot create two entries for the same top-up.
// Best-effort: the wallet is already credited, so a ledger failure (including the
// expected duplicate-key when the other path won the race) is logged, not surfaced.
func (s *userService) recordTopUpLedger(ctx context.Context, pmt *models.Payment) {
	key := "topup_ledger_" + pmt.ID.String()
	if _, err := s.ledgerRepo.Create(ctx, &models.TransactionLedger{
		ID:             uuid.New(),
		AccountID:      pmt.AccountID,
		Type:           models.LedgerTypeTopupCredit,
		AmountCents:    pmt.AmountCents, // POSITIVE = money into the user's wallet
		ReferenceID:    &pmt.ID,
		ReferenceType:  strPtr("payment"),
		IdempotencyKey: &key,
		Description:    strPtr("Wallet top-up via payment gateway"),
		Status:         models.LedgerStatusCompleted,
		CreatedAt:      time.Now(),
	}); err != nil {
		fmt.Printf("[TOPUP] ledger entry not written for payment %s (ok if duplicate): %v\n", pmt.ID, err)
	}
}

// RefundTopUpToSource initiates a Razorpay refund of a top-up payment back to
// the customer's original payment instrument (card/UPI/netbank). This is the
// admin-gated "refund to source" flow — the alternative to F4's wallet-credit
// refund. See bugs-and-risks.md "F7" / skill flows.md.
//
// Safety checks (in order):
//  1. Idempotency: a payments row with req.IdempotencyKey already exists →
//     return it. The payments.idempotency_key UNIQUE index serialises concurrent
//     duplicate requests at the DB level too.
//  2. Top-up must be a completed top-up with a Razorpay payment id.
//  3. Refund must not exceed the top-up's remaining source-refund headroom
//     (top-up amount minus refunds already issued against the same pay_xxx).
//  4. Refund must not exceed the user's current wallet balance — you can't
//     refund money the user has since spent on uncancelled bookings.
//
// Flow (this is NOT wrapped in a single DB transaction — same shape as the
// existing payout flow; see "Phase A/B/C" pattern):
//
//	A. Create refund payment row (status=pending) + ledger debit + Debit wallet.
//	B. Call Razorpay (external HTTP, must run outside any open tx).
//	C. On Razorpay success: update the payment row with rfnd_xxx + status.
//	   On Razorpay failure: reverse via writeSourceRefundReversal.
//
// Async finalisation: the `refund.processed` / `refund.failed` webhook calls
// ApplyRefundWebhook to flip the row to completed/failed.
func (s *userService) RefundTopUpToSource(ctx context.Context, topupPaymentID uuid.UUID, req SourceRefundRequest) (*models.Payment, error) {
	if req.AmountCents <= 0 {
		return nil, errors.New("refund amount must be positive")
	}
	if req.IdempotencyKey == "" {
		return nil, errors.New("idempotency_key is required for source refund")
	}

	// 1. Idempotency: short-circuit on a duplicate request.
	if existing, err := s.paymentRepo.GetByIdempotencyKey(ctx, req.IdempotencyKey); err == nil && existing != nil {
		return existing, nil
	}

	// 2. Load + validate the top-up.
	topup, err := s.paymentRepo.GetByID(ctx, topupPaymentID)
	if err != nil {
		return nil, fmt.Errorf("load top-up payment: %w", err)
	}
	if topup == nil {
		return nil, errors.New("top-up payment not found")
	}
	if topup.Type != models.PaymentTypeTopup {
		return nil, errors.New("payment is not a top-up; cannot source-refund")
	}
	if topup.Status != models.PaymentStatusCompleted {
		return nil, fmt.Errorf("top-up status is %q; only completed top-ups can be refunded to source", topup.Status)
	}
	if topup.GatewayPaymentID == nil || *topup.GatewayPaymentID == "" {
		return nil, errors.New("top-up has no Razorpay payment id; cannot source-refund")
	}

	// 3. Headroom: don't exceed what Razorpay will accept on this pay_xxx.
	alreadyRefunded, err := s.paymentRepo.SumActiveRefundsAgainstPayment(ctx, topup.ID)
	if err != nil {
		return nil, fmt.Errorf("compute refund headroom: %w", err)
	}
	headroom := topup.AmountCents - alreadyRefunded
	if req.AmountCents > headroom {
		return nil, fmt.Errorf("refund exceeds top-up headroom: requested %d, available %d (top-up %d, already refunded %d)",
			req.AmountCents, headroom, topup.AmountCents, alreadyRefunded)
	}

	// 4. Wallet check: can only refund money the user still has in their wallet.
	account, err := s.accountRepo.GetByID(ctx, topup.AccountID)
	if err != nil {
		return nil, fmt.Errorf("load user account: %w", err)
	}
	if account == nil {
		return nil, errors.New("user account not found")
	}
	if req.AmountCents > account.BalanceCents {
		return nil, fmt.Errorf("refund exceeds user's wallet balance: requested %d, available %d — cancel the relevant bookings first to put money back in the wallet",
			req.AmountCents, account.BalanceCents)
	}

	// ── Phase A: stake out the refund row + ledger + wallet debit ────────
	displayRef := fmt.Sprintf("RFS-%05d", time.Now().UnixMilli()%100000)
	refundPayment := &models.Payment{
		ID:                uuid.New(),
		IdempotencyKey:    req.IdempotencyKey,
		AccountID:         account.ID,
		Type:              models.PaymentTypeRefund,
		ReferenceID:       &topup.ID,
		AmountCents:       req.AmountCents,
		Status:            models.PaymentStatusPending,
		DisplayReference:  &displayRef,
		RefundOfPaymentID: &topup.ID,
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	}
	if err := s.paymentRepo.Create(ctx, refundPayment); err != nil {
		return nil, fmt.Errorf("create refund payment row: %w", err)
	}

	ledgerKey := "source_refund_" + refundPayment.ID.String()
	if _, err := s.ledgerRepo.Create(ctx, &models.TransactionLedger{
		ID:             uuid.New(),
		AccountID:      account.ID,
		Type:           models.LedgerTypeSourceRefundDebit,
		AmountCents:    -req.AmountCents, // NEGATIVE = money leaving the wallet
		ReferenceID:    &refundPayment.ID,
		ReferenceType:  strPtr("payment"),
		IdempotencyKey: &ledgerKey,
		Description:    strPtr(fmt.Sprintf("Refund to source: %s", req.Reason)),
		Status:         models.LedgerStatusPending,
		CreatedAt:      time.Now(),
		CreatedBy:      req.AdminActorID,
	}); err != nil {
		// Couldn't write the ledger entry — the payment row exists but is not
		// reflected in the journal. Mark the payment failed so it drops out of
		// active-refunds; admin can retry.
		_ = s.paymentRepo.UpdateStatus(ctx, refundPayment.ID, models.PaymentStatusFailed, strPtr(err.Error()))
		return nil, fmt.Errorf("write source refund ledger entry: %w", err)
	}

	if err := s.accountRepo.Debit(ctx, account.ID, req.AmountCents); err != nil {
		// Should not normally happen — we balance-checked above — but cover the race.
		s.writeSourceRefundReversal(ctx, account.ID, refundPayment.ID, req.AmountCents,
			"wallet debit failed: "+err.Error(), false)
		_ = s.paymentRepo.UpdateStatus(ctx, refundPayment.ID, models.PaymentStatusFailed, strPtr(err.Error()))
		return nil, fmt.Errorf("debit user wallet: %w", err)
	}

	// ── Phase B: call Razorpay (external HTTP, must NOT be inside a DB tx) ─
	resp, err := s.paymentProvider.CreateRefund(ctx, payment.RefundRequest{
		GatewayPaymentID: *topup.GatewayPaymentID,
		AmountCents:      req.AmountCents,
		Speed:            "normal",
		Notes: map[string]string{
			"refund_payment_id": refundPayment.ID.String(),
			"topup_payment_id":  topup.ID.String(),
			"reason":            req.Reason,
		},
	})
	if err != nil {
		// Razorpay rejected the refund. Reverse the wallet debit + ledger entry
		// and mark the payment failed so it drops out of active-refunds (the
		// headroom is restored).
		s.writeSourceRefundReversal(ctx, account.ID, refundPayment.ID, req.AmountCents,
			"razorpay refund failed: "+err.Error(), true)
		errMsg := err.Error()
		_ = s.paymentRepo.UpdateStatus(ctx, refundPayment.ID, models.PaymentStatusFailed, &errMsg)
		return nil, fmt.Errorf("razorpay refund failed: %w", err)
	}

	// ── Phase C: persist the Razorpay refund id + map the async status ───
	refundPayment.GatewayRefundID = &resp.RefundID
	refundPayment.UpdatedAt = time.Now()
	switch resp.Status {
	case "processed":
		refundPayment.Status = models.PaymentStatusCompleted
	case "failed":
		refundPayment.Status = models.PaymentStatusFailed
	default: // "pending" — the refund webhook will finalise.
		refundPayment.Status = models.PaymentStatusProcessing
	}
	if err := s.paymentRepo.Update(ctx, refundPayment); err != nil {
		// Non-fatal — the webhook can still finalise. Log so we don't silently
		// lose the rfnd_xxx linkage.
		fmt.Printf("[REFUND] Warning: failed to persist rfnd_xxx %s on payment %s: %v\n",
			resp.RefundID, refundPayment.ID, err)
	}

	return refundPayment, nil
}

// writeSourceRefundReversal is the compensating action when the Razorpay call
// fails after Phase A has already debited the wallet + written a ledger entry.
// Uses a deterministic idempotency key so a retry can't double-credit.
// If creditWallet is false, only the ledger entry is written (wallet debit
// hadn't run yet).
func (s *userService) writeSourceRefundReversal(ctx context.Context, accountID, refundPaymentID uuid.UUID, amountCents int64, reason string, creditWallet bool) {
	reversalKey := "source_refund_reversal_" + refundPaymentID.String()
	_, _ = s.ledgerRepo.Create(ctx, &models.TransactionLedger{
		ID:             uuid.New(),
		AccountID:      accountID,
		Type:           models.LedgerTypeWebhookReversal,
		AmountCents:    amountCents, // POSITIVE = money back into the wallet
		ReferenceID:    &refundPaymentID,
		ReferenceType:  strPtr("payment"),
		IdempotencyKey: &reversalKey,
		Description:    strPtr("Reversal - source refund failed: " + reason),
		Status:         models.LedgerStatusCompleted,
		CreatedAt:      time.Now(),
	})
	if creditWallet {
		if err := s.accountRepo.Credit(ctx, accountID, amountCents); err != nil {
			fmt.Printf("[REFUND] CRITICAL: failed to credit wallet on reversal for payment %s: %v — manual fix required\n", refundPaymentID, err)
		}
	}
}

// ApplyRefundWebhook is called by the Razorpay webhook handler for
// `refund.processed` and `refund.failed` events. It looks up the payments row
// by rfnd_xxx and finalises its status. On `failed`, the wallet debit is
// reversed (Razorpay didn't actually send the money to the card).
func (s *userService) ApplyRefundWebhook(ctx context.Context, gatewayRefundID string, newStatus models.PaymentStatus, reason string) error {
	pmt, err := s.paymentRepo.GetByGatewayRefundID(ctx, gatewayRefundID)
	if err != nil {
		return fmt.Errorf("look up refund payment: %w", err)
	}
	if pmt == nil {
		// Unknown refund — could be a webhook for a refund created outside this
		// system, or a race. Don't error the webhook (provider will retry); just
		// log and skip.
		fmt.Printf("[REFUND] Webhook for unknown gateway_refund_id=%s (status=%s) — ignored\n", gatewayRefundID, newStatus)
		return nil
	}
	// Already finalised — idempotent no-op.
	if pmt.Status == models.PaymentStatusCompleted || pmt.Status == models.PaymentStatusFailed {
		return nil
	}

	if newStatus == models.PaymentStatusFailed {
		// Razorpay failed the refund after we'd already debited the wallet —
		// reverse the debit so the user is whole.
		s.writeSourceRefundReversal(ctx, pmt.AccountID, pmt.ID, pmt.AmountCents,
			"razorpay refund webhook: "+reason, true)
		errMsg := reason
		return s.paymentRepo.UpdateStatus(ctx, pmt.ID, models.PaymentStatusFailed, &errMsg)
	}

	// Success path.
	return s.paymentRepo.UpdateStatus(ctx, pmt.ID, models.PaymentStatusCompleted, nil)
}

// GetWalletTransactions returns the user's payment history (top-ups, bookings,
// refunds) for the wallet-history UI. Ordered newest-first by created_at.
// Includes all statuses (pending/processing/completed/failed/reversed) so the
// user can see in-flight and historical activity.
func (s *userService) GetWalletTransactions(ctx context.Context, userID uuid.UUID, limit, offset int) ([]*models.Payment, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	account, err := s.accountRepo.GetByOwner(ctx, models.AccountOwnerUser, userID)
	if err != nil {
		return nil, fmt.Errorf("load user account: %w", err)
	}
	if account == nil {
		return nil, errors.New("user account not found")
	}
	return s.paymentRepo.ListByAccountID(ctx, account.ID, limit, offset)
}

// SendPhoneOTP generates and sends a 6-digit OTP to user's phone
func (s *userService) SendPhoneOTP(ctx context.Context, userID uuid.UUID) error {
	user, err := s.repo.GetByID(ctx, userID)
	if err != nil {
		return err
	}
	if user == nil {
		return errors.New("user not found")
	}

	if user.PhnNumber == "" {
		return errors.New("phone number not found for user")
	}

	// Rate limit: Don't send more than one OTP per minute
	if user.OTPExpiresAt != nil {
		// OTP expires in 10 minutes. If more than 9 minutes are left, it was sent less than a minute ago.
		if time.Until(*user.OTPExpiresAt) > 9*time.Minute {
			return errors.New("please wait a minute before requesting another OTP")
		}
	}

	// Generate 6-digit OTP
	otp := fmt.Sprintf("%06d", time.Now().UnixNano()%1000000)
	expiresAt := time.Now().Add(10 * time.Minute)

	// Store in database
	if err := s.repo.UpdateOTP(ctx, userID, otp, expiresAt); err != nil {
		return fmt.Errorf("failed to store OTP: %w", err)
	}

	// Send via SMS
	body := fmt.Sprintf("Your MySlotMate verification code is: %s. Valid for 10 minutes.", otp)
	return s.notifService.SendSMS(ctx, user.PhnNumber, body)
}

// VerifyPhoneOTP validates the provided OTP
func (s *userService) VerifyPhoneOTP(ctx context.Context, userID uuid.UUID, otp string) error {
	user, err := s.repo.GetByID(ctx, userID)
	if err != nil {
		return err
	}
	if user == nil {
		return errors.New("user not found")
	}

	if user.OTP == nil || *user.OTP == "" || user.OTPExpiresAt == nil {
		return errors.New("OTP not found")
	}

	if time.Now().After(*user.OTPExpiresAt) {
		return errors.New("OTP has expired")
	}

	if *user.OTP != otp {
		return errors.New("invalid OTP")
	}

	// Mark as verified in database
	if err := s.repo.SetPhoneVerified(ctx, userID); err != nil {
		return fmt.Errorf("failed to set phone as verified: %w", err)
	}

	// Clear OTP from database
	_ = s.repo.UpdateOTP(ctx, userID, "", time.Time{})

	return nil
}

// GetByAuthUID retrieves a user by their Firebase authentication UID
func (s *userService) GetByAuthUID(ctx context.Context, authUID string) (*models.User, error) {
	user, err := s.repo.GetByAuthUID(ctx, authUID)
	if err != nil {
		return nil, err
	}
	if user == nil {
		return nil, errors.New("user not found")
	}
	return user, nil
}
