package models

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// PrivateAccessMode is how a private event decides who may book.
//
//   - passkey: the guest types a code at the Book step (the original behaviour).
//   - rsvp:    the guest asks to join and fills in the event's attendee-details
//     form; a host or admin approves. Approval unlocks booking — it
//     does NOT book or charge anything on the guest's behalf.
type PrivateAccessMode string

const (
	PrivateAccessModePasskey PrivateAccessMode = "passkey"
	PrivateAccessModeRSVP    PrivateAccessMode = "rsvp"
)

// JoinRequestStatus tracks one guest's request against one event.
type JoinRequestStatus string

const (
	JoinRequestPending   JoinRequestStatus = "pending"
	JoinRequestApproved  JoinRequestStatus = "approved"
	JoinRequestRejected  JoinRequestStatus = "rejected"
	JoinRequestWithdrawn JoinRequestStatus = "withdrawn"
)

// ReviewerKind distinguishes the two authentication systems that can decide a
// request. Hosts sign in as users; platform admins have their own JWT, so the
// reviewer reference cannot be a single foreign key.
type ReviewerKind string

const (
	ReviewerKindHost  ReviewerKind = "host"
	ReviewerKindAdmin ReviewerKind = "admin"
)

// JoinAnswers is the snapshot of attendee-details answers as submitted with a
// request. It is a record of what the guest actually sent, not the source of
// truth — the live answers live on the guest's AttendeeProfile, which is what
// the booking guard reads.
type JoinAnswers map[string]any

func (a *JoinAnswers) Scan(src interface{}) error {
	if src == nil {
		*a = nil
		return nil
	}
	var data []byte
	switch v := src.(type) {
	case []byte:
		data = v
	case string:
		data = []byte(v)
	default:
		return fmt.Errorf("answers_snapshot: unsupported scan type %T", src)
	}
	if len(data) == 0 {
		*a = nil
		return nil
	}
	return json.Unmarshal(data, a)
}

func (a JoinAnswers) Value() (driver.Value, error) {
	if a == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(a)
}

// EventJoinRequest is one guest asking to be let into one SESSION of a private
// event.
//
// Approval is per occurrence, not per event: a host vets each date separately,
// so a guest welcome on the 23rd is not automatically welcome on the 30th. A
// guest may therefore hold several live requests against one event — one per
// slot they asked about.
type EventJoinRequest struct {
	ID      uuid.UUID `db:"id" json:"id"`
	EventID uuid.UUID `db:"event_id" json:"event_id"`
	UserID  uuid.UUID `db:"user_id" json:"user_id"`

	// OccurrenceDate is the exact session this request is for. It matches the
	// occurrence_date the eventual booking will carry, which is what lets the
	// booking gate compare the two directly.
	OccurrenceDate time.Time `db:"occurrence_date" json:"occurrence_date"`

	Status          JoinRequestStatus `db:"status" json:"status"`
	Message         *string           `db:"message" json:"message,omitempty"`
	AnswersSnapshot JoinAnswers       `db:"answers_snapshot" json:"answers_snapshot"`

	ReviewedByKind  *ReviewerKind `db:"reviewed_by_kind" json:"reviewed_by_kind,omitempty"`
	ReviewedByID    *uuid.UUID    `db:"reviewed_by_id" json:"reviewed_by_id,omitempty"`
	ReviewedByLabel *string       `db:"reviewed_by_label" json:"reviewed_by_label,omitempty"`
	ReviewedAt      *time.Time    `db:"reviewed_at" json:"reviewed_at,omitempty"`
	ReviewNote      *string       `db:"review_note" json:"review_note,omitempty"`

	CreatedAt time.Time `db:"created_at" json:"created_at"`
	UpdatedAt time.Time `db:"updated_at" json:"updated_at"`

	// Joined for the host/admin review screens (not columns).
	UserName      string  `db:"-" json:"user_name,omitempty"`
	UserEmail     string  `db:"-" json:"user_email,omitempty"`
	UserPhone     string  `db:"-" json:"user_phone,omitempty"`
	UserAvatarURL *string `db:"-" json:"user_avatar_url,omitempty"`
	EventTitle    string  `db:"-" json:"event_title,omitempty"`
	EventSlug     string  `db:"-" json:"event_slug,omitempty"`
}

// IsLive reports whether the request still occupies the guest's one active slot
// for this event (and so blocks a duplicate request).
func (r *EventJoinRequest) IsLive() bool {
	return r != nil &&
		(r.Status == JoinRequestPending || r.Status == JoinRequestApproved)
}
