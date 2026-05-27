package models

import (
	"time"

	"github.com/google/uuid"
)

// Payment is a transaction (booking, withdrawal, refund, payout, topup) with idempotency support.
type Payment struct {
	ID               uuid.UUID     `db:"id" json:"id"`
	IdempotencyKey   string        `db:"idempotency_key" json:"idempotency_key"`
	AccountID        uuid.UUID     `db:"account_id" json:"account_id"`
	Type             PaymentType   `db:"type" json:"type"`
	ReferenceID      *uuid.UUID    `db:"reference_id" json:"reference_id,omitempty"`
	AmountCents      int64         `db:"amount_cents" json:"amount_cents"`
	Status           PaymentStatus `db:"status" json:"status"`
	RetryCount       int           `db:"retry_count" json:"retry_count"`
	LastError        *string       `db:"last_error" json:"last_error,omitempty"`
	PayoutMethodID   *uuid.UUID    `db:"payout_method_id" json:"payout_method_id,omitempty"`
	DisplayReference *string       `db:"display_reference" json:"display_reference,omitempty"`   // e.g. TXN-88234
	GatewayOrderID   *string       `db:"gateway_order_id" json:"gateway_order_id,omitempty"`     // Razorpay order_xxxxx
	GatewayPaymentID *string       `db:"gateway_payment_id" json:"gateway_payment_id,omitempty"` // Razorpay pay_xxxxx

	// Source-refund linkage (admin-gated "refund to card" flow). Populated only
	// on refund-type payment rows that go back to the original instrument via
	// the Razorpay Refunds API; nil for cancellation→wallet refunds.
	GatewayRefundID    *string    `db:"gateway_refund_id" json:"gateway_refund_id,omitempty"`         // Razorpay rfnd_xxxxx
	RefundOfPaymentID  *uuid.UUID `db:"refund_of_payment_id" json:"refund_of_payment_id,omitempty"`   // the top-up being refunded

	CreatedAt time.Time `db:"created_at" json:"created_at"`
	UpdatedAt time.Time `db:"updated_at" json:"updated_at"`
}
