package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"myslotmate-backend/internal/models"

	"github.com/google/uuid"
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
		       sac_code, gst_rate_bps, invoice_prefix, credit_note_prefix, updated_at
		  FROM billing_settings WHERE id`).
		Scan(&s.Enabled, &s.LegalName, &s.TradeName, &s.GSTIN, &s.PAN, &s.Address, &s.StateCode, &s.Email,
			&s.SACCode, &s.GSTRateBps, &s.InvoicePrefix, &s.CreditNotePrefix, &s.UpdatedAt)
	if err == sql.ErrNoRows {
		return &models.BillingSettings{GSTRateBps: 1800, InvoicePrefix: "F", CreditNotePrefix: "C"}, nil
	}
	return s, err
}

func (r *BillingRepository) SaveSettings(ctx context.Context, s *models.BillingSettings) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO billing_settings (id, enabled, legal_name, trade_name, gstin, pan, address, state_code, email,
		                              sac_code, gst_rate_bps, invoice_prefix, credit_note_prefix, updated_at)
		VALUES (true, $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, now())
		ON CONFLICT (id) DO UPDATE SET
		  enabled = EXCLUDED.enabled, legal_name = EXCLUDED.legal_name, trade_name = EXCLUDED.trade_name,
		  gstin = EXCLUDED.gstin, pan = EXCLUDED.pan, address = EXCLUDED.address,
		  state_code = EXCLUDED.state_code, email = EXCLUDED.email, sac_code = EXCLUDED.sac_code,
		  gst_rate_bps = EXCLUDED.gst_rate_bps, invoice_prefix = EXCLUDED.invoice_prefix,
		  credit_note_prefix = EXCLUDED.credit_note_prefix, updated_at = now()`,
		s.Enabled, s.LegalName, s.TradeName, s.GSTIN, s.PAN, s.Address, s.StateCode, s.Email,
		s.SACCode, s.GSTRateBps, s.InvoicePrefix, s.CreditNotePrefix)
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
		                      supplier, taxable_cents, cgst_cents, sgst_cents, igst_cents, total_cents, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21, now())`,
		inv.ID, inv.Kind, inv.Number, inv.FY, inv.IssuedAt, inv.SourceType, inv.SourceID, inv.EventID, inv.EventTitle,
		inv.OriginalInvoiceID, inv.UserID, inv.BuyerName, inv.BuyerEmail, inv.BuyerPhone, inv.PlaceOfSupply,
		supplier, inv.TaxableCents, inv.CGSTCents, inv.SGSTCents, inv.IGSTCents, inv.TotalCents); err != nil {
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
	i.pdf_key, i.created_at`

const invoiceFrom = ` FROM invoices i LEFT JOIN invoices o ON o.id = i.original_invoice_id `

func scanInvoice(row interface{ Scan(...any) error }) (*models.Invoice, error) {
	inv := &models.Invoice{}
	var supplier []byte
	if err := row.Scan(&inv.ID, &inv.Kind, &inv.Number, &inv.FY, &inv.IssuedAt, &inv.SourceType, &inv.SourceID,
		&inv.EventID, &inv.EventTitle, &inv.OriginalInvoiceID, &inv.OriginalNumber, &inv.UserID, &inv.BuyerName,
		&inv.BuyerEmail, &inv.BuyerPhone, &inv.PlaceOfSupply, &supplier, &inv.TaxableCents, &inv.CGSTCents,
		&inv.SGSTCents, &inv.IGSTCents, &inv.TotalCents, &inv.PDFKey, &inv.CreatedAt); err != nil {
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
	return r.list(ctx, `WHERE i.user_id = $1 ORDER BY i.issued_at DESC, i.number DESC LIMIT $2`, userID, limit)
}
