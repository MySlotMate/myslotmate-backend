-- +migrate Up
-- Push notifications for the mobile app (Expo push service), and the in-app
-- notification inbox they are recorded in.
--
-- push_tokens: one row per device. The token is the key, so a phone that signs
-- in to another account moves to that user instead of notifying both.
CREATE TABLE IF NOT EXISTS push_tokens (
    token       TEXT PRIMARY KEY,
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    platform    TEXT NOT NULL DEFAULT 'unknown',  -- android | ios | unknown
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_push_tokens_user ON push_tokens (user_id);

-- booking_reminders: which bookings have had their "starting soon" push. Kept
-- out of bookings itself so nothing that reads or scans bookings rows changes.
-- The primary key is the send-once guard: the reminder job claims a booking by
-- inserting here first, so two runs can never both send.
CREATE TABLE IF NOT EXISTS booking_reminders (
    booking_id  UUID PRIMARY KEY REFERENCES bookings(id) ON DELETE CASCADE,
    sent_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- notifications: the in-app inbox behind the home-screen bell. Every push the
-- server sends is also written here, so it can be read later even if the
-- device was off, the token was gone, or push was never allowed.
CREATE TABLE IF NOT EXISTS notifications (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    title       TEXT NOT NULL,
    body        TEXT NOT NULL,
    url         TEXT,             -- in-app route, e.g. /journey/<booking id>
    read_at     TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_notifications_user_created ON notifications (user_id, created_at DESC);

-- +migrate Down
DROP TABLE IF EXISTS notifications;
DROP TABLE IF EXISTS booking_reminders;
DROP TABLE IF EXISTS push_tokens;
