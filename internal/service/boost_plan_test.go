package service

import (
	"strings"
	"testing"

	"myslotmate-backend/internal/models"
)

func defaultBoostSettings() *models.BoostSettings {
	return &models.BoostSettings{MinDailyBudgetCents: 50000, BudgetStepCents: 10000, MinDays: 3, PaymentWindowDays: 7, BudgetGSTBps: 1800}
}

// PRD worked example: ₹1,000 a day for 7 days = ₹7,000 + ₹1,260 GST = ₹8,260.
func TestBudgetWorkedExample(t *testing.T) {
	days := daysIncl("2026-12-01", "2026-12-07")
	if days != 7 {
		t.Fatalf("days = %d, want 7", days)
	}
	total := int64(100000) * int64(days)
	if gst := budgetGST(total, 1800); total != 700000 || gst != 126000 {
		t.Fatalf("total %d gst %d, want 700000 / 126000", total, gst)
	}
	// Marketing fee: 8% of ₹800 × 60 tickets = ₹3,840.
	if fee := BoostCommission(80000*60, 800); fee != 384000 {
		t.Fatalf("fee = %d, want 384000", fee)
	}
}

func TestPlanProblems(t *testing.T) {
	st := defaultBoostSettings()
	cases := []struct {
		name                        string
		daily                       int64
		from, until, today, lastDay string
		want                        string // substring of a problem, "" = no problems
	}{
		{"ok", 100000, "2026-12-01", "2026-12-03", "2026-11-27", "2026-12-10", ""},
		{"below minimum", 40000, "2026-12-01", "2026-12-03", "2026-11-27", "", "at least ₹500"},
		{"off step", 55000, "2026-12-01", "2026-12-03", "2026-11-27", "", "steps of ₹100"},
		{"too short", 100000, "2026-12-01", "2026-12-02", "2026-11-27", "", "at least 3 days"},
		{"past start", 100000, "2026-11-20", "2026-12-02", "2026-11-27", "", "past"},
		{"after event", 100000, "2026-12-01", "2026-12-20", "2026-11-27", "2026-12-10", "last date"},
		{"missing dates", 100000, "", "", "2026-11-27", "", "start and end"},
		{"end before start", 100000, "2026-12-05", "2026-12-01", "2026-11-27", "", "at least 3 days"},
	}
	for _, c := range cases {
		p := planProblems(st, c.daily, c.from, c.until, c.today, c.lastDay)
		got := strings.Join(p, " | ")
		if c.want == "" && len(p) > 0 || c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: problems %q, want %q", c.name, got, c.want)
		}
	}
}

func TestBudgetEnd(t *testing.T) {
	u := "2026-12-07"
	end := budgetEnd(&models.Boost{BudgetUntil: &u})
	if end == nil || end.In(istZone).Format("2006-01-02 15:04") != "2026-12-08 00:00" {
		t.Fatalf("end = %v, want midnight IST after the last day", end)
	}
}

func TestBuildResults(t *testing.T) {
	spend := []models.BoostSpendEntry{
		{Day: "2026-12-02", Channel: "instagram", SpendCents: 60000, Reach: 1000, Clicks: 40, Impressions: 3000},
		{Day: "2026-12-01", Channel: "instagram", SpendCents: 50000, Reach: 900, Clicks: 30, Impressions: 2500},
		{Day: "2026-12-01", Channel: "google", SpendCents: 40000, Reach: 100, Clicks: 20, Impressions: 500},
	}
	res := buildResults(spend, map[string]int{"2026-12-01": 3, "2026-12-03": 2})
	if len(res.Days) != 3 || res.Days[0].Day != "2026-12-01" || res.Days[2].Day != "2026-12-03" {
		t.Fatalf("days not merged and sorted: %+v", res.Days)
	}
	if res.Days[0].SpendCents != 90000 || res.Days[0].TicketsSold != 3 {
		t.Fatalf("day 1 = %+v", res.Days[0])
	}
	if res.Totals.SpendCents != 150000 || res.Totals.TicketsSold != 5 || res.Totals.Clicks != 90 {
		t.Fatalf("totals = %+v", res.Totals)
	}
	if len(res.Channels) != 2 || res.Channels[0].Channel != "instagram" || res.Channels[0].SpendCents != 110000 {
		t.Fatalf("channels = %+v", res.Channels)
	}
}

func TestCheckBands(t *testing.T) {
	i := func(v int64) *int64 { return &v }
	good := []models.BoostBand{
		{MinPriceCents: 0, MaxPriceCents: i(49999), GuideRateBps: 1000, MinRateBps: 800, MaxRateBps: 1200},
		{MinPriceCents: 50000, MaxPriceCents: nil, GuideRateBps: 500, MinRateBps: 400, MaxRateBps: 800},
	}
	if err := checkBands(good); err != nil {
		t.Fatalf("good bands refused: %v", err)
	}
	gap := []models.BoostBand{good[0], {MinPriceCents: 60000, GuideRateBps: 500, MinRateBps: 400, MaxRateBps: 800}}
	if checkBands(gap) == nil {
		t.Fatal("a gap between bands was accepted")
	}
	bad := []models.BoostBand{{MinPriceCents: 0, GuideRateBps: 300, MinRateBps: 400, MaxRateBps: 800}}
	if checkBands(bad) == nil {
		t.Fatal("guide below min was accepted")
	}
	closed := []models.BoostBand{{MinPriceCents: 0, MaxPriceCents: i(100), GuideRateBps: 500, MinRateBps: 400, MaxRateBps: 800}}
	if checkBands(closed) == nil {
		t.Fatal("a closed last band was accepted")
	}
}

func TestBudgetBandRange(t *testing.T) {
	bands := []models.BoostBand{
		{Version: 2, MinPriceCents: 0, MaxPriceCents: ptr64(49999), GuideRateBps: 1000, MinRateBps: 800, MaxRateBps: 1200},
		{Version: 2, MinPriceCents: 50000, MaxPriceCents: nil, GuideRateBps: 800, MinRateBps: 600, MaxRateBps: 1000},
	}
	guide, err := guideRates([]models.BoostRate{{TicketKey: "", TicketName: "Ticket", PriceCents: 80000}}, bands)
	if err != nil || guide[0].RateBps != 800 || guide[0].BandVersion != 2 {
		t.Fatalf("guide = %+v, %v", guide, err)
	}
	if _, err := applyProposal(guide, bands, []ProposalRate{{TicketKey: "", RateBps: 1200, Reason: "x"}}); err == nil {
		t.Fatal("12% on a ₹800 ticket (max 10%) was accepted")
	}
	if out, err := applyProposal(guide, bands, []ProposalRate{{TicketKey: "", RateBps: 600, Reason: "launch offer"}}); err != nil || out[0].RateBps != 600 {
		t.Fatalf("6%% with a reason refused: %v", err)
	}
}


func TestCreativeSizeLimit(t *testing.T) {
	st := &models.BoostSettings{CreativeImageMaxMB: 20, CreativeVideoMaxMB: 200, CreativeOtherMaxMB: 50}
	for _, c := range []struct {
		typ, mime string
		want      int64
	}{
		{models.CreativeReel, "video/mp4", 200 * mb},
		{models.CreativePost, "image/png", 20 * mb},
		{models.CreativeBrand, "image/png", 50 * mb}, // brand material: 50 MB whatever the file
		{models.CreativeBrand, "application/zip", 50 * mb},
		{"", "image/jpeg", 50 * mb}, // upload URL: the type isn't known yet
	} {
		if got := creativeSizeLimit(st, c.typ, c.mime); got != c.want {
			t.Errorf("%s %s: %d, want %d", c.typ, c.mime, got, c.want)
		}
	}
}

func TestWeeklySummary(t *testing.T) {
	b := &models.Boost{EventTitle: "Jam <night>", PaidCents: 700000}
	res := &Results{Totals: ResultsDay{SpendCents: 300000, Reach: 5000, Clicks: 200, TicketsSold: 30}}
	got := weeklySummary(b, res)
	for _, want := range []string{"Jam &lt;night&gt;", "₹3000 of ₹7000", "Tickets sold: 30", "cost per ticket: ₹100"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(weeklySummary(b, &Results{}), "cost per ticket: –") {
		t.Error("no tickets should show a dash, not divide by zero")
	}
}
