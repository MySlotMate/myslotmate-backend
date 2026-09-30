package repository

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lib/pq"
)

// This repository once detected duplicates with a type assertion on lib/pq's
// *pq.Error, while the application connects through pgx. The assertion never
// matched, so ErrDuplicateKey was never returned: the top-up reservation
// reported a hard error instead of "already credited", and webhook replay
// detection never fired.
//
// These tests pin the behaviour to the driver actually in use.

func TestUniqueViolationDetectsPgxError(t *testing.T) {
	// What pgx returns — the driver internal/db opens.
	if !isUniqueViolation(&pgconn.PgError{Code: "23505"}) {
		t.Fatal("a pgx unique violation was not recognised; this is the driver we actually use")
	}
}

func TestUniqueViolationSeesThroughWrapping(t *testing.T) {
	wrapped := fmt.Errorf("insert ledger: %w", &pgconn.PgError{Code: "23505"})
	if !isUniqueViolation(wrapped) {
		t.Fatal("a wrapped unique violation was missed — errors.As, not a type assertion")
	}
}

func TestUniqueViolationStillHandlesLibPq(t *testing.T) {
	if !isUniqueViolation(&pq.Error{Code: "23505"}) {
		t.Fatal("lib/pq support was dropped; the repository should work under either driver")
	}
}

func TestUniqueViolationIgnoresOtherErrors(t *testing.T) {
	for _, err := range []error{
		errors.New("connection refused"),
		&pgconn.PgError{Code: "23503"}, // foreign key, not uniqueness
		&pgconn.PgError{Code: "42P08"}, // the parameter-type error, not uniqueness
		nil,
	} {
		if isUniqueViolation(err) {
			t.Errorf("%v was misread as a unique violation — callers would silently skip a real failure", err)
		}
	}
}
