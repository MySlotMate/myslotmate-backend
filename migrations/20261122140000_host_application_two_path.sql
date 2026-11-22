-- +migrate Up
-- Two-path host application (frontend spec docs/backend-spec-host-application.md).
-- Stores every answer the /become-host form sends. The *_url document columns
-- for ID / selfie / certificate / degree are private: never put them on a
-- public host DTO.

ALTER TABLE hosts
    ADD COLUMN IF NOT EXISTS host_type TEXT NOT NULL DEFAULT 'community'
        CHECK (host_type IN ('community', 'professional')),
    ADD COLUMN IF NOT EXISTS selfie_url TEXT NULL,
    ADD COLUMN IF NOT EXISTS community_name TEXT NULL,
    ADD COLUMN IF NOT EXISTS community_category TEXT NULL,
    ADD COLUMN IF NOT EXISTS years_experience TEXT NULL,
    ADD COLUMN IF NOT EXISTS events_hosted TEXT NULL,
    ADD COLUMN IF NOT EXISTS portfolio_url TEXT NULL,
    ADD COLUMN IF NOT EXISTS portfolio_photo_urls TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS profession TEXT NULL,
    ADD COLUMN IF NOT EXISTS certificate_url TEXT NULL,
    ADD COLUMN IF NOT EXISTS degree_url TEXT NULL,
    ADD COLUMN IF NOT EXISTS license_number TEXT NULL,
    ADD COLUMN IF NOT EXISTS professional_body TEXT NULL,
    ADD COLUMN IF NOT EXISTS specialization TEXT NULL,
    ADD COLUMN IF NOT EXISTS work_history TEXT NULL,
    ADD COLUMN IF NOT EXISTS languages TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS achievements TEXT NULL,
    ADD COLUMN IF NOT EXISTS testimonial_url TEXT NULL,
    -- Reject already receives a reason; it was never stored.
    ADD COLUMN IF NOT EXISTS rejection_reason TEXT NULL;

-- Existing hosts: professionals were marked by an admin via is_professional.
UPDATE hosts SET host_type = 'professional' WHERE is_professional AND host_type <> 'professional';

-- +migrate Down
ALTER TABLE hosts
    DROP COLUMN IF EXISTS host_type,
    DROP COLUMN IF EXISTS selfie_url,
    DROP COLUMN IF EXISTS community_name,
    DROP COLUMN IF EXISTS community_category,
    DROP COLUMN IF EXISTS years_experience,
    DROP COLUMN IF EXISTS events_hosted,
    DROP COLUMN IF EXISTS portfolio_url,
    DROP COLUMN IF EXISTS portfolio_photo_urls,
    DROP COLUMN IF EXISTS profession,
    DROP COLUMN IF EXISTS certificate_url,
    DROP COLUMN IF EXISTS degree_url,
    DROP COLUMN IF EXISTS license_number,
    DROP COLUMN IF EXISTS professional_body,
    DROP COLUMN IF EXISTS specialization,
    DROP COLUMN IF EXISTS work_history,
    DROP COLUMN IF EXISTS languages,
    DROP COLUMN IF EXISTS achievements,
    DROP COLUMN IF EXISTS testimonial_url,
    DROP COLUMN IF EXISTS rejection_reason;
