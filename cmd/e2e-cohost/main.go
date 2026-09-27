// e2e-cohost drives the whole co-hosting flow against a real PostgreSQL schema:
// invite → accept → grant withdrawal → withdraw → second withdrawal refused.
//
// Point DATABASE_URL at a local scratch database, never a real one (the harness
// refuses anything but localhost). The payout provider is stubbed, so no money
// moves and no network call is made. It seeds its own fixtures and clears what a
// previous run left behind, so it is safe to re-run.
//
// A scratch database from nothing:
//
//	export PATH=/opt/homebrew/opt/postgresql@16/bin:$PATH
//	mkdir -p /tmp/msmpg              # short socket dir: the socket path has a 103-byte limit
//	initdb -D /tmp/msmpg/data -U test
//	pg_ctl -D /tmp/msmpg/data -o "-p 55432 -k /tmp/msmpg" -l /tmp/msmpg/log start
//	createdb -h /tmp/msmpg -p 55432 -U test msm
//	export DATABASE_URL="postgres://test@localhost:55432/msm?sslmode=disable"
//	go run ./cmd/migrate && go run ./cmd/e2e-cohost
//
// Tear down with: pg_ctl -D /tmp/msmpg/data stop && rm -rf /tmp/msmpg
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"

	"myslotmate-backend/internal/db"
	"myslotmate-backend/internal/lib/event"
	"myslotmate-backend/internal/lib/payout"
	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"
	"myslotmate-backend/internal/service"

	"github.com/google/uuid"
)

var (
	ownerHost  = uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001")
	cohostHost = uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000002")
	eventID    = uuid.MustParse("eeeeeeee-0000-0000-0000-000000000001")
	cohostMail = "cohost@e2e.test"
)

// stubProvider stands in for Cashfree: accepts the transfer and reports
// "processing", which is exactly what the real provider does in production.
type stubProvider struct{ calls []payout.TransferRequest }

func (p *stubProvider) RegisterBeneficiary(ctx context.Context, req payout.TransferRequest) error {
	return nil
}
func (p *stubProvider) InitiateTransfer(ctx context.Context, req payout.TransferRequest) (*payout.TransferResponse, error) {
	p.calls = append(p.calls, req)
	return &payout.TransferResponse{ProviderRefID: "stub_" + req.PaymentID.String(), Status: "processing"}, nil
}
func (p *stubProvider) CheckStatus(ctx context.Context, ref string) (*payout.TransferResponse, error) {
	return &payout.TransferResponse{ProviderRefID: ref, Status: "processing"}, nil
}
func (p *stubProvider) ValidateWebhookSignature(payload []byte, sig, ts string) bool { return true }

func step(n float64, what string) {
	fmt.Printf("\n[%s] %s\n", strings.TrimSuffix(fmt.Sprintf("%.1f", n), ".0"), what)
}
func ok(format string, args ...interface{}) {
	fmt.Printf("    PASS  "+format+"\n", args...)
}
func fail(format string, args ...interface{}) {
	fmt.Printf("    FAIL  "+format+"\n", args...)
	os.Exit(1)
}

func main() {
	url := strings.TrimPrefix(os.Getenv("DATABASE_URL"), "DATABASE_URL=")
	if url == "" || !strings.Contains(url, "localhost") {
		log.Fatal("refusing to run: point DATABASE_URL at a local scratch database")
	}
	ctx := context.Background()
	conn, err := db.OpenWithContext(ctx, url)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	seed(conn)

	cohostRepo := repository.NewCoHostRepository(conn)
	eventRepo := repository.NewEventRepository(conn)
	paymentRepo := repository.NewPaymentRepository(conn)
	provider := &stubProvider{}
	svc := service.NewPayoutService(
		repository.NewPayoutRepository(conn),
		repository.NewAccountRepository(conn),
		paymentRepo,
		repository.NewBookingRepository(conn),
		repository.NewHostRepository(conn),
		repository.NewTransactionLedgerRepository(conn),
		cohostRepo,
		eventRepo,
		repository.NewUserRepository(conn),
		provider,
		event.GetDispatcher(),
		nil, // no email from the harness
		"http://localhost:3000",
	)

	// ── 1. invite ───────────────────────────────────────────────────────────
	step(1, "owner invites cohost@e2e.test to co-host the event")
	ch, err := svc.InviteCoHost(ctx, ownerHost, eventID, cohostMail, false)
	if err != nil {
		fail("invite failed: %v", err)
	}
	ok("invitation %s created, status=%s can_withdraw=%v", ch.ID, ch.Status, ch.CanWithdraw)
	if ch.Status != models.CoHostPending || ch.CanWithdraw {
		fail("a fresh invite must be pending with withdrawals OFF")
	}

	step(2, "invite is rejected for a stranger email and for a duplicate")
	if _, err := svc.InviteCoHost(ctx, ownerHost, eventID, "nobody@nowhere.test", false); err == nil {
		fail("inviting a non-account email must fail")
	} else {
		ok("non-account email refused: %v", err)
	}
	if _, err := svc.InviteCoHost(ctx, ownerHost, eventID, cohostMail, false); err == nil {
		fail("duplicate invite must fail")
	} else {
		ok("duplicate refused: %v", err)
	}
	if _, err := svc.InviteCoHost(ctx, cohostHost, eventID, cohostMail, false); err == nil {
		fail("a non-owner must not be able to invite")
	} else {
		ok("non-owner refused: %v", err)
	}

	step(2.5, "invitation detail is visible to both sides")
	if sh, err := svc.ListSharedEvents(ctx, cohostHost); err != nil || len(sh) != 1 {
		fail("invitee must see the pending invitation: %v (rows %d)", err, len(sh))
	} else if sh[0].InvitedAt.IsZero() || sh[0].EventTime.IsZero() {
		fail("invited_at / event_time must be populated, got %v / %v", sh[0].InvitedAt, sh[0].EventTime)
	} else {
		ok("invitee sees %q from %s, invited %s, runs %s, can_withdraw=%v",
			sh[0].EventTitle, sh[0].OwnerName,
			sh[0].InvitedAt.Format("2 Jan 2006"), sh[0].EventTime.Format("2 Jan 2006 15:04"), sh[0].CanWithdraw)
	}
	// Resending needs a mail service; the harness wires none, so it must say so
	// rather than pretend it sent something.
	if err := svc.ResendCoHostInvite(ctx, ownerHost, ch.ID); err == nil {
		fail("resend must fail when no email service is configured")
	} else {
		ok("resend reports the real reason: %v", err)
	}

	// ── 3. roster ───────────────────────────────────────────────────────────
	step(3, "owner sees the roster")
	rows, err := cohostRepo.ListByEvent(ctx, eventID)
	if err != nil {
		fail("roster failed: %v", err)
	}
	if len(rows) != 1 {
		fail("expected 1 roster row, got %d", len(rows))
	}
	ok("%s <%s> status=%s claimed=%d", rows[0].HostName, rows[0].HostEmail, rows[0].Status, rows[0].ClaimedCents)

	// ── 4. co-manage gate before / after acceptance ─────────────────────────
	step(4, "a pending co-host cannot manage the event yet")
	if can, _ := eventRepo.HostCanManage(ctx, eventID, cohostHost); can {
		fail("pending invite must not grant management")
	} else {
		ok("HostCanManage=false while pending")
	}

	step(5, "co-host accepts, and can now manage the event")
	if err := svc.RespondToCoHostInvite(ctx, cohostHost, ch.ID, true); err != nil {
		fail("accept failed: %v", err)
	}
	if can, _ := eventRepo.HostCanManage(ctx, eventID, cohostHost); !can {
		fail("accepted co-host must be able to manage the event")
	}
	ok("HostCanManage=true after accepting")
	evs, err := eventRepo.ListManageableByHostID(ctx, cohostHost)
	if err != nil || len(evs) != 1 {
		fail("shared event must appear in the co-host's manageable list (got %d, err %v)", len(evs), err)
	}
	ok("shared event %q appears in the co-host's dashboard list", evs[0].Title)
	// Owner-only listings must NOT absorb it: they feed stats, ratings and the
	// earnings breakdown, none of which belong to a co-host.
	if owned, err := eventRepo.ListByHostID(ctx, cohostHost); err != nil || len(owned) != 0 {
		fail("owner-only list must stay empty for a co-host (got %d, err %v)", len(owned), err)
	}
	if ids, err := eventRepo.ListByHostIDForIDs(ctx, cohostHost); err != nil || len(ids) != 0 {
		fail("rating/attention id list must stay owner-only (got %d, err %v)", len(ids), err)
	}
	ok("owner-only listings stay empty — no borrowed stats, ratings or earnings")

	// ── 6. withdrawal is gated on the owner's toggle ─────────────────────────
	step(6, "withdrawal refused while the owner's toggle is OFF")
	if _, err := svc.RequestCoHostWithdrawal(ctx, cohostHost, ch.ID, service.WithdrawalRequest{}); err == nil {
		fail("withdrawal must be refused without the toggle")
	} else {
		ok("refused: %v", err)
	}

	step(7, "owner grants withdrawal; co-host sees the available pool")
	if err := svc.SetCoHostCanWithdraw(ctx, ownerHost, ch.ID, true); err != nil {
		fail("toggle failed: %v", err)
	}
	shared, err := svc.ListSharedEvents(ctx, cohostHost)
	if err != nil || len(shared) != 1 {
		fail("shared list failed: %v (rows %d)", err, len(shared))
	}
	s := shared[0]
	ok("%q by %s: passed=%d claimed=%d available=%d can_withdraw=%v",
		s.EventTitle, s.OwnerName, s.PassedCents, s.ClaimedCents, s.AvailableCents, s.CanWithdraw)
	if s.PassedCents != 100000 {
		fail("expected 100000 passed earnings (upcoming session excluded), got %d", s.PassedCents)
	}

	// ── 8. partial withdrawal ───────────────────────────────────────────────
	step(8, "co-host withdraws 400.00 of the 1000.00 pool")
	pay, err := svc.RequestCoHostWithdrawal(ctx, cohostHost, ch.ID, service.WithdrawalRequest{AmountCents: 40000})
	if err != nil {
		fail("withdrawal failed: %v", err)
	}
	ok("payment %s amount=%d status=%s", pay.ID, pay.AmountCents, pay.Status)
	if pay.AmountCents != 40000 {
		fail("wrong amount paid out")
	}
	// The money must leave the OWNER's account but land on the CO-HOST's method.
	ownerAcct := mustScanUUID(conn, `SELECT account_id FROM hosts WHERE id = $1`, ownerHost)
	if pay.AccountID != ownerAcct {
		fail("payout must be debited from the owner's account, got %s", pay.AccountID)
	}
	ok("debited the owner's account %s", pay.AccountID)
	if pay.PayoutMethodID == nil || *pay.PayoutMethodID != uuid.MustParse("dddddddd-0000-0000-0000-000000000002") {
		fail("payout must target the CO-HOST's own verified method, got %v", pay.PayoutMethodID)
	}
	ok("paid to the co-host's own bank method %s (provider saw beneficiary %q)",
		*pay.PayoutMethodID, provider.calls[0].BeneficiaryName)

	claimed, _ := cohostRepo.EventClaimedCents(ctx, eventID)
	if claimed != 40000 {
		fail("claim ledger should hold 40000, holds %d", claimed)
	}
	ok("claim recorded: %d reserved against the event", claimed)

	// ── 9. the remaining pool, then exhaustion ──────────────────────────────
	step(9, "second withdrawal takes the remaining 600.00; a third is refused")
	pay2, err := svc.RequestCoHostWithdrawal(ctx, cohostHost, ch.ID, service.WithdrawalRequest{})
	if err != nil {
		fail("second withdrawal failed: %v", err)
	}
	ok("payment %s amount=%d (blank amount = whatever is left)", pay2.ID, pay2.AmountCents)
	if pay2.AmountCents != 60000 {
		fail("expected the remaining 60000, got %d", pay2.AmountCents)
	}
	if _, err := svc.RequestCoHostWithdrawal(ctx, cohostHost, ch.ID, service.WithdrawalRequest{AmountCents: 100}); err == nil {
		fail("a third withdrawal must be refused — the event is drained")
	} else {
		ok("refused: %v", err)
	}

	step(10, "the owner cannot withdraw the same earnings either")
	if _, err := svc.RequestWithdrawal(ctx, ownerHost, service.WithdrawalRequest{AmountCents: 10000}); err == nil {
		fail("owner must not be able to re-withdraw earnings the co-host already took")
	} else {
		ok("refused: %v", err)
	}

	// ── 11. revoking ────────────────────────────────────────────────────────
	// ── 11. webhook outcomes mirror onto the claim ──────────────────────────
	step(11, "a FAILED payout webhook releases its slice; a COMPLETED one keeps it")
	if err := svc.HandlePayoutWebhook(ctx, pay.ID, "failed", "stub: bank rejected"); err != nil {
		fail("failed-webhook handling errored: %v", err)
	}
	if claimed, _ := cohostRepo.EventClaimedCents(ctx, eventID); claimed != 60000 {
		fail("a failed payout must release its 40000, claims still hold %d", claimed)
	}
	ok("failed payout released 40000 — 60000 still held by the live claim")
	pay3, err := svc.RequestCoHostWithdrawal(ctx, cohostHost, ch.ID, service.WithdrawalRequest{})
	if err != nil {
		fail("the released 40000 must be withdrawable again: %v", err)
	}
	ok("re-withdrew the released slice: payment %s amount=%d", pay3.ID, pay3.AmountCents)
	if err := svc.HandlePayoutWebhook(ctx, pay2.ID, "completed", ""); err != nil {
		fail("completed-webhook handling errored: %v", err)
	}
	if claimed, _ := cohostRepo.EventClaimedCents(ctx, eventID); claimed != 100000 {
		fail("a completed payout must keep its slice, claims hold %d", claimed)
	}
	ok("completed payout keeps its 60000 — event fully drained again at 100000")

	step(12, "owner revokes co-hosting; management and withdrawal both stop")
	if err := svc.RevokeCoHost(ctx, ownerHost, ch.ID); err != nil {
		fail("revoke failed: %v", err)
	}
	if can, _ := eventRepo.HostCanManage(ctx, eventID, cohostHost); can {
		fail("a revoked co-host must not be able to manage the event")
	}
	ok("HostCanManage=false after revoke")
	if _, err := svc.RequestCoHostWithdrawal(ctx, cohostHost, ch.ID, service.WithdrawalRequest{AmountCents: 100}); err == nil {
		fail("a revoked co-host must not be able to withdraw")
	} else {
		ok("refused: %v", err)
	}
	// Past payouts stand — that was the explicit product decision.
	var stillPaid int
	_ = conn.QueryRow(`SELECT count(*) FROM event_cohost_claims WHERE status <> 'failed'`).Scan(&stillPaid)
	if stillPaid != 2 {
		fail("revoking must not unwind completed claims, found %d live claims", stillPaid)
	}
	ok("both earlier payouts still stand after revoke")

	// ── 13. withdrawal access granted at invite time ─────────────────────────
	step(13, "re-inviting with earnings access granted up front")
	again, err := svc.InviteCoHost(ctx, ownerHost, eventID, cohostMail, true)
	if err != nil {
		fail("re-invite after revoke failed: %v", err)
	}
	if !again.CanWithdraw || again.Status != models.CoHostPending {
		fail("invite must carry can_withdraw=true while still pending, got %v/%s", again.CanWithdraw, again.Status)
	}
	ok("invitation %s created pending with can_withdraw=true", again.ID)
	// Access granted at invite time still waits for acceptance.
	if _, err := svc.RequestCoHostWithdrawal(ctx, cohostHost, again.ID, service.WithdrawalRequest{AmountCents: 100}); err == nil {
		fail("a pending invite must not allow withdrawal even with access granted")
	} else {
		ok("still refused until accepted: %v", err)
	}

	// ── 14. the share fades out on its own ──────────────────────────────────
	step(14, "an old, fully-withdrawn experience drops out of the co-host's dashboard")
	rec, gran, err := cohostRepo.CountsForHost(ctx, cohostHost)
	if err != nil {
		fail("counts failed: %v", err)
	}
	if rec != 1 {
		fail("a live invite on a recent experience must count, got received=%d", rec)
	}
	ok("while the experience is recent: received=%d granted=%d", rec, gran)

	// Age the whole thing past the 30-day wrap-up window. The still-upcoming
	// session is cancelled first — ageing it would turn its locked 500.00 into
	// fresh withdrawable money, which (correctly) keeps the share alive.
	if _, err := conn.Exec(`UPDATE bookings SET status = 'cancelled' WHERE event_id = $1 AND occurrence_date >= NOW()`, eventID); err != nil {
		fail("could not cancel the upcoming session: %v", err)
	}
	if _, err := conn.Exec(`UPDATE bookings SET occurrence_date = occurrence_date - INTERVAL '90 days' WHERE event_id = $1`, eventID); err != nil {
		fail("could not age bookings: %v", err)
	}
	if _, err := conn.Exec(`UPDATE events SET time = time - INTERVAL '90 days', is_recurring = false WHERE id = $1`, eventID); err != nil {
		fail("could not age the event: %v", err)
	}
	rec, _, err = cohostRepo.CountsForHost(ctx, cohostHost)
	if err != nil {
		fail("counts failed: %v", err)
	}
	if rec != 0 {
		fail("an expired, fully-withdrawn share must stop counting, got received=%d", rec)
	}
	shared, err = svc.ListSharedEvents(ctx, cohostHost)
	if err != nil {
		fail("shared list failed: %v", err)
	}
	if len(shared) != 0 {
		fail("the expired share must leave the card, still shows %d row(s)", len(shared))
	}
	ok("no revoke needed: the tab and the card both go empty once it expires")

	// But money still owed keeps the share alive, however old the experience is.
	if _, err := conn.Exec(`UPDATE event_cohost_claims SET status = 'failed' WHERE event_id = $1`, eventID); err != nil {
		fail("could not release claims: %v", err)
	}
	rec, _, err = cohostRepo.CountsForHost(ctx, cohostHost)
	if err != nil {
		fail("counts failed: %v", err)
	}
	if rec == 0 {
		fail("an old experience with unwithdrawn earnings must stay visible")
	}
	ok("unwithdrawn earnings keep it visible even 90 days later: received=%d", rec)

	fmt.Println("\nALL STEPS PASSED")
}

// seed builds the fixtures the flow needs and clears anything a previous run
// left behind, so the harness is repeatable: one owner, one co-host, one event
// with 1000.00 of already-happened earnings (plus 500.00 still locked behind an
// upcoming session), and a verified bank method for each host.
func seed(conn *sql.DB) {
	const sql = `
DELETE FROM event_cohost_claims;
DELETE FROM event_co_hosts;
DELETE FROM webhook_executions;
DELETE FROM payments;
DELETE FROM transaction_ledger;
DELETE FROM bookings WHERE event_id = 'eeeeeeee-0000-0000-0000-000000000001';

-- WHERE NOT EXISTS, not ON CONFLICT: users and hosts carry BEFORE INSERT
-- triggers that create an account row, and those fire even for a row that
-- ON CONFLICT would then discard.
INSERT INTO users (id, auth_uid, name, email, phn_number)
SELECT * FROM (VALUES
 ('11111111-1111-1111-1111-111111111111'::uuid,'uid-owner','Owner Host','owner@e2e.test','+919000000001'),
 ('22222222-2222-2222-2222-222222222222'::uuid,'uid-cohost','Co Host','cohost@e2e.test','+919000000002'),
 ('33333333-3333-3333-3333-333333333333'::uuid,'uid-guest','Guest One','guest@e2e.test','+919000000003')
) AS v(id, auth_uid, name, email, phn)
WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u.id = v.id);

INSERT INTO hosts (id, user_id, slug, first_name, last_name, application_status)
SELECT * FROM (VALUES
 ('aaaaaaaa-0000-0000-0000-000000000001'::uuid,'11111111-1111-1111-1111-111111111111'::uuid,'owner-host','Owner','Host','approved'::host_application_status),
 ('aaaaaaaa-0000-0000-0000-000000000002'::uuid,'22222222-2222-2222-2222-222222222222'::uuid,'co-host','Co','Host','approved'::host_application_status)
) AS v(id, user_id, slug, first_name, last_name, status)
WHERE NOT EXISTS (SELECT 1 FROM hosts h WHERE h.id = v.id);

INSERT INTO events (id, host_id, title, slug, time, price_cents, is_free, status, capacity)
VALUES ('eeeeeeee-0000-0000-0000-000000000001','aaaaaaaa-0000-0000-0000-000000000001',
        'Pottery Weekend','pottery-weekend', now() - interval '2 days', 100000, false, 'live', 10)
ON CONFLICT (id) DO NOTHING;

-- 60000 + 40000 already happened (withdrawable); 50000 still upcoming (locked).
INSERT INTO bookings (id, event_id, user_id, quantity, occurrence_date, status, amount_cents, net_earning_cents) VALUES
 (gen_random_uuid(),'eeeeeeee-0000-0000-0000-000000000001','33333333-3333-3333-3333-333333333333',1, now() - interval '2 days','confirmed',60000,60000),
 (gen_random_uuid(),'eeeeeeee-0000-0000-0000-000000000001','33333333-3333-3333-3333-333333333333',1, now() - interval '1 day', 'confirmed',40000,40000),
 (gen_random_uuid(),'eeeeeeee-0000-0000-0000-000000000001','33333333-3333-3333-3333-333333333333',1, now() + interval '5 days','confirmed',50000,50000);

INSERT INTO payout_methods (id, host_id, type, bank_name, account_type, last_four_digits,
        account_number_encrypted, ifsc, beneficiary_name, is_verified, is_primary) VALUES
 ('dddddddd-0000-0000-0000-000000000001','aaaaaaaa-0000-0000-0000-000000000001','bank','SBI','savings','1111','000011111111','SBIN0001234','Owner Host',true,true),
 ('dddddddd-0000-0000-0000-000000000002','aaaaaaaa-0000-0000-0000-000000000002','bank','HDFC','savings','4242','000011114242','HDFC0001234','Co Host',true,true)
ON CONFLICT (id) DO NOTHING;`
	if _, err := conn.Exec(sql); err != nil {
		log.Fatalf("seed failed: %v", err)
	}
}

func mustScanUUID(conn *sql.DB, query string, args ...interface{}) uuid.UUID {
	var id uuid.UUID
	if err := conn.QueryRow(query, args...).Scan(&id); err != nil {
		log.Fatal(err)
	}
	return id
}
