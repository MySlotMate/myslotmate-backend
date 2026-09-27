package repository

import (
	"context"
	"database/sql"
	"time"

	"myslotmate-backend/internal/models"

	"github.com/google/uuid"
)

// CoHostRepository is data access for event co-hosting: the invitations
// themselves, and the per-event withdrawal claims that stop the same event
// earnings being paid out twice.
type CoHostRepository interface {
	Create(ctx context.Context, ch *models.EventCoHost) error
	GetByID(ctx context.Context, id uuid.UUID) (*models.EventCoHost, error)
	// GetByEventAndHost returns the live (pending or accepted) row for this pair,
	// or (nil, nil) when there is none.
	GetByEventAndHost(ctx context.Context, eventID, hostID uuid.UUID) (*models.EventCoHost, error)
	// ListByEvent is the owner's panel: every co-host of the event, joined with
	// their name and email, newest first.
	ListByEvent(ctx context.Context, eventID uuid.UUID) ([]*CoHostRow, error)
	// ListSharedWithHost is the co-host's view: events other hosts shared with
	// them, with what is left to withdraw on each.
	ListSharedWithHost(ctx context.Context, hostID uuid.UUID) ([]*SharedEventRow, error)
	SetCanWithdraw(ctx context.Context, id uuid.UUID, canWithdraw bool) error
	SetStatus(ctx context.Context, id uuid.UUID, status models.CoHostStatus) error

	// EventPassedEarnings is the event's own withdrawable pool: net earnings on
	// confirmed bookings whose occurrence has already happened. Gross of claims —
	// the netting lives in ReserveClaim, so this stays a plain upper bound.
	EventPassedEarnings(ctx context.Context, eventID uuid.UUID) (int64, error)
	// EventClaimedCents is the total already reserved or paid out against the
	// event by co-hosts (failed claims excluded).
	EventClaimedCents(ctx context.Context, eventID uuid.UUID) (int64, error)
	// ReserveClaim atomically books `amountCents` of the event's passed earnings
	// for this co-host. The affordability check is part of the INSERT, so two
	// concurrent withdrawals cannot both pass it. Returns the claim id, or
	// (uuid.Nil, nil) when the event cannot cover the amount.
	ReserveClaim(ctx context.Context, cohostID, eventID, hostID uuid.UUID, amountCents int64) (uuid.UUID, error)
	// AttachPayment links a reserved claim to the payout payment row it produced.
	AttachPayment(ctx context.Context, claimID, paymentID uuid.UUID) error
	// SetClaimStatus finalizes a claim by id — used when the payout fails
	// synchronously, before any payment row exists to key on.
	SetClaimStatus(ctx context.Context, claimID uuid.UUID, status string) error
	// SetClaimStatusByPayment mirrors the payout's terminal state onto the claim.
	// Called from the payout webhook; a no-op when the payment is not a co-host
	// withdrawal.
	SetClaimStatusByPayment(ctx context.Context, paymentID uuid.UUID, status string) error

	// CountsForHost answers "does co-hosting concern this host at all?" in one
	// query: how many live invitations they received, and how many co-hosts they
	// granted on their own experiences. Used to decide whether the dashboard
	// shows a co-hosting tab.
	CountsForHost(ctx context.Context, hostID uuid.UUID) (received int, granted int, err error)
}

// CoHostRow is one co-host plus who they are, for the owner's manage panel.
type CoHostRow struct {
	models.EventCoHost
	HostName   string  `json:"host_name"`
	HostEmail  string  `json:"host_email"`
	HostAvatar *string `json:"host_avatar_url,omitempty"`
	// ClaimedCents is what this co-host has already reserved or withdrawn from
	// the event — the "already withdrew" signal for the owner's UI.
	ClaimedCents int64 `json:"claimed_cents"`
}

// SharedEventRow is one event shared WITH the caller, plus the withdrawal state
// they need to act on it.
type SharedEventRow struct {
	CoHostID    uuid.UUID           `json:"cohost_id"`
	EventID     uuid.UUID           `json:"event_id"`
	EventTitle  string              `json:"event_title"`
	EventSlug   *string             `json:"event_slug,omitempty"`
	Status      models.CoHostStatus `json:"status"`
	CanWithdraw bool                `json:"can_withdraw"`
	OwnerName   string              `json:"owner_name"`
	// InvitedAt and EventTime let the co-host judge an invitation before
	// accepting: who asked, when, and when the experience actually runs.
	InvitedAt time.Time `json:"invited_at"`
	EventTime time.Time `json:"event_time"`
	// PassedCents is the event's withdrawable pool, ClaimedCents what co-hosts
	// already took, AvailableCents what is left for this co-host to withdraw.
	PassedCents    int64 `json:"passed_cents"`
	ClaimedCents   int64 `json:"claimed_cents"`
	AvailableCents int64 `json:"available_cents"`
	// MyClaimedCents is this caller's own share of ClaimedCents.
	MyClaimedCents int64 `json:"my_claimed_cents"`
}

type postgresCoHostRepository struct {
	db DBTX
}

func NewCoHostRepository(db *sql.DB) CoHostRepository {
	return &postgresCoHostRepository{db: db}
}

const coHostColumns = `id, event_id, host_id, invited_by_host_id, status, can_withdraw, responded_at, created_at, updated_at`

func scanCoHost(row interface{ Scan(...interface{}) error }) (*models.EventCoHost, error) {
	ch := &models.EventCoHost{}
	err := row.Scan(&ch.ID, &ch.EventID, &ch.HostID, &ch.InvitedByHostID,
		&ch.Status, &ch.CanWithdraw, &ch.RespondedAt, &ch.CreatedAt, &ch.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ch, nil
}

func (r *postgresCoHostRepository) Create(ctx context.Context, ch *models.EventCoHost) error {
	if ch.ID == uuid.Nil {
		ch.ID = uuid.New()
	}
	const query = `
		INSERT INTO event_co_hosts (id, event_id, host_id, invited_by_host_id, status, can_withdraw)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING created_at, updated_at`
	return r.db.QueryRowContext(ctx, query,
		ch.ID, ch.EventID, ch.HostID, ch.InvitedByHostID, ch.Status, ch.CanWithdraw,
	).Scan(&ch.CreatedAt, &ch.UpdatedAt)
}

func (r *postgresCoHostRepository) GetByID(ctx context.Context, id uuid.UUID) (*models.EventCoHost, error) {
	return scanCoHost(r.db.QueryRowContext(ctx, `SELECT `+coHostColumns+` FROM event_co_hosts WHERE id = $1`, id))
}

func (r *postgresCoHostRepository) GetByEventAndHost(ctx context.Context, eventID, hostID uuid.UUID) (*models.EventCoHost, error) {
	const query = `SELECT ` + coHostColumns + ` FROM event_co_hosts
		WHERE event_id = $1 AND host_id = $2 AND status IN ('pending', 'accepted')`
	return scanCoHost(r.db.QueryRowContext(ctx, query, eventID, hostID))
}

func (r *postgresCoHostRepository) ListByEvent(ctx context.Context, eventID uuid.UUID) ([]*CoHostRow, error) {
	const query = `
		SELECT ch.id, ch.event_id, ch.host_id, ch.invited_by_host_id, ch.status,
		       ch.can_withdraw, ch.responded_at, ch.created_at, ch.updated_at,
		       TRIM(COALESCE(h.first_name, '') || ' ' || COALESCE(h.last_name, '')),
		       COALESCE(u.email, ''), h.avatar_url,
		       COALESCE((SELECT SUM(c.amount_cents) FROM event_cohost_claims c
		                  WHERE c.event_co_host_id = ch.id AND c.status <> 'failed'), 0)::BIGINT
		FROM event_co_hosts ch
		JOIN hosts h ON h.id = ch.host_id
		JOIN users u ON u.id = h.user_id
		WHERE ch.event_id = $1
		ORDER BY ch.created_at DESC`
	rows, err := r.db.QueryContext(ctx, query, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*CoHostRow
	for rows.Next() {
		cr := &CoHostRow{}
		if err := rows.Scan(&cr.ID, &cr.EventID, &cr.HostID, &cr.InvitedByHostID, &cr.Status,
			&cr.CanWithdraw, &cr.RespondedAt, &cr.CreatedAt, &cr.UpdatedAt,
			&cr.HostName, &cr.HostEmail, &cr.HostAvatar, &cr.ClaimedCents); err != nil {
			return nil, err
		}
		out = append(out, cr)
	}
	return out, rows.Err()
}

func (r *postgresCoHostRepository) ListSharedWithHost(ctx context.Context, hostID uuid.UUID) ([]*SharedEventRow, error) {
	// passed / claimed are computed per event the same way the withdrawal gate
	// computes them, so the card never shows an amount the payout would refuse.
	const query = `
		SELECT ch.id, e.id, e.title, e.slug, ch.status, ch.can_withdraw,
		       TRIM(COALESCE(owner.first_name, '') || ' ' || COALESCE(owner.last_name, '')),
		       ch.created_at, e.time,
		       COALESCE((SELECT SUM(b.net_earning_cents) FROM bookings b
		                  WHERE b.event_id = e.id AND b.status = 'confirmed'
		                    AND b.occurrence_date < NOW()), 0)::BIGINT AS passed,
		       COALESCE((SELECT SUM(c.amount_cents) FROM event_cohost_claims c
		                  WHERE c.event_id = e.id AND c.status <> 'failed'), 0)::BIGINT AS claimed,
		       COALESCE((SELECT SUM(c.amount_cents) FROM event_cohost_claims c
		                  WHERE c.event_co_host_id = ch.id AND c.status <> 'failed'), 0)::BIGINT AS mine
		FROM event_co_hosts ch
		JOIN events e     ON e.id = ch.event_id
		JOIN hosts  owner ON owner.id = e.host_id
		WHERE ch.host_id = $1 AND ` + coHostActive + `
		ORDER BY ch.created_at DESC`
	rows, err := r.db.QueryContext(ctx, query, hostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*SharedEventRow
	for rows.Next() {
		sr := &SharedEventRow{}
		if err := rows.Scan(&sr.CoHostID, &sr.EventID, &sr.EventTitle, &sr.EventSlug,
			&sr.Status, &sr.CanWithdraw, &sr.OwnerName, &sr.InvitedAt, &sr.EventTime,
			&sr.PassedCents, &sr.ClaimedCents, &sr.MyClaimedCents); err != nil {
			return nil, err
		}
		sr.AvailableCents = sr.PassedCents - sr.ClaimedCents
		if sr.AvailableCents < 0 {
			sr.AvailableCents = 0
		}
		out = append(out, sr)
	}
	return out, rows.Err()
}

func (r *postgresCoHostRepository) SetCanWithdraw(ctx context.Context, id uuid.UUID, canWithdraw bool) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE event_co_hosts SET can_withdraw = $2, updated_at = now() WHERE id = $1`, id, canWithdraw)
	return err
}

func (r *postgresCoHostRepository) SetStatus(ctx context.Context, id uuid.UUID, status models.CoHostStatus) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE event_co_hosts SET status = $2, responded_at = now(), updated_at = now() WHERE id = $1`, id, status)
	return err
}

// coHostActive decides whether a co-host relationship still deserves a place in
// the invited host's dashboard. It is presentation only — it gates the tab and
// the shared-experiences card, never authorization (HostCanManage stays a plain
// status check, so a co-host working a late or rescheduled session is never
// locked out at the door).
//
// A share stays active while the invitation is live AND any of:
//   - the experience recurs (its sessions keep coming; `events.time` is rolled
//     forward in Go, not in SQL, so a date test would expire it wrongly),
//   - its last real session is less than 30 days old (wrap-up window),
//   - the co-host still has money to withdraw on it — this term is what stops
//     the UI hiding a row the withdrawal endpoint would still accept, so it is
//     computed exactly as ReserveClaim computes it.
//
// Expects `ch` (event_co_hosts) and `e` (events) in scope.
const coHostActive = `(
	ch.status IN ('pending', 'accepted')
	AND (
		e.is_recurring
		OR GREATEST(
			e.time,
			COALESCE((SELECT MAX(b.occurrence_date) FROM bookings b
			           WHERE b.event_id = e.id AND b.status = 'confirmed'), e.time)
		) >= NOW() - INTERVAL '30 days'
		OR COALESCE((SELECT SUM(b.net_earning_cents) FROM bookings b
		              WHERE b.event_id = e.id AND b.status = 'confirmed'
		                AND b.occurrence_date < NOW()), 0)
		   > COALESCE((SELECT SUM(c.amount_cents) FROM event_cohost_claims c
		                WHERE c.event_id = e.id AND c.status <> 'failed'), 0)
	)
)`

func (r *postgresCoHostRepository) CountsForHost(ctx context.Context, hostID uuid.UUID) (int, int, error) {
	const query = `
		SELECT
			COUNT(*) FILTER (WHERE ch.host_id = $1)::INT AS received,
			COUNT(*) FILTER (WHERE e.host_id  = $1)::INT AS granted
		FROM event_co_hosts ch
		JOIN events e ON e.id = ch.event_id
		WHERE ` + coHostActive
	var received, granted int
	if err := r.db.QueryRowContext(ctx, query, hostID).Scan(&received, &granted); err != nil {
		return 0, 0, err
	}
	return received, granted, nil
}

func (r *postgresCoHostRepository) EventPassedEarnings(ctx context.Context, eventID uuid.UUID) (int64, error) {
	const query = `
		SELECT COALESCE(SUM(net_earning_cents), 0)::BIGINT
		FROM bookings
		WHERE event_id = $1 AND status = 'confirmed' AND occurrence_date < NOW()`
	var sum int64
	err := r.db.QueryRowContext(ctx, query, eventID).Scan(&sum)
	return sum, err
}

func (r *postgresCoHostRepository) EventClaimedCents(ctx context.Context, eventID uuid.UUID) (int64, error) {
	const query = `
		SELECT COALESCE(SUM(amount_cents), 0)::BIGINT
		FROM event_cohost_claims
		WHERE event_id = $1 AND status <> 'failed'`
	var sum int64
	err := r.db.QueryRowContext(ctx, query, eventID).Scan(&sum)
	return sum, err
}

func (r *postgresCoHostRepository) ReserveClaim(ctx context.Context, cohostID, eventID, hostID uuid.UUID, amountCents int64) (uuid.UUID, error) {
	// The whole decision is one statement: the requested amount is inserted only
	// if the event's passed earnings still cover every non-failed claim plus this
	// one. Two co-hosts withdrawing at the same instant therefore cannot both
	// succeed — the second sees the first's row.
	const query = `
		INSERT INTO event_cohost_claims (id, event_co_host_id, event_id, host_id, amount_cents, status)
		-- $5 is cast explicitly: it appears both as a column value and inside the
		-- comparison below, and without the cast Postgres deduces two different
		-- types for the same parameter (SQLSTATE 42P08). $3 is reused the same way
		-- but needs no cast — every one of its uses is a uuid, so there is nothing
		-- to deduce inconsistently.
		SELECT $1, $2, $3, $4, $5::BIGINT, 'reserved'
		WHERE $5::BIGINT <= (
			COALESCE((SELECT SUM(b.net_earning_cents) FROM bookings b
			           WHERE b.event_id = $3 AND b.status = 'confirmed'
			             AND b.occurrence_date < NOW()), 0)
			- COALESCE((SELECT SUM(c.amount_cents) FROM event_cohost_claims c
			             WHERE c.event_id = $3 AND c.status <> 'failed'), 0)
		)
		RETURNING id`
	claimID := uuid.New()
	var out uuid.UUID
	err := r.db.QueryRowContext(ctx, query, claimID, cohostID, eventID, hostID, amountCents).Scan(&out)
	if err == sql.ErrNoRows {
		// The WHERE failed: the event cannot cover this withdrawal.
		return uuid.Nil, nil
	}
	if err != nil {
		return uuid.Nil, err
	}
	return out, nil
}

func (r *postgresCoHostRepository) AttachPayment(ctx context.Context, claimID, paymentID uuid.UUID) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE event_cohost_claims SET payment_id = $2, updated_at = now() WHERE id = $1`, claimID, paymentID)
	return err
}

func (r *postgresCoHostRepository) SetClaimStatus(ctx context.Context, claimID uuid.UUID, status string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE event_cohost_claims SET status = $2, updated_at = now() WHERE id = $1`, claimID, status)
	return err
}

func (r *postgresCoHostRepository) SetClaimStatusByPayment(ctx context.Context, paymentID uuid.UUID, status string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE event_cohost_claims SET status = $2, updated_at = now() WHERE payment_id = $1`, paymentID, status)
	return err
}
