package service

import (
	"context"
	"fmt"
	"time"

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
