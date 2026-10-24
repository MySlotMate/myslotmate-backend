-- +migrate Up
-- Profile "Connect": one row per pair of users. A request is `pending` until
-- the addressee accepts it; decline, cancel and disconnect all delete the row.
-- Not to be confused with the Instagram/LinkedIn "social connect" on hosts.
CREATE TABLE IF NOT EXISTS user_connections (
    requester_id  UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    addressee_id  UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    status        TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'accepted')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    responded_at  TIMESTAMPTZ,
    PRIMARY KEY (requester_id, addressee_id),
    CHECK (requester_id <> addressee_id)
);

-- One row per pair whichever way round: A→B and B→A cannot both exist.
CREATE UNIQUE INDEX IF NOT EXISTS idx_user_connections_pair
    ON user_connections (LEAST(requester_id, addressee_id), GREATEST(requester_id, addressee_id));
CREATE INDEX IF NOT EXISTS idx_user_connections_addressee ON user_connections (addressee_id, status);

-- +migrate Down
DROP TABLE IF EXISTS user_connections;
