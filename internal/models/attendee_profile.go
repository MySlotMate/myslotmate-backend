package models

import (
	"time"

	"github.com/google/uuid"
)

// AttendeeProfile is a user's reusable set of attendee-details answers. One row
// per user, upserted whenever they submit the attendee form for a booking, so
// future bookings can auto-fill. All fields are optional — only the fields an
// event actually requires get populated. Keys mirror the frontend field catalog.
type AttendeeProfile struct {
	UserID           uuid.UUID `db:"user_id" json:"user_id"`
	Name             *string   `db:"name" json:"name,omitempty"`
	Age              *int      `db:"age" json:"age,omitempty"`
	Gender           *string   `db:"gender" json:"gender,omitempty"`
	Qualification    *string   `db:"qualification" json:"qualification,omitempty"`
	Occupation       *string   `db:"occupation" json:"occupation,omitempty"`
	MaritalStatus    *string   `db:"marital_status" json:"marital_status,omitempty"`
	ContactNumber    *string   `db:"contact_number" json:"contact_number,omitempty"`
	WhatsappNumber   *string   `db:"whatsapp_number" json:"whatsapp_number,omitempty"`
	RegistrationType *string   `db:"registration_type" json:"registration_type,omitempty"`
	GovtIDURL        *string   `db:"govt_id_url" json:"govt_id_url,omitempty"`
	Travel           *bool     `db:"travel" json:"travel,omitempty"`
	CreatedAt        time.Time `db:"created_at" json:"created_at"`
	UpdatedAt        time.Time `db:"updated_at" json:"updated_at"`
}

// HasField reports whether the profile has a non-empty value for the given
// attendee-field catalog key. Used by the booking guard to validate that every
// required field was supplied.
func (p *AttendeeProfile) HasField(key string) bool {
	if p == nil {
		return false
	}
	nonEmptyStr := func(s *string) bool { return s != nil && *s != "" }
	switch key {
	case "name":
		return nonEmptyStr(p.Name)
	case "age":
		return p.Age != nil
	case "gender":
		return nonEmptyStr(p.Gender)
	case "qualification":
		return nonEmptyStr(p.Qualification)
	case "occupation":
		return nonEmptyStr(p.Occupation)
	case "marital_status":
		return nonEmptyStr(p.MaritalStatus)
	case "contact_number":
		return nonEmptyStr(p.ContactNumber)
	case "whatsapp_number":
		return nonEmptyStr(p.WhatsappNumber)
	case "registration_type":
		return nonEmptyStr(p.RegistrationType)
	case "govt_id_url":
		return nonEmptyStr(p.GovtIDURL)
	case "travel":
		return p.Travel != nil
	default:
		// Unknown keys are treated as satisfied so a stray config value can't
		// permanently block bookings.
		return true
	}
}
