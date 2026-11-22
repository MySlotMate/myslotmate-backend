-- +migrate Up
-- When the user last passed the phone OTP. The host application submit
-- requires a verification in the last 15 minutes (error phone_not_verified).
ALTER TABLE users ADD COLUMN IF NOT EXISTS phone_verified_at TIMESTAMPTZ NULL;

-- +migrate Down
ALTER TABLE users DROP COLUMN IF EXISTS phone_verified_at;
