package service

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"

	"github.com/google/uuid"
)

func commissionSettings() *models.BillingSettings {
	return &models.BillingSettings{Enabled: true, LegalName: "Moodverse Private Limited", TradeName: "MySlotMate",
		GSTIN: "19AAACM1234F1Z5", Address: "Kolkata, West Bengal", StateCode: "19", SACCode: "998599",
		CommissionSACCode: "998365", GSTRateBps: 1800, InvoicePrefix: "F", CreditNotePrefix: "C", CommissionInvoicePrefix: "M"}
}

func TestCommissionIssuingNeedsItsSAC(t *testing.T) {
	st := commissionSettings()
	if !st.IssuingCommission() {
		t.Fatal("should issue")
	}
	st.CommissionSACCode = ""
	if st.IssuingCommission() || !st.Issuing() {
		t.Fatal("commission must wait for its SAC code; fee invoices must not")
	}
}

func TestGroupByEvent(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	rows := []repository.CommissionBooking{
		{BookingID: uuid.New(), EventID: a, EventTitle: "Pottery", Quantity: 2, CommissionCents: 44970},
		{BookingID: uuid.New(), EventID: b, EventTitle: "Clay", Quantity: 1, CommissionCents: 39984},
		{BookingID: uuid.New(), EventID: a, EventTitle: "Pottery", Quantity: 1, CommissionCents: 22485},
	}
	g := groupByEvent(rows)
	if len(g) != 2 || g[0].title != "Clay" || g[1].tickets != 3 || g[1].totalCents != 67455 || len(g[1].bookingIDs) != 2 {
		t.Fatalf("groups %+v", g)
	}
}

func TestBuildCommissionDoc(t *testing.T) {
	st := commissionSettings()
	lines := []commissionLine{{title: "Pottery", tickets: 3, totalCents: 67455}, {title: "Clay", tickets: 1, totalCents: 39984}}
	host := uuid.New()
	at := time.Date(2026, 12, 2, 6, 0, 0, 0, istZone)

	// Unregistered host: our state, CGST + SGST.
	unreg := &repository.CommissionBuyer{UserID: uuid.New(), Name: "Riya Sen"}
	inv := buildCommissionDoc(models.InvoiceKindInvoice, st, host, unreg, lines, "2026-11", at, "M/2627/0000001", "2627", nil)
	if inv.PlaceOfSupply != "19" || inv.IGSTCents != 0 || inv.CGSTCents == 0 {
		t.Fatalf("unregistered: %+v", inv)
	}
	if inv.TotalCents != 107439 || inv.TaxableCents+inv.CGSTCents+inv.SGSTCents+inv.IGSTCents != inv.TotalCents {
		t.Fatalf("totals %+v", inv)
	}
	if *inv.Period != "2026-11" || inv.SourceType != models.InvoiceSourceHostCommission || inv.SourceID != host {
		t.Fatalf("source %+v", inv)
	}
	if len(inv.Lines) != 2 || inv.Lines[0].SACCode != "998365" || !strings.Contains(inv.Lines[0].Description, "Pottery (3 tickets, November 2026)") {
		t.Fatalf("lines %+v", inv.Lines)
	}

	// Registered in Maharashtra: their state, IGST.
	g, sc := "27ABCDE1234F1Z9", "27"
	reg := &repository.CommissionBuyer{UserID: uuid.New(), Name: "Studio LLP", GSTIN: &g, StateCode: &sc}
	inv2 := buildCommissionDoc(models.InvoiceKindInvoice, st, host, reg, lines, "2026-11", at, "M/2627/0000002", "2627", nil)
	if inv2.PlaceOfSupply != "27" || inv2.CGSTCents != 0 || inv2.IGSTCents == 0 || *inv2.BuyerGSTIN != g {
		t.Fatalf("registered: %+v", inv2)
	}

	// Credit note references the invoice.
	cn := buildCommissionDoc(models.InvoiceKindCreditNote, st, host, unreg, lines[:1], "2026-11", at, "C/2627/0000009", "2627", inv)
	if cn.OriginalInvoiceID == nil || *cn.OriginalInvoiceID != inv.ID || !strings.HasPrefix(cn.Lines[0].Description, "Refund of") {
		t.Fatalf("credit note %+v", cn)
	}

	for _, d := range []*models.Invoice{inv, inv2, cn} {
		d.OriginalNumber = "M/2627/0000001"
		b, err := RenderInvoicePDF(d)
		if err != nil || !bytes.HasPrefix(b, []byte("%PDF-")) {
			t.Fatalf("pdf %s: %v", d.Number, err)
		}
	}
}

func TestPreviousMonth(t *testing.T) {
	if m := previousMonth(time.Date(2027, 1, 1, 0, 30, 0, 0, istZone)); m != "2026-12" {
		t.Fatalf("got %s", m)
	}
	// 30 Nov 19:00 UTC is already 1 Dec in India.
	if m := previousMonth(time.Date(2026, 11, 30, 19, 0, 0, 0, time.UTC)); m != "2026-11" {
		t.Fatalf("got %s", m)
	}
}
