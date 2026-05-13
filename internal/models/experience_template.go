package models

import (
	"time"

	"github.com/google/uuid"
)

// ExperienceTemplate is a reusable mood-keyed suggestion (title + hook_line)
// shown in the event creation form. Hosts can pick one to prefill fields,
// then edit freely.
type ExperienceTemplate struct {
	ID        uuid.UUID `db:"id" json:"id"`
	Mood      string    `db:"mood" json:"mood"`
	Title     string    `db:"title" json:"title"`
	HookLine  string    `db:"hook_line" json:"hook_line"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
	UpdatedAt time.Time `db:"updated_at" json:"updated_at"`
}
