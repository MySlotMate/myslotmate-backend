-- +migrate Up
-- Fields the onboarding flow collects but had nowhere to store.
--
-- Everything is nullable or defaulted: the columns are additive, and the Go
-- backend and web app keep working untouched because they never select them.

ALTER TABLE users
  ADD COLUMN IF NOT EXISTS username  text,
  ADD COLUMN IF NOT EXISTS bio       text,
  -- Chosen on select-interests. Plain text[] rather than event_mood[] because
  -- the picker's labels are broader than the mood enum; matching happens in the
  -- recommendation query, not the column type.
  ADD COLUMN IF NOT EXISTS interests text[] NOT NULL DEFAULT '{}',
  -- The three toggles listed on notification-permission.
  ADD COLUMN IF NOT EXISTS notify_new_experiences boolean NOT NULL DEFAULT true,
  ADD COLUMN IF NOT EXISTS notify_booking_updates boolean NOT NULL DEFAULT true,
  ADD COLUMN IF NOT EXISTS notify_community       boolean NOT NULL DEFAULT true;

-- profile-setup renders myslotmate.com/@handle, so handles must be unique and
-- case-insensitive. Partial: most users never set one, and NULLs must not collide.
CREATE UNIQUE INDEX IF NOT EXISTS users_username_key
  ON users (lower(username)) WHERE username IS NOT NULL;
