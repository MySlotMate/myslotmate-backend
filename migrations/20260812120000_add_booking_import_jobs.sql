-- +migrate Up
-- Bulk booking import: a host uploads an .xlsx of guest name + phone (+ quantity)
-- and each row is turned into a booking in the background.
--
-- Design — the import is a thin batch driver over the EXISTING walk-in free path
-- (walkInService.InitiateWalkIn). It never invents a money path: the whole job is
-- rejected up front unless the event books at zero cost (free event, or a host
-- coupon that comps it). So no wallet debit, no ledger row, no fee split is ever
-- reachable from here.
--
-- Two tables because the host needs per-row failure reporting ("which 12 of my
-- 200 rows failed, and why"), which a counters-only job row cannot give.

CREATE TABLE IF NOT EXISTS booking_import_jobs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    host_id         UUID NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    event_id        UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    -- The occurrence every row in this file books onto. Chosen once by the host at
    -- upload time (recurring events); the event's own time otherwise. Per-row dates
    -- are deliberately not supported.
    occurrence_date TIMESTAMPTZ NOT NULL,
    -- Comp code applied to every row when the event is paid. NULL for free events.
    coupon_code     TEXT,
    file_name       TEXT NOT NULL,
    -- pending | processing | completed | failed
    -- 'completed' means the job finished, NOT that every row succeeded — per-row
    -- outcomes live in booking_import_rows. 'failed' is reserved for a job that
    -- could not run at all (e.g. the event vanished mid-flight).
    status          TEXT NOT NULL DEFAULT 'pending',
    error_message   TEXT,
    total_rows      INT  NOT NULL DEFAULT 0,
    processed_rows  INT  NOT NULL DEFAULT 0,
    success_rows    INT  NOT NULL DEFAULT 0,
    failed_rows     INT  NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at    TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_booking_import_jobs_host ON booking_import_jobs (host_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_booking_import_jobs_event ON booking_import_jobs (event_id);
-- Drives resume-after-restart: find jobs the in-memory worker pool abandoned.
CREATE INDEX IF NOT EXISTS idx_booking_import_jobs_status ON booking_import_jobs (status)
    WHERE status IN ('pending', 'processing');

CREATE TABLE IF NOT EXISTS booking_import_rows (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id        UUID NOT NULL REFERENCES booking_import_jobs(id) ON DELETE CASCADE,
    -- 1-based row number as it appears in the host's spreadsheet (header excluded),
    -- so an error message can point at a line they can actually find.
    row_number    INT  NOT NULL,
    guest_name    TEXT NOT NULL,
    guest_phone   TEXT NOT NULL,
    quantity      INT  NOT NULL DEFAULT 1,
    -- pending | success | failed
    status        TEXT NOT NULL DEFAULT 'pending',
    error_message TEXT,
    booking_id    UUID REFERENCES bookings(id) ON DELETE SET NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- A row is processed at most once even if the job is re-driven after a restart.
CREATE UNIQUE INDEX IF NOT EXISTS booking_import_rows_job_row_key ON booking_import_rows (job_id, row_number);
CREATE INDEX IF NOT EXISTS idx_booking_import_rows_job_status ON booking_import_rows (job_id, status);

-- +migrate Down
DROP TABLE IF EXISTS booking_import_rows;
DROP TABLE IF EXISTS booking_import_jobs;
