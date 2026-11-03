package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"myslotmate-backend/internal/lib/timeutil"
	"myslotmate-backend/internal/models"
)

// Weekly auto payout: every Tuesday (IST), each admin-selected host
// (hosts.auto_payout_enabled) gets their full withdrawable balance sent to a
// verified bank account. Hosts with no verified bank account are skipped —
// UPI payouts are not enabled on the Cashfree account.

// autoPayoutStartHour is the IST hour on Tuesday from which the run fires.
const autoPayoutStartHour = 10

// autoPayoutMinCents: balances below this stay put and roll into a later
// Tuesday — a tiny transfer costs more in Cashfree fees than it is worth.
const autoPayoutMinCents = 100_00 // ₹100

// AutoPayoutDue reports whether `now` falls inside the Tuesday payout window.
// The scheduler ticks hourly, so every Tuesday tick from 10:00 IST onward
// calls RunAutoPayouts; the weekly idempotency key makes repeats no-ops.
func AutoPayoutDue(now time.Time) bool {
	t := now.In(timeutil.IST)
	return t.Weekday() == time.Tuesday && t.Hour() >= autoPayoutStartHour
}

// autoPayoutKey is one key per host per ISO week, so a host is paid at most
// once a week however many times the run fires (restarts, overlapping
// deploys, hourly re-ticks). RequestWithdrawal dedups on it via the ledger's
// UNIQUE idempotency_key.
func autoPayoutKey(hostID uuid.UUID, now time.Time) string {
	year, week := now.In(timeutil.IST).ISOWeek()
	return fmt.Sprintf("auto_payout_%s_%d-W%02d", hostID, year, week)
}

// pickBankMethod returns the primary method if it is a verified bank account,
// else any verified bank account, else nil.
func pickBankMethod(methods []*models.PayoutMethod) *models.PayoutMethod {
	var fallback *models.PayoutMethod
	for _, m := range methods {
		if m.Type != models.PayoutMethodBank || !m.IsVerified {
			continue
		}
		if m.IsPrimary {
			return m
		}
		if fallback == nil {
			fallback = m
		}
	}
	return fallback
}

// RunAutoPayouts pays out every auto-payout host once for the current week.
// Per-host failures and skips are logged and do not stop the batch.
func (s *payoutService) RunAutoPayouts(ctx context.Context, now time.Time) error {
	hostIDs, err := s.hostRepo.ListAutoPayoutHostIDs(ctx)
	if err != nil {
		return fmt.Errorf("list auto payout hosts: %w", err)
	}

	for _, hostID := range hostIDs {
		if _, err := s.RunAutoPayoutForHost(ctx, hostID, now); err != nil {
			fmt.Printf("[AUTO_PAYOUT] host=%s skipped: %v\n", hostID, err)
		}
	}
	return nil
}

// RunAutoPayoutForHost pays one host's withdrawable balance to a verified bank
// account under this week's idempotency key. Used by the Tuesday batch and the
// admin "run now" button, so a run-now counts as that host's payout for the
// week. Returns an error saying why when the host is skipped.
//
// ponytail: RequestWithdrawal checks available-then-writes with no lock, so a
// manual withdrawal in the same instant could overdraw (same race as two
// manual withdrawals); add a per-host pg_advisory_xact_lock if that matters.
func (s *payoutService) RunAutoPayoutForHost(ctx context.Context, hostID uuid.UUID, now time.Time) (*models.Payment, error) {
	host, err := s.hostRepo.GetByID(ctx, hostID)
	if err != nil {
		return nil, err
	}
	if host == nil {
		return nil, errors.New("host not found")
	}
	if !host.AutoPayoutEnabled {
		return nil, errors.New("auto payout is off for this host")
	}

	// Co-host withdrawals draw on the owner's balance, so sweeping it would
	// pay the co-host's share into the owner's bank. Leave these to manual.
	if _, granted, err := s.cohostRepo.CountsForHost(ctx, hostID); err != nil {
		return nil, fmt.Errorf("count co-hosts: %w", err)
	} else if granted > 0 {
		return nil, errors.New("host has co-hosts on their events; pay out manually")
	}

	methods, err := s.payoutRepo.ListPayoutMethodsByHostID(ctx, hostID)
	if err != nil {
		return nil, fmt.Errorf("list payout methods: %w", err)
	}
	method := pickBankMethod(methods)
	if method == nil {
		return nil, errors.New("no verified bank account")
	}

	summary, err := s.GetEarningsSummary(ctx, hostID)
	if err != nil {
		return nil, fmt.Errorf("earnings summary: %w", err)
	}
	if summary.AvailableBalanceCents < autoPayoutMinCents {
		return nil, fmt.Errorf("available balance %d paise is below the %d paise minimum", summary.AvailableBalanceCents, autoPayoutMinCents)
	}

	payment, err := s.RequestWithdrawal(ctx, hostID, WithdrawalRequest{
		AmountCents:    summary.AvailableBalanceCents,
		PayoutMethodID: &method.ID,
		IdempotencyKey: autoPayoutKey(hostID, now),
	})
	if err != nil {
		return nil, err
	}
	fmt.Printf("[AUTO_PAYOUT] host=%s payment=%s amount=%d status=%s\n", hostID, payment.ID, payment.AmountCents, payment.Status)
	return payment, nil
}
