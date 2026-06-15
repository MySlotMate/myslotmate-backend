// Package db provides a PostgreSQL connection for the backend.
// Set DATABASE_URL in env.
package db

import (
	"context"
	"database/sql"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Open opens a connection to PostgreSQL using connURL.
func Open(connURL string) (*sql.DB, error) {
	if connURL == "" {
		return nil, sql.ErrNoRows // or a custom error; caller should check config
	}
	return sql.Open("pgx", connURL)
}

// ConfigurePool applies connection-pool limits suited to a remote Postgres
// (e.g. Supabase). Establishing a new connection to a distant DB costs ~1.5–2s
// (TCP + TLS + SCRAM auth round trips), so the goal is to open connections once
// and keep them warm rather than re-dialing per request — Go's default keeps
// only 2 idle connections, which causes constant reconnects under any churn.
func ConfigurePool(db *sql.DB) {
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(25) // keep all opened connections idle (reused, not closed)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(10 * time.Minute) // recycle before the server's idle timeout
}

// WarmPool eagerly opens n physical connections so the first real requests don't
// each pay the handshake cost. Best-effort: errors are ignored. Each goroutine
// briefly holds its connection so the others grab distinct ones rather than
// serially reusing a single connection.
func WarmPool(ctx context.Context, db *sql.DB, n int) {
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := db.Conn(ctx)
			if err != nil {
				return
			}
			_ = conn.PingContext(ctx)
			time.Sleep(250 * time.Millisecond)
			_ = conn.Close() // returns the connection to the pool as idle
		}()
	}
	wg.Wait()
}

// OpenWithContext is like Open but also pings the database with the given context.
func OpenWithContext(ctx context.Context, connURL string) (*sql.DB, error) {
	db, err := Open(connURL)
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}
