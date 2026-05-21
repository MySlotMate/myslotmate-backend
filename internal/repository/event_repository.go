package repository

import (
	"context"
	"database/sql"
	"fmt"
	"myslotmate-backend/internal/lib/recurrence"
	"myslotmate-backend/internal/models"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// applyRecurrence rolls a past recurring event's `time` (and `end_time`) forward
// to the next future occurrence so the frontend's "hide if past" filter still
// shows it. Non-recurring events and recurring events whose stored time is
// already in the future are left untouched.
func applyRecurrence(e *models.Event) {
	if e == nil || !e.IsRecurring || e.RecurrenceRule == nil {
		return
	}
	now := time.Now()
	if !e.Time.Before(now) {
		return
	}
	next, ok := recurrence.NextOccurrence(*e.RecurrenceRule, e.Time, now)
	if !ok {
		return
	}
	if e.EndTime != nil {
		duration := e.EndTime.Sub(e.Time)
		newEnd := next.Add(duration)
		e.EndTime = &newEnd
	}
	e.Time = next
}

type EventRepository interface {
	Create(ctx context.Context, event *models.Event) error
	Update(ctx context.Context, event *models.Event) error
	Delete(ctx context.Context, id uuid.UUID) error
	GetByID(ctx context.Context, id uuid.UUID) (*models.Event, error)
	ListByHostID(ctx context.Context, hostID uuid.UUID) ([]*models.Event, error)
	ListByHostIDFiltered(ctx context.Context, hostID uuid.UUID, status *models.EventStatus, search string, sortBy string, limit, offset int) ([]*models.Event, error)
	ListByDateRange(ctx context.Context, hostID uuid.UUID, start, end time.Time) ([]*models.Event, error)
	ListTodayByHostID(ctx context.Context, hostID uuid.UUID, dayStart, dayEnd time.Time) ([]*models.Event, error)
	ListByHostIDForIDs(ctx context.Context, hostID uuid.UUID) ([]uuid.UUID, error)
	ListPublished(ctx context.Context, limit, offset int) ([]*models.Event, error)
	UpdateStatus(ctx context.Context, id uuid.UUID, status models.EventStatus) error
	IncrementBookingCount(ctx context.Context, eventID uuid.UUID, quantity int) error
	IncrementReviewCount(ctx context.Context, eventID uuid.UUID) error
	UpdateAverageRating(ctx context.Context, eventID uuid.UUID, avgRating float64) error
}

type postgresEventRepository struct {
	db *sql.DB
}

func NewEventRepository(db *sql.DB) EventRepository {
	return &postgresEventRepository{db: db}
}

var eventColumns = `id, host_id,
	title, hook_line, mood, description,
	cover_image_url, gallery_urls,
	is_online, meeting_link, location, location_lat, location_lng, google_maps_url, duration_minutes, min_group_size, max_group_size, capacity,
	languages, level,
	price_cents, is_free, time, end_time, is_recurring, recurrence_rule,
	cancellation_policy, status, published_at, paused_at, paused_from, paused_dates,
	ai_suggestion, avg_rating, total_bookings, total_reviews,
	created_at, updated_at`

func scanEvent(row interface {
	Scan(dest ...interface{}) error
}) (*models.Event, error) {
	e := &models.Event{}
	err := row.Scan(
		&e.ID, &e.HostID,
		&e.Title, &e.HookLine, &e.Mood, &e.Description,
		&e.CoverImageURL, &e.GalleryURLs,
		&e.IsOnline, &e.MeetingLink, &e.Location, &e.LocationLat, &e.LocationLng, &e.GoogleMapsURL, &e.DurationMinutes, &e.MinGroupSize, &e.MaxGroupSize, &e.Capacity,
		&e.Languages, &e.Level,
		&e.PriceCents, &e.IsFree, &e.Time, &e.EndTime, &e.IsRecurring, &e.RecurrenceRule,
		&e.CancellationPolicy, &e.Status, &e.PublishedAt, &e.PausedAt, &e.PausedFrom, &e.PausedDates,
		&e.AISuggestion, &e.AvgRating, &e.TotalBookings, &e.TotalReviews,
		&e.CreatedAt, &e.UpdatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	normalizedMood, err := models.NormalizeEventMood(e.Mood)
	if err != nil {
		return nil, err
	}
	e.Mood = normalizedMood
	applyRecurrence(e)
	return e, nil
}

func (r *postgresEventRepository) Create(ctx context.Context, event *models.Event) error {
	query := `
		INSERT INTO events (
			id, host_id,
			title, hook_line, mood, description,
			cover_image_url, gallery_urls,
			is_online, meeting_link, location, location_lat, location_lng, google_maps_url, duration_minutes, min_group_size, max_group_size, capacity,
			languages, level,
			price_cents, is_free, time, end_time, is_recurring, recurrence_rule,
			cancellation_policy, status, published_at, paused_from, paused_dates,
			ai_suggestion,
			created_at, updated_at
		) VALUES (
			$1, $2,
			$3, $4, $5, $6,
			$7, $8,
			$9, $10, $11, $12, $13, $14, $15, $16, $17, $18,
			$19, $20,
			$21, $22, $23, $24, $25, $26,
			$27, $28, $29, $30, $31,
			$32,
			$33, $34
		)
	`
	if event.ID == uuid.Nil {
		event.ID = uuid.New()
	}
	_, err := r.db.ExecContext(ctx, query,
		event.ID, event.HostID,
		event.Title, event.HookLine, event.Mood, event.Description,
		event.CoverImageURL, pq.Array(event.GalleryURLs),
		event.IsOnline, event.MeetingLink, event.Location, event.LocationLat, event.LocationLng, event.GoogleMapsURL, event.DurationMinutes, event.MinGroupSize, event.MaxGroupSize, event.Capacity,
		pq.Array(event.Languages), event.Level,
		event.PriceCents, event.IsFree, event.Time, event.EndTime, event.IsRecurring, event.RecurrenceRule,
		event.CancellationPolicy, event.Status, event.PublishedAt, event.PausedFrom, pq.Array(event.PausedDates),
		event.AISuggestion,
		event.CreatedAt, event.UpdatedAt,
	)
	return err
}

func (r *postgresEventRepository) Update(ctx context.Context, event *models.Event) error {
	query := `
		UPDATE events SET
			title = $1, hook_line = $2, mood = $3, description = $4,
			cover_image_url = $5, gallery_urls = $6,
			is_online = $7, meeting_link = $8, location = $9, location_lat = $10, location_lng = $11, google_maps_url = $12, duration_minutes = $13, min_group_size = $14, max_group_size = $15, capacity = $16,
			languages = $17, level = $18,
			price_cents = $19, is_free = $20, time = $21, end_time = $22, is_recurring = $23, recurrence_rule = $24,
			cancellation_policy = $25, status = $26, published_at = $27, paused_at = $28, paused_from = $29, paused_dates = $30,
			ai_suggestion = $31, avg_rating = $32, total_bookings = $33
		WHERE id = $34
	`
	_, err := r.db.ExecContext(ctx, query,
		event.Title, event.HookLine, event.Mood, event.Description,
		event.CoverImageURL, pq.Array(event.GalleryURLs),
		event.IsOnline, event.MeetingLink, event.Location, event.LocationLat, event.LocationLng, event.GoogleMapsURL, event.DurationMinutes, event.MinGroupSize, event.MaxGroupSize, event.Capacity,
		pq.Array(event.Languages), event.Level,
		event.PriceCents, event.IsFree, event.Time, event.EndTime, event.IsRecurring, event.RecurrenceRule,
		event.CancellationPolicy, event.Status, event.PublishedAt, event.PausedAt, event.PausedFrom, pq.Array(event.PausedDates),
		event.AISuggestion, event.AvgRating, event.TotalBookings,
		event.ID,
	)
	return err
}

func (r *postgresEventRepository) Delete(ctx context.Context, id uuid.UUID) error {
	query := `DELETE FROM events WHERE id = $1`
	_, err := r.db.ExecContext(ctx, query, id)
	return err
}

func (r *postgresEventRepository) GetByID(ctx context.Context, id uuid.UUID) (*models.Event, error) {
	query := `SELECT ` + eventColumns + ` FROM events WHERE id = $1`
	return scanEvent(r.db.QueryRowContext(ctx, query, id))
}

func (r *postgresEventRepository) ListByHostID(ctx context.Context, hostID uuid.UUID) ([]*models.Event, error) {
	query := `SELECT ` + eventColumns + ` FROM events WHERE host_id = $1 ORDER BY created_at DESC`
	fmt.Printf("[EVENT_REPO] ListByHostID: hostID=%s\n", hostID)
	return r.scanEvents(ctx, query, hostID)
}

func (r *postgresEventRepository) ListByHostIDFiltered(ctx context.Context, hostID uuid.UUID, status *models.EventStatus, search string, sortBy string, limit, offset int) ([]*models.Event, error) {
	args := []interface{}{hostID}
	conditions := []string{"host_id = $1"}
	idx := 2

	if status != nil {
		conditions = append(conditions, fmt.Sprintf("status = $%d", idx))
		args = append(args, *status)
		idx++
	}
	if search != "" {
		conditions = append(conditions, fmt.Sprintf("title ILIKE $%d", idx))
		args = append(args, "%"+search+"%")
		idx++
	}

	orderBy := "created_at DESC"
	if sortBy == "oldest" {
		orderBy = "created_at ASC"
	}

	query := fmt.Sprintf(`SELECT %s FROM events WHERE %s ORDER BY %s LIMIT $%d OFFSET $%d`,
		eventColumns, strings.Join(conditions, " AND "), orderBy, idx, idx+1)
	args = append(args, limit, offset)

	return r.scanEvents(ctx, query, args...)
}

func (r *postgresEventRepository) ListByDateRange(ctx context.Context, hostID uuid.UUID, start, end time.Time) ([]*models.Event, error) {
	query := `SELECT ` + eventColumns + ` FROM events WHERE host_id = $1 AND time >= $2 AND time < $3 ORDER BY time ASC`
	return r.scanEvents(ctx, query, hostID, start, end)
}

func (r *postgresEventRepository) ListTodayByHostID(ctx context.Context, hostID uuid.UUID, dayStart, dayEnd time.Time) ([]*models.Event, error) {
	query := `SELECT ` + eventColumns + ` FROM events WHERE host_id = $1 AND time >= $2 AND time < $3 AND status = 'live' ORDER BY time ASC`
	return r.scanEvents(ctx, query, hostID, dayStart, dayEnd)
}

func (r *postgresEventRepository) ListByHostIDForIDs(ctx context.Context, hostID uuid.UUID) ([]uuid.UUID, error) {
	query := `SELECT id FROM events WHERE host_id = $1`
	rows, err := r.db.QueryContext(ctx, query, hostID)
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

func (r *postgresEventRepository) UpdateStatus(ctx context.Context, id uuid.UUID, status models.EventStatus) error {
	var query string
	switch status {
	case models.EventStatusLive:
		query = `UPDATE events SET status = $1, published_at = NOW(), paused_at = NULL WHERE id = $2`
	case models.EventStatusPaused:
		query = `UPDATE events SET status = $1, paused_at = NOW() WHERE id = $2`
	default:
		query = `UPDATE events SET status = $1 WHERE id = $2`
	}
	_, err := r.db.ExecContext(ctx, query, status, id)
	return err
}

func (r *postgresEventRepository) ListPublished(ctx context.Context, limit, offset int) ([]*models.Event, error) {
	query := `SELECT ` + eventColumns + ` FROM events WHERE status IN ('live', 'paused') ORDER BY time ASC LIMIT $1 OFFSET $2`
	return r.scanEvents(ctx, query, limit, offset)
}

func (r *postgresEventRepository) scanEvents(ctx context.Context, query string, args ...interface{}) ([]*models.Event, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		fmt.Printf("[EVENT_REPO] scanEvents ERROR: %v\n", err)
		return nil, err
	}
	defer rows.Close()

	var events []*models.Event
	for rows.Next() {
		e := &models.Event{}
		if err := rows.Scan(
			&e.ID, &e.HostID,
			&e.Title, &e.HookLine, &e.Mood, &e.Description,
			&e.CoverImageURL, &e.GalleryURLs,
			&e.IsOnline, &e.MeetingLink, &e.Location, &e.LocationLat, &e.LocationLng, &e.GoogleMapsURL, &e.DurationMinutes, &e.MinGroupSize, &e.MaxGroupSize, &e.Capacity,
			&e.Languages, &e.Level,
			&e.PriceCents, &e.IsFree, &e.Time, &e.EndTime, &e.IsRecurring, &e.RecurrenceRule,
			&e.CancellationPolicy, &e.Status, &e.PublishedAt, &e.PausedAt, &e.PausedFrom, &e.PausedDates,
			&e.AISuggestion, &e.AvgRating, &e.TotalBookings, &e.TotalReviews,
			&e.CreatedAt, &e.UpdatedAt,
		); err != nil {
			fmt.Printf("[EVENT_REPO] scanEvents Scan ERROR: %v\n", err)
			return nil, err
		}
		fmt.Printf("[EVENT_REPO] scanEvents: Found event - id=%s, title=%s, hostID=%s, status=%v\n",
			e.ID, e.Title, e.HostID, e.Status)
		normalizedMood, err := models.NormalizeEventMood(e.Mood)
		if err != nil {
			return nil, err
		}
		e.Mood = normalizedMood
		applyRecurrence(e)
		events = append(events, e)
	}
	fmt.Printf("[EVENT_REPO] scanEvents: Total events found: %d\n", len(events))
	return events, nil
}

// IncrementBookingCount atomically increments the total_bookings counter for an event.
func (r *postgresEventRepository) IncrementBookingCount(ctx context.Context, eventID uuid.UUID, quantity int) error {
	query := `UPDATE events SET total_bookings = total_bookings + $1 WHERE id = $2`
	_, err := r.db.ExecContext(ctx, query, quantity, eventID)
	return err
}

// IncrementReviewCount atomically increments the total_reviews counter for an event.
func (r *postgresEventRepository) IncrementReviewCount(ctx context.Context, eventID uuid.UUID) error {
	query := `UPDATE events SET total_reviews = total_reviews + 1 WHERE id = $1`
	_, err := r.db.ExecContext(ctx, query, eventID)
	return err
}

// UpdateAverageRating updates the average rating for an event.
func (r *postgresEventRepository) UpdateAverageRating(ctx context.Context, eventID uuid.UUID, avgRating float64) error {
	query := `UPDATE events SET avg_rating = $1, updated_at = now() WHERE id = $2`
	_, err := r.db.ExecContext(ctx, query, avgRating, eventID)
	return err
}
