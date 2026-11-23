package service

import (
	"bytes"
	"testing"
	"time"

	"myslotmate-backend/internal/repository"

	"github.com/google/uuid"
)

func TestBuildHostStatementTotals(t *testing.T) {
	pan := "ABCDE1234F"
	h := &repository.StatementHost{ID: uuid.New(), Name: "Riya Sen", PAN: &pan}
	now := time.Now()
	sales := []repository.StatementSale{
		// ₹1,000 kept, 10% boost commission, TCS ₹5, TDS ₹1.
		{Kind: "booking", ID: uuid.New(), CreatedAt: now, EventTitle: "Yoga", Quantity: 2, TicketCents: 100000,
			NetEarningCents: 90000, TCSCents: 500, TDSCents: 100, PayableCents: 89400, Status: "confirmed"},
		// Refunded: counts in sales and refunds, not in what's owed.
		{Kind: "booking", ID: uuid.New(), CreatedAt: now, EventTitle: "Yoga", Quantity: 1, TicketCents: 50000,
			NetEarningCents: 50000, PayableCents: 50000, Status: "refunded", Returned: true},
		{Kind: "pass", ID: uuid.New(), CreatedAt: now, EventTitle: "Yoga", Quantity: 1, TicketCents: 250000,
			NetEarningCents: 250000, PayableCents: 250000, Status: "active"},
	}
	payouts := []repository.StatementPayout{
		{ID: uuid.New(), CreatedAt: now, AmountCents: 80000, Status: "completed"},
		{ID: uuid.New(), CreatedAt: now, AmountCents: 20000, Status: "processing"},
		{ID: uuid.New(), CreatedAt: now, AmountCents: 5000, Status: "failed"},
	}
	from, to, _ := MonthRange("2026-11")
	st := buildHostStatement("2026-11", from, to, h, sales, payouts)
	tt := st.Totals
	if tt.Sales != 3 || tt.TicketCents != 400000 || tt.RefundedCents != 50000 || tt.NetTicketCents != 350000 {
		t.Fatalf("sales totals %+v", tt)
	}
	if tt.CommissionCents != 10000 || tt.TCSCents != 500 || tt.TDSCents != 100 || tt.PayableCents != 339400 {
		t.Fatalf("deductions %+v", tt)
	}
	if tt.PaidOutCents != 80000 || tt.InFlightCents != 20000 {
		t.Fatalf("payouts %+v", tt)
	}
	if st.Host.PANMasked != "XXXXXX234F" {
		t.Fatalf("PAN mask %q", st.Host.PANMasked)
	}
	b, err := RenderHostStatementPDF(st)
	if err != nil || !bytes.HasPrefix(b, []byte("%PDF-")) {
		t.Fatalf("pdf: %v", err)
	}
	if StatementFilename(st) != "MySlotMate-statement-2026-11-Riya-Sen.pdf" {
		t.Fatalf("filename %q", StatementFilename(st))
	}
}
