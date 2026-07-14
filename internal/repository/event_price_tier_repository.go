package repository

import (
	"context"
	"database/sql"

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

	// Delete tiers not referenced by any booking; keep referenced ones for history.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM event_price_tiers
		WHERE event_id = $1
		  AND id NOT IN (SELECT price_tier_id FROM bookings WHERE price_tier_id IS NOT NULL)
	`, eventID); err != nil {
		return err
	}

	// Deactivate any surviving (booking-referenced) tiers so they no longer surface.
	if _, err := tx.ExecContext(ctx, `
		UPDATE event_price_tiers SET is_active = FALSE, updated_at = now() WHERE event_id = $1
	`, eventID); err != nil {
		return err
	}

	// Insert the new set as active tiers.
	for i, t := range tiers {
		sortOrder := t.SortOrder
		if sortOrder == 0 {
			sortOrder = i
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO event_price_tiers (id, event_id, name, price_cents, capacity, sort_order, is_active)
			VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, TRUE)
		`, eventID, t.Name, t.PriceCents, t.Capacity, sortOrder); err != nil {
			return err
		}
	}

	return tx.Commit()
}
