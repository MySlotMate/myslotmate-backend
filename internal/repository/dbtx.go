package repository

import (
	"context"
	"database/sql"
)

// DBTX is the common query surface shared by *sql.DB and *sql.Tx. Repositories
// hold a DBTX rather than a concrete *sql.DB so the same repository code can run
// either standalone or inside a caller-managed transaction.
//
// A repository is bound to a transaction via its WithTx method, e.g.:
//
//	tx, _ := db.BeginTx(ctx, nil)
//	ledgerRepo.WithTx(tx).Create(ctx, entry)   // runs on tx
//	accountRepo.WithTx(tx).Credit(ctx, id, amt) // same tx
//	tx.Commit()                                 // all-or-nothing
//
// Both *sql.DB and *sql.Tx satisfy DBTX.
type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row
}
