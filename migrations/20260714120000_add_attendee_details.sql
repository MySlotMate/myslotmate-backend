-- +migrate Up
-- Per-event attendee-details config + a reusable per-user attendee profile.
-- Admins mark an event as requiring extra attendee details and pick which fields
-- from a fixed catalog are needed. A guest fills those on booking; the answers are
-- upserted onto their attendee_profiles row so future bookings auto-fill.

ALTER TABLE events ADD COLUMN IF NOT EXISTS requires_attendee_details BOOLEAN NOT NULL DEFAULT FALSE;
-- Nullable (matches the existing gallery_urls/languages array columns): the
-- INSERT always lists this column and passes pq.Array(nil) for events created
-- without attendee config, which serializes to SQL NULL. A nullable column
-- accepts that; the booking guard treats NULL/empty as "no fields required".
ALTER TABLE events ADD COLUMN IF NOT EXISTS attendee_fields TEXT[] DEFAULT '{}';

CREATE TABLE IF NOT EXISTS attendee_profiles (
    user_id           UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    name              TEXT,
    age               INT,
    gender            TEXT,
    qualification     TEXT,
    occupation        TEXT,
    marital_status    TEXT,
    contact_number    TEXT,
    whatsapp_number   TEXT,
    registration_type TEXT,
    govt_id_url       TEXT,
    travel            BOOLEAN,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +migrate Down
DROP TABLE IF EXISTS attendee_profiles;
ALTER TABLE events DROP COLUMN IF EXISTS attendee_fields;
ALTER TABLE events DROP COLUMN IF EXISTS requires_attendee_details;
