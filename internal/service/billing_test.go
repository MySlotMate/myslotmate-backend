package service

import (
	"bytes"
	"testing"
	"time"

	"myslotmate-backend/internal/models"

	"github.com/google/uuid"
)

func TestFinancialYear(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+1800)
	cases := []struct {
		at   time.Time
		want string
	}{
		{time.Date(2026, 4, 1, 0, 0, 0, 0, ist), "2627"},
		{time.Date(2027, 3, 31, 23, 59, 0, 0, ist), "2627"},
		{time.Date(2026, 3, 31, 23, 59, 0, 0, ist), "2526"},
		// 31 Mar 20:00 UTC is already 1 Apr in India.
		{time.Date(2027, 3, 31, 20, 0, 0, 0, time.UTC), "2728"},
		{time.Date(2099, 6, 1, 0, 0, 0, 0, ist), "9900"},
	}
	for _, c := range cases {
		if got := FinancialYear(c.at); got != c.want {
			t.Errorf("FinancialYear(%v) = %s, want %s", c.at, got, c.want)
		}
	}
}

func TestInvoiceNumber(t *testing.T) {
	got := InvoiceNumber("F", "2627", 1)
	if got != "F/2627/0000001" {
		t.Fatalf("got %q", got)
	}
	if len(InvoiceNumber("ABC", "2627", 9999999)) > 16 {
		t.Fatal("longest number exceeds GST's 16 characters")
	}
}

func TestSplitInclusiveGST(t *testing.T) {
	cases := []struct {
		total int64
		intra bool
		want  GSTSplit
	}{
		// ₹40 fee on a ₹500 ticket: GST 18/118 of 4000 = 610.17 -> 610.
		{4000, true, GSTSplit{Taxable: 3390, CGST: 305, SGST: 305}},
		// Odd GST paisa goes to SGST.
		{3999, true, GSTSplit{Taxable: 3389, CGST: 305, SGST: 305}},
		{1180, true, GSTSplit{Taxable: 1000, CGST: 90, SGST: 90}},
		{1181, true, GSTSplit{Taxable: 1001, CGST: 90, SGST: 90}},
		{1, true, GSTSplit{Taxable: 1, CGST: 0, SGST: 0}},
		{4000, false, GSTSplit{Taxable: 3390, IGST: 610}},
		{0, true, GSTSplit{}},
	}
	for _, c := range cases {
		got := SplitInclusiveGST(c.total, 1800, c.intra)
		if got != c.want {
			t.Errorf("SplitInclusiveGST(%d, intra=%v) = %+v, want %+v", c.total, c.intra, got, c.want)
		}
		if c.total > 0 && got.Taxable+got.CGST+got.SGST+got.IGST != c.total {
			t.Errorf("split of %d does not add up: %+v", c.total, got)
		}
	}
	// An odd GST amount: 3 paise of GST splits 1 + 2.
	s := SplitInclusiveGST(20, 1800, true) // GST = round(20*18/118 = 3.05) = 3
	if s.CGST != 1 || s.SGST != 2 || s.Taxable != 17 {
		t.Errorf("odd split = %+v", s)
	}
}

func TestMoneyAndPct(t *testing.T) {
	for cents, want := range map[int64]string{
		0: "0.00", 5: "0.05", 4000: "40.00", 123456: "1,234.56",
		12345678: "1,23,456.78", 1234567890: "1,23,45,678.90", -4000: "-40.00",
	} {
		if got := money(cents); got != want {
			t.Errorf("money(%d) = %s, want %s", cents, got, want)
		}
	}
	for bps, want := range map[int]string{1800: "18%", 900: "9%", 250: "2.5%", 0: "0%"} {
		if got := pct(bps); got != want {
			t.Errorf("pct(%d) = %s, want %s", bps, got, want)
		}
	}
}

func TestBillingSettingsIssuing(t *testing.T) {
	s := &models.BillingSettings{Enabled: true, LegalName: "MySlotMate Pvt Ltd", GSTIN: "19ABCDE1234F1Z5",
		Address: "Kolkata, West Bengal", StateCode: "19"}
	if s.Issuing() {
		t.Fatal("issuing without a SAC code")
	}
	s.SACCode = "998599"
	if !s.Issuing() {
		t.Fatalf("not issuing; missing %v", s.Missing())
	}
	s.Enabled = false
	if s.Issuing() {
		t.Fatal("issuing while switched off")
	}
}

func TestRenderInvoicePDF(t *testing.T) {
	orig := uuid.New()
	for _, kind := range []string{models.InvoiceKindInvoice, models.InvoiceKindCreditNote} {
		inv := &models.Invoice{
			ID: uuid.New(), Kind: kind, Number: "F/2627/0000001", FY: "2627", IssuedAt: time.Now(),
			SourceType: models.InvoiceSourceBooking, SourceID: uuid.New(), EventTitle: "Sunrise Yoga by the Lake – Café edition",
			BuyerName: "Asha Test", BuyerEmail: "asha@example.com", BuyerPhone: "+919800000000", PlaceOfSupply: "19",
			Supplier: models.InvoiceSupplier{LegalName: "MySlotMate Private Limited", GSTIN: "19ABCDE1234F1Z5",
				Address: "1 Park Street, Kolkata, West Bengal 700016", StateCode: "19"},
			TaxableCents: 3390, CGSTCents: 305, SGSTCents: 305, TotalCents: 4000,
			Lines: []models.InvoiceLine{{Position: 1, Description: "Platform fee for booking Sunrise Yoga by the Lake – Café edition, a long title that wraps onto a second line (2 tickets)",
				SACCode: "998599", Quantity: 1, TaxableCents: 3390, GSTRateBps: 1800, CGSTCents: 305, SGSTCents: 305, TotalCents: 4000}},
		}
		if kind == models.InvoiceKindCreditNote {
			inv.Number, inv.OriginalInvoiceID, inv.OriginalNumber = "C/2627/0000001", &orig, "F/2627/0000001"
		}
		b, err := RenderInvoicePDF(inv)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if !bytes.HasPrefix(b, []byte("%PDF-")) || len(b) < 1000 {
			t.Fatalf("%s: not a PDF (%d bytes)", kind, len(b))
		}
	}
}
