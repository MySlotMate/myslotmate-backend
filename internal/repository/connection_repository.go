package repository

import (
	"context"
	"database/sql"
	"time"

	"github.com/google/uuid"
)

// HostConnection is one host the caller is connected with.
type HostConnection struct {
	HostID    uuid.UUID `json:"host_id"`
	CreatedAt time.Time `json:"created_at"`
}

// HostConnector is one person connected with a host (host's own view).
type HostConnector struct {
	UserID    uuid.UUID `json:"user_id"`
	Name      string    `json:"name"`
	AvatarURL *string   `json:"avatar_url"`
	City      *string   `json:"city"`
	CreatedAt time.Time `json:"created_at"`
}

// ConnectionRepository stores one-way user→host connections (the connections table).
type ConnectionRepository interface {
	ListByUser(ctx context.Context, userID uuid.UUID) ([]HostConnection, error)
	// Connect is idempotent; it reports whether a new row was created.
	Connect(ctx context.Context, userID, hostID uuid.UUID) (bool, error)
	Disconnect(ctx context.Context, userID, hostID uuid.UUID) error
	CountForHost(ctx context.Context, hostID uuid.UUID) (int, error)
	// ListForHost pages newest first; before (if set) keeps rows created earlier.
	ListForHost(ctx context.Context, hostID uuid.UUID, before *time.Time, limit int) ([]HostConnector, error)
}

type postgresConnectionRepository struct{ db *sql.DB }

func NewConnectionRepository(db *sql.DB) ConnectionRepository {
	return &postgresConnectionRepository{db: db}
}

func (r *postgresConnectionRepository) ListByUser(ctx context.Context, userID uuid.UUID) ([]HostConnection, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT host_id, created_at FROM connections WHERE user_id = $1 ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HostConnection{}
	for rows.Next() {
		var c HostConnection
		if err := rows.Scan(&c.HostID, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *postgresConnectionRepository) Connect(ctx context.Context, userID, hostID uuid.UUID) (bool, error) {
	res, err := r.db.ExecContext(ctx, `INSERT INTO connections (user_id, host_id) VALUES ($1, $2) ON CONFLICT (user_id, host_id) DO NOTHING`, userID, hostID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (r *postgresConnectionRepository) Disconnect(ctx context.Context, userID, hostID uuid.UUID) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM connections WHERE user_id = $1 AND host_id = $2`, userID, hostID)
	return err
}

func (r *postgresConnectionRepository) CountForHost(ctx context.Context, hostID uuid.UUID) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM connections WHERE host_id = $1`, hostID).Scan(&n)
	return n, err
}

func (r *postgresConnectionRepository) ListForHost(ctx context.Context, hostID uuid.UUID, before *time.Time, limit int) ([]HostConnector, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT u.id, u.name, u.avatar_url, u.city, c.created_at
		FROM connections c JOIN users u ON u.id = c.user_id
		WHERE c.host_id = $1 AND ($2::timestamptz IS NULL OR c.created_at < $2)
		ORDER BY c.created_at DESC LIMIT $3`, hostID, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HostConnector{}
	for rows.Next() {
		var c HostConnector
		if err := rows.Scan(&c.UserID, &c.Name, &c.AvatarURL, &c.City, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
