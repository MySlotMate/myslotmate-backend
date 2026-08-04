package repository

import (
	"context"
	"database/sql"
	"myslotmate-backend/internal/models"

	"github.com/google/uuid"
)

// PaymentRepository provides payment (transaction ledger) data access.
type PaymentRepository interface {
	Create(ctx context.Context, payment *models.Payment) error
	GetByID(ctx context.Context, id uuid.UUID) (*models.Payment, error)
	GetByIdempotencyKey(ctx context.Context, key string) (*models.Payment, error)
	GetByGatewayOrderID(ctx context.Context, orderID string) (*models.Payment, error)
	GetByReferenceAndType(ctx context.Context, referenceID uuid.UUID, paymentType models.PaymentType) (*models.Payment, error)
	Update(ctx context.Context, payment *models.Payment) error
	UpdateStatus(ctx context.Context, id uuid.UUID, status models.PaymentStatus, lastError *string) error
	IncrementRetry(ctx context.Context, id uuid.UUID, lastError string) error
	ListByAccountID(ctx context.Context, accountID uuid.UUID, limit, offset int) ([]*models.Payment, error)
	ListByTypeAndAccount(ctx context.Context, accountID uuid.UUID, paymentType models.PaymentType, limit, offset int) ([]*models.Payment, error)
	// ListStuckPayouts returns payout/withdrawal payments still in 'processing',
	// oldest first — candidates for reconciliation against the provider.
	ListStuckPayouts(ctx context.Context, limit int) ([]*models.Payment, error)
	// SumActivePayoutAmountByAccount returns the total amount_cents across all
	// payout/withdrawal payments for the account whose status is pending,
	// processing, or completed — i.e. money that is in flight or already paid
	// out. Used to compute the host's available-to-withdraw balance.
	SumActivePayoutAmountByAccount(ctx context.Context, accountID uuid.UUID) (int64, error)
	// GetByGatewayRefundID looks up the refund-type payment row for a Razorpay
	// rfnd_xxx id (used by the refund webhook handler).
	GetByGatewayRefundID(ctx context.Context, refundID string) (*models.Payment, error)
	// SumActiveRefundsAgainstPayment returns the total amount_cents of refund-type
	// payment rows linked to the given top-up payment whose status is pending,
	// processing, or completed — used to compute the top-up's remaining source-
	// refund headroom.
	SumActiveRefundsAgainstPayment(ctx context.Context, paymentID uuid.UUID) (int64, error)
	// FindMostRecentRefundableTopup returns the user's newest completed top-up
	// payment that still has at least minHeadroom cents of source-refund
	// headroom on its Razorpay payment id. Used by user-facing "refund to card"
	// when cancelling a booking, to auto-pick the lot to refund against.
	// Returns (nil, nil) if no eligible top-up exists.
	FindMostRecentRefundableTopup(ctx context.Context, accountID uuid.UUID, minHeadroom int64) (*models.Payment, error)
	// WithTx returns a copy of the repository bound to the given transaction.
	WithTx(tx *sql.Tx) PaymentRepository
}

// paymentColumns is the canonical SELECT/INSERT column order. Keep INSERT
// VALUES, SELECT lists, and Scan() calls in lockstep with this.
const paymentColumns = `id, idempotency_key, account_id, type, reference_id, amount_cents, status, retry_count, last_error, payout_method_id, display_reference, gateway_order_id, gateway_payment_id, gateway_refund_id, refund_of_payment_id, created_at, updated_at`

type postgresPaymentRepository struct {
	db DBTX
}

func NewPaymentRepository(db *sql.DB) PaymentRepository {
	return &postgresPaymentRepository{db: db}
}

func (r *postgresPaymentRepository) WithTx(tx *sql.Tx) PaymentRepository {
	return &postgresPaymentRepository{db: tx}
}

func (r *postgresPaymentRepository) Create(ctx context.Context, payment *models.Payment) error {
	query := `INSERT INTO payments (` + paymentColumns + `)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`
	if payment.ID == uuid.Nil {
		payment.ID = uuid.New()
	}
	_, err := r.db.ExecContext(ctx, query,
		payment.ID, payment.IdempotencyKey, payment.AccountID, payment.Type,
		payment.ReferenceID, payment.AmountCents, payment.Status, payment.RetryCount,
		payment.LastError, payment.PayoutMethodID, payment.DisplayReference,
		payment.GatewayOrderID, payment.GatewayPaymentID,
		payment.GatewayRefundID, payment.RefundOfPaymentID,
		payment.CreatedAt, payment.UpdatedAt,
	)
	return err
}

// scanPayment scans a single SELECT-by-paymentColumns row into a Payment.
func scanPayment(scanner interface{ Scan(dest ...interface{}) error }) (*models.Payment, error) {
	p := &models.Payment{}
	err := scanner.Scan(
		&p.ID, &p.IdempotencyKey, &p.AccountID, &p.Type, &p.ReferenceID,
		&p.AmountCents, &p.Status, &p.RetryCount, &p.LastError,
		&p.PayoutMethodID, &p.DisplayReference,
		&p.GatewayOrderID, &p.GatewayPaymentID,
		&p.GatewayRefundID, &p.RefundOfPaymentID,
		&p.CreatedAt, &p.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (r *postgresPaymentRepository) GetByID(ctx context.Context, id uuid.UUID) (*models.Payment, error) {
	p, err := scanPayment(r.db.QueryRowContext(ctx, `SELECT `+paymentColumns+` FROM payments WHERE id = $1`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return p, err
}

func (r *postgresPaymentRepository) GetByIdempotencyKey(ctx context.Context, key string) (*models.Payment, error) {
	p, err := scanPayment(r.db.QueryRowContext(ctx, `SELECT `+paymentColumns+` FROM payments WHERE idempotency_key = $1`, key))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return p, err
}

func (r *postgresPaymentRepository) GetByGatewayOrderID(ctx context.Context, orderID string) (*models.Payment, error) {
	p, err := scanPayment(r.db.QueryRowContext(ctx, `SELECT `+paymentColumns+` FROM payments WHERE gateway_order_id = $1`, orderID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return p, err
}

func (r *postgresPaymentRepository) GetByGatewayRefundID(ctx context.Context, refundID string) (*models.Payment, error) {
	p, err := scanPayment(r.db.QueryRowContext(ctx, `SELECT `+paymentColumns+` FROM payments WHERE gateway_refund_id = $1`, refundID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return p, err
}

// GetByReferenceAndType returns the most recent payment for a given reference
// (e.g. a booking ID) and payment type. Used to locate the booking payment so
// it can be reversed on cancellation. Returns (nil, nil) when none exists.
func (r *postgresPaymentRepository) GetByReferenceAndType(ctx context.Context, referenceID uuid.UUID, paymentType models.PaymentType) (*models.Payment, error) {
	p, err := scanPayment(r.db.QueryRowContext(ctx,
		`SELECT `+paymentColumns+` FROM payments WHERE reference_id = $1 AND type = $2 ORDER BY created_at DESC LIMIT 1`,
		referenceID, paymentType))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return p, err
}

func (r *postgresPaymentRepository) Update(ctx context.Context, payment *models.Payment) error {
	query := `UPDATE payments
		SET status = $1, last_error = $2, gateway_order_id = $3, gateway_payment_id = $4,
		    gateway_refund_id = $5, updated_at = $6
		WHERE id = $7`
	_, err := r.db.ExecContext(ctx, query,
		payment.Status, payment.LastError, payment.GatewayOrderID, payment.GatewayPaymentID,
		payment.GatewayRefundID, payment.UpdatedAt, payment.ID,
	)
	return err
}

func (r *postgresPaymentRepository) UpdateStatus(ctx context.Context, id uuid.UUID, status models.PaymentStatus, lastError *string) error {
	query := `UPDATE payments SET status = $1, last_error = $2 WHERE id = $3`
	_, err := r.db.ExecContext(ctx, query, status, lastError, id)
	return err
}

func (r *postgresPaymentRepository) IncrementRetry(ctx context.Context, id uuid.UUID, lastError string) error {
	query := `UPDATE payments SET retry_count = retry_count + 1, last_error = $1, status = 'failed' WHERE id = $2`
	_, err := r.db.ExecContext(ctx, query, lastError, id)
	return err
}

func (r *postgresPaymentRepository) ListByAccountID(ctx context.Context, accountID uuid.UUID, limit, offset int) ([]*models.Payment, error) {
	query := `SELECT ` + paymentColumns + ` FROM payments WHERE account_id = $1 ORDER BY created_at DESC LIMIT $2 OFFSET $3`
	return r.scanPayments(ctx, query, accountID, limit, offset)
}

func (r *postgresPaymentRepository) ListByTypeAndAccount(ctx context.Context, accountID uuid.UUID, paymentType models.PaymentType, limit, offset int) ([]*models.Payment, error) {
	query := `SELECT ` + paymentColumns + ` FROM payments WHERE account_id = $1 AND type = $2 ORDER BY created_at DESC LIMIT $3 OFFSET $4`
	return r.scanPayments(ctx, query, accountID, paymentType, limit, offset)
}

func (r *postgresPaymentRepository) ListStuckPayouts(ctx context.Context, limit int) ([]*models.Payment, error) {
	query := `SELECT ` + paymentColumns + ` FROM payments
		WHERE type IN ('payout', 'withdrawal') AND status = 'processing'
		ORDER BY created_at ASC LIMIT $1`
	return r.scanPayments(ctx, query, limit)
}

// SumActiveRefundsAgainstPayment returns the total amount_cents across refund-
// type payment rows linked to the given top-up payment whose status is pending,
// processing, or completed. Used to compute the remaining source-refund
// headroom against a Razorpay payment id (you cannot refund more than the
// original payment minus what's already been refunded).
func (r *postgresPaymentRepository) SumActiveRefundsAgainstPayment(ctx context.Context, paymentID uuid.UUID) (int64, error) {
	var sum int64
	query := `SELECT COALESCE(SUM(amount_cents), 0) FROM payments
		WHERE refund_of_payment_id = $1
		  AND type = 'refund'
		  AND status IN ('pending', 'processing', 'completed')`
	err := r.db.QueryRowContext(ctx, query, paymentID).Scan(&sum)
	return sum, err
}

// FindMostRecentRefundableTopup picks the newest completed top-up with at least
// minHeadroom cents of remaining source-refund headroom. LIFO lot selection
// (newest first) — newer payments are within Razorpay's refund window and
// have the most refundable headroom. See skill flows.md "lot tracking".
func (r *postgresPaymentRepository) FindMostRecentRefundableTopup(ctx context.Context, accountID uuid.UUID, minHeadroom int64) (*models.Payment, error) {
	query := `SELECT ` + paymentColumns + ` FROM payments p
		WHERE p.account_id = $1
		  AND p.type = 'topup'
		  AND p.status = 'completed'
		  AND p.gateway_payment_id IS NOT NULL
		  AND (p.amount_cents - COALESCE(
		      (SELECT SUM(r.amount_cents) FROM payments r
		         WHERE r.refund_of_payment_id = p.id
		           AND r.type = 'refund'
		           AND r.status IN ('pending', 'processing', 'completed')),
		      0)) >= $2
		ORDER BY p.created_at DESC
		LIMIT 1`
	p, err := scanPayment(r.db.QueryRowContext(ctx, query, accountID, minHeadroom))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return p, err
}

func (r *postgresPaymentRepository) SumActivePayoutAmountByAccount(ctx context.Context, accountID uuid.UUID) (int64, error) {
	var sum int64
	query := `SELECT COALESCE(SUM(amount_cents), 0) FROM payments
		WHERE account_id = $1
		  AND type IN ('payout', 'withdrawal')
		  AND status IN ('pending', 'processing', 'completed')`
	err := r.db.QueryRowContext(ctx, query, accountID).Scan(&sum)
	return sum, err
}

func (r *postgresPaymentRepository) scanPayments(ctx context.Context, query string, args ...interface{}) ([]*models.Payment, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var payments []*models.Payment
	for rows.Next() {
		p, err := scanPayment(rows)
		if err != nil {
			return nil, err
		}
		payments = append(payments, p)
	}
	return payments, nil
}
