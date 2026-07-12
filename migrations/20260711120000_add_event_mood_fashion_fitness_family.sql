-- +migrate Up
-- Add 'fashion', 'fitness', and 'family' to the event_mood enum. The Go model
-- (models.NormalizeEventMood) and the create-experience form already offer
-- these moods, and experience_templates seeds title suggestions for them, but
-- the enum was never extended — so saving an event with any of these moods
-- failed with "invalid input value for enum event_mood".

ALTER TYPE event_mood ADD VALUE IF NOT EXISTS 'fashion';
ALTER TYPE event_mood ADD VALUE IF NOT EXISTS 'fitness';
ALTER TYPE event_mood ADD VALUE IF NOT EXISTS 'family';

-- +migrate Down
-- PostgreSQL does not support removing values from an enum. The values will
-- remain even if this migration is rolled back — that is safe because nothing
-- depends on their absence.
