-- +migrate Up
-- Clean, URL-safe slugs for public links: /experience/{slug}, /host/{slug},
-- /blogs/{slug}. Slugs are generated once at create time and are immutable, so
-- shared links never break. Old UUID URLs keep working because the API resolves
-- either a slug or a UUID.
--
-- Backfill derives each slug from the record's title/name, lowercasing and
-- hyphenating, then disambiguates same-base collisions with a numeric suffix
-- (foo, foo-2, foo-3 …) matching the Go generator's scheme.

-- ── events ───────────────────────────────────────────────────────────────────
ALTER TABLE events ADD COLUMN IF NOT EXISTS slug TEXT;

WITH base AS (
	SELECT id, created_at,
		COALESCE(NULLIF(trim(BOTH '-' FROM regexp_replace(lower(title), '[^a-z0-9]+', '-', 'g')), ''), 'event') AS b
	FROM events
),
numbered AS (
	SELECT id, b,
		row_number() OVER (PARTITION BY b ORDER BY created_at, id) AS rn
	FROM base
)
UPDATE events e
SET slug = CASE WHEN n.rn = 1 THEN n.b ELSE n.b || '-' || n.rn END
FROM numbered n
WHERE e.id = n.id;

ALTER TABLE events ALTER COLUMN slug SET NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS events_slug_key ON events (slug);

-- ── hosts ────────────────────────────────────────────────────────────────────
ALTER TABLE hosts ADD COLUMN IF NOT EXISTS slug TEXT;

WITH base AS (
	SELECT id, created_at,
		COALESCE(NULLIF(trim(BOTH '-' FROM regexp_replace(lower(concat_ws(' ', first_name, last_name)), '[^a-z0-9]+', '-', 'g')), ''), 'host') AS b
	FROM hosts
),
numbered AS (
	SELECT id, b,
		row_number() OVER (PARTITION BY b ORDER BY created_at, id) AS rn
	FROM base
)
UPDATE hosts h
SET slug = CASE WHEN n.rn = 1 THEN n.b ELSE n.b || '-' || n.rn END
FROM numbered n
WHERE h.id = n.id;

ALTER TABLE hosts ALTER COLUMN slug SET NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS hosts_slug_key ON hosts (slug);

-- ── blogs ────────────────────────────────────────────────────────────────────
ALTER TABLE blogs ADD COLUMN IF NOT EXISTS slug TEXT;

WITH base AS (
	SELECT id, created_at,
		COALESCE(NULLIF(trim(BOTH '-' FROM regexp_replace(lower(title), '[^a-z0-9]+', '-', 'g')), ''), 'blog') AS b
	FROM blogs
),
numbered AS (
	SELECT id, b,
		row_number() OVER (PARTITION BY b ORDER BY created_at, id) AS rn
	FROM base
)
UPDATE blogs bl
SET slug = CASE WHEN n.rn = 1 THEN n.b ELSE n.b || '-' || n.rn END
FROM numbered n
WHERE bl.id = n.id;

ALTER TABLE blogs ALTER COLUMN slug SET NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS blogs_slug_key ON blogs (slug);

-- +migrate Down
DROP INDEX IF EXISTS events_slug_key;
ALTER TABLE events DROP COLUMN IF EXISTS slug;
DROP INDEX IF EXISTS hosts_slug_key;
ALTER TABLE hosts DROP COLUMN IF EXISTS slug;
DROP INDEX IF EXISTS blogs_slug_key;
ALTER TABLE blogs DROP COLUMN IF EXISTS slug;
