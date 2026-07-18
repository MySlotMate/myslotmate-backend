-- +migrate Up
-- Door check-in for bookings. A booking covers `quantity` guests who need not
-- all arrive at once, so check-in is a running count rather than a boolean: the
-- host scans the same ticket each time part of the group turns up and admits
-- however many are at the door. The CHECK keeps the count from exceeding the
-- tickets actually paid for; the guarded UPDATE in the repository relies on it.

ALTER TABLE bookings ADD COLUMN IF NOT EXISTS checked_in_count INT NOT NULL DEFAULT 0;
ALTER TABLE bookings ADD COLUMN IF NOT EXISTS last_checked_in_at TIMESTAMPTZ;

ALTER TABLE bookings DROP CONSTRAINT IF EXISTS bookings_checked_in_count_range;
ALTER TABLE bookings ADD CONSTRAINT bookings_checked_in_count_range
  CHECK (checked_in_count >= 0 AND checked_in_count <= quantity);

-- +migrate Down
ALTER TABLE bookings DROP CONSTRAINT IF EXISTS bookings_checked_in_count_range;
ALTER TABLE bookings DROP COLUMN IF EXISTS last_checked_in_at;
ALTER TABLE bookings DROP COLUMN IF EXISTS checked_in_count;
