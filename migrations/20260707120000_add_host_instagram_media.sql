-- +migrate Up
-- Media pulled once from the host's Instagram profile when they apply without
-- a profile photo: gallery_urls holds up to 3 recent post photos (re-hosted on
-- S3, shown in the host page gallery), instagram_scraped_at marks that the
-- one-time scrape already ran so it is never repeated.
ALTER TABLE hosts ADD COLUMN gallery_urls TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE hosts ADD COLUMN instagram_scraped_at TIMESTAMPTZ NULL;

-- +migrate Down
ALTER TABLE hosts DROP COLUMN IF EXISTS gallery_urls;
ALTER TABLE hosts DROP COLUMN IF EXISTS instagram_scraped_at;
