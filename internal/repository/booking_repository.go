package repository

import (
	"context"
	"database/sql"
	"myslotmate-backend/internal/models"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

type BookingRepository interface {
	Create(ctx context.Context, booking *models.Booking) error
	GetByID(ctx context.Context, id uuid.UUID) (*models.Booking, error)
	ListByUserID(ctx context.Context, userID uuid.UUID) ([]*models.Booking, error)
	ListByEventID(ctx context.Context, eventID uuid.UUID) ([]*models.Booking, error)
	ListByEventOccurrence(ctx context.Context, eventID uuid.UUID, occurrenceDate time.Time) ([]*models.Booking, error)
	GetTotalBookedQuantity(ctx context.Context, eventID uuid.UUID) (int, error)
	GetTotalBookedQuantityForOccurrence(ctx context.Context, eventID uuid.UUID, occurrenceDate time.Time) (int, error)
	UpdateStatus(ctx context.Context, id uuid.UUID, status models.BookingStatus) error
	ListRecentCancelledByEventIDs(ctx context.Context, eventIDs []uuid.UUID, limit int) ([]*models.Booking, error)
	CountConfirmedByEventIDs(ctx context.Context, eventIDs []uuid.UUID) (int, error)
	MarkWhatsappNotificationSent(ctx context.Context, id uuid.UUID) error
	MarkEmailNotificationSent(ctx context.Context, id uuid.UUID) error
	MarkEmailReminderNotificationSent(ctx context.Context, id uuid.UUID) error
	MarkWhatsappReminderNotificationSent(ctx context.Context, id uuid.UUID) error
	ListPendingReminderNotifications(ctx context.Context, limit int) ([]*models.Booking, error)
	GetOccupancyForEvents(ctx context.Context, eventIDs []uuid.UUID, since time.Time) (map[uuid.UUID]map[string]int, error)
}

type postgresBookingRepository struct {
	db *sql.DB
}

func NewBookingRepository(db *sql.DB) BookingRepository {
	return &postgresBookingRepository{db: db}
}

// bookingColumns is the canonical column list for SELECT queries.
const bookingColumns = `id, event_id, user_id, occurrence_date, quantity, status, payment_id, idempotency_key, amount_cents, service_fee_cents, net_earning_cents, created_at, updated_at, cancelled_at, notification_sent_whatsapp, notification_sent_email, reminder_notification_sent_email, reminder_notification_sent_at, reminder_notification_sent_whatsapp, reminder_whatsapp_sent_at`

// scanBooking scans a single row into a Booking struct.
func scanBooking(scanner interface{ Scan(dest ...interface{}) error }) (*models.Booking, error) {
	b := &models.Booking{}
	err := scanner.Scan(
		&b.ID, &b.EventID, &b.UserID, &b.OccurrenceDate, &b.Quantity, &b.Status, &b.PaymentID, &b.IdempotencyKey, &b.AmountCents, &b.ServiceFeeCents, &b.NetEarningCents, &b.CreatedAt, &b.UpdatedAt, &b.CancelledAt, &b.NotificationSentWhatsapp, &b.NotificationSentEmail, &b.ReminderNotificationSentEmail, &b.ReminderNotificationSentAt, &b.ReminderNotificationSentWhatsapp, &b.ReminderWhatsappSentAt,
	)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// scanBookingRows scans multiple rows into a slice of Booking pointers.
func scanBookingRows(rows *sql.Rows) ([]*models.Booking, error) {
	var bookings []*models.Booking
	for rows.Next() {
		b, err := scanBooking(rows)
		if err != nil {
			return nil, err
		}
		bookings = append(bookings, b)
	}
	return bookings, rows.Err()
}

func (r *postgresBookingRepository) Create(ctx context.Context, booking *models.Booking) error {
	query := `
		INSERT INTO bookings (id, event_id, user_id, occurrence_date, quantity, status, payment_id, idempotency_key, amount_cents, service_fee_cents, net_earning_cents, notification_sent_whatsapp, notification_sent_email, reminder_notification_sent_email, reminder_notification_sent_whatsapp, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
	`
	if booking.ID == uuid.Nil {
		booking.ID = uuid.New()
	}
	_, err := r.db.ExecContext(ctx, query,
		booking.ID, booking.EventID, booking.UserID, booking.OccurrenceDate, booking.Quantity, booking.Status, booking.PaymentID, booking.IdempotencyKey, booking.AmountCents, booking.ServiceFeeCents, booking.NetEarningCents, booking.NotificationSentWhatsapp, booking.NotificationSentEmail, booking.ReminderNotificationSentEmail, booking.ReminderNotificationSentWhatsapp, booking.CreatedAt, booking.UpdatedAt,
	)
	return err
}

func (r *postgresBookingRepository) GetByID(ctx context.Context, id uuid.UUID) (*models.Booking, error) {
	query := `SELECT ` + bookingColumns + ` FROM bookings WHERE id = $1`
	b, err := scanBooking(r.db.QueryRowContext(ctx, query, id))
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return b, nil
}

func (r *postgresBookingRepository) ListByUserID(ctx context.Context, userID uuid.UUID) ([]*models.Booking, error) {
	query := `SELECT ` + bookingColumns + ` FROM bookings WHERE user_id = $1`
	rows, err := r.db.QueryContext(ctx, query, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBookingRows(rows)
}

func (r *postgresBookingRepository) ListByEventID(ctx context.Context, eventID uuid.UUID) ([]*models.Booking, error) {
	query := `SELECT ` + bookingColumns + `
		FROM bookings WHERE event_id = $1 AND status IN ('pending', 'confirmed') ORDER BY created_at ASC`
	rows, err := r.db.QueryContext(ctx, query, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBookingRows(rows)
}

func (r *postgresBookingRepository) ListByEventOccurrence(ctx context.Context, eventID uuid.UUID, occurrenceDate time.Time) ([]*models.Booking, error) {
	query := `SELECT ` + bookingColumns + `
		FROM bookings WHERE event_id = $1 AND occurrence_date = $2 AND status IN ('pending', 'confirmed') ORDER BY created_at ASC`
	rows, err := r.db.QueryContext(ctx, query, eventID, occurrenceDate)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBookingRows(rows)
}

// GetTotalBookedQuantity returns the total booked quantity for an event across ALL occurrences.
// Kept for backward compatibility with non-recurring events and aggregate views.
func (r *postgresBookingRepository) GetTotalBookedQuantity(ctx context.Context, eventID uuid.UUID) (int, error) {
	query := `SELECT COALESCE(SUM(quantity), 0) FROM bookings WHERE event_id = $1 AND status IN ('pending', 'confirmed')`
	var total int
	err := r.db.QueryRowContext(ctx, query, eventID).Scan(&total)
	return total, err
}

// GetTotalBookedQuantityForOccurrence returns the total booked quantity for a specific
// date/occurrence of an event. This is the key fix for recurring event capacity tracking.
func (r *postgresBookingRepository) GetTotalBookedQuantityForOccurrence(ctx context.Context, eventID uuid.UUID, occurrenceDate time.Time) (int, error) {
	query := `SELECT COALESCE(SUM(quantity), 0) FROM bookings WHERE event_id = $1 AND occurrence_date = $2 AND status IN ('pending', 'confirmed')`
	var total int
	err := r.db.QueryRowContext(ctx, query, eventID, occurrenceDate).Scan(&total)
	return total, err
}

func (r *postgresBookingRepository) UpdateStatus(ctx context.Context, id uuid.UUID, status models.BookingStatus) error {
	query := `UPDATE bookings SET status = $1 WHERE id = $2`
	if status == models.BookingStatusCancelled {
		query = `UPDATE bookings SET status = $1, cancelled_at = NOW() WHERE id = $2`
	}
	_, err := r.db.ExecContext(ctx, query, status, id)
	return err
}

func (r *postgresBookingRepository) ListRecentCancelledByEventIDs(ctx context.Context, eventIDs []uuid.UUID, limit int) ([]*models.Booking, error) {
	if len(eventIDs) == 0 {
		return nil, nil
	}
	query := `SELECT ` + bookingColumns + `
		FROM bookings WHERE event_id = ANY($1) AND status = 'cancelled' ORDER BY cancelled_at DESC LIMIT $2`
	rows, err := r.db.QueryContext(ctx, query, pq.Array(eventIDs), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBookingRows(rows)
}

func (r *postgresBookingRepository) CountConfirmedByEventIDs(ctx context.Context, eventIDs []uuid.UUID) (int, error) {
	if len(eventIDs) == 0 {
		return 0, nil
	}
	var count int
	query := `SELECT COUNT(*) FROM bookings WHERE event_id = ANY($1) AND status = 'confirmed'`
	err := r.db.QueryRowContext(ctx, query, pq.Array(eventIDs)).Scan(&count)
	return count, err
}

func (r *postgresBookingRepository) MarkWhatsappNotificationSent(ctx context.Context, id uuid.UUID) error {
	query := `UPDATE bookings SET notification_sent_whatsapp = true WHERE id = $1`
	_, err := r.db.ExecContext(ctx, query, id)
	return err
}

func (r *postgresBookingRepository) MarkEmailReminderNotificationSent(ctx context.Context, id uuid.UUID) error {
	query := `UPDATE bookings SET reminder_notification_sent_email = true, reminder_notification_sent_at = NOW() WHERE id = $1`
	_, err := r.db.ExecContext(ctx, query, id)
	return err
}

func (r *postgresBookingRepository) ListPendingReminderNotifications(ctx context.Context, limit int) ([]*models.Booking, error) {
	query := `SELECT ` + bookingColumns + `
		FROM bookings 
		WHERE status IN ('pending', 'confirmed') AND reminder_notification_sent_email = false
		LIMIT $1`
	rows, err := r.db.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBookingRows(rows)
}

func (r *postgresBookingRepository) MarkEmailNotificationSent(ctx context.Context, id uuid.UUID) error {
	query := `UPDATE bookings SET notification_sent_email = true WHERE id = $1`
	_, err := r.db.ExecContext(ctx, query, id)
	return err
}

func (r *postgresBookingRepository) MarkWhatsappReminderNotificationSent(ctx context.Context, id uuid.UUID) error {
	query := `UPDATE bookings SET reminder_notification_sent_whatsapp = true, reminder_whatsapp_sent_at = NOW() WHERE id = $1`
	_, err := r.db.ExecContext(ctx, query, id)
	return err
}
func (r *postgresBookingRepository) GetOccupancyForEvents(ctx context.Context, eventIDs []uuid.UUID, since time.Time) (map[uuid.UUID]map[string]int, error) {
	if len(eventIDs) == 0 {
		return make(map[uuid.UUID]map[string]int), nil
	}

	query := `
		SELECT event_id, occurrence_date, SUM(quantity)
		FROM bookings
		WHERE event_id = ANY($1) AND occurrence_date >= $2 AND status IN ('pending', 'confirmed')
		GROUP BY event_id, occurrence_date
	`

	rows, err := r.db.QueryContext(ctx, query, pq.Array(eventIDs), since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[uuid.UUID]map[string]int)
	for rows.Next() {
		var eid uuid.UUID
		var date time.Time
		var qty int
		if err := rows.Scan(&eid, &date, &qty); err != nil {
			return nil, err
		}

		if _, ok := result[eid]; !ok {
			result[eid] = make(map[string]int)
		}
		// Format date to ISO string for consistent map lookup
		result[eid][date.Format(time.RFC3339)] = qty
	}
	return result, rows.Err()
}
