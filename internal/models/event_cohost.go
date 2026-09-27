package models

import (
	"time"

	"github.com/google/uuid"
)

// CoHostStatus is the lifecycle of one co-host invitation.
type CoHostStatus string

const (
	CoHostPending  CoHostStatus = "pending"
	CoHostAccepted CoHostStatus = "accepted"
	CoHostDeclined CoHostStatus = "declined"
	CoHostRevoked  CoHostStatus = "revoked"
)

// EventCoHost is another host sharing ONE event with its owner. An accepted
// co-host co-manages that event (edit, attendees, check-in — see
// EventRepository.HostCanManage) and, when CanWithdraw is granted by the owner,
// may withdraw that event's passed earnings to their own verified payout
// method.
//
// It is not a revenue split: earnings stay on the owner's account, only the
// payout destination is redirected. See migration 20260926120000.
type EventCoHost struct {
	ID      uuid.UUID `db:"id" json:"id"`
	EventID uuid.UUID `db:"event_id" json:"event_id"`
	// HostID is the invited host. Invites resolve to an existing host account —
	// there is no pending-by-email row.
	HostID          uuid.UUID    `db:"host_id" json:"host_id"`
	InvitedByHostID uuid.UUID    `db:"invited_by_host_id" json:"invited_by_host_id"`
	Status          CoHostStatus `db:"status" json:"status"`
	// CanWithdraw gates FUTURE withdrawals only; revoking it never reverses a
	// payout that already went out.
	CanWithdraw bool       `db:"can_withdraw" json:"can_withdraw"`
	RespondedAt *time.Time `db:"responded_at" json:"responded_at,omitempty"`
	CreatedAt   time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time  `db:"updated_at" json:"updated_at"`
}

// CoHostClaim statuses — one co-host withdrawal against one event's earnings.
// Only "failed" releases the reserved slice back to the event pool.
const (
	CoHostClaimReserved  = "reserved"
	CoHostClaimCompleted = "completed"
	CoHostClaimFailed    = "failed"
)
