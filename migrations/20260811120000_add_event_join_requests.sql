-- +migrate Up
-- RSVP / request-to-join as a second way to gate a private event.
--
-- A private event has always been unlocked by a passkey: the guest types a code
-- at the Book step and is in. RSVP inverts that — the guest asks, fills in the
-- event's attendee-details form, and the host (or an admin) approves. Approval
-- unlocks booking exactly as a correct passkey does; the guest still books and
-- still pays. No wallet is ever debited without the guest present.
--
--   private_access_mode  'passkey' (default, every existing private event)
--                        | 'rsvp'
--
-- The two modes are exclusive: an RSVP event has no access_passkey, and its
-- coupons cannot unlock access (they can still comp the price). Both invariants
-- are enforced in eventService, not here.
ALTER TABLE events ADD COLUMN IF NOT EXISTS private_access_mode VARCHAR(20) NOT NULL DEFAULT 'passkey';

-- Approval is per EVENT, not per occurrence: the host is vetting a person, not
-- a session. One approval lets that guest book any session of the event — which
-- is what "request to join" means, and keeps the gate independent of the
-- recurring / one-on-one session machinery.
CREATE TABLE IF NOT EXISTS event_join_requests (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id    UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    status      TEXT NOT NULL DEFAULT 'pending',  -- pending | approved | rejected | withdrawn

    -- The guest's free-text note ("why I want to join"). Their attendee-details
    -- answers are NOT stored here as the source of truth — those are upserted
    -- onto attendee_profiles exactly as the booking form does, so an approved
    -- guest doesn't hit "attendee details are required" at checkout.
    -- answers_snapshot is a copy taken at request time, for the host's review
    -- screen and as an audit trail of what was actually submitted.
    message           TEXT,
    answers_snapshot  JSONB NOT NULL DEFAULT '{}'::jsonb,

    -- Who decided. A host and a platform admin authenticate through different
    -- systems: a host is a user (and so has a hosts.id), while an admin JWT
    -- carries only a username. So the reviewer is stored as a kind plus EITHER
    -- an id (hosts) or a label (admins) — no FK, since no one table covers both.
    reviewed_by_kind  TEXT,        -- 'host' | 'admin'
    reviewed_by_id    UUID,        -- hosts.id when a host decided; NULL for admins
    reviewed_by_label TEXT,        -- admin username when an admin decided
    reviewed_at       TIMESTAMPTZ,
    review_note       TEXT,

    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- A guest may hold at most one live request per event. Rejected and withdrawn
-- rows are left out of the constraint so a guest can be re-invited to ask again.
CREATE UNIQUE INDEX IF NOT EXISTS event_join_requests_active_key
    ON event_join_requests (event_id, user_id)
    WHERE status IN ('pending', 'approved');

-- The host/admin queues read "pending requests, newest first" per event.
CREATE INDEX IF NOT EXISTS idx_join_requests_event_status
    ON event_join_requests (event_id, status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_join_requests_user
    ON event_join_requests (user_id, status);

-- +migrate Down
DROP TABLE IF EXISTS event_join_requests;
ALTER TABLE events DROP COLUMN IF EXISTS private_access_mode;
