package repository

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// AdminDirectoryRepository provides read-only, aggregated views of users and
// hosts for the admin dashboard's directory tabs. It deliberately sits beside
// the domain repositories (rather than extending their interfaces) because the
// shapes are dashboard-specific and join across bookings/events.
type AdminDirectoryRepository struct {
	db *sql.DB
}

func NewAdminDirectoryRepository(db *sql.DB) *AdminDirectoryRepository {
	return &AdminDirectoryRepository{db: db}
}

// AdminUserRow is one row of the users directory with booking aggregates.
type AdminUserRow struct {
	ID              uuid.UUID
	Name            string
	Email           string
	City            string
	IsVerified      bool
	CreatedAt       time.Time
	TotalBookings   int64
	TotalSpentCents int64
}

// ListUsersParams controls pagination and server-side filtering of the users
// directory.
type ListUsersParams struct {
	Limit  int
	Offset int
	Search string // matches name / email / city (case-insensitive)
	City   string // exact city match
	Tier   string // "", "high_value", "repeat", "new"
}

// ListUsers returns a page of users with their confirmed-booking count and total
// confirmed spend (newest first), plus the total count matching the filters.
func (r *AdminDirectoryRepository) ListUsers(ctx context.Context, p ListUsersParams) ([]AdminUserRow, int, error) {
	// Filter conditions live in WHERE (non-aggregate) and HAVING (aggregate/tier).
	var conds []string
	var args []any

	if s := strings.TrimSpace(p.Search); s != "" {
		args = append(args, "%"+s+"%")
		i := len(args)
		conds = append(conds, fmt.Sprintf("(u.name ILIKE $%d OR u.email ILIKE $%d OR COALESCE(u.city,'') ILIKE $%d)", i, i, i))
	}
	if c := strings.TrimSpace(p.City); c != "" {
		args = append(args, c)
		conds = append(conds, fmt.Sprintf("u.city = $%d", len(args)))
	}

	whereSQL := ""
	if len(conds) > 0 {
		whereSQL = "WHERE " + strings.Join(conds, " AND ")
	}

	havingSQL := ""
	switch p.Tier {
	case "high_value":
		havingSQL = "HAVING COALESCE(SUM(b.amount_cents) FILTER (WHERE b.status = 'confirmed'), 0) >= 50000"
	case "repeat":
		havingSQL = "HAVING COUNT(b.id) FILTER (WHERE b.status = 'confirmed') >= 5"
	case "new":
		havingSQL = "HAVING COUNT(b.id) FILTER (WHERE b.status = 'confirmed') <= 3"
	}

	// Total count: wrap the grouped/having query so tier filters count correctly.
	countSQL := fmt.Sprintf(`
		SELECT COUNT(*) FROM (
			SELECT u.id
			FROM users u
			LEFT JOIN bookings b ON b.user_id = u.id
			%s
			GROUP BY u.id
			%s
		) t`, whereSQL, havingSQL)

	var total int
	if err := r.db.QueryRowContext(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	// Page query appends LIMIT/OFFSET as the last two placeholders.
	pageArgs := append(append([]any{}, args...), p.Limit, p.Offset)
	limitIdx, offsetIdx := len(args)+1, len(args)+2

	pageSQL := fmt.Sprintf(`
		SELECT
			u.id,
			u.name,
			u.email,
			COALESCE(u.city, '') AS city,
			u.is_verified,
			u.created_at,
			COUNT(b.id) FILTER (WHERE b.status = 'confirmed') AS total_bookings,
			COALESCE(SUM(b.amount_cents) FILTER (WHERE b.status = 'confirmed'), 0)::BIGINT AS total_spent_cents
		FROM users u
		LEFT JOIN bookings b ON b.user_id = u.id
		%s
		GROUP BY u.id
		%s
		ORDER BY u.created_at DESC
		LIMIT $%d OFFSET $%d`, whereSQL, havingSQL, limitIdx, offsetIdx)

	rows, err := r.db.QueryContext(ctx, pageSQL, pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := make([]AdminUserRow, 0)
	for rows.Next() {
		var u AdminUserRow
		if err := rows.Scan(
			&u.ID, &u.Name, &u.Email, &u.City, &u.IsVerified, &u.CreatedAt,
			&u.TotalBookings, &u.TotalSpentCents,
		); err != nil {
			return nil, 0, err
		}
		out = append(out, u)
	}
	return out, total, rows.Err()
}

// AdminEventRow is one row of the experiences/events directory, joined with the
// owning host for display.
type AdminEventRow struct {
	ID            uuid.UUID
	Title         string
	Mood          sql.NullString
	PriceCents    sql.NullInt64
	IsFree        bool
	TotalBookings int64
	AvgRating     sql.NullFloat64
	Status        string
	HostFirstName sql.NullString
	HostLastName  sql.NullString
	HostCity      sql.NullString
}

// ListEventsParams controls pagination and server-side filtering of the events
// directory.
type ListEventsParams struct {
	Limit  int
	Offset int
	Search string // matches title / host name / city / mood
	Status string // exact event status: draft | live | paused | cancelled
}

// ListEvents returns a page of all events (any status) joined with their host,
// newest first, plus the total count matching the filters.
func (r *AdminDirectoryRepository) ListEvents(ctx context.Context, p ListEventsParams) ([]AdminEventRow, int, error) {
	var conds []string
	var args []any

	if s := strings.TrimSpace(p.Search); s != "" {
		args = append(args, "%"+s+"%")
		i := len(args)
		conds = append(conds, fmt.Sprintf(
			"(e.title ILIKE $%d OR (COALESCE(h.first_name,'') || ' ' || COALESCE(h.last_name,'')) ILIKE $%d OR COALESCE(h.city,'') ILIKE $%d OR COALESCE(e.mood::text,'') ILIKE $%d)",
			i, i, i, i))
	}
	if st := strings.TrimSpace(p.Status); st != "" {
		args = append(args, st)
		conds = append(conds, fmt.Sprintf("e.status = $%d", len(args)))
	}

	whereSQL := ""
	if len(conds) > 0 {
		whereSQL = "WHERE " + strings.Join(conds, " AND ")
	}

	countSQL := "SELECT COUNT(*) FROM events e LEFT JOIN hosts h ON h.id = e.host_id " + whereSQL
	var total int
	if err := r.db.QueryRowContext(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	pageArgs := append(append([]any{}, args...), p.Limit, p.Offset)
	limitIdx, offsetIdx := len(args)+1, len(args)+2

	pageSQL := fmt.Sprintf(`
		SELECT
			e.id,
			e.title,
			e.mood,
			e.price_cents,
			e.is_free,
			e.total_bookings,
			e.avg_rating,
			e.status,
			h.first_name,
			h.last_name,
			h.city
		FROM events e
		LEFT JOIN hosts h ON h.id = e.host_id
		%s
		ORDER BY e.created_at DESC
		LIMIT $%d OFFSET $%d`, whereSQL, limitIdx, offsetIdx)

	rows, err := r.db.QueryContext(ctx, pageSQL, pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := make([]AdminEventRow, 0)
	for rows.Next() {
		var e AdminEventRow
		if err := rows.Scan(
			&e.ID, &e.Title, &e.Mood, &e.PriceCents, &e.IsFree,
			&e.TotalBookings, &e.AvgRating, &e.Status,
			&e.HostFirstName, &e.HostLastName, &e.HostCity,
		); err != nil {
			return nil, 0, err
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}

// HostAggregates holds the dashboard stats for a single host.
type HostAggregates struct {
	ExperiencesCreated int64
	BookingsGenerated  int64
	RevenueCents       int64
}

// GetHostAggregates returns the experiences count, confirmed-booking count, and
// net revenue for a single host.
func (r *AdminDirectoryRepository) GetHostAggregates(ctx context.Context, hostID uuid.UUID) (HostAggregates, error) {
	const query = `
		SELECT
			(SELECT COUNT(*) FROM events WHERE host_id = $1) AS experiences_created,
			COALESCE((
				SELECT COUNT(b.id)
				FROM bookings b JOIN events e ON e.id = b.event_id
				WHERE e.host_id = $1 AND b.status = 'confirmed'
			), 0) AS bookings_generated,
			COALESCE((
				SELECT SUM(b.net_earning_cents)
				FROM bookings b JOIN events e ON e.id = b.event_id
				WHERE e.host_id = $1 AND b.status = 'confirmed'
			), 0)::BIGINT AS revenue_cents`

	var a HostAggregates
	err := r.db.QueryRowContext(ctx, query, hostID).Scan(
		&a.ExperiencesCreated, &a.BookingsGenerated, &a.RevenueCents,
	)
	return a, err
}

// AdminHostRow is one row of the hosts directory with listing/booking/revenue
// aggregates.
type AdminHostRow struct {
	ID                 uuid.UUID
	FirstName          string
	LastName           string
	City               string
	ApplicationStatus  string
	AvgRating          sql.NullFloat64
	SocialInstagram    sql.NullString
	ExperiencesCreated int64
	BookingsGenerated  int64
	RevenueCents       int64
	CreatedAt          time.Time
}

// ListHostsParams controls pagination and server-side filtering of the hosts
// directory.
type ListHostsParams struct {
	Limit  int
	Offset int
	Search string // matches full name / city (case-insensitive)
}

// ListHosts returns a page of submitted hosts (draft applications excluded) with
// their experiences count, confirmed bookings, and net revenue (newest first),
// plus the total count matching the filters.
func (r *AdminDirectoryRepository) ListHosts(ctx context.Context, p ListHostsParams) ([]AdminHostRow, int, error) {
	conds := []string{"h.application_status <> 'draft'"}
	var args []any

	if s := strings.TrimSpace(p.Search); s != "" {
		args = append(args, "%"+s+"%")
		i := len(args)
		conds = append(conds, fmt.Sprintf("((h.first_name || ' ' || h.last_name) ILIKE $%d OR h.city ILIKE $%d)", i, i))
	}

	whereSQL := "WHERE " + strings.Join(conds, " AND ")

	countSQL := "SELECT COUNT(*) FROM hosts h " + whereSQL
	var total int
	if err := r.db.QueryRowContext(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	pageArgs := append(append([]any{}, args...), p.Limit, p.Offset)
	limitIdx, offsetIdx := len(args)+1, len(args)+2

	pageSQL := fmt.Sprintf(`
		SELECT
			h.id,
			h.first_name,
			h.last_name,
			h.city,
			h.application_status,
			h.avg_rating,
			h.social_instagram,
			COALESCE(ev.experiences_created, 0) AS experiences_created,
			COALESCE(bk.bookings_generated, 0)  AS bookings_generated,
			COALESCE(bk.revenue_cents, 0)::BIGINT AS revenue_cents,
			h.created_at
		FROM hosts h
		LEFT JOIN (
			SELECT host_id, COUNT(*) AS experiences_created
			FROM events
			GROUP BY host_id
		) ev ON ev.host_id = h.id
		LEFT JOIN (
			SELECT e.host_id,
			       COUNT(b.id) AS bookings_generated,
			       COALESCE(SUM(b.net_earning_cents), 0) AS revenue_cents
			FROM bookings b
			JOIN events e ON e.id = b.event_id
			WHERE b.status = 'confirmed'
			GROUP BY e.host_id
		) bk ON bk.host_id = h.id
		%s
		ORDER BY h.created_at DESC
		LIMIT $%d OFFSET $%d`, whereSQL, limitIdx, offsetIdx)

	rows, err := r.db.QueryContext(ctx, pageSQL, pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := make([]AdminHostRow, 0)
	for rows.Next() {
		var h AdminHostRow
		if err := rows.Scan(
			&h.ID, &h.FirstName, &h.LastName, &h.City, &h.ApplicationStatus,
			&h.AvgRating, &h.SocialInstagram,
			&h.ExperiencesCreated, &h.BookingsGenerated, &h.RevenueCents,
			&h.CreatedAt,
		); err != nil {
			return nil, 0, err
		}
		out = append(out, h)
	}
	return out, total, rows.Err()
}
