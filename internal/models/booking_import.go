package models

import (
	"time"

	"github.com/google/uuid"
)

// Job / row status values. Free-text in the DB (matching the transaction_ledger
// convention in this repo) but always written through these constants.
const (
	ImportJobPending    = "pending"
	ImportJobProcessing = "processing"
	ImportJobCompleted  = "completed"
	ImportJobFailed     = "failed"

	// How a job's bookings were paid for. Derived server-side at upload time from
	// the event price and coupon — never taken from the client.
	//   free    — the event costs nothing.
	//   coupon  — paid event comped to ₹0 by a host coupon.
	//   offline — paid event; the host collected the fee outside the platform.
	//             These bookings carry amount_cents = 0 and generate NO ledger
	//             rows and NO host earnings: the host already holds the cash, so
	//             the platform owes them nothing for these seats.
	ImportPaymentFree    = "free"
	ImportPaymentCoupon  = "coupon"
	ImportPaymentOffline = "offline"

	ImportRowPending = "pending"
	ImportRowSuccess = "success"
	ImportRowFailed  = "failed"
)

// BookingImportJob is one uploaded spreadsheet of guests for one event occurrence.
//
// Status semantics: "completed" means the job finished walking its rows, not that
// every row booked. A 200-row file onto a 50-seat event completes with 50 success
// and 150 failed — that is a successful job with failed rows, not a failed job.
// "failed" is only for a job that could not run at all.
type BookingImportJob struct {
	ID             uuid.UUID `json:"id"`
	HostID         uuid.UUID `json:"host_id"`
	EventID        uuid.UUID `json:"event_id"`
	OccurrenceDate time.Time `json:"occurrence_date"`
	CouponCode     *string   `json:"coupon_code,omitempty"`
	PaymentMode    string    `json:"payment_mode"`
	// UnitPriceCents is the per-seat price at upload time, so the amount collected
	// offline stays reconstructable even if the event is later repriced.
	UnitPriceCents int64 `json:"unit_price_cents"`
	// OfflineAck records the host's explicit "I have collected payment for these
	// guests myself". Required (server-side) before an offline-mode job runs.
	OfflineAck    bool       `json:"offline_ack"`
	FileName      string     `json:"file_name"`
	Status        string     `json:"status"`
	ErrorMessage  *string    `json:"error_message,omitempty"`
	TotalRows     int        `json:"total_rows"`
	ProcessedRows int        `json:"processed_rows"`
	SuccessRows   int        `json:"success_rows"`
	FailedRows    int        `json:"failed_rows"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
}

// BookingImportRow is one spreadsheet line and its outcome.
type BookingImportRow struct {
	ID           uuid.UUID  `json:"id"`
	JobID        uuid.UUID  `json:"job_id"`
	RowNumber    int        `json:"row_number"`
	GuestName    string     `json:"guest_name"`
	GuestPhone   string     `json:"guest_phone"`
	Quantity     int        `json:"quantity"`
	Status       string     `json:"status"`
	ErrorMessage *string    `json:"error_message,omitempty"`
	BookingID    *uuid.UUID `json:"booking_id,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}
