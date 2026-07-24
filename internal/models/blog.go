package models

import (
	"time"

	"github.com/google/uuid"
)

// Blog represents a blog post
type Blog struct {
	ID               uuid.UUID  `db:"id" json:"id"`
	// Slug is the clean, URL-safe identifier used in public links
	// (/blogs/{slug}). Generated once at create time; immutable.
	Slug             string     `db:"slug" json:"slug"`
	Title            string     `db:"title" json:"title"`
	Description      *string    `db:"description" json:"description,omitempty"`
	Category         string     `db:"category" json:"category"`
	Content          string     `db:"content" json:"content"`
	CoverImageURL    *string    `db:"cover_image_url" json:"cover_image_url,omitempty"`
	AuthorID         uuid.UUID  `db:"author_id" json:"author_id"`
	AuthorName       string     `db:"author_name" json:"author_name"`
	ReadTimeMinutes  int        `db:"read_time_minutes" json:"read_time_minutes"`
	PublishedAt      *time.Time `db:"published_at" json:"published_at,omitempty"`
	CreatedAt        time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt        time.Time  `db:"updated_at" json:"updated_at"`
}
