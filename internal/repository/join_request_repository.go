package repository

import (
	"context"
	"database/sql"
	"time"

	"myslotmate-backend/internal/models"

	"github.com/google/uuid"
)

// JoinRequestRepository stores RSVP requests against private events.
//
// Approval is per event, so every lookup here keys on (event_id, user_id) — a
// guest holds at most one live request per event, enforced by a partial unique
// index over the pending/approved statuses.
type JoinRequestRepository interface {
	Create(ctx context.Context, req *models.EventJoinRequest) error
	GetByID(ctx context.Context, id uuid.UUID) (*models.EventJoinRequest, error)
	// GetLiveForUser returns the guest's pending or approved request for this
	// event, or nil. This is what the booking gate reads.
	GetLiveForUser(ctx context.Context, eventID, userID uuid.UUID) (*models.EventJoinRequest, error)
	// GetLatestForUser returns the guest's most recent request whatever its
	// status, so the public page can show "rejected" rather than "not asked".
	GetLatestForUser(ctx context.Context, eventID, userID uuid.UUID) (*models.EventJoinRequest, error)
	ListByEvent(ctx context.Context, eventID uuid.UUID, status string) ([]*models.EventJoinRequest, error)
	// ListByHost spans every event the host owns — the dashboard queue.
	ListByHost(ctx context.Context, hostID uuid.UUID, status string, limit, offset int) ([]*models.EventJoinRequest, error)
	// ListAll spans every host — the admin queue.
	ListAll(ctx context.Context, status string, limit, offset int) ([]*models.EventJoinRequest, error)
	CountPendingByHost(ctx context.Context, hostID uuid.UUID) (int, error)
	Review(ctx context.Context, id uuid.UUID, status models.JoinRequestStatus,
		kind models.ReviewerKind, reviewerID *uuid.UUID, reviewerLabel *string, note *string) error
	// Withdraw lets the guest retract their own pending request.
	Withdraw(ctx context.Context, id, userID uuid.UUID) error
}

type postgresJoinRequestRepository struct {
	db DBTX
}

func NewJoinRequestRepository(db *sql.DB) JoinRequestRepository {
	return &postgresJoinRequestRepository{db: db}
}

const joinRequestColumns = `r.id, r.event_id, r.user_id, r.status, r.message,
	r.answers_snapshot, r.reviewed_by_kind, r.reviewed_by_id, r.reviewed_by_label,
	r.reviewed_at, r.review_note, r.created_at, r.updated_at`

// joinRequestWithContext adds the guest and event details the review screens
// need, so a queue renders from one query instead of N follow-ups.
const joinRequestSelect = `
	SELECT ` + joinRequestColumns + `,
		u.name, u.email, u.phn_number, u.avatar_url,
		e.title, e.slug
	FROM event_join_requests r
	JOIN users  u ON u.id = r.user_id
	JOIN events e ON e.id = r.event_id`

func scanJoinRequest(row interface{ Scan(dest ...any) error }) (*models.EventJoinRequest, error) {
	jr := &models.EventJoinRequest{}
	err := row.Scan(
		&jr.ID, &jr.EventID, &jr.UserID, &jr.Status, &jr.Message,
		&jr.AnswersSnapshot, &jr.ReviewedByKind, &jr.ReviewedByID, &jr.ReviewedByLabel,
		&jr.ReviewedAt, &jr.ReviewNote, &jr.CreatedAt, &jr.UpdatedAt,
		&jr.UserName, &jr.UserEmail, &jr.UserPhone, &jr.UserAvatarURL,
		&jr.EventTitle, &jr.EventSlug,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return jr, nil
}

func scanJoinRequests(rows *sql.Rows) ([]*models.EventJoinRequest, error) {
	defer rows.Close()
	out := []*models.EventJoinRequest{}
	for rows.Next() {
		jr, err := scanJoinRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, jr)
	}
	return out, rows.Err()
}

func (r *postgresJoinRequestRepository) Create(ctx context.Context, req *models.EventJoinRequest) error {
	if req.ID == uuid.Nil {
		req.ID = uuid.New()
	}
	if req.Status == "" {
		req.Status = models.JoinRequestPending
	}
	const query = `
		INSERT INTO event_join_requests (id, event_id, user_id, status, message, answers_snapshot)
		VALUES ($1, $2, $3, $4, $5, $6)`
	_, err := r.db.ExecContext(ctx, query,
		req.ID, req.EventID, req.UserID, req.Status, req.Message, req.AnswersSnapshot)
	return err
}

func (r *postgresJoinRequestRepository) GetByID(ctx context.Context, id uuid.UUID) (*models.EventJoinRequest, error) {
	return scanJoinRequest(r.db.QueryRowContext(ctx, joinRequestSelect+` WHERE r.id = $1`, id))
}

func (r *postgresJoinRequestRepository) GetLiveForUser(ctx context.Context, eventID, userID uuid.UUID) (*models.EventJoinRequest, error) {
	const where = ` WHERE r.event_id = $1 AND r.user_id = $2
		AND r.status IN ('pending', 'approved')`
	return scanJoinRequest(r.db.QueryRowContext(ctx, joinRequestSelect+where, eventID, userID))
}

func (r *postgresJoinRequestRepository) GetLatestForUser(ctx context.Context, eventID, userID uuid.UUID) (*models.EventJoinRequest, error) {
	const where = ` WHERE r.event_id = $1 AND r.user_id = $2
		ORDER BY r.created_at DESC LIMIT 1`
	return scanJoinRequest(r.db.QueryRowContext(ctx, joinRequestSelect+where, eventID, userID))
}

func (r *postgresJoinRequestRepository) ListByEvent(ctx context.Context, eventID uuid.UUID, status string) ([]*models.EventJoinRequest, error) {
	query := joinRequestSelect + ` WHERE r.event_id = $1
		AND ($2 = '' OR r.status = $2)
		ORDER BY r.created_at DESC`
	rows, err := r.db.QueryContext(ctx, query, eventID, status)
	if err != nil {
		return nil, err
	}
	return scanJoinRequests(rows)
}

func (r *postgresJoinRequestRepository) ListByHost(ctx context.Context, hostID uuid.UUID, status string, limit, offset int) ([]*models.EventJoinRequest, error) {
	limit, offset = clampPage(limit, offset)
	query := joinRequestSelect + ` WHERE e.host_id = $1
		AND ($2 = '' OR r.status = $2)
		ORDER BY r.created_at DESC
		LIMIT $3 OFFSET $4`
	rows, err := r.db.QueryContext(ctx, query, hostID, status, limit, offset)
	if err != nil {
		return nil, err
	}
	return scanJoinRequests(rows)
}

func (r *postgresJoinRequestRepository) ListAll(ctx context.Context, status string, limit, offset int) ([]*models.EventJoinRequest, error) {
	limit, offset = clampPage(limit, offset)
	query := joinRequestSelect + ` WHERE ($1 = '' OR r.status = $1)
		ORDER BY r.created_at DESC
		LIMIT $2 OFFSET $3`
	rows, err := r.db.QueryContext(ctx, query, status, limit, offset)
	if err != nil {
		return nil, err
	}
	return scanJoinRequests(rows)
}

func (r *postgresJoinRequestRepository) CountPendingByHost(ctx context.Context, hostID uuid.UUID) (int, error) {
	const query = `
		SELECT COUNT(*) FROM event_join_requests r
		JOIN events e ON e.id = r.event_id
		WHERE e.host_id = $1 AND r.status = 'pending'`
	var n int
	err := r.db.QueryRowContext(ctx, query, hostID).Scan(&n)
	return n, err
}

// Review records a decision. The status guard in the WHERE clause makes this
// idempotent under a double-click and stops two reviewers overwriting each
// other — the second one simply matches no rows.
func (r *postgresJoinRequestRepository) Review(
	ctx context.Context, id uuid.UUID, status models.JoinRequestStatus,
	kind models.ReviewerKind, reviewerID *uuid.UUID, reviewerLabel *string, note *string,
) error {
	const query = `
		UPDATE event_join_requests
		SET status = $1, reviewed_by_kind = $2, reviewed_by_id = $3, reviewed_by_label = $4,
			reviewed_at = $5, review_note = $6, updated_at = now()
		WHERE id = $7 AND status = 'pending'`
	res, err := r.db.ExecContext(ctx, query, status, kind, reviewerID, reviewerLabel, time.Now(), note, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (r *postgresJoinRequestRepository) Withdraw(ctx context.Context, id, userID uuid.UUID) error {
	const query = `
		UPDATE event_join_requests
		SET status = 'withdrawn', updated_at = now()
		WHERE id = $1 AND user_id = $2 AND status = 'pending'`
	res, err := r.db.ExecContext(ctx, query, id, userID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func clampPage(limit, offset int) (int, int) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}
