-- +migrate Up
-- Indexes for the homepage feed (GET /home).
--
-- The feed only ever asks for live, future events ordered by start time, and
-- active hosts ordered by rating. Partial indexes keep them small: drafts,
-- cancelled and past events are the bulk of the table over time and none of
-- them belong in any of these scans.

-- Drives: WHERE status = 'live' AND time > now() ORDER BY time
CREATE INDEX IF NOT EXISTS events_live_upcoming_idx
  ON events (time)
  WHERE status = 'live';

-- Drives the host join on the feed, and every "events by host" screen.
CREATE INDEX IF NOT EXISTS events_host_id_idx
  ON events (host_id);

-- Drives: WHERE is_active ORDER BY avg_rating DESC
CREATE INDEX IF NOT EXISTS hosts_active_rating_idx
  ON hosts (avg_rating DESC)
  WHERE is_active;

-- Drives the category counts (GROUP BY mood over live upcoming events).
CREATE INDEX IF NOT EXISTS events_live_mood_idx
  ON events (mood)
  WHERE status = 'live';
