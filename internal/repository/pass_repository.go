package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"myslotmate-backend/internal/models"

	"github.com/google/uuid"
)

// ErrPassUnusable is returned by ConsumeSession when the pass can no longer
// cover a session — expired, refunded, or its included sessions are spent.
var ErrPassUnusable = errors.New("pass can no longer be used for this session")

// PassRepository provides monthly-pass data access.
type PassRepository interface {
	Create(ctx context.Context, p *models.UserPass) error
	GetByID(ctx context.Context, id uuid.UUID) (*models.UserPass, error)
	// GetActiveForUserEvent returns the guest's live pass for this event, or nil.
	GetActiveForUserEvent(ctx context.Context, eventID, userID uuid.UUID) (*models.UserPass, error)
	ListByUserID(ctx context.Context, userID uuid.UUID) ([]*models.UserPass, error)
	// ListHolders is the host's roster of everyone holding a pass on this event.
	ListHolders(ctx context.Context, eventID uuid.UUID) ([]*models.PassHolder, error)
	// CountActive counts live passes on an event, for the sold-out check.
	CountActive(ctx context.Context, eventID uuid.UUID) (int, error)
	// ConsumeSession books one session against the pass. The guards live in the
	// UPDATE itself, so two concurrent reservations cannot both take the last
	// included session (same pattern as CouponRepository.Redeem). Returns
	// ErrPassUnusable when the pass no longer qualifies.
	ConsumeSession(ctx context.Context, passID uuid.UUID, occurrence time.Time) error
	// ReleaseSession hands a session back when a reserved date is cancelled.
	ReleaseSession(ctx context.Context, passID uuid.UUID) error
	UpdateStatus(ctx context.Context, passID uuid.UUID, status models.PassStatus) error
	UpdatePaymentID(ctx context.Context, passID, paymentID uuid.UUID) error
	// ExpireLapsed flips the guest's out-of-window passes for this event to
	// 'expired' so the one-active-pass index does not block a repurchase.
	ExpireLapsed(ctx context.Context, eventID, userID uuid.UUID) error
	WithTx(tx *sql.Tx) PassRepository
}

type postgresPassRepository struct {
	db DBTX
}

func NewPassRepository(db *sql.DB) PassRepository {
	return &postgresPassRepository{db: db}
}

func (r *postgresPassRepository) WithTx(tx *sql.Tx) PassRepository {
	return &postgresPassRepository{db: tx}
}

const passColumns = `id, event_id, user_id, price_cents, service_fee_cents, net_earning_cents,
	sessions_included, sessions_used, valid_from, valid_until, status, payment_id,
	idempotency_key, created_at, updated_at`

func scanPass(row interface {
	Scan(dest ...interface{}) error
}) (*models.UserPass, error) {
	p := &models.UserPass{}
	err := row.Scan(
		&p.ID, &p.EventID, &p.UserID, &p.PriceCents, &p.ServiceFeeCents, &p.NetEarningCents,
		&p.SessionsIncluded, &p.SessionsUsed, &p.ValidFrom, &p.ValidUntil, &p.Status, &p.PaymentID,
		&p.IdempotencyKey, &p.CreatedAt, &p.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return p, nil
}

func (r *postgresPassRepository) Create(ctx context.Context, p *models.UserPass) error {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	if p.Status == "" {
		p.Status = models.PassActive
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO user_passes (
			id, event_id, user_id, price_cents, service_fee_cents, net_earning_cents,
			sessions_included, valid_from, valid_until, status, idempotency_key
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		p.ID, p.EventID, p.UserID, p.PriceCents, p.ServiceFeeCents, p.NetEarningCents,
		p.SessionsIncluded, p.ValidFrom, p.ValidUntil, p.Status, p.IdempotencyKey,
	)
	return err
}

func (r *postgresPassRepository) GetByID(ctx context.Context, id uuid.UUID) (*models.UserPass, error) {
	return scanPass(r.db.QueryRowContext(ctx,
		`SELECT `+passColumns+` FROM user_passes WHERE id = $1`, id))
}

func (r *postgresPassRepository) GetActiveForUserEvent(ctx context.Context, eventID, userID uuid.UUID) (*models.UserPass, error) {
	return scanPass(r.db.QueryRowContext(ctx,
		`SELECT `+passColumns+` FROM user_passes
		 WHERE event_id = $1 AND user_id = $2 AND status = 'active'`, eventID, userID))
}

func (r *postgresPassRepository) ListByUserID(ctx context.Context, userID uuid.UUID) ([]*models.UserPass, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+passColumns+` FROM user_passes WHERE user_id = $1 ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	passes := []*models.UserPass{}
	for rows.Next() {
		p, err := scanPass(rows)
		if err != nil {
			return nil, err
		}
		passes = append(passes, p)
	}
	return passes, rows.Err()
}

func (r *postgresPassRepository) ListHolders(ctx context.Context, eventID uuid.UUID) ([]*models.PassHolder, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT p.id, p.event_id, p.user_id, p.price_cents, p.service_fee_cents, p.net_earning_cents,
		       p.sessions_included, p.sessions_used, p.valid_from, p.valid_until, p.status, p.payment_id,
		       p.idempotency_key, p.created_at, p.updated_at,
		       COALESCE(u.name, ''), COALESCE(u.email, ''), COALESCE(u.phn_number, '')
		FROM user_passes p
		JOIN users u ON u.id = p.user_id
		WHERE p.event_id = $1 AND p.status <> 'refunded'
		ORDER BY p.created_at DESC`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	holders := []*models.PassHolder{}
	for rows.Next() {
		h := &models.PassHolder{}
		if err := rows.Scan(
			&h.ID, &h.EventID, &h.UserID, &h.PriceCents, &h.ServiceFeeCents, &h.NetEarningCents,
			&h.SessionsIncluded, &h.SessionsUsed, &h.ValidFrom, &h.ValidUntil, &h.Status, &h.PaymentID,
			&h.IdempotencyKey, &h.CreatedAt, &h.UpdatedAt,
			&h.UserName, &h.UserEmail, &h.UserPhone,
		); err != nil {
			return nil, err
		}
		holders = append(holders, h)
	}
	return holders, rows.Err()
}

func (r *postgresPassRepository) CountActive(ctx context.Context, eventID uuid.UUID) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM user_passes WHERE event_id = $1 AND status = 'active' AND valid_until > now()`,
		eventID).Scan(&n)
	return n, err
}

func (r *postgresPassRepository) ConsumeSession(ctx context.Context, passID uuid.UUID, occurrence time.Time) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE user_passes
		SET sessions_used = sessions_used + 1, updated_at = now()
		WHERE id = $1
		  AND status = 'active'
		  AND $2 BETWEEN valid_from AND valid_until
		  AND (sessions_included IS NULL OR sessions_used < sessions_included)`,
		passID, occurrence)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrPassUnusable
	}
	return nil
}

func (r *postgresPassRepository) ReleaseSession(ctx context.Context, passID uuid.UUID) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE user_passes
		SET sessions_used = GREATEST(sessions_used - 1, 0), updated_at = now()
		WHERE id = $1`, passID)
	return err
}

func (r *postgresPassRepository) UpdateStatus(ctx context.Context, passID uuid.UUID, status models.PassStatus) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE user_passes SET status = $2, updated_at = now() WHERE id = $1`, passID, status)
	return err
}

func (r *postgresPassRepository) UpdatePaymentID(ctx context.Context, passID, paymentID uuid.UUID) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE user_passes SET payment_id = $2, updated_at = now() WHERE id = $1`, passID, paymentID)
	return err
}

func (r *postgresPassRepository) ExpireLapsed(ctx context.Context, eventID, userID uuid.UUID) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE user_passes SET status = 'expired', updated_at = now()
		WHERE event_id = $1 AND user_id = $2 AND status = 'active' AND valid_until <= now()`,
		eventID, userID)
	return err
}
