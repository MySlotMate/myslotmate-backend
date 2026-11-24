package service

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sort"
	"time"

	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"

	"github.com/google/uuid"
)

// Boost commission invoices (roadmap task 25). Once a month has ended, each
// host with kept boosted bookings that month gets one GST tax invoice for the
// Event Boost commission, one line per event. A booking already on an invoice
// that is refunded later gets a credit note against that invoice in the next
// run. The commission already includes GST, so the invoice only splits it.
//
// Place of supply: the host's GSTIN state when registered (B2B: CGST+SGST in
// our state, else IGST); otherwise MySlotMate's own state, as for guest fees.
// Pending CA sign-off (roadmap task 10), like the SAC code.

const commissionRunKind = "boost_commission"

// CommissionRunResult says what a run issued.
type CommissionRunResult struct {
	Month       string `json:"month"`
	Invoices    int    `json:"invoices"`
	CreditNotes int    `json:"credit_notes"`
	Skipped     string `json:"skipped,omitempty"` // why nothing was issued
}

// commissionLine groups one event's bookings on a document.
type commissionLine struct {
	eventID    uuid.UUID
	title      string
	tickets    int
	totalCents int64
	bookingIDs []uuid.UUID
}

func groupByEvent(rows []repository.CommissionBooking) []commissionLine {
	idx := map[uuid.UUID]int{}
	var out []commissionLine
	for _, r := range rows {
		i, ok := idx[r.EventID]
		if !ok {
			i = len(out)
			idx[r.EventID] = i
			out = append(out, commissionLine{eventID: r.EventID, title: r.EventTitle})
		}
		out[i].tickets += r.Quantity
		out[i].totalCents += r.CommissionCents
		out[i].bookingIDs = append(out[i].bookingIDs, r.BookingID)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].title < out[b].title })
	return out
}

func ticketsWord(n int) string {
	if n == 1 {
		return "1 ticket"
	}
	return fmt.Sprintf("%d tickets", n)
}

// buildCommissionDoc assembles a commission invoice or credit note (not saved).
func buildCommissionDoc(kind string, st *models.BillingSettings, hostID uuid.UUID, buyer *repository.CommissionBuyer,
	lines []commissionLine, period string, at time.Time, number, fy string, orig *models.Invoice) *models.Invoice {
	pos := st.StateCode
	if buyer.GSTIN != nil && *buyer.GSTIN != "" && buyer.StateCode != nil && *buyer.StateCode != "" {
		pos = *buyer.StateCode
	}
	intra := pos == st.StateCode
	monthLabel := period
	if t, err := time.ParseInLocation("2006-01", period, istZone); err == nil {
		monthLabel = t.Format("January 2006")
	}
	p := period
	inv := &models.Invoice{
		ID: uuid.New(), Kind: kind, Number: number, FY: fy, IssuedAt: at,
		SourceType: models.InvoiceSourceHostCommission, SourceID: hostID, Period: &p,
		UserID: buyer.UserID, BuyerName: buyer.Name, BuyerEmail: buyer.Email, BuyerPhone: buyer.Phone,
		BuyerGSTIN: buyer.GSTIN, BuyerStateCode: buyer.StateCode, PlaceOfSupply: pos,
		Supplier: models.InvoiceSupplier{LegalName: st.LegalName, TradeName: st.TradeName, GSTIN: st.GSTIN,
			PAN: st.PAN, Address: st.Address, StateCode: st.StateCode, Email: st.Email},
		EventTitle: "Marketing Boost commission, " + monthLabel,
	}
	if orig != nil {
		inv.OriginalInvoiceID = &orig.ID
		inv.OriginalNumber = orig.Number
		inv.EventTitle = "Refund of Marketing Boost commission, " + monthLabel
	}
	for i, l := range lines {
		split := SplitInclusiveGST(l.totalCents, st.GSTRateBps, intra)
		desc := fmt.Sprintf("Marketing Boost commission: %s (%s, %s)", l.title, ticketsWord(l.tickets), monthLabel)
		if kind == models.InvoiceKindCreditNote {
			desc = fmt.Sprintf("Refund of Marketing Boost commission: %s (%s refunded)", l.title, ticketsWord(l.tickets))
		}
		inv.Lines = append(inv.Lines, models.InvoiceLine{
			ID: uuid.New(), InvoiceID: inv.ID, Position: i + 1, Description: desc, SACCode: st.CommissionSACCode,
			Quantity: l.tickets, TaxableCents: split.Taxable, GSTRateBps: st.GSTRateBps,
			CGSTCents: split.CGST, SGSTCents: split.SGST, IGSTCents: split.IGST, TotalCents: l.totalCents,
		})
		inv.TaxableCents += split.Taxable
		inv.CGSTCents += split.CGST
		inv.SGSTCents += split.SGST
		inv.IGSTCents += split.IGST
		inv.TotalCents += l.totalCents
	}
	return inv
}

// RunCommission issues the commission invoices for a month that has ended,
// and credit notes for invoiced bookings refunded since. Safe to run again:
// bookings already invoiced or credited are skipped.
func (s *BillingService) RunCommission(ctx context.Context, month string, now time.Time) (*CommissionRunResult, error) {
	from, to, err := MonthRange(month)
	if err != nil {
		return nil, &ErrBillingSettings{Message: err.Error()}
	}
	res := &CommissionRunResult{Month: month}
	if now.Before(to) {
		return nil, &ErrBillingSettings{Message: "Commission invoices are issued once the month has ended."}
	}
	st, err := s.repo.GetSettings(ctx)
	if err != nil {
		return nil, err
	}
	if !st.IssuingCommission() {
		res.Skipped = "Billing is off, or the commission SAC code is not set."
		return res, nil
	}

	// Invoices: one per host with uninvoiced kept bookings in the month.
	rows, err := s.repo.UninvoicedCommission(ctx, nil, from, to, false)
	if err != nil {
		return nil, err
	}
	for _, hostID := range hostsOf(rows) {
		h := hostID
		err := s.inTx(ctx, func(r *repository.BillingRepository) error {
			mine, err := r.UninvoicedCommission(ctx, &h, from, to, true)
			if err != nil || len(mine) == 0 {
				return err
			}
			buyer, err := r.CommissionBuyer(ctx, h)
			if err != nil || buyer == nil {
				return fmt.Errorf("host %s: %v", h, err)
			}
			fy := FinancialYear(now)
			n, err := r.NextNumber(ctx, st.CommissionInvoicePrefix, fy)
			if err != nil {
				return err
			}
			lines := groupByEvent(mine)
			inv := buildCommissionDoc(models.InvoiceKindInvoice, st, h, buyer, lines, month, now,
				InvoiceNumber(st.CommissionInvoicePrefix, fy, n), fy, nil)
			if err := r.Create(ctx, inv); err != nil {
				return err
			}
			var ids []uuid.UUID
			for _, l := range lines {
				ids = append(ids, l.bookingIDs...)
			}
			if err := r.MarkCommission(ctx, ids, inv.ID, false); err != nil {
				return err
			}
			res.Invoices++
			return nil
		})
		if err != nil {
			return res, fmt.Errorf("commission invoice for host %s: %w", h, err)
		}
	}

	// Credit notes: per host, one against each invoice with refunded bookings.
	rev, err := s.repo.RefundedInvoicedCommission(ctx, nil, false)
	if err != nil {
		return res, err
	}
	for _, hostID := range hostsOf(rev) {
		h := hostID
		err := s.inTx(ctx, func(r *repository.BillingRepository) error {
			mine, err := r.RefundedInvoicedCommission(ctx, &h, true)
			if err != nil || len(mine) == 0 {
				return err
			}
			buyer, err := r.CommissionBuyer(ctx, h)
			if err != nil || buyer == nil {
				return fmt.Errorf("host %s: %v", h, err)
			}
			byInvoice := map[uuid.UUID][]repository.CommissionBooking{}
			var order []uuid.UUID
			for _, m := range mine {
				if _, ok := byInvoice[*m.InvoiceID]; !ok {
					order = append(order, *m.InvoiceID)
				}
				byInvoice[*m.InvoiceID] = append(byInvoice[*m.InvoiceID], m)
			}
			for _, invID := range order {
				orig, err := r.GetByID(ctx, invID)
				if err != nil || orig == nil {
					return fmt.Errorf("load invoice %s: %v", invID, err)
				}
				fy := FinancialYear(now)
				n, err := r.NextNumber(ctx, st.CreditNotePrefix, fy)
				if err != nil {
					return err
				}
				period := month
				if orig.Period != nil {
					period = *orig.Period
				}
				lines := groupByEvent(byInvoice[invID])
				cn := buildCommissionDoc(models.InvoiceKindCreditNote, st, h, buyer, lines, period, now,
					InvoiceNumber(st.CreditNotePrefix, fy, n), fy, orig)
				// A credit note follows its invoice's place of supply.
				cn.PlaceOfSupply = orig.PlaceOfSupply
				if err := r.Create(ctx, cn); err != nil {
					return err
				}
				var ids []uuid.UUID
				for _, l := range lines {
					ids = append(ids, l.bookingIDs...)
				}
				if err := r.MarkCommission(ctx, ids, cn.ID, true); err != nil {
					return err
				}
				res.CreditNotes++
			}
			return nil
		})
		if err != nil {
			return res, fmt.Errorf("commission credit note for host %s: %w", h, err)
		}
	}

	if err := s.repo.RecordRun(ctx, commissionRunKind, month, res.Invoices, res.CreditNotes); err != nil {
		return res, err
	}
	return res, nil
}

func hostsOf(rows []repository.CommissionBooking) []uuid.UUID {
	seen := map[uuid.UUID]bool{}
	var out []uuid.UUID
	for _, r := range rows {
		if !seen[r.HostID] {
			seen[r.HostID] = true
			out = append(out, r.HostID)
		}
	}
	return out
}

// HostCommissionDocs lists a host's commission invoices and credit notes.
func (s *BillingService) HostCommissionDocs(ctx context.Context, hostID uuid.UUID) ([]*models.Invoice, error) {
	out, err := s.repo.ListHostCommission(ctx, hostID)
	if out == nil {
		out = []*models.Invoice{}
	}
	return out, err
}

// previousMonth is the IST calendar month before now, e.g. "2026-10".
func previousMonth(now time.Time) string {
	t := now.In(istZone)
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, istZone).AddDate(0, -1, 0).Format("2006-01")
}

// RunCommissionInvoicer issues last month's commission invoices once, checking
// every interval (so it catches up after downtime or when billing turns on).
func (s *BillingService) RunCommissionInvoicer(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		now := time.Now()
		month := previousMonth(now)
		if done, err := s.repo.RunDone(ctx, commissionRunKind, month); err != nil {
			log.Printf("[BILLING] commission run check: %v", err)
		} else if !done {
			if res, err := s.RunCommission(ctx, month, now); err != nil {
				log.Printf("[BILLING] commission run %s: %v", month, err)
			} else if res.Skipped == "" {
				log.Printf("[BILLING] commission run %s: %d invoice(s), %d credit note(s)", month, res.Invoices, res.CreditNotes)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// inTx runs fn with the billing repository bound to a transaction.
func (s *BillingService) inTx(ctx context.Context, fn func(r *repository.BillingRepository) error) error {
	if s.db == nil {
		return fmt.Errorf("billing: no database handle")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(s.repo.WithTx(tx)); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// WithDB gives the billing service a database handle for its transactions.
func (s *BillingService) WithDB(db *sql.DB) *BillingService {
	s.db = db
	return s
}
