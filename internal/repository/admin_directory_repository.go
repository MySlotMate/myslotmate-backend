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
	HostID        sql.NullString
	IsRecurring   bool
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
			e.host_id,
			e.is_recurring,
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
			&e.ID, &e.HostID, &e.IsRecurring, &e.Title, &e.Mood, &e.PriceCents, &e.IsFree,
			&e.TotalBookings, &e.AvgRating, &e.Status,
			&e.HostFirstName, &e.HostLastName, &e.HostCity,
		); err != nil {
			return nil, 0, err
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}

// AdminBookingRow is one row of the bookings directory, joined with the guest,
// experience, owning host, and payment for display.
type AdminBookingRow struct {
	ID             uuid.UUID
	EventID        uuid.UUID
	OccurrenceDate sql.NullTime
	CreatedAt      time.Time
	Quantity       int64
	AmountCents    sql.NullInt64
	Status         string
	UserName       sql.NullString
	UserEmail      sql.NullString
	EventTitle     sql.NullString
	HostFirstName  sql.NullString
	HostLastName   sql.NullString
	HostCity       sql.NullString
	PaymentStatus  sql.NullString
}

// ListBookingsParams controls pagination and server-side filtering of the
// bookings directory.
type ListBookingsParams struct {
	Limit   int
	Offset  int
	Search  string // matches guest name/email, experience title, or host name
	Status  string // exact booking status: pending | confirmed | cancelled | refunded
	EventID string // exact event (experience) UUID
}

// ListBookings returns a page of all bookings (newest first) joined with guest,
// experience, host, and payment, plus the total count matching the filters.
func (r *AdminDirectoryRepository) ListBookings(ctx context.Context, p ListBookingsParams) ([]AdminBookingRow, int, error) {
	var conds []string
	var args []any

	if s := strings.TrimSpace(p.Search); s != "" {
		args = append(args, "%"+s+"%")
		i := len(args)
		conds = append(conds, fmt.Sprintf(
			"(COALESCE(u.name,'') ILIKE $%d OR COALESCE(u.email,'') ILIKE $%d OR COALESCE(e.title,'') ILIKE $%d OR (COALESCE(h.first_name,'') || ' ' || COALESCE(h.last_name,'')) ILIKE $%d)",
			i, i, i, i))
	}
	if st := strings.TrimSpace(p.Status); st != "" {
		args = append(args, st)
		conds = append(conds, fmt.Sprintf("b.status = $%d", len(args)))
	}
	if e := strings.TrimSpace(p.EventID); e != "" {
		args = append(args, e)
		conds = append(conds, fmt.Sprintf("b.event_id = $%d::uuid", len(args)))
	}

	whereSQL := ""
	if len(conds) > 0 {
		whereSQL = "WHERE " + strings.Join(conds, " AND ")
	}

	const joins = `
		FROM bookings b
		LEFT JOIN users  u ON u.id = b.user_id
		LEFT JOIN events e ON e.id = b.event_id
		LEFT JOIN hosts  h ON h.id = e.host_id`

	countSQL := "SELECT COUNT(*) " + joins + " " + whereSQL
	var total int
	if err := r.db.QueryRowContext(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	pageArgs := append(append([]any{}, args...), p.Limit, p.Offset)
	limitIdx, offsetIdx := len(args)+1, len(args)+2

	pageSQL := fmt.Sprintf(`
		SELECT
			b.id, b.event_id, b.occurrence_date, b.created_at, b.quantity, b.amount_cents, b.status,
			u.name, u.email,
			e.title,
			h.first_name, h.last_name, h.city,
			p.status
		%s
		LEFT JOIN payments p ON p.id = b.payment_id
		%s
		ORDER BY b.created_at DESC
		LIMIT $%d OFFSET $%d`, joins, whereSQL, limitIdx, offsetIdx)

	rows, err := r.db.QueryContext(ctx, pageSQL, pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := make([]AdminBookingRow, 0)
	for rows.Next() {
		var b AdminBookingRow
		if err := rows.Scan(
			&b.ID, &b.EventID, &b.OccurrenceDate, &b.CreatedAt, &b.Quantity, &b.AmountCents, &b.Status,
			&b.UserName, &b.UserEmail,
			&b.EventTitle,
			&b.HostFirstName, &b.HostLastName, &b.HostCity,
			&b.PaymentStatus,
		); err != nil {
			return nil, 0, err
		}
		out = append(out, b)
	}
	return out, total, rows.Err()
}

// ── Dashboard summary ────────────────────────────────────────────────────────

// MonthlyBookingCount is one bar of the monthly-bookings trend.
type MonthlyBookingCount struct {
	Month string // short label, e.g. "Jan"
	Count int64
}

// DashboardTopHost is a leaderboard entry ranked by net revenue.
type DashboardTopHost struct {
	ID           uuid.UUID
	FirstName    string
	LastName     string
	City         string
	RevenueCents int64
}

// DashboardTopExperience is a leaderboard entry ranked by bookings.
type DashboardTopExperience struct {
	ID            uuid.UUID
	Title         string
	HostFirstName sql.NullString
	HostLastName  sql.NullString
	Bookings      int64
}

// DashboardData holds aggregate counts and leaderboards for the admin dashboard.
type DashboardData struct {
	TotalEvents         int64
	TotalHosts          int64
	TotalBookings       int64
	TotalRevenueCents   int64
	PlatformIncomeCents int64
	PendingHosts        int64
	ReviewEvents        int64
	RefundsToReview     int64
	MonthlyBookings     []MonthlyBookingCount
	TopHosts            []DashboardTopHost
	TopExperiences      []DashboardTopExperience
}

// GetDashboardStats computes the dashboard summary in a few aggregate queries.
func (r *AdminDirectoryRepository) GetDashboardStats(ctx context.Context) (*DashboardData, error) {
	d := &DashboardData{}

	// 1. Headline counts + confirmed revenue/income in a single round-trip.
	const countsSQL = `
		SELECT
			(SELECT COUNT(*) FROM events),
			(SELECT COUNT(*) FROM hosts WHERE application_status <> 'draft'),
			(SELECT COUNT(*) FROM bookings),
			(SELECT COUNT(*) FROM hosts WHERE application_status IN ('pending','under_review')),
			(SELECT COUNT(*) FROM events WHERE status IN ('draft','paused')),
			(SELECT COUNT(*) FROM bookings WHERE status IN ('refunded','cancelled')),
			COALESCE((SELECT SUM(amount_cents) FROM bookings WHERE status = 'confirmed'), 0)::BIGINT,
			COALESCE((SELECT SUM(service_fee_cents) FROM bookings WHERE status = 'confirmed'), 0)::BIGINT`
	if err := r.db.QueryRowContext(ctx, countsSQL).Scan(
		&d.TotalEvents, &d.TotalHosts, &d.TotalBookings,
		&d.PendingHosts, &d.ReviewEvents, &d.RefundsToReview,
		&d.TotalRevenueCents, &d.PlatformIncomeCents,
	); err != nil {
		return nil, err
	}

	// 2. Monthly bookings — last 6 months, zero-filled.
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).AddDate(0, -5, 0)
	const monthlySQL = `
		SELECT EXTRACT(YEAR FROM created_at)::int, EXTRACT(MONTH FROM created_at)::int, COUNT(*)
		FROM bookings
		WHERE created_at >= $1
		GROUP BY 1, 2`
	rows, err := r.db.QueryContext(ctx, monthlySQL, start)
	if err != nil {
		return nil, err
	}
	monthCounts := make(map[int]int64)
	for rows.Next() {
		var y, m int
		var c int64
		if err := rows.Scan(&y, &m, &c); err != nil {
			rows.Close()
			return nil, err
		}
		monthCounts[y*100+m] = c
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	cur := start
	for i := 0; i < 6; i++ {
		key := cur.Year()*100 + int(cur.Month())
		d.MonthlyBookings = append(d.MonthlyBookings, MonthlyBookingCount{Month: cur.Format("Jan"), Count: monthCounts[key]})
		cur = cur.AddDate(0, 1, 0)
	}

	// 3. Top hosts by net revenue (confirmed bookings).
	const topHostsSQL = `
		SELECT h.id, h.first_name, h.last_name, h.city,
		       COALESCE(SUM(b.net_earning_cents), 0)::BIGINT AS rev
		FROM hosts h
		JOIN events e   ON e.host_id = h.id
		JOIN bookings b ON b.event_id = e.id AND b.status = 'confirmed'
		GROUP BY h.id
		ORDER BY rev DESC
		LIMIT 5`
	hrows, err := r.db.QueryContext(ctx, topHostsSQL)
	if err != nil {
		return nil, err
	}
	for hrows.Next() {
		var th DashboardTopHost
		if err := hrows.Scan(&th.ID, &th.FirstName, &th.LastName, &th.City, &th.RevenueCents); err != nil {
			hrows.Close()
			return nil, err
		}
		d.TopHosts = append(d.TopHosts, th)
	}
	hrows.Close()
	if err := hrows.Err(); err != nil {
		return nil, err
	}

	// 4. Top experiences by bookings.
	const topExpSQL = `
		SELECT e.id, e.title, h.first_name, h.last_name, e.total_bookings
		FROM events e
		LEFT JOIN hosts h ON h.id = e.host_id
		WHERE e.total_bookings > 0
		ORDER BY e.total_bookings DESC
		LIMIT 5`
	erows, err := r.db.QueryContext(ctx, topExpSQL)
	if err != nil {
		return nil, err
	}
	for erows.Next() {
		var te DashboardTopExperience
		if err := erows.Scan(&te.ID, &te.Title, &te.HostFirstName, &te.HostLastName, &te.Bookings); err != nil {
			erows.Close()
			return nil, err
		}
		d.TopExperiences = append(d.TopExperiences, te)
	}
	erows.Close()
	if err := erows.Err(); err != nil {
		return nil, err
	}

	return d, nil
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

// ─────────────────────────────────────────────────────────────────────────────
//  Admin Payments tab — payments feed, ledger, balances, reconciliation summary
// ─────────────────────────────────────────────────────────────────────────────

// ownerJoins resolves an account's owner display name across users/hosts/platform.
const ownerJoins = `
	JOIN accounts a ON a.id = %s.account_id
	LEFT JOIN users u ON a.owner_type = 'user' AND u.id = a.owner_id
	LEFT JOIN hosts h ON a.owner_type = 'host' AND h.id = a.owner_id`

const ownerNameExpr = `CASE a.owner_type
		WHEN 'host'     THEN COALESCE(h.first_name,'') || ' ' || COALESCE(h.last_name,'')
		WHEN 'user'     THEN COALESCE(u.name,'')
		ELSE 'Platform' END`

const ownerSearchExpr = `(COALESCE(u.name,'') ILIKE $%d OR COALESCE(u.email,'') ILIKE $%d OR (COALESCE(h.first_name,'') || ' ' || COALESCE(h.last_name,'')) ILIKE $%d)`

// AdminPaymentRow is one row of the global payments feed with its owner.
type AdminPaymentRow struct {
	ID               uuid.UUID
	Type             string
	AmountCents      int64
	Status           string
	DisplayReference sql.NullString
	ReferenceID      sql.NullString
	CreatedAt        time.Time
	OwnerType        string
	OwnerName        sql.NullString
	OwnerEmail       sql.NullString
}

// ListPaymentsParams controls pagination and filtering of the payments feed.
type ListPaymentsParams struct {
	Limit, Offset int
	Search        string // owner name / email
	Type          string // payment_type
	Status        string // payment_status
	From, To      string // YYYY-MM-DD (inclusive)
}

// ListPayments returns a page of all payments (newest first) joined to the
// owning account's user/host, plus the total count matching the filters.
func (r *AdminDirectoryRepository) ListPayments(ctx context.Context, p ListPaymentsParams) ([]AdminPaymentRow, int, error) {
	var conds []string
	var args []any
	if s := strings.TrimSpace(p.Search); s != "" {
		args = append(args, "%"+s+"%")
		i := len(args)
		conds = append(conds, fmt.Sprintf(ownerSearchExpr, i, i, i))
	}
	if t := strings.TrimSpace(p.Type); t != "" {
		args = append(args, t)
		conds = append(conds, fmt.Sprintf("pm.type = $%d", len(args)))
	}
	if st := strings.TrimSpace(p.Status); st != "" {
		args = append(args, st)
		conds = append(conds, fmt.Sprintf("pm.status = $%d", len(args)))
	}
	if f := strings.TrimSpace(p.From); f != "" {
		args = append(args, f)
		conds = append(conds, fmt.Sprintf("pm.created_at >= $%d::date", len(args)))
	}
	if t := strings.TrimSpace(p.To); t != "" {
		args = append(args, t)
		conds = append(conds, fmt.Sprintf("pm.created_at < ($%d::date + 1)", len(args)))
	}
	whereSQL := ""
	if len(conds) > 0 {
		whereSQL = "WHERE " + strings.Join(conds, " AND ")
	}
	joins := "FROM payments pm" + fmt.Sprintf(ownerJoins, "pm")

	var total int
	if err := r.db.QueryRowContext(ctx, "SELECT COUNT(*) "+joins+" "+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	pageArgs := append(append([]any{}, args...), p.Limit, p.Offset)
	pageSQL := fmt.Sprintf(`
		SELECT pm.id, pm.type, pm.amount_cents, pm.status,
		       pm.display_reference, pm.reference_id::text, pm.created_at,
		       a.owner_type, %s,
		       CASE a.owner_type WHEN 'user' THEN u.email ELSE NULL END
		%s %s
		ORDER BY pm.created_at DESC
		LIMIT $%d OFFSET $%d`, ownerNameExpr, joins, whereSQL, len(args)+1, len(args)+2)

	rows, err := r.db.QueryContext(ctx, pageSQL, pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := make([]AdminPaymentRow, 0)
	for rows.Next() {
		var p AdminPaymentRow
		if err := rows.Scan(&p.ID, &p.Type, &p.AmountCents, &p.Status,
			&p.DisplayReference, &p.ReferenceID, &p.CreatedAt,
			&p.OwnerType, &p.OwnerName, &p.OwnerEmail); err != nil {
			return nil, 0, err
		}
		out = append(out, p)
	}
	return out, total, rows.Err()
}

// AdminLedgerRow is one transaction_ledger entry with its account owner.
type AdminLedgerRow struct {
	ID                uuid.UUID
	Type              string
	AmountCents       int64
	BalanceAfterCents int64
	ReferenceType     sql.NullString
	Description       sql.NullString
	Status            string
	CreatedAt         time.Time
	OwnerType         string
	OwnerName         sql.NullString
}

// ListLedgerParams controls pagination and filtering of the ledger feed.
type ListLedgerParams struct {
	Limit, Offset int
	Search        string // owner name / email
	Type          string // ledger transaction type
	From, To      string // YYYY-MM-DD (inclusive)
}

// ListLedgerEntries returns a page of ledger entries (newest first) joined to
// the owning account's user/host, plus the total count matching the filters.
func (r *AdminDirectoryRepository) ListLedgerEntries(ctx context.Context, p ListLedgerParams) ([]AdminLedgerRow, int, error) {
	var conds []string
	var args []any
	if s := strings.TrimSpace(p.Search); s != "" {
		args = append(args, "%"+s+"%")
		i := len(args)
		conds = append(conds, fmt.Sprintf(ownerSearchExpr, i, i, i))
	}
	if t := strings.TrimSpace(p.Type); t != "" {
		args = append(args, t)
		conds = append(conds, fmt.Sprintf("l.type = $%d", len(args)))
	}
	if f := strings.TrimSpace(p.From); f != "" {
		args = append(args, f)
		conds = append(conds, fmt.Sprintf("l.created_at >= $%d::date", len(args)))
	}
	if t := strings.TrimSpace(p.To); t != "" {
		args = append(args, t)
		conds = append(conds, fmt.Sprintf("l.created_at < ($%d::date + 1)", len(args)))
	}
	whereSQL := ""
	if len(conds) > 0 {
		whereSQL = "WHERE " + strings.Join(conds, " AND ")
	}
	joins := "FROM transaction_ledger l" + fmt.Sprintf(ownerJoins, "l")

	var total int
	if err := r.db.QueryRowContext(ctx, "SELECT COUNT(*) "+joins+" "+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	pageArgs := append(append([]any{}, args...), p.Limit, p.Offset)
	pageSQL := fmt.Sprintf(`
		SELECT l.id, l.type, l.amount_cents, l.balance_after_cents,
		       l.reference_type, l.description, l.status, l.created_at,
		       a.owner_type, %s
		%s %s
		ORDER BY l.created_at DESC
		LIMIT $%d OFFSET $%d`, ownerNameExpr, joins, whereSQL, len(args)+1, len(args)+2)

	rows, err := r.db.QueryContext(ctx, pageSQL, pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := make([]AdminLedgerRow, 0)
	for rows.Next() {
		var l AdminLedgerRow
		if err := rows.Scan(&l.ID, &l.Type, &l.AmountCents, &l.BalanceAfterCents,
			&l.ReferenceType, &l.Description, &l.Status, &l.CreatedAt,
			&l.OwnerType, &l.OwnerName); err != nil {
			return nil, 0, err
		}
		out = append(out, l)
	}
	return out, total, rows.Err()
}

// AdminBalanceRow is one account's stored balance vs its ledger sum (drift).
type AdminBalanceRow struct {
	AccountID    uuid.UUID
	OwnerType    string
	OwnerName    sql.NullString
	BalanceCents int64
	LedgerCents  int64
	DriftCents   int64
}

// ListBalancesParams controls pagination and filtering of the balances view.
type ListBalancesParams struct {
	Limit, Offset int
	Search        string // owner name / email
	OwnerType     string // user | host | platform
}

// ListAccountBalances returns a page of accounts with their stored balance, the
// sum of their ledger entries, and the drift between the two (largest drift first).
func (r *AdminDirectoryRepository) ListAccountBalances(ctx context.Context, p ListBalancesParams) ([]AdminBalanceRow, int, error) {
	var conds []string
	var args []any
	if s := strings.TrimSpace(p.Search); s != "" {
		args = append(args, "%"+s+"%")
		i := len(args)
		conds = append(conds, fmt.Sprintf(ownerSearchExpr, i, i, i))
	}
	if ot := strings.TrimSpace(p.OwnerType); ot != "" {
		args = append(args, ot)
		conds = append(conds, fmt.Sprintf("a.owner_type = $%d", len(args)))
	}
	whereSQL := ""
	if len(conds) > 0 {
		whereSQL = "WHERE " + strings.Join(conds, " AND ")
	}
	ownerOnly := `
		FROM accounts a
		LEFT JOIN users u ON a.owner_type = 'user' AND u.id = a.owner_id
		LEFT JOIN hosts h ON a.owner_type = 'host' AND h.id = a.owner_id`

	var total int
	if err := r.db.QueryRowContext(ctx, "SELECT COUNT(*) "+ownerOnly+" "+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	pageArgs := append(append([]any{}, args...), p.Limit, p.Offset)
	pageSQL := fmt.Sprintf(`
		SELECT a.id, a.owner_type, %s, a.balance_cents,
		       COALESCE((SELECT SUM(amount_cents) FROM transaction_ledger l WHERE l.account_id = a.id), 0)::bigint AS ledger_cents,
		       (a.balance_cents - COALESCE((SELECT SUM(amount_cents) FROM transaction_ledger l WHERE l.account_id = a.id), 0))::bigint AS drift_cents
		%s %s
		ORDER BY ABS(a.balance_cents - COALESCE((SELECT SUM(amount_cents) FROM transaction_ledger l WHERE l.account_id = a.id), 0)) DESC, a.balance_cents DESC
		LIMIT $%d OFFSET $%d`, ownerNameExpr, ownerOnly, whereSQL, len(args)+1, len(args)+2)

	rows, err := r.db.QueryContext(ctx, pageSQL, pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := make([]AdminBalanceRow, 0)
	for rows.Next() {
		var b AdminBalanceRow
		if err := rows.Scan(&b.AccountID, &b.OwnerType, &b.OwnerName,
			&b.BalanceCents, &b.LedgerCents, &b.DriftCents); err != nil {
			return nil, 0, err
		}
		out = append(out, b)
	}
	return out, total, rows.Err()
}

// PaymentsSummary holds the reconciliation/health figures for the Payments tab.
type PaymentsSummary struct {
	TopupsInCents    int64 `json:"topups_in_cents"`
	PayoutsOutCents  int64 `json:"payouts_out_cents"`
	RefundsCents     int64 `json:"refunds_cents"`
	BookingVolCents  int64 `json:"booking_volume_cents"`
	PlatformBalCents int64 `json:"platform_balance_cents"`
	PendingCount     int64 `json:"pending_count"`
	FailedCount      int64 `json:"failed_count"`
	DriftAccounts    int64 `json:"drift_accounts"`
}

// GetPaymentsSummary computes money-in/out totals, pending/failed counts, the
// retained platform balance, and the number of accounts whose stored balance
// disagrees with their ledger sum.
func (r *AdminDirectoryRepository) GetPaymentsSummary(ctx context.Context) (*PaymentsSummary, error) {
	s := &PaymentsSummary{}
	err := r.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(amount_cents) FILTER (WHERE type='topup' AND status='completed'), 0),
			COALESCE(SUM(amount_cents) FILTER (WHERE type IN ('payout','withdrawal') AND status='completed'), 0),
			COALESCE(SUM(amount_cents) FILTER (WHERE type='refund' AND status='completed'), 0),
			COALESCE(SUM(amount_cents) FILTER (WHERE type='booking' AND status='completed'), 0),
			COUNT(*) FILTER (WHERE status='pending'),
			COUNT(*) FILTER (WHERE status='failed')
		FROM payments`).Scan(
		&s.TopupsInCents, &s.PayoutsOutCents, &s.RefundsCents, &s.BookingVolCents,
		&s.PendingCount, &s.FailedCount)
	if err != nil {
		return nil, err
	}
	if err := r.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(balance_cents),0) FROM accounts WHERE owner_type='platform'`).Scan(&s.PlatformBalCents); err != nil {
		return nil, err
	}
	if err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM (
			SELECT a.id
			FROM accounts a
			LEFT JOIN transaction_ledger l ON l.account_id = a.id
			GROUP BY a.id, a.balance_cents
			HAVING a.balance_cents <> COALESCE(SUM(l.amount_cents), 0)
		) t`).Scan(&s.DriftAccounts); err != nil {
		return nil, err
	}
	return s, nil
}
