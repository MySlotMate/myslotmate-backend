package repository

import (
	"context"
	"database/sql"
	"errors"

	"myslotmate-backend/internal/models"

	"github.com/google/uuid"
)

// Co-host invitations to an email with no host profile yet (migration
// 20261125160000). They wait in event_cohost_email_invites and become normal
// pending event_co_hosts rows once a host with that email exists.

// ErrEmailInviteOpen means the event already has an open invite for that email.
var ErrEmailInviteOpen = errors.New("email invite already open")

// CoHostEmailInvite is one waiting invitation.
type CoHostEmailInvite struct {
	ID              uuid.UUID
	EventID         uuid.UUID
	Email           string
	InvitedByHostID uuid.UUID
	CanWithdraw     bool
	Status          string // pending | joined | revoked
}

// AttachedInvite is an email invite that just became an event_co_hosts row.
type AttachedInvite struct {
	CoHostID    uuid.UUID
	EventID     uuid.UUID
	EventTitle  string
	OwnerHostID uuid.UUID
	OwnerName   string
}

func (r *postgresCoHostRepository) CreateEmailInvite(ctx context.Context, inv *CoHostEmailInvite) error {
	if inv.ID == uuid.Nil {
		inv.ID = uuid.New()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO event_cohost_email_invites (id, event_id, email, invited_by_host_id, can_withdraw)
		VALUES ($1, $2, $3, $4, $5)`, inv.ID, inv.EventID, inv.Email, inv.InvitedByHostID, inv.CanWithdraw)
	if isUniqueViolation(err) {
		return ErrEmailInviteOpen
	}
	inv.Status = "pending"
	return err
}

func (r *postgresCoHostRepository) GetEmailInvite(ctx context.Context, id uuid.UUID) (*CoHostEmailInvite, error) {
	inv := &CoHostEmailInvite{}
	err := r.db.QueryRowContext(ctx, `
		SELECT id, event_id, email, invited_by_host_id, can_withdraw, status
		  FROM event_cohost_email_invites WHERE id = $1`, id).
		Scan(&inv.ID, &inv.EventID, &inv.Email, &inv.InvitedByHostID, &inv.CanWithdraw, &inv.Status)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return inv, err
}

// ListEmailInvites is the event's open email invites, as roster rows with
// status "awaiting_signup" and no host.
func (r *postgresCoHostRepository) ListEmailInvites(ctx context.Context, eventID uuid.UUID) ([]*CoHostRow, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, event_id, invited_by_host_id, can_withdraw, created_at, updated_at, email
		  FROM event_cohost_email_invites
		 WHERE event_id = $1 AND status = 'pending'
		 ORDER BY created_at DESC`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*CoHostRow
	for rows.Next() {
		cr := &CoHostRow{}
		if err := rows.Scan(&cr.ID, &cr.EventID, &cr.InvitedByHostID, &cr.CanWithdraw, &cr.CreatedAt, &cr.UpdatedAt, &cr.HostEmail); err != nil {
			return nil, err
		}
		cr.Status = models.CoHostAwaitingSignup
		out = append(out, cr)
	}
	return out, rows.Err()
}

func (r *postgresCoHostRepository) SetEmailInviteStatus(ctx context.Context, id uuid.UUID, status string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE event_cohost_email_invites SET status = $2, updated_at = now() WHERE id = $1 AND status = 'pending'`, id, status)
	return err
}

func (r *postgresCoHostRepository) SetEmailInviteCanWithdraw(ctx context.Context, id uuid.UUID, canWithdraw bool) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE event_cohost_email_invites SET can_withdraw = $2, updated_at = now() WHERE id = $1`, id, canWithdraw)
	return err
}

// AttachEmailInvites turns the open email invites for this host's sign-in
// email into pending event_co_hosts rows, in one statement, and marks them
// joined. An invite from the host to themselves, or for an event they already
// co-host, is closed without a row. Returns what was attached.
func (r *postgresCoHostRepository) AttachEmailInvites(ctx context.Context, hostID uuid.UUID) ([]AttachedInvite, error) {
	rows, err := r.db.QueryContext(ctx, `
		WITH me AS (
		    SELECT h.id AS host_id, lower(btrim(u.email)) AS email
		      FROM hosts h JOIN users u ON u.id = h.user_id
		     WHERE h.id = $1 AND COALESCE(btrim(u.email), '') <> ''
		), open AS (
		    SELECT i.* FROM event_cohost_email_invites i JOIN me ON me.email = i.email
		     WHERE i.status = 'pending'
		     FOR UPDATE OF i
		), made AS (
		    INSERT INTO event_co_hosts (event_id, host_id, invited_by_host_id, status, can_withdraw)
		    SELECT o.event_id, me.host_id, o.invited_by_host_id, 'pending', o.can_withdraw
		      FROM open o CROSS JOIN me
		     WHERE o.invited_by_host_id <> me.host_id
		    ON CONFLICT DO NOTHING
		    RETURNING id, event_id
		), closed AS (
		    UPDATE event_cohost_email_invites i
		       SET status = 'joined', co_host_id = made.id, updated_at = now()
		      FROM open o LEFT JOIN made ON made.event_id = o.event_id
		     WHERE i.id = o.id
		    RETURNING i.id
		)
		SELECT made.id, made.event_id, e.title, e.host_id,
		       TRIM(COALESCE(oh.first_name, '') || ' ' || COALESCE(oh.last_name, ''))
		  FROM made JOIN events e ON e.id = made.event_id JOIN hosts oh ON oh.id = e.host_id`, hostID)
	// (PostgreSQL always runs the data-modifying "closed" step, read or not.)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AttachedInvite
	for rows.Next() {
		var a AttachedInvite
		if err := rows.Scan(&a.CoHostID, &a.EventID, &a.EventTitle, &a.OwnerHostID, &a.OwnerName); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Notify adds a line to the user's in-app inbox (the notifications table the
// app's bell reads; Node's pushToUsers writes the same rows).
func (r *postgresCoHostRepository) Notify(ctx context.Context, userID uuid.UUID, title, body string, url *string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO notifications (user_id, title, body, url) VALUES ($1, $2, $3, $4)`, userID, title, body, url)
	return err
}
