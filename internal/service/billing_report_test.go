package service

import (
	"testing"

	"myslotmate-backend/internal/repository"
)

func TestBuildGSTReportNetsCreditNotes(t *testing.T) {
	rows := []repository.ReportRow{
		{Kind: "invoice", PlaceOfSupply: "19", SACCode: "998599", RateBps: 1800, Documents: 3, Quantity: 3,
			TaxableCents: 20340, CGSTCents: 1830, SGSTCents: 1830, TotalCents: 24000},
		{Kind: "credit_note", PlaceOfSupply: "19", SACCode: "998599", RateBps: 1800, Documents: 1, Quantity: 1,
			TaxableCents: 6780, CGSTCents: 610, SGSTCents: 610, TotalCents: 8000},
	}
	ranges := []repository.SeriesRange{
		{Kind: "invoice", First: "F/2627/0000004", Last: "F/2627/0000006", Count: 3},
		{Kind: "credit_note", First: "C/2627/0000002", Last: "C/2627/0000002", Count: 1},
	}
	from, to, err := MonthRange("2026-11")
	if err != nil {
		t.Fatal(err)
	}
	rep := buildGSTReport("2026-11", from, to, rows, ranges)
	if rep.Net.TotalCents != 16000 || rep.Net.TaxableCents != 13560 || rep.Net.CGSTCents != 1220 {
		t.Fatalf("net = %+v", rep.Net)
	}
	if rep.Invoices.Documents != 3 || rep.CreditNotes.Documents != 1 || rep.CreditNotes.TotalCents != 8000 {
		t.Fatalf("invoices %+v credit notes %+v", rep.Invoices, rep.CreditNotes)
	}
	if len(rep.B2CS) != 1 || rep.B2CS[0].TaxableCents != 13560 || rep.B2CS[0].StateName != "West Bengal" {
		t.Fatalf("b2cs = %+v", rep.B2CS)
	}
	if len(rep.SAC) != 1 || rep.SAC[0].Quantity != 2 || rep.SAC[0].TotalCents != 16000 {
		t.Fatalf("sac = %+v", rep.SAC)
	}
	if len(rep.Documents) != 2 || rep.Documents[1].Nature != "Credit Note" {
		t.Fatalf("docs = %+v", rep.Documents)
	}
	if got := rupeesCSV(-6780); got != "-67.80" {
		t.Fatalf("rupeesCSV = %s", got)
	}
	if from.Format("2006-01-02 15:04 -0700") != "2026-11-01 00:00 +0530" || to.Month() != 12 {
		t.Fatalf("month range %v – %v", from, to)
	}
	if _, _, err := MonthRange("Nov 2026"); err == nil {
		t.Fatal("bad month accepted")
	}
}
