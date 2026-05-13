-- Migration: Add granular pausing options to events
-- Added on: 2026-05-11
-- Description: Adds paused_from and paused_dates to support pausing recurring events from a specific date or for specific occurrences.

ALTER TABLE events ADD COLUMN IF NOT EXISTS paused_from TIMESTAMPTZ;
ALTER TABLE events ADD COLUMN IF NOT EXISTS paused_dates TIMESTAMPTZ[] DEFAULT '{}';
