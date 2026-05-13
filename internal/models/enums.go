package models

import (
	"fmt"
	"strings"
)

// AccountOwnerType is the owner of an account (user, host, or platform).
type AccountOwnerType string

const (
	AccountOwnerUser     AccountOwnerType = "user"
	AccountOwnerHost     AccountOwnerType = "host"
	AccountOwnerPlatform AccountOwnerType = "platform"
)

// BookingStatus represents the lifecycle of a booking.
type BookingStatus string

const (
	BookingStatusPending   BookingStatus = "pending"
	BookingStatusConfirmed BookingStatus = "confirmed"
	BookingStatusCancelled BookingStatus = "cancelled"
	BookingStatusRefunded  BookingStatus = "refunded"
)

// PaymentType is the kind of payment transaction.
type PaymentType string

const (
	PaymentTypeBooking    PaymentType = "booking"
	PaymentTypeWithdrawal PaymentType = "withdrawal"
	PaymentTypeRefund     PaymentType = "refund"
	PaymentTypePayout     PaymentType = "payout"
	PaymentTypeTopup      PaymentType = "topup"
)

// PaymentStatus is the status of a payment.
type PaymentStatus string

const (
	PaymentStatusPending    PaymentStatus = "pending"
	PaymentStatusProcessing PaymentStatus = "processing"
	PaymentStatusCompleted  PaymentStatus = "completed"
	PaymentStatusFailed     PaymentStatus = "failed"
	PaymentStatusReversed   PaymentStatus = "reversed"
)

// PayoutMethodType is the type of payout method (bank or UPI).
type PayoutMethodType string

const (
	PayoutMethodBank PayoutMethodType = "bank"
	PayoutMethodUPI  PayoutMethodType = "upi"
)

// SupportTicketStatus is the status of a support ticket.
type SupportTicketStatus string

const (
	SupportTicketOpen       SupportTicketStatus = "open"
	SupportTicketInProgress SupportTicketStatus = "in_progress"
	SupportTicketResolved   SupportTicketStatus = "resolved"
	SupportTicketClosed     SupportTicketStatus = "closed"
)

// FraudFlagType is the reason for a fraud flag.
type FraudFlagType string

const (
	FraudFlagAbnormalBookingSpike FraudFlagType = "abnormal_booking_spike"
	FraudFlagPaymentAbuse         FraudFlagType = "payment_abuse"
	FraudFlagSuspiciousActivity   FraudFlagType = "suspicious_activity"
	FraudFlagManualBlock          FraudFlagType = "manual_block"
)

// HostApplicationStatus is the lifecycle of a host application.
type HostApplicationStatus string

const (
	HostApplicationDraft       HostApplicationStatus = "draft"
	HostApplicationPending     HostApplicationStatus = "pending" // submitted
	HostApplicationUnderReview HostApplicationStatus = "under_review"
	HostApplicationApproved    HostApplicationStatus = "approved"
	HostApplicationRejected    HostApplicationStatus = "rejected"
)

// EventStatus is the publication status of an experience/event.
type EventStatus string

const (
	EventStatusDraft  EventStatus = "draft"
	EventStatusLive   EventStatus = "live"
	EventStatusPaused EventStatus = "paused"
)

// EventMood is the mood/category tag for an experience.
type EventMood string

const (
	EventMoodAdventurous EventMood = "adventurous"
	EventMoodRelaxing    EventMood = "relaxing"
	EventMoodCreative    EventMood = "creative"
	EventMoodSocial      EventMood = "social"
	EventMoodEducational EventMood = "educational"
	EventMoodWellness    EventMood = "wellness"
	EventMoodCulinary    EventMood = "culinary"
	EventMoodCultural    EventMood = "cultural"

	// Legacy DB/API aliases kept for backward compatibility.
	EventMoodAdventureLegacy    EventMood = "adventure"
	EventMoodChillLegacy        EventMood = "chill"
	EventMoodRomanticLegacy     EventMood = "romantic"
	EventMoodIntellectualLegacy EventMood = "intellectual"
	EventMoodFoodieLegacy       EventMood = "foodie"
	EventMoodNightlifeLegacy    EventMood = "nightlife"
)

var canonicalEventMoodValues = []string{
	string(EventMoodAdventurous),
	string(EventMoodRelaxing),
	string(EventMoodCreative),
	string(EventMoodSocial),
	string(EventMoodEducational),
	string(EventMoodWellness),
	string(EventMoodCulinary),
	string(EventMoodCultural),
}

// NormalizeEventMood converts both legacy and frontend mood labels into the
// canonical frontend set used by the current product UI.
func NormalizeEventMood(mood *EventMood) (*EventMood, error) {
	if mood == nil {
		return nil, nil
	}

	normalized := strings.ToLower(strings.TrimSpace(string(*mood)))
	var canonical EventMood

	switch normalized {
	case string(EventMoodAdventurous), string(EventMoodAdventureLegacy):
		canonical = EventMoodAdventurous
	case string(EventMoodRelaxing), string(EventMoodChillLegacy):
		canonical = EventMoodRelaxing
	case string(EventMoodCreative), string(EventMoodRomanticLegacy):
		canonical = EventMoodCreative
	case string(EventMoodSocial):
		canonical = EventMoodSocial
	case string(EventMoodEducational), string(EventMoodIntellectualLegacy):
		canonical = EventMoodEducational
	case string(EventMoodWellness):
		canonical = EventMoodWellness
	case string(EventMoodCulinary), string(EventMoodFoodieLegacy):
		canonical = EventMoodCulinary
	case string(EventMoodCultural), string(EventMoodNightlifeLegacy):
		canonical = EventMoodCultural
	default:
		return nil, fmt.Errorf(
			"invalid event mood: %q (allowed: %s)",
			string(*mood),
			strings.Join(canonicalEventMoodValues, ", "),
		)
	}

	return &canonical, nil
}

// CancellationPolicy defines the refund policy for an experience.
type CancellationPolicy string

const (
	CancellationFlexible CancellationPolicy = "flexible"  // Full refund 24h prior
	CancellationModerate CancellationPolicy = "moderate"  // Full refund 5 days prior
	CancellationStrict   CancellationPolicy = "strict"    // 50% refund 1 week prior
	CancellationNoRefund CancellationPolicy = "no_refund" // Non-refundable
)

// SupportCategory is the type of support request.
type SupportCategory string

const (
	SupportCategoryReportParticipant SupportCategory = "report_participant"
	SupportCategoryTechnicalSupport  SupportCategory = "technical_support"
	SupportCategoryPolicyHelp        SupportCategory = "policy_help"
)

// ReportReason is the specific reason for reporting a participant.
type ReportReason string

const (
	ReportReasonVerbalHarassment      ReportReason = "verbal_harassment"
	ReportReasonSafetyConcern         ReportReason = "safety_concern"
	ReportReasonInappropriateBehavior ReportReason = "inappropriate_behavior"
	ReportReasonSpamOrScam            ReportReason = "spam_or_scam"
)

// MessageSenderType identifies who sent a message in an inbox thread.
type MessageSenderType string

const (
	MessageSenderSystem MessageSenderType = "system"
	MessageSenderHost   MessageSenderType = "host"
	MessageSenderGuest  MessageSenderType = "guest"
)
