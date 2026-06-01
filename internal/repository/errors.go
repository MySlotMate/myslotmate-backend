package repository

import "errors"

// Sentinel errors for repository operations.
var (
	ErrInsufficientBalance = errors.New("insufficient balance")
	ErrNotFound            = errors.New("record not found")
	// ErrDuplicateKey signals that an idempotency-key INSERT collided. Callers
	// treat this as "another path already did the work; this call is a no-op."
	// Returned by TransactionLedgerRepository.Create.
	ErrDuplicateKey = errors.New("duplicate idempotency key")
	// ErrDuplicateWebhook signals that a webhook with the same provider/event
	// id was already recorded — i.e. a replay or concurrent re-delivery. The
	// HTTP handler should respond 200 OK without re-running the side effects.
	// Returned by TransactionLedgerRepository.RecordWebhookExecution.
	ErrDuplicateWebhook = errors.New("webhook already processed")
)
