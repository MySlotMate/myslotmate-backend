package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"myslotmate-backend/internal/lib/bookingimport"
	"myslotmate-backend/internal/lib/timeutil"
	"myslotmate-backend/internal/lib/worker"
	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"

	"github.com/google/uuid"
	"github.com/xuri/excelize/v2"
)

// BookingImportService turns an uploaded .xlsx of guests into bookings.
//
// Design — this is a batch DRIVER over walkInService.InitiateWalkIn, not a second
// booking implementation. Every row goes through the identical path the on-spot
// modal uses, so capacity locking, the duplicate guard, coupon redemption and the
// attendee-details gate all behave exactly as they do for a single booking.
//
// Money: a bulk upload has no per-row checkout, so the importer is confined to
// walk-in's zero-cost path — a free event, or a host coupon that comps the event
// to ₹0. StartImport rejects anything else up front. No wallet debit, ledger row
// or fee split is reachable from here, by construction.
//
// Concurrency: the whole job is ONE worker-pool task that walks its rows
// sequentially. Submitting a task per row would be actively harmful — the pool's
// 100-slot queue falls through to a bare `go task()` when full (worker/pool.go),
// so a large file would spawn hundreds of goroutines racing the same capacity
// check and blowing through the event's capacity.
type BookingImportService interface {
	// ValidateEventForImport reports whether this event can be bulk-imported at
	// all, so the UI can refuse before the host picks a file.
	ValidateEventForImport(ctx context.Context, hostID, eventID uuid.UUID, couponCode string) (*ImportEligibility, error)
	// StartImport parses + validates the sheet synchronously and returns a job
	// that is already processing in the background.
	StartImport(ctx context.Context, req BookingImportRequest) (*models.BookingImportJob, error)
	GetJob(ctx context.Context, hostID, jobID uuid.UUID) (*BookingImportStatus, error)
	// ListRows returns the job's rows, optionally narrowed to one status. Used by
	// the on-screen "Booked / Failed" report tabs. Kept off GetJob so the 2-second
	// poll stays small — a 1000-row job would otherwise ship the whole list on
	// every tick.
	ListRows(ctx context.Context, hostID, jobID uuid.UUID, status string) ([]*models.BookingImportRow, error)
	// BuildReport renders the downloadable .xlsx summarising the finished import.
	BuildReport(ctx context.Context, hostID, jobID uuid.UUID) (*excelize.File, string, error)
	ListJobs(ctx context.Context, hostID uuid.UUID, limit int) ([]*models.BookingImportJob, error)
	// ResumeInterrupted re-drives jobs abandoned by a process restart. Called once
	// at startup.
	ResumeInterrupted(ctx context.Context)
}

// BookingImportRequest is one upload.
type BookingImportRequest struct {
	HostID         uuid.UUID
	EventID        uuid.UUID
	OccurrenceDate *time.Time // required for recurring events, as with walk-in
	CouponCode     string     // comps a paid event to zero when it grants free booking
	FileName       string
	File           io.Reader
	// OfflineAck is the host confirming they have already collected payment for
	// these guests. Required for a paid event with no comp code.
	OfflineAck bool
}

// ImportEligibility tells the UI whether an event can be imported and, crucially,
// on what payment terms — so a host importing onto a PAID event is shown what
// they are taking responsibility for instead of silence.
type ImportEligibility struct {
	Importable bool `json:"importable"`
	// PaymentMode is free | coupon | offline (models.ImportPayment*).
	PaymentMode string `json:"payment_mode"`
	// UnitPriceCents is the per-seat price; 0 for a free event.
	UnitPriceCents int64 `json:"unit_price_cents"`
	// RequiresOfflineAck is true when PaymentMode is offline: the host must
	// confirm they collected the money themselves before the upload is accepted.
	RequiresOfflineAck bool     `json:"requires_offline_ack"`
	ExpectedHeaders    []string `json:"expected_headers"`
	MaxRows            int      `json:"max_rows"`
}

// BookingImportStatus is what the polling UI renders: the job counters plus the
// rows that failed, so the host can fix and re-upload just those.
type BookingImportStatus struct {
	Job        *models.BookingImportJob   `json:"job"`
	FailedRows []*models.BookingImportRow `json:"failed_rows"`
}

// Whole-job rejections, surfaced before any job row exists.
var (
	ErrImportNotOwner = errors.New("you do not own this event")
	// ErrImportOfflineAckRequired guards the money-adjacent case: importing onto a
	// paid event without a comp code means the host is asserting they collected
	// the fee themselves. That assertion must be explicit and recorded, so it is
	// enforced here and not merely by a checkbox in the UI.
	ErrImportOfflineAckRequired = errors.New("this event is paid — confirm you have collected payment from these guests before importing")
	ErrImportAttendeeDetails    = errors.New("this event collects extra attendee details, which the import template cannot carry — please use on-spot booking for it")
	ErrImportTiered             = errors.New("bulk import is not supported for events with ticket tiers")
	ErrImportJobNotFound        = errors.New("import job not found")
)

type bookingImportService struct {
	importRepo     repository.BookingImportRepository
	eventRepo      repository.EventRepository
	tierRepo       repository.EventPriceTierRepository
	walkInService  WalkInService
	bookingService BookingService
	workerPool     *worker.WorkerPool
}

func NewBookingImportService(
	ir repository.BookingImportRepository,
	er repository.EventRepository,
	tr repository.EventPriceTierRepository,
	ws WalkInService,
	bs BookingService,
	wp *worker.WorkerPool,
) BookingImportService {
	return &bookingImportService{
		importRepo:     ir,
		eventRepo:      er,
		tierRepo:       tr,
		walkInService:  ws,
		bookingService: bs,
		workerPool:     wp,
	}
}

// rowProcessTimeout bounds a single row's booking attempt so one wedged query
// cannot hold a pool worker forever.
const rowProcessTimeout = 30 * time.Second

// ValidateEventForImport enforces every whole-job precondition and reports the
// payment terms. Kept separate so the UI can call it when the host picks an
// event, and StartImport can call it again before spending time on the file.
//
// Paid events ARE importable. Since a bulk upload has no per-row checkout, they
// resolve to one of two zero-cost routes: a comp coupon, or the host declaring
// they collected the fee offline. Either way the booking is written at ₹0 with
// no ledger entry and no host earnings — see models.ImportPayment* and
// BookingCreateRequest.PaymentCollectedOffline.
func (s *bookingImportService) ValidateEventForImport(ctx context.Context, hostID, eventID uuid.UUID, couponCode string) (*ImportEligibility, error) {
	evt, err := s.eventRepo.GetByID(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if evt == nil {
		return nil, errors.New("event not found")
	}
	if evt.HostID != hostID {
		return nil, ErrImportNotOwner
	}

	// Tiered events have no single price the importer could apply — same reason
	// walk-in rejects them.
	tiers, err := s.tierRepo.ListByEventID(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if len(tiers) > 0 {
		return nil, ErrImportTiered
	}

	// The template is name/phone/quantity only, so an event that gates booking on
	// attendee answers would fail every single row inside CreateBooking. Reject it
	// here, with an explanation, rather than producing a 100%-failed job.
	if evt.RequiresAttendeeDetails && len(evt.AttendeeFields) > 0 {
		return nil, ErrImportAttendeeDetails
	}

	out := &ImportEligibility{
		Importable:      true,
		PaymentMode:     models.ImportPaymentFree,
		ExpectedHeaders: bookingimport.ExpectedHeaders(),
		MaxRows:         bookingimport.MaxRows,
	}

	if evt.IsFree || evt.PriceCents == nil || *evt.PriceCents <= 0 {
		return out, nil
	}
	out.UnitPriceCents = *evt.PriceCents

	code := strings.TrimSpace(couponCode)
	if code == "" {
		// Paid, no comp code → the host is on the hook for collecting the fee.
		out.PaymentMode = models.ImportPaymentOffline
		out.RequiresOfflineAck = true
		return out, nil
	}

	// Validate the code once, up front. uuid.Nil as the user means "no specific
	// guest" — the per-user redemption limit is the only check that consults it
	// and it is re-run per guest at booking time anyway. Row processing
	// re-validates and atomically redeems, so this preview never consumes a
	// redemption; it just means a typo'd code fails the upload instead of
	// failing all 200 rows one at a time.
	coupon, err := s.bookingService.ValidateCoupon(ctx, eventID, uuid.Nil, code)
	if err != nil {
		return nil, err
	}
	if !coupon.GrantsFree {
		return nil, errors.New("that code only grants access, not a free booking — use a code that makes the booking free, or clear it and confirm you collected payment yourself")
	}
	out.PaymentMode = models.ImportPaymentCoupon
	return out, nil
}

func (s *bookingImportService) StartImport(ctx context.Context, req BookingImportRequest) (*models.BookingImportJob, error) {
	eligibility, err := s.ValidateEventForImport(ctx, req.HostID, req.EventID, req.CouponCode)
	if err != nil {
		return nil, err
	}
	// The offline acknowledgment is enforced HERE, server-side. A checkbox that
	// only exists in React is not a record that the host accepted responsibility
	// for money the platform will never see.
	if eligibility.RequiresOfflineAck && !req.OfflineAck {
		return nil, ErrImportOfflineAckRequired
	}

	evt, err := s.eventRepo.GetByID(ctx, req.EventID)
	if err != nil {
		return nil, err
	}
	if evt == nil {
		return nil, errors.New("event not found")
	}
	// Resolve the one occurrence every row books onto, using the same rule as the
	// on-spot modal.
	occurrence, err := resolveWalkInOccurrence(evt, req.OccurrenceDate)
	if err != nil {
		return nil, err
	}

	// Parse synchronously — header problems must fail the upload with a 400, before
	// a job exists for the host to poll.
	parsed, err := bookingimport.Parse(req.File)
	if err != nil {
		return nil, err
	}

	job := &models.BookingImportJob{
		HostID:         req.HostID,
		EventID:        req.EventID,
		OccurrenceDate: occurrence,
		FileName:       req.FileName,
		Status:         models.ImportJobPending,
		TotalRows:      len(parsed),
		// Derived from the event + coupon, never taken from the client, so the
		// recorded mode can't disagree with what actually happened.
		PaymentMode:    eligibility.PaymentMode,
		UnitPriceCents: eligibility.UnitPriceCents,
		OfflineAck:     eligibility.RequiresOfflineAck && req.OfflineAck,
	}
	if code := strings.TrimSpace(req.CouponCode); code != "" {
		job.CouponCode = &code
	}

	rows := make([]*models.BookingImportRow, 0, len(parsed))
	for _, p := range parsed {
		row := &models.BookingImportRow{
			RowNumber:  p.RowNumber,
			GuestName:  p.Name,
			GuestPhone: p.Phone,
			Quantity:   p.Quantity,
			Status:     models.ImportRowPending,
		}
		// Rows the parser already rejected are stored as failed rather than
		// dropped, so the host's report accounts for every line in their file.
		if p.Err != "" {
			msg := p.Err
			row.Status = models.ImportRowFailed
			row.ErrorMessage = &msg
		}
		rows = append(rows, row)
	}

	if err := s.importRepo.CreateJobWithRows(ctx, job, rows); err != nil {
		return nil, err
	}

	s.enqueue(job.ID)
	return job, nil
}

// enqueue submits the single per-job task.
func (s *bookingImportService) enqueue(jobID uuid.UUID) {
	s.workerPool.Submit(func() {
		// A fresh context: the request context is cancelled the moment the upload
		// response is written, which would kill every query this task makes.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()
		s.processJob(ctx, jobID)
	})
}

// processJob walks the job's pending rows sequentially.
func (s *bookingImportService) processJob(ctx context.Context, jobID uuid.UUID) {
	job, err := s.importRepo.GetJob(ctx, jobID)
	if err != nil || job == nil {
		log.Printf("booking import: cannot load job %s: %v", jobID, err)
		return
	}
	if err := s.importRepo.SetJobStatus(ctx, jobID, models.ImportJobProcessing, nil); err != nil {
		log.Printf("booking import: cannot mark job %s processing: %v", jobID, err)
		return
	}

	rows, err := s.importRepo.ListPendingRows(ctx, jobID)
	if err != nil {
		s.failJob(ctx, jobID, fmt.Sprintf("could not read import rows: %v", err))
		return
	}

	for _, row := range rows {
		if ctx.Err() != nil {
			// Shutting down. Rows stay pending and the startup sweep re-drives them.
			log.Printf("booking import: job %s interrupted, %d rows left pending", jobID, len(rows))
			return
		}
		s.processRow(ctx, job, row)
	}

	if err := s.importRepo.SetJobStatus(ctx, jobID, models.ImportJobCompleted, nil); err != nil {
		log.Printf("booking import: cannot complete job %s: %v", jobID, err)
	}
}

// processRow books one guest. Every failure here is a ROW failure, never a job
// failure: a full event, a guest already booked, an exhausted coupon — the host
// asked for exactly this report ("50 booked, 150 failed, here's why"). Row 13
// failing never stops rows 14+.
//
// The recover is what makes that promise hold unconditionally. The whole job is
// a single worker-pool task, and the pool's own recover (worker/pool.go) sits
// around the entire task — so an unexpected panic on one row would otherwise
// abandon every remaining row AND strand the job at 'processing' forever, since
// the completion write is never reached and the resume sweep only runs at
// startup. Containing it here turns a panic into one more failed row.
func (s *bookingImportService) processRow(ctx context.Context, job *models.BookingImportJob, row *models.BookingImportRow) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("booking import: panic on job %s row %d: %v", job.ID, row.RowNumber, r)
			s.markRow(ctx, row.ID, models.ImportRowFailed,
				"an unexpected error occurred booking this guest — please retry this row", nil)
		}
	}()

	rowCtx, cancel := context.WithTimeout(ctx, rowProcessTimeout)
	defer cancel()

	coupon := ""
	if job.CouponCode != nil {
		coupon = *job.CouponCode
	}
	occ := job.OccurrenceDate
	hostID := job.HostID

	resp, err := s.walkInService.InitiateWalkIn(rowCtx, WalkInInitiateRequest{
		GuestName:      row.GuestName,
		GuestPhone:     row.GuestPhone,
		EventID:        job.EventID,
		Quantity:       row.Quantity,
		OccurrenceDate: &occ,
		CouponCode:     coupon,
		HostID:         &hostID,
		// Offline mode: the host already took the fee, so the booking is written
		// at ₹0 with no ledger row and no host earnings. Tagged so these seats
		// stay identifiable (and reconcilable) forever.
		PaymentCollectedOffline: job.PaymentMode == models.ImportPaymentOffline,
		Source:                  models.BookingSourceBulkImport,
		ImportJobID:             &job.ID,
		// Deterministic per row: re-driving this job after a restart replays the
		// same key, and CreateBooking's idempotency check returns the existing
		// booking instead of creating a second one.
		IdempotencyKey: fmt.Sprintf("bulk_import_%s_%d", job.ID, row.RowNumber),
	})
	if err != nil {
		s.markRow(ctx, row.ID, models.ImportRowFailed, humanizeRowError(err), nil)
		return
	}
	// Defensive: every import mode resolves to the zero-cost path, so a paid
	// response means the event changed shape mid-import (e.g. tiers were added).
	// Fail the row rather than leave an unpaid Razorpay order stranded against
	// the guest's wallet.
	if resp.Paid || resp.Booking == nil {
		s.markRow(ctx, row.ID, models.ImportRowFailed,
			"this experience changed while importing — please retry this guest", nil)
		return
	}
	s.markRow(ctx, row.ID, models.ImportRowSuccess, "", &resp.Booking.ID)
}

func (s *bookingImportService) markRow(ctx context.Context, rowID uuid.UUID, status, errMsg string, bookingID *uuid.UUID) {
	var msg *string
	if errMsg != "" {
		msg = &errMsg
	}
	// Deliberately not rowCtx — the row's own timeout may already have fired, and
	// the result must still be recorded or the host's counters stall.
	writeCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.importRepo.MarkRowResult(writeCtx, rowID, status, msg, bookingID); err != nil {
		log.Printf("booking import: cannot record row %s result: %v", rowID, err)
	}
}

func (s *bookingImportService) failJob(ctx context.Context, jobID uuid.UUID, msg string) {
	if err := s.importRepo.SetJobStatus(ctx, jobID, models.ImportJobFailed, &msg); err != nil {
		log.Printf("booking import: cannot fail job %s: %v", jobID, err)
	}
}

// humanizeRowError turns the walk-in service's errors into something a host
// reading a failure report can act on.
func humanizeRowError(err error) string {
	switch {
	case errors.Is(err, ErrWalkInDuplicate):
		return "this guest already has a booking for this slot"
	case errors.Is(err, ErrWalkInNotOwner):
		return "you do not own this event"
	case errors.Is(err, ErrWalkInTiered):
		return "this event uses ticket tiers, which bulk import cannot price"
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out while booking — please retry this guest"
	}
	if msg := err.Error(); strings.Contains(msg, "capacity") {
		return "no seats left for this slot"
	}
	return err.Error()
}

func (s *bookingImportService) GetJob(ctx context.Context, hostID, jobID uuid.UUID) (*BookingImportStatus, error) {
	// Ownership is checked in the service, not the controller, so every caller
	// gets it.
	job, err := s.ownedJob(ctx, hostID, jobID)
	if err != nil {
		return nil, err
	}
	failed, err := s.importRepo.ListRows(ctx, jobID, models.ImportRowFailed)
	if err != nil {
		return nil, err
	}
	if failed == nil {
		failed = []*models.BookingImportRow{}
	}
	return &BookingImportStatus{Job: job, FailedRows: failed}, nil
}

func (s *bookingImportService) ListJobs(ctx context.Context, hostID uuid.UUID, limit int) ([]*models.BookingImportJob, error) {
	return s.importRepo.ListJobsByHost(ctx, hostID, limit)
}

// ownedJob loads a job and proves the caller owns it. Returns ErrImportJobNotFound
// for both "missing" and "someone else's" — a host must not be able to probe for
// the existence of another host's jobs.
func (s *bookingImportService) ownedJob(ctx context.Context, hostID, jobID uuid.UUID) (*models.BookingImportJob, error) {
	job, err := s.importRepo.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if job == nil || job.HostID != hostID {
		return nil, ErrImportJobNotFound
	}
	return job, nil
}

func (s *bookingImportService) ListRows(ctx context.Context, hostID, jobID uuid.UUID, status string) ([]*models.BookingImportRow, error) {
	if _, err := s.ownedJob(ctx, hostID, jobID); err != nil {
		return nil, err
	}
	rows, err := s.importRepo.ListRows(ctx, jobID, status)
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []*models.BookingImportRow{}
	}
	return rows, nil
}

// BuildReport renders the finished import as a downloadable .xlsx and returns it
// with a suggested filename.
func (s *bookingImportService) BuildReport(ctx context.Context, hostID, jobID uuid.UUID) (*excelize.File, string, error) {
	job, err := s.ownedJob(ctx, hostID, jobID)
	if err != nil {
		return nil, "", err
	}
	rows, err := s.importRepo.ListRows(ctx, jobID, "")
	if err != nil {
		return nil, "", err
	}

	// The event may have been renamed or deleted since the import; the report is
	// a historical record, so a missing event degrades to a blank title rather
	// than failing the download.
	title := ""
	if evt, err := s.eventRepo.GetByID(ctx, job.EventID); err == nil && evt != nil {
		title = evt.Title
	}

	reportRows := make([]bookingimport.ReportRow, 0, len(rows))
	for _, r := range rows {
		msg := ""
		if r.ErrorMessage != nil {
			msg = *r.ErrorMessage
		}
		reportRows = append(reportRows, bookingimport.ReportRow{
			RowNumber: r.RowNumber,
			Name:      r.GuestName,
			Phone:     r.GuestPhone,
			Quantity:  r.Quantity,
			Status:    r.Status,
			Error:     msg,
		})
	}

	f, err := bookingimport.BuildReport(bookingimport.ReportJob{
		FileName:       job.FileName,
		EventTitle:     title,
		OccurrenceIST:  timeutil.FormatIST(job.OccurrenceDate, "02 Jan 2006, 3:04 PM") + " IST",
		Status:         job.Status,
		TotalRows:      job.TotalRows,
		SuccessRows:    job.SuccessRows,
		FailedRows:     job.FailedRows,
		CreatedAt:      job.CreatedAt,
		CompletedAt:    job.CompletedAt,
		PaymentMode:    job.PaymentMode,
		UnitPriceCents: job.UnitPriceCents,
	}, reportRows)
	if err != nil {
		return nil, "", err
	}
	return f, fmt.Sprintf("booking-import-report-%s.xlsx", job.ID.String()[:8]), nil
}

// ResumeInterrupted re-drives jobs that a process restart abandoned. Safe because
// row processing is keyed per row: already-booked rows are no longer pending, and
// the deterministic idempotency key covers a row that was killed mid-booking.
func (s *bookingImportService) ResumeInterrupted(ctx context.Context) {
	ids, err := s.importRepo.ClaimStaleJobs(ctx)
	if err != nil {
		log.Printf("booking import: resume sweep failed: %v", err)
		return
	}
	for _, id := range ids {
		log.Printf("booking import: resuming interrupted job %s", id)
		s.enqueue(id)
	}
}
