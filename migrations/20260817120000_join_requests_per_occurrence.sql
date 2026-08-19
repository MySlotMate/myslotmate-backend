-- +migrate Up
-- Move RSVP approval from per-EVENT to per-OCCURRENCE.
--
-- Approval used to be granted once per event: a guest vetted for a recurring
-- experience could then book every date it ran. Hosts want to decide each
-- session separately — a regular who is welcome on the 23rd is not
-- automatically welcome on the 30th — so a request now names the slot it is
-- for, and the booking gate checks that slot and no other.
--
-- Existing rows are removed rather than migrated: they carry an approval whose
-- meaning ("any date") no longer exists, and backfilling them to one date would
-- silently revoke access to the others without telling anybody. Confirmed with
-- the product owner that the only rows present are from testing.
DELETE FROM event_join_requests;

ALTER TABLE event_join_requests
    ADD COLUMN IF NOT EXISTS occurrence_date TIMESTAMPTZ NOT NULL;

-- One live request per guest per SLOT. A guest may now hold several at once on
-- the same event — one per date they asked about — which is the point.
DROP INDEX IF EXISTS event_join_requests_active_key;
CREATE UNIQUE INDEX IF NOT EXISTS event_join_requests_active_key
    ON event_join_requests (event_id, user_id, occurrence_date)
    WHERE status IN ('pending', 'approved');

-- The booking gate asks "is this guest approved for this exact slot?".
CREATE INDEX IF NOT EXISTS idx_join_requests_gate
    ON event_join_requests (event_id, user_id, occurrence_date, status);

-- +migrate Down
DROP INDEX IF EXISTS idx_join_requests_gate;
DROP INDEX IF EXISTS event_join_requests_active_key;
ALTER TABLE event_join_requests DROP COLUMN IF EXISTS occurrence_date;
CREATE UNIQUE INDEX IF NOT EXISTS event_join_requests_active_key
    ON event_join_requests (event_id, user_id)
    WHERE status IN ('pending', 'approved');
