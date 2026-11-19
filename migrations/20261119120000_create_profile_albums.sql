-- +migrate Up
-- Profile "Photos" tab: named albums owned by a user (hosts are users too), each
-- holding ordered photos. Photo files live in S3; rows only keep their URLs.
CREATE TABLE IF NOT EXISTS profile_albums (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id        UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    title          TEXT NOT NULL CHECK (char_length(title) BETWEEN 1 AND 40),
    description    TEXT CHECK (char_length(description) <= 280),
    position       INT  NOT NULL DEFAULT 0,
    cover_photo_id UUID,                      -- optional; NULL = first photo
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS profile_albums_user_idx ON profile_albums (user_id, position);

CREATE TABLE IF NOT EXISTS album_photos (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    album_id   UUID NOT NULL REFERENCES profile_albums(id) ON DELETE CASCADE,
    url        TEXT NOT NULL,
    caption    TEXT CHECK (char_length(caption) <= 200),
    width      INT,
    height     INT,
    position   INT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS album_photos_album_idx ON album_photos (album_id, position);
CREATE INDEX IF NOT EXISTS album_photos_url_idx ON album_photos (url);

-- Deleting the cover photo falls back to "first photo" rather than leaving a
-- dangling id.
ALTER TABLE profile_albums DROP CONSTRAINT IF EXISTS profile_albums_cover_photo_fk;
ALTER TABLE profile_albums ADD CONSTRAINT profile_albums_cover_photo_fk
    FOREIGN KEY (cover_photo_id) REFERENCES album_photos(id) ON DELETE SET NULL;

-- +migrate Down
ALTER TABLE profile_albums DROP CONSTRAINT IF EXISTS profile_albums_cover_photo_fk;
DROP TABLE IF EXISTS album_photos;
DROP TABLE IF EXISTS profile_albums;
