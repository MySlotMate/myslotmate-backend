package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"myslotmate-backend/internal/models"

	"github.com/google/uuid"
)

// PayoutRepository provides payout method, host earnings, and platform settings data access.
type PayoutRepository interface {
	// Payout Methods (Host)
	CreatePayoutMethod(ctx context.Context, pm *models.PayoutMethod) error
	GetPayoutMethodByID(ctx context.Context, id uuid.UUID) (*models.PayoutMethod, error)
	ListPayoutMethodsByHostID(ctx context.Context, hostID uuid.UUID) ([]*models.PayoutMethod, error)
	GetPrimaryPayoutMethod(ctx context.Context, hostID uuid.UUID) (*models.PayoutMethod, error)
	SetPrimary(ctx context.Context, hostID uuid.UUID, methodID uuid.UUID) error
	DeletePayoutMethod(ctx context.Context, id uuid.UUID) error

	// Payout Methods (Admin/Platform)
	ListAdminPayoutMethods(ctx context.Context) ([]*models.PayoutMethod, error)
	GetAdminPrimaryPayoutMethod(ctx context.Context) (*models.PayoutMethod, error)
	SetAdminPrimary(ctx context.Context, methodID uuid.UUID) error

	// Host Earnings
	GetHostEarnings(ctx context.Context, hostID uuid.UUID) (*models.HostEarnings, error)
	IncrementEarnings(ctx context.Context, hostID uuid.UUID, amountCents int64) error
	DecrementEarnings(ctx context.Context, hostID uuid.UUID, amountCents int64) error
	AddPendingClearance(ctx context.Context, hostID uuid.UUID, amountCents int64) error
	ClearPending(ctx context.Context, hostID uuid.UUID, amountCents int64) error

	// Platform Settings
	GetPlatformFeeConfig(ctx context.Context) (*models.PlatformFeeConfig, error)

	// Fraud Flags
	HasActiveFraudFlag(ctx context.Context, userID uuid.UUID) (bool, error)

	// WithTx returns a copy of the repository bound to the given transaction.
	WithTx(tx *sql.Tx) PayoutRepository
}

type postgresPayoutRepository struct {
	db DBTX
}

func NewPayoutRepository(db *sql.DB) PayoutRepository {
	return &postgresPayoutRepository{db: db}
}

func (r *postgresPayoutRepository) WithTx(tx *sql.Tx) PayoutRepository {
	return &postgresPayoutRepository{db: tx}
}

// ── Payout Methods ──────────────────────────────────────────────────────────

func (r *postgresPayoutRepository) CreatePayoutMethod(ctx context.Context, pm *models.PayoutMethod) error {
	query := `
		INSERT INTO payout_methods (id, host_id, type, bank_name, account_type, last_four_digits, account_number_encrypted, ifsc, beneficiary_name, upi_id, is_verified, is_primary, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
	`
	if pm.ID == uuid.Nil {
		pm.ID = uuid.New()
	}
	_, err := r.db.ExecContext(ctx, query,
		pm.ID, pm.HostID, pm.Type, pm.BankName, pm.AccountType,
		pm.LastFourDigits, pm.AccountNumberEncrypted, pm.IFSC, pm.BeneficiaryName,
		pm.UPIID, pm.IsVerified, pm.IsPrimary, pm.CreatedAt, pm.UpdatedAt,
	)
	return err
}

func (r *postgresPayoutRepository) GetPayoutMethodByID(ctx context.Context, id uuid.UUID) (*models.PayoutMethod, error) {
	pm := &models.PayoutMethod{}
	query := `SELECT id, host_id, type, bank_name, account_type, last_four_digits, account_number_encrypted, ifsc, beneficiary_name, upi_id, is_verified, is_primary, created_at, updated_at
		FROM payout_methods WHERE id = $1`
	err := r.db.QueryRowContext(ctx, query, id).Scan(
		&pm.ID, &pm.HostID, &pm.Type, &pm.BankName, &pm.AccountType,
		&pm.LastFourDigits, &pm.AccountNumberEncrypted, &pm.IFSC, &pm.BeneficiaryName,
		&pm.UPIID, &pm.IsVerified, &pm.IsPrimary, &pm.CreatedAt, &pm.UpdatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return pm, nil
}

func (r *postgresPayoutRepository) ListPayoutMethodsByHostID(ctx context.Context, hostID uuid.UUID) ([]*models.PayoutMethod, error) {
	query := `SELECT id, host_id, type, bank_name, account_type, last_four_digits, account_number_encrypted, ifsc, beneficiary_name, upi_id, is_verified, is_primary, created_at, updated_at
		FROM payout_methods WHERE host_id = $1 ORDER BY is_primary DESC, created_at DESC`
	rows, err := r.db.QueryContext(ctx, query, hostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var methods []*models.PayoutMethod
	for rows.Next() {
		pm := &models.PayoutMethod{}
		if err := rows.Scan(
			&pm.ID, &pm.HostID, &pm.Type, &pm.BankName, &pm.AccountType,
			&pm.LastFourDigits, &pm.AccountNumberEncrypted, &pm.IFSC, &pm.BeneficiaryName,
			&pm.UPIID, &pm.IsVerified, &pm.IsPrimary, &pm.CreatedAt, &pm.UpdatedAt,
		); err != nil {
			return nil, err
		}
		methods = append(methods, pm)
	}
	return methods, nil
}

func (r *postgresPayoutRepository) GetPrimaryPayoutMethod(ctx context.Context, hostID uuid.UUID) (*models.PayoutMethod, error) {
	pm := &models.PayoutMethod{}
	query := `SELECT id, host_id, type, bank_name, account_type, last_four_digits, account_number_encrypted, ifsc, beneficiary_name, upi_id, is_verified, is_primary, created_at, updated_at
		FROM payout_methods WHERE (host_id = $1 OR (host_id IS NULL AND $1 = '00000000-0000-0000-0000-000000000000')) AND is_primary = true LIMIT 1`
	err := r.db.QueryRowContext(ctx, query, hostID).Scan(
		&pm.ID, &pm.HostID, &pm.Type, &pm.BankName, &pm.AccountType,
		&pm.LastFourDigits, &pm.AccountNumberEncrypted, &pm.IFSC, &pm.BeneficiaryName,
		&pm.UPIID, &pm.IsVerified, &pm.IsPrimary, &pm.CreatedAt, &pm.UpdatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return pm, nil
}

func (r *postgresPayoutRepository) SetPrimary(ctx context.Context, hostID uuid.UUID, methodID uuid.UUID) error {
	// Unset all, then set the chosen one. Manages its own transaction, so it
	// cannot run on a tx-bound instance (see WithTx / DBTX).
	sqlDB, ok := r.db.(*sql.DB)
	if !ok {
		return errors.New("SetPrimary requires a direct DB connection, not a transaction")
	}
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	_, err = tx.ExecContext(ctx, `UPDATE payout_methods SET is_primary = false, updated_at = now() WHERE host_id = $1`, hostID)
	if err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx, `UPDATE payout_methods SET is_primary = true, updated_at = now() WHERE id = $1 AND host_id = $2`, methodID, hostID)
	if err != nil {
		return err
	}

	if err = tx.Commit(); err != nil {
		return err
	}
	return nil
}

func (r *postgresPayoutRepository) DeletePayoutMethod(ctx context.Context, id uuid.UUID) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM payout_methods WHERE id = $1`, id)
	return err
}

// ── Admin Payout Methods ────────────────────────────────────────────────────

func (r *postgresPayoutRepository) ListAdminPayoutMethods(ctx context.Context) ([]*models.PayoutMethod, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, host_id, type, bank_name, account_type, last_four_digits, 
		       account_number_encrypted, ifsc, beneficiary_name, upi_id, 
		       is_verified, is_primary, created_at, updated_at
		FROM payout_methods
		WHERE host_id IS NULL
		ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var methods []*models.PayoutMethod
	for rows.Next() {
		pm := &models.PayoutMethod{}
		err := rows.Scan(
			&pm.ID, &pm.HostID, &pm.Type, &pm.BankName, &pm.AccountType,
			&pm.LastFourDigits, &pm.AccountNumberEncrypted, &pm.IFSC,
			&pm.BeneficiaryName, &pm.UPIID, &pm.IsVerified, &pm.IsPrimary,
			&pm.CreatedAt, &pm.UpdatedAt,
		)
		if err != nil {
			return nil, err
		}
		methods = append(methods, pm)
	}
	return methods, rows.Err()
}

func (r *postgresPayoutRepository) GetAdminPrimaryPayoutMethod(ctx context.Context) (*models.PayoutMethod, error) {
	return r.GetPrimaryPayoutMethod(ctx, uuid.Nil)
}

func (r *postgresPayoutRepository) SetAdminPrimary(ctx context.Context, methodID uuid.UUID) error {
	// Unset all admin methods, then set the chosen one. Manages its own
	// transaction, so it cannot run on a tx-bound instance (see WithTx / DBTX).
	sqlDB, ok := r.db.(*sql.DB)
	if !ok {
		return errors.New("SetAdminPrimary requires a direct DB connection, not a transaction")
	}
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	_, err = tx.ExecContext(ctx, `UPDATE payout_methods SET is_primary = false, updated_at = now() WHERE host_id IS NULL`)
	if err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx, `UPDATE payout_methods SET is_primary = true, updated_at = now() WHERE id = $1 AND host_id IS NULL`, methodID)
	if err != nil {
		return err
	}

	if err = tx.Commit(); err != nil {
		return err
	}
	return nil
}

// ── Host Earnings ───────────────────────────────────────────────────────────

func (r *postgresPayoutRepository) GetHostEarnings(ctx context.Context, hostID uuid.UUID) (*models.HostEarnings, error) {
	he := &models.HostEarnings{}
	query := `SELECT id, host_id, total_earnings_cents, pending_clearance_cents, estimated_clearance_at, created_at, updated_at FROM host_earnings WHERE host_id = $1`
	err := r.db.QueryRowContext(ctx, query, hostID).Scan(
		&he.ID, &he.HostID, &he.TotalEarningsCents, &he.PendingClearanceCents,
		&he.EstimatedClearanceAt, &he.CreatedAt, &he.UpdatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return he, nil
}

func (r *postgresPayoutRepository) IncrementEarnings(ctx context.Context, hostID uuid.UUID, amountCents int64) error {
	query := `UPDATE host_earnings SET total_earnings_cents = total_earnings_cents + $1 WHERE host_id = $2`
	_, err := r.db.ExecContext(ctx, query, amountCents, hostID)
	return err
}

// DecrementEarnings reverses a host's lifetime earnings (e.g. on booking
// cancellation). GREATEST(...,0) prevents the total going negative.
func (r *postgresPayoutRepository) DecrementEarnings(ctx context.Context, hostID uuid.UUID, amountCents int64) error {
	query := `UPDATE host_earnings SET total_earnings_cents = GREATEST(total_earnings_cents - $1, 0) WHERE host_id = $2`
	_, err := r.db.ExecContext(ctx, query, amountCents, hostID)
	return err
}

func (r *postgresPayoutRepository) AddPendingClearance(ctx context.Context, hostID uuid.UUID, amountCents int64) error {
	query := `UPDATE host_earnings 
		SET pending_clearance_cents = pending_clearance_cents + $1,
		    estimated_clearance_at = CASE 
		      WHEN estimated_clearance_at IS NULL THEN now()
		      ELSE estimated_clearance_at
		    END
		WHERE host_id = $2`
	_, err := r.db.ExecContext(ctx, query, amountCents, hostID)
	return err
}

func (r *postgresPayoutRepository) ClearPending(ctx context.Context, hostID uuid.UUID, amountCents int64) error {
	query := `UPDATE host_earnings SET pending_clearance_cents = GREATEST(pending_clearance_cents - $1, 0) WHERE host_id = $2`
	_, err := r.db.ExecContext(ctx, query, amountCents, hostID)
	return err
}

// ── Platform Settings ───────────────────────────────────────────────────────

func (r *postgresPayoutRepository) GetPlatformFeeConfig(ctx context.Context) (*models.PlatformFeeConfig, error) {
	var raw []byte
	query := `SELECT value FROM platform_settings WHERE key = 'platform_fee'`
	err := r.db.QueryRowContext(ctx, query).Scan(&raw)
	if err != nil {
		if err == sql.ErrNoRows {
			// Return default 70/30 if not seeded
			return &models.PlatformFeeConfig{HostPercentage: 70, PlatformPercentage: 30}, nil
		}
		return nil, err
	}
	var cfg models.PlatformFeeConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ── Fraud Flags ─────────────────────────────────────────────────────────────

func (r *postgresPayoutRepository) HasActiveFraudFlag(ctx context.Context, userID uuid.UUID) (bool, error) {
	var exists bool
	query := `SELECT EXISTS(SELECT 1 FROM fraud_flags WHERE user_id = $1 AND is_active = true AND (blocked_until IS NULL OR blocked_until > NOW()))`
	err := r.db.QueryRowContext(ctx, query, userID).Scan(&exists)
	return exists, err
}
