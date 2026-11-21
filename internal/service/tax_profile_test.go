package service

import (
	"testing"
	"time"

	"myslotmate-backend/internal/lib/timeutil"
	"myslotmate-backend/internal/repository"
)

func TestNormalizeTaxProfile(t *testing.T) {
	g := func(s string) *string { return &s }
	cases := []struct {
		name    string
		in      TaxProfileInput
		wantErr bool
		state   string
	}{
		{"valid individual", TaxProfileInput{EntityType: "individual", PAN: "ABCDE1234F", PANName: "Asha Rao"}, false, ""},
		{"lowercase PAN is normalised", TaxProfileInput{EntityType: "individual", PAN: " abcde1234f ", PANName: "Asha Rao"}, false, ""},
		{"malformed PAN", TaxProfileInput{EntityType: "individual", PAN: "ABCD1234F", PANName: "Asha Rao"}, true, ""},
		{"missing name", TaxProfileInput{EntityType: "individual", PAN: "ABCDE1234F", PANName: "  "}, true, ""},
		{"unknown entity", TaxProfileInput{EntityType: "trust", PAN: "ABCDE1234F", PANName: "Asha Rao"}, true, ""},
		{"GSTIN matching PAN", TaxProfileInput{EntityType: "company", PAN: "ABCDE1234F", PANName: "Asha Co", GSTIN: g("27abcde1234f1z5")}, false, "27"},
		{"GSTIN for another PAN", TaxProfileInput{EntityType: "company", PAN: "ABCDE1234F", PANName: "Asha Co", GSTIN: g("27ZZZZZ9999Z1Z5")}, true, ""},
		{"empty GSTIN means none", TaxProfileInput{EntityType: "individual", PAN: "ABCDE1234F", PANName: "Asha Rao", GSTIN: g(" ")}, false, ""},
	}
	for _, c := range cases {
		p, err := NormalizeTaxProfile(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err=%v, wantErr=%v", c.name, err, c.wantErr)
			continue
		}
		if err != nil {
			continue
		}
		if p.PAN != "ABCDE1234F" {
			t.Errorf("%s: PAN = %q", c.name, p.PAN)
		}
		gotState := ""
		if p.GSTStateCode != nil {
			gotState = *p.GSTStateCode
		}
		if gotState != c.state {
			t.Errorf("%s: state = %q, want %q", c.name, gotState, c.state)
		}
	}
}

func TestFinancialYearStart(t *testing.T) {
	ist := timeutil.IST
	cases := []struct{ now, want time.Time }{
		{time.Date(2026, 11, 21, 12, 0, 0, 0, ist), time.Date(2026, 4, 1, 0, 0, 0, 0, ist)},
		{time.Date(2027, 3, 31, 23, 59, 0, 0, ist), time.Date(2026, 4, 1, 0, 0, 0, 0, ist)},
		{time.Date(2027, 4, 1, 0, 0, 0, 0, ist), time.Date(2027, 4, 1, 0, 0, 0, 0, ist)},
		// 31 Mar 20:00 UTC is already 1 Apr 01:30 IST — the new FY.
		{time.Date(2027, 3, 31, 20, 0, 0, 0, time.UTC), time.Date(2027, 4, 1, 0, 0, 0, 0, ist)},
	}
	for _, c := range cases {
		if got := FinancialYearStart(c.now); !got.Equal(c.want) {
			t.Errorf("FinancialYearStart(%v) = %v, want %v", c.now, got, c.want)
		}
	}
}

func TestMaskPAN(t *testing.T) {
	if got := MaskPAN("ABCDE1234F"); got != "ABCDE****F" {
		t.Errorf("MaskPAN = %q", got)
	}
}

func TestQuarterlyTDS(t *testing.T) {
	got := QuarterlyTDS(2026, []repository.MonthTax{
		{Month: "2026-04", TDSCents: 10}, {Month: "2026-06", TDSCents: 5},
		{Month: "2026-07", TDSCents: 7}, {Month: "2027-01", TDSCents: 3}, {Month: "2027-03", TDSCents: 2},
		{Month: "2027-04", TDSCents: 99}, // next FY, ignored
	})
	want := []int64{15, 7, 0, 5}
	for i, q := range got {
		if q.TDSCents != want[i] {
			t.Errorf("%s = %d, want %d", q.Quarter, q.TDSCents, want[i])
		}
	}
}
