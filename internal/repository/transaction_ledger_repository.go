package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"myslotmate-backend/internal/models"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// TransactionLedgerRepository handles all transaction journal operations
type TransactionLedgerRepository interface {
	// Create ledger entry and return it with calculated balance
	Create(ctx context.Context, entry *models.TransactionLedger) (*models.TransactionLedger, error)

	// Get single entry by ID
	GetByID(ctx context.Context, id uuid.UUID) (*models.TransactionLedger, error)

	// List all ledger entries for an account (paginated)
	ListByAccountID(ctx context.Context, accountID uuid.UUID, limit, offset int) ([]*models.TransactionLedger, error)

	// Calculate current balance from ledger (SUM of all amounts)
	CalculateBalance(ctx context.Context, accountID uuid.UUID) (int64, error)

	// Get entry by idempotency key to prevent duplicates
	GetByIdempotencyKey(ctx context.Context, key string) (*models.TransactionLedger, error)

	// Track webhook processing to prevent replays
	RecordWebhookExecution(ctx context.Context, exec *models.WebhookExecution) error
	GetWebhookExecution(ctx context.Context, eventType, providerID, externalEventID string) (*models.WebhookExecution, error)

	// Audit log
	LogAudit(ctx context.Context, log *models.AuditLog) error

	// Reconciliation
	RunReconciliation(ctx context.Context) (*models.ReconciliationRun, error)
	GetLastReconciliation(ctx context.Context) (*models.ReconciliationRun, error)

	// WithTx returns a copy of the repository bound to the given transaction.
	WithTx(tx *sql.Tx) TransactionLedgerRepository
}

type postgresTransactionLedgerRepository struct {
	db DBTX
}

func NewTransactionLedgerRepository(db *sql.DB) TransactionLedgerRepository {
	return &postgresTransactionLedgerRepository{db: db}
}

func (r *postgresTransactionLedgerRepository) WithTx(tx *sql.Tx) TransactionLedgerRepository {
	return &postgresTransactionLedgerRepository{db: tx}
}

// Create inserts a new ledger entry and recalculates balance
func (r *postgresTransactionLedgerRepository) Create(ctx context.Context, entry *models.TransactionLedger) (*models.TransactionLedger, error) {
	if entry.ID == uuid.Nil {
		entry.ID = uuid.New()
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now()
	}

	// Calculate balance after this entry
	currentBalance, err := r.CalculateBalance(ctx, entry.AccountID)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate current balance: %w", err)
	}
	entry.BalanceAfterCents = currentBalance + entry.AmountCents

	// Serialize metadata
	var metadataJSON []byte
	if entry.Metadata != nil {
		metadataJSON, _ = json.Marshal(entry.Metadata)
	}

	query := `
		INSERT INTO transaction_ledger (
			id, account_id, type, amount_cents, reference_id, reference_type,
			idempotency_key, description, balance_after_cents, external_reference_id,
			reversal_of_ledger_id, status, metadata, created_at, created_by
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15
		)
	`

	_, err = r.db.ExecContext(ctx, query,
		entry.ID,
		entry.AccountID,
		string(entry.Type),
		entry.AmountCents,
		entry.ReferenceID,
		entry.ReferenceType,
		entry.IdempotencyKey,
		entry.Description,
		entry.BalanceAfterCents,
		entry.ExternalReferenceID,
		entry.ReversalOfLedgerID,
		string(entry.Status),
		metadataJSON,
		entry.CreatedAt,
		entry.CreatedBy,
	)

	if err != nil {
		// Unique-constraint violation on idempotency_key: another path raced
		// us to insert this entry. Return a sentinel so callers can detect
		// "already credited" and skip the side effect.
		if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "23505" {
			return nil, ErrDuplicateKey
		}
		return nil, err
	}

	return entry, nil
}

// GetByID retrieves a single ledger entry
func (r *postgresTransactionLedgerRepository) GetByID(ctx context.Context, id uuid.UUID) (*models.TransactionLedger, error) {
	entry := &models.TransactionLedger{}
	query := `
		SELECT id, account_id, type, amount_cents, reference_id, reference_type,
		       idempotency_key, description, balance_after_cents, external_reference_id,
		       reversal_of_ledger_id, status, metadata, created_at, created_by
		FROM transaction_ledger WHERE id = $1
	`
	var metadataJSON []byte
	err := r.db.QueryRowContext(ctx, query, id).Scan(
		&entry.ID, &entry.AccountID, (*string)(&entry.Type), &entry.AmountCents,
		&entry.ReferenceID, &entry.ReferenceType, &entry.IdempotencyKey, &entry.Description,
		&entry.BalanceAfterCents, &entry.ExternalReferenceID, &entry.ReversalOfLedgerID,
		(*string)(&entry.Status), &metadataJSON, &entry.CreatedAt, &entry.CreatedBy,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}

	if metadataJSON != nil {
		_ = json.Unmarshal(metadataJSON, &entry.Metadata)
	}
	return entry, nil
}

// ListByAccountID retrieves all ledger entries for an account
func (r *postgresTransactionLedgerRepository) ListByAccountID(ctx context.Context, accountID uuid.UUID, limit, offset int) ([]*models.TransactionLedger, error) {
	query := `
		SELECT id, account_id, type, amount_cents, reference_id, reference_type,
		       idempotency_key, description, balance_after_cents, external_reference_id,
		       reversal_of_ledger_id, status, metadata, created_at, created_by
		FROM transaction_ledger
		WHERE account_id = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`
	rows, err := r.db.QueryContext(ctx, query, accountID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []*models.TransactionLedger
	for rows.Next() {
		entry := &models.TransactionLedger{}
		var metadataJSON []byte
		if err := rows.Scan(
			&entry.ID, &entry.AccountID, (*string)(&entry.Type), &entry.AmountCents,
			&entry.ReferenceID, &entry.ReferenceType, &entry.IdempotencyKey, &entry.Description,
			&entry.BalanceAfterCents, &entry.ExternalReferenceID, &entry.ReversalOfLedgerID,
			(*string)(&entry.Status), &metadataJSON, &entry.CreatedAt, &entry.CreatedBy,
		); err != nil {
			return nil, err
		}
		if metadataJSON != nil {
			_ = json.Unmarshal(metadataJSON, &entry.Metadata)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// CalculateBalance sums all ledger entries for account (source of truth for balance)
func (r *postgresTransactionLedgerRepository) CalculateBalance(ctx context.Context, accountID uuid.UUID) (int64, error) {
	var balance int64
	query := `SELECT COALESCE(SUM(amount_cents), 0) FROM transaction_ledger WHERE account_id = $1`
	err := r.db.QueryRowContext(ctx, query, accountID).Scan(&balance)
	return balance, err
}

// GetByIdempotencyKey prevents duplicate ledger entries
func (r *postgresTransactionLedgerRepository) GetByIdempotencyKey(ctx context.Context, key string) (*models.TransactionLedger, error) {
	entry := &models.TransactionLedger{}
	query := `
		SELECT id, account_id, type, amount_cents, reference_id, reference_type,
		       idempotency_key, description, balance_after_cents, external_reference_id,
		       reversal_of_ledger_id, status, metadata, created_at, created_by
		FROM transaction_ledger WHERE idempotency_key = $1 LIMIT 1
	`
	var metadataJSON []byte
	err := r.db.QueryRowContext(ctx, query, key).Scan(
		&entry.ID, &entry.AccountID, (*string)(&entry.Type), &entry.AmountCents,
		&entry.ReferenceID, &entry.ReferenceType, &entry.IdempotencyKey, &entry.Description,
		&entry.BalanceAfterCents, &entry.ExternalReferenceID, &entry.ReversalOfLedgerID,
		(*string)(&entry.Status), &metadataJSON, &entry.CreatedAt, &entry.CreatedBy,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if metadataJSON != nil {
		_ = json.Unmarshal(metadataJSON, &entry.Metadata)
	}
	return entry, nil
}

// ───────────────────────────────────────────────────────────────────────

// RecordWebhookExecution tracks webhook processing
func (r *postgresTransactionLedgerRepository) RecordWebhookExecution(ctx context.Context, exec *models.WebhookExecution) error {
	if exec.ID == uuid.Nil {
		exec.ID = uuid.New()
	}
	if exec.ProcessedAt.IsZero() {
		exec.ProcessedAt = time.Now()
	}

	payloadJSON, _ := json.Marshal(exec.RawPayload)

	query := `
		INSERT INTO webhook_executions (
			id, event_type, provider_id, external_event_id, idempotency_key,
			ledger_id, status, error_message, raw_payload, received_at, processed_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`
	_, err := r.db.ExecContext(ctx, query,
		exec.ID, exec.EventType, exec.ProviderID, exec.ExternalEventID,
		exec.IdempotencyKey, exec.LedgerID, exec.Status, exec.ErrorMessage,
		payloadJSON, exec.ReceivedAt, exec.ProcessedAt,
	)
	if err != nil {
		// Unique violation on (event_type, provider_id, external_event_id):
		// this webhook (or its concurrent re-delivery) was already recorded.
		// Caller treats this as "replay" and responds 200 OK without re-running
		// side effects — see bug C5.
		if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "23505" {
			return ErrDuplicateWebhook
		}
		return err
	}
	return nil
}

// GetWebhookExecution retrieves a webhook record to check if already processed
func (r *postgresTransactionLedgerRepository) GetWebhookExecution(ctx context.Context, eventType, providerID, externalEventID string) (*models.WebhookExecution, error) {
	exec := &models.WebhookExecution{}
	query := `
		SELECT id, event_type, provider_id, external_event_id, idempotency_key,
		       ledger_id, status, error_message, raw_payload, received_at, processed_at
		FROM webhook_executions
		WHERE event_type = $1 AND provider_id = $2 AND external_event_id = $3
		LIMIT 1
	`
	var payloadJSON []byte
	err := r.db.QueryRowContext(ctx, query, eventType, providerID, externalEventID).Scan(
		&exec.ID, &exec.EventType, &exec.ProviderID, &exec.ExternalEventID,
		&exec.IdempotencyKey, &exec.LedgerID, &exec.Status, &exec.ErrorMessage,
		&payloadJSON, &exec.ReceivedAt, &exec.ProcessedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if payloadJSON != nil {
		_ = json.Unmarshal(payloadJSON, &exec.RawPayload)
	}
	return exec, nil
}

// ───────────────────────────────────────────────────────────────────────

// LogAudit records sensitive operations
func (r *postgresTransactionLedgerRepository) LogAudit(ctx context.Context, log *models.AuditLog) error {
	if log.ID == uuid.Nil {
		log.ID = uuid.New()
	}
	if log.CreatedAt.IsZero() {
		log.CreatedAt = time.Now()
	}

	oldValuesJSON, _ := json.Marshal(log.OldValues)
	newValuesJSON, _ := json.Marshal(log.NewValues)

	query := `
		INSERT INTO audit_log (
			id, entity_type, entity_id, action, actor_id, actor_type,
			old_values, new_values, ip_address, user_agent, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`
	_, err := r.db.ExecContext(ctx, query,
		log.ID, log.EntityType, log.EntityID, log.Action,
		log.ActorID, log.ActorType, oldValuesJSON, newValuesJSON,
		log.IPAddress, log.UserAgent, log.CreatedAt,
	)
	return err
}

// ───────────────────────────────────────────────────────────────────────

// RunReconciliation performs daily balance verification
func (r *postgresTransactionLedgerRepository) RunReconciliation(ctx context.Context) (*models.ReconciliationRun, error) {
	reconcRun := &models.ReconciliationRun{
		ID:      uuid.New(),
		RunDate: time.Now(),
		RunAt:   time.Now(),
	}

	// Get all accounts
	query := `SELECT id FROM accounts`
	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var discrepancies []*models.ReconciliationDiscrepancy

	for rows.Next() {
		var accountID uuid.UUID
		if err := rows.Scan(&accountID); err != nil {
			continue
		}

		// Get stored balance
		var storedBalance int64
		err := r.db.QueryRowContext(ctx, `SELECT balance_cents FROM accounts WHERE id = $1`, accountID).Scan(&storedBalance)
		if err != nil {
			continue
		}

		// Calculate from ledger
		ledgerBalance, _ := r.CalculateBalance(ctx, accountID)

		reconcRun.TotalAccountsChecked++
		reconcRun.TotalBalanceAmountCents += storedBalance
		reconcRun.TotalLedgerAmountCents += ledgerBalance

		if storedBalance != ledgerBalance {
			reconcRun.AccountsDiscrepancy++
			discrepancies = append(discrepancies, &models.ReconciliationDiscrepancy{
				ID:                    uuid.New(),
				ReconciliationRunID:   reconcRun.ID,
				AccountID:             accountID,
				StoredBalanceCents:    storedBalance,
				LedgerCalculatedCents: ledgerBalance,
				DifferenceCents:       storedBalance - ledgerBalance,
				Severity:              "critical",
				CreatedAt:             time.Now(),
			})
		} else {
			reconcRun.AccountsMatched++
		}
	}

	if len(discrepancies) == 0 {
		reconcRun.Status = "success"
	} else if len(discrepancies) <= reconcRun.TotalAccountsChecked/100 { // <1% discrepancy
		reconcRun.Status = "warning"
	} else {
		reconcRun.Status = "critical"
	}

	// Save reconciliation run
	now := time.Now()
	reconcRun.CompletedAt = &now

	errorJSON, _ := json.Marshal(reconcRun.ErrorDetails)
	insertQuery := `
		INSERT INTO reconciliation_runs (
			id, run_date, total_accounts_checked, accounts_matched, accounts_discrepancy,
			total_ledger_amount_cents, total_balance_amount_cents, status, error_details,
			run_at, completed_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`
	_, err = r.db.ExecContext(ctx, insertQuery,
		reconcRun.ID, reconcRun.RunDate, reconcRun.TotalAccountsChecked,
		reconcRun.AccountsMatched, reconcRun.AccountsDiscrepancy,
		reconcRun.TotalLedgerAmountCents, reconcRun.TotalBalanceAmountCents,
		reconcRun.Status, errorJSON, reconcRun.RunAt, reconcRun.CompletedAt,
	)
	if err != nil {
		return nil, err
	}

	// Save discrepancies
	for _, disc := range discrepancies {
		discQuery := `
			INSERT INTO reconciliation_discrepancies (
				id, reconciliation_run_id, account_id, stored_balance_cents,
				ledger_calculated_cents, difference_cents, reason, severity, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		`
		r.db.ExecContext(ctx, discQuery,
			disc.ID, disc.ReconciliationRunID, disc.AccountID, disc.StoredBalanceCents,
			disc.LedgerCalculatedCents, disc.DifferenceCents, disc.Reason,
			disc.Severity, disc.CreatedAt,
		)
	}

	return reconcRun, nil
}

// GetLastReconciliation retrieves most recent reconciliation run
func (r *postgresTransactionLedgerRepository) GetLastReconciliation(ctx context.Context) (*models.ReconciliationRun, error) {
	reconcRun := &models.ReconciliationRun{}
	query := `
		SELECT id, run_date, total_accounts_checked, accounts_matched, accounts_discrepancy,
		       total_ledger_amount_cents, total_balance_amount_cents, status, error_details,
		       run_at, completed_at
		FROM reconciliation_runs
		ORDER BY run_date DESC
		LIMIT 1
	`
	var errorJSON []byte
	err := r.db.QueryRowContext(ctx, query).Scan(
		&reconcRun.ID, &reconcRun.RunDate, &reconcRun.TotalAccountsChecked,
		&reconcRun.AccountsMatched, &reconcRun.AccountsDiscrepancy,
		&reconcRun.TotalLedgerAmountCents, &reconcRun.TotalBalanceAmountCents,
		&reconcRun.Status, &errorJSON, &reconcRun.RunAt, &reconcRun.CompletedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if errorJSON != nil {
		_ = json.Unmarshal(errorJSON, &reconcRun.ErrorDetails)
	}
	return reconcRun, nil
}
