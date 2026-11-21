package service

import (
	"testing"

	"myslotmate-backend/internal/models"
)

// Golden vectors from the Marketing Boost v2 PRD (₹799 × 40 example and the
// Payout and tax spec). The Node backend's tests/booking-tax.test.ts uses the
// same table — both backends must stamp identical numbers.
func TestComputeBookingTax(t *testing.T) {
	on := models.TaxSettings{Enabled: true, TCSBps: 50, TDSBps: 10, TDSThresholdCents: 50000000}
	gstin := "27ABCDE1234F1Z5"
	indiv := &models.TaxProfile{EntityType: "individual", PAN: "ABCDE1234F"}
	indivGST := &models.TaxProfile{EntityType: "individual", PAN: "ABCDE1234F", GSTIN: &gstin}
	companyGST := &models.TaxProfile{EntityType: "company", PAN: "ABCDE1234F", GSTIN: &gstin}

	cases := []struct {
		name                 string
		ticket, net, fyPrior int64
		profile              *models.TaxProfile
		settings             models.TaxSettings
		tcs, tds, payout     int64
	}{
		{"individual, no GSTIN, under 5L", 3196000, 3196000, 0, indiv, on, 0, 0, 3196000},
		{"individual, GSTIN, under 5L", 3196000, 3196000, 0, indivGST, on, 15980, 0, 3180020},
		{"company, GSTIN", 3196000, 3196000, 0, companyGST, on, 15980, 3196, 3176824},
		{"PRD Boost row: 20% commission", 3196000, 2556800, 0, companyGST, on, 15980, 3196, 2537624},
		{"single 799, GSTIN, above 5L", 79900, 79900, 60000000, indivGST, on, 400, 80, 79420},
		{"crossing booking takes catch-up", 200000, 200000, 49900000, indiv, on, 0, 50100, 149900},
		{"exactly at threshold", 100000, 100000, 49900000, indiv, on, 0, 0, 100000},
		{"no profile treated as individual", 79900, 79900, 60000000, nil, on, 0, 80, 79820},
		{"disabled", 79900, 79900, 0, companyGST, models.TaxSettings{}, 0, 0, 79900},
	}
	for _, c := range cases {
		got := ComputeBookingTax(TaxInput{
			TicketCents:        c.ticket,
			NetEarningCents:    c.net,
			FYGrossBeforeCents: c.fyPrior,
			Profile:            c.profile,
			Settings:           c.settings,
		})
		if got.TCSCents != c.tcs || got.TDSCents != c.tds || got.HostPayoutCents != c.payout {
			t.Errorf("%s: got tcs=%d tds=%d payout=%d, want tcs=%d tds=%d payout=%d",
				c.name, got.TCSCents, got.TDSCents, got.HostPayoutCents, c.tcs, c.tds, c.payout)
		}
	}
}

func TestComputeBookingTax_PlatformFeeGST(t *testing.T) {
	// 8% of ₹31,960 = ₹2,556.80; GST inside it at 18/118 = ₹390.02 (390.0203…).
	got := ComputeBookingTax(TaxInput{ServiceFeeCents: 255680})
	if got.PlatformFeeGSTCents != 39002 {
		t.Errorf("fee GST: got %d, want 39002", got.PlatformFeeGSTCents)
	}
}

func TestComputeBookingTax_Interstate(t *testing.T) {
	on := models.TaxSettings{Enabled: true, TCSBps: 50, TDSBps: 10, TDSThresholdCents: 50000000}
	gstin, gsc := "18ABCDE1234F1Z5", "18"
	p := &models.TaxProfile{EntityType: "individual", PAN: "ABCDE1234F", GSTIN: &gstin, GSTStateCode: &gsc}
	mh, as := "27", "18"

	if got := ComputeBookingTax(TaxInput{TicketCents: 79900, NetEarningCents: 79900, Profile: p, EventStateCode: &mh, Settings: on}); !got.TCSInterstate {
		t.Error("Assam GSTIN, Maharashtra venue: want interstate")
	}
	if got := ComputeBookingTax(TaxInput{TicketCents: 79900, NetEarningCents: 79900, Profile: p, EventStateCode: &as, Settings: on}); got.TCSInterstate {
		t.Error("same state: want intra-state")
	}
	if got := ComputeBookingTax(TaxInput{TicketCents: 79900, NetEarningCents: 79900, Profile: p, Settings: on}); got.TCSInterstate {
		t.Error("unknown venue state: want intra-state")
	}
}
