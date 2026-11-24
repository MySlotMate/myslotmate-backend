package service

import (
	"strings"
	"testing"

	"myslotmate-backend/internal/models"
)

func testBands() []models.BoostBand {
	p := func(v int64) *int64 { return &v }
	return []models.BoostBand{
		{Version: 1, MinPriceCents: 0, MaxPriceCents: p(49999), GuideRateBps: 2000, MinRateBps: 2000, MaxRateBps: 2000},
		{Version: 1, MinPriceCents: 50000, MaxPriceCents: p(99999), GuideRateBps: 2000, MinRateBps: 2000, MaxRateBps: 2000},
		{Version: 1, MinPriceCents: 100000, MaxPriceCents: p(199999), GuideRateBps: 1500, MinRateBps: 1200, MaxRateBps: 2000},
		{Version: 1, MinPriceCents: 200000, MaxPriceCents: p(499999), GuideRateBps: 1200, MinRateBps: 1200, MaxRateBps: 2000},
		{Version: 1, MinPriceCents: 500000, GuideRateBps: 1200, MinRateBps: 1200, MaxRateBps: 2000},
	}
}

func TestBoostGuideRatesFollowTheHostGuide(t *testing.T) {
	types := []models.BoostRate{
		{TicketKey: "general", TicketName: "General", PriceCents: 79900},
		{TicketKey: "pottery", TicketName: "Pottery", PriceCents: 149900},
		{TicketKey: "vip", TicketName: "VIP", PriceCents: 249900},
		{TicketKey: "retreat", TicketName: "Retreat", PriceCents: 500000},
		{TicketKey: "", TicketName: "Ticket", PriceCents: 49999},
	}
	got, err := guideRates(types, testBands())
	if err != nil {
		t.Fatal(err)
	}
	want := []int{2000, 1500, 1200, 1200, 2000}
	for i, g := range got {
		if g.GuideRateBps != want[i] || g.RateBps != want[i] || g.BandVersion != 1 {
			t.Errorf("%s: %+v, want %d", g.TicketName, g, want[i])
		}
	}
}

func TestBoostCommissionRounding(t *testing.T) {
	// Host guide example: ₹1,499 at 15% = ₹224.85; 40 tickets = ₹8,994.
	if c := BoostCommission(149900, 1500); c != 22485 {
		t.Fatalf("one ticket = %d", c)
	}
	if c := BoostCommission(149900*40, 1500); c != 899400 {
		t.Fatalf("40 tickets = %d", c)
	}
	if c := BoostCommission(333, 1500); c != 50 { // 49.95 → 50, half-up
		t.Fatalf("half-up = %d", c)
	}
	if BoostCommission(0, 2000) != 0 || BoostCommission(1000, 0) != 0 {
		t.Fatal("zero cases")
	}
	if gstInside(22485) != 3430 { // 224.85 × 18/118 = 34.30
		t.Fatalf("gst = %d", gstInside(22485))
	}
}

func TestBoostProposalRules(t *testing.T) {
	bands := testBands()
	guide, _ := guideRates([]models.BoostRate{
		{TicketKey: "general", TicketName: "General", PriceCents: 79900},
		{TicketKey: "vip", TicketName: "VIP", PriceCents: 249900},
	}, bands)

	// Under ₹1,000 is fixed at 20%.
	if _, err := applyProposal(guide, bands, []ProposalRate{{TicketKey: "general", RateBps: 1500, Reason: "x"}}); err == nil || !strings.Contains(err.Error(), "fixed at 20%") {
		t.Fatalf("fixed band: %v", err)
	}
	// ₹1,000+ may go 12–20%, with a reason when it differs.
	if _, err := applyProposal(guide, bands, []ProposalRate{{TicketKey: "vip", RateBps: 1800}}); err == nil || !strings.Contains(err.Error(), "reason") {
		t.Fatalf("reason required: %v", err)
	}
	if _, err := applyProposal(guide, bands, []ProposalRate{{TicketKey: "vip", RateBps: 2100, Reason: "x"}}); err == nil || !strings.Contains(err.Error(), "between 12% and 20%") {
		t.Fatalf("range: %v", err)
	}
	got, err := applyProposal(guide, bands, []ProposalRate{{TicketKey: "VIP ", RateBps: 1800, Reason: "Premium retreat, heavier ad spend"}})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].RateBps != 2000 || got[1].RateBps != 1800 || got[1].Reason == nil {
		t.Fatalf("rates %+v", got)
	}
	// Left out = guide rate; unknown ticket = error.
	if _, err := applyProposal(guide, bands, []ProposalRate{{TicketKey: "gold", RateBps: 1500, Reason: "x"}}); err == nil {
		t.Fatal("unknown ticket accepted")
	}
}

func TestBoostPricesUnchanged(t *testing.T) {
	q := []models.BoostRate{{TicketKey: "general", PriceCents: 79900}, {TicketKey: "vip", PriceCents: 249900}}
	if !pricesUnchanged(q, []models.BoostRate{{TicketKey: "vip", PriceCents: 249900}, {TicketKey: "general", PriceCents: 79900}}) {
		t.Fatal("same prices flagged")
	}
	if pricesUnchanged(q, []models.BoostRate{{TicketKey: "general", PriceCents: 89900}, {TicketKey: "vip", PriceCents: 249900}}) {
		t.Fatal("price change missed")
	}
	if pricesUnchanged(q, append(q, models.BoostRate{TicketKey: "gold", PriceCents: 150000})) {
		t.Fatal("new ticket type missed")
	}
}
