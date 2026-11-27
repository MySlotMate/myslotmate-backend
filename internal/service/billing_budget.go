package service

import (
	"context"
	"fmt"
	"time"

	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"

	"github.com/google/uuid"
)

// Boost ad budget invoices (PRD: Premium Hosts and paid Boost, "Money
// flows"). Each paid budget or top-up gets one GST tax invoice to the host;
// GST is added on top of the budget, so the taxable value is the budget
// itself. A reversed offline payment gets a credit note against it. Issued
// only when billing is on and the budget SAC code is set (CA to confirm the
// treatment).

// BudgetSale is one paid Boost budget payment to invoice.
type BudgetSale struct {
	PaymentID   uuid.UUID
	HostID      uuid.UUID
	EventID     uuid.UUID
	EventTitle  string
	Description string // the line text
	AmountCents int64  // before GST
	GSTCents    int64
}

// buildBudgetDoc assembles a budget invoice or credit note (not saved).
func buildBudgetDoc(kind string, st *models.BillingSettings, buyer *repository.CommissionBuyer, sale BudgetSale,
	number, fy string, orig *models.Invoice) *models.Invoice {
	pos := st.StateCode
	if buyer.GSTIN != nil && *buyer.GSTIN != "" && buyer.StateCode != nil && *buyer.StateCode != "" {
		pos = *buyer.StateCode
	}
	if orig != nil {
		pos = orig.PlaceOfSupply
	}
	var cgst, sgst, igst int64
	if pos == st.StateCode {
		cgst = sale.GSTCents / 2
		sgst = sale.GSTCents - cgst
	} else {
		igst = sale.GSTCents
	}
	evt := sale.EventID
	inv := &models.Invoice{
		ID: uuid.New(), Kind: kind, Number: number, FY: fy, IssuedAt: time.Now(),
		SourceType: models.InvoiceSourceBoostBudget, SourceID: sale.PaymentID, EventID: &evt,
		UserID: buyer.UserID, BuyerName: buyer.Name, BuyerEmail: buyer.Email, BuyerPhone: buyer.Phone,
		BuyerGSTIN: buyer.GSTIN, BuyerStateCode: buyer.StateCode, PlaceOfSupply: pos,
		Supplier: models.InvoiceSupplier{LegalName: st.LegalName, TradeName: st.TradeName, GSTIN: st.GSTIN,
			PAN: st.PAN, Address: st.Address, StateCode: st.StateCode, Email: st.Email},
		EventTitle:   "Marketing Boost ad budget: " + sale.EventTitle,
		TaxableCents: sale.AmountCents, CGSTCents: cgst, SGSTCents: sgst, IGSTCents: igst,
		TotalCents: sale.AmountCents + sale.GSTCents,
	}
	desc := sale.Description
	if orig != nil {
		inv.OriginalInvoiceID = &orig.ID
		inv.OriginalNumber = orig.Number
		inv.EventTitle = "Reversal of Marketing Boost ad budget: " + sale.EventTitle
		desc = "Reversal: " + desc
	}
	inv.Lines = []models.InvoiceLine{{
		ID: uuid.New(), InvoiceID: inv.ID, Position: 1, Description: desc, SACCode: st.BudgetSACCode, Quantity: 1,
		TaxableCents: sale.AmountCents, GSTRateBps: st.GSTRateBps, CGSTCents: cgst, SGSTCents: sgst, IGSTCents: igst,
		TotalCents: inv.TotalCents,
	}}
	return inv
}

// IssueBudgetDoc issues the budget invoice (kind invoice) or its credit note
// (kind credit_note) for a payment. Returns nil when budget invoices are not
// being issued. Safe to call again: an existing document is returned.
func (s *BillingService) IssueBudgetDoc(ctx context.Context, kind string, sale BudgetSale) (*models.Invoice, error) {
	st, err := s.repo.GetSettings(ctx)
	if err != nil {
		return nil, err
	}
	if !st.IssuingBudget() {
		return nil, nil
	}
	var out *models.Invoice
	err = s.inTx(ctx, func(r *repository.BillingRepository) error {
		if inv, err := r.GetBySource(ctx, models.InvoiceSourceBoostBudget, sale.PaymentID, kind); err != nil || inv != nil {
			out = inv
			return err
		}
		var orig *models.Invoice
		prefix := st.BudgetInvoicePrefix
		if kind == models.InvoiceKindCreditNote {
			o, err := r.GetBySource(ctx, models.InvoiceSourceBoostBudget, sale.PaymentID, models.InvoiceKindInvoice)
			if err != nil || o == nil {
				return err // nothing was invoiced, so nothing to reverse
			}
			orig, prefix = o, st.CreditNotePrefix
		}
		buyer, err := r.CommissionBuyer(ctx, sale.HostID)
		if err != nil || buyer == nil {
			return fmt.Errorf("host %s: %v", sale.HostID, err)
		}
		fy := FinancialYear(time.Now())
		n, err := r.NextNumber(ctx, prefix, fy)
		if err != nil {
			return err
		}
		out = buildBudgetDoc(kind, st, buyer, sale, InvoiceNumber(prefix, fy, n), fy, orig)
		return r.Create(ctx, out)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
