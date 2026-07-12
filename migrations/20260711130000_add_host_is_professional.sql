-- +migrate Up
-- Add an is_professional flag to hosts. Set from the host application form
-- (the applicant self-declares) and used by the Explore "Professionals only"
-- filter to surface professional hosts and their events.

ALTER TABLE hosts ADD COLUMN IF NOT EXISTS is_professional BOOLEAN NOT NULL DEFAULT FALSE;

-- +migrate Down
ALTER TABLE hosts DROP COLUMN IF EXISTS is_professional;
