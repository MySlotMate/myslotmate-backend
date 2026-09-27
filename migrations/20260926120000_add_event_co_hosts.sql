-- +migrate Up
-- Co-hosting: one host shares a single event with another host.
--
-- A co-host co-manages the event (edit, attendees, check-in, messages — the
-- authorization predicate lives in event_repository) and, when the owner grants
-- the withdraw toggle, may also pull that event's earnings out to their OWN
-- verified payout method.
--
-- The money model is deliberately NOT a revenue split: booking earnings keep
-- landing on the OWNER's account and ledger exactly as before. A co-host
-- withdrawal only redirects the payout DESTINATION, capped at that one event's
-- passed earnings, and every co-host withdrawal still passes the owner's normal
-- available-balance gate. So nothing here can create money the owner has not
-- earned, and no schema change touches bookings, payments or the ledger.
CREATE TABLE IF NOT EXISTS event_co_hosts (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id            UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    -- The invited host. Invitation is by email, but it only ever resolves to an
    -- existing host account — there is no pending-by-email state.
    host_id             UUID NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    invited_by_host_id  UUID NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,

    status              TEXT NOT NULL DEFAULT 'pending',  -- pending | accepted | declined | revoked

    -- Owner-controlled toggle. Gates FUTURE withdrawals only: turning it off
    -- never touches a payout that already went out.
    can_withdraw        BOOLEAN NOT NULL DEFAULT FALSE,

    responded_at        TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One live invite per (event, host). Declined / revoked rows are left out so the
-- owner can invite the same host again.
CREATE UNIQUE INDEX IF NOT EXISTS event_co_hosts_active_key
    ON event_co_hosts (event_id, host_id)
    WHERE status IN ('pending', 'accepted');

-- "who co-hosts this event" (owner's manage panel) and "what was shared with
-- me" (co-host's earnings page).
CREATE INDEX IF NOT EXISTS idx_event_co_hosts_event ON event_co_hosts (event_id, status);
CREATE INDEX IF NOT EXISTS idx_event_co_hosts_host  ON event_co_hosts (host_id, status);

-- A claim is one co-host withdrawal against one event's earnings. It exists so
-- the same event earnings cannot be paid out twice: the reserving INSERT checks
-- "requested <= event_passed_earnings − already-claimed" inside the statement
-- itself, which makes two simultaneous co-host withdrawals mutually exclusive.
--
-- Partial withdrawals are allowed: several claims may stack up against one
-- event until its passed earnings are exhausted.
--
-- Status mirrors the payment: reserved (in flight) | completed | failed.
-- Only 'failed' releases the reservation — so a payout stuck at 'processing'
-- keeps holding its slice, which is the safe direction.
CREATE TABLE IF NOT EXISTS event_cohost_claims (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_co_host_id  UUID NOT NULL REFERENCES event_co_hosts(id) ON DELETE CASCADE,
    event_id          UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    host_id           UUID NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,

    amount_cents      BIGINT NOT NULL CHECK (amount_cents > 0),
    status            TEXT NOT NULL DEFAULT 'reserved',  -- reserved | completed | failed

    -- Set right after the payout payment row is created. Nullable because the
    -- claim is reserved first (that is what makes the check atomic).
    payment_id        UUID REFERENCES payments(id) ON DELETE SET NULL,

    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_cohost_claims_event   ON event_cohost_claims (event_id, status);
CREATE INDEX IF NOT EXISTS idx_cohost_claims_payment ON event_cohost_claims (payment_id);

-- +migrate Down
DROP TABLE IF EXISTS event_cohost_claims;
DROP TABLE IF EXISTS event_co_hosts;
