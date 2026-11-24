package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"myslotmate-backend/internal/models"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// BoostRepository stores Event Boosts. Bind it to a transaction with WithTx.
type BoostRepository struct{ db DBTX }

func NewBoostRepository(db DBTX) *BoostRepository { return &BoostRepository{db: db} }

func (r *BoostRepository) WithTx(tx DBTX) *BoostRepository { return &BoostRepository{db: tx} }

// ErrBoostOpen is returned when the event already has an open Boost.
var ErrBoostOpen = errors.New("boost_already_open")

// Bands returns the newest version of the host-guide bands, cheapest first.
func (r *BoostRepository) Bands(ctx context.Context) ([]models.BoostBand, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT version, min_price_cents, max_price_cents, guide_rate_bps, min_rate_bps, max_rate_bps
		  FROM boost_rate_bands
		 WHERE version = (SELECT MAX(version) FROM boost_rate_bands WHERE active_from <= now())
		 ORDER BY min_price_cents`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.BoostBand
	for rows.Next() {
		var b models.BoostBand
		if err := rows.Scan(&b.Version, &b.MinPriceCents, &b.MaxPriceCents, &b.GuideRateBps, &b.MinRateBps, &b.MaxRateBps); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (r *BoostRepository) Create(ctx context.Context, b *models.Boost) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO event_boosts (id, event_id, host_id, status, goal_text, audience_notes, channels, requested_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		b.ID, b.EventID, b.HostID, b.Status, b.GoalText, b.AudienceNotes, pq.Array(b.Channels), b.RequestedAt)
	if isUniqueViolation(err) {
		return ErrBoostOpen
	}
	if err != nil {
		return err
	}
	return r.ReplaceRates(ctx, b.ID, b.Rates)
}

func (r *BoostRepository) ReplaceRates(ctx context.Context, boostID uuid.UUID, rates []models.BoostRate) error {
	if _, err := r.db.ExecContext(ctx, `DELETE FROM event_boost_rates WHERE boost_id = $1`, boostID); err != nil {
		return err
	}
	for _, x := range rates {
		if _, err := r.db.ExecContext(ctx, `
			INSERT INTO event_boost_rates (boost_id, ticket_key, price_tier_id, ticket_name, price_cents, guide_rate_bps, rate_bps, reason, band_version)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			boostID, x.TicketKey, x.PriceTierID, x.TicketName, x.PriceCents, x.GuideRateBps, x.RateBps, x.Reason, x.BandVersion); err != nil {
			return err
		}
	}
	return nil
}

const boostCols = `b.id, b.event_id, b.host_id, b.status, b.goal_text, b.audience_notes, b.channels, b.requested_at,
	b.proposed_at, b.admin_note, b.decline_reason, b.planned_from, b.planned_until, b.accepted_at, b.live_from,
	b.live_until, b.ended_reason, b.reach, b.clicks, e.title, trim(concat_ws(' ', h.first_name, h.last_name))`

const boostFrom = ` FROM event_boosts b JOIN events e ON e.id = b.event_id JOIN hosts h ON h.id = b.host_id `

func (r *BoostRepository) query(ctx context.Context, tail string, args ...any) ([]*models.Boost, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+boostCols+boostFrom+tail, args...)
	if err != nil {
		return nil, err
	}
	var out []*models.Boost
	for rows.Next() {
		b := &models.Boost{}
		if err := rows.Scan(&b.ID, &b.EventID, &b.HostID, &b.Status, &b.GoalText, &b.AudienceNotes, pq.Array(&b.Channels),
			&b.RequestedAt, &b.ProposedAt, &b.AdminNote, &b.DeclineReason, &b.PlannedFrom, &b.PlannedUntil, &b.AcceptedAt,
			&b.LiveFrom, &b.LiveUntil, &b.EndedReason, &b.Reach, &b.Clicks, &b.EventTitle, &b.HostName); err != nil {
			rows.Close()
			return nil, err
		}
		if b.Channels == nil {
			b.Channels = []string{}
		}
		out = append(out, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, b := range out {
		if err := r.fill(ctx, b); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// fill loads a Boost's rates and its sales so far.
func (r *BoostRepository) fill(ctx context.Context, b *models.Boost) error {
	rows, err := r.db.QueryContext(ctx, `
		SELECT ticket_key, price_tier_id, ticket_name, price_cents, guide_rate_bps, rate_bps, reason, band_version
		  FROM event_boost_rates WHERE boost_id = $1 ORDER BY price_cents, ticket_name`, b.ID)
	if err != nil {
		return err
	}
	b.Rates = []models.BoostRate{}
	for rows.Next() {
		var x models.BoostRate
		if err := rows.Scan(&x.TicketKey, &x.PriceTierID, &x.TicketName, &x.PriceCents, &x.GuideRateBps, &x.RateBps, &x.Reason, &x.BandVersion); err != nil {
			rows.Close()
			return err
		}
		b.Rates = append(b.Rates, x)
		if b.TopupReason == nil && x.Reason != nil && *x.Reason != "" {
			b.TopupReason = x.Reason
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	return r.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(quantity) FILTER (WHERE status IN ('pending', 'confirmed')), 0),
		       COALESCE(SUM(boost_commission_cents) FILTER (WHERE status IN ('pending', 'confirmed') OR boost_commission_kept), 0)
		  FROM bookings WHERE boost_id = $1 AND (status IN ('pending', 'confirmed') OR boost_commission_kept)`, b.ID).
		Scan(&b.TicketsSold, &b.CommissionCents)
}

func (r *BoostRepository) one(ctx context.Context, tail string, args ...any) (*models.Boost, error) {
	out, err := r.query(ctx, tail, args...)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return out[0], nil
}

func (r *BoostRepository) GetByID(ctx context.Context, id uuid.UUID) (*models.Boost, error) {
	return r.one(ctx, `WHERE b.id = $1`, id)
}

// GetForUpdate locks the Boost row for the rest of the transaction.
func (r *BoostRepository) GetForUpdate(ctx context.Context, id uuid.UUID) (*models.Boost, error) {
	var one int
	if err := r.db.QueryRowContext(ctx, `SELECT 1 FROM event_boosts WHERE id = $1 FOR UPDATE`, id).Scan(&one); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return r.GetByID(ctx, id)
}

// LatestForEvent is the event's newest Boost, whatever its status.
func (r *BoostRepository) LatestForEvent(ctx context.Context, eventID uuid.UUID) (*models.Boost, error) {
	return r.one(ctx, `WHERE b.event_id = $1 ORDER BY b.requested_at DESC LIMIT 1`, eventID)
}

func (r *BoostRepository) ListByHost(ctx context.Context, hostID uuid.UUID) ([]*models.Boost, error) {
	return r.query(ctx, `WHERE b.host_id = $1 ORDER BY b.requested_at DESC`, hostID)
}

// ListByStatus is the admin queue; "" lists every status.
func (r *BoostRepository) ListByStatus(ctx context.Context, status string, limit int) ([]*models.Boost, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	if status == "" {
		return r.query(ctx, `ORDER BY b.requested_at DESC LIMIT $1`, limit)
	}
	return r.query(ctx, `WHERE b.status = $1 ORDER BY b.requested_at DESC LIMIT $2`, status, limit)
}

// CountByStatus is the admin queue's tab counts.
func (r *BoostRepository) CountByStatus(ctx context.Context) (map[string]int, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM event_boosts GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		out[s] = n
	}
	return out, rows.Err()
}

func (r *BoostRepository) exec(ctx context.Context, q string, args ...any) error {
	_, err := r.db.ExecContext(ctx, q, args...)
	return err
}

func (r *BoostRepository) SetProposal(ctx context.Context, id uuid.UUID, note *string, from, until *time.Time) error {
	return r.exec(ctx, `UPDATE event_boosts SET status = 'quote_ready', proposed_at = now(), admin_note = $2,
		planned_from = $3, planned_until = $4, accepted_at = NULL, accepted_ip = NULL, accepted_user_agent = NULL,
		terms_version = NULL, updated_at = now() WHERE id = $1`, id, note, from, until)
}

func (r *BoostRepository) SetDeclined(ctx context.Context, id uuid.UUID, reason string) error {
	return r.exec(ctx, `UPDATE event_boosts SET status = 'declined', decline_reason = $2, updated_at = now() WHERE id = $1`, id, reason)
}

func (r *BoostRepository) SetAccepted(ctx context.Context, id uuid.UUID, ip, ua string) error {
	return r.exec(ctx, `UPDATE event_boosts SET accepted_at = now(), accepted_ip = $2, accepted_user_agent = $3,
		terms_version = $4, updated_at = now() WHERE id = $1`, id, ip, ua, models.BoostTermsVersion)
}

func (r *BoostRepository) SetCancelled(ctx context.Context, id uuid.UUID) error {
	return r.exec(ctx, `UPDATE event_boosts SET status = 'cancelled', updated_at = now() WHERE id = $1`, id)
}

func (r *BoostRepository) SetLive(ctx context.Context, id uuid.UUID, from time.Time, until *time.Time) error {
	return r.exec(ctx, `UPDATE event_boosts SET status = 'live', live_from = $2, live_until = $3, updated_at = now() WHERE id = $1`, id, from, until)
}

func (r *BoostRepository) SetEnded(ctx context.Context, id uuid.UUID, at time.Time, reason string) error {
	return r.exec(ctx, `UPDATE event_boosts SET status = 'ended', live_until = $2, ended_reason = $3, updated_at = now()
		WHERE id = $1 AND status = 'live'`, id, at, reason)
}

func (r *BoostRepository) SetCampaign(ctx context.Context, id uuid.UUID, reach, clicks int64) error {
	return r.exec(ctx, `UPDATE event_boosts SET reach = $2, clicks = $3, updated_at = now() WHERE id = $1`, id, reach, clicks)
}

// Log appends to the Boost's audit trail.
func (r *BoostRepository) Log(ctx context.Context, boostID uuid.UUID, actorKind, actor, action string, details any) error {
	var raw []byte
	if details != nil {
		b, err := json.Marshal(details)
		if err != nil {
			return err
		}
		raw = b
	}
	var a *string
	if actor != "" {
		a = &actor
	}
	return r.exec(ctx, `INSERT INTO event_boost_log (boost_id, actor_kind, actor, action, details) VALUES ($1, $2, $3, $4, $5)`,
		boostID, actorKind, a, action, raw)
}

func (r *BoostRepository) LogEntries(ctx context.Context, boostID uuid.UUID) ([]models.BoostLogEntry, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT actor_kind, actor, action, details, created_at FROM event_boost_log WHERE boost_id = $1 ORDER BY created_at, id`, boostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.BoostLogEntry
	for rows.Next() {
		var e models.BoostLogEntry
		var details []byte
		if err := rows.Scan(&e.ActorKind, &e.Actor, &e.Action, &details, &e.CreatedAt); err != nil {
			return nil, err
		}
		if len(details) > 0 {
			e.Details = details
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// LiveRate is the commission rate that applies to a ticket type right now.
type LiveRate struct {
	BoostID    uuid.UUID
	RateBps    int
	PriceCents int64 // the price the rate was quoted on
}

// LiveRateFor finds the live Boost's rate for one ticket type of an event at
// time at (ticketKey: see models.BoostTicketKey). Nil when no Boost is live or
// the ticket type has no quoted rate.
func (r *BoostRepository) LiveRateFor(ctx context.Context, eventID uuid.UUID, ticketKey string, at time.Time) (*LiveRate, error) {
	lr := &LiveRate{}
	err := r.db.QueryRowContext(ctx, `
		SELECT b.id, x.rate_bps, x.price_cents
		  FROM event_boosts b JOIN event_boost_rates x ON x.boost_id = b.id
		 WHERE b.event_id = $1 AND b.status = 'live'
		   AND b.live_from <= $3 AND (b.live_until IS NULL OR b.live_until > $3)
		   AND x.ticket_key = $2
		 LIMIT 1`, eventID, ticketKey, at).Scan(&lr.BoostID, &lr.RateBps, &lr.PriceCents)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return lr, err
}

// DueToEnd lists live Boosts whose window is over at time now: past
// live_until, or a one-time event that has started. The second value is when
// each ended.
func (r *BoostRepository) DueToEnd(ctx context.Context, now time.Time) (map[uuid.UUID]time.Time, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT b.id, CASE WHEN b.live_until IS NOT NULL AND b.live_until <= $1 THEN b.live_until ELSE e.time END
		  FROM event_boosts b JOIN events e ON e.id = b.event_id
		 WHERE b.status = 'live'
		   AND ((b.live_until IS NOT NULL AND b.live_until <= $1)
		        OR (COALESCE(e.schedule_type, 'one_time') = 'one_time' AND e.time <= $1))`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]time.Time{}
	for rows.Next() {
		var id uuid.UUID
		var at time.Time
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		out[id] = at
	}
	return out, rows.Err()
}

// HostBoostAccess is what the request checks need to know about the host.
type HostBoostAccess struct {
	Invited         bool
	Approved        bool
	HasPayoutMethod bool
	CompletedEvents int
}

func (r *BoostRepository) HostAccess(ctx context.Context, hostID uuid.UUID) (*HostBoostAccess, error) {
	a := &HostBoostAccess{}
	err := r.db.QueryRowContext(ctx, `
		SELECT h.boost_invited, h.application_status = 'approved',
		       EXISTS (SELECT 1 FROM payout_methods m WHERE m.host_id = h.id),
		       (SELECT COUNT(*) FROM events e WHERE e.host_id = h.id AND e.time < now() AND e.status <> 'cancelled')
		  FROM hosts h WHERE h.id = $1`, hostID).Scan(&a.Invited, &a.Approved, &a.HasPayoutMethod, &a.CompletedEvents)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return a, err
}

func (r *BoostRepository) SetInvited(ctx context.Context, hostID uuid.UUID, invited bool) error {
	return r.exec(ctx, `UPDATE hosts SET boost_invited = $2 WHERE id = $1`, hostID, invited)
}
