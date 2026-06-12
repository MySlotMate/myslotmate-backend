-- cleanup_bookings.sql  —  FULL WIPE of all booking/payment/wallet transaction data.
-- Deletes every booking, payment, top-up, and ledger entry; zeroes balances,
-- host earnings, and per-event booking counters. Experiences/users/hosts are kept.
--
-- ⚠️  DESTRUCTIVE & IRREVERSIBLE (no backup taken, per request). Production DB.
-- Single transaction; verification runs before COMMIT.

BEGIN;

-- Empty the gateway/reconciliation companion tables first (FK to ledger).
-- Currently 0 rows; defensive in case any appear before execution.
DELETE FROM webhook_executions;
DELETE FROM reconciliation_discrepancies;
DELETE FROM reconciliation_runs;

-- Core transactional money data.
DELETE FROM payments;             -- all (booking + refund + topup)
DELETE FROM transaction_ledger;   -- all (booking + topup)
DELETE FROM bookings;             -- all

-- Reset derived aggregates / counters / balances (rows kept).
UPDATE accounts      SET balance_cents = 0 WHERE balance_cents <> 0;
UPDATE host_earnings SET total_earnings_cents = 0,
                         pending_clearance_cents = 0,
                         estimated_clearance_at = NULL;
UPDATE events        SET total_bookings = 0 WHERE total_bookings <> 0;

-- Verify BEFORE committing (every count must be 0).
\echo '== remaining (all expect 0) =='
SELECT
  (SELECT count(*) FROM bookings)                                            AS bookings,
  (SELECT count(*) FROM payments)                                            AS payments,
  (SELECT count(*) FROM transaction_ledger)                                  AS ledger,
  (SELECT count(*) FROM accounts      WHERE balance_cents <> 0)              AS nonzero_balances,
  (SELECT count(*) FROM host_earnings WHERE total_earnings_cents <> 0
                                         OR pending_clearance_cents <> 0)    AS nonzero_earnings,
  (SELECT count(*) FROM events        WHERE total_bookings <> 0)             AS nonzero_event_counters;

-- If every column above is 0:
COMMIT;
-- Otherwise abort with:  ROLLBACK;
