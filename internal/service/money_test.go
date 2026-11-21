package service

import (
	"context"
	"testing"

	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"

	"github.com/google/uuid"
)

// Fakes embed the real interfaces so only the handful of methods the money
// helpers actually call need implementing; anything else would panic loudly.
type fakeLedger struct {
	repository.TransactionLedgerRepository
	entries []*models.TransactionLedger
}

func (f *fakeLedger) Create(ctx context.Context, e *models.TransactionLedger) (*models.TransactionLedger, error) {
	f.entries = append(f.entries, e)
	return e, nil
}

func (f *fakeLedger) GetByIdempotencyKey(ctx context.Context, key string) (*models.TransactionLedger, error) {
	for _, e := range f.entries {
		if e.IdempotencyKey != nil && *e.IdempotencyKey == key {
			return e, nil
		}
	}
	return nil, nil
}

func (f *fakeLedger) sum() int64 {
	var total int64
	for _, e := range f.entries {
		total += e.AmountCents
	}
	return total
}

type fakePayout struct {
	repository.PayoutRepository
	earnings, pending int64
}

func (f *fakePayout) IncrementEarnings(ctx context.Context, hostID uuid.UUID, cents int64) error {
	f.earnings += cents
	return nil
}
func (f *fakePayout) AddPendingClearance(ctx context.Context, hostID uuid.UUID, cents int64) error {
	f.pending += cents
	return nil
}
func (f *fakePayout) DecrementEarnings(ctx context.Context, hostID uuid.UUID, cents int64) error {
	f.earnings -= cents
	return nil
}
func (f *fakePayout) ClearPending(ctx context.Context, hostID uuid.UUID, cents int64) error {
	f.pending -= cents
	return nil
}

type fakeAccounts struct {
	repository.AccountRepository
	credited int64
}

func (f *fakeAccounts) Credit(ctx context.Context, accountID uuid.UUID, cents int64) error {
	f.credited += cents
	return nil
}

type fakePayments struct {
	repository.PaymentRepository
	created []*models.Payment
}

func (f *fakePayments) Create(ctx context.Context, p *models.Payment) error {
	f.created = append(f.created, p)
	return nil
}

// A purchase and its cancellation must leave the ledger exactly where it
// started: every entry summed across all accounts nets to zero. This is the
// invariant reconciliation checks (skill golden rule 8) — if a future edit
// drops or mis-signs an entry, this fails.
func TestCreditThenReverseNetsToZero(t *testing.T) {
	ctx := context.Background()
	const total, hostEarning, platformFee = 250000, 212500, 37500 // ₹2,500 pass at 15%

	ledger := &fakeLedger{}
	payout := &fakePayout{}
	accounts := &fakeAccounts{}
	payments := &fakePayments{}

	refID := uuid.New()
	if err := creditSplit(ctx, ledger, payout, purchaseSplit{
		UserID:             uuid.New(),
		UserAccountID:      uuid.New(),
		PlatformAccountID:  uuid.New(),
		HostAccountID:      uuid.New(),
		HostID:             uuid.New(),
		TotalCents:         total,
		HostEarningCents:   hostEarning,
		ReferenceID:        refID,
		ReferenceType:      "pass",
		IdempotencyKey:     "pass_test_1",
		PlatformPercentage: 15,
	}); err != nil {
		t.Fatalf("creditSplit: %v", err)
	}

	if len(ledger.entries) != 4 {
		t.Fatalf("expected 4 ledger entries, got %d", len(ledger.entries))
	}
	if got := ledger.sum(); got != 0 {
		t.Fatalf("forward entries must net to zero, got %d", got)
	}
	if payout.earnings != hostEarning || payout.pending != hostEarning {
		t.Fatalf("host earnings/pending = %d/%d, want %d/%d", payout.earnings, payout.pending, hostEarning, hostEarning)
	}

	if err := reverseSplit(ctx, ledger, accounts, payments, payout, refundSplit{
		UserID:          uuid.New(),
		UserAccountID:   uuid.New(),
		HostID:          uuid.New(),
		HostAccountID:   uuid.New(),
		PlatformAccount: &models.Account{ID: uuid.New()},
		AmountCents:     total,
		NetEarningCents: hostEarning,
		ServiceFeeCents: platformFee,
		ReferenceID:     refID,
		ReferenceType:   "pass",
		Noun:            "monthly pass",
	}); err != nil {
		t.Fatalf("reverseSplit: %v", err)
	}

	if got := ledger.sum(); got != 0 {
		t.Fatalf("purchase + reversal must net to zero, got %d", got)
	}
	if accounts.credited != total {
		t.Fatalf("buyer refunded %d, want %d", accounts.credited, total)
	}
	if payout.earnings != 0 || payout.pending != 0 {
		t.Fatalf("host earnings/pending after reversal = %d/%d, want 0/0", payout.earnings, payout.pending)
	}
	if len(payments.created) != 1 {
		t.Fatalf("expected one refund payment row, got %d", len(payments.created))
	}
}

// Refunding twice must be refused by the ledger idempotency guard, not silently
// double-credit the buyer.
func TestReverseSplitRefusesSecondRefund(t *testing.T) {
	ctx := context.Background()
	ledger := &fakeLedger{}
	accounts := &fakeAccounts{}
	payments := &fakePayments{}
	payout := &fakePayout{}

	split := refundSplit{
		UserID:        uuid.New(),
		UserAccountID: uuid.New(),
		AmountCents:   100000,
		ReferenceID:   uuid.New(),
		ReferenceType: "pass",
		Noun:          "monthly pass",
	}
	if err := reverseSplit(ctx, ledger, accounts, payments, payout, split); err != nil {
		t.Fatalf("first reverseSplit: %v", err)
	}
	if err := reverseSplit(ctx, ledger, accounts, payments, payout, split); err != ErrAlreadyRefunded {
		t.Fatalf("second reverseSplit = %v, want ErrAlreadyRefunded", err)
	}
	if accounts.credited != 100000 {
		t.Fatalf("buyer credited %d, want a single 100000", accounts.credited)
	}
}

// A taxed booking credits the host only host_payout (net − TCS − TDS); the
// platform holds the tax. Its cancellation must still net to zero on every
// account, using the amounts bookingReversalAmounts derives from the row.
func TestReverseSplit_TaxedBooking_Balances(t *testing.T) {
	ctx := context.Background()
	// ₹799 ticket, 8% guest fee ₹63.92, company host with GSTIN.
	const ticket, fee, tcs, tds = 79900, 6392, 400, 80
	total, net := int64(ticket+fee), int64(ticket)
	payout := net - tcs - tds // 79420

	b := &models.Booking{AmountCents: &total, ServiceFeeCents: ptr64(fee), NetEarningCents: &net,
		TCSCents: tcs, TDSCents: tds, HostPayoutCents: &payout}

	ledger, pay, accounts, payments := &fakeLedger{}, &fakePayout{}, &fakeAccounts{}, &fakePayments{}
	userAcc, platformAcc, hostAcc, refID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if err := creditSplit(ctx, ledger, pay, purchaseSplit{
		UserID: uuid.New(), UserAccountID: userAcc, PlatformAccountID: platformAcc,
		HostAccountID: hostAcc, HostID: uuid.New(), TotalCents: total, HostEarningCents: b.PayableCents(),
		ReferenceID: refID, ReferenceType: "event", IdempotencyKey: "taxed_1",
	}); err != nil {
		t.Fatalf("creditSplit: %v", err)
	}

	hostBack, platformBack := bookingReversalAmounts(b)
	if hostBack != 79420 || platformBack != 6872 {
		t.Fatalf("reversal amounts = host %d platform %d, want 79420 / 6872", hostBack, platformBack)
	}
	if err := reverseSplit(ctx, ledger, accounts, payments, pay, refundSplit{
		UserID: uuid.New(), UserAccountID: userAcc, HostID: uuid.New(), HostAccountID: hostAcc,
		PlatformAccount: &models.Account{ID: platformAcc}, AmountCents: total,
		NetEarningCents: hostBack, ServiceFeeCents: platformBack,
		ReferenceID: refID, ReferenceType: "booking", Noun: "booking",
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
	if pay.earnings != 0 || pay.pending != 0 {
		t.Errorf("host earnings/pending = %d/%d, want 0/0", pay.earnings, pay.pending)
	}
}

func ptr64(v int64) *int64 { return &v }
