package repository

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"

	"myslotmate-backend/internal/models"
)

// AttentionWindow is how far ahead "ending soon" looks.
const AttentionWindow = 7 * 24 * time.Hour

// AttentionRepository builds the "Needs attention" list for one experience.
type AttentionRepository interface {
	ForEvent(ctx context.Context, eventID uuid.UUID, now time.Time) ([]models.AttentionItem, error)
}

type postgresAttentionRepository struct{ db *sql.DB }

func NewAttentionRepository(db *sql.DB) AttentionRepository {
	return &postgresAttentionRepository{db: db}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// daysLeft rounds up, so anything ending later today or tomorrow reads "1 day".
func daysLeft(now, until time.Time) int {
	d := int(math.Ceil(until.Sub(now).Hours() / 24))
	if d < 1 {
		d = 1
	}
	return d
}

func (r *postgresAttentionRepository) ForEvent(ctx context.Context, eventID uuid.UUID, now time.Time) ([]models.AttentionItem, error) {
	items := []models.AttentionItem{}
	soon := now.Add(AttentionWindow)

	// 1. Join requests waiting for a decision.
	var pending int
	if err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM event_join_requests WHERE event_id = $1 AND status = 'pending'`,
		eventID).Scan(&pending); err != nil {
		return nil, fmt.Errorf("pending join requests: %w", err)
	}
	if pending > 0 {
		items = append(items, models.AttentionItem{
			Kind:  "join_requests",
			Count: pending,
			Title: fmt.Sprintf("%d %s to join waiting for you", pending, plural(pending, "request", "requests")),
		})
	}

	// 2. Shared codes (more than one use) on this experience or all of the
	// host's, that are used up, nearly used up, or ending soon. Single-use
	// codes are left out: a batch of them would flood the list.
	rows, err := r.db.QueryContext(ctx, `
		SELECT c.code, c.times_redeemed, c.max_redemptions, c.valid_until
		  FROM coupons c
		  JOIN events e ON e.id = $1
		 WHERE c.host_id = e.host_id
		   AND (c.event_id = e.id OR c.event_id IS NULL)
		   AND c.is_active
		   AND (c.max_redemptions IS NULL OR c.max_redemptions > 1)
		   AND (c.valid_until IS NULL OR c.valid_until > $2)
		   AND (
		         (c.max_redemptions IS NOT NULL AND c.times_redeemed * 5 >= c.max_redemptions * 4)
		      OR (c.valid_until IS NOT NULL AND c.valid_until <= $3)
		   )
		 ORDER BY c.code`, eventID, now, soon)
	if err != nil {
		return nil, fmt.Errorf("codes needing attention: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			code  string
			used  int
			max   sql.NullInt64
			until sql.NullTime
		)
		if err := rows.Scan(&code, &used, &max, &until); err != nil {
			return nil, err
		}
		var due *time.Time
		if until.Valid {
			t := until.Time
			due = &t
		}
		switch {
		case max.Valid && used >= int(max.Int64):
			items = append(items, models.AttentionItem{Kind: "code_used_up", Code: code, Due: due,
				Title: fmt.Sprintf("Code %s is used up (%d of %d)", code, used, max.Int64)})
		case max.Valid && used*5 >= int(max.Int64)*4:
			left := int(max.Int64) - used
			items = append(items, models.AttentionItem{Kind: "code_near_limit", Code: code, Due: due,
				Title: fmt.Sprintf("Code %s has %d %s left", code, left, plural(left, "use", "uses"))})
		default:
			d := daysLeft(now, until.Time)
			items = append(items, models.AttentionItem{Kind: "code_expiring", Code: code, Due: due,
				Title: fmt.Sprintf("Code %s stops working in %d %s", code, d, plural(d, "day", "days"))})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 3. Monthly passes that end within the window.
	var passes int
	var earliest sql.NullTime
	if err := r.db.QueryRowContext(ctx, `
		SELECT count(*), min(valid_until) FROM user_passes
		 WHERE event_id = $1 AND status = 'active'
		   AND valid_until > $2 AND valid_until <= $3`,
		eventID, now, soon).Scan(&passes, &earliest); err != nil {
		return nil, fmt.Errorf("passes ending soon: %w", err)
	}
	if passes > 0 {
		t := earliest.Time
		items = append(items, models.AttentionItem{
			Kind:  "passes_expiring",
			Count: passes,
			Due:   &t,
			Title: fmt.Sprintf("%d monthly %s %s this week", passes, plural(passes, "pass", "passes"), plural(passes, "ends", "end")),
		})
	}
	return items, nil
}
