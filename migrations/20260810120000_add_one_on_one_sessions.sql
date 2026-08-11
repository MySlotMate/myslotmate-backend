-- +migrate Up
-- One-on-one session support.
--
-- A 1-on-1 experience always has capacity 1, so the existing per-occurrence
-- availability, booking and pause machinery works unchanged. Two flavours:
--
--   dated  — the host named specific days. Sessions are expanded once and
--            stored in events.custom_dates (schedule_type = custom_dates).
--   weekly — the host declared repeating office hours. Nothing is stored in
--            custom_dates; the sessions are generated from session_windows on
--            every read, so the calendar never runs out (schedule_type =
--            recurring, is_recurring = true).
--
-- session_windows is therefore both the host's editable input AND, for the
-- weekly flavour, the source of truth for what is bookable.
--
--   session_type    'group' (default, every existing event) | 'one_on_one'
--   break_minutes   gap between consecutive sessions in a window; 0 = back-to-back
--   session_windows dated:  [{"date":"2026-08-15","start":"10:00","end":"12:00"}]
--                   weekly: [{"weekday":1,"start":"10:00","end":"12:00"}]
--                   weekday is 0=Sunday..6=Saturday; all times are IST
--                   wall-clock, matching the host's input

ALTER TABLE events ADD COLUMN IF NOT EXISTS session_type VARCHAR(20) NOT NULL DEFAULT 'group';
ALTER TABLE events ADD COLUMN IF NOT EXISTS break_minutes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE events ADD COLUMN IF NOT EXISTS session_windows JSONB NOT NULL DEFAULT '[]'::jsonb;

-- +migrate Down
ALTER TABLE events DROP COLUMN IF EXISTS session_type;
ALTER TABLE events DROP COLUMN IF EXISTS break_minutes;
ALTER TABLE events DROP COLUMN IF EXISTS session_windows;
