package models

import (
	"time"

	"github.com/google/uuid"
)

// ProfileAlbum is a named photo album on a user's profile (Photos tab).
type ProfileAlbum struct {
	ID           uuid.UUID     `json:"id"`
	UserID       uuid.UUID     `json:"user_id"`
	Title        string        `json:"title"`
	Description  *string       `json:"description"`
	Position     int           `json:"position"`
	CoverPhotoID *uuid.UUID    `json:"cover_photo_id"`
	PhotoCount   int           `json:"photo_count"`
	Photos       []*AlbumPhoto `json:"photos"`
	CreatedAt    time.Time     `json:"created_at"`
	UpdatedAt    time.Time     `json:"updated_at"`
}

// AlbumPhoto is one photo inside a ProfileAlbum.
type AlbumPhoto struct {
	ID        uuid.UUID `json:"id"`
	AlbumID   uuid.UUID `json:"album_id"`
	URL       string    `json:"url"`
	Caption   *string   `json:"caption"`
	Width     *int      `json:"width"`
	Height    *int      `json:"height"`
	Position  int       `json:"position"`
	CreatedAt time.Time `json:"created_at"`
}
