package repository

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"myslotmate-backend/internal/models"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

// Monthly pass sales count in every tax read alongside bookings; a refunded
// pass counts only as a return. Runs in a rolled-back transaction. Skipped
// unless TEST_DATABASE_URL points at a migrated, throwaway database.
func TestTaxReads_IncludePasses(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	db, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	repo := NewPayoutRepository(db).WithTx(tx)
	liabilityBefore, _, err := repo.TaxLiability(ctx)
	if err != nil {
		t.Fatal(err)
	}

	userID, hostID, eventID := uuid.New(), uuid.New(), uuid.New()
	mustExec := func(q string, args ...interface{}) {
		t.Helper()
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec(`INSERT INTO users (id, auth_uid) VALUES ($1, $2)`, userID, "tax-"+userID.String())
	mustExec(`INSERT INTO hosts (id, user_id, first_name, last_name, city, slug) VALUES ($1, $2, 'T', 'H', 'G', $3)`,
		hostID, userID, "tax-"+hostID.String())
	mustExec(`INSERT INTO events (id, host_id, title, time, slug) VALUES ($1, $2, 'E', now(), $3)`,
		eventID, hostID, "tax-"+eventID.String())
	mustExec(`INSERT INTO bookings (event_id, user_id, quantity, status, occurrence_date, ticket_cents, tcs_cents, tds_cents)
		VALUES ($1, $2, 1, 'confirmed', now(), 1000, 5, 1)`, eventID, userID)

	passes := NewPassRepository(db).WithTx(tx)
	for _, p := range []struct {
		status      models.PassStatus
		ticket, tcs int64
		tds, payout int64
	}{
		{models.PassActive, 2000, 10, 2, 1988},
		{models.PassExpired, 4000, 20, 4, 3976},
		{models.PassRefunded, 3000, 15, 3, 2982},
	} {
		up := &models.UserPass{EventID: eventID, UserID: uuid.New(), PriceCents: p.ticket, NetEarningCents: p.ticket,
			ValidFrom: time.Now(), ValidUntil: time.Now().AddDate(0, 0, 30), Status: p.status,
			TicketCents: &p.ticket, TCSCents: p.tcs, TDSCents: p.tds, TCSInterstate: true, HostPayoutCents: &p.payout}
		mustExec(`INSERT INTO users (id, auth_uid) VALUES ($1, $2)`, up.UserID, "tax-"+up.UserID.String())
		if err := passes.Create(ctx, up); err != nil {
			t.Fatalf("create pass: %v", err)
		}
		got, err := passes.GetByID(ctx, up.ID)
		if err != nil || got == nil || got.HostPayoutCents == nil || *got.HostPayoutCents != p.payout ||
			*got.TicketCents != p.ticket || got.TCSCents != p.tcs || got.TDSCents != p.tds || !got.TCSInterstate {
			t.Fatalf("pass tax columns did not round-trip: %+v, %v", got, err)
		}
	}

	fyStart := time.Now().AddDate(0, -1, 0)
	if gross, err := repo.FYGrossTicketCents(ctx, hostID, fyStart); err != nil || gross != 7000 {
		t.Errorf("FY gross = %d, %v; want 7000 (booking + live passes, not the refunded one)", gross, err)
	}
	if tcs, tds, err := repo.HostTaxTotals(ctx, hostID, fyStart); err != nil || tcs != 35 || tds != 7 {
		t.Errorf("totals = %d/%d, %v; want 35/7", tcs, tds, err)
	}
	if months, err := repo.HostTaxByMonth(ctx, hostID, fyStart, time.Now().Add(time.Hour)); err != nil || len(months) != 1 ||
		months[0].TCSCents != 35 || months[0].TDSCents != 7 {
		t.Errorf("by month = %+v, %v; want one month 35/7", months, err)
	}
	rows, err := repo.TaxReportRows(ctx, fyStart, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range rows {
		if r.HostID != hostID {
			continue
		}
		found = true
		if r.GrossCents != 10000 || r.ReturnsCents != 3000 || r.TCSIntraCents != 5 || r.TCSInterCents != 30 || r.TDSCents != 7 {
			t.Errorf("report row = %+v; want gross 10000 returns 3000 tcs 5/30 tds 7", r)
		}
	}
	if !found {
		t.Error("host missing from tax report")
	}
	if stamped, _, err := repo.TaxLiability(ctx); err != nil || stamped-liabilityBefore != 42 {
		t.Errorf("liability rose by %d, %v; want 42", stamped-liabilityBefore, err)
	}
}
