-- Admin-settable overrides for the three headline stats on a host's public
-- profile (Experiences Hosted / People Met / Avg. Rating).
--
-- All three are normally derived: the first two are computed from the host's
-- events, and avg_rating is recomputed by review_service on every new review.
-- These columns let an admin pin a value instead. NULL = no override, i.e. keep
-- showing the derived number.
--
-- avg_rating_override is deliberately a separate column from avg_rating: writing
-- into avg_rating itself would be silently reverted the next time a review lands.

ALTER TABLE hosts ADD COLUMN IF NOT EXISTS events_hosted_override INTEGER;
ALTER TABLE hosts ADD COLUMN IF NOT EXISTS people_met_override INTEGER;
ALTER TABLE hosts ADD COLUMN IF NOT EXISTS avg_rating_override DOUBLE PRECISION;
