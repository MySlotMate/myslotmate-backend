package bookingimport

import (
	"fmt"
	"time"

	"myslotmate-backend/internal/lib/timeutil"

	"github.com/xuri/excelize/v2"
)

// ReportJob is the job summary the report header needs. Declared here rather
// than importing models so this package stays a pure spreadsheet library with
// no dependency on the data layer (Parse/BuildTemplate have none either).
type ReportJob struct {
	FileName      string
	EventTitle    string
	OccurrenceIST string
	Status        string
	TotalRows     int
	SuccessRows   int
	FailedRows    int
	// PaymentMode is free | coupon | offline. UnitPriceCents is the per-seat price
	// at upload time. Together they give the host a reconciliation trail for an
	// offline-paid import: the platform holds none of this money, so this report
	// is the only record of what they should have collected at the door.
	PaymentMode    string
	UnitPriceCents int64
	CreatedAt      time.Time
	CompletedAt    *time.Time
}

// ReportRow is one row's outcome.
type ReportRow struct {
	RowNumber int
	Name      string
	Phone     string
	Quantity  int
	Status    string // success | failed | pending
	Error     string
}

const reportSummarySheet = "Summary"
const reportResultsSheet = "Results"

// BuildReport generates the post-import .xlsx the host downloads: what booked,
// what didn't, and why.
//
// Two sheets on purpose. "Summary" is the at-a-glance answer ("19 of 20
// booked"), while "Results" is the working list — every row in the original
// order, each with its outcome. Keeping the per-row table free of summary lines
// means the host can filter or sort it in Excel without tripping over header
// junk, and can paste the failed rows straight back into a fresh template.
func BuildReport(job ReportJob, rows []ReportRow) (*excelize.File, error) {
	f := excelize.NewFile()
	if _, err := f.NewSheet(reportSummarySheet); err != nil {
		return nil, err
	}
	idx, err := f.NewSheet(reportResultsSheet)
	if err != nil {
		return nil, err
	}
	f.SetActiveSheet(idx)
	if def := f.GetSheetName(0); def != reportSummarySheet && def != reportResultsSheet {
		_ = f.DeleteSheet(def)
	}

	labelStyle, err := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}})
	if err != nil {
		return nil, err
	}
	headerStyle, err := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Bold: true, Color: "FFFFFF"},
		Fill: excelize.Fill{Type: "pattern", Color: []string{"4F46E5"}, Pattern: 1},
	})
	if err != nil {
		return nil, err
	}
	okStyle, err := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Color: "047857", Bold: true},
	})
	if err != nil {
		return nil, err
	}
	failStyle, err := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Color: "B91C1C", Bold: true},
	})
	if err != nil {
		return nil, err
	}
	// Phone as text so Excel doesn't strip a leading zero or re-render the
	// number in scientific notation when the host reopens the report.
	textStyle, err := f.NewStyle(&excelize.Style{NumFmt: 49})
	if err != nil {
		return nil, err
	}
	// Money as a NUMBER with a rupee format, not a string — reconciliation is the
	// whole point of the offline column, and a host must be able to select it and
	// read a SUM.
	moneyStyle, err := f.NewStyle(&excelize.Style{NumFmt: 8}) // currency, 2dp
	if err != nil {
		return nil, err
	}

	// ---- Summary ----
	completed := "—"
	if job.CompletedAt != nil {
		completed = formatIST(*job.CompletedAt)
	}
	summary := [][2]string{
		{"Experience", job.EventTitle},
		{"Date & time", job.OccurrenceIST},
		{"File", job.FileName},
		{"Uploaded", formatIST(job.CreatedAt)},
		{"Finished", completed},
		{"", ""},
		{"Total guests in file", fmt.Sprintf("%d", job.TotalRows)},
		{"Booked", fmt.Sprintf("%d", job.SuccessRows)},
		{"Failed", fmt.Sprintf("%d", job.FailedRows)},
	}
	if job.PaymentMode == "offline" {
		summary = append(summary,
			[2]string{"", ""},
			[2]string{"Payment", "Collected by you, outside MySlotMate"},
			[2]string{"Price per seat", formatRupees(job.UnitPriceCents)},
			[2]string{"Total collected offline", formatRupees(job.UnitPriceCents * int64(bookedSeats(rows)))},
		)
	} else if job.PaymentMode == "coupon" {
		summary = append(summary,
			[2]string{"", ""},
			[2]string{"Payment", "Comped with a free-booking code"})
	}
	_ = f.SetColWidth(reportSummarySheet, "A", "A", 24)
	_ = f.SetColWidth(reportSummarySheet, "B", "B", 44)
	for i, kv := range summary {
		row := i + 1
		if kv[0] == "" {
			continue
		}
		_ = f.SetCellStr(reportSummarySheet, fmt.Sprintf("A%d", row), kv[0])
		_ = f.SetCellStyle(reportSummarySheet, fmt.Sprintf("A%d", row), fmt.Sprintf("A%d", row), labelStyle)
		_ = f.SetCellStr(reportSummarySheet, fmt.Sprintf("B%d", row), kv[1])
	}
	// Colour the two counts that matter.
	_ = f.SetCellStyle(reportSummarySheet, "B8", "B8", okStyle)
	_ = f.SetCellStyle(reportSummarySheet, "B9", "B9", failStyle)

	// ---- Results ----
	headers := []string{"Row", "Name", "Phone", "Quantity", "Status", "Reason"}
	widths := []float64{8, 26, 20, 10, 14, 56}
	// Offline imports get a per-row money column — it is the host's only record of
	// what each guest owed them at the door.
	offline := job.PaymentMode == "offline"
	if offline {
		headers = append(headers, "Collected offline")
		widths = append(widths, 18)
	}
	for i, h := range headers {
		col, _ := excelize.ColumnNumberToName(i + 1)
		_ = f.SetCellStr(reportResultsSheet, col+"1", h)
		_ = f.SetCellStyle(reportResultsSheet, col+"1", col+"1", headerStyle)
		_ = f.SetColWidth(reportResultsSheet, col, col, widths[i])
	}

	for i, r := range rows {
		n := i + 2 // header occupies row 1
		_ = f.SetCellInt(reportResultsSheet, fmt.Sprintf("A%d", n), int64(r.RowNumber))
		_ = f.SetCellStr(reportResultsSheet, fmt.Sprintf("B%d", n), r.Name)
		_ = f.SetCellStr(reportResultsSheet, fmt.Sprintf("C%d", n), r.Phone)
		_ = f.SetCellStyle(reportResultsSheet, fmt.Sprintf("C%d", n), fmt.Sprintf("C%d", n), textStyle)
		_ = f.SetCellInt(reportResultsSheet, fmt.Sprintf("D%d", n), int64(r.Quantity))

		label, style := "Not booked", failStyle
		switch r.Status {
		case "success":
			label, style = "Booked", okStyle
		case "pending":
			// Only reachable while a job is still running, or after an interrupted
			// one has yet to be resumed.
			label, style = "Still processing", labelStyle
		}
		_ = f.SetCellStr(reportResultsSheet, fmt.Sprintf("E%d", n), label)
		_ = f.SetCellStyle(reportResultsSheet, fmt.Sprintf("E%d", n), fmt.Sprintf("E%d", n), style)
		_ = f.SetCellStr(reportResultsSheet, fmt.Sprintf("F%d", n), r.Error)
		if offline {
			// Only booked seats were actually collected for; a failed row is left
			// blank rather than zero, so it doesn't read as "collected nothing".
			if r.Status == "success" {
				cell := fmt.Sprintf("G%d", n)
				_ = f.SetCellFloat(reportResultsSheet, cell,
					float64(job.UnitPriceCents*int64(r.Quantity))/100, 2, 64)
				_ = f.SetCellStyle(reportResultsSheet, cell, cell, moneyStyle)
			}
		}
	}

	// Freeze the header and switch on autofilter so a host with 500 rows can
	// immediately narrow to the failures.
	if len(rows) > 0 {
		_ = f.SetPanes(reportResultsSheet, &excelize.Panes{
			Freeze:      true,
			Split:       false,
			XSplit:      0,
			YSplit:      1,
			TopLeftCell: "A2",
			ActivePane:  "bottomLeft",
		})
		lastCol := "F"
		if offline {
			lastCol = "G"
		}
		_ = f.AutoFilter(reportResultsSheet, fmt.Sprintf("A1:%s%d", lastCol, len(rows)+1), nil)
	}
	return f, nil
}

// bookedSeats totals the seats on rows that actually booked.
func bookedSeats(rows []ReportRow) int {
	n := 0
	for _, r := range rows {
		if r.Status == "success" {
			n += r.Quantity
		}
	}
	return n
}

// formatRupees renders paise as a rupee string. Money is stored in the smallest
// unit throughout this codebase.
func formatRupees(cents int64) string {
	return fmt.Sprintf("Rs. %d.%02d", cents/100, cents%100)
}

// formatIST renders a stored UTC instant as an IST wall-clock string. Stored
// times are UTC and must be converted before display — see the timeutil package,
// which owns the zone.
func formatIST(t time.Time) string {
	return timeutil.FormatIST(t, "02 Jan 2006, 3:04 PM")
}
