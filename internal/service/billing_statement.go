package service

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"

	"github.com/go-pdf/fpdf"
	"github.com/google/uuid"
)

// A host's monthly payout statement: every paid sale on their events that
// month, what MySlotMate kept as commission (only when an admin set one),
// TCS/TDS withheld for the government, what the host is owed, and the payouts
// sent to their bank or UPI. It is a statement, not a tax invoice.

type StatementLine struct {
	Kind         string    `json:"kind"` // booking | pass
	ID           uuid.UUID `json:"id"`
	Date         time.Time `json:"date"`
	EventTitle   string    `json:"event_title"`
	Quantity     int       `json:"quantity"`
	TicketCents  int64     `json:"ticket_cents"`
	Commission   int64     `json:"commission_cents"`
	TCSCents     int64     `json:"tcs_cents"`
	TDSCents     int64     `json:"tds_cents"`
	PayableCents int64     `json:"payable_cents"`
	Status       string    `json:"status"`
	Refunded     bool      `json:"refunded"`
}

type StatementPayoutLine struct {
	ID          uuid.UUID `json:"id"`
	Date        time.Time `json:"date"`
	AmountCents int64     `json:"amount_cents"`
	Status      string    `json:"status"`
	Reference   string    `json:"reference"`
}

type StatementTotals struct {
	Sales           int   `json:"sales"`
	TicketCents     int64 `json:"ticket_cents"`     // every sale made
	RefundedCents   int64 `json:"refunded_cents"`   // of which refunded / cancelled
	NetTicketCents  int64 `json:"net_ticket_cents"` // kept sales
	CommissionCents int64 `json:"commission_cents"` // on kept sales
	TCSCents        int64 `json:"tcs_cents"`        // on kept sales
	TDSCents        int64 `json:"tds_cents"`        // on kept sales
	PayableCents    int64 `json:"payable_cents"`    // owed for the month's kept sales
	PaidOutCents    int64 `json:"paid_out_cents"`   // payouts completed this month
	InFlightCents   int64 `json:"in_flight_cents"`  // payouts pending / processing
}

type HostStatement struct {
	Month   string                 `json:"month"`
	From    time.Time              `json:"from"`
	To      time.Time              `json:"to"`
	Host    StatementHostView      `json:"host"`
	Issuer  models.InvoiceSupplier `json:"issuer"`
	Lines   []StatementLine        `json:"lines"`
	Payouts []StatementPayoutLine  `json:"payouts"`
	Totals  StatementTotals        `json:"totals"`
}

type StatementHostView struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	PANMasked string    `json:"pan_masked,omitempty"`
	GSTIN     string    `json:"gstin,omitempty"`
}

// ErrHostNotFound is returned for an unknown host id.
var ErrHostNotFound = &ErrBillingSettings{Message: "Host not found."}

// maskPAN keeps the last four characters: XXXXXX234F.
func maskPAN(p *string) string {
	if p == nil || len(*p) < 4 {
		return ""
	}
	return strings.Repeat("X", len(*p)-4) + (*p)[len(*p)-4:]
}

// HostStatement builds a host's statement for month "2026-11" (IST).
func (s *BillingService) HostStatement(ctx context.Context, hostID uuid.UUID, month string) (*HostStatement, error) {
	from, to, err := MonthRange(month)
	if err != nil {
		return nil, &ErrBillingSettings{Message: err.Error()}
	}
	h, err := s.repo.StatementHost(ctx, hostID)
	if err != nil {
		return nil, err
	}
	if h == nil {
		return nil, ErrHostNotFound
	}
	sales, err := s.repo.StatementSales(ctx, hostID, from, to)
	if err != nil {
		return nil, err
	}
	payouts, err := s.repo.StatementPayouts(ctx, hostID, from, to)
	if err != nil {
		return nil, err
	}
	settings, err := s.repo.GetSettings(ctx)
	if err != nil {
		return nil, err
	}
	st := buildHostStatement(month, from, to, h, sales, payouts)
	st.Issuer = models.InvoiceSupplier{LegalName: settings.LegalName, TradeName: settings.TradeName,
		GSTIN: settings.GSTIN, Address: settings.Address, Email: settings.Email}
	return st, nil
}

func buildHostStatement(month string, from, to time.Time, h *repository.StatementHost,
	sales []repository.StatementSale, payouts []repository.StatementPayout) *HostStatement {
	st := &HostStatement{Month: month, From: from, To: to, Lines: []StatementLine{}, Payouts: []StatementPayoutLine{},
		Host: StatementHostView{ID: h.ID, Name: h.Name, Email: h.Email, PANMasked: maskPAN(h.PAN)}}
	if h.GSTIN != nil {
		st.Host.GSTIN = *h.GSTIN
	}
	t := &st.Totals
	for _, x := range sales {
		l := StatementLine{Kind: x.Kind, ID: x.ID, Date: x.CreatedAt, EventTitle: x.EventTitle, Quantity: x.Quantity,
			TicketCents: x.TicketCents, Commission: x.TicketCents - x.NetEarningCents, TCSCents: x.TCSCents,
			TDSCents: x.TDSCents, PayableCents: x.PayableCents, Status: x.Status, Refunded: x.Returned}
		if l.Commission < 0 {
			l.Commission = 0 // older rows without a ticket split
		}
		st.Lines = append(st.Lines, l)
		t.Sales++
		t.TicketCents += l.TicketCents
		if l.Refunded {
			t.RefundedCents += l.TicketCents
			continue
		}
		t.NetTicketCents += l.TicketCents
		t.CommissionCents += l.Commission
		t.TCSCents += l.TCSCents
		t.TDSCents += l.TDSCents
		t.PayableCents += l.PayableCents
	}
	for _, p := range payouts {
		st.Payouts = append(st.Payouts, StatementPayoutLine{ID: p.ID, Date: p.CreatedAt, AmountCents: p.AmountCents,
			Status: p.Status, Reference: p.Reference})
		switch p.Status {
		case string(models.PaymentStatusCompleted):
			t.PaidOutCents += p.AmountCents
		case string(models.PaymentStatusPending), string(models.PaymentStatusProcessing):
			t.InFlightCents += p.AmountCents
		}
	}
	return st
}

// StatementFilename is e.g. "MySlotMate-statement-2026-11-Riya-Sen.pdf".
func StatementFilename(st *HostStatement) string {
	name := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, st.Host.Name)
	return fmt.Sprintf("MySlotMate-statement-%s-%s.pdf", st.Month, strings.Trim(name, "-"))
}

// RenderHostStatementPDF draws the statement (A4 landscape: the sales table is wide).
func RenderHostStatementPDF(st *HostStatement) ([]byte, error) {
	pdf := fpdf.New("L", "mm", "A4", "")
	pdf.SetCreator("MySlotMate", true)
	pdf.SetTitle("Payout statement "+st.Month, true)
	pdf.SetMargins(12, 12, 12)
	pdf.SetAutoPageBreak(true, 12)
	pdf.AddPage()
	tr := pdf.UnicodeTranslatorFromDescriptor("")
	const pageW = 273.0

	month := st.From.In(istZone).Format("January 2006")
	pdf.SetFont("Helvetica", "B", 15)
	pdf.CellFormat(pageW, 8, tr("PAYOUT STATEMENT · "+strings.ToUpper(month)), "", 1, "R", false, 0, "")
	top := pdf.GetY()

	// Issuer (left), host (right).
	iss := st.Issuer
	pdf.SetFont("Helvetica", "B", 10)
	issuerName := iss.LegalName
	if issuerName == "" {
		issuerName = "MySlotMate"
	}
	pdf.MultiCell(130, 5, tr(issuerName), "", "L", false)
	pdf.SetFont("Helvetica", "", 8.5)
	if iss.TradeName != "" && iss.TradeName != iss.LegalName {
		pdf.MultiCell(130, 4.2, tr("Trading as "+iss.TradeName), "", "L", false)
	}
	if iss.Address != "" {
		pdf.MultiCell(130, 4.2, tr(iss.Address), "", "L", false)
	}
	if iss.GSTIN != "" {
		pdf.MultiCell(130, 4.2, "GSTIN: "+iss.GSTIN, "", "L", false)
	}
	leftEnd := pdf.GetY()
	pdf.SetXY(160, top)
	pdf.SetFont("Helvetica", "B", 10)
	pdf.MultiCell(125, 5, tr("Host: "+st.Host.Name), "", "R", false)
	pdf.SetFont("Helvetica", "", 8.5)
	for _, s := range []string{st.Host.Email, prefixed("PAN: ", st.Host.PANMasked), prefixed("GSTIN: ", st.Host.GSTIN),
		"Period: " + st.From.In(istZone).Format("02 Jan 2006") + " - " + st.To.Add(-time.Second).In(istZone).Format("02 Jan 2006")} {
		if s != "" {
			pdf.SetX(160)
			pdf.MultiCell(125, 4.2, tr(s), "", "R", false)
		}
	}
	pdf.SetY(max(leftEnd, pdf.GetY()) + 5)

	// Summary.
	t := st.Totals
	summary := [][2]string{
		{"Ticket sales", money(t.TicketCents)},
		{"Refunded or cancelled", neg(t.RefundedCents)},
		{"Kept sales", money(t.NetTicketCents)},
		{"MySlotMate commission", neg(t.CommissionCents)},
		{"TCS withheld (GST)", neg(t.TCSCents)},
		{"TDS withheld (income tax)", neg(t.TDSCents)},
		{"Earned this month", "INR " + money(t.PayableCents)},
		{"Paid out this month", "INR " + money(t.PaidOutCents)},
	}
	if t.InFlightCents > 0 {
		summary = append(summary, [2]string{"Payouts on the way", "INR " + money(t.InFlightCents)})
	}
	pdf.SetFillColor(244, 244, 248)
	for i, r := range summary {
		bold := r[0] == "Earned this month"
		if bold {
			pdf.SetFont("Helvetica", "B", 9.5)
		} else {
			pdf.SetFont("Helvetica", "", 9)
		}
		x := 12.0
		if i%2 == 1 {
			x = 12 + pageW/2
		}
		pdf.SetX(x)
		pdf.CellFormat(pageW/2-40, 6, r[0], "", 0, "L", bold, 0, "")
		pdf.CellFormat(36, 6, r[1], "", 0, "R", bold, 0, "")
		if i%2 == 1 || i == len(summary)-1 {
			pdf.Ln(6)
		}
	}
	pdf.Ln(4)

	// Sales.
	pdf.SetFont("Helvetica", "B", 10)
	pdf.CellFormat(pageW, 6, fmt.Sprintf("Sales (%d)", t.Sales), "", 1, "L", false, 0, "")
	cols := []struct {
		h     string
		w     float64
		align string
	}{{"Date", 20, "L"}, {"Ref", 20, "L"}, {"Experience", 79, "L"}, {"Type", 18, "L"}, {"Qty", 10, "R"},
		{"Ticket", 24, "R"}, {"Commission", 22, "R"}, {"TCS", 18, "R"}, {"TDS", 18, "R"}, {"Earned", 24, "R"}, {"Status", 20, "L"}}
	pdf.SetFont("Helvetica", "B", 7.5)
	for _, c := range cols {
		pdf.CellFormat(c.w, 6, c.h, "B", 0, c.align, true, 0, "")
	}
	pdf.Ln(-1)
	pdf.SetFont("Helvetica", "", 7.5)
	if len(st.Lines) == 0 {
		pdf.CellFormat(pageW, 7, "No paid sales this month.", "", 1, "L", false, 0, "")
	}
	for _, l := range st.Lines {
		kind := "Ticket"
		if l.Kind == models.InvoiceSourcePass {
			kind = "Pass"
		}
		status := "Kept"
		earned := money(l.PayableCents)
		if l.Refunded {
			status, earned = "Refunded", "-"
		}
		title := l.EventTitle
		for pdf.GetStringWidth(tr(title)) > cols[2].w-2 && len(title) > 4 {
			title = strings.TrimSpace(title[:len(title)-4]) + "..."
		}
		vals := []string{l.Date.In(istZone).Format("02 Jan"), shortRef(l.ID.String()), tr(title), kind, fmt.Sprint(l.Quantity),
			money(l.TicketCents), money(l.Commission), money(l.TCSCents), money(l.TDSCents), earned, status}
		for i, c := range cols {
			pdf.CellFormat(c.w, 5.5, vals[i], "B", 0, c.align, false, 0, "")
		}
		pdf.Ln(-1)
	}
	pdf.Ln(4)

	// Payouts.
	pdf.SetFont("Helvetica", "B", 10)
	pdf.CellFormat(pageW, 6, "Payouts", "", 1, "L", false, 0, "")
	pdf.SetFont("Helvetica", "", 7.5)
	if len(st.Payouts) == 0 {
		pdf.CellFormat(pageW, 6, "No payouts this month.", "", 1, "L", false, 0, "")
	}
	for _, p := range st.Payouts {
		ref := p.Reference
		if ref == "" {
			ref = shortRef(p.ID.String())
		}
		pdf.CellFormat(30, 5.5, p.Date.In(istZone).Format("02 Jan 2006"), "B", 0, "L", false, 0, "")
		pdf.CellFormat(40, 5.5, ref, "B", 0, "L", false, 0, "")
		pdf.CellFormat(30, 5.5, strings.ToUpper(p.Status[:1])+p.Status[1:], "B", 0, "L", false, 0, "")
		pdf.CellFormat(30, 5.5, money(p.AmountCents), "B", 1, "R", false, 0, "")
	}

	pdf.Ln(6)
	pdf.SetFont("Helvetica", "", 7.5)
	pdf.SetTextColor(90, 90, 100)
	for _, n := range []string{
		"Earned = ticket price - MySlotMate commission (only when one applies) - TCS - TDS. The guest's platform fee is MySlotMate's and is not part of your sales.",
		"TCS and TDS are deposited with the government against your GSTIN / PAN. Claim them in your GST and income-tax returns.",
		"Sales are counted in the month they were made. This is a statement of account, not a tax invoice.",
	} {
		pdf.MultiCell(pageW, 3.8, tr(n), "", "L", false)
	}

	if pdf.Err() {
		return nil, pdf.Error()
	}
	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func prefixed(p, v string) string {
	if v == "" {
		return ""
	}
	return p + v
}

func neg(cents int64) string {
	if cents == 0 {
		return money(0)
	}
	return "-" + money(cents)
}
