package payout

import (
	"context"

	"github.com/google/uuid"
)

// TransferRequest contains everything the external provider needs to initiate a payout.
type TransferRequest struct {
	PaymentID   uuid.UUID
	AmountCents int64
	// Bank details
	BankName        string
	AccountNumber   string // decrypted at call time
	IFSC            string
	BeneficiaryName string
	// Optional beneficiary id to reuse an existing registered beneId at provider
	BeneID string
	// UPI details
	UPIID string
	// Method type
	MethodType string // "bank" or "upi"
	// Idempotency
	IdempotencyKey string
}

// TransferResponse is what the external provider returns.
type TransferResponse struct {
	ProviderRefID string // e.g. provider transfer/payout ID
	Status        string // "processing", "completed", "failed"
	Error         string // non-empty on failure
}

// Provider is the Strategy interface for external payout providers (Razorpay, Cashfree, etc.).
type Provider interface {
	// RegisterBeneficiary creates/registers a beneficiary without initiating a transfer.
	RegisterBeneficiary(ctx context.Context, req TransferRequest) error

	// InitiateTransfer sends money to the host's bank/UPI.
	InitiateTransfer(ctx context.Context, req TransferRequest) (*TransferResponse, error)

	// CheckStatus queries the provider for current transfer status.
	CheckStatus(ctx context.Context, providerRefID string) (*TransferResponse, error)

	// ValidateWebhookSignature verifies the callback is genuine. `timestamp` is
	// the x-webhook-timestamp header value (Cashfree signs timestamp+body).
	ValidateWebhookSignature(payload []byte, signature, timestamp string) bool
}
