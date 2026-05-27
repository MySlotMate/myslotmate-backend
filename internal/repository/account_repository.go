package repository

import (
	"context"
	"database/sql"
	"myslotmate-backend/internal/models"

	"github.com/google/uuid"
)

// AccountRepository provides wallet (account) data access.
type AccountRepository interface {
	Create(ctx context.Context, account *models.Account) error
	GetByID(ctx context.Context, id uuid.UUID) (*models.Account, error)
	GetByOwner(ctx context.Context, ownerType models.AccountOwnerType, ownerID uuid.UUID) (*models.Account, error)
	Credit(ctx context.Context, accountID uuid.UUID, amountCents int64) error
	Debit(ctx context.Context, accountID uuid.UUID, amountCents int64) error
	GetBalance(ctx context.Context, accountID uuid.UUID) (int64, error)
	// WithTx returns a copy of the repository bound to the given transaction.
	WithTx(tx *sql.Tx) AccountRepository
}

type postgresAccountRepository struct {
	db DBTX
}

func NewAccountRepository(db *sql.DB) AccountRepository {
	return &postgresAccountRepository{db: db}
}

func (r *postgresAccountRepository) WithTx(tx *sql.Tx) AccountRepository {
	return &postgresAccountRepository{db: tx}
}

func (r *postgresAccountRepository) GetByID(ctx context.Context, id uuid.UUID) (*models.Account, error) {
	a := &models.Account{}
	query := `SELECT id, owner_type, owner_id, balance_cents, bank_details, created_at, updated_at FROM accounts WHERE id = $1`
	err := r.db.QueryRowContext(ctx, query, id).Scan(
		&a.ID, &a.OwnerType, &a.OwnerID, &a.BalanceCents, &a.BankDetails, &a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return a, nil
}

func (r *postgresAccountRepository) GetByOwner(ctx context.Context, ownerType models.AccountOwnerType, ownerID uuid.UUID) (*models.Account, error) {
	a := &models.Account{}
	query := `SELECT id, owner_type, owner_id, balance_cents, bank_details, created_at, updated_at FROM accounts WHERE owner_type = $1 AND owner_id = $2`
	err := r.db.QueryRowContext(ctx, query, ownerType, ownerID).Scan(
		&a.ID, &a.OwnerType, &a.OwnerID, &a.BalanceCents, &a.BankDetails, &a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return a, nil
}

// Credit atomically adds amount to the account balance.
func (r *postgresAccountRepository) Credit(ctx context.Context, accountID uuid.UUID, amountCents int64) error {
	query := `UPDATE accounts SET balance_cents = balance_cents + $1 WHERE id = $2`
	result, err := r.db.ExecContext(ctx, query, amountCents, accountID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// Debit atomically subtracts amount from the account balance.
// The CHECK (balance_cents >= 0) constraint in the DB will reject if insufficient.
func (r *postgresAccountRepository) Debit(ctx context.Context, accountID uuid.UUID, amountCents int64) error {
	query := `UPDATE accounts SET balance_cents = balance_cents - $1 WHERE id = $2 AND balance_cents >= $1`
	result, err := r.db.ExecContext(ctx, query, amountCents, accountID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrInsufficientBalance
	}
	return nil
}

func (r *postgresAccountRepository) GetBalance(ctx context.Context, accountID uuid.UUID) (int64, error) {
	var balance int64
	query := `SELECT balance_cents FROM accounts WHERE id = $1`
	err := r.db.QueryRowContext(ctx, query, accountID).Scan(&balance)
	return balance, err
}

// Create inserts a new account into the database.
func (r *postgresAccountRepository) Create(ctx context.Context, account *models.Account) error {
	query := `INSERT INTO accounts (id, owner_type, owner_id, balance_cents, bank_details, created_at, updated_at) 
	         VALUES ($1, $2, $3, $4, $5, $6, $7)`
	_, err := r.db.ExecContext(ctx, query,
		account.ID,
		account.OwnerType,
		account.OwnerID,
		account.BalanceCents,
		account.BankDetails,
		account.CreatedAt,
		account.UpdatedAt,
	)
	return err
}
