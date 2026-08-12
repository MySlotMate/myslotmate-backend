package repository

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"myslotmate-backend/internal/models"

	"github.com/google/uuid"
)

// BookingImportRepository stores bulk booking-import jobs and their rows.
type BookingImportRepository interface {
	// CreateJobWithRows inserts the job and all of its rows in one transaction, so
	// a job is never visible to the status endpoint with a partial row set.
	CreateJobWithRows(ctx context.Context, job *models.BookingImportJob, rows []*models.BookingImportRow) error
	GetJob(ctx context.Context, id uuid.UUID) (*models.BookingImportJob, error)
	ListJobsByHost(ctx context.Context, hostID uuid.UUID, limit int) ([]*models.BookingImportJob, error)
	// ListPendingRows returns the job's not-yet-processed rows in sheet order.
	// Driving the worker off this (rather than the in-memory slice) is what makes
	// a job safe to re-run after a restart: already-processed rows are skipped.
	ListPendingRows(ctx context.Context, jobID uuid.UUID) ([]*models.BookingImportRow, error)
	ListRows(ctx context.Context, jobID uuid.UUID, status string) ([]*models.BookingImportRow, error)
	// MarkRowResult records one row's outcome and bumps the job counters in the
	// same transaction, so processed/success/failed can never disagree with the
	// rows table.
	MarkRowResult(ctx context.Context, rowID uuid.UUID, status string, errMsg *string, bookingID *uuid.UUID) error
	SetJobStatus(ctx context.Context, jobID uuid.UUID, status string, errMsg *string) error
	// ClaimStaleJobs flips interrupted jobs back to pending and returns their IDs,
	// for the startup resume sweep.
	ClaimStaleJobs(ctx context.Context) ([]uuid.UUID, error)
}

type postgresBookingImportRepository struct {
	db *sql.DB
}

func NewBookingImportRepository(db *sql.DB) BookingImportRepository {
	return &postgresBookingImportRepository{db: db}
}

const bookingImportJobColumns = `id, host_id, event_id, occurrence_date, coupon_code, file_name,
	status, error_message, total_rows, processed_rows, success_rows, failed_rows,
	created_at, updated_at, completed_at, payment_mode, unit_price_cents, offline_ack`

const bookingImportRowColumns = `id, job_id, row_number, guest_name, guest_phone, quantity,
	status, error_message, booking_id, created_at, updated_at`

func (r *postgresBookingImportRepository) CreateJobWithRows(ctx context.Context, job *models.BookingImportJob, rows []*models.BookingImportRow) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	err = tx.QueryRowContext(ctx, `
		INSERT INTO booking_import_jobs (host_id, event_id, occurrence_date, coupon_code, file_name, status, total_rows, payment_mode, unit_price_cents, offline_ack)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id, created_at, updated_at`,
		job.HostID, job.EventID, job.OccurrenceDate, job.CouponCode, job.FileName, job.Status, job.TotalRows,
		job.PaymentMode, job.UnitPriceCents, job.OfflineAck,
	).Scan(&job.ID, &job.CreatedAt, &job.UpdatedAt)
	if err != nil {
		return fmt.Errorf("failed to create import job: %w", err)
	}

	if len(rows) > 0 {
		// One multi-VALUES insert rather than N round trips — at 1000 rows against
		// a remote Supabase instance the difference is seconds of upload latency
		// the host spends staring at a spinner.
		var sb strings.Builder
		sb.WriteString(`INSERT INTO booking_import_rows (job_id, row_number, guest_name, guest_phone, quantity, status, error_message) VALUES `)
		args := make([]interface{}, 0, len(rows)*7)
		for i, row := range rows {
			if i > 0 {
				sb.WriteString(", ")
			}
			n := i * 7
			fmt.Fprintf(&sb, "($%d, $%d, $%d, $%d, $%d, $%d, $%d)", n+1, n+2, n+3, n+4, n+5, n+6, n+7)
			args = append(args, job.ID, row.RowNumber, row.GuestName, row.GuestPhone, row.Quantity, row.Status, row.ErrorMessage)
		}
		if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
			return fmt.Errorf("failed to create import rows: %w", err)
		}
	}

	// Rows that arrived already-failed (bad phone, missing name) are counted here
	// so the job's totals are correct from the moment it is created, before the
	// worker has touched anything.
	preFailed := 0
	for _, row := range rows {
		if row.Status == models.ImportRowFailed {
			preFailed++
		}
	}
	if preFailed > 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE booking_import_jobs
			SET processed_rows = $2, failed_rows = $2, updated_at = now()
			WHERE id = $1`, job.ID, preFailed); err != nil {
			return fmt.Errorf("failed to seed import counters: %w", err)
		}
		job.ProcessedRows = preFailed
		job.FailedRows = preFailed
	}

	return tx.Commit()
}

func scanImportJob(s interface{ Scan(...interface{}) error }) (*models.BookingImportJob, error) {
	var j models.BookingImportJob
	err := s.Scan(&j.ID, &j.HostID, &j.EventID, &j.OccurrenceDate, &j.CouponCode, &j.FileName,
		&j.Status, &j.ErrorMessage, &j.TotalRows, &j.ProcessedRows, &j.SuccessRows, &j.FailedRows,
		&j.CreatedAt, &j.UpdatedAt, &j.CompletedAt, &j.PaymentMode, &j.UnitPriceCents, &j.OfflineAck)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &j, nil
}

func (r *postgresBookingImportRepository) GetJob(ctx context.Context, id uuid.UUID) (*models.BookingImportJob, error) {
	return scanImportJob(r.db.QueryRowContext(ctx,
		`SELECT `+bookingImportJobColumns+` FROM booking_import_jobs WHERE id = $1`, id))
}

func (r *postgresBookingImportRepository) ListJobsByHost(ctx context.Context, hostID uuid.UUID, limit int) ([]*models.BookingImportJob, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+bookingImportJobColumns+` FROM booking_import_jobs
		 WHERE host_id = $1 ORDER BY created_at DESC LIMIT $2`, hostID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*models.BookingImportJob
	for rows.Next() {
		j, err := scanImportJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (r *postgresBookingImportRepository) queryRows(ctx context.Context, query string, args ...interface{}) ([]*models.BookingImportRow, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*models.BookingImportRow
	for rows.Next() {
		var m models.BookingImportRow
		if err := rows.Scan(&m.ID, &m.JobID, &m.RowNumber, &m.GuestName, &m.GuestPhone, &m.Quantity,
			&m.Status, &m.ErrorMessage, &m.BookingID, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}

func (r *postgresBookingImportRepository) ListPendingRows(ctx context.Context, jobID uuid.UUID) ([]*models.BookingImportRow, error) {
	return r.queryRows(ctx, `SELECT `+bookingImportRowColumns+` FROM booking_import_rows
		WHERE job_id = $1 AND status = $2 ORDER BY row_number ASC`, jobID, models.ImportRowPending)
}

func (r *postgresBookingImportRepository) ListRows(ctx context.Context, jobID uuid.UUID, status string) ([]*models.BookingImportRow, error) {
	if status == "" {
		return r.queryRows(ctx, `SELECT `+bookingImportRowColumns+` FROM booking_import_rows
			WHERE job_id = $1 ORDER BY row_number ASC`, jobID)
	}
	return r.queryRows(ctx, `SELECT `+bookingImportRowColumns+` FROM booking_import_rows
		WHERE job_id = $1 AND status = $2 ORDER BY row_number ASC`, jobID, status)
}

func (r *postgresBookingImportRepository) MarkRowResult(ctx context.Context, rowID uuid.UUID, status string, errMsg *string, bookingID *uuid.UUID) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// The status guard makes this a no-op if the row was already resolved, so a
	// job re-driven after a restart can never double-count its counters.
	var jobID uuid.UUID
	err = tx.QueryRowContext(ctx, `
		UPDATE booking_import_rows
		SET status = $2, error_message = $3, booking_id = $4, updated_at = now()
		WHERE id = $1 AND status = $5
		RETURNING job_id`, rowID, status, errMsg, bookingID, models.ImportRowPending).Scan(&jobID)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}

	success, failed := 0, 0
	if status == models.ImportRowSuccess {
		success = 1
	} else {
		failed = 1
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE booking_import_jobs
		SET processed_rows = processed_rows + 1,
		    success_rows   = success_rows + $2,
		    failed_rows    = failed_rows + $3,
		    updated_at     = now()
		WHERE id = $1`, jobID, success, failed); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *postgresBookingImportRepository) SetJobStatus(ctx context.Context, jobID uuid.UUID, status string, errMsg *string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE booking_import_jobs
		SET status = $2,
		    error_message = $3,
		    completed_at = CASE WHEN $2 IN ('completed', 'failed') THEN now() ELSE completed_at END,
		    updated_at = now()
		WHERE id = $1`, jobID, status, errMsg)
	return err
}

// staleJobAge is how long a job must have sat untouched before the resume sweep
// treats it as abandoned. Every processed row bumps the job's updated_at, so a
// live job is never this old.
const staleJobAge = "5 minutes"

// ClaimStaleJobs resets jobs left mid-flight by a process restart. The worker
// pool is in-memory and PM2/Render restarts kill in-flight jobs, which would
// otherwise strand forever at 'processing' with rows still pending.
//
// The age filter matters: StartImport creates jobs as 'pending' and enqueues
// them, so without it a sweep would also "resume" a job accepted seconds ago
// that is merely waiting its turn in the pool queue — handing it a second
// concurrent worker walking the same rows. MarkRowResult's status guard and the
// per-row idempotency key would most likely absorb that, but this avoids the
// race rather than relying on them to.
func (r *postgresBookingImportRepository) ClaimStaleJobs(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := r.db.QueryContext(ctx, `
		UPDATE booking_import_jobs
		SET status = $1, updated_at = now()
		WHERE status IN ($1, $2)
		  AND updated_at < now() - INTERVAL '`+staleJobAge+`'
		RETURNING id`, models.ImportJobPending, models.ImportJobProcessing)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
