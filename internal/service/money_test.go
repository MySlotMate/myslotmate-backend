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
