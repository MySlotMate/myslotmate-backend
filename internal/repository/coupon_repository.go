package repository

import (
	"context"
	"database/sql"

	"myslotmate-backend/internal/models"

	"github.com/google/uuid"
)

// CouponRepository provides comp-coupon data access.
type CouponRepository interface {
	Create(ctx context.Context, c *models.Coupon) error
	Update(ctx context.Context, c *models.Coupon) error
	Delete(ctx context.Context, id, hostID uuid.UUID) error
	GetByID(ctx context.Context, id uuid.UUID) (*models.Coupon, error)
	ListByHost(ctx context.Context, hostID uuid.UUID) ([]*models.Coupon, error)
	// FindForCode resolves a code that applies to an event: it must belong to the
	// event's host and be either event-scoped to this event or host-wide
	// (event_id NULL). Matched case-insensitively. Returns nil when no such code
	// exists — the caller validates active/window/limits for precise messages.
	FindForCode(ctx context.Context, hostID, eventID uuid.UUID, code string) (*models.Coupon, error)
	// CountUserRedemptions counts non-cancelled bookings the user already made
	// with this coupon (enforces per_user_limit).
	CountUserRedemptions(ctx context.Context, couponID, userID uuid.UUID) (int, error)
	// Redeem atomically increments times_redeemed, refusing to exceed
	// max_redemptions. Returns ErrCouponExhausted when the cap is already hit.
	// Run inside the booking transaction so the redemption commits with the booking.
	Redeem(ctx context.Context, couponID uuid.UUID) error
	// WithTx returns a copy of the repository bound to the given transaction.
	WithTx(tx *sql.Tx) CouponRepository
}

// ErrCouponExhausted is returned by Redeem when max_redemptions is reached.
var ErrCouponExhausted = &CouponError{msg: "coupon has reached its redemption limit"}

type CouponError struct{ msg string }

func (e *CouponError) Error() string { return e.msg }

type postgresCouponRepository struct {
	db DBTX
}

func NewCouponRepository(db *sql.DB) CouponRepository {
	return &postgresCouponRepository{db: db}
}

func (r *postgresCouponRepository) WithTx(tx *sql.Tx) CouponRepository {
	return &postgresCouponRepository{db: tx}
}

const couponColumns = `id, host_id, event_id, code, max_redemptions, times_redeemed, per_user_limit, valid_from, valid_until, is_active, created_at, updated_at`

func scanCoupon(row interface {
	Scan(dest ...interface{}) error
}) (*models.Coupon, error) {
	c := &models.Coupon{}
	err := row.Scan(
		&c.ID, &c.HostID, &c.EventID, &c.Code, &c.MaxRedemptions, &c.TimesRedeemed,
		&c.PerUserLimit, &c.ValidFrom, &c.ValidUntil, &c.IsActive, &c.CreatedAt, &c.UpdatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return c, nil
}

func (r *postgresCouponRepository) Create(ctx context.Context, c *models.Coupon) error {
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	query := `
		INSERT INTO coupons (id, host_id, event_id, code, max_redemptions, per_user_limit, valid_from, valid_until, is_active)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`
	_, err := r.db.ExecContext(ctx, query,
		c.ID, c.HostID, c.EventID, c.Code, c.MaxRedemptions, c.PerUserLimit, c.ValidFrom, c.ValidUntil, c.IsActive,
	)
	return err
}

func (r *postgresCouponRepository) Update(ctx context.Context, c *models.Coupon) error {
	// Scoped to host_id so a host can only edit their own coupons.
	query := `
		UPDATE coupons SET
			event_id = $1, code = $2, max_redemptions = $3, per_user_limit = $4,
			valid_from = $5, valid_until = $6, is_active = $7, updated_at = now()
		WHERE id = $8 AND host_id = $9
	`
	_, err := r.db.ExecContext(ctx, query,
		c.EventID, c.Code, c.MaxRedemptions, c.PerUserLimit, c.ValidFrom, c.ValidUntil, c.IsActive, c.ID, c.HostID,
	)
	return err
}

func (r *postgresCouponRepository) Delete(ctx context.Context, id, hostID uuid.UUID) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM coupons WHERE id = $1 AND host_id = $2`, id, hostID)
	return err
}

func (r *postgresCouponRepository) GetByID(ctx context.Context, id uuid.UUID) (*models.Coupon, error) {
	query := `SELECT ` + couponColumns + ` FROM coupons WHERE id = $1`
	return scanCoupon(r.db.QueryRowContext(ctx, query, id))
}

func (r *postgresCouponRepository) ListByHost(ctx context.Context, hostID uuid.UUID) ([]*models.Coupon, error) {
	query := `SELECT ` + couponColumns + ` FROM coupons WHERE host_id = $1 ORDER BY created_at DESC`
	rows, err := r.db.QueryContext(ctx, query, hostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	coupons := []*models.Coupon{}
	for rows.Next() {
		c, err := scanCoupon(rows)
		if err != nil {
			return nil, err
		}
		coupons = append(coupons, c)
	}
	return coupons, rows.Err()
}

func (r *postgresCouponRepository) FindForCode(ctx context.Context, hostID, eventID uuid.UUID, code string) (*models.Coupon, error) {
	query := `SELECT ` + couponColumns + ` FROM coupons
		WHERE host_id = $1 AND lower(code) = lower($2) AND (event_id IS NULL OR event_id = $3)
		LIMIT 1`
	return scanCoupon(r.db.QueryRowContext(ctx, query, hostID, code, eventID))
}

func (r *postgresCouponRepository) CountUserRedemptions(ctx context.Context, couponID, userID uuid.UUID) (int, error) {
	var n int
	query := `SELECT COUNT(*) FROM bookings
		WHERE coupon_id = $1 AND user_id = $2 AND status <> 'cancelled'`
	err := r.db.QueryRowContext(ctx, query, couponID, userID).Scan(&n)
	return n, err
}

func (r *postgresCouponRepository) Redeem(ctx context.Context, couponID uuid.UUID) error {
	// Atomic guard: the increment only fires while under the cap. Zero rows
	// affected means the cap was already reached (or the coupon vanished).
	res, err := r.db.ExecContext(ctx, `
		UPDATE coupons
		SET times_redeemed = times_redeemed + 1, updated_at = now()
		WHERE id = $1 AND (max_redemptions IS NULL OR times_redeemed < max_redemptions)
	`, couponID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrCouponExhausted
	}
	return nil
}
