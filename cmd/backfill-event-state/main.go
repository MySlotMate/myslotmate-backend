// backfill-event-state fills events.state_code (the venue's GST state code)
// for events created before it existed, by reading the state name out of the
// saved address — the same rule event create/update now apply.
//
// Run from the backend root (needs DATABASE_URL in .env):
//
//	go run ./cmd/backfill-event-state -dry-run   # print what would change
//	go run ./cmd/backfill-event-state            # write it
//
// Only events with a location and no state_code are touched; addresses that
// name no Indian state are left NULL and show up as venue_state_unknown in the
// admin tax report.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/joho/godotenv"

	"myslotmate-backend/internal/config"
	"myslotmate-backend/internal/db"
	"myslotmate-backend/internal/lib/gststate"
)

func main() {
	dryRun := flag.Bool("dry-run", false, "print the changes without writing them")
	flag.Parse()

	if err := godotenv.Load(".env"); err != nil {
		log.Printf("warning: could not load .env: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}
	if cfg.Database.URL == "" {
		log.Fatal("DATABASE_URL is not set")
	}

	ctx := context.Background()
	sqlDB, err := db.OpenWithContext(ctx, cfg.Database.URL)
	if err != nil {
		log.Fatalf("db connection failed: %v", err)
	}
	defer sqlDB.Close()

	rows, err := sqlDB.QueryContext(ctx,
		`SELECT id, location FROM events WHERE state_code IS NULL AND COALESCE(location, '') <> ''`)
	if err != nil {
		log.Fatalf("list events: %v", err)
	}
	type ev struct{ id, loc string }
	var evs []ev
	for rows.Next() {
		var e ev
		if err := rows.Scan(&e.id, &e.loc); err != nil {
			log.Fatalf("scan: %v", err)
		}
		evs = append(evs, e)
	}
	rows.Close()

	var set, unknown int
	for _, e := range evs {
		code := gststate.FromAddress(e.loc)
		if code == "" {
			unknown++
			log.Printf("no state: %s %q", e.id, e.loc)
			continue
		}
		set++
		log.Printf("%s -> %s  %q", e.id, code, e.loc)
		if *dryRun {
			continue
		}
		if _, err := sqlDB.ExecContext(ctx, `UPDATE events SET state_code = $1 WHERE id = $2 AND state_code IS NULL`, code, e.id); err != nil {
			log.Fatalf("update %s: %v", e.id, err)
		}
	}
	log.Printf("done: %d set, %d with no recognisable state, dry_run=%v", set, unknown, *dryRun)
}
