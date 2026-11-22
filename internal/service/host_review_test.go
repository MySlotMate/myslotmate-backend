package service

import (
	"context"
	"testing"

	"myslotmate-backend/internal/lib/event"
	"myslotmate-backend/internal/models"

	"github.com/google/uuid"
)

type fakeReviewHosts struct {
	fakeAppHosts
	byID *models.Host
}

func (f *fakeReviewHosts) GetByID(ctx context.Context, id uuid.UUID) (*models.Host, error) {
	return f.byID, nil
}

type fakeReviewUsers struct{ fakeAppUsers }

func (fakeReviewUsers) SetVerified(ctx context.Context, id uuid.UUID) error { return nil }

func pendingHost(hostType string) *models.Host {
	h := &models.Host{ID: uuid.New(), UserID: uuid.New(), ApplicationStatus: models.HostApplicationPending}
	h.HostType = hostType
	return h
}

func newReviewSvc(h *models.Host) (*hostService, *fakeReviewHosts) {
	hosts := &fakeReviewHosts{byID: h}
	return &hostService{hostRepo: hosts, userRepo: fakeReviewUsers{}, dispatcher: &event.Dispatcher{}}, hosts
}

func TestApprove_ProfessionalSetsIsProfessional(t *testing.T) {
	s, hosts := newReviewSvc(pendingHost(models.HostTypeProfessional))
	if _, err := s.ApproveApplication(context.Background(), hosts.byID.ID, false); err != nil {
		t.Fatal(err)
	}
	if u := hosts.updated; u == nil || !u.IsProfessional || u.ApplicationStatus != models.HostApplicationApproved || u.HostType != models.HostTypeProfessional {
		t.Fatalf("got %+v", u)
	}
}

func TestApprove_CommunityLeavesIsProfessionalAlone(t *testing.T) {
	h := pendingHost(models.HostTypeCommunity)
	h.IsProfessional = true // set earlier by an admin; approval must not undo it
	s, hosts := newReviewSvc(h)
	if _, err := s.ApproveApplication(context.Background(), h.ID, false); err != nil {
		t.Fatal(err)
	}
	if !hosts.updated.IsProfessional {
		t.Fatal("community approval cleared an admin-set is_professional")
	}
}

func TestApproveAsCommunity(t *testing.T) {
	s, hosts := newReviewSvc(pendingHost(models.HostTypeProfessional))
	if _, err := s.ApproveApplication(context.Background(), hosts.byID.ID, true); err != nil {
		t.Fatal(err)
	}
	if u := hosts.updated; u.IsProfessional || u.HostType != models.HostTypeCommunity || u.ApplicationStatus != models.HostApplicationApproved {
		t.Fatalf("got host_type=%q is_professional=%v status=%s", u.HostType, u.IsProfessional, u.ApplicationStatus)
	}
}

func TestReject_StoresReason(t *testing.T) {
	s, hosts := newReviewSvc(pendingHost(models.HostTypeCommunity))
	if _, err := s.RejectApplication(context.Background(), hosts.byID.ID, "  ID photo is blurry  "); err != nil {
		t.Fatal(err)
	}
	if u := hosts.updated; u.RejectionReason == nil || *u.RejectionReason != "ID photo is blurry" || u.RejectedAt == nil {
		t.Fatalf("got %+v", u)
	}
}

func TestReapply_ClearsRejectionReason(t *testing.T) {
	reason := "blurry"
	old := &models.Host{ID: uuid.New(), ApplicationStatus: models.HostApplicationRejected, RejectionReason: &reason}
	hosts := &fakeAppHosts{existing: old}
	if _, err := newAppService(hosts).SaveDraft(context.Background(), uuid.New(), HostApplicationRequest{FirstName: "A"}); err != nil {
		t.Fatal(err)
	}
	if hosts.updated.RejectionReason != nil {
		t.Fatal("old rejection reason survived a new application")
	}
}
