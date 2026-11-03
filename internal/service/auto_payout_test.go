package service

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"myslotmate-backend/internal/lib/timeutil"
	"myslotmate-backend/internal/models"
)

func TestAutoPayoutDue(t *testing.T) {
	cases := []struct {
		at   time.Time
		want bool
	}{
		{time.Date(2026, 11, 3, 9, 59, 0, 0, timeutil.IST), false}, // Tue before window
		{time.Date(2026, 11, 3, 10, 0, 0, 0, timeutil.IST), true},  // Tue window opens
		{time.Date(2026, 11, 3, 23, 59, 0, 0, timeutil.IST), true}, // Tue late
		{time.Date(2026, 11, 4, 0, 0, 0, 0, timeutil.IST), false},  // Wed
		{time.Date(2026, 11, 2, 23, 0, 0, 0, timeutil.IST), false}, // Mon
		// 04:30 UTC Tue = 10:00 IST Tue; 20:00 UTC Mon = 01:30 IST Tue (too early).
		{time.Date(2026, 11, 3, 4, 30, 0, 0, time.UTC), true},
		{time.Date(2026, 11, 2, 20, 0, 0, 0, time.UTC), false},
	}
	for _, c := range cases {
		if got := AutoPayoutDue(c.at); got != c.want {
			t.Errorf("AutoPayoutDue(%s) = %v, want %v", c.at, got, c.want)
		}
	}
}

func TestAutoPayoutKeyIsWeekly(t *testing.T) {
	h := uuid.New()
	tue10 := time.Date(2026, 11, 3, 10, 0, 0, 0, timeutil.IST)
	if autoPayoutKey(h, tue10) != autoPayoutKey(h, tue10.Add(13*time.Hour)) {
		t.Error("same Tuesday must give the same key")
	}
	if autoPayoutKey(h, tue10) == autoPayoutKey(h, tue10.AddDate(0, 0, 7)) {
		t.Error("next Tuesday must give a new key")
	}
	if autoPayoutKey(h, tue10) == autoPayoutKey(uuid.New(), tue10) {
		t.Error("different hosts must give different keys")
	}
}

func TestPickBankMethod(t *testing.T) {
	upiPrimary := &models.PayoutMethod{ID: uuid.New(), Type: models.PayoutMethodUPI, IsVerified: true, IsPrimary: true}
	unverifiedBank := &models.PayoutMethod{ID: uuid.New(), Type: models.PayoutMethodBank}
	bank := &models.PayoutMethod{ID: uuid.New(), Type: models.PayoutMethodBank, IsVerified: true}
	primaryBank := &models.PayoutMethod{ID: uuid.New(), Type: models.PayoutMethodBank, IsVerified: true, IsPrimary: true}

	if got := pickBankMethod([]*models.PayoutMethod{upiPrimary, unverifiedBank}); got != nil {
		t.Errorf("no verified bank: got %v, want nil", got.ID)
	}
	if got := pickBankMethod([]*models.PayoutMethod{upiPrimary, unverifiedBank, bank}); got != bank {
		t.Error("UPI primary: want the verified bank fallback")
	}
	if got := pickBankMethod([]*models.PayoutMethod{bank, primaryBank}); got != primaryBank {
		t.Error("want the primary bank over a non-primary one")
	}
}
