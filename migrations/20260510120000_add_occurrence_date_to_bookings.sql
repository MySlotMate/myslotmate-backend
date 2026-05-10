-- Migration: Add occurrence_date to bookings for per-occurrence capacity tracking
-- This fixes the bug where recurring events share capacity across all dates.

-- Step 1: Add column as nullable first
ALTER TABLE bookings ADD COLUMN IF NOT EXISTS occurrence_date TIMESTAMPTZ;

-- Step 2: Backfill from the parent event's scheduled time
UPDATE bookings b
SET occurrence_date = e.time
FROM events e
WHERE b.event_id = e.id AND b.occurrence_date IS NULL;

-- Step 3: Make NOT NULL after backfill
ALTER TABLE bookings ALTER COLUMN occurrence_date SET NOT NULL;

-- Step 4: Index for efficient per-occurrence capacity queries
CREATE INDEX IF NOT EXISTS idx_bookings_event_occurrence
    ON bookings (event_id, occurrence_date)
    WHERE status IN ('pending', 'confirmed');
