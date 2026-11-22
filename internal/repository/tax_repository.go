package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"myslotmate-backend/internal/models"

	"github.com/google/uuid"
)

// Statutory tax storage (Marketing Boost v2 Phase 1). These live on the payout
// repository: it is already in both the booking and payout services and has
// WithTx, so the booking transaction can read the FY total under its locks.

// GetTaxSettings returns platform_settings.tax_settings; all-off when unseeded.
func (r *postgresPayoutRepository) GetTaxSettings(ctx context.Context) (*models.TaxSettings, error) {
	cfg := &models.TaxSettings{}
	raw, err := r.GetPlatformSetting(ctx, models.TaxSettingsKey)
	if err != nil || raw == nil {
		return cfg, err
	}
	if err := json.Unmarshal(raw, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// GetTaxProfile returns the host's tax profile, or nil when none is saved.
func (r *postgresPayoutRepository) GetTaxProfile(ctx context.Context, hostID uuid.UUID) (*models.TaxProfile, error) {
	p := &models.TaxProfile{}
	err := r.db.QueryRowContext(ctx, `
		SELECT host_id, entity_type, pan, pan_name, gstin, gst_state_code, updated_at
		  FROM host_tax_profiles WHERE host_id = $1`, hostID,
	).Scan(&p.HostID, &p.EntityType, &p.PAN, &p.PANName, &p.GSTIN, &p.GSTStateCode, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

// UpsertTaxProfile saves the host's tax profile. Changes apply to bookings
// confirmed after updated_at.
func (r *postgresPayoutRepository) UpsertTaxProfile(ctx context.Context, p *models.TaxProfile) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO host_tax_profiles (host_id, entity_type, pan, pan_name, gstin, gst_state_code, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, now())
		ON CONFLICT (host_id) DO UPDATE SET
		  entity_type = EXCLUDED.entity_type, pan = EXCLUDED.pan, pan_name = EXCLUDED.pan_name,
		  gstin = EXCLUDED.gstin, gst_state_code = EXCLUDED.gst_state_code, updated_at = now()`,
		p.HostID, p.EntityType, p.PAN, p.PANName, p.GSTIN, p.GSTStateCode)
	return err
}

// taxSales is every taxable sale: ticket bookings and monthly passes, with the
// same tax columns. live = the host kept the sale (booking pending/confirmed —
// a no-refund guest cancel stays confirmed — or pass active/expired);
// returned = it was refunded or cancelled. Alias it b so the queries below
// read as they did when they covered bookings only.
const taxSales = `(
	SELECT event_id, created_at, ticket_cents, tcs_cents, tcs_interstate, tds_cents,
	       status IN ('pending', 'confirmed') AS live, status IN ('refunded', 'cancelled') AS returned
	  FROM bookings
	UNION ALL
	SELECT event_id, created_at, ticket_cents, tcs_cents, tcs_interstate, tds_cents,
	       status IN ('active', 'expired') AS live, status = 'refunded' AS returned
	  FROM user_passes
)`

// FYGrossTicketCents is the host's ticket value on live sales (bookings and
// passes) created since fyStart. Refunded and cancelled sales drop out.
func (r *postgresPayoutRepository) FYGrossTicketCents(ctx context.Context, hostID uuid.UUID, fyStart time.Time) (int64, error) {
	var total int64
	err := r.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(b.ticket_cents), 0)::BIGINT
		  FROM `+taxSales+` b JOIN events e ON e.id = b.event_id
		 WHERE e.host_id = $1 AND b.live AND b.created_at >= $2`,
		hostID, fyStart,
	).Scan(&total)
	return total, err
}

// HostTaxTotals sums the host's stamped TCS/TDS on live sales since fyStart.
func (r *postgresPayoutRepository) HostTaxTotals(ctx context.Context, hostID uuid.UUID, fyStart time.Time) (tcs, tds int64, err error) {
	err = r.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(b.tcs_cents), 0)::BIGINT, COALESCE(SUM(b.tds_cents), 0)::BIGINT
		  FROM `+taxSales+` b JOIN events e ON e.id = b.event_id
		 WHERE e.host_id = $1 AND b.live AND b.created_at >= $2`,
		hostID, fyStart,
	).Scan(&tcs, &tds)
	return tcs, tds, err
}

// LockHost takes a row lock on the host for the rest of the transaction.
// Lock order everywhere: event row first, then host row.
func (r *postgresPayoutRepository) LockHost(ctx context.Context, hostID uuid.UUID) error {
	var one int
	return r.db.QueryRowContext(ctx, `SELECT 1 FROM hosts WHERE id = $1 FOR UPDATE`, hostID).Scan(&one)
}

// MonthTax is one IST calendar month of a host's stamped tax.
type MonthTax struct {
	Month    string `json:"month"` // "2026-04"
	TCSCents int64  `json:"tcs_cents"`
	TDSCents int64  `json:"tds_cents"`
}

// HostTaxByMonth sums the host's stamped TCS/TDS per IST month in [from, to).
func (r *postgresPayoutRepository) HostTaxByMonth(ctx context.Context, hostID uuid.UUID, from, to time.Time) ([]MonthTax, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT to_char(b.created_at AT TIME ZONE 'Asia/Kolkata', 'YYYY-MM') AS month,
		       COALESCE(SUM(b.tcs_cents), 0)::BIGINT, COALESCE(SUM(b.tds_cents), 0)::BIGINT
		  FROM `+taxSales+` b JOIN events e ON e.id = b.event_id
		 WHERE e.host_id = $1 AND b.live
		   AND b.created_at >= $2 AND b.created_at < $3
		 GROUP BY 1 ORDER BY 1`, hostID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MonthTax
	for rows.Next() {
		var m MonthTax
		if err := rows.Scan(&m.Month, &m.TCSCents, &m.TDSCents); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// TaxReportRow is one host's line in the monthly finance tax report.
type TaxReportRow struct {
	HostID            uuid.UUID
	HostName          string
	PAN, GSTIN        *string
	GSTStateCode      *string
	EntityType        *string
	GrossCents        int64 // ticket value of bookings created in the month
	ReturnsCents      int64 // of which refunded / cancelled
	TCSIntraCents     int64 // on live bookings, venue in the GSTIN's state (or unknown)
	TCSInterCents     int64
	TDSCents          int64
	VenueStateUnknown bool // some TCS was booked with no venue state on the event
}

// TaxReportRows aggregates stamped tax per host for sales (bookings and
// passes) created in [from, to).
func (r *postgresPayoutRepository) TaxReportRows(ctx context.Context, from, to time.Time) ([]TaxReportRow, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT h.id, trim(concat_ws(' ', h.first_name, h.last_name)), p.pan, p.gstin, p.gst_state_code, p.entity_type,
		       COALESCE(SUM(b.ticket_cents), 0)::BIGINT,
		       COALESCE(SUM(b.ticket_cents) FILTER (WHERE b.returned), 0)::BIGINT,
		       COALESCE(SUM(b.tcs_cents) FILTER (WHERE b.live AND NOT b.tcs_interstate), 0)::BIGINT,
		       COALESCE(SUM(b.tcs_cents) FILTER (WHERE b.live AND b.tcs_interstate), 0)::BIGINT,
		       COALESCE(SUM(b.tds_cents) FILTER (WHERE b.live), 0)::BIGINT,
		       COALESCE(bool_or(b.tcs_cents > 0 AND e.state_code IS NULL), false)
		  FROM `+taxSales+` b
		  JOIN events e ON e.id = b.event_id
		  JOIN hosts  h ON h.id = e.host_id
		  LEFT JOIN host_tax_profiles p ON p.host_id = h.id
		 WHERE b.created_at >= $1 AND b.created_at < $2 AND COALESCE(b.ticket_cents, 0) > 0
		 GROUP BY h.id, h.first_name, h.last_name, p.pan, p.gstin, p.gst_state_code, p.entity_type
		 ORDER BY 2`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaxReportRow
	for rows.Next() {
		var t TaxReportRow
		if err := rows.Scan(&t.HostID, &t.HostName, &t.PAN, &t.GSTIN, &t.GSTStateCode, &t.EntityType,
			&t.GrossCents, &t.ReturnsCents, &t.TCSIntraCents, &t.TCSInterCents, &t.TDSCents, &t.VenueStateUnknown); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TaxLiability returns all TCS+TDS stamped on live sales and all tax
// deposits finance has logged, both all-time.
func (r *postgresPayoutRepository) TaxLiability(ctx context.Context) (stamped, deposited int64, err error) {
	err = r.db.QueryRowContext(ctx, `
		SELECT (SELECT COALESCE(SUM(tcs_cents + tds_cents), 0) FROM `+taxSales+` s WHERE live)::BIGINT,
		       (SELECT COALESCE(SUM(amount_cents), 0) FROM tax_deposits)::BIGINT`,
	).Scan(&stamped, &deposited)
	return stamped, deposited, err
}

// TaxDeposit is a TCS/TDS payment to the government, logged by finance.
type TaxDeposit struct {
	ID          uuid.UUID  `json:"id"`
	Kind        string     `json:"kind"`   // tcs | tds
	Period      string     `json:"period"` // "2026-11"
	AmountCents int64      `json:"amount_cents"`
	ChallanRef  *string    `json:"challan_ref,omitempty"`
	DepositedAt time.Time  `json:"deposited_at"`
	CreatedBy   *uuid.UUID `json:"created_by,omitempty"`
}

func (r *postgresPayoutRepository) CreateTaxDeposit(ctx context.Context, d *TaxDeposit) error {
	return r.db.QueryRowContext(ctx, `
		INSERT INTO tax_deposits (kind, period, amount_cents, challan_ref, deposited_at, created_by)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		d.Kind, d.Period, d.AmountCents, d.ChallanRef, d.DepositedAt, d.CreatedBy,
	).Scan(&d.ID)
}

// EventEarnings is one event's line in the host's earnings breakdown (PRD
// Earnings screen: gross sales, deductions, net). Live bookings only.
type EventEarnings struct {
	EventID     uuid.UUID `json:"event_id"`
	EventTitle  string    `json:"event_title"`
	Bookings    int64     `json:"bookings"`
	GrossCents  int64     `json:"gross_cents"` // ticket sales after coupons, before any deduction
	FeeCents    int64     `json:"fee_cents"`   // platform commission taken from the host's share (0 when the guest pays the fee)
	TCSCents    int64     `json:"tcs_cents"`
	TDSCents    int64     `json:"tds_cents"`
	EarnedCents int64     `json:"earned_cents"` // what the host is paid for this event
}

// HostEventEarnings groups the host's live bookings by event, newest event first.
func (r *postgresPayoutRepository) HostEventEarnings(ctx context.Context, hostID uuid.UUID) ([]EventEarnings, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT e.id, e.title, COUNT(*)::BIGINT,
		       COALESCE(SUM(COALESCE(b.ticket_cents, b.amount_cents)), 0)::BIGINT,
		       COALESCE(SUM(COALESCE(b.ticket_cents, b.amount_cents) - COALESCE(b.net_earning_cents, 0)), 0)::BIGINT,
		       COALESCE(SUM(b.tcs_cents), 0)::BIGINT,
		       COALESCE(SUM(b.tds_cents), 0)::BIGINT,
		       COALESCE(SUM(COALESCE(b.host_payout_cents, b.net_earning_cents)), 0)::BIGINT
		  FROM bookings b JOIN events e ON e.id = b.event_id
		 WHERE e.host_id = $1 AND b.status = 'confirmed' AND COALESCE(b.amount_cents, 0) > 0
		 GROUP BY e.id, e.title
		 ORDER BY MAX(b.occurrence_date) DESC`, hostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EventEarnings{}
	for rows.Next() {
		var x EventEarnings
		if err := rows.Scan(&x.EventID, &x.EventTitle, &x.Bookings, &x.GrossCents, &x.FeeCents, &x.TCSCents, &x.TDSCents, &x.EarnedCents); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
