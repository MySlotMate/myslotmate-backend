package service

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"sort"
	"strconv"
	"time"

	"myslotmate-backend/internal/lib/gststate"
	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"
)

// The monthly GST report on platform fee invoices: what finance needs to file
// GSTR-1 for MySlotMate's own fee. Every guest is unregistered, so sales go in
// B2C (small) by place of supply and rate, net of credit notes; plus the SAC
// (HSN) summary and the documents-issued table. Host TCS/TDS (GSTR-8, 26Q) is
// the separate tax report.

// GSTTotals is one set of amounts.
type GSTTotals struct {
	Documents    int   `json:"documents"`
	TaxableCents int64 `json:"taxable_cents"`
	CGSTCents    int64 `json:"cgst_cents"`
	SGSTCents    int64 `json:"sgst_cents"`
	IGSTCents    int64 `json:"igst_cents"`
	TotalCents   int64 `json:"total_cents"`
}

func (t *GSTTotals) add(r repository.ReportRow, sign int64) {
	t.TaxableCents += sign * r.TaxableCents
	t.CGSTCents += sign * r.CGSTCents
	t.SGSTCents += sign * r.SGSTCents
	t.IGSTCents += sign * r.IGSTCents
	t.TotalCents += sign * r.TotalCents
}

// B2CSRow is a GSTR-1 table 7 line: net sales to consumers by state and rate.
type B2CSRow struct {
	PlaceOfSupply string `json:"place_of_supply"`
	StateName     string `json:"state_name"`
	RateBps       int    `json:"rate_bps"`
	GSTTotals
}

// SACRow is a GSTR-1 table 12 line: net sales by SAC code and rate.
type SACRow struct {
	SACCode  string `json:"sac_code"`
	RateBps  int    `json:"rate_bps"`
	Quantity int64  `json:"quantity"`
	GSTTotals
}

// DocRange is a GSTR-1 table 13 line: one series' numbers used.
type DocRange struct {
	Nature string `json:"nature"`
	First  string `json:"first"`
	Last   string `json:"last"`
	Count  int    `json:"count"`
}

// B2BRow is one document to a GST-registered buyer: GSTR-1 table 4 (invoices)
// or 9B (credit notes).
type B2BRow struct {
	Kind          string    `json:"kind"`
	GSTIN         string    `json:"gstin"`
	Name          string    `json:"name"`
	Number        string    `json:"number"`
	Date          time.Time `json:"date"`
	Against       string    `json:"against,omitempty"`
	PlaceOfSupply string    `json:"place_of_supply"`
	StateName     string    `json:"state_name"`
	RateBps       int       `json:"rate_bps"`
	GSTTotals
}

type GSTReport struct {
	Month       string     `json:"month"` // "2026-11"
	From        time.Time  `json:"from"`
	To          time.Time  `json:"to"` // exclusive
	Invoices    GSTTotals  `json:"invoices"`
	CreditNotes GSTTotals  `json:"credit_notes"`
	Net         GSTTotals  `json:"net"`       // invoices − credit notes: the revenue to report
	FeeNet      GSTTotals  `json:"fee_net"`   // of which guest platform fees
	BoostNet    GSTTotals  `json:"boost_net"` // of which host Boost commission
	B2CS        []B2CSRow  `json:"b2cs"`      // unregistered buyers, net
	B2B         []B2BRow   `json:"b2b"`       // registered buyers: invoices
	CDNR        []B2BRow   `json:"cdnr"`      // registered buyers: credit notes
	SAC         []SACRow   `json:"sac"`
	Documents   []DocRange `json:"documents"`
}

// MonthRange is [first of month, first of next month) in IST for "2026-11".
func MonthRange(month string) (time.Time, time.Time, error) {
	from, err := time.ParseInLocation("2006-01", month, istZone)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("month must be YYYY-MM")
	}
	return from, from.AddDate(0, 1, 0), nil
}

// GSTReport builds the month's report.
func (s *BillingService) GSTReport(ctx context.Context, month string) (*GSTReport, error) {
	from, to, err := MonthRange(month)
	if err != nil {
		return nil, &ErrBillingSettings{Message: err.Error()}
	}
	rows, err := s.repo.ReportRows(ctx, from, to)
	if err != nil {
		return nil, err
	}
	ranges, err := s.repo.SeriesRanges(ctx, from, to)
	if err != nil {
		return nil, err
	}
	rep := buildGSTReport(month, from, to, rows, ranges)
	docs, err := s.repo.Register(ctx, from, to)
	if err != nil {
		return nil, err
	}
	addB2B(rep, docs)
	return rep, nil
}

// addB2B lists documents to GST-registered buyers, one row per document.
func addB2B(rep *GSTReport, docs []*models.Invoice) {
	rep.B2B, rep.CDNR = []B2BRow{}, []B2BRow{}
	for _, d := range docs {
		if d.BuyerGSTIN == nil || *d.BuyerGSTIN == "" {
			continue
		}
		rate := 0
		if len(d.Lines) > 0 {
			rate = d.Lines[0].GSTRateBps
		}
		row := B2BRow{Kind: d.Kind, GSTIN: *d.BuyerGSTIN, Name: d.BuyerName, Number: d.Number, Date: d.IssuedAt,
			Against: d.OriginalNumber, PlaceOfSupply: d.PlaceOfSupply, StateName: gststate.Name(d.PlaceOfSupply), RateBps: rate,
			GSTTotals: GSTTotals{Documents: 1, TaxableCents: d.TaxableCents, CGSTCents: d.CGSTCents, SGSTCents: d.SGSTCents,
				IGSTCents: d.IGSTCents, TotalCents: d.TotalCents}}
		if d.Kind == models.InvoiceKindCreditNote {
			rep.CDNR = append(rep.CDNR, row)
		} else {
			rep.B2B = append(rep.B2B, row)
		}
	}
}

func buildGSTReport(month string, from, to time.Time, rows []repository.ReportRow, ranges []repository.SeriesRange) *GSTReport {
	rep := &GSTReport{Month: month, From: from, To: to, B2CS: []B2CSRow{}, SAC: []SACRow{}, Documents: []DocRange{}}
	type b2csKey struct {
		pos  string
		rate int
	}
	type sacKey struct {
		sac  string
		rate int
	}
	b2cs := map[b2csKey]*B2CSRow{}
	sac := map[sacKey]*SACRow{}
	for _, r := range rows {
		sign := int64(1)
		if r.Kind == models.InvoiceKindCreditNote {
			sign = -1
			rep.CreditNotes.Documents += r.Documents
			rep.CreditNotes.add(r, 1)
		} else {
			rep.Invoices.Documents += r.Documents
			rep.Invoices.add(r, 1)
		}
		rep.Net.add(r, sign)
		if r.Commission {
			rep.BoostNet.add(r, sign)
		} else {
			rep.FeeNet.add(r, sign)
		}

		if !r.Registered {
			bk := b2csKey{r.PlaceOfSupply, r.RateBps}
			if b2cs[bk] == nil {
				b2cs[bk] = &B2CSRow{PlaceOfSupply: r.PlaceOfSupply, StateName: gststate.Name(r.PlaceOfSupply), RateBps: r.RateBps}
			}
			b2cs[bk].add(r, sign)
		}

		sk := sacKey{r.SACCode, r.RateBps}
		if sac[sk] == nil {
			sac[sk] = &SACRow{SACCode: r.SACCode, RateBps: r.RateBps}
		}
		sac[sk].add(r, sign)
		sac[sk].Quantity += sign * r.Quantity
	}
	rep.Net.Documents = rep.Invoices.Documents + rep.CreditNotes.Documents
	for _, v := range b2cs {
		rep.B2CS = append(rep.B2CS, *v)
	}
	sort.Slice(rep.B2CS, func(i, j int) bool {
		a, b := rep.B2CS[i], rep.B2CS[j]
		return a.PlaceOfSupply < b.PlaceOfSupply || (a.PlaceOfSupply == b.PlaceOfSupply && a.RateBps < b.RateBps)
	})
	for _, v := range sac {
		rep.SAC = append(rep.SAC, *v)
	}
	sort.Slice(rep.SAC, func(i, j int) bool {
		a, b := rep.SAC[i], rep.SAC[j]
		return a.SACCode < b.SACCode || (a.SACCode == b.SACCode && a.RateBps < b.RateBps)
	})
	for _, r := range ranges {
		nature := "Invoices for outward supply"
		if r.Kind == models.InvoiceKindCreditNote {
			nature = "Credit Note"
		}
		rep.Documents = append(rep.Documents, DocRange{Nature: nature, First: r.First, Last: r.Last, Count: r.Count})
	}
	return rep
}

// rupeesCSV prints paise as rupees with two decimals, e.g. 6780 -> "67.80".
func rupeesCSV(cents int64) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	return fmt.Sprintf("%s%d.%02d", sign, cents/100, cents%100)
}

func rateCSV(bps int) string { return strconv.FormatFloat(float64(bps)/100, 'f', -1, 64) }

func writeCSV(rows [][]string) []byte {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.WriteAll(rows)
	return buf.Bytes()
}

// CSV returns one table of the report, laid out like the GSTR-1 offline tool's
// sheets: "b2cs", "hsn", "docs"; or "register", every document in the month.
func (s *BillingService) GSTReportCSV(ctx context.Context, month, table string) ([]byte, error) {
	rep, err := s.GSTReport(ctx, month)
	if err != nil {
		return nil, err
	}
	switch table {
	case "b2cs":
		out := [][]string{{"Type", "Place Of Supply", "Applicable % of Tax Rate", "Rate", "Taxable Value", "Cess Amount", "E-Commerce GSTIN"}}
		for _, r := range rep.B2CS {
			out = append(out, []string{"OE", r.PlaceOfSupply + "-" + r.StateName, "", rateCSV(r.RateBps), rupeesCSV(r.TaxableCents), "0.00", ""})
		}
		return writeCSV(out), nil
	case "hsn":
		st, err := s.repo.GetSettings(ctx)
		if err != nil {
			return nil, err
		}
		out := [][]string{{"HSN", "Description", "UQC", "Total Quantity", "Total Value", "Rate", "Taxable Value", "Integrated Tax Amount", "Central Tax Amount", "State/UT Tax Amount", "Cess Amount"}}
		for _, r := range rep.SAC {
			out = append(out, []string{r.SACCode, sacDescription(st, r.SACCode), "NA", strconv.FormatInt(r.Quantity, 10), rupeesCSV(r.TotalCents),
				rateCSV(r.RateBps), rupeesCSV(r.TaxableCents), rupeesCSV(r.IGSTCents), rupeesCSV(r.CGSTCents), rupeesCSV(r.SGSTCents), "0.00"})
		}
		return writeCSV(out), nil
	case "b2b":
		out := [][]string{{"GSTIN/UIN of Recipient", "Receiver Name", "Invoice Number", "Invoice date", "Invoice Value", "Place Of Supply",
			"Reverse Charge", "Applicable % of Tax Rate", "Invoice Type", "E-Commerce GSTIN", "Rate", "Taxable Value", "Cess Amount"}}
		for _, r := range rep.B2B {
			out = append(out, []string{r.GSTIN, r.Name, r.Number, r.Date.In(istZone).Format("02-Jan-2006"), rupeesCSV(r.TotalCents),
				r.PlaceOfSupply + "-" + r.StateName, "N", "", "Regular B2B", "", rateCSV(r.RateBps), rupeesCSV(r.TaxableCents), "0.00"})
		}
		return writeCSV(out), nil
	case "cdnr":
		out := [][]string{{"GSTIN/UIN of Recipient", "Receiver Name", "Note Number", "Note Date", "Note Type", "Place Of Supply",
			"Reverse Charge", "Note Supply Type", "Note Value", "Applicable % of Tax Rate", "Rate", "Taxable Value", "Cess Amount"}}
		for _, r := range rep.CDNR {
			out = append(out, []string{r.GSTIN, r.Name, r.Number, r.Date.In(istZone).Format("02-Jan-2006"), "C",
				r.PlaceOfSupply + "-" + r.StateName, "N", "Regular B2B", rupeesCSV(r.TotalCents), "", rateCSV(r.RateBps), rupeesCSV(r.TaxableCents), "0.00"})
		}
		return writeCSV(out), nil
	case "docs":
		out := [][]string{{"Nature of Document", "Sr. No. From", "Sr. No. To", "Total Number", "Cancelled"}}
		for _, d := range rep.Documents {
			out = append(out, []string{d.Nature, d.First, d.Last, strconv.Itoa(d.Count), "0"})
		}
		return writeCSV(out), nil
	case "register":
		docs, err := s.repo.Register(ctx, rep.From, rep.To)
		if err != nil {
			return nil, err
		}
		out := [][]string{{"Type", "Number", "Date", "Against invoice", "For", "Event", "Buyer", "Buyer email", "Place of supply", "SAC", "Rate %", "Taxable value", "CGST", "SGST", "IGST", "Total"}}
		for _, d := range docs {
			kind, sac, rate := "Invoice", "", ""
			if d.Kind == models.InvoiceKindCreditNote {
				kind = "Credit note"
			}
			if len(d.Lines) > 0 {
				sac, rate = d.Lines[0].SACCode, rateCSV(d.Lines[0].GSTRateBps)
			}
			out = append(out, []string{kind, d.Number, d.IssuedAt.In(istZone).Format("2006-01-02"), d.OriginalNumber,
				d.SourceType + " " + d.SourceID.String(), d.EventTitle, d.BuyerName, d.BuyerEmail, d.PlaceOfSupply, sac, rate,
				rupeesCSV(d.TaxableCents), rupeesCSV(d.CGSTCents), rupeesCSV(d.SGSTCents), rupeesCSV(d.IGSTCents), rupeesCSV(d.TotalCents)})
		}
		return writeCSV(out), nil
	}
	return nil, &ErrBillingSettings{Message: "table must be b2cs, b2b, cdnr, hsn, docs or register"}
}

// sacDescription names what a SAC code covers on our documents.
func sacDescription(st *models.BillingSettings, code string) string {
	fee, boost := st != nil && code == st.SACCode, st != nil && code == st.CommissionSACCode
	switch {
	case fee && boost:
		return "Platform fee and Marketing Boost commission"
	case boost:
		return "Marketing Boost commission"
	default:
		return "Platform fee"
	}
}
