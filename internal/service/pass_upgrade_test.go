package service

import (
	"context"
	"testing"

	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"

	"github.com/google/uuid"
)

type fakeTiers struct {
	repository.EventPriceTierRepository
	tiers []models.EventPriceTier
}

func (f fakeTiers) ListByEventID(ctx context.Context, eventID uuid.UUID) ([]models.EventPriceTier, error) {
	return f.tiers, nil
}

// A pass session covers the basic (cheapest active) ticket; a pricier tier
// costs only the difference.
func TestPassUpgradeCents(t *testing.T) {
	bronze, gold, vip := uuid.New(), uuid.New(), uuid.New()
	tiers := []models.EventPriceTier{
		{ID: gold, PriceCents: 150000, IsActive: true},
		{ID: bronze, PriceCents: 50000, IsActive: true},
		{ID: vip, PriceCents: 10000, IsActive: false}, // inactive: never the base
	}
	s := &bookingService{tierRepo: fakeTiers{tiers: tiers}}
	ctx := context.Background()

	cases := []struct {
		name     string
		tiers    []models.EventPriceTier
		chosen   int64
		tier     *uuid.UUID
		wantDiff int64
		wantBase *uuid.UUID
	}{
		{"no tier chosen takes the basic ticket", tiers, 50000, nil, 0, &bronze},
		{"basic tier chosen costs nothing", tiers, 50000, &bronze, 0, &bronze},
		{"gold costs the difference", tiers, 150000, &gold, 100000, &bronze},
		{"a tier cheaper than basic costs nothing", tiers, 10000, &vip, 0, &bronze},
		{"no tiers, nothing to pay", nil, 70000, nil, 0, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s.tierRepo = fakeTiers{tiers: c.tiers}
			diff, base, err := s.passUpgradeCents(ctx, uuid.New(), c.chosen, c.tier)
			if err != nil {
				t.Fatal(err)
			}
			if diff != c.wantDiff {
				t.Fatalf("diff = %d, want %d", diff, c.wantDiff)
			}
			if (base == nil) != (c.wantBase == nil) || (base != nil && *base != *c.wantBase) {
				t.Fatalf("base = %v, want %v", base, c.wantBase)
			}
		})
	}
}
