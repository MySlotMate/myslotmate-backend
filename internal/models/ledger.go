package models

import (
	"time"

	"github.com/google/uuid"
)

// ── LedgerTransactionType represents types of wallet transactions ─────────

type LedgerTransactionType string

const (
	// Credits (money in)
	LedgerTypeBookingCredit     LedgerTransactionType = "booking_credit"      // Host earned from booking
	LedgerTypePlatformFeeCredit LedgerTransactionType = "platform_fee_credit" // Platform earned from booking
	LedgerTypeRefundCredit      LedgerTransactionType = "refund_credit"       // User/Host refunded on cancellation
	LedgerTypeWebhookReversal   LedgerTransactionType = "webhook_reversal"    // Reversal due to failed payout
	LedgerTypeManualCredit      LedgerTransactionType = "manual_credit"       // Admin adjustment (with audit)
	LedgerTypeTopupCredit       LedgerTransactionType = "topup_credit"        // User wallet top-up via payment gateway

	// Debits (money out)
	LedgerTypeWithdrawalDebit   LedgerTransactionType = "withdrawal_debit"   // Host/Admin withdrawal request
	LedgerTypeCancellationDebit LedgerTransactionType = "cancellation_debit" // User booking cancellation
	LedgerTypePayoutDebit       LedgerTransactionType = "payout_debit"       // Payout initiated (temporary, reverses if fails)
	LedgerTypeManualDebit       LedgerTransactionType = "manual_debit"       // Admin adjustment (with audit)
	LedgerTypeSourceRefundDebit LedgerTransactionType = "source_refund_debit" // Wallet drained as money goes back to card (Razorpay refund)
)

// ── LedgerStatus represents status of ledger entry ──────────────────────

type LedgerStatus string

const (
	LedgerStatusPending   LedgerStatus = "pending"   // Async operation not yet confirmed
	LedgerStatusCompleted LedgerStatus = "completed" // Confirmed by webhook or immediate
	LedgerStatusFailed    LedgerStatus = "failed"    // Operation failed, should be reversed
	LedgerStatusReversed  LedgerStatus = "reversed"  // Explicitly reversed (e.g., refund)
)

// ── TransactionLedger is immutable journal of all wallet movements ────────

type TransactionLedger struct {
	ID                  uuid.UUID              `json:"id"`
	AccountID           uuid.UUID              `json:"account_id"`
	Type                LedgerTransactionType  `json:"type"`
	AmountCents         int64                  `json:"amount_cents"` // +ve = credit, -ve = debit
	ReferenceID         *uuid.UUID             `json:"reference_id,omitempty"`
	ReferenceType       *string                `json:"reference_type,omitempty"` // booking, payment, user, etc.
	IdempotencyKey      *string                `json:"idempotency_key,omitempty"`
	Description         *string                `json:"description,omitempty"`
	BalanceAfterCents   int64                  `json:"balance_after_cents"`
	ExternalReferenceID *string                `json:"external_reference_id,omitempty"` // Razorpay/Cashfree ID
	ReversalOfLedgerID  *uuid.UUID             `json:"reversal_of_ledger_id,omitempty"`
	Status              LedgerStatus           `json:"status"`
	Metadata            map[string]interface{} `json:"metadata,omitempty"`
	CreatedAt           time.Time              `json:"created_at"`
	CreatedBy           *uuid.UUID             `json:"created_by,omitempty"`
}

// ── WebhookExecution tracks processed webhooks (prevent replays) ─────────

type WebhookExecution struct {
	ID              uuid.UUID              `json:"id"`
	EventType       string                 `json:"event_type"`  // razorpay_payment, cashfree_payout
	ProviderID      string                 `json:"provider_id"` // razorpay, cashfree
	ExternalEventID string                 `json:"external_event_id"`
	IdempotencyKey  string                 `json:"idempotency_key"`
	LedgerID        *uuid.UUID             `json:"ledger_id,omitempty"`
	Status          string                 `json:"status"` // success, failed, skipped
	ErrorMessage    *string                `json:"error_message,omitempty"`
	RawPayload      map[string]interface{} `json:"raw_payload"`
	ReceivedAt      time.Time              `json:"received_at"`
	ProcessedAt     time.Time              `json:"processed_at"`
}

// ── AuditLog tracks sensitive operations ──────────────────────────────

type AuditLog struct {
	ID         uuid.UUID              `json:"id"`
	EntityType string                 `json:"entity_type"` // account, payout_method, ledger
	EntityID   uuid.UUID              `json:"entity_id"`
	Action     string                 `json:"action"` // created, updated, deleted, withdrawal_requested
	ActorID    *uuid.UUID             `json:"actor_id,omitempty"`
	ActorType  *string                `json:"actor_type,omitempty"` // user, admin, system
	OldValues  map[string]interface{} `json:"old_values,omitempty"`
	NewValues  map[string]interface{} `json:"new_values,omitempty"`
	IPAddress  *string                `json:"ip_address,omitempty"`
	UserAgent  *string                `json:"user_agent,omitempty"`
	CreatedAt  time.Time              `json:"created_at"`
}

// ── ReconciliationRun tracks daily balance verification ───────────────

type ReconciliationRun struct {
	ID                      uuid.UUID              `json:"id"`
	RunDate                 time.Time              `json:"run_date"`
	TotalAccountsChecked    int                    `json:"total_accounts_checked"`
	AccountsMatched         int                    `json:"accounts_matched"`
	AccountsDiscrepancy     int                    `json:"accounts_discrepancy"`
	TotalLedgerAmountCents  int64                  `json:"total_ledger_amount_cents"`
	TotalBalanceAmountCents int64                  `json:"total_balance_amount_cents"`
	Status                  string                 `json:"status"` // success, warning, critical
	ErrorDetails            map[string]interface{} `json:"error_details,omitempty"`
	RunAt                   time.Time              `json:"run_at"`
	CompletedAt             *time.Time             `json:"completed_at,omitempty"`
}

// ── ReconciliationDiscrepancy ────────────────────────────────────────

type ReconciliationDiscrepancy struct {
	ID                    uuid.UUID `json:"id"`
	ReconciliationRunID   uuid.UUID `json:"reconciliation_run_id"`
	AccountID             uuid.UUID `json:"account_id"`
	StoredBalanceCents    int64     `json:"stored_balance_cents"`
	LedgerCalculatedCents int64     `json:"ledger_calculated_cents"`
	DifferenceCents       int64     `json:"difference_cents"`
	Reason                *string   `json:"reason,omitempty"`
	Severity              string    `json:"severity"` // warning, critical
	CreatedAt             time.Time `json:"created_at"`
}
