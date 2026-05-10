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

	pmtRecord.Status = models.PaymentStatusCompleted
	pmtRecord.GatewayPaymentID = &razorpayPaymentID
	pmtRecord.UpdatedAt = time.Now()
	_ = s.paymentRepo.Update(ctx, pmtRecord)

	return nil
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
