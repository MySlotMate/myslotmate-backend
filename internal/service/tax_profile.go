package service

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"myslotmate-backend/internal/lib/timeutil"
	"myslotmate-backend/internal/models"
)

// ErrTaxProfileRequired: a withdrawal (or auto payout) needs a PAN on file
// first. The HTTP body carries this exact string; clients match on it to open
// the tax form.
var ErrTaxProfileRequired = errors.New("tax_profile_required")

var (
	panRe   = regexp.MustCompile(`^[A-Z]{5}[0-9]{4}[A-Z]$`)
	gstinRe = regexp.MustCompile(`^[0-9]{2}[A-Z0-9]{10}[0-9A-Z]{3}$`)
)

var entityTypes = map[string]bool{
	models.EntityIndividual: true, models.EntityHUF: true, models.EntityCompany: true,
	models.EntityFirm: true, models.EntityLLP: true, models.EntityOther: true,
}

// TaxProfileInput is what the host submits. gst_state_code is never accepted
// from the client; it is derived from the GSTIN.
type TaxProfileInput struct {
	EntityType string  `json:"entity_type"`
	PAN        string  `json:"pan"`
	PANName    string  `json:"pan_name"`
	GSTIN      *string `json:"gstin,omitempty"`
}

// NormalizeTaxProfile upper-cases and trims the input, validates it (same
// rules as the host_tax_profiles CHECK constraints), and derives the GST state.
func NormalizeTaxProfile(in TaxProfileInput) (*models.TaxProfile, error) {
	p := &models.TaxProfile{
		EntityType: strings.TrimSpace(strings.ToLower(in.EntityType)),
		PAN:        strings.ToUpper(strings.TrimSpace(in.PAN)),
		PANName:    strings.TrimSpace(in.PANName),
	}
	if !entityTypes[p.EntityType] {
		return nil, errors.New("entity_type must be one of individual, huf, company, firm, llp, other")
	}
	if !panRe.MatchString(p.PAN) {
		return nil, errors.New("PAN must look like ABCDE1234F")
	}
	if p.PANName == "" {
		return nil, errors.New("name as on PAN is required")
	}
	if in.GSTIN != nil {
		g := strings.ToUpper(strings.TrimSpace(*in.GSTIN))
		if g != "" {
			if !gstinRe.MatchString(g) {
				return nil, errors.New("GSTIN must be 15 characters, like 27ABCDE1234F1Z5")
			}
			if g[2:12] != p.PAN {
				return nil, errors.New("GSTIN does not belong to this PAN")
			}
			state := g[:2]
			p.GSTIN, p.GSTStateCode = &g, &state
		}
	}
	return p, nil
}

// MaskPAN hides the middle digits: ABCDE1234F → ABCDE****F.
func MaskPAN(pan string) string {
	if len(pan) != 10 {
		return "**********"
	}
	return pan[:5] + "****" + pan[9:]
}

// FinancialYearStart is 1 April 00:00 IST of the Indian financial year that
// contains now.
func FinancialYearStart(now time.Time) time.Time {
	t := now.In(timeutil.IST)
	year := t.Year()
	if t.Month() < time.April {
		year--
	}
	return time.Date(year, time.April, 1, 0, 0, 0, 0, timeutil.IST)
}
