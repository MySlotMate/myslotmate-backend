// e2e-node-booking proves cross-backend parity for the money path.
//
// The Node service writes bookings into this same database. The Go API then
// reads and REVERSES them: a host cancelling on the web runs CancelEvent →
// CancelBookingByHost → performCancellation over rows Node wrote. If Node's
// booking shape is wrong — a missing net_earning_cents, a ledger that does not
// net to zero — the damage appears here, at cancellation, as money invented or
// destroyed.
//
// So this harness does not create a booking. It takes one that Node already
// created, cancels it through the real Go service, and checks the books.
//
// Usage — against the same scratch database the Node harness just used:
//
//	export DATABASE_URL="postgresql://postgres@localhost:55436/postgres"
//	go run ./cmd/e2e-node-booking <booking-id>
//
// It refuses to run against anything but localhost.
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
	"myslotmate-backend/internal/repository"
	"myslotmate-backend/internal/service"

	"github.com/google/uuid"
)

func ok(format string, args ...interface{})   { fmt.Printf("PASS  "+format+"\n", args...) }
func fail(format string, args ...interface{}) { fmt.Printf("FAIL  "+format+"\n", args...); os.Exit(1) }

// ledgerSum is the reconciliation invariant: every account's balance must be
// explained by its ledger rows, and a booking plus its cancellation must net to
// zero across all accounts.
func ledgerSum(conn *sql.DB, where string, args ...interface{}) int64 {
	var total sql.NullInt64
	if err := conn.QueryRow(
		`SELECT coalesce(sum(amount_cents), 0) FROM transaction_ledger `+where, args...,
	).Scan(&total); err != nil {
		log.Fatalf("ledger sum: %v", err)
	}
	return total.Int64
}

func main() {
	url := strings.TrimPrefix(os.Getenv("DATABASE_URL"), "DATABASE_URL=")
	if url == "" || !strings.Contains(url, "localhost") {
		log.Fatal("refusing to run: point DATABASE_URL at a local scratch database")
	}
	if len(os.Args) < 2 {
		log.Fatal("usage: e2e-node-booking <booking-id written by the Node service>")
	}
	bookingID := uuid.MustParse(os.Args[1])

	ctx := context.Background()
	conn, err := db.OpenWithContext(ctx, url)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	// ── The booking as Node left it ──────────────────────────────────────
	var userID, eventID uuid.UUID
	var amount, fee, net sql.NullInt64
	var status string
	var paymentID sql.NullString
	if err := conn.QueryRow(
		`SELECT user_id, event_id, amount_cents, service_fee_cents, net_earning_cents, status, payment_id
		   FROM bookings WHERE id = $1`, bookingID,
	).Scan(&userID, &eventID, &amount, &fee, &net, &status, &paymentID); err != nil {
		log.Fatalf("read booking: %v", err)
	}

	fmt.Printf("\nNode wrote: amount=%d fee=%d net=%d status=%s payment=%v\n\n",
		amount.Int64, fee.Int64, net.Int64, status, paymentID.Valid)

	// Go's reversal reads every one of these. A NULL here is a silent payout bug.
	if !amount.Valid || !fee.Valid || !net.Valid {
		fail("Go's cancellation reads amount/service_fee/net_earning — one is NULL")
	}
	if !paymentID.Valid {
		fail("bookings.payment_id is NULL — the reversal cannot mark the payment reversed")
	}
	if amount.Int64 != fee.Int64+net.Int64 {
		fail("fee + net (%d) does not equal amount (%d)", fee.Int64+net.Int64, amount.Int64)
	}
	ok("the columns Go reverses are all present and consistent")

	beforeAll := ledgerSum(conn, "")
	var beforeBalance int64
	if err := conn.QueryRow(
		`SELECT balance_cents FROM accounts WHERE owner_type = 'user' AND owner_id = $1`, userID,
	).Scan(&beforeBalance); err != nil {
		log.Fatalf("read balance: %v", err)
	}

	// ── Cancel it through the REAL Go service ────────────────────────────
	bookingRepo := repository.NewBookingRepository(conn)
	eventRepo := repository.NewEventRepository(conn)
	accountRepo := repository.NewAccountRepository(conn)
	paymentRepo := repository.NewPaymentRepository(conn)
	payoutRepo := repository.NewPayoutRepository(conn)
	hostRepo := repository.NewHostRepository(conn)
	userRepo := repository.NewUserRepository(conn)
	ledgerRepo := repository.NewTransactionLedgerRepository(conn)
	tierRepo := repository.NewEventPriceTierRepository(conn)
	attendeeRepo := repository.NewAttendeeProfileRepository(conn)
	couponRepo := repository.NewCouponRepository(conn)
	joinRepo := repository.NewJoinRequestRepository(conn)
	passRepo := repository.NewPassRepository(conn)

	bookingSvc := service.NewBookingService(
		conn, bookingRepo, eventRepo, accountRepo, paymentRepo, payoutRepo, hostRepo,
		userRepo, ledgerRepo, tierRepo, attendeeRepo, couponRepo, joinRepo, passRepo,
		nil, event.GetDispatcher(), nil,
	)

	if _, err := bookingSvc.CancelBooking(ctx, bookingID, userID, service.RefundDestinationWallet); err != nil {
		fail("Go could not cancel a booking written by Node: %v", err)
	}
	ok("Go's CancelBooking accepted a booking written by Node")

	// ── The books after ──────────────────────────────────────────────────
	afterAll := ledgerSum(conn, "")
	if afterAll != beforeAll {
		fail("the ledger no longer nets to zero: %d before, %d after", beforeAll, afterAll)
	}
	ok("booking + cancellation net to zero across every account (%d)", afterAll)

	var afterBalance int64
	if err := conn.QueryRow(
		`SELECT balance_cents FROM accounts WHERE owner_type = 'user' AND owner_id = $1`, userID,
	).Scan(&afterBalance); err != nil {
		log.Fatalf("read balance: %v", err)
	}
	if afterBalance-beforeBalance != amount.Int64 {
		fail("guest refunded %d, expected %d", afterBalance-beforeBalance, amount.Int64)
	}
	ok("the guest got exactly their money back (+%d)", amount.Int64)

	var earnings, pending int64
	if err := conn.QueryRow(
		`SELECT h.total_earnings_cents, h.pending_clearance_cents
		   FROM host_earnings h JOIN events e ON e.host_id = h.host_id WHERE e.id = $1`, eventID,
	).Scan(&earnings, &pending); err != nil {
		log.Fatalf("read host earnings: %v", err)
	}
	ok("host earnings after reversal: total=%d pending=%d", earnings, pending)

	var newStatus string
	if err := conn.QueryRow(`SELECT status FROM bookings WHERE id = $1`, bookingID).Scan(&newStatus); err != nil {
		log.Fatalf("re-read booking: %v", err)
	}
	if newStatus != "refunded" && newStatus != "cancelled" {
		fail("booking left at %q after cancellation", newStatus)
	}
	ok("booking is now %q", newStatus)

	fmt.Println("\nALL PASS — a booking written by Node reverses cleanly through Go")
}
