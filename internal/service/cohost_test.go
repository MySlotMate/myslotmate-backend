package service

import (
	"context"
	"strings"
	"testing"

	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"

	"github.com/google/uuid"
)

// fakeCoHost answers only what the withdrawal guards ask. reserveOK=false
// simulates "the event's earnings are already claimed".
type fakeCoHost struct {
	repository.CoHostRepository
	row       *models.EventCoHost
	passed    int64
	claimed   int64
	reserveOK bool
	reserved  int64
}

func (f *fakeCoHost) GetByID(ctx context.Context, id uuid.UUID) (*models.EventCoHost, error) {
	return f.row, nil
}
func (f *fakeCoHost) EventPassedEarnings(ctx context.Context, eventID uuid.UUID) (int64, error) {
	return f.passed, nil
}
func (f *fakeCoHost) EventClaimedCents(ctx context.Context, eventID uuid.UUID) (int64, error) {
	return f.claimed, nil
}
func (f *fakeCoHost) ReserveClaim(ctx context.Context, cohostID, eventID, hostID uuid.UUID, amountCents int64) (uuid.UUID, error) {
	if !f.reserveOK {
		return uuid.Nil, nil
	}
	f.reserved = amountCents
	return uuid.New(), nil
}

type fakeEvents struct {
	repository.EventRepository
	evt       *models.Event
	canManage bool
}

func (f *fakeEvents) GetByID(ctx context.Context, id uuid.UUID) (*models.Event, error) {
	return f.evt, nil
}
func (f *fakeEvents) HostCanManage(ctx context.Context, eventID, hostID uuid.UUID) (bool, error) {
	return f.canManage, nil
}

// svc wires just enough of payoutService for the co-host guards. Any guard that
// passes would call RequestWithdrawal and nil-panic — which is the point: every
// case below must be refused before the money path is reached.
func svc(ch *fakeCoHost, ev *fakeEvents) *payoutService {
	return &payoutService{cohostRepo: ch, eventRepo: ev}
}

func TestCoHostWithdrawalGuards(t *testing.T) {
	cohostHost := uuid.New()
	owner := uuid.New()
	eventID := uuid.New()
	evt := &models.Event{ID: eventID, HostID: owner}

	accepted := func(canWithdraw bool) *models.EventCoHost {
		return &models.EventCoHost{
			ID: uuid.New(), EventID: eventID, HostID: cohostHost,
			InvitedByHostID: owner, Status: models.CoHostAccepted, CanWithdraw: canWithdraw,
		}
	}

	cases := []struct {
		name   string
		ch     *fakeCoHost
		amount int64
		want   string
	}{
		{
			name: "invitation not accepted yet",
			ch:   &fakeCoHost{row: &models.EventCoHost{ID: uuid.New(), EventID: eventID, HostID: cohostHost, Status: models.CoHostPending, CanWithdraw: true}, passed: 10000, reserveOK: true},
			want: "accept the co-host invitation",
		},
		{
			name: "owner has not granted the withdraw toggle",
			ch:   &fakeCoHost{row: accepted(false), passed: 10000, reserveOK: true},
			want: "not granted you withdrawal access",
		},
		{
			name: "whole pool requested but everything is already claimed",
			ch:   &fakeCoHost{row: accepted(true), passed: 10000, claimed: 10000, reserveOK: true},
			want: "nothing left to withdraw",
		},
		{
			name:   "explicit amount loses the race to another claim",
			ch:     &fakeCoHost{row: accepted(true), passed: 10000, reserveOK: false},
			amount: 5000,
			want:   "already withdrawn",
		},
		{
			name: "someone else's shared event",
			ch:   &fakeCoHost{row: accepted(true), passed: 10000, reserveOK: true},
			want: "shared event not found",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			caller := cohostHost
			if tc.want == "shared event not found" {
				caller = uuid.New() // not the invited host
			}
			_, err := svc(tc.ch, &fakeEvents{evt: evt}).
				RequestCoHostWithdrawal(context.Background(), caller, tc.ch.row.ID, WithdrawalRequest{AmountCents: tc.amount})
			if err == nil {
				t.Fatalf("expected refusal, got nil error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %q", tc.want, err.Error())
			}
			if tc.ch.reserved != 0 {
				t.Fatalf("a refused withdrawal must not reserve earnings, reserved %d", tc.ch.reserved)
			}
		})
	}
}

func TestHostCanManageEvent(t *testing.T) {
	owner, other := uuid.New(), uuid.New()
	evt := &models.Event{ID: uuid.New(), HostID: owner}

	if !HostCanManageEvent(context.Background(), &fakeEvents{evt: evt}, evt, owner) {
		t.Fatal("owner must be able to manage their own event")
	}
	if !HostCanManageEvent(context.Background(), &fakeEvents{evt: evt, canManage: true}, evt, other) {
		t.Fatal("accepted co-host must be able to manage the shared event")
	}
	if HostCanManageEvent(context.Background(), &fakeEvents{evt: evt, canManage: false}, evt, other) {
		t.Fatal("an unrelated host must not be able to manage the event")
	}
	if HostCanManageEvent(context.Background(), &fakeEvents{canManage: true}, nil, other) {
		t.Fatal("a nil event must never be manageable")
	}
}
