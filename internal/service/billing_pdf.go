package service

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"myslotmate-backend/internal/lib/gststate"
	"myslotmate-backend/internal/models"

	"github.com/go-pdf/fpdf"
)

// RenderInvoicePDF draws a tax invoice or credit note from its stored row. The
// row holds a copy of everything printed, so the same row always gives the
// same document. Core fonts only (no ₹ glyph): amounts print as "INR".
func RenderInvoicePDF(inv *models.Invoice) ([]byte, error) {
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetCreator("MySlotMate", true)
	pdf.SetTitle(inv.Number, true)
	pdf.SetMargins(15, 15, 15)
	pdf.SetAutoPageBreak(true, 15)
	pdf.AddPage()
	tr := pdf.UnicodeTranslatorFromDescriptor("") // cp1252: keeps "·", "–" and accents readable
	const pageW = 180.0

	title := "TAX INVOICE"
	if inv.Kind == models.InvoiceKindCreditNote {
		title = "CREDIT NOTE"
	}

	// ── Supplier (left) and document details (right) ─────────────────
	sup := inv.Supplier
	pdf.SetFont("Helvetica", "B", 16)
	pdf.CellFormat(pageW, 8, title, "", 1, "R", false, 0, "")
	top := pdf.GetY()

	pdf.SetFont("Helvetica", "B", 11)
	pdf.MultiCell(100, 5, tr(sup.LegalName), "", "L", false)
	pdf.SetFont("Helvetica", "", 9)
	if sup.TradeName != "" && sup.TradeName != sup.LegalName {
		pdf.MultiCell(100, 4.5, tr("Trading as "+sup.TradeName), "", "L", false)
	}
	pdf.MultiCell(100, 4.5, tr(sup.Address), "", "L", false)
	pdf.MultiCell(100, 4.5, "GSTIN: "+sup.GSTIN, "", "L", false)
	if sup.PAN != "" {
		pdf.MultiCell(100, 4.5, "PAN: "+sup.PAN, "", "L", false)
	}
	if sup.Email != "" {
		pdf.MultiCell(100, 4.5, tr(sup.Email), "", "L", false)
	}
	leftEnd := pdf.GetY()

	pdf.SetXY(120, top)
	details := [][2]string{
		{"Number", inv.Number},
		{"Date", inv.IssuedAt.In(istZone).Format("02 Jan 2006")},
		{"Place of supply", placeOfSupply(inv.PlaceOfSupply)},
	}
	if inv.Period != nil && *inv.Period != "" {
		if t, err := time.ParseInLocation("2006-01", *inv.Period, istZone); err == nil {
			details = append(details, [2]string{"Period", t.Format("January 2006")})
		}
	}
	if inv.Kind == models.InvoiceKindCreditNote && inv.OriginalNumber != "" {
		details = append(details, [2]string{"Against invoice", inv.OriginalNumber})
	}
	details = append(details, [2]string{"Reverse charge", "No"})
	for _, d := range details {
		pdf.SetX(120)
		pdf.SetFont("Helvetica", "", 9)
		pdf.CellFormat(28, 5, d[0], "", 0, "L", false, 0, "")
		pdf.SetFont("Helvetica", "B", 9)
		pdf.CellFormat(47, 5, tr(d[1]), "", 1, "R", false, 0, "")
	}
	pdf.SetY(max(leftEnd, pdf.GetY()) + 6)

	// ── Buyer and what it was for ────────────────────────────────────
	pdf.SetFont("Helvetica", "B", 9)
	pdf.CellFormat(pageW, 5, "Billed to", "", 1, "L", false, 0, "")
	pdf.SetFont("Helvetica", "", 9)
	buyer := inv.BuyerName
	if buyer == "" {
		buyer = "Guest"
	}
	pdf.MultiCell(pageW, 4.5, tr(buyer), "", "L", false)
	contact := strings.Trim(strings.Join([]string{inv.BuyerEmail, inv.BuyerPhone}, " · "), " ·")
	if contact != "" {
		pdf.MultiCell(pageW, 4.5, tr(contact), "", "L", false)
	}
	switch {
	case inv.BuyerGSTIN != nil && *inv.BuyerGSTIN != "":
		pdf.MultiCell(pageW, 4.5, "GSTIN: "+*inv.BuyerGSTIN, "", "L", false)
	case inv.SourceType == models.InvoiceSourceHostCommission:
		pdf.MultiCell(pageW, 4.5, "Unregistered", "", "L", false)
	default:
		pdf.MultiCell(pageW, 4.5, "Unregistered (consumer)", "", "L", false)
	}
	if inv.SourceType == models.InvoiceSourceHostCommission {
		pdf.Ln(2)
		pdf.MultiCell(pageW, 4.5, tr(inv.EventTitle), "", "L", false)
	} else if inv.EventTitle != "" {
		pdf.Ln(2)
		what := "Booking"
		if inv.SourceType == models.InvoiceSourcePass {
			what = "Monthly pass"
		}
		pdf.MultiCell(pageW, 4.5, tr(fmt.Sprintf("%s: %s (ref. %s)", what, inv.EventTitle, shortRef(inv.SourceID.String()))), "", "L", false)
	}
	pdf.Ln(5)

	// ── Lines ────────────────────────────────────────────────────────
	intra := inv.IGSTCents == 0
	rate := 0
	if len(inv.Lines) > 0 {
		rate = inv.Lines[0].GSTRateBps
	}
	type col struct {
		head  string
		w     float64
		align string
	}
	cols := []col{{"#", 8, "C"}, {"Description", 72, "L"}, {"SAC", 18, "C"}, {"Taxable value", 27, "R"}}
	if intra {
		cols = append(cols, col{"CGST " + pct(rate/2), 18, "R"}, col{"SGST " + pct(rate-rate/2), 18, "R"})
	} else {
		cols = append(cols, col{"IGST " + pct(rate), 36, "R"})
	}
	cols = append(cols, col{"Total", 19, "R"})

	pdf.SetFillColor(240, 240, 245)
	pdf.SetFont("Helvetica", "B", 8)
	for _, c := range cols {
		pdf.CellFormat(c.w, 7, c.head, "1", 0, c.align, true, 0, "")
	}
	pdf.Ln(-1)
	pdf.SetFont("Helvetica", "", 8)
	for _, l := range inv.Lines {
		vals := []string{fmt.Sprint(l.Position), l.Description, l.SACCode, money(l.TaxableCents)}
		if intra {
			vals = append(vals, money(l.CGSTCents), money(l.SGSTCents))
		} else {
			vals = append(vals, money(l.IGSTCents))
		}
		vals = append(vals, money(l.TotalCents))
		// The description may wrap; size the row to it.
		lines := pdf.SplitLines([]byte(tr(l.Description)), cols[1].w-2) // bytes: cp1252-safe
		h := 6.0 * float64(max(1, len(lines)))
		x, y := pdf.GetXY()
		for i, c := range cols {
			if i == 1 {
				pdf.Rect(x, y, c.w, h, "D")
				pdf.SetXY(x+1, y+1)
				pdf.MultiCell(c.w-2, 4.5, tr(vals[i]), "", "L", false)
			} else {
				pdf.SetXY(x, y)
				pdf.CellFormat(c.w, h, vals[i], "1", 0, c.align, false, 0, "")
			}
			x += c.w
		}
		pdf.SetXY(15, y+h)
	}

	// ── Totals ───────────────────────────────────────────────────────
	pdf.Ln(3)
	totals := [][2]string{{"Taxable value", money(inv.TaxableCents)}}
	if intra {
		totals = append(totals, [2]string{"CGST", money(inv.CGSTCents)}, [2]string{"SGST", money(inv.SGSTCents)})
	} else {
		totals = append(totals, [2]string{"IGST", money(inv.IGSTCents)})
	}
	label := "Total"
	if inv.Kind == models.InvoiceKindCreditNote {
		label = "Total credited"
	}
	totals = append(totals, [2]string{label, "INR " + money(inv.TotalCents)})
	for i, t := range totals {
		last := i == len(totals)-1
		if last {
			pdf.SetFont("Helvetica", "B", 10)
		} else {
			pdf.SetFont("Helvetica", "", 9)
		}
		pdf.SetX(15 + pageW - 80)
		pdf.CellFormat(45, 6, t[0], "", 0, "L", false, 0, "")
		pdf.CellFormat(35, 6, t[1], "", 1, "R", false, 0, "")
	}

	// ── Notes ────────────────────────────────────────────────────────
	pdf.Ln(8)
	pdf.SetFont("Helvetica", "", 8)
	pdf.SetTextColor(90, 90, 100)
	note := "This invoice is for the platform fee only. The fee includes GST. The ticket price is paid to the host and is not part of this invoice."
	if inv.Kind == models.InvoiceKindCreditNote {
		note = "This credit note reverses the platform fee of the invoice above, which was refunded to the guest."
	}
	if inv.SourceType == models.InvoiceSourceHostCommission {
		note = "Marketing Boost commission on tickets sold while your Boost was live, already deducted from your payouts. The commission includes GST."
		if inv.BuyerGSTIN != nil && *inv.BuyerGSTIN != "" {
			note += " As a GST-registered business you can claim this GST as input tax credit."
		}
		if inv.Kind == models.InvoiceKindCreditNote {
			note = "This credit note reverses the Marketing Boost commission on bookings that were refunded after the invoice above."
		}
	}
	pdf.MultiCell(pageW, 4, tr(note), "", "L", false)
	pdf.MultiCell(pageW, 4, "This is a computer-generated document and needs no signature.", "", "L", false)

	if pdf.Err() {
		return nil, pdf.Error()
	}
	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func placeOfSupply(code string) string {
	if n := gststate.Name(code); n != "" {
		return fmt.Sprintf("%s (%s)", n, code)
	}
	return code
}

func shortRef(id string) string {
	if len(id) >= 8 {
		return strings.ToUpper(id[:8])
	}
	return id
}

// pct prints basis points as a percentage: 900 -> "9%", 250 -> "2.5%".
func pct(bps int) string {
	if bps%100 == 0 {
		return fmt.Sprintf("%d%%", bps/100)
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", float64(bps)/100), "0"), ".") + "%"
}

// money prints paise in Indian grouping: 12345678 -> "1,23,456.78".
func money(cents int64) string {
	neg := cents < 0
	if neg {
		cents = -cents
	}
	rupees, paise := cents/100, cents%100
	s := fmt.Sprint(rupees)
	if len(s) > 3 {
		head, tail := s[:len(s)-3], s[len(s)-3:]
		var parts []string
		for len(head) > 2 {
			parts = append([]string{head[len(head)-2:]}, parts...)
			head = head[:len(head)-2]
		}
		if head != "" {
			parts = append([]string{head}, parts...)
		}
		s = strings.Join(append(parts, tail), ",")
	}
	out := fmt.Sprintf("%s.%02d", s, paise)
	if neg {
		return "-" + out
	}
	return out
}
