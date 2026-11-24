-- +migrate Up
-- Co-host invitations sent to an email that has no host profile yet (not on
-- MySlotMate, or a guest who hasn't become a host). event_co_hosts needs a
-- host, so these wait here; once a host with that email exists, the invite is
-- turned into a normal pending event_co_hosts row (status 'joined') the next
-- time that host loads their dashboard.
CREATE TABLE IF NOT EXISTS event_cohost_email_invites (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id           UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    email              TEXT NOT NULL CHECK (email = lower(btrim(email)) AND email <> ''),
    invited_by_host_id UUID NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    can_withdraw       BOOLEAN NOT NULL DEFAULT false,
    status             TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'joined', 'revoked')),
    co_host_id         UUID REFERENCES event_co_hosts(id) ON DELETE SET NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- One open invite per event and email.
CREATE UNIQUE INDEX IF NOT EXISTS event_cohost_email_invites_open_key
    ON event_cohost_email_invites (event_id, email) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_event_cohost_email_invites_email
    ON event_cohost_email_invites (email) WHERE status = 'pending';

-- +migrate Down
DROP TABLE IF EXISTS event_cohost_email_invites;
