package service

import (
	"context"
	"fmt"
	"time"

	"myslotmate-backend/internal/lib/timeutil"
	"myslotmate-backend/internal/repository"

	"github.com/google/uuid"
)

// TaxProfileView is what a host sees of their own tax profile. The PAN is
// masked: the full value never leaves the server on a host-facing route.
type TaxProfileView struct {
	EntityType string    `json:"entity_type"`
	PANMasked  string    `json:"pan_masked"`
	PANName    string    `json:"pan_name"`
	GSTIN      *string   `json:"gstin,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// TaxStatements is a host's tax for one financial year.
type TaxStatements struct {
	FY       int                   `json:"fy"` // 2026 = 1 Apr 2026 – 31 Mar 2027
	Months   []repository.MonthTax `json:"months"`
	Quarters []QuarterTDS          `json:"quarters"`
}

// QuarterTDS is TDS per quarter of the FY (Q1 = Apr–Jun), matching the
// quarterly TDS returns and Form 16A.
type QuarterTDS struct {
	Quarter  string `json:"quarter"`
	TDSCents int64  `json:"tds_cents"`
}

func (s *payoutService) GetTaxProfile(ctx context.Context, hostID uuid.UUID) (*TaxProfileView, error) {
	p, err := s.payoutRepo.GetTaxProfile(ctx, hostID)
	if err != nil || p == nil {
		return nil, err
	}
	return &TaxProfileView{EntityType: p.EntityType, PANMasked: MaskPAN(p.PAN), PANName: p.PANName, GSTIN: p.GSTIN, UpdatedAt: p.UpdatedAt}, nil
}

func (s *payoutService) SaveTaxProfile(ctx context.Context, hostID uuid.UUID, in TaxProfileInput) (*TaxProfileView, error) {
	p, err := NormalizeTaxProfile(in)
	if err != nil {
		return nil, err
	}
	p.HostID = hostID
	if err := s.payoutRepo.UpsertTaxProfile(ctx, p); err != nil {
		return nil, fmt.Errorf("save tax profile: %w", err)
	}
	return s.GetTaxProfile(ctx, hostID)
}

func (s *payoutService) GetTaxStatements(ctx context.Context, hostID uuid.UUID, fy int) (*TaxStatements, error) {
	from := FinancialYearStart(time.Date(fy, time.April, 1, 12, 0, 0, 0, time.UTC))
	months, err := s.payoutRepo.HostTaxByMonth(ctx, hostID, from, from.AddDate(1, 0, 0))
	if err != nil {
		return nil, fmt.Errorf("tax by month: %w", err)
	}
	return &TaxStatements{FY: fy, Months: months, Quarters: QuarterlyTDS(fy, months)}, nil
}

// QuarterlyTDS folds monthly TDS into the FY's four quarters.
func QuarterlyTDS(fy int, months []repository.MonthTax) []QuarterTDS {
	q := []QuarterTDS{{Quarter: "Q1"}, {Quarter: "Q2"}, {Quarter: "Q3"}, {Quarter: "Q4"}}
	for _, m := range months {
		var y, mo int
		if _, err := fmt.Sscanf(m.Month, "%d-%d", &y, &mo); err != nil {
			continue
		}
		idx := ((y-fy)*12 + mo - 4) / 3 // Apr..Jun → 0 … Jan..Mar next year → 3
		if idx >= 0 && idx < 4 {
			q[idx].TDSCents += m.TDSCents
		}
	}
	return q
}

// SplitTCS divides TCS into CGST + SGST (intra-state, half each, the odd
// paisa to CGST) or IGST (interstate).
func SplitTCS(tcsCents int64, interstate bool) (cgst, sgst, igst int64) {
	if interstate {
		return 0, 0, tcsCents
	}
	sgst = tcsCents / 2
	return tcsCents - sgst, sgst, 0
}

// platformWithdrawable is the platform balance less TCS/TDS stamped but not
// yet deposited with the government. That tax belongs to the government; the
// platform only holds it until finance pays the challan.
func platformWithdrawable(balanceCents, stampedTaxCents, depositedTaxCents int64) int64 {
	owed := stampedTaxCents - depositedTaxCents
	if owed < 0 {
		owed = 0
	}
	if w := balanceCents - owed; w > 0 {
		return w
	}
	return 0
}

// TaxReportCSV renders the monthly finance report for month "2026-11" (IST).
// Columns per the PRD Payout and tax spec §5 /admin/tax-report.
func (s *payoutService) TaxReportCSV(ctx context.Context, month string) ([][]string, error) {
	start, err := time.ParseInLocation("2006-01", month, timeutil.IST)
	if err != nil {
		return nil, fmt.Errorf("month must look like 2026-11")
	}
	rows, err := s.payoutRepo.TaxReportRows(ctx, start, start.AddDate(0, 1, 0))
	if err != nil {
		return nil, err
	}
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	out := [][]string{{"host_id", "host_name", "pan", "gstin", "gst_state_code", "entity_type",
		"gross_cents", "refunds_cents", "net_cents", "tcs_cgst_cents", "tcs_sgst_cents", "tcs_igst_cents",
		"tds_cents", "venue_state_unknown"}}
	for _, r := range rows {
		cgst, sgst, _ := SplitTCS(r.TCSIntraCents, false)
		out = append(out, []string{r.HostID.String(), r.HostName, str(r.PAN), str(r.GSTIN), str(r.GSTStateCode), str(r.EntityType),
			fmt.Sprint(r.GrossCents), fmt.Sprint(r.ReturnsCents), fmt.Sprint(r.GrossCents - r.ReturnsCents),
			fmt.Sprint(cgst), fmt.Sprint(sgst), fmt.Sprint(r.TCSInterCents),
			fmt.Sprint(r.TDSCents), fmt.Sprint(r.VenueStateUnknown)})
	}
	return out, nil
}

// AddTaxDeposit logs a TCS/TDS challan paid by finance.
func (s *payoutService) AddTaxDeposit(ctx context.Context, d *repository.TaxDeposit) error {
	if d.Kind != "tcs" && d.Kind != "tds" {
		return fmt.Errorf("kind must be tcs or tds")
	}
	if _, err := time.Parse("2006-01", d.Period); err != nil {
		return fmt.Errorf("period must look like 2026-11")
	}
	if d.AmountCents <= 0 {
		return fmt.Errorf("amount must be positive")
	}
	if d.DepositedAt.IsZero() {
		d.DepositedAt = time.Now()
	}
	return s.payoutRepo.CreateTaxDeposit(ctx, d)
}

// GetEventEarnings is the per-event earnings breakdown (gross, fee, TCS, TDS, earned).
func (s *payoutService) GetEventEarnings(ctx context.Context, hostID uuid.UUID) ([]repository.EventEarnings, error) {
	return s.payoutRepo.HostEventEarnings(ctx, hostID)
}
