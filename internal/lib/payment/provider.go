package payment

import "context"

// OrderRequest contains everything needed to create a payment order.
type OrderRequest struct {
	AmountCents int64  // in paise (INR cents)
	Currency    string // "INR"
	ReceiptID   string // our internal reference (idempotency)
	Notes       map[string]string
}

// OrderResponse is returned after creating an order with the provider.
type OrderResponse struct {
	OrderID     string // provider's order ID (e.g. order_xxxxx)
	AmountCents int64
	Currency    string
	Status      string // "created"
}

// VerifyRequest contains data the client sends after completing checkout.
type VerifyRequest struct {
	OrderID   string // razorpay_order_id
	PaymentID string // razorpay_payment_id
	Signature string // razorpay_signature
}

// RefundRequest tells the provider to refund a captured payment back to its
// original instrument. Razorpay only refunds against a specific payment id, so
// the caller MUST know which top-up to refund.
type RefundRequest struct {
	GatewayPaymentID string            // Razorpay pay_xxxxx — the original payment to refund
	AmountCents      int64             // partial allowed; must be ≤ remaining headroom on the payment
	Notes            map[string]string // free-form metadata stored on the refund
	Speed            string            // "normal" (default) or "optimum"
}

// RefundResponse is returned after a refund is created. Status is async — the
// final outcome arrives via `refund.processed` / `refund.failed` webhooks.
type RefundResponse struct {
	RefundID    string // provider's refund id (e.g. rfnd_xxxxx)
	AmountCents int64
	Status      string // "pending" | "processed" | "failed"
}

// Provider is the Strategy interface for payment collection (top-up, future direct pay).
// This is separate from the payout.Provider which is for sending money out.
type Provider interface {
	// CreateOrder creates a payment order. Client uses the returned order_id
	// to complete payment via Razorpay checkout SDK.
	CreateOrder(ctx context.Context, req OrderRequest) (*OrderResponse, error)

	// VerifyPaymentSignature verifies the client-side callback signature
	// (razorpay_order_id|razorpay_payment_id signed with key_secret).
	VerifyPaymentSignature(req VerifyRequest) bool

	// ValidateWebhookSignature verifies an incoming webhook payload.
	ValidateWebhookSignature(payload []byte, signature string) bool

	// GetKeyID returns the public key ID for the client-side checkout SDK.
	GetKeyID() string

	// CreateRefund initiates a refund of a captured payment back to its
	// original source. Status is async — final outcome arrives via webhook.
	CreateRefund(ctx context.Context, req RefundRequest) (*RefundResponse, error)
}
