package repository

import (
	"context"
	"database/sql"
	"myslotmate-backend/internal/models"
	"time"

	"github.com/google/uuid"
)

// UserRepository defines the interface for user data access (Abstract Factory / Repository Pattern)
// This interface allows swapping implementations (e.g., PostgreSQL, Mock for testing)
type UserRepository interface {
	Create(ctx context.Context, user *models.User) error
	GetByEmail(ctx context.Context, email string) (*models.User, error)
	GetByID(ctx context.Context, id uuid.UUID) (*models.User, error)
	ExistsByEmail(ctx context.Context, email string) (bool, error)
	SetVerified(ctx context.Context, id uuid.UUID) error
	Update(ctx context.Context, user *models.User) error
	GetByAuthUID(ctx context.Context, authUID string) (*models.User, error)
	UpdateOTP(ctx context.Context, id uuid.UUID, otp string, expiresAt time.Time) error
	SetPhoneVerified(ctx context.Context, id uuid.UUID) error
}

// postgresUserRepository is the concrete implementation
type postgresUserRepository struct {
	db *sql.DB
}

// NewUserRepository Factory for creating a UserRepository
func NewUserRepository(db *sql.DB) UserRepository {
	return &postgresUserRepository{db: db}
}

func (r *postgresUserRepository) SetVerified(ctx context.Context, id uuid.UUID) error {
	query := `UPDATE users SET is_verified = true, verified_at = NOW(), updated_at = NOW() WHERE id = $1`
	_, err := r.db.ExecContext(ctx, query, id)
	return err
}

func (r *postgresUserRepository) UpdateOTP(ctx context.Context, id uuid.UUID, otp string, expiresAt time.Time) error {
	query := `UPDATE users SET otp = $2, otp_expires_at = $3, updated_at = NOW() WHERE id = $1`
	_, err := r.db.ExecContext(ctx, query, id, otp, expiresAt)
	return err
}

func (r *postgresUserRepository) SetPhoneVerified(ctx context.Context, id uuid.UUID) error {
	query := `UPDATE users SET is_phone_verified = true, updated_at = NOW() WHERE id = $1`
	_, err := r.db.ExecContext(ctx, query, id)
	return err
}

func (r *postgresUserRepository) Create(ctx context.Context, user *models.User) error {
	query := `
		INSERT INTO users (id, auth_uid, name, email, phn_number, avatar_url, city, is_verified, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`
	if user.ID == uuid.Nil {
		user.ID = uuid.New()
	}

	_, err := r.db.ExecContext(ctx, query,
		user.ID,
		user.AuthUID,
		user.Name,
		user.Email,
		user.PhnNumber,
		user.AvatarURL,
		user.City,
		user.IsVerified,
		user.CreatedAt,
		user.UpdatedAt,
	)
	return err
}

func (r *postgresUserRepository) Update(ctx context.Context, user *models.User) error {
	query := `
		UPDATE users SET name = $2, avatar_url = $3, city = $4, updated_at = $5
		WHERE id = $1
	`
	_, err := r.db.ExecContext(ctx, query,
		user.ID,
		user.Name,
		user.AvatarURL,
		user.City,
		user.UpdatedAt,
	)
	return err
}

const userColumns = `id, auth_uid, name, email, phn_number, avatar_url, city, is_verified, is_phone_verified, otp, otp_expires_at, verified_at, created_at, updated_at`

func scanUser(row interface {
	Scan(dest ...interface{}) error
}) (*models.User, error) {
	user := &models.User{}
	err := row.Scan(
		&user.ID,
		&user.AuthUID,
		&user.Name,
		&user.Email,
		&user.PhnNumber,
		&user.AvatarURL,
		&user.City,
		&user.IsVerified,
		&user.IsPhoneVerified,
		&user.OTP,
		&user.OTPExpiresAt,
		&user.VerifiedAt,
		&user.CreatedAt,
		&user.UpdatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return user, nil
}

func (r *postgresUserRepository) GetByEmail(ctx context.Context, email string) (*models.User, error) {
	query := `SELECT ` + userColumns + ` FROM users WHERE email = $1`
	return scanUser(r.db.QueryRowContext(ctx, query, email))
}

func (r *postgresUserRepository) GetByID(ctx context.Context, id uuid.UUID) (*models.User, error) {
	query := `SELECT ` + userColumns + ` FROM users WHERE id = $1`
	return scanUser(r.db.QueryRowContext(ctx, query, id))
}

func (r *postgresUserRepository) GetByAuthUID(ctx context.Context, authUID string) (*models.User, error) {
	query := `SELECT ` + userColumns + ` FROM users WHERE auth_uid = $1`
	return scanUser(r.db.QueryRowContext(ctx, query, authUID))
}

func (r *postgresUserRepository) ExistsByEmail(ctx context.Context, email string) (bool, error) {
	var exists bool
	query := `SELECT EXISTS(SELECT 1 FROM users WHERE email = $1)`
	err := r.db.QueryRowContext(ctx, query, email).Scan(&exists)
	return exists, err
}
