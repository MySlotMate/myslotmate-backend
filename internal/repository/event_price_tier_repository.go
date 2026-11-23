package repository

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"myslotmate-backend/internal/models"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

type EventPriceTierRepository interface {
	GetByID(ctx context.Context, id uuid.UUID) (*models.EventPriceTier, error)
	ListByEventID(ctx context.Context, eventID uuid.UUID) ([]models.EventPriceTier, error)
	ListByEventIDs(ctx context.Context, eventIDs []uuid.UUID) (map[uuid.UUID][]models.EventPriceTier, error)
	// ReplaceForEvent swaps the event's tier set for the supplied one. Tiers that
	// are already referenced by a booking are kept but deactivated (history +
	// FK integrity); unreferenced tiers are deleted. The new set is inserted active.
	ReplaceForEvent(ctx context.Context, eventID uuid.UUID, tiers []models.EventPriceTier) error
	// BookedByTier returns seats held per ticket type per session for an event,
	// keyed by TierKey(name) then occurrence (UTC RFC3339), for sessions after
	// since. Counting by name keeps bookings made on an older row of the same
	// ticket type (before an edit) in the count.
	BookedByTier(ctx context.Context, eventID uuid.UUID, since time.Time) (map[string]map[string]int, error)
}

type postgresEventPriceTierRepository struct {
	db *sql.DB
}

func NewEventPriceTierRepository(db *sql.DB) EventPriceTierRepository {
	return &postgresEventPriceTierRepository{db: db}
}

const eventPriceTierColumns = `id, event_id, name, price_cents, capacity, sort_order, is_active, created_at, updated_at`

func scanEventPriceTier(row interface {
	Scan(dest ...interface{}) error
}) (*models.EventPriceTier, error) {
	t := &models.EventPriceTier{}
	err := row.Scan(
		&t.ID, &t.EventID, &t.Name, &t.PriceCents, &t.Capacity,
		&t.SortOrder, &t.IsActive, &t.CreatedAt, &t.UpdatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return t, nil
}

func (r *postgresEventPriceTierRepository) GetByID(ctx context.Context, id uuid.UUID) (*models.EventPriceTier, error) {
	query := `SELECT ` + eventPriceTierColumns + ` FROM event_price_tiers WHERE id = $1`
	return scanEventPriceTier(r.db.QueryRowContext(ctx, query, id))
}

func (r *postgresEventPriceTierRepository) ListByEventID(ctx context.Context, eventID uuid.UUID) ([]models.EventPriceTier, error) {
	query := `SELECT ` + eventPriceTierColumns + ` FROM event_price_tiers
		WHERE event_id = $1 AND is_active = TRUE
		ORDER BY sort_order ASC, created_at ASC`
	rows, err := r.db.QueryContext(ctx, query, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tiers := []models.EventPriceTier{}
	for rows.Next() {
		t, err := scanEventPriceTier(rows)
		if err != nil {
			return nil, err
		}
		tiers = append(tiers, *t)
	}
	return tiers, rows.Err()
}

func (r *postgresEventPriceTierRepository) ListByEventIDs(ctx context.Context, eventIDs []uuid.UUID) (map[uuid.UUID][]models.EventPriceTier, error) {
	out := make(map[uuid.UUID][]models.EventPriceTier)
	if len(eventIDs) == 0 {
		return out, nil
	}
	query := `SELECT ` + eventPriceTierColumns + ` FROM event_price_tiers
		WHERE event_id = ANY($1) AND is_active = TRUE
		ORDER BY sort_order ASC, created_at ASC`
	rows, err := r.db.QueryContext(ctx, query, pq.Array(eventIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		t, err := scanEventPriceTier(rows)
		if err != nil {
			return nil, err
		}
		out[t.EventID] = append(out[t.EventID], *t)
	}
	return out, rows.Err()
}

func (r *postgresEventPriceTierRepository) ReplaceForEvent(ctx context.Context, eventID uuid.UUID, tiers []models.EventPriceTier) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// A ticket type that keeps its name keeps its id, so bookings already made
	// on it still count toward its seat limit after the host saves the event.
	// Matching is by name, case- and space-insensitive.
	existing := map[string]uuid.UUID{}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, lower(trim(name)) FROM event_price_tiers
		WHERE event_id = $1 ORDER BY is_active DESC, updated_at DESC`, eventID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id uuid.UUID
		var key string
		if err := rows.Scan(&id, &key); err != nil {
			rows.Close()
			return err
		}
		if _, seen := existing[key]; !seen {
			existing[key] = id
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	keep := []string{}
	for i, t := range tiers {
		sortOrder := t.SortOrder
		if sortOrder == 0 {
			sortOrder = i
		}
		key := TierKey(t.Name)
		if id, ok := existing[key]; ok {
			delete(existing, key) // a repeated name in the new set gets a new row
			if _, err := tx.ExecContext(ctx, `
				UPDATE event_price_tiers
				SET name = $2, price_cents = $3, capacity = $4, sort_order = $5, is_active = TRUE, updated_at = now()
				WHERE id = $1`, id, t.Name, t.PriceCents, t.Capacity, sortOrder); err != nil {
				return err
			}
			keep = append(keep, id.String())
			continue
		}
		var id uuid.UUID
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO event_price_tiers (id, event_id, name, price_cents, capacity, sort_order, is_active)
			VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, TRUE) RETURNING id`,
			eventID, t.Name, t.PriceCents, t.Capacity, sortOrder).Scan(&id); err != nil {
			return err
		}
		keep = append(keep, id.String())
	}

	// Tiers no longer in the set: delete if no booking uses them, otherwise
	// deactivate so they stop showing but keep booking history intact.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM event_price_tiers
		WHERE event_id = $1 AND NOT (id = ANY($2::uuid[]))
		  AND id NOT IN (SELECT price_tier_id FROM bookings WHERE price_tier_id IS NOT NULL)
	`, eventID, pq.Array(keep)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE event_price_tiers SET is_active = FALSE, updated_at = now()
		WHERE event_id = $1 AND NOT (id = ANY($2::uuid[])) AND is_active
	`, eventID, pq.Array(keep)); err != nil {
		return err
	}

	return tx.Commit()
}

// TierKey is how two ticket-type rows are recognised as the same ticket type.
func TierKey(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

func (r *postgresEventPriceTierRepository) BookedByTier(ctx context.Context, eventID uuid.UUID, since time.Time) (map[string]map[string]int, error) {
	const query = `SELECT lower(trim(t.name)), b.occurrence_date, COALESCE(SUM(b.quantity), 0)
		FROM bookings b
		JOIN event_price_tiers t ON t.id = b.price_tier_id
		WHERE b.event_id = $1 AND b.occurrence_date >= $2
		  AND b.status IN ('pending', 'confirmed')
		GROUP BY lower(trim(t.name)), b.occurrence_date`
	rows, err := r.db.QueryContext(ctx, query, eventID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]map[string]int)
	for rows.Next() {
		var tierID string
		var occ time.Time
		var n int
		if err := rows.Scan(&tierID, &occ, &n); err != nil {
			return nil, err
		}
		if out[tierID] == nil {
			out[tierID] = make(map[string]int)
		}
		out[tierID][occ.UTC().Format(time.RFC3339)] = n
	}
	return out, rows.Err()
}
