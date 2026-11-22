package service

import (
	"context"
	"testing"
	"time"

	"myslotmate-backend/internal/models"

	"github.com/google/uuid"
)

// fakeTaxPayout adds the reads stampTax makes under the host lock.
type fakeTaxPayout struct {
	fakePayout
	profile *models.TaxProfile
	fyGross int64
	locked  bool
}

func (f *fakeTaxPayout) LockHost(ctx context.Context, hostID uuid.UUID) error {
	f.locked = true
	return nil
}
func (f *fakeTaxPayout) GetTaxProfile(ctx context.Context, hostID uuid.UUID) (*models.TaxProfile, error) {
	return f.profile, nil
}
func (f *fakeTaxPayout) FYGrossTicketCents(ctx context.Context, hostID uuid.UUID, fyStart time.Time) (int64, error) {
	return f.fyGross, nil
}

var taxOn = models.TaxSettings{Enabled: true, TCSBps: 50, TDSBps: 10, TDSThresholdCents: 50000000}

// sellPass runs the pass purchase money path the way PurchasePass does: tax on
// the pass price only (not the guest fee), host credited host_payout. Returns
// the pass as it would be stored, plus the ledger and payout fakes.
func sellPass(t *testing.T, pay *fakeTaxPayout, price, bookingFee int64, settings models.TaxSettings) (*models.UserPass, *fakeLedger, [3]uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	hostEarning, platformFee := price, bookingFee // 0% host commission, the default
	tax, err := stampTax(ctx, pay, uuid.New(), TaxInput{TicketCents: price, NetEarningCents: hostEarning,
		ServiceFeeCents: platformFee, Settings: settings})
	if err != nil {
		t.Fatalf("stampTax: %v", err)
	}
	p := &models.UserPass{ID: uuid.New(), PriceCents: price + bookingFee, ServiceFeeCents: platformFee,
		NetEarningCents: hostEarning, TicketCents: &price, TCSCents: tax.TCSCents, TDSCents: tax.TDSCents,
		HostPayoutCents: &tax.HostPayoutCents}
	accs := [3]uuid.UUID{uuid.New(), uuid.New(), uuid.New()} // user, platform, host
	ledger := &fakeLedger{}
	if err := creditSplit(ctx, ledger, pay, purchaseSplit{
		UserID: uuid.New(), UserAccountID: accs[0], PlatformAccountID: accs[1], HostAccountID: accs[2],
		HostID: uuid.New(), TotalCents: p.PriceCents, HostEarningCents: tax.HostPayoutCents,
		ReferenceID: p.ID, ReferenceType: "pass", IdempotencyKey: "pass_" + p.ID.String(),
	}); err != nil {
		t.Fatalf("creditSplit: %v", err)
	}
	return p, ledger, accs
}

func TestPassTax_Off_AmountsUnchanged(t *testing.T) {
	pay := &fakeTaxPayout{}
	p, _, _ := sellPass(t, pay, 250000, 20000, models.TaxSettings{TCSBps: 50, TDSBps: 10})
	if pay.locked {
		t.Error("tax off must not lock the host")
	}
	if *p.HostPayoutCents != p.NetEarningCents || p.TCSCents != 0 || p.TDSCents != 0 {
		t.Errorf("tax off: payout %d tcs %d tds %d, want %d/0/0", *p.HostPayoutCents, p.TCSCents, p.TDSCents, p.NetEarningCents)
	}
	if pay.earnings != 250000 {
		t.Errorf("host credited %d, want 250000", pay.earnings)
	}
	host, platform := passReversalAmounts(p)
	if host != 250000 || platform != 20000 {
		t.Errorf("reversal = %d/%d, want 250000/20000", host, platform)
	}
}

func TestPassTax_GSTINHost_TCSOnPriceOnly(t *testing.T) {
	gstin, st := "18ABCDE1234F1Z5", "18"
	pay := &fakeTaxPayout{profile: &models.TaxProfile{EntityType: models.EntityIndividual, GSTIN: &gstin, GSTStateCode: &st}}
	p, _, _ := sellPass(t, pay, 100000, 8000, taxOn)
	if !pay.locked {
		t.Error("tax on must lock the host")
	}
	if p.TCSCents != 500 { // 0.5% of ₹1,000, not of ₹1,080 with the guest fee
		t.Errorf("tcs = %d, want 500", p.TCSCents)
	}
	if p.TDSCents != 0 || *p.HostPayoutCents != 99500 || pay.earnings != 99500 {
		t.Errorf("tds %d payout %d credited %d, want 0/99500/99500", p.TDSCents, *p.HostPayoutCents, pay.earnings)
	}
}

func TestPassTax_IndividualCrossingThreshold_CatchUpTDS(t *testing.T) {
	pay := &fakeTaxPayout{fyGross: 49950000} // ₹4,99,500 already sold this FY
	p, _, _ := sellPass(t, pay, 100000, 0, taxOn)
	if p.TDSCents != 50050 { // 0.1% of the whole ₹5,00,500 YTD
		t.Errorf("tds = %d, want 50050", p.TDSCents)
	}
	if pay.earnings != 100000-50050 {
		t.Errorf("host credited %d, want %d", pay.earnings, 100000-50050)
	}
}

// Cancelling a taxed pass, including one whose catch-up TDS left a negative
// host share, must net every account's ledger to zero.
func TestPassTax_Cancel_NetsToZeroPerAccount(t *testing.T) {
	gstin := "18ABCDE1234F1Z5"
	cases := []struct {
		name         string
		pay          *fakeTaxPayout
		price, fee   int64
		wantNegative bool
	}{
		{"gstin company", &fakeTaxPayout{profile: &models.TaxProfile{EntityType: models.EntityCompany, GSTIN: &gstin}}, 250000, 20000, false},
		{"negative host share", &fakeTaxPayout{fyGross: 49999000}, 2000, 160, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, ledger, accs := sellPass(t, c.pay, c.price, c.fee, taxOn)
			if got := *p.HostPayoutCents < 0; got != c.wantNegative {
				t.Fatalf("host payout %d, negative=%v want %v", *p.HostPayoutCents, got, c.wantNegative)
			}
			host, platform := passReversalAmounts(p)
			if host+platform != p.PriceCents {
				t.Fatalf("reversal %d+%d != price %d", host, platform, p.PriceCents)
			}
			if err := reverseSplit(context.Background(), ledger, &fakeAccounts{}, &fakePayments{}, c.pay, refundSplit{
				UserID: uuid.New(), UserAccountID: accs[0], HostID: uuid.New(), HostAccountID: accs[2],
				PlatformAccount: &models.Account{ID: accs[1]}, AmountCents: p.PriceCents,
				NetEarningCents: host, ServiceFeeCents: platform,
				ReferenceID: p.ID, ReferenceType: "pass", Noun: "monthly pass",
			}); err != nil {
				t.Fatalf("reverseSplit: %v", err)
			}
			perAccount := map[uuid.UUID]int64{}
			for _, e := range ledger.entries {
				perAccount[e.AccountID] += e.AmountCents
			}
			for acc, sum := range perAccount {
				if sum != 0 {
					t.Errorf("account %s nets to %d after cancel, want 0", acc, sum)
				}
			}
			if c.pay.earnings != 0 || c.pay.pending != 0 {
				t.Errorf("host earnings/pending = %d/%d, want 0/0", c.pay.earnings, c.pay.pending)
			}
		})
	}
}
