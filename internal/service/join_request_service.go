package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"

	"github.com/google/uuid"
)

// Join requests (RSVP) — the second way to gate a private event.
//
// A guest asks to join, fills in whatever attendee details the event requires,
// and waits. A host or a platform admin approves. Approval unlocks booking and
// nothing more: the guest still goes through the normal booking flow and still
// pays. Nothing here touches money.

var (
	ErrJoinRequestNotApplicable = errors.New("this experience does not take join requests")
	ErrJoinRequestExists        = errors.New("you already have a request for this experience")
	ErrJoinRequestNotFound      = errors.New("join request not found")
	ErrJoinRequestNotPending    = errors.New("that request has already been decided")
	ErrJoinRequestForbidden     = errors.New("you do not have access to that request")
)

type JoinRequestService interface {
	// Submit records a guest's request. The attendee answers are ALSO upserted
	// onto their profile, because that profile — not the request — is what the
	// booking guard reads later.
	Submit(ctx context.Context, eventID, userID uuid.UUID, in JoinRequestInput) (*models.EventJoinRequest, error)
	GetForUser(ctx context.Context, eventID, userID uuid.UUID) (*models.EventJoinRequest, error)
	Withdraw(ctx context.Context, requestID, userID uuid.UUID) error

	ListForHost(ctx context.Context, hostID uuid.UUID, status string, limit, offset int) ([]*models.EventJoinRequest, error)
	CountPendingForHost(ctx context.Context, hostID uuid.UUID) (int, error)
	ListForEvent(ctx context.Context, eventID, hostID uuid.UUID, status string) ([]*models.EventJoinRequest, error)
	ListAll(ctx context.Context, status string, limit, offset int) ([]*models.EventJoinRequest, error)

	// ReviewAsHost verifies the host owns the event before deciding. The host is
	// derived from the auth context by the controller — never from the body.
	ReviewAsHost(ctx context.Context, requestID, hostID uuid.UUID, approve bool, note *string) (*models.EventJoinRequest, error)
	// ReviewAsAdmin is authorised by the admin middleware; adminLabel is the
	// admin's username, recorded for the audit trail (admin JWTs carry no UUID).
	ReviewAsAdmin(ctx context.Context, requestID uuid.UUID, adminLabel string, approve bool, note *string) (*models.EventJoinRequest, error)

	// HasApproved is the booking gate's question: may this guest book?
	HasApproved(ctx context.Context, eventID, userID uuid.UUID) (bool, error)
}

// JoinRequestInput is what the guest submits. Answers mirror the attendee-field
// catalog keys; Message is the free-text note, which is always offered so a
// request is never an empty click even when the event configures no fields.
type JoinRequestInput struct {
	Message string                  `json:"message"`
	Answers *models.AttendeeProfile `json:"answers,omitempty"`
}

type joinRequestService struct {
	repo         repository.JoinRequestRepository
	eventRepo    repository.EventRepository
	attendeeRepo repository.AttendeeProfileRepository
	hostRepo     repository.HostRepository
	notifier     JoinRequestNotifier
}

// JoinRequestNotifier delivers the "you're in" message. Kept as a narrow
// interface so the service doesn't depend on the notification stack, and so a
// delivery failure can never fail an approval.
type JoinRequestNotifier interface {
	// NotifyJoinRequestReceived tells the host someone applied. hostName and
	// hostPhone are passed in because the notifier has no repositories.
	NotifyJoinRequestReceived(ctx context.Context, req *models.EventJoinRequest, hostName, hostPhone string)
	NotifyJoinRequestDecided(ctx context.Context, req *models.EventJoinRequest, approved bool)
}

func NewJoinRequestService(
	repo repository.JoinRequestRepository,
	eventRepo repository.EventRepository,
	attendeeRepo repository.AttendeeProfileRepository,
	hostRepo repository.HostRepository,
	notifier JoinRequestNotifier,
) JoinRequestService {
	return &joinRequestService{
		repo: repo, eventRepo: eventRepo, attendeeRepo: attendeeRepo,
		hostRepo: hostRepo, notifier: notifier,
	}
}

func (s *joinRequestService) Submit(ctx context.Context, eventID, userID uuid.UUID, in JoinRequestInput) (*models.EventJoinRequest, error) {
	evt, err := s.eventRepo.GetByID(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if evt == nil {
		return nil, errors.New("event not found")
	}
	if !evt.IsPrivate || evt.PrivateAccessMode != models.PrivateAccessModeRSVP {
		return nil, ErrJoinRequestNotApplicable
	}

	// One live request per guest per event. The partial unique index enforces
	// this too; checking first turns a constraint violation into a clear message.
	if existing, err := s.repo.GetLiveForUser(ctx, eventID, userID); err != nil {
		return nil, err
	} else if existing != nil {
		return nil, ErrJoinRequestExists
	}

	// Attendee answers go onto the profile, exactly as the booking form does.
	// Skipping this would approve a guest who then gets blocked at checkout by
	// the attendee-details gate.
	if in.Answers != nil {
		profile := *in.Answers
		profile.UserID = userID
		if err := s.attendeeRepo.Upsert(ctx, &profile); err != nil {
			return nil, fmt.Errorf("failed to save your details: %w", err)
		}
	}
	if evt.RequiresAttendeeDetails && len(evt.AttendeeFields) > 0 {
		profile, err := s.attendeeRepo.GetByUserID(ctx, userID)
		if err != nil {
			return nil, err
		}
		for _, field := range evt.AttendeeFields {
			if !profile.HasField(field) {
				return nil, errors.New("please complete all the requested details")
			}
		}
	}

	req := &models.EventJoinRequest{
		EventID:         eventID,
		UserID:          userID,
		Status:          models.JoinRequestPending,
		AnswersSnapshot: snapshotAnswers(in.Answers, evt.AttendeeFields),
	}
	if msg := strings.TrimSpace(in.Message); msg != "" {
		req.Message = &msg
	}
	if err := s.repo.Create(ctx, req); err != nil {
		return nil, err
	}

	// Reload so the notification has the guest's name and the event title.
	saved, err := s.repo.GetByID(ctx, req.ID)
	if err != nil {
		return nil, err
	}

	// Best-effort: the request is already recorded, so a WhatsApp failure must
	// not fail the guest's submission.
	if s.notifier != nil && saved != nil {
		hostName, hostPhone := s.hostContact(ctx, evt.HostID)
		s.notifier.NotifyJoinRequestReceived(ctx, saved, hostName, hostPhone)
	}
	return saved, nil
}

// hostContact resolves the host's display name and phone for notifications.
// Returns empty strings when the host can't be loaded — the notifier treats a
// missing phone as "nothing to send" rather than an error.
func (s *joinRequestService) hostContact(ctx context.Context, hostID uuid.UUID) (string, string) {
	if s.hostRepo == nil {
		return "", ""
	}
	host, err := s.hostRepo.GetByID(ctx, hostID)
	if err != nil || host == nil {
		return "", ""
	}
	name := strings.TrimSpace(host.FirstName + " " + host.LastName)
	return name, host.PhnNumber
}

func (s *joinRequestService) GetForUser(ctx context.Context, eventID, userID uuid.UUID) (*models.EventJoinRequest, error) {
	return s.repo.GetLatestForUser(ctx, eventID, userID)
}

func (s *joinRequestService) Withdraw(ctx context.Context, requestID, userID uuid.UUID) error {
	if err := s.repo.Withdraw(ctx, requestID, userID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrJoinRequestNotPending
		}
		return err
	}
	return nil
}

func (s *joinRequestService) ListForHost(ctx context.Context, hostID uuid.UUID, status string, limit, offset int) ([]*models.EventJoinRequest, error) {
	return s.repo.ListByHost(ctx, hostID, status, limit, offset)
}

func (s *joinRequestService) CountPendingForHost(ctx context.Context, hostID uuid.UUID) (int, error) {
	return s.repo.CountPendingByHost(ctx, hostID)
}

func (s *joinRequestService) ListForEvent(ctx context.Context, eventID, hostID uuid.UUID, status string) ([]*models.EventJoinRequest, error) {
	if err := s.assertHostOwnsEvent(ctx, eventID, hostID); err != nil {
		return nil, err
	}
	return s.repo.ListByEvent(ctx, eventID, status)
}

func (s *joinRequestService) ListAll(ctx context.Context, status string, limit, offset int) ([]*models.EventJoinRequest, error) {
	return s.repo.ListAll(ctx, status, limit, offset)
}

func (s *joinRequestService) ReviewAsHost(ctx context.Context, requestID, hostID uuid.UUID, approve bool, note *string) (*models.EventJoinRequest, error) {
	req, err := s.repo.GetByID(ctx, requestID)
	if err != nil {
		return nil, err
	}
	if req == nil {
		return nil, ErrJoinRequestNotFound
	}
	// Ownership is the whole authorisation story here: a host may only decide
	// requests on their own events.
	if err := s.assertHostOwnsEvent(ctx, req.EventID, hostID); err != nil {
		return nil, err
	}
	return s.review(ctx, req, models.ReviewerKindHost, &hostID, nil, approve, note)
}

func (s *joinRequestService) ReviewAsAdmin(ctx context.Context, requestID uuid.UUID, adminLabel string, approve bool, note *string) (*models.EventJoinRequest, error) {
	req, err := s.repo.GetByID(ctx, requestID)
	if err != nil {
		return nil, err
	}
	if req == nil {
		return nil, ErrJoinRequestNotFound
	}
	var label *string
	if adminLabel != "" {
		label = &adminLabel
	}
	return s.review(ctx, req, models.ReviewerKindAdmin, nil, label, approve, note)
}

func (s *joinRequestService) review(
	ctx context.Context, req *models.EventJoinRequest,
	kind models.ReviewerKind, reviewerID *uuid.UUID, reviewerLabel *string,
	approve bool, note *string,
) (*models.EventJoinRequest, error) {
	status := models.JoinRequestRejected
	if approve {
		status = models.JoinRequestApproved
	}
	if err := s.repo.Review(ctx, req.ID, status, kind, reviewerID, reviewerLabel, note); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Someone else got there first, or it was already decided.
			return nil, ErrJoinRequestNotPending
		}
		return nil, err
	}

	updated, err := s.repo.GetByID(ctx, req.ID)
	if err != nil {
		return nil, err
	}
	// Best-effort: the guest has to come back and book, so they need telling —
	// but a mail failure must not undo a decision the host already made.
	if s.notifier != nil && updated != nil {
		s.notifier.NotifyJoinRequestDecided(ctx, updated, approve)
	}
	return updated, nil
}

func (s *joinRequestService) HasApproved(ctx context.Context, eventID, userID uuid.UUID) (bool, error) {
	req, err := s.repo.GetLiveForUser(ctx, eventID, userID)
	if err != nil {
		return false, err
	}
	return req != nil && req.Status == models.JoinRequestApproved, nil
}

func (s *joinRequestService) assertHostOwnsEvent(ctx context.Context, eventID, hostID uuid.UUID) error {
	evt, err := s.eventRepo.GetByID(ctx, eventID)
	if err != nil {
		return err
	}
	if evt == nil {
		return ErrJoinRequestNotFound
	}
	if evt.HostID != hostID {
		return ErrJoinRequestForbidden
	}
	return nil
}

// snapshotAnswers records only the fields the event actually asked for, so the
// review screen shows the host what they requested and nothing more of the
// guest's saved profile.
func snapshotAnswers(p *models.AttendeeProfile, fields []string) models.JoinAnswers {
	out := models.JoinAnswers{}
	if p == nil {
		return out
	}
	put := func(key string, val any) {
		if val != nil {
			out[key] = val
		}
	}
	for _, f := range fields {
		switch f {
		case "name":
			put(f, p.Name)
		case "age":
			put(f, p.Age)
		case "gender":
			put(f, p.Gender)
		case "qualification":
			put(f, p.Qualification)
		case "occupation":
			put(f, p.Occupation)
		case "marital_status":
			put(f, p.MaritalStatus)
		case "contact_number":
			put(f, p.ContactNumber)
		case "whatsapp_number":
			put(f, p.WhatsappNumber)
		case "registration_type":
			put(f, p.RegistrationType)
		case "govt_id_url":
			put(f, p.GovtIDURL)
		case "travel":
			put(f, p.Travel)
		case "social_link":
			put(f, p.SocialLink)
		}
	}
	return out
}
