package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"

	"github.com/google/uuid"
)

// The wallet split every purchase on this platform performs, and its exact
// reversal. Both live here, and only here, so a booking and a monthly pass move
// money through identical code — one place to read, one place to audit, and no
// chance of the two drifting apart.
//
// Balance discipline (skill golden rule 8): the four forward entries net to
// zero — user −total, platform +total, platform −hostEarning, host +hostEarning
// — and the three reversal entries undo exactly that. Host and platform
// accounts.balance_cents are deliberately never touched; host money lives in
// host_earnings. Only the user's wallet balance moves.

// purchaseSplit is one buyer→platform→host money movement.
type purchaseSplit struct {
	UserID            uuid.UUID
	UserAccountID     uuid.UUID
	PlatformAccountID uuid.UUID
	HostAccountID     uuid.UUID
	HostID            uuid.UUID

	TotalCents       int64 // what the buyer pays
	HostEarningCents int64 // total minus the platform's commission

	// ReferenceID/ReferenceType point the ledger at what was bought.
	ReferenceID    uuid.UUID
	ReferenceType  string
	IdempotencyKey string

	// Ledger line descriptions. PlatformFee's wording is shared by both callers.
	UserDescription     string
	PlatformDescription string
	HostDescription     string
	PlatformPercentage  int
}

// creditSplit writes the four ledger entries and bumps the host's earnings.
// It does NOT debit the buyer's wallet — the caller does that first, so an
// insufficient balance is rejected before any ledger row exists.
//
// Must be called inside the caller's transaction (pass the WithTx repos).
func creditSplit(
	ctx context.Context,
	ledgerTx repository.TransactionLedgerRepository,
	payoutTx repository.PayoutRepository,
	s purchaseSplit,
) error {
	// Ledger Entry 1: user debit — money flowing user → platform.
	userDebit, err := ledgerTx.Create(ctx, &models.TransactionLedger{
		ID:             uuid.New(),
		AccountID:      s.UserAccountID,
		Type:           models.LedgerTypeBookingCredit,
		AmountCents:    -s.TotalCents, // NEGATIVE = money out
		ReferenceID:    &s.ReferenceID,
		ReferenceType:  strPtr(s.ReferenceType),
		IdempotencyKey: strPtr(s.IdempotencyKey),
		Description:    strPtr(s.UserDescription),
		Status:         models.LedgerStatusCompleted,
		CreatedAt:      time.Now(),
		CreatedBy:      &s.UserID,
	})
	if err != nil {
		return fmt.Errorf("failed to create user debit ledger: %w", err)
	}

	// Ledger Entry 2: platform credit — receives the user payment.
	platformCredit, err := ledgerTx.Create(ctx, &models.TransactionLedger{
		ID:            uuid.New(),
		AccountID:     s.PlatformAccountID,
		Type:          models.LedgerTypeBookingCredit,
		AmountCents:   s.TotalCents, // POSITIVE = money in
		ReferenceID:   &userDebit.ID,
		ReferenceType: strPtr("ledger"),
		Description:   strPtr(s.PlatformDescription),
		Status:        models.LedgerStatusCompleted,
		CreatedAt:     time.Now(),
	})
	if err != nil {
		return fmt.Errorf("failed to create platform credit ledger: %w", err)
	}

	// Ledger Entry 3: platform disburses the host's share. Amount MUST be
	// -hostEarning (not -platformFee) so the four entries net to zero.
	if _, err := ledgerTx.Create(ctx, &models.TransactionLedger{
		ID:            uuid.New(),
		AccountID:     s.PlatformAccountID,
		Type:          models.LedgerTypePlatformFeeCredit,
		AmountCents:   -s.HostEarningCents, // NEGATIVE = host's share paid out of platform account
		ReferenceID:   &s.ReferenceID,
		ReferenceType: strPtr(s.ReferenceType),
		Description:   strPtr(fmt.Sprintf("Host earning disbursed (platform keeps %d%% commission)", s.PlatformPercentage)),
		Status:        models.LedgerStatusCompleted,
		CreatedAt:     time.Now(),
	}); err != nil {
		return fmt.Errorf("failed to create platform fee ledger: %w", err)
	}

	// Ledger Entry 4: host credit — the host's earning (pending settlement).
	if _, err := ledgerTx.Create(ctx, &models.TransactionLedger{
		ID:            uuid.New(),
		AccountID:     s.HostAccountID,
		Type:          models.LedgerTypeBookingCredit,
		AmountCents:   s.HostEarningCents, // POSITIVE = money reserved for host
		ReferenceID:   &platformCredit.ID,
		ReferenceType: strPtr("ledger"),
		Description:   strPtr(s.HostDescription),
		Status:        models.LedgerStatusCompleted,
		CreatedAt:     time.Now(),
	}); err != nil {
		return fmt.Errorf("failed to create host credit ledger: %w", err)
	}

	// Update host earnings aggregate.
	if err := payoutTx.IncrementEarnings(ctx, s.HostID, s.HostEarningCents); err != nil {
		return fmt.Errorf("failed to increment host earnings: %w", err)
	}
	if err := payoutTx.AddPendingClearance(ctx, s.HostID, s.HostEarningCents); err != nil {
		return fmt.Errorf("failed to add host pending clearance: %w", err)
	}
	return nil
}

// refundSplit is the undo of a purchaseSplit: what to give back, and to whom.
type refundSplit struct {
	UserID        uuid.UUID
	UserAccountID uuid.UUID // zero value when nothing is refundable

	HostID          uuid.UUID
	HostAccountID   uuid.UUID
	PlatformAccount *models.Account

	AmountCents     int64 // back to the buyer's wallet
	NetEarningCents int64 // taken off the host
	ServiceFeeCents int64 // taken off the platform

	ReferenceID   uuid.UUID
	ReferenceType string
	// Noun names the thing being reversed in ledger descriptions ("booking").
	Noun string
}

// ErrAlreadyRefunded means the reversal ran before — the ledger already holds
// the refund entry for this reference.
var ErrAlreadyRefunded = errors.New("already refunded")

// reverseSplit undoes a creditSplit: it refunds the buyer's wallet (ledger
// entry + balance credit + a refund payment row), reverses the host's earning
// and pending clearance, and reverses the platform's commission.
//
// Must be called inside the caller's transaction (pass the WithTx repos).
func reverseSplit(
	ctx context.Context,
	ledgerTx repository.TransactionLedgerRepository,
	accountTx repository.AccountRepository,
	paymentTx repository.PaymentRepository,
	payoutTx repository.PayoutRepository,
	s refundSplit,
) error {
	// ── Buyer refund ─────────────────────────────────────────────────────
	if s.AmountCents > 0 {
		// Idempotency guard: one refund_credit ledger entry per reference. If it
		// already exists this was already refunded — abort rather than refund
		// twice. (The ledger's UNIQUE(idempotency_key) is the hard guard; this
		// check just yields a friendlier error.)
		refundLedgerKey := "refund_credit_" + s.ReferenceID.String()
		if existing, err := ledgerTx.GetByIdempotencyKey(ctx, refundLedgerKey); err != nil {
			return fmt.Errorf("cancel: refund idempotency check: %w", err)
		} else if existing != nil {
			return ErrAlreadyRefunded
		}

		if _, err := ledgerTx.Create(ctx, &models.TransactionLedger{
			ID:             uuid.New(),
			AccountID:      s.UserAccountID,
			Type:           models.LedgerTypeRefundCredit,
			AmountCents:    s.AmountCents, // POSITIVE = money back into the user's wallet
			ReferenceID:    &s.ReferenceID,
			ReferenceType:  strPtr(s.ReferenceType),
			IdempotencyKey: &refundLedgerKey,
			Description:    strPtr(fmt.Sprintf("Refund for cancelled %s", s.Noun)),
			Status:         models.LedgerStatusCompleted,
			CreatedAt:      time.Now(),
			CreatedBy:      &s.UserID,
		}); err != nil {
			return fmt.Errorf("cancel: write refund ledger entry: %w", err)
		}

		if err := accountTx.Credit(ctx, s.UserAccountID, s.AmountCents); err != nil {
			return fmt.Errorf("cancel: credit user wallet: %w", err)
		}

		refundKey := fmt.Sprintf("refund_%s", s.ReferenceID)
		displayRef := fmt.Sprintf("RF-%05d", time.Now().UnixMilli()%100000)
		if err := paymentTx.Create(ctx, &models.Payment{
			ID:               uuid.New(),
			IdempotencyKey:   refundKey,
			AccountID:        s.UserAccountID,
			Type:             models.PaymentTypeRefund,
			ReferenceID:      &s.ReferenceID,
			AmountCents:      s.AmountCents,
			Status:           models.PaymentStatusCompleted,
			DisplayReference: &displayRef,
			CreatedAt:        time.Now(),
			UpdatedAt:        time.Now(),
		}); err != nil {
			return fmt.Errorf("cancel: create refund payment record: %w", err)
		}
	}

	// ── Reverse the host side ────────────────────────────────────────────
	if s.NetEarningCents > 0 && s.HostAccountID != uuid.Nil {
		cancelLedgerKey := "cancellation_debit_" + s.ReferenceID.String()
		if _, err := ledgerTx.Create(ctx, &models.TransactionLedger{
			ID:             uuid.New(),
			AccountID:      s.HostAccountID,
			Type:           models.LedgerTypeCancellationDebit,
			AmountCents:    -s.NetEarningCents, // NEGATIVE = host earning reversed
			ReferenceID:    &s.ReferenceID,
			ReferenceType:  strPtr(s.ReferenceType),
			IdempotencyKey: &cancelLedgerKey,
			Description:    strPtr(fmt.Sprintf("Host earning reversed — %s cancelled", s.Noun)),
			Status:         models.LedgerStatusCompleted,
			CreatedAt:      time.Now(),
		}); err != nil {
			return fmt.Errorf("cancel: write host cancellation ledger entry: %w", err)
		}
	}
	if s.NetEarningCents > 0 && s.HostID != uuid.Nil {
		// Reduce both the pending clearance and the lifetime earnings total.
		if err := payoutTx.ClearPending(ctx, s.HostID, s.NetEarningCents); err != nil {
			return fmt.Errorf("cancel: clear host pending clearance: %w", err)
		}
		if err := payoutTx.DecrementEarnings(ctx, s.HostID, s.NetEarningCents); err != nil {
			return fmt.Errorf("cancel: decrement host earnings: %w", err)
		}
	}

	// ── Reverse the platform commission ──────────────────────────────────
	// With the user fully refunded and the host earning reversed, this leaves
	// the cancelled purchase with a net-zero footprint across the ledger.
	if s.ServiceFeeCents > 0 && s.PlatformAccount != nil {
		platformCancelKey := "cancellation_platform_" + s.ReferenceID.String()
		if _, err := ledgerTx.Create(ctx, &models.TransactionLedger{
			ID:             uuid.New(),
			AccountID:      s.PlatformAccount.ID,
			Type:           models.LedgerTypeCancellationDebit,
			AmountCents:    -s.ServiceFeeCents, // NEGATIVE = platform commission reversed
			ReferenceID:    &s.ReferenceID,
			ReferenceType:  strPtr(s.ReferenceType),
			IdempotencyKey: &platformCancelKey,
			Description:    strPtr(fmt.Sprintf("Platform commission reversed — %s cancelled", s.Noun)),
			Status:         models.LedgerStatusCompleted,
			CreatedAt:      time.Now(),
		}); err != nil {
			return fmt.Errorf("cancel: write platform cancellation ledger entry: %w", err)
		}
	}
	return nil
}
