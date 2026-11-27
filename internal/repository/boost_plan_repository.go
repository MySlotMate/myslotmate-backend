package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"myslotmate-backend/internal/models"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Paid-budget Boost and Premium Hosts storage (PRD 2026-11-26). Methods hang
// off BoostRepository so they share its transactions.

/* ---- settings ---- */

func (r *BoostRepository) Settings(ctx context.Context) (*models.BoostSettings, error) {
	s := &models.BoostSettings{}
	err := r.db.QueryRowContext(ctx, `
		SELECT min_daily_budget_cents, budget_step_cents, min_days, payment_window_days, budget_gst_bps,
		       budget_required, creative_image_max_mb, creative_video_max_mb, creative_other_max_mb, report_max_mb,
		       updated_at
		  FROM boost_settings WHERE id`).
		Scan(&s.MinDailyBudgetCents, &s.BudgetStepCents, &s.MinDays, &s.PaymentWindowDays, &s.BudgetGSTBps,
			&s.BudgetRequired, &s.CreativeImageMaxMB, &s.CreativeVideoMaxMB, &s.CreativeOtherMaxMB, &s.ReportMaxMB,
			&s.UpdatedAt)
	if err == sql.ErrNoRows {
		return &models.BoostSettings{MinDailyBudgetCents: 50000, BudgetStepCents: 10000, MinDays: 3,
			PaymentWindowDays: 7, BudgetGSTBps: 1800, CreativeImageMaxMB: 20, CreativeVideoMaxMB: 200,
			CreativeOtherMaxMB: 50, ReportMaxMB: 50}, nil
	}
	return s, err
}

func (r *BoostRepository) SaveSettings(ctx context.Context, s *models.BoostSettings) error {
	return r.exec(ctx, `
		INSERT INTO boost_settings (id, min_daily_budget_cents, budget_step_cents, min_days, payment_window_days,
		                            budget_gst_bps, budget_required, creative_image_max_mb, creative_video_max_mb,
		                            creative_other_max_mb, report_max_mb, updated_at)
		VALUES (true, $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now())
		ON CONFLICT (id) DO UPDATE SET min_daily_budget_cents = EXCLUDED.min_daily_budget_cents,
		  budget_step_cents = EXCLUDED.budget_step_cents, min_days = EXCLUDED.min_days,
		  payment_window_days = EXCLUDED.payment_window_days, budget_gst_bps = EXCLUDED.budget_gst_bps,
		  budget_required = EXCLUDED.budget_required, creative_image_max_mb = EXCLUDED.creative_image_max_mb,
		  creative_video_max_mb = EXCLUDED.creative_video_max_mb, creative_other_max_mb = EXCLUDED.creative_other_max_mb,
		  report_max_mb = EXCLUDED.report_max_mb, updated_at = now()`,
		s.MinDailyBudgetCents, s.BudgetStepCents, s.MinDays, s.PaymentWindowDays, s.BudgetGSTBps, s.BudgetRequired,
		s.CreativeImageMaxMB, s.CreativeVideoMaxMB, s.CreativeOtherMaxMB, s.ReportMaxMB)
}

/* ---- bands ---- */

// AllBands returns every band version, oldest first.
func (r *BoostRepository) AllBands(ctx context.Context) ([]models.BoostBand, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT version, min_price_cents, max_price_cents, guide_rate_bps, min_rate_bps, max_rate_bps
		  FROM boost_rate_bands ORDER BY version, min_price_cents`)
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

// InsertBandVersion stores bands as the next version and returns it. Run it
// in a transaction: the version number is read then written.
func (r *BoostRepository) InsertBandVersion(ctx context.Context, bands []models.BoostBand) (int, error) {
	if _, err := r.db.ExecContext(ctx, `LOCK TABLE boost_rate_bands IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return 0, err
	}
	var v int
	if err := r.db.QueryRowContext(ctx, `SELECT GREATEST(COALESCE(MAX(version), 0) + 1, 2) FROM boost_rate_bands`).Scan(&v); err != nil {
		return 0, err
	}
	for _, b := range bands {
		if err := r.exec(ctx, `
			INSERT INTO boost_rate_bands (version, min_price_cents, max_price_cents, guide_rate_bps, min_rate_bps, max_rate_bps)
			VALUES ($1, $2, $3, $4, $5, $6)`, v, b.MinPriceCents, b.MaxPriceCents, b.GuideRateBps, b.MinRateBps, b.MaxRateBps); err != nil {
			return 0, err
		}
	}
	return v, nil
}

/* ---- budget plan ---- */

// SetBudgetPlan stores the budget plan and its totals (before any payment).
func (r *BoostRepository) SetBudgetPlan(ctx context.Context, id uuid.UUID, daily int64, from, until string, total, gst int64) error {
	return r.exec(ctx, `UPDATE event_boosts SET daily_budget_cents = $2, budget_from = $3, budget_until = $4,
		budget_total_cents = $5, budget_gst_cents = $6, updated_at = now() WHERE id = $1`, id, daily, from, until, total, gst)
}

func (r *BoostRepository) SetBudgetStatus(ctx context.Context, id uuid.UUID, status string) error {
	return r.exec(ctx, `UPDATE event_boosts SET budget_status = $2, updated_at = now() WHERE id = $1`, id, status)
}

// ApplyTopUp adds a paid top-up to the plan: more days (the end moves) or a
// higher daily budget.
func (r *BoostRepository) ApplyTopUp(ctx context.Context, id uuid.UUID, amount, gst int64, daysAdded int, newDaily *int64) error {
	return r.exec(ctx, `UPDATE event_boosts SET budget_total_cents = budget_total_cents + $2,
		budget_gst_cents = budget_gst_cents + $3, budget_until = budget_until + $4::int,
		daily_budget_cents = COALESCE($5, daily_budget_cents),
		live_until = CASE WHEN $4::int > 0 AND live_until IS NOT NULL THEN live_until + make_interval(days => $4::int) ELSE live_until END,
		updated_at = now() WHERE id = $1`, id, amount, gst, daysAdded, newDaily)
}

// SetCancelledFor cancels a Boost that never went live, with a reason.
func (r *BoostRepository) SetCancelledFor(ctx context.Context, id uuid.UUID, reason string) error {
	return r.exec(ctx, `UPDATE event_boosts SET status = 'cancelled', ended_reason = $2, updated_at = now() WHERE id = $1`, id, reason)
}

// SetPricingModel moves a Boost to another pricing model (an open
// no_upfront request re-proposed with a budget plan).
func (r *BoostRepository) SetPricingModel(ctx context.Context, id uuid.UUID, model, budgetStatus string) error {
	return r.exec(ctx, `UPDATE event_boosts SET pricing_model = $2, budget_status = $3, updated_at = now() WHERE id = $1`, id, model, budgetStatus)
}

// LogOnce appends action to the Boost's log unless it is already there, and
// reports whether it did (reminders and summaries go out once).
func (r *BoostRepository) LogOnce(ctx context.Context, boostID uuid.UUID, action string) (bool, error) {
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO event_boost_log (boost_id, actor_kind, action)
		SELECT $1, 'system', $2 WHERE NOT EXISTS (SELECT 1 FROM event_boost_log WHERE boost_id = $1 AND action = $2)`, boostID, action)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ReminderCandidates are budget Boosts a reminder or summary may be due for.
type ReminderCandidates struct {
	Unpaid      []uuid.UUID // accepted, unpaid, still within the window
	NoCreatives []uuid.UUID // planned start within 2 days, no creatives yet
	Live        []uuid.UUID // live for a week or more
}

func (r *BoostRepository) ReminderCandidates(ctx context.Context, now time.Time) (*ReminderCandidates, error) {
	out := &ReminderCandidates{}
	for _, q := range []struct {
		dst *[]uuid.UUID
		sql string
	}{
		{&out.Unpaid, `SELECT id FROM event_boosts WHERE pricing_model = 'budget' AND status = 'quote_ready'
			AND budget_status = 'unpaid' AND accepted_at IS NOT NULL AND accepted_at <= $1::timestamptz - interval '2 days'
			AND payment_due_by > $1::timestamptz`},
		{&out.NoCreatives, `SELECT b.id FROM event_boosts b WHERE b.pricing_model = 'budget'
			AND b.status IN ('under_review', 'quote_ready') AND b.budget_from IS NOT NULL
			AND b.budget_from BETWEEN ($1::timestamptz AT TIME ZONE 'Asia/Kolkata')::date AND ($1::timestamptz AT TIME ZONE 'Asia/Kolkata')::date + 2
			AND NOT EXISTS (SELECT 1 FROM boost_creatives c WHERE c.boost_id = b.id)`},
		{&out.Live, `SELECT id FROM event_boosts WHERE pricing_model = 'budget' AND status = 'live'
			AND live_from <= $1::timestamptz - interval '7 days'`},
	} {
		rows, err := r.db.QueryContext(ctx, q.sql, now)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			*q.dst = append(*q.dst, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (r *BoostRepository) SetChannels(ctx context.Context, id uuid.UUID, channels []string) error {
	return r.exec(ctx, `UPDATE event_boosts SET channels = $2, updated_at = now() WHERE id = $1`, id, pq.Array(channels))
}

func (r *BoostRepository) SetClosed(ctx context.Context, id uuid.UUID, by string) error {
	return r.exec(ctx, `UPDATE event_boosts SET closed_at = now(), closed_by = $2, updated_at = now() WHERE id = $1`, id, by)
}

// DueForPayment lists budget Boosts accepted but unpaid past their due time.
func (r *BoostRepository) DueForPayment(ctx context.Context, now time.Time) ([]uuid.UUID, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id FROM event_boosts
		 WHERE status = 'quote_ready' AND budget_status = 'unpaid' AND accepted_at IS NOT NULL
		   AND payment_due_by IS NOT NULL AND payment_due_by <= $1`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

/* ---- payments ---- */

const boostPaymentCols = `id, boost_id, kind, amount_cents, gst_cents, days_added, new_daily_budget_cents, method,
	razorpay_order_id, razorpay_payment_id, razorpay_link_id, link_url, offline_reference,
	to_char(offline_paid_on, 'YYYY-MM-DD'), recorded_by, status, reverse_reason, invoice_id, created_at, paid_at`

func scanBoostPayment(row interface{ Scan(...any) error }) (*models.BoostPayment, error) {
	p := &models.BoostPayment{}
	err := row.Scan(&p.ID, &p.BoostID, &p.Kind, &p.AmountCents, &p.GSTCents, &p.DaysAdded, &p.NewDailyBudgetCents, &p.Method,
		&p.RazorpayOrderID, &p.RazorpayPaymentID, &p.RazorpayLinkID, &p.LinkURL, &p.OfflineReference,
		&p.OfflinePaidOn, &p.RecordedBy, &p.Status, &p.ReverseReason, &p.InvoiceID, &p.CreatedAt, &p.PaidAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return p, err
}

func (r *BoostRepository) CreatePayment(ctx context.Context, p *models.BoostPayment) error {
	return r.exec(ctx, `
		INSERT INTO boost_payments (id, boost_id, kind, amount_cents, gst_cents, days_added, new_daily_budget_cents, method,
		                            razorpay_order_id, razorpay_link_id, link_url, offline_reference, offline_paid_on,
		                            recorded_by, status, paid_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
		p.ID, p.BoostID, p.Kind, p.AmountCents, p.GSTCents, p.DaysAdded, p.NewDailyBudgetCents, p.Method,
		p.RazorpayOrderID, p.RazorpayLinkID, p.LinkURL, p.OfflineReference, p.OfflinePaidOn, p.RecordedBy, p.Status, p.PaidAt)
}

func (r *BoostRepository) GetPayment(ctx context.Context, id uuid.UUID) (*models.BoostPayment, error) {
	return scanBoostPayment(r.db.QueryRowContext(ctx, `SELECT `+boostPaymentCols+` FROM boost_payments WHERE id = $1`, id))
}

func (r *BoostRepository) GetPaymentForUpdate(ctx context.Context, id uuid.UUID) (*models.BoostPayment, error) {
	return scanBoostPayment(r.db.QueryRowContext(ctx, `SELECT `+boostPaymentCols+` FROM boost_payments WHERE id = $1 FOR UPDATE`, id))
}

// PaymentIDByGateway finds a Boost payment by Razorpay order or payment-link
// id ("" skips that one). Nil when the order isn't a Boost payment.
func (r *BoostRepository) PaymentIDByGateway(ctx context.Context, orderID, linkID string) (*uuid.UUID, error) {
	var id uuid.UUID
	err := r.db.QueryRowContext(ctx, `
		SELECT id FROM boost_payments
		 WHERE ($1 <> '' AND razorpay_order_id = $1) OR ($2 <> '' AND razorpay_link_id = $2)
		 LIMIT 1`, orderID, linkID).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &id, err
}

func (r *BoostRepository) ListPayments(ctx context.Context, boostID uuid.UUID) ([]*models.BoostPayment, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+boostPaymentCols+` FROM boost_payments WHERE boost_id = $1 ORDER BY created_at`, boostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*models.BoostPayment{}
	for rows.Next() {
		p, err := scanBoostPayment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// MarkPaymentPaid moves a payment to paid: from created, or from expired /
// failed when the money arrives late. False when it was already paid or
// reversed.
func (r *BoostRepository) MarkPaymentPaid(ctx context.Context, id uuid.UUID, orderID, paymentID string) (bool, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE boost_payments SET status = 'paid', paid_at = now(), updated_at = now(),
		       razorpay_order_id = COALESCE(razorpay_order_id, NULLIF($2, '')),
		       razorpay_payment_id = COALESCE(NULLIF($3, ''), razorpay_payment_id)
		 WHERE id = $1 AND status IN ('created', 'expired', 'failed')`, id, orderID, paymentID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (r *BoostRepository) SetPaymentStatus(ctx context.Context, id uuid.UUID, status string, reason *string) error {
	return r.exec(ctx, `UPDATE boost_payments SET status = $2, reverse_reason = COALESCE($3, reverse_reason), updated_at = now()
		WHERE id = $1`, id, status, reason)
}

// ExpireOpenPayments marks a Boost's unpaid orders and links expired.
func (r *BoostRepository) ExpireOpenPayments(ctx context.Context, boostID uuid.UUID) error {
	return r.exec(ctx, `UPDATE boost_payments SET status = 'expired', updated_at = now() WHERE boost_id = $1 AND status = 'created'`, boostID)
}

func (r *BoostRepository) SetPaymentInvoice(ctx context.Context, id, invoiceID uuid.UUID) error {
	return r.exec(ctx, `UPDATE boost_payments SET invoice_id = $2, updated_at = now() WHERE id = $1`, id, invoiceID)
}

// UninvoicedPayments lists paid payments that have no budget invoice yet.
func (r *BoostRepository) UninvoicedPayments(ctx context.Context, limit int) ([]uuid.UUID, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id FROM boost_payments WHERE status = 'paid' AND invoice_id IS NULL
		ORDER BY paid_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// BudgetInvoice is one budget invoice or credit note, in the website's shape.
type BudgetInvoice struct {
	ID          uuid.UUID `json:"id"`
	Kind        string    `json:"kind"`
	Number      string    `json:"number"`
	AmountCents int64     `json:"amount_cents"`
	GSTCents    int64     `json:"gst_cents"`
	IssuedAt    time.Time `json:"issued_at"`
	PDFURL      *string   `json:"pdf_url"`
}

// BudgetInvoices lists a Boost's budget invoices and credit notes.
func (r *BoostRepository) BudgetInvoices(ctx context.Context, boostID uuid.UUID) ([]BudgetInvoice, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT i.id, i.kind, i.number, i.taxable_cents, i.total_cents - i.taxable_cents, i.issued_at
		  FROM invoices i JOIN boost_payments p ON p.id = i.source_id
		 WHERE i.source_type = 'boost_budget' AND p.boost_id = $1
		 ORDER BY i.issued_at, i.number`, boostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BudgetInvoice{}
	for rows.Next() {
		var x BudgetInvoice
		if err := rows.Scan(&x.ID, &x.Kind, &x.Number, &x.AmountCents, &x.GSTCents, &x.IssuedAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

/* ---- spend ---- */

func (r *BoostRepository) UpsertSpend(ctx context.Context, boostID uuid.UUID, e models.BoostSpendEntry) error {
	return r.exec(ctx, `
		INSERT INTO boost_spend_entries (boost_id, day, channel, spend_cents, impressions, reach, clicks, note, entered_by, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now())
		ON CONFLICT (boost_id, day, channel) DO UPDATE SET spend_cents = EXCLUDED.spend_cents,
		  impressions = EXCLUDED.impressions, reach = EXCLUDED.reach, clicks = EXCLUDED.clicks,
		  note = EXCLUDED.note, entered_by = EXCLUDED.entered_by, updated_at = now()`,
		boostID, e.Day, e.Channel, e.SpendCents, e.Impressions, e.Reach, e.Clicks, e.Note, e.EnteredBy)
}

// RefreshSpent recomputes the Boost's spend and reach/clicks totals from its
// log and returns the spend.
func (r *BoostRepository) RefreshSpent(ctx context.Context, boostID uuid.UUID) (int64, error) {
	var spent int64
	err := r.db.QueryRowContext(ctx, `
		UPDATE event_boosts b SET spent_cents = t.spend, reach = t.reach, clicks = t.clicks, updated_at = now()
		  FROM (SELECT COALESCE(SUM(spend_cents), 0) AS spend, COALESCE(SUM(reach), 0) AS reach,
		               COALESCE(SUM(clicks), 0) AS clicks
		          FROM boost_spend_entries WHERE boost_id = $1) t
		 WHERE b.id = $1 RETURNING b.spent_cents`, boostID).Scan(&spent)
	return spent, err
}

func (r *BoostRepository) SpendEntries(ctx context.Context, boostID uuid.UUID) ([]models.BoostSpendEntry, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT to_char(day, 'YYYY-MM-DD'), channel, spend_cents, impressions, reach, clicks, note, entered_by, updated_at
		  FROM boost_spend_entries WHERE boost_id = $1 ORDER BY day, channel`, boostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.BoostSpendEntry{}
	for rows.Next() {
		var e models.BoostSpendEntry
		if err := rows.Scan(&e.Day, &e.Channel, &e.SpendCents, &e.Impressions, &e.Reach, &e.Clicks, &e.Note, &e.EnteredBy, &e.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// TicketsPerDay counts tickets sold with this Boost per IST day, net of refunds.
func (r *BoostRepository) TicketsPerDay(ctx context.Context, boostID uuid.UUID) (map[string]int, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT to_char((created_at AT TIME ZONE 'Asia/Kolkata')::date, 'YYYY-MM-DD'), COALESCE(SUM(quantity), 0)
		  FROM bookings WHERE boost_id = $1 AND status IN ('pending', 'confirmed') GROUP BY 1`, boostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var d string
		var n int
		if err := rows.Scan(&d, &n); err != nil {
			return nil, err
		}
		out[d] = n
	}
	return out, rows.Err()
}

/* ---- creatives ---- */

const creativeCols = `id, boost_id, type, files, caption, channels, uploaded_by_kind, uploaded_by, status, status_note,
	replaces_id, created_at`

func scanCreative(row interface{ Scan(...any) error }) (*models.BoostCreative, error) {
	c := &models.BoostCreative{}
	var files []byte
	err := row.Scan(&c.ID, &c.BoostID, &c.Type, &files, &c.Caption, pq.Array(&c.Channels), &c.UploadedByKind, &c.UploadedBy,
		&c.Status, &c.StatusNote, &c.ReplacesID, &c.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(files, &c.Files); err != nil {
		return nil, err
	}
	if c.Channels == nil {
		c.Channels = []string{}
	}
	return c, nil
}

func (r *BoostRepository) CreateCreative(ctx context.Context, c *models.BoostCreative) error {
	files, err := json.Marshal(c.Files)
	if err != nil {
		return err
	}
	return r.exec(ctx, `
		INSERT INTO boost_creatives (id, boost_id, type, files, caption, channels, uploaded_by_kind, uploaded_by, status, replaces_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		c.ID, c.BoostID, c.Type, files, c.Caption, pq.Array(c.Channels), c.UploadedByKind, c.UploadedBy, c.Status, c.ReplacesID, c.CreatedAt)
}

func (r *BoostRepository) GetCreativeForUpdate(ctx context.Context, boostID, id uuid.UUID) (*models.BoostCreative, error) {
	return scanCreative(r.db.QueryRowContext(ctx, `SELECT `+creativeCols+` FROM boost_creatives WHERE id = $1 AND boost_id = $2 FOR UPDATE`, id, boostID))
}

func (r *BoostRepository) GetCreative(ctx context.Context, id uuid.UUID) (*models.BoostCreative, error) {
	return scanCreative(r.db.QueryRowContext(ctx, `SELECT `+creativeCols+` FROM boost_creatives WHERE id = $1`, id))
}

func (r *BoostRepository) ListCreatives(ctx context.Context, boostID uuid.UUID) ([]*models.BoostCreative, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+creativeCols+` FROM boost_creatives WHERE boost_id = $1 ORDER BY created_at DESC`, boostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*models.BoostCreative{}
	for rows.Next() {
		c, err := scanCreative(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *BoostRepository) SetCreativeStatus(ctx context.Context, id uuid.UUID, status string, note *string) error {
	return r.exec(ctx, `UPDATE boost_creatives SET status = $2, status_note = $3, updated_at = now() WHERE id = $1`, id, status, note)
}

func (r *BoostRepository) DeleteCreative(ctx context.Context, id uuid.UUID) error {
	return r.exec(ctx, `DELETE FROM boost_creatives WHERE id = $1`, id)
}

func (r *BoostRepository) HasCreativeInUse(ctx context.Context, boostID uuid.UUID) (bool, error) {
	var ok bool
	err := r.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM boost_creatives WHERE boost_id = $1 AND status = 'in_use')`, boostID).Scan(&ok)
	return ok, err
}

/* ---- reports ---- */

const reportCols = `id, boost_id, kind, title, to_char(covers_from, 'YYYY-MM-DD'), to_char(covers_until, 'YYYY-MM-DD'), note,
	file_key, file_name, mime_type, size, uploaded_by, replaces_id, replaced_at, created_at`

func scanReport(row interface{ Scan(...any) error }) (*models.BoostReport, error) {
	x := &models.BoostReport{}
	err := row.Scan(&x.ID, &x.BoostID, &x.Kind, &x.Title, &x.CoversFrom, &x.CoversUntil, &x.Note,
		&x.File.Key, &x.File.Name, &x.File.MimeType, &x.File.Size, &x.UploadedBy, &x.ReplacesID, &x.ReplacedAt, &x.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return x, err
}

func (r *BoostRepository) CreateReport(ctx context.Context, x *models.BoostReport) error {
	return r.exec(ctx, `
		INSERT INTO boost_reports (id, boost_id, kind, title, covers_from, covers_until, note, file_key, file_name, mime_type,
		                           size, uploaded_by, replaces_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		x.ID, x.BoostID, x.Kind, x.Title, x.CoversFrom, x.CoversUntil, x.Note, x.File.Key, x.File.Name, x.File.MimeType,
		x.File.Size, x.UploadedBy, x.ReplacesID, x.CreatedAt)
}

func (r *BoostRepository) GetReportForUpdate(ctx context.Context, boostID, id uuid.UUID) (*models.BoostReport, error) {
	return scanReport(r.db.QueryRowContext(ctx, `SELECT `+reportCols+` FROM boost_reports WHERE id = $1 AND boost_id = $2 FOR UPDATE`, id, boostID))
}

func (r *BoostRepository) MarkReportReplaced(ctx context.Context, id uuid.UUID) error {
	return r.exec(ctx, `UPDATE boost_reports SET replaced_at = now() WHERE id = $1`, id)
}

// ListReports returns a Boost's reports, newest first; withHistory adds the
// replaced ones.
func (r *BoostRepository) ListReports(ctx context.Context, boostID uuid.UUID, withHistory bool) ([]*models.BoostReport, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+reportCols+` FROM boost_reports
		WHERE boost_id = $1 AND ($2 OR replaced_at IS NULL) ORDER BY created_at DESC`, boostID, withHistory)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*models.BoostReport{}
	for rows.Next() {
		x, err := scanReport(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (r *BoostRepository) HasFinalReport(ctx context.Context, boostID uuid.UUID) (bool, error) {
	var ok bool
	err := r.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM boost_reports WHERE boost_id = $1 AND kind = 'final' AND replaced_at IS NULL)`, boostID).Scan(&ok)
	return ok, err
}

/* ---- Premium ---- */

const premiumCols = `m.host_id, m.status, m.applied_at, m.application_note, m.decided_at, m.decided_by, m.granted,
	m.manager_name, m.manager_contact, m.decline_reason, m.remove_reason, m.updated_at,
	trim(concat_ws(' ', h.first_name, h.last_name)), COALESCE(u.email, '')`

const premiumFrom = ` FROM premium_memberships m JOIN hosts h ON h.id = m.host_id LEFT JOIN users u ON u.id = h.user_id `

func scanPremium(row interface{ Scan(...any) error }) (*models.PremiumMembership, error) {
	m := &models.PremiumMembership{}
	err := row.Scan(&m.HostID, &m.Status, &m.AppliedAt, &m.ApplicationNote, &m.DecidedAt, &m.DecidedBy, &m.Granted,
		&m.ManagerName, &m.ManagerContact, &m.DeclineReason, &m.RemoveReason, &m.UpdatedAt, &m.HostName, &m.HostEmail)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return m, err
}

// Premium returns the host's membership, nil when they never applied.
func (r *BoostRepository) Premium(ctx context.Context, hostID uuid.UUID) (*models.PremiumMembership, error) {
	return scanPremium(r.db.QueryRowContext(ctx, `SELECT `+premiumCols+premiumFrom+` WHERE m.host_id = $1`, hostID))
}

func (r *BoostRepository) PremiumForUpdate(ctx context.Context, hostID uuid.UUID) (*models.PremiumMembership, error) {
	return scanPremium(r.db.QueryRowContext(ctx, `SELECT `+premiumCols+premiumFrom+` WHERE m.host_id = $1 FOR UPDATE OF m`, hostID))
}

// PremiumActive reports whether the host is an Active Premium host.
func (r *BoostRepository) PremiumActive(ctx context.Context, hostID uuid.UUID) (bool, error) {
	var ok bool
	err := r.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM premium_memberships WHERE host_id = $1 AND status = 'active')`, hostID).Scan(&ok)
	return ok, err
}

// SavePremium writes the whole membership row.
func (r *BoostRepository) SavePremium(ctx context.Context, m *models.PremiumMembership) error {
	return r.exec(ctx, `
		INSERT INTO premium_memberships (host_id, status, applied_at, application_note, decided_at, decided_by, granted,
		                                 manager_name, manager_contact, decline_reason, remove_reason, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, now())
		ON CONFLICT (host_id) DO UPDATE SET status = EXCLUDED.status, applied_at = EXCLUDED.applied_at,
		  application_note = EXCLUDED.application_note, decided_at = EXCLUDED.decided_at,
		  decided_by = EXCLUDED.decided_by, granted = EXCLUDED.granted, manager_name = EXCLUDED.manager_name,
		  manager_contact = EXCLUDED.manager_contact, decline_reason = EXCLUDED.decline_reason,
		  remove_reason = EXCLUDED.remove_reason, updated_at = now()`,
		m.HostID, m.Status, m.AppliedAt, m.ApplicationNote, m.DecidedAt, m.DecidedBy, m.Granted,
		m.ManagerName, m.ManagerContact, m.DeclineReason, m.RemoveReason)
}

func (r *BoostRepository) PremiumLog(ctx context.Context, hostID uuid.UUID, actorKind, actor, action string, details any) error {
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
	return r.exec(ctx, `INSERT INTO premium_membership_log (host_id, actor_kind, actor, action, details) VALUES ($1, $2, $3, $4, $5)`,
		hostID, actorKind, a, action, raw)
}

func (r *BoostRepository) PremiumLogEntries(ctx context.Context, hostID uuid.UUID) ([]models.BoostLogEntry, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT actor_kind, actor, action, details, created_at FROM premium_membership_log
		WHERE host_id = $1 ORDER BY created_at, id`, hostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.BoostLogEntry{}
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

// ListPremium lists memberships ("" = every status), newest change first.
func (r *BoostRepository) ListPremium(ctx context.Context, status string) ([]*models.PremiumMembership, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+premiumCols+premiumFrom+` WHERE ($1 = '' OR m.status = $1)
		ORDER BY m.updated_at DESC LIMIT 500`, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*models.PremiumMembership{}
	for rows.Next() {
		m, err := scanPremium(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (r *BoostRepository) CountPremium(ctx context.Context) (map[string]int, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM premium_memberships GROUP BY status`)
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
