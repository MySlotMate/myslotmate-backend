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
	ListAttendeesByEventID(ctx context.Context, eventID uuid.UUID) ([]*models.Attendee, error)
	ListAttendeesByEventOccurrence(ctx context.Context, eventID uuid.UUID, occurrenceDate time.Time) ([]*models.Attendee, error)
	GetTotalBookedQuantity(ctx context.Context, eventID uuid.UUID) (int, error)
	GetTotalBookedQuantityForOccurrence(ctx context.Context, eventID uuid.UUID, occurrenceDate time.Time) (int, error)
	// HasActiveBookingForOccurrence reports whether the user already holds an
	// active (pending/confirmed) booking for this exact event occurrence. Used to
	// stop the same guest (matched by phone) double-booking the same slot.
	HasActiveBookingForOccurrence(ctx context.Context, userID, eventID uuid.UUID, occurrenceDate time.Time) (bool, error)
	GetBookedQuantitySince(ctx context.Context, eventID uuid.UUID, since time.Time) (int, error)
	UpdateStatus(ctx context.Context, id uuid.UUID, status models.BookingStatus) error
	UpdatePaymentID(ctx context.Context, id uuid.UUID, paymentID uuid.UUID) error
	ListRecentCancelledByEventIDs(ctx context.Context, eventIDs []uuid.UUID, limit int) ([]*models.Booking, error)
	CountConfirmedByEventIDs(ctx context.Context, eventIDs []uuid.UUID) (int, error)
	MarkWhatsappNotificationSent(ctx context.Context, id uuid.UUID) error
	MarkEmailNotificationSent(ctx context.Context, id uuid.UUID) error
	MarkEmailReminderNotificationSent(ctx context.Context, id uuid.UUID) error
	MarkWhatsappReminderNotificationSent(ctx context.Context, id uuid.UUID) error
	ListPendingReminderNotifications(ctx context.Context, limit int) ([]*models.Booking, error)
	GetOccupancyForEvents(ctx context.Context, eventIDs []uuid.UUID, since time.Time) (map[uuid.UUID]map[string]int, error)
	// GetHostEarningsBreakdown computes the host's earnings from the bookings
	// table — the authoritative source. Cancelled / refunded bookings are
	// naturally excluded (status != 'confirmed'), so the result is already
	// "lifetime net of refunds". Splits earnings by whether each booking's
	// event has happened yet (`occurrence_date < NOW()`) for the available-vs-
	// pending distinction.
	GetHostEarningsBreakdown(ctx context.Context, hostID uuid.UUID) (*HostEarningsBreakdown, error)
	// ListHostSales returns every booking made on any of this host's events,
	// joined with the buyer (users) and event (events) for display in the
	// earnings dashboard. Newest first. Includes cancelled / refunded bookings
	// so the host can see the full picture. When fromDate is non-nil, only
	// bookings created at or after that timestamp are returned.
	ListHostSales(ctx context.Context, hostID uuid.UUID, limit, offset int, fromDate *time.Time) ([]*HostSale, error)
	// GetScanTarget loads the one booking a scanned ticket refers to, joined with
	// the fields the door needs to judge it: the owning host, the event title and
	// the guest's name. Returns nil when no such booking exists.
	GetScanTarget(ctx context.Context, bookingID uuid.UUID) (*BookingScanTarget, error)
	// CheckInGuests admits `count` more guests against a booking. The whole
	// decision is expressed as one guarded UPDATE — host ownership, event and
	// occurrence match, confirmed status and remaining capacity are all re-checked
	// in the WHERE clause, so two doors scanning the same ticket at once can never
	// admit more guests than were paid for. Reports how many rows matched (0 or 1);
	// 0 means some condition failed and the caller should re-read to explain why.
	CheckInGuests(ctx context.Context, p CheckInParams) (int, error)
	// WithTx returns a copy of the repository bound to the given transaction.
	WithTx(tx *sql.Tx) BookingRepository
}

// BookingScanTarget is a booking plus the context needed to validate a ticket
// at the door — who owns the event, what it is, and who the booking is for.
type BookingScanTarget struct {
	models.Booking
	HostID     uuid.UUID
	EventTitle string
	GuestName  string
	GuestEmail string
}

// CheckInParams are the conditions a check-in must satisfy. EventID and
// OccurrenceDate come from the session the host opened (the event they are
// manning), not from the scanned ticket — that is what stops a ticket for one
// event being admitted at another.
type CheckInParams struct {
	BookingID      uuid.UUID
	HostID         uuid.UUID
	EventID        uuid.UUID
	OccurrenceDate time.Time
	Count          int
}

// HostEarningsBreakdown is the live, booking-derived view of a host's earnings,
// used by the earnings dashboard and the withdrawal-availability gate.
//
//	TotalCents = EventPassedCents + EventUpcomingCents (always).
type HostEarningsBreakdown struct {
	TotalCents         int64 // lifetime net (refunds deducted)
	EventPassedCents   int64 // confirmed bookings whose event has happened — eligible to withdraw
	EventUpcomingCents int64 // confirmed bookings whose event is upcoming — locked
}

// HostSale is one booking row joined with its buyer + event, used by the
// host's "Sales" panel — answers "who bought what for how much."
type HostSale struct {
	BookingID       uuid.UUID
	EventID         uuid.UUID
	EventTitle      string
	BuyerUserID     uuid.UUID
	BuyerName       string
	BuyerEmail      string
	BuyerAvatarURL  *string
	OccurrenceDate  time.Time
	Quantity        int
	AmountCents     int64
	NetEarningCents *int64 // host's share — nil for free bookings
	ServiceFeeCents *int64
	Status          models.BookingStatus
	CreatedAt       time.Time
	CancelledAt     *time.Time
}

type postgresBookingRepository struct {
	db DBTX
}

func NewBookingRepository(db *sql.DB) BookingRepository {
	return &postgresBookingRepository{db: db}
}

func (r *postgresBookingRepository) WithTx(tx *sql.Tx) BookingRepository {
	return &postgresBookingRepository{db: tx}
}

func (r *postgresBookingRepository) ListHostSales(ctx context.Context, hostID uuid.UUID, limit, offset int, fromDate *time.Time) ([]*HostSale, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	var fromArg sql.NullTime
	if fromDate != nil {
		fromArg = sql.NullTime{Time: *fromDate, Valid: true}
	}
	const query = `
		SELECT
			b.id,           b.event_id,     e.title,
			b.user_id,      u.name,         u.email,         u.avatar_url,
			b.occurrence_date, b.quantity,
			b.amount_cents, b.net_earning_cents, b.service_fee_cents,
			b.status,       b.created_at,   b.cancelled_at
		FROM bookings b
		JOIN events e ON e.id = b.event_id
		JOIN users  u ON u.id = b.user_id
		WHERE e.host_id = $1
		  AND ($4::timestamptz IS NULL OR b.created_at >= $4)
		ORDER BY b.created_at DESC
		LIMIT $2 OFFSET $3`
	rows, err := r.db.QueryContext(ctx, query, hostID, limit, offset, fromArg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sales []*HostSale
	for rows.Next() {
		s := &HostSale{}
		var amount sql.NullInt64
		if err := rows.Scan(
			&s.BookingID, &s.EventID, &s.EventTitle,
			&s.BuyerUserID, &s.BuyerName, &s.BuyerEmail, &s.BuyerAvatarURL,
			&s.OccurrenceDate, &s.Quantity,
			&amount, &s.NetEarningCents, &s.ServiceFeeCents,
			&s.Status, &s.CreatedAt, &s.CancelledAt,
		); err != nil {
			return nil, err
		}
		if amount.Valid {
			s.AmountCents = amount.Int64
		}
		sales = append(sales, s)
	}
	return sales, rows.Err()
}

func (r *postgresBookingRepository) GetHostEarningsBreakdown(ctx context.Context, hostID uuid.UUID) (*HostEarningsBreakdown, error) {
	const query = `
		SELECT
			COALESCE(SUM(b.net_earning_cents), 0)::BIGINT AS total,
			COALESCE(SUM(b.net_earning_cents) FILTER (WHERE b.occurrence_date <  NOW()), 0)::BIGINT AS event_passed,
			COALESCE(SUM(b.net_earning_cents) FILTER (WHERE b.occurrence_date >= NOW()), 0)::BIGINT AS event_upcoming
		FROM bookings b
		JOIN events   e ON e.id = b.event_id
		WHERE e.host_id = $1
		  AND b.status  = 'confirmed'`
	var b HostEarningsBreakdown
	if err := r.db.QueryRowContext(ctx, query, hostID).Scan(&b.TotalCents, &b.EventPassedCents, &b.EventUpcomingCents); err != nil {
		return nil, err
	}
	return &b, nil
}

func (r *postgresBookingRepository) GetScanTarget(ctx context.Context, bookingID uuid.UUID) (*BookingScanTarget, error) {
	const query = `
		SELECT
			b.id, b.event_id, b.user_id, b.occurrence_date, b.quantity, b.status,
			b.checked_in_count, b.last_checked_in_at,
			e.host_id, e.title, u.name, u.email
		FROM bookings b
		JOIN events e ON e.id = b.event_id
		JOIN users  u ON u.id = b.user_id
		WHERE b.id = $1`
	t := &BookingScanTarget{}
	err := r.db.QueryRowContext(ctx, query, bookingID).Scan(
		&t.ID, &t.EventID, &t.UserID, &t.OccurrenceDate, &t.Quantity, &t.Status,
		&t.CheckedInCount, &t.LastCheckedInAt,
		&t.HostID, &t.EventTitle, &t.GuestName, &t.GuestEmail,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return t, nil
}

func (r *postgresBookingRepository) CheckInGuests(ctx context.Context, p CheckInParams) (int, error) {
	// Every condition the verify step checks is repeated here so the write is
	// safe on its own, independent of what verify saw a moment earlier.
	const query = `
		UPDATE bookings b
		SET checked_in_count   = b.checked_in_count + $5,
		    last_checked_in_at = now(),
		    updated_at         = now()
		FROM events e
		WHERE b.id              = $1
		  AND e.id              = b.event_id
		  AND e.host_id         = $2
		  AND b.event_id        = $3
		  AND b.occurrence_date = $4
		  AND b.status          = 'confirmed'
		  AND b.checked_in_count + $5 <= b.quantity`
	res, err := r.db.ExecContext(ctx, query, p.BookingID, p.HostID, p.EventID, p.OccurrenceDate, p.Count)
	if err != nil {
		return 0, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(affected), nil
}

// bookingColumns is the canonical column list for SELECT queries.
const bookingColumns = `id, event_id, user_id, occurrence_date, quantity, status, payment_id, idempotency_key, amount_cents, service_fee_cents, net_earning_cents, created_at, updated_at, cancelled_at, notification_sent_whatsapp, notification_sent_email, reminder_notification_sent_email, reminder_notification_sent_at, reminder_notification_sent_whatsapp, reminder_whatsapp_sent_at, price_tier_id, unit_price_cents, checked_in_count, last_checked_in_at`

// scanBooking scans a single row into a Booking struct.
func scanBooking(scanner interface{ Scan(dest ...interface{}) error }) (*models.Booking, error) {
	b := &models.Booking{}
	err := scanner.Scan(
		&b.ID, &b.EventID, &b.UserID, &b.OccurrenceDate, &b.Quantity, &b.Status, &b.PaymentID, &b.IdempotencyKey, &b.AmountCents, &b.ServiceFeeCents, &b.NetEarningCents, &b.CreatedAt, &b.UpdatedAt, &b.CancelledAt, &b.NotificationSentWhatsapp, &b.NotificationSentEmail, &b.ReminderNotificationSentEmail, &b.ReminderNotificationSentAt, &b.ReminderNotificationSentWhatsapp, &b.ReminderWhatsappSentAt, &b.PriceTierID, &b.UnitPriceCents, &b.CheckedInCount, &b.LastCheckedInAt,
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
		INSERT INTO bookings (id, event_id, user_id, occurrence_date, quantity, status, payment_id, idempotency_key, amount_cents, service_fee_cents, net_earning_cents, notification_sent_whatsapp, notification_sent_email, reminder_notification_sent_email, reminder_notification_sent_whatsapp, created_at, updated_at, price_tier_id, unit_price_cents)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
	`
	if booking.ID == uuid.Nil {
		booking.ID = uuid.New()
	}
	_, err := r.db.ExecContext(ctx, query,
		booking.ID, booking.EventID, booking.UserID, booking.OccurrenceDate, booking.Quantity, booking.Status, booking.PaymentID, booking.IdempotencyKey, booking.AmountCents, booking.ServiceFeeCents, booking.NetEarningCents, booking.NotificationSentWhatsapp, booking.NotificationSentEmail, booking.ReminderNotificationSentEmail, booking.ReminderNotificationSentWhatsapp, booking.CreatedAt, booking.UpdatedAt, booking.PriceTierID, booking.UnitPriceCents,
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

// attendeeColumns extends bookingColumns with the joined user fields.
const attendeeColumns = `b.id, b.event_id, b.user_id, b.occurrence_date, b.quantity, b.status, b.payment_id, b.idempotency_key, b.amount_cents, b.service_fee_cents, b.net_earning_cents, b.created_at, b.updated_at, b.cancelled_at, b.notification_sent_whatsapp, b.notification_sent_email, b.reminder_notification_sent_email, b.reminder_notification_sent_at, b.reminder_notification_sent_whatsapp, b.reminder_whatsapp_sent_at, b.price_tier_id, b.unit_price_cents, u.name AS user_name, u.email AS user_email, u.avatar_url AS user_avatar_url`

func scanAttendeeRows(rows *sql.Rows) ([]*models.Attendee, error) {
	var attendees []*models.Attendee
	for rows.Next() {
		a := &models.Attendee{}
		if err := rows.Scan(
			&a.ID, &a.EventID, &a.UserID, &a.OccurrenceDate, &a.Quantity, &a.Status, &a.PaymentID, &a.IdempotencyKey, &a.AmountCents, &a.ServiceFeeCents, &a.NetEarningCents, &a.CreatedAt, &a.UpdatedAt, &a.CancelledAt, &a.NotificationSentWhatsapp, &a.NotificationSentEmail, &a.ReminderNotificationSentEmail, &a.ReminderNotificationSentAt, &a.ReminderNotificationSentWhatsapp, &a.ReminderWhatsappSentAt, &a.PriceTierID, &a.UnitPriceCents,
			&a.UserName, &a.UserEmail, &a.UserAvatarURL,
		); err != nil {
			return nil, err
		}
		attendees = append(attendees, a)
	}
	return attendees, rows.Err()
}

func (r *postgresBookingRepository) ListAttendeesByEventID(ctx context.Context, eventID uuid.UUID) ([]*models.Attendee, error) {
	query := `SELECT ` + attendeeColumns + `
		FROM bookings b JOIN users u ON u.id = b.user_id
		WHERE b.event_id = $1 AND b.status IN ('pending', 'confirmed') ORDER BY b.created_at ASC`
	rows, err := r.db.QueryContext(ctx, query, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAttendeeRows(rows)
}

func (r *postgresBookingRepository) ListAttendeesByEventOccurrence(ctx context.Context, eventID uuid.UUID, occurrenceDate time.Time) ([]*models.Attendee, error) {
	query := `SELECT ` + attendeeColumns + `
		FROM bookings b JOIN users u ON u.id = b.user_id
		WHERE b.event_id = $1 AND b.occurrence_date = $2 AND b.status IN ('pending', 'confirmed') ORDER BY b.created_at ASC`
	rows, err := r.db.QueryContext(ctx, query, eventID, occurrenceDate)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAttendeeRows(rows)
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

// HasActiveBookingForOccurrence reports whether userID already has a
// pending/confirmed booking for the exact (eventID, occurrenceDate) slot.
func (r *postgresBookingRepository) HasActiveBookingForOccurrence(ctx context.Context, userID, eventID uuid.UUID, occurrenceDate time.Time) (bool, error) {
	const query = `SELECT EXISTS(
		SELECT 1 FROM bookings
		WHERE user_id = $1 AND event_id = $2 AND occurrence_date = $3
		  AND status IN ('pending', 'confirmed')
	)`
	var exists bool
	err := r.db.QueryRowContext(ctx, query, userID, eventID, occurrenceDate).Scan(&exists)
	return exists, err
}

// GetBookedQuantitySince returns the total booked quantity (people count, summing
// quantity per booking) for an event since the given timestamp. Used to surface
// recent booking activity like "N people booked this week".
func (r *postgresBookingRepository) GetBookedQuantitySince(ctx context.Context, eventID uuid.UUID, since time.Time) (int, error) {
	query := `SELECT COALESCE(SUM(quantity), 0) FROM bookings WHERE event_id = $1 AND created_at >= $2 AND status IN ('pending', 'confirmed')`
	var total int
	err := r.db.QueryRowContext(ctx, query, eventID, since).Scan(&total)
	return total, err
}

func (r *postgresBookingRepository) UpdateStatus(ctx context.Context, id uuid.UUID, status models.BookingStatus) error {
	query := `UPDATE bookings SET status = $1 WHERE id = $2`
	if status == models.BookingStatusCancelled || status == models.BookingStatusRefunded {
		query = `UPDATE bookings SET status = $1, cancelled_at = NOW() WHERE id = $2`
	}
	_, err := r.db.ExecContext(ctx, query, status, id)
	return err
}

// UpdatePaymentID links a booking row to its payment record (bookings.payment_id).
func (r *postgresBookingRepository) UpdatePaymentID(ctx context.Context, id uuid.UUID, paymentID uuid.UUID) error {
	query := `UPDATE bookings SET payment_id = $1, updated_at = NOW() WHERE id = $2`
	_, err := r.db.ExecContext(ctx, query, paymentID, id)
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
	// Select bookings where AT LEAST ONE channel reminder is still unsent.
	// Gating on email alone caused WhatsApp to be re-sent on every tick for
	// users without an email (the email send fails before the email flag is set).
	query := `SELECT ` + bookingColumns + `
		FROM bookings
		WHERE status IN ('pending', 'confirmed')
		  AND (reminder_notification_sent_email = false OR reminder_notification_sent_whatsapp = false)
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
