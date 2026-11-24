package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"myslotmate-backend/internal/models"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// BillingRepository stores billing settings, invoice numbers, invoices and
// credit notes. Bind it to the sale's transaction with WithTx when issuing, so
// the invoice commits or rolls back with the money.
type BillingRepository struct{ db DBTX }

func NewBillingRepository(db DBTX) *BillingRepository { return &BillingRepository{db: db} }

func (r *BillingRepository) WithTx(tx DBTX) *BillingRepository { return &BillingRepository{db: tx} }

func (r *BillingRepository) GetSettings(ctx context.Context) (*models.BillingSettings, error) {
	s := &models.BillingSettings{}
	err := r.db.QueryRowContext(ctx, `
		SELECT enabled, legal_name, trade_name, gstin, pan, address, state_code, email,
		       sac_code, gst_rate_bps, invoice_prefix, credit_note_prefix,
		       commission_invoice_prefix, commission_sac_code, updated_at
		  FROM billing_settings WHERE id`).
		Scan(&s.Enabled, &s.LegalName, &s.TradeName, &s.GSTIN, &s.PAN, &s.Address, &s.StateCode, &s.Email,
			&s.SACCode, &s.GSTRateBps, &s.InvoicePrefix, &s.CreditNotePrefix,
			&s.CommissionInvoicePrefix, &s.CommissionSACCode, &s.UpdatedAt)
	if err == sql.ErrNoRows {
		return &models.BillingSettings{GSTRateBps: 1800, InvoicePrefix: "F", CreditNotePrefix: "C", CommissionInvoicePrefix: "M"}, nil
	}
	return s, err
}

func (r *BillingRepository) SaveSettings(ctx context.Context, s *models.BillingSettings) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO billing_settings (id, enabled, legal_name, trade_name, gstin, pan, address, state_code, email,
		                              sac_code, gst_rate_bps, invoice_prefix, credit_note_prefix,
		                              commission_invoice_prefix, commission_sac_code, updated_at)
		VALUES (true, $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, now())
		ON CONFLICT (id) DO UPDATE SET
		  enabled = EXCLUDED.enabled, legal_name = EXCLUDED.legal_name, trade_name = EXCLUDED.trade_name,
		  gstin = EXCLUDED.gstin, pan = EXCLUDED.pan, address = EXCLUDED.address,
		  state_code = EXCLUDED.state_code, email = EXCLUDED.email, sac_code = EXCLUDED.sac_code,
		  gst_rate_bps = EXCLUDED.gst_rate_bps, invoice_prefix = EXCLUDED.invoice_prefix,
		  credit_note_prefix = EXCLUDED.credit_note_prefix,
		  commission_invoice_prefix = EXCLUDED.commission_invoice_prefix,
		  commission_sac_code = EXCLUDED.commission_sac_code, updated_at = now()`,
		s.Enabled, s.LegalName, s.TradeName, s.GSTIN, s.PAN, s.Address, s.StateCode, s.Email,
		s.SACCode, s.GSTRateBps, s.InvoicePrefix, s.CreditNotePrefix,
		s.CommissionInvoicePrefix, s.CommissionSACCode)
	return err
}

// NextNumber takes the next number in a series. The upsert locks the series row
// until the transaction ends, so concurrent sales queue and a rollback returns
// the number: the series stays consecutive.
func (r *BillingRepository) NextNumber(ctx context.Context, prefix, fy string) (int64, error) {
	var n int64
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO invoice_series (prefix, fy, last_number) VALUES ($1, $2, 1)
		ON CONFLICT (prefix, fy) DO UPDATE SET last_number = invoice_series.last_number + 1
		RETURNING last_number`, prefix, fy).Scan(&n)
	return n, err
}

func (r *BillingRepository) Create(ctx context.Context, inv *models.Invoice) error {
	supplier, err := json.Marshal(inv.Supplier)
	if err != nil {
		return err
	}
	if _, err := r.db.ExecContext(ctx, `
		INSERT INTO invoices (id, kind, number, fy, issued_at, source_type, source_id, event_id, event_title,
		                      original_invoice_id, user_id, buyer_name, buyer_email, buyer_phone, place_of_supply,
		                      supplier, taxable_cents, cgst_cents, sgst_cents, igst_cents, total_cents,
		                      buyer_gstin, buyer_state_code, period, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24, now())`,
		inv.ID, inv.Kind, inv.Number, inv.FY, inv.IssuedAt, inv.SourceType, inv.SourceID, inv.EventID, inv.EventTitle,
		inv.OriginalInvoiceID, inv.UserID, inv.BuyerName, inv.BuyerEmail, inv.BuyerPhone, inv.PlaceOfSupply,
		supplier, inv.TaxableCents, inv.CGSTCents, inv.SGSTCents, inv.IGSTCents, inv.TotalCents,
		inv.BuyerGSTIN, inv.BuyerStateCode, inv.Period); err != nil {
		return fmt.Errorf("insert invoice: %w", err)
	}
	for _, l := range inv.Lines {
		if _, err := r.db.ExecContext(ctx, `
			INSERT INTO invoice_lines (id, invoice_id, position, description, sac_code, quantity, taxable_cents,
			                           gst_rate_bps, cgst_cents, sgst_cents, igst_cents, total_cents)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			l.ID, inv.ID, l.Position, l.Description, l.SACCode, l.Quantity, l.TaxableCents,
			l.GSTRateBps, l.CGSTCents, l.SGSTCents, l.IGSTCents, l.TotalCents); err != nil {
			return fmt.Errorf("insert invoice line: %w", err)
		}
	}
	return nil
}

const invoiceCols = `i.id, i.kind, i.number, i.fy, i.issued_at, i.source_type, i.source_id, i.event_id, i.event_title,
	i.original_invoice_id, COALESCE(o.number, ''), i.user_id, i.buyer_name, i.buyer_email, i.buyer_phone,
	i.place_of_supply, i.supplier, i.taxable_cents, i.cgst_cents, i.sgst_cents, i.igst_cents, i.total_cents,
	i.pdf_key, i.created_at, i.buyer_gstin, i.buyer_state_code, i.period`

const invoiceFrom = ` FROM invoices i LEFT JOIN invoices o ON o.id = i.original_invoice_id `

func scanInvoice(row interface{ Scan(...any) error }) (*models.Invoice, error) {
	inv := &models.Invoice{}
	var supplier []byte
	if err := row.Scan(&inv.ID, &inv.Kind, &inv.Number, &inv.FY, &inv.IssuedAt, &inv.SourceType, &inv.SourceID,
		&inv.EventID, &inv.EventTitle, &inv.OriginalInvoiceID, &inv.OriginalNumber, &inv.UserID, &inv.BuyerName,
		&inv.BuyerEmail, &inv.BuyerPhone, &inv.PlaceOfSupply, &supplier, &inv.TaxableCents, &inv.CGSTCents,
		&inv.SGSTCents, &inv.IGSTCents, &inv.TotalCents, &inv.PDFKey, &inv.CreatedAt,
		&inv.BuyerGSTIN, &inv.BuyerStateCode, &inv.Period); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(supplier, &inv.Supplier); err != nil {
		return nil, err
	}
	return inv, nil
}

func (r *BillingRepository) one(ctx context.Context, where string, args ...any) (*models.Invoice, error) {
	inv, err := scanInvoice(r.db.QueryRowContext(ctx, `SELECT `+invoiceCols+invoiceFrom+where, args...))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return inv, r.loadLines(ctx, inv)
}

func (r *BillingRepository) GetByID(ctx context.Context, id uuid.UUID) (*models.Invoice, error) {
	return r.one(ctx, `WHERE i.id = $1`, id)
}

// GetBySource returns the invoice or credit note of one sale, or nil.
func (r *BillingRepository) GetBySource(ctx context.Context, sourceType string, sourceID uuid.UUID, kind string) (*models.Invoice, error) {
	return r.one(ctx, `WHERE i.source_type = $1 AND i.source_id = $2 AND i.kind = $3`, sourceType, sourceID, kind)
}

func (r *BillingRepository) loadLines(ctx context.Context, inv *models.Invoice) error {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, invoice_id, position, description, sac_code, quantity, taxable_cents, gst_rate_bps,
		       cgst_cents, sgst_cents, igst_cents, total_cents
		  FROM invoice_lines WHERE invoice_id = $1 ORDER BY position`, inv.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	inv.Lines = nil
	for rows.Next() {
		var l models.InvoiceLine
		if err := rows.Scan(&l.ID, &l.InvoiceID, &l.Position, &l.Description, &l.SACCode, &l.Quantity,
			&l.TaxableCents, &l.GSTRateBps, &l.CGSTCents, &l.SGSTCents, &l.IGSTCents, &l.TotalCents); err != nil {
			return err
		}
		inv.Lines = append(inv.Lines, l)
	}
	return rows.Err()
}

// ListBySource returns the invoice and any credit note of one sale, oldest first.
func (r *BillingRepository) ListBySource(ctx context.Context, sourceType string, sourceID uuid.UUID) ([]*models.Invoice, error) {
	return r.list(ctx, `WHERE i.source_type = $1 AND i.source_id = $2 ORDER BY i.issued_at, i.kind DESC`, sourceType, sourceID)
}

// List returns invoices and credit notes, newest first, and the total match count.
func (r *BillingRepository) List(ctx context.Context, f models.InvoiceFilter) ([]*models.Invoice, int, error) {
	var conds []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		conds = append(conds, strings.ReplaceAll(cond, "?", fmt.Sprintf("$%d", len(args))))
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		add(`(i.number ILIKE ? OR i.buyer_name ILIKE ? OR i.buyer_email ILIKE ? OR i.buyer_phone ILIKE ? OR i.event_title ILIKE ?)`,
			"%"+q+"%")
	}
	if f.Kind != "" {
		add(`i.kind = ?`, f.Kind)
	}
	if f.From != nil {
		add(`i.issued_at >= ?`, *f.From)
	}
	if f.To != nil {
		add(`i.issued_at < ?`, *f.To)
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	var total int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*)`+invoiceFrom+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	args = append(args, limit, max(f.Offset, 0))
	out, err := r.list(ctx, where+fmt.Sprintf(` ORDER BY i.issued_at DESC, i.number DESC LIMIT $%d OFFSET $%d`, len(args)-1, len(args)), args...)
	return out, total, err
}

func (r *BillingRepository) list(ctx context.Context, tail string, args ...any) ([]*models.Invoice, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+invoiceCols+invoiceFrom+tail, args...)
	if err != nil {
		return nil, err
	}
	var out []*models.Invoice
	for rows.Next() {
		inv, err := scanInvoice(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, inv)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, inv := range out {
		if err := r.loadLines(ctx, inv); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Unarchived returns invoices whose PDF is not in S3 yet, oldest first.
func (r *BillingRepository) Unarchived(ctx context.Context, limit int) ([]*models.Invoice, error) {
	return r.list(ctx, `WHERE i.pdf_key IS NULL ORDER BY i.created_at LIMIT $1`, limit)
}

func (r *BillingRepository) SetPDFKey(ctx context.Context, id uuid.UUID, key string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE invoices SET pdf_key = $2 WHERE id = $1 AND pdf_key IS NULL`, id, key)
	return err
}

// ListByUser returns a guest's invoices and credit notes, newest first.
func (r *BillingRepository) ListByUser(ctx context.Context, userID uuid.UUID, limit int) ([]*models.Invoice, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	return r.list(ctx, `WHERE i.user_id = $1 AND i.source_type IN ('booking', 'pass')
		ORDER BY i.issued_at DESC, i.number DESC LIMIT $2`, userID, limit)
}

// ListHostCommission returns a host's commission invoices and credit notes, newest first.
func (r *BillingRepository) ListHostCommission(ctx context.Context, hostID uuid.UUID) ([]*models.Invoice, error) {
	return r.list(ctx, `WHERE i.source_type = 'host_commission' AND i.source_id = $1
		ORDER BY i.issued_at DESC, i.number DESC`, hostID)
}

// ReportRow is one group of document lines in a period.
type ReportRow struct {
	Kind          string
	Registered    bool // buyer has a GSTIN (B2B)
	Commission    bool // a host commission document (else a guest fee)
	PlaceOfSupply string
	SACCode       string
	RateBps       int
	Documents     int
	Quantity      int64
	TaxableCents  int64
	CGSTCents     int64
	SGSTCents     int64
	IGSTCents     int64
	TotalCents    int64
}

// ReportRows sums document lines issued in [from, to) by kind, place of supply,
// SAC code and GST rate.
func (r *BillingRepository) ReportRows(ctx context.Context, from, to time.Time) ([]ReportRow, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT i.kind, COALESCE(i.buyer_gstin, '') <> '', i.source_type = 'host_commission',
		       i.place_of_supply, l.sac_code, l.gst_rate_bps, COUNT(DISTINCT i.id), COALESCE(SUM(l.quantity), 0),
		       SUM(l.taxable_cents), SUM(l.cgst_cents), SUM(l.sgst_cents), SUM(l.igst_cents), SUM(l.total_cents)
		  FROM invoices i JOIN invoice_lines l ON l.invoice_id = i.id
		 WHERE i.issued_at >= $1 AND i.issued_at < $2
		 GROUP BY 1, 2, 3, 4, 5, 6
		 ORDER BY 1, 2, 3, 4, 5, 6`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReportRow
	for rows.Next() {
		var x ReportRow
		if err := rows.Scan(&x.Kind, &x.Registered, &x.Commission, &x.PlaceOfSupply, &x.SACCode, &x.RateBps, &x.Documents, &x.Quantity,
			&x.TaxableCents, &x.CGSTCents, &x.SGSTCents, &x.IGSTCents, &x.TotalCents); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// SeriesRange is the first and last number used in one series in a period.
type SeriesRange struct {
	Kind  string
	First string
	Last  string
	Count int
}

// SeriesRanges returns each series' number range in [from, to). Numbers are
// zero-padded, so text order is number order within a series.
func (r *BillingRepository) SeriesRanges(ctx context.Context, from, to time.Time) ([]SeriesRange, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT kind, MIN(number), MAX(number), COUNT(*)
		  FROM invoices
		 WHERE issued_at >= $1 AND issued_at < $2
		 GROUP BY kind, split_part(number, '/', 1), fy
		 ORDER BY kind DESC, MIN(number)`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SeriesRange
	for rows.Next() {
		var x SeriesRange
		if err := rows.Scan(&x.Kind, &x.First, &x.Last, &x.Count); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// Register returns every document issued in [from, to), in number order.
func (r *BillingRepository) Register(ctx context.Context, from, to time.Time) ([]*models.Invoice, error) {
	return r.list(ctx, `WHERE i.issued_at >= $1 AND i.issued_at < $2 ORDER BY i.kind DESC, i.number`, from, to)
}

// StatementSale is one paid sale on a host's events, for the payout statement.
type StatementSale struct {
	Kind            string // "booking" or "pass"
	ID              uuid.UUID
	CreatedAt       time.Time
	EventTitle      string
	Quantity        int
	TicketCents     int64
	NetEarningCents int64 // ticket − MySlotMate commission (if any)
	TCSCents        int64
	TDSCents        int64
	PayableCents    int64 // what the host is owed (net − TCS − TDS)
	BoostCents      int64 // of the commission, the Event Boost part
	Status          string
	Returned        bool // refunded / cancelled: no longer owed
}

// StatementSales lists the paid sales (bookings and passes) on the host's
// events created in [from, to), oldest first.
func (r *BillingRepository) StatementSales(ctx context.Context, hostID uuid.UUID, from, to time.Time) ([]StatementSale, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT s.kind, s.id, s.created_at, e.title, s.quantity, s.ticket, s.net, s.tcs_cents, s.tds_cents,
		       COALESCE(s.host_payout_cents, s.net), s.boost, s.status, s.returned
		  FROM (
		    SELECT 'booking' AS kind, id, event_id, created_at, quantity,
		           COALESCE(ticket_cents, amount_cents, 0) AS ticket, COALESCE(net_earning_cents, 0) AS net,
		           tcs_cents, tds_cents, host_payout_cents, boost_commission_cents AS boost, status::text AS status,
		           status IN ('refunded', 'cancelled') AS returned
		      FROM bookings
		    UNION ALL
		    SELECT 'pass', id, event_id, created_at, 1,
		           COALESCE(ticket_cents, price_cents, 0), net_earning_cents,
		           tcs_cents, tds_cents, host_payout_cents, 0::bigint, status::text,
		           status = 'refunded'
		      FROM user_passes
		  ) s
		  JOIN events e ON e.id = s.event_id
		 WHERE e.host_id = $1 AND s.created_at >= $2 AND s.created_at < $3 AND s.ticket > 0
		 ORDER BY s.created_at, s.id`, hostID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StatementSale
	for rows.Next() {
		var x StatementSale
		if err := rows.Scan(&x.Kind, &x.ID, &x.CreatedAt, &x.EventTitle, &x.Quantity, &x.TicketCents, &x.NetEarningCents,
			&x.TCSCents, &x.TDSCents, &x.PayableCents, &x.BoostCents, &x.Status, &x.Returned); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// StatementPayout is money sent to the host's bank or UPI.
type StatementPayout struct {
	ID          uuid.UUID
	CreatedAt   time.Time
	AmountCents int64
	Status      string
	Reference   string
}

// StatementPayouts lists payouts and withdrawals from the host's account
// requested in [from, to), oldest first. Failed and reversed ones are kept so
// the statement explains them.
func (r *BillingRepository) StatementPayouts(ctx context.Context, hostID uuid.UUID, from, to time.Time) ([]StatementPayout, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT p.id, p.created_at, p.amount_cents, p.status::text, COALESCE(p.display_reference, '')
		  FROM payments p JOIN accounts a ON a.id = p.account_id
		 WHERE a.owner_type = 'host' AND a.owner_id = $1
		   AND p.type IN ('payout', 'withdrawal')
		   AND p.created_at >= $2 AND p.created_at < $3
		 ORDER BY p.created_at`, hostID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StatementPayout
	for rows.Next() {
		var x StatementPayout
		if err := rows.Scan(&x.ID, &x.CreatedAt, &x.AmountCents, &x.Status, &x.Reference); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// StatementHost is the header of a host's statement.
type StatementHost struct {
	ID    uuid.UUID
	Name  string
	Email string
	Phone string
	PAN   *string
	GSTIN *string
}

func (r *BillingRepository) StatementHost(ctx context.Context, hostID uuid.UUID) (*StatementHost, error) {
	h := &StatementHost{ID: hostID}
	err := r.db.QueryRowContext(ctx, `
		SELECT trim(concat_ws(' ', h.first_name, h.last_name)), COALESCE(u.email, ''), COALESCE(h.phn_number, ''), p.pan, p.gstin
		  FROM hosts h LEFT JOIN users u ON u.id = h.user_id LEFT JOIN host_tax_profiles p ON p.host_id = h.id
		 WHERE h.id = $1`, hostID).Scan(&h.Name, &h.Email, &h.Phone, &h.PAN, &h.GSTIN)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return h, err
}

// CommissionBooking is one boosted booking for a commission invoice or credit note.
type CommissionBooking struct {
	BookingID       uuid.UUID
	HostID          uuid.UUID
	EventID         uuid.UUID
	EventTitle      string
	Quantity        int
	CommissionCents int64
	InvoiceID       *uuid.UUID // the commission invoice it is on (reversals only)
}

func scanCommission(rows *sql.Rows) ([]CommissionBooking, error) {
	defer rows.Close()
	var out []CommissionBooking
	for rows.Next() {
		var x CommissionBooking
		if err := rows.Scan(&x.BookingID, &x.HostID, &x.EventID, &x.EventTitle, &x.Quantity, &x.CommissionCents, &x.InvoiceID); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// UninvoicedCommission lists kept boosted bookings created in [from, to) that
// are not on a commission invoice yet. hostID nil = every host. Locks the rows
// when called inside a transaction, so a refund can't slip in between.
func (r *BillingRepository) UninvoicedCommission(ctx context.Context, hostID *uuid.UUID, from, to time.Time, lock bool) ([]CommissionBooking, error) {
	q := `SELECT b.id, e.host_id, e.id, e.title, b.quantity, b.boost_commission_cents, b.boost_invoice_id
		  FROM bookings b JOIN events e ON e.id = b.event_id
		 WHERE b.boost_commission_cents > 0 AND b.boost_invoice_id IS NULL
		   AND b.status IN ('pending', 'confirmed')
		   AND b.created_at >= $1 AND b.created_at < $2
		   AND ($3::uuid IS NULL OR e.host_id = $3)
		 ORDER BY e.title, b.created_at`
	if lock {
		q += ` FOR UPDATE OF b`
	}
	rows, err := r.db.QueryContext(ctx, q, from, to, hostID)
	if err != nil {
		return nil, err
	}
	return scanCommission(rows)
}

// RefundedInvoicedCommission lists boosted bookings that are on a commission
// invoice but were refunded since, with no credit note yet. hostID nil = all.
func (r *BillingRepository) RefundedInvoicedCommission(ctx context.Context, hostID *uuid.UUID, lock bool) ([]CommissionBooking, error) {
	q := `SELECT b.id, e.host_id, e.id, e.title, b.quantity, b.boost_commission_cents, b.boost_invoice_id
		  FROM bookings b JOIN events e ON e.id = b.event_id
		 WHERE b.boost_invoice_id IS NOT NULL AND b.boost_credit_note_id IS NULL AND b.status = 'refunded'
		   AND ($1::uuid IS NULL OR e.host_id = $1)
		 ORDER BY b.boost_invoice_id, e.title, b.created_at`
	if lock {
		q += ` FOR UPDATE OF b`
	}
	rows, err := r.db.QueryContext(ctx, q, hostID)
	if err != nil {
		return nil, err
	}
	return scanCommission(rows)
}

// MarkCommission stamps bookings with the invoice (or credit note) that covers
// them. Fails unless every booking was still unstamped.
func (r *BillingRepository) MarkCommission(ctx context.Context, bookingIDs []uuid.UUID, docID uuid.UUID, creditNote bool) error {
	col := "boost_invoice_id"
	if creditNote {
		col = "boost_credit_note_id"
	}
	ids := make([]string, len(bookingIDs))
	for i, id := range bookingIDs {
		ids[i] = id.String()
	}
	res, err := r.db.ExecContext(ctx, `UPDATE bookings SET `+col+` = $1 WHERE id = ANY($2::uuid[]) AND `+col+` IS NULL`, docID, pq.Array(ids))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); int(n) != len(bookingIDs) {
		return fmt.Errorf("commission: %d of %d bookings were already stamped", len(bookingIDs)-int(n), len(bookingIDs))
	}
	return nil
}

// CommissionBuyer is the host as the buyer on a commission invoice.
type CommissionBuyer struct {
	UserID    uuid.UUID
	Name      string
	Email     string
	Phone     string
	GSTIN     *string
	StateCode *string
}

func (r *BillingRepository) CommissionBuyer(ctx context.Context, hostID uuid.UUID) (*CommissionBuyer, error) {
	b := &CommissionBuyer{}
	err := r.db.QueryRowContext(ctx, `
		SELECT h.user_id,
		       COALESCE(NULLIF(trim(p.pan_name), ''), trim(concat_ws(' ', h.first_name, h.last_name))),
		       COALESCE(u.email, ''), COALESCE(NULLIF(h.phn_number, ''), u.phn_number, ''), p.gstin, p.gst_state_code
		  FROM hosts h LEFT JOIN users u ON u.id = h.user_id LEFT JOIN host_tax_profiles p ON p.host_id = h.id
		 WHERE h.id = $1`, hostID).Scan(&b.UserID, &b.Name, &b.Email, &b.Phone, &b.GSTIN, &b.StateCode)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return b, err
}

// RunDone reports whether a billing run finished for a period.
func (r *BillingRepository) RunDone(ctx context.Context, kind, period string) (bool, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM billing_runs WHERE kind = $1 AND period = $2`, kind, period).Scan(&n)
	return n > 0, err
}

func (r *BillingRepository) RecordRun(ctx context.Context, kind, period string, invoices, creditNotes int) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO billing_runs (kind, period, invoices, credit_notes) VALUES ($1, $2, $3, $4)
		ON CONFLICT (kind, period) DO UPDATE SET ran_at = now(),
		  invoices = billing_runs.invoices + EXCLUDED.invoices, credit_notes = billing_runs.credit_notes + EXCLUDED.credit_notes`,
		kind, period, invoices, creditNotes)
	return err
}
