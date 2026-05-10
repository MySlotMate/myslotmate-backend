package models

import (
	"time"

	"github.com/google/uuid"
)

// User is a registered person. Only verified users can become hosts.
type User struct {
	ID         uuid.UUID  `db:"id" json:"id"`
	AuthUID    string     `db:"auth_uid" json:"auth_uid"`
	Name       string     `db:"name" json:"name"`
	PhnNumber  string     `db:"phn_number" json:"phn_number"`
	Email      string     `db:"email" json:"email"`
	AvatarURL  *string    `db:"avatar_url" json:"avatar_url,omitempty"`
	City       *string    `db:"city" json:"city,omitempty"`
	AccountID       *uuid.UUID `db:"account_id" json:"account_id,omitempty"`
	IsVerified      bool       `db:"is_verified" json:"is_verified"`
	IsPhoneVerified bool       `db:"is_phone_verified" json:"is_phone_verified"`
	OTP             *string    `db:"otp" json:"otp,omitempty"`
	OTPExpiresAt    *time.Time `db:"otp_expires_at" json:"otp_expires_at,omitempty"`
	VerifiedAt      *time.Time `db:"verified_at" json:"verified_at,omitempty"`
	CreatedAt  time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt  time.Time  `db:"updated_at" json:"updated_at"`
}
