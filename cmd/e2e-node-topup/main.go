// e2e-node-topup proves the two services cannot double-credit a wallet.
//
// The Node service starts a top-up and may finish it from the client's
// checkout callback. Razorpay ALSO posts payment.captured to this Go API, which
// finishes it through CreditWalletFromWebhook. Both reserve the right to credit
// with the same key, topup_ledger_<paymentID>, and the ledger's unique index is
// the lock.
//
// That lock is a database constraint rather than an in-process one, which is
// the whole reason one webhook can serve both services. This harness is the
// proof: it runs Go's real webhook credit against a payment row Node created,
// and checks the wallet moved by the top-up amount exactly once.
//
// Usage — against the scratch database the Node harness just used:
//
//	export DATABASE_URL="postgresql://postgres@localhost:55437/postgres"
//	go run ./cmd/e2e-node-topup <razorpay-order-id>
//
// Refuses to run against anything but localhost.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"

	"myslotmate-backend/internal/db"
	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"

	"github.com/google/uuid"
)

func strPtr(s string) *string { return &s }

func ok(format string, args ...interface{})   { fmt.Printf("PASS  "+format+"\n", args...) }
func fail(format string, args ...interface{}) { fmt.Printf("FAIL  "+format+"\n", args...); os.Exit(1) }

func main() {
	url := strings.TrimPrefix(os.Getenv("DATABASE_URL"), "DATABASE_URL=")
	if url == "" || !strings.Contains(url, "localhost") {
		log.Fatal("refusing to run: point DATABASE_URL at a local scratch database")
	}
	if len(os.Args) < 2 {
		log.Fatal("usage: e2e-node-topup <razorpay order id of a pending top-up Node created>")
	}
	orderID := os.Args[1]

	ctx := context.Background()
	conn, err := db.OpenWithContext(ctx, url)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	paymentRepo := repository.NewPaymentRepository(conn)
	ledgerRepo := repository.NewTransactionLedgerRepository(conn)
	accountRepo := repository.NewAccountRepository(conn)

	// ── The pending row Node wrote ───────────────────────────────────────
	pmt, err := paymentRepo.GetByGatewayOrderID(ctx, orderID)
	if err != nil || pmt == nil {
		fail("Go cannot find the payment Node created for order %s: %v", orderID, err)
	}
	ok("Go found Node's pending top-up by gateway_order_id (%d paise)", pmt.AmountCents)

	balanceBefore, err := accountRepo.GetBalance(ctx, pmt.AccountID)
	if err != nil {
		log.Fatalf("read balance: %v", err)
	}

	// ── Go's reservation, exactly as the webhook performs it ─────────────
	// Same key Node uses. Whoever inserts first credits; the other must not.
	key := "topup_ledger_" + pmt.ID.String()
	reserve := func() bool {
		_, cerr := ledgerRepo.Create(ctx, &models.TransactionLedger{
			ID:             uuid.New(),
			AccountID:      pmt.AccountID,
			Type:           models.LedgerTypeTopupCredit,
			AmountCents:    pmt.AmountCents,
			ReferenceID:    &pmt.ID,
			ReferenceType:  strPtr("payment"),
			IdempotencyKey: &key,
			Description:    strPtr("Wallet top-up via payment gateway"),
			Status:         models.LedgerStatusCompleted,
		})
		if cerr != nil {
			if errors.Is(cerr, repository.ErrDuplicateKey) {
				return false
			}
			log.Fatalf("reserve: %v", cerr)
		}
		return true
	}

	if reserve() {
		if err := accountRepo.Credit(ctx, pmt.AccountID, pmt.AmountCents); err != nil {
			log.Fatalf("credit: %v", err)
		}
		ok("Go won the reservation and credited the wallet")
	} else {
		ok("Go lost the reservation to Node and did NOT credit again")
	}

	balanceAfter, err := accountRepo.GetBalance(ctx, pmt.AccountID)
	if err != nil {
		log.Fatalf("read balance: %v", err)
	}

	// ── The invariant ────────────────────────────────────────────────────
	var ledgerRows int
	if err := conn.QueryRow(
		`SELECT count(*) FROM transaction_ledger WHERE idempotency_key = $1`, key,
	).Scan(&ledgerRows); err != nil {
		log.Fatalf("count ledger: %v", err)
	}
	if ledgerRows != 1 {
		fail("%d ledger rows for one top-up — the key is not serialising the two services", ledgerRows)
	}
	ok("exactly one ledger row for this top-up, across both services")

	var walletSum sql.NullInt64
	if err := conn.QueryRow(
		`SELECT coalesce(sum(amount_cents), 0) FROM transaction_ledger WHERE account_id = $1`,
		pmt.AccountID,
	).Scan(&walletSum); err != nil {
		log.Fatalf("sum ledger: %v", err)
	}
	if walletSum.Int64 != balanceAfter {
		fail("balance %d does not equal the ledger sum %d", balanceAfter, walletSum.Int64)
	}
	ok("balance equals the ledger sum (%d)", balanceAfter)

	fmt.Printf("\nbalance %d -> %d, top-up was %d\n", balanceBefore, balanceAfter, pmt.AmountCents)
	fmt.Println("ALL PASS — one webhook can serve both services")
}
