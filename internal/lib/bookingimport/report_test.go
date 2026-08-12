package bookingimport

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"
)

func TestBuildReport(t *testing.T) {
	completed := time.Date(2026, 8, 12, 9, 30, 0, 0, time.UTC)
	f, err := BuildReport(ReportJob{
		FileName:      "guests.xlsx",
		EventTitle:    "After-Work Social Escape",
		OccurrenceIST: "14 Aug 2026, 7:00 PM IST",
		Status:        "completed",
		TotalRows:     3,
		SuccessRows:   2,
		FailedRows:    1,
		CreatedAt:     completed.Add(-5 * time.Minute),
		CompletedAt:   &completed,
	}, []ReportRow{
		{RowNumber: 1, Name: "Asha Menon", Phone: "+919876543210", Quantity: 2, Status: "success"},
		{RowNumber: 2, Name: "Ravi Kumar", Phone: "+919876543211", Quantity: 1, Status: "failed", Error: "no seats left for this slot"},
		{RowNumber: 3, Name: "Priya S", Phone: "+919876543212", Quantity: 1, Status: "success"},
	})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}

	// Round-trip through the serialized bytes — an in-memory File can hide
	// problems that only surface once written.
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out, err := excelize.OpenReader(&buf)
	if err != nil {
		t.Fatalf("generated report is not readable: %v", err)
	}
	defer out.Close()

	sheets := out.GetSheetList()
	if len(sheets) != 2 {
		t.Fatalf("expected exactly the Summary and Results sheets, got %v", sheets)
	}

	rows, err := out.GetRows(reportResultsSheet)
	if err != nil {
		t.Fatalf("GetRows: %v", err)
	}
	// Header + 3 data rows.
	if len(rows) != 4 {
		t.Fatalf("expected 4 rows in Results, got %d", len(rows))
	}
	if rows[0][0] != "Row" || rows[0][4] != "Status" || rows[0][5] != "Reason" {
		t.Fatalf("unexpected Results header: %v", rows[0])
	}

	// A booked row reads "Booked" with no reason; a failed one carries the error.
	if rows[1][4] != "Booked" {
		t.Errorf("row 1 status = %q, want Booked", rows[1][4])
	}
	if rows[2][4] != "Not booked" {
		t.Errorf("row 2 status = %q, want Not booked", rows[2][4])
	}
	if rows[2][5] != "no seats left for this slot" {
		t.Errorf("row 2 reason = %q", rows[2][5])
	}
	// The phone must survive as text, not be mangled into a number.
	if rows[1][2] != "+919876543210" {
		t.Errorf("row 1 phone = %q, want +919876543210", rows[1][2])
	}

	summary, err := out.GetRows(reportSummarySheet)
	if err != nil {
		t.Fatalf("GetRows(Summary): %v", err)
	}
	if len(summary) < 9 {
		t.Fatalf("summary sheet is too short: %v", summary)
	}
	if summary[0][1] != "After-Work Social Escape" {
		t.Errorf("summary title = %q", summary[0][1])
	}
	if summary[7][1] != "2" || summary[8][1] != "1" {
		t.Errorf("summary counts wrong: booked=%q failed=%q", summary[7][1], summary[8][1])
	}
}

// An offline-paid import is the one case where the platform holds none of the
// money, so this report is the host's only reconciliation record. It must carry
// the per-row and total amounts, and only for rows that actually booked.
func TestBuildReportOfflinePayment(t *testing.T) {
	f, err := BuildReport(ReportJob{
		FileName:       "guests.xlsx",
		EventTitle:     "Paid Workshop",
		Status:         "completed",
		TotalRows:      3,
		SuccessRows:    2,
		FailedRows:     1,
		PaymentMode:    "offline",
		UnitPriceCents: 50000, // Rs. 500 per seat
		CreatedAt:      time.Now(),
	}, []ReportRow{
		{RowNumber: 1, Name: "Asha", Phone: "+919876543210", Quantity: 2, Status: "success"},
		{RowNumber: 2, Name: "Ravi", Phone: "+919876543211", Quantity: 1, Status: "failed", Error: "no seats left for this slot"},
		{RowNumber: 3, Name: "Priya", Phone: "+919876543212", Quantity: 1, Status: "success"},
	})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out, err := excelize.OpenReader(&buf)
	if err != nil {
		t.Fatalf("not readable: %v", err)
	}
	defer out.Close()

	rows, err := out.GetRows(reportResultsSheet)
	if err != nil {
		t.Fatalf("GetRows: %v", err)
	}
	// excelize trims trailing empty cells, so a row whose money cell is blank
	// comes back short — read defensively.
	at := func(row []string, i int) string {
		if i < len(row) {
			return row[i]
		}
		return ""
	}
	if at(rows[0], 6) != "Collected offline" {
		t.Fatalf("expected the offline money column, header = %v", rows[0])
	}
	// 2 seats x Rs.500.
	// Stored as a NUMBER so Excel can SUM the column; assert on the value, not on
	// how a given reader renders the currency format.
	amount := func(cell string) float64 {
		v, err := strconv.ParseFloat(strings.ReplaceAll(cell, ",", ""), 64)
		if err != nil {
			t.Fatalf("money cell %q is not numeric — a host cannot SUM it: %v", cell, err)
		}
		return v
	}
	if got := amount(at(rows[1], 6)); got != 1000 {
		t.Errorf("row 1 collected = %v, want 1000 (2 seats x Rs.500)", got)
	}
	// A failed row collected nothing — charging for a seat that never booked
	// would be exactly the wrong number to hand a host.
	if at(rows[2], 6) != "" {
		t.Errorf("failed row should show no amount, got %q", at(rows[2], 6))
	}
	if got := amount(at(rows[3], 6)); got != 500 {
		t.Errorf("row 3 collected = %v, want 500", got)
	}

	summary, err := out.GetRows(reportSummarySheet)
	if err != nil {
		t.Fatalf("GetRows(Summary): %v", err)
	}
	var total string
	for _, row := range summary {
		if len(row) >= 2 && row[0] == "Total collected offline" {
			total = row[1]
		}
	}
	// 3 booked seats x Rs.500 — the failed row is excluded.
	if total != "Rs. 1500.00" {
		t.Errorf("summary total = %q, want Rs. 1500.00", total)
	}
}

// A free/coupon import must NOT grow the money column — there is nothing to
// reconcile and an empty column would imply otherwise.
func TestBuildReportFreeHasNoMoneyColumn(t *testing.T) {
	f, err := BuildReport(ReportJob{
		FileName: "g.xlsx", PaymentMode: "free", TotalRows: 1, SuccessRows: 1,
		CreatedAt: time.Now(),
	}, []ReportRow{{RowNumber: 1, Name: "A", Phone: "+919876543210", Quantity: 1, Status: "success"}})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out, _ := excelize.OpenReader(&buf)
	defer out.Close()
	rows, _ := out.GetRows(reportResultsSheet)
	if len(rows[0]) != 6 {
		t.Fatalf("free import should have 6 columns, got %d: %v", len(rows[0]), rows[0])
	}
}

// A job with no rows at all must still produce a valid file rather than erroring
// on the autofilter range.
func TestBuildReportEmpty(t *testing.T) {
	f, err := BuildReport(ReportJob{FileName: "empty.xlsx", CreatedAt: time.Now()}, nil)
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := excelize.OpenReader(&buf); err != nil {
		t.Fatalf("empty report is not readable: %v", err)
	}
}
