package models

import (
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// MarketingPage is an admin-edited landing page served at the site root
// (/{slug}). It links to an event for live price/slots/booking, but its
// content is an independent copy: prefilled from the event at create time and
// never synced afterwards.
type MarketingPage struct {
	ID             uuid.UUID      `db:"id" json:"id"`
	Slug           string         `db:"slug" json:"slug"`
	EventID        uuid.UUID      `db:"event_id" json:"event_id"`
	Title          string         `db:"title" json:"title"`
	HookLine       *string        `db:"hook_line" json:"hook_line,omitempty"`
	CoverImageURL  *string        `db:"cover_image_url" json:"cover_image_url,omitempty"`
	GalleryURLs    pq.StringArray `db:"gallery_urls" json:"gallery_urls"`
	Content        string         `db:"content" json:"content"` // sanitized HTML from the rich text editor
	CTAText        *string        `db:"cta_text" json:"cta_text,omitempty"`
	SEODescription *string        `db:"seo_description" json:"seo_description,omitempty"`
	PublishedAt    *time.Time     `db:"published_at" json:"published_at,omitempty"`
	CreatedAt      time.Time      `db:"created_at" json:"created_at"`
	UpdatedAt      time.Time      `db:"updated_at" json:"updated_at"`

	// Joined from events (read-only): the linked event's public slug and title.
	EventSlug  string `db:"-" json:"event_slug"`
	EventTitle string `db:"-" json:"event_title"`
}
