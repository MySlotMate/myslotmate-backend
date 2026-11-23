-- +migrate Up
-- Marketing landing pages served at the site root (myslotmate.com/{slug}).
-- Each page is linked to one event (for live price, slots and the Book CTA)
-- but owns its own copy of the content: it is prefilled from the event once
-- at create time and never synced afterwards, so marketing can rewrite it
-- freely without touching the host's event.
CREATE TABLE IF NOT EXISTS marketing_pages (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  slug TEXT NOT NULL,
  event_id UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE,
  title TEXT NOT NULL,
  hook_line TEXT,
  cover_image_url TEXT,
  gallery_urls TEXT[] NOT NULL DEFAULT '{}',
  content TEXT NOT NULL DEFAULT '',
  cta_text TEXT,
  seo_description TEXT,
  published_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS marketing_pages_slug_key ON marketing_pages (slug);
CREATE INDEX IF NOT EXISTS idx_marketing_pages_event_id ON marketing_pages (event_id);

-- +migrate Down
DROP TABLE IF EXISTS marketing_pages;
