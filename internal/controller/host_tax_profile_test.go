package controller

import (
	"testing"
	"time"

	"myslotmate-backend/internal/service"
)

// The web TaxDetails component (docs/backend-spec-boost-and-payouts.md in the
// frontend repo) expects an object even before a PAN exists, with nulls.
func TestHostTaxProfileDTO(t *testing.T) {
	empty := hostTaxProfileDTO(nil)
	if empty.EntityType != "individual" || empty.PANMasked != nil || empty.PANName != nil ||
		empty.GSTIN != nil || empty.GSTStateCode != nil || empty.UpdatedAt != nil {
		t.Errorf("no profile: got %+v", empty)
	}

	g := "27ABCDE1234F1Z5"
	now := time.Now()
	got := hostTaxProfileDTO(&service.TaxProfileView{EntityType: "company", PANMasked: "ABCDE****F", PANName: "Asha Co", GSTIN: &g, UpdatedAt: now})
	if got.EntityType != "company" || *got.PANMasked != "ABCDE****F" || *got.PANName != "Asha Co" ||
		*got.GSTIN != g || *got.GSTStateCode != "27" || !got.UpdatedAt.Equal(now) {
		t.Errorf("profile: got %+v", got)
	}
}
