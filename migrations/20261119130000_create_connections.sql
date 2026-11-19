-- +migrate Up
-- One-way "Connect" with a host, like following: saved instantly, nothing to
-- accept. Separate from user_connections (the mobile app's request/accept
-- person-to-person connections).
CREATE TABLE IF NOT EXISTS connections (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,  -- who pressed Connect
    host_id    UUID NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, host_id)
);
CREATE INDEX IF NOT EXISTS connections_host_idx ON connections (host_id, created_at DESC);

-- +migrate Down
DROP TABLE IF EXISTS connections;
