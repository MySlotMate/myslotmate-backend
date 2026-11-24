package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"

	"github.com/google/uuid"
)

// Billing core (roadmap task 18): a GST tax invoice for the guest booking fee
// on every paid booking and monthly pass, written in the sale's own
// transaction, and a credit note written in the refund's transaction.
//
// The fee the guest pays already includes GST, so the invoice splits it into
// taxable value + GST; it never adds anything. Place of supply is MySlotMate's
// own state (the guest's state is not on record), so the GST is CGST + SGST.
// Pending CA sign-off (roadmap task 10).

var istZone = func() *time.Location {
	if loc, err := time.LoadLocation("Asia/Kolkata"); err == nil {
		return loc
	}
	return time.FixedZone("IST", 5*3600+1800)
}()

// FinancialYear is the Indian financial year of t as "2627" (FY 2026-27),
// April to March in IST.
func FinancialYear(t time.Time) string {
	t = t.In(istZone)
	start := t.Year()
	if t.Month() < time.April {
		start--
	}
	return fmt.Sprintf("%02d%02d", start%100, (start+1)%100)
}

// InvoiceNumber formats a number like F/2627/0000001 (14 characters; GST
// allows up to 16).
func InvoiceNumber(prefix, fy string, n int64) string {
	return fmt.Sprintf("%s/%s/%07d", prefix, fy, n)
}

// GSTSplit is a GST-inclusive amount split for an invoice line.
type GSTSplit struct {
	Taxable, CGST, SGST, IGST int64
}

// SplitInclusiveGST splits a GST-inclusive amount at rateBps (1800 = 18%),
// rounding the GST half-up to the paisa. Intra-state is CGST + SGST in equal
// halves (SGST takes an odd paisa); inter-state is all IGST.
func SplitInclusiveGST(totalCents int64, rateBps int, intraState bool) GSTSplit {
	if totalCents <= 0 {
		return GSTSplit{}
	}
	den := int64(10000 + rateBps)
	gst := (totalCents*int64(rateBps) + den/2) / den
	s := GSTSplit{Taxable: totalCents - gst}
	if intraState {
		s.CGST = gst / 2
		s.SGST = gst - s.CGST
	} else {
		s.IGST = gst
	}
	return s
}

// FeeSale is one paid sale whose guest booking fee gets an invoice.
type FeeSale struct {
	SourceType  string // models.InvoiceSourceBooking or InvoiceSourcePass
	SourceID    uuid.UUID
	UserID      uuid.UUID
	EventID     *uuid.UUID
	EventTitle  string
	Description string // the line text, e.g. "Platform fee: 2 tickets"
	FeeCents    int64  // the guest booking fee, GST included
	At          time.Time
}

// issueFeeInvoice writes the fee invoice for a sale inside its transaction.
// Returns nil (no invoice) when billing is off or incomplete, or the fee is 0.
func issueFeeInvoice(ctx context.Context, tx repository.DBTX, sale FeeSale) (*models.Invoice, error) {
	if sale.FeeCents <= 0 {
		return nil, nil
	}
	repo := repository.NewBillingRepository(tx)
	settings, err := repo.GetSettings(ctx)
	if err != nil {
		return nil, fmt.Errorf("billing: load settings: %w", err)
	}
	if !settings.Issuing() {
		return nil, nil
	}

	var name, email, phone string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(name,''), COALESCE(email,''), COALESCE(phn_number,'') FROM users WHERE id = $1`,
		sale.UserID).Scan(&name, &email, &phone); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("billing: load buyer: %w", err)
	}

	at := sale.At
	if at.IsZero() {
		at = time.Now()
	}
	fy := FinancialYear(at)
	n, err := repo.NextNumber(ctx, settings.InvoicePrefix, fy)
	if err != nil {
		return nil, fmt.Errorf("billing: next invoice number: %w", err)
	}

	pos := settings.StateCode // place of supply: see the file comment
	split := SplitInclusiveGST(sale.FeeCents, settings.GSTRateBps, pos == settings.StateCode)
	inv := &models.Invoice{
		ID: uuid.New(), Kind: models.InvoiceKindInvoice, Number: InvoiceNumber(settings.InvoicePrefix, fy, n), FY: fy,
		IssuedAt: at, SourceType: sale.SourceType, SourceID: sale.SourceID, EventID: sale.EventID, EventTitle: sale.EventTitle,
		UserID: sale.UserID, BuyerName: name, BuyerEmail: email, BuyerPhone: phone, PlaceOfSupply: pos,
		Supplier: models.InvoiceSupplier{LegalName: settings.LegalName, TradeName: settings.TradeName, GSTIN: settings.GSTIN,
			PAN: settings.PAN, Address: settings.Address, StateCode: settings.StateCode, Email: settings.Email},
		TaxableCents: split.Taxable, CGSTCents: split.CGST, SGSTCents: split.SGST, IGSTCents: split.IGST, TotalCents: sale.FeeCents,
	}
	inv.Lines = []models.InvoiceLine{{
		ID: uuid.New(), InvoiceID: inv.ID, Position: 1, Description: sale.Description, SACCode: settings.SACCode, Quantity: 1,
		TaxableCents: split.Taxable, GSTRateBps: settings.GSTRateBps,
		CGSTCents: split.CGST, SGSTCents: split.SGST, IGSTCents: split.IGST, TotalCents: sale.FeeCents,
	}}
	if err := repo.Create(ctx, inv); err != nil {
		return nil, fmt.Errorf("billing: %w", err)
	}
	return inv, nil
}

// issueCreditNote writes a credit note reversing a sale's fee invoice in full,
// inside the refund's transaction. Nil when the sale has no invoice; the
// existing note when one was already written.
func issueCreditNote(ctx context.Context, tx repository.DBTX, sourceType string, sourceID uuid.UUID, at time.Time) (*models.Invoice, error) {
	repo := repository.NewBillingRepository(tx)
	orig, err := repo.GetBySource(ctx, sourceType, sourceID, models.InvoiceKindInvoice)
	if err != nil {
		return nil, fmt.Errorf("billing: load invoice: %w", err)
	}
	if orig == nil {
		return nil, nil
	}
	if cn, err := repo.GetBySource(ctx, sourceType, sourceID, models.InvoiceKindCreditNote); err != nil || cn != nil {
		return cn, err
	}
	settings, err := repo.GetSettings(ctx)
	if err != nil {
		return nil, fmt.Errorf("billing: load settings: %w", err)
	}
	if at.IsZero() {
		at = time.Now()
	}
	fy := FinancialYear(at)
	n, err := repo.NextNumber(ctx, settings.CreditNotePrefix, fy)
	if err != nil {
		return nil, fmt.Errorf("billing: next credit note number: %w", err)
	}
	cn := *orig
	cn.ID = uuid.New()
	cn.Kind = models.InvoiceKindCreditNote
	cn.Number = InvoiceNumber(settings.CreditNotePrefix, fy, n)
	cn.FY = fy
	cn.IssuedAt = at
	cn.OriginalInvoiceID = &orig.ID
	cn.OriginalNumber = orig.Number
	cn.PDFKey = nil
	cn.Lines = make([]models.InvoiceLine, len(orig.Lines))
	for i, l := range orig.Lines {
		l.ID = uuid.New()
		l.InvoiceID = cn.ID
		l.Description = "Refund of " + lowerFirst(l.Description)
		cn.Lines[i] = l
	}
	if err := repo.Create(ctx, &cn); err != nil {
		return nil, fmt.Errorf("billing: %w", err)
	}
	return &cn, nil
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

/* ------------------------------------------------------------------ */
/*  BillingService: settings, reads, PDFs                              */
/* ------------------------------------------------------------------ */

// InvoiceStore archives invoice PDFs. Implemented by storage.UploadService;
// nil when S3 is not configured (PDFs are then always rebuilt on download).
type InvoiceStore interface {
	PutPrivateObject(ctx context.Context, key, contentType string, body []byte) error
	GetPrivateObject(ctx context.Context, key string) ([]byte, error)
}

type BillingService struct {
	repo  *repository.BillingRepository
	store InvoiceStore
	db    *sql.DB // for the commission run's transactions (WithDB)
}

func NewBillingService(repo *repository.BillingRepository, store InvoiceStore) *BillingService {
	return &BillingService{repo: repo, store: store}
}

// ErrBillingSettings is a validation failure on the billing settings form.
type ErrBillingSettings struct{ Message string }

func (e *ErrBillingSettings) Error() string { return e.Message }

var (
	billingGSTINRe = regexp.MustCompile(`^[0-9]{2}[A-Z]{5}[0-9]{4}[A-Z][1-9A-Z]Z[0-9A-Z]$`)
	billingPANRe   = regexp.MustCompile(`^[A-Z]{5}[0-9]{4}[A-Z]$`)
	sacRe          = regexp.MustCompile(`^99[0-9]{4}$`)
	prefixRe       = regexp.MustCompile(`^[A-Z]{1,3}$`)
)

func (s *BillingService) GetSettings(ctx context.Context) (*models.BillingSettings, error) {
	return s.repo.GetSettings(ctx)
}

// SaveSettings validates and saves the supplier details. The state code comes
// from the GSTIN. Turning billing on needs every required field.
func (s *BillingService) SaveSettings(ctx context.Context, in models.BillingSettings) (*models.BillingSettings, error) {
	in.LegalName = strings.TrimSpace(in.LegalName)
	in.TradeName = strings.TrimSpace(in.TradeName)
	in.GSTIN = strings.ToUpper(strings.TrimSpace(in.GSTIN))
	in.PAN = strings.ToUpper(strings.TrimSpace(in.PAN))
	in.Address = strings.TrimSpace(in.Address)
	in.Email = strings.TrimSpace(in.Email)
	in.SACCode = strings.TrimSpace(in.SACCode)
	in.InvoicePrefix = strings.ToUpper(strings.TrimSpace(in.InvoicePrefix))
	in.CreditNotePrefix = strings.ToUpper(strings.TrimSpace(in.CreditNotePrefix))
	if in.InvoicePrefix == "" {
		in.InvoicePrefix = "F"
	}
	if in.CreditNotePrefix == "" {
		in.CreditNotePrefix = "C"
	}
	in.CommissionInvoicePrefix = strings.ToUpper(strings.TrimSpace(in.CommissionInvoicePrefix))
	in.CommissionSACCode = strings.TrimSpace(in.CommissionSACCode)
	if in.CommissionInvoicePrefix == "" {
		in.CommissionInvoicePrefix = "M"
	}
	if in.GSTRateBps == 0 {
		in.GSTRateBps = 1800
	}

	bad := func(msg string) (*models.BillingSettings, error) { return nil, &ErrBillingSettings{Message: msg} }
	in.StateCode = ""
	if in.GSTIN != "" {
		if !billingGSTINRe.MatchString(in.GSTIN) {
			return bad("GSTIN should be 15 characters, like 19ABCDE1234F1Z5.")
		}
		in.StateCode = in.GSTIN[:2]
		if in.PAN == "" {
			in.PAN = in.GSTIN[2:12]
		}
	}
	if in.PAN != "" && !billingPANRe.MatchString(in.PAN) {
		return bad("PAN should be 10 characters, like ABCDE1234F.")
	}
	if in.GSTIN != "" && in.PAN != in.GSTIN[2:12] {
		return bad("The PAN doesn't match the one inside the GSTIN.")
	}
	if in.SACCode != "" && !sacRe.MatchString(in.SACCode) {
		return bad("SAC code should be 6 digits starting with 99.")
	}
	if !prefixRe.MatchString(in.InvoicePrefix) || !prefixRe.MatchString(in.CreditNotePrefix) {
		return bad("Prefixes should be 1 to 3 capital letters.")
	}
	if in.CommissionSACCode != "" && !sacRe.MatchString(in.CommissionSACCode) {
		return bad("Commission SAC code should be 6 digits starting with 99.")
	}
	if !prefixRe.MatchString(in.CommissionInvoicePrefix) {
		return bad("Prefixes should be 1 to 3 capital letters.")
	}
	if in.InvoicePrefix == in.CreditNotePrefix || in.CommissionInvoicePrefix == in.InvoicePrefix || in.CommissionInvoicePrefix == in.CreditNotePrefix {
		return bad("Fee invoices, commission invoices and credit notes need different prefixes.")
	}
	if in.GSTRateBps < 0 || in.GSTRateBps > 2800 {
		return bad("GST rate should be between 0% and 28%.")
	}
	if in.Enabled {
		if m := in.Missing(); len(m) > 0 {
			return bad("Fill in " + strings.ReplaceAll(strings.Join(m, ", "), "_", " ") + " before turning invoices on.")
		}
	}
	if err := s.repo.SaveSettings(ctx, &in); err != nil {
		return nil, err
	}
	return s.repo.GetSettings(ctx)
}

func (s *BillingService) List(ctx context.Context, f models.InvoiceFilter) ([]*models.Invoice, int, error) {
	return s.repo.List(ctx, f)
}

func (s *BillingService) Get(ctx context.Context, id uuid.UUID) (*models.Invoice, error) {
	return s.repo.GetByID(ctx, id)
}

// ForSource returns a sale's invoice and credit note (if any).
func (s *BillingService) ForSource(ctx context.Context, sourceType string, sourceID uuid.UUID) ([]*models.Invoice, error) {
	return s.repo.ListBySource(ctx, sourceType, sourceID)
}

// ForUser returns a guest's own invoices and credit notes, newest first.
func (s *BillingService) ForUser(ctx context.Context, userID uuid.UUID) ([]*models.Invoice, error) {
	return s.repo.ListByUser(ctx, userID, 200)
}

// InvoiceFilename is the download name of a document, e.g.
// "MySlotMate-F-2627-0000001.pdf".
func InvoiceFilename(inv *models.Invoice) string {
	return "MySlotMate-" + strings.ReplaceAll(inv.Number, "/", "-") + ".pdf"
}

// BookingReceipt returns the fee invoice PDF of a booking, or nil (no error)
// when the booking has none (free, or billing was off when it was made).
func (s *BillingService) BookingReceipt(ctx context.Context, bookingID uuid.UUID) (string, []byte, error) {
	inv, err := s.repo.GetBySource(ctx, models.InvoiceSourceBooking, bookingID, models.InvoiceKindInvoice)
	if err != nil || inv == nil {
		return "", nil, err
	}
	b, err := s.PDF(ctx, inv)
	if err != nil {
		return "", nil, err
	}
	return InvoiceFilename(inv), b, nil
}

// PDF returns the invoice PDF: the archived copy when there is one, else a
// fresh render (archived on the way out when S3 is configured).
func (s *BillingService) PDF(ctx context.Context, inv *models.Invoice) ([]byte, error) {
	if s.store != nil && inv.PDFKey != nil {
		if b, err := s.store.GetPrivateObject(ctx, *inv.PDFKey); err == nil {
			return b, nil
		} else {
			log.Printf("[BILLING] archived PDF %s unreadable, rebuilding: %v", inv.Number, err)
		}
	}
	b, err := RenderInvoicePDF(inv)
	if err != nil {
		return nil, err
	}
	if s.store != nil && inv.PDFKey == nil {
		s.archive(ctx, inv, b)
	}
	return b, nil
}

// InvoicePDFKey is where an invoice's PDF is archived (private, no-store).
// The id makes the key unguessable even if the bucket's public-read policy
// were ever widened to this folder.
func InvoicePDFKey(inv *models.Invoice) string {
	return fmt.Sprintf("%s%s/%s-%s.pdf", models.BillingDocsFolder, inv.FY, strings.ReplaceAll(inv.Number, "/", "-"), inv.ID)
}

func (s *BillingService) archive(ctx context.Context, inv *models.Invoice, pdf []byte) {
	key := InvoicePDFKey(inv)
	if err := s.store.PutPrivateObject(ctx, key, "application/pdf", pdf); err != nil {
		log.Printf("[BILLING] archive %s: %v", inv.Number, err)
		return
	}
	if err := s.repo.SetPDFKey(ctx, inv.ID, key); err != nil {
		log.Printf("[BILLING] record archive %s: %v", inv.Number, err)
		return
	}
	inv.PDFKey = &key
}

// ArchivePending renders and archives PDFs not in S3 yet. A failure is retried
// on the next run. Returns how many were archived.
func (s *BillingService) ArchivePending(ctx context.Context) (int, error) {
	if s.store == nil {
		return 0, nil
	}
	pending, err := s.repo.Unarchived(ctx, 50)
	if err != nil {
		return 0, err
	}
	done := 0
	for _, inv := range pending {
		b, err := RenderInvoicePDF(inv)
		if err != nil {
			log.Printf("[BILLING] render %s: %v", inv.Number, err)
			continue
		}
		s.archive(ctx, inv, b)
		if inv.PDFKey != nil {
			done++
		}
	}
	return done, nil
}

// RunArchiver archives pending PDFs every interval until ctx ends.
func (s *BillingService) RunArchiver(ctx context.Context, every time.Duration) {
	if s.store == nil {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if n, err := s.ArchivePending(ctx); err != nil {
			log.Printf("[BILLING] archiver: %v", err)
		} else if n > 0 {
			log.Printf("[BILLING] archived %d invoice PDF(s)", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
